package main

import (
	"context"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jclement/meshflash/internal/catalog"
)

const (
	meshcoreRepo   = "meshcore-dev/MeshCore"
	meshcoreHome   = "https://meshcore.co.uk"
	meshcoreProjID = "meshcore"
)

// meshcoreAsset matches MeshCore's release asset naming, which is uniform
// enough to parse directly:
//
//	Heltec_t114_companion_radio_ble-v1.17.1-d929643.uf2
//	Ebyte_EoRa-S3_repeater-v1.17.1-d929643-merged.bin
//
// Parsing filenames rather than the web flasher's config.json keeps the
// generator independent of that app's layout, which serves a different
// firmware family and changes shape more often.
var meshcoreAsset = regexp.MustCompile(
	`^(?P<device>.+?)_(?P<role>companion_radio_ble|companion_radio_usb|repeater|room_server|terminal_chat|sensor)` +
		`-v(?P<version>[0-9][0-9.]*)-(?P<commit>[0-9a-f]+)(?P<merged>-merged)?\.(?P<ext>bin|uf2|zip)$`)

// variantKey identifies one flashable configuration of a board.
type variantKey struct {
	device string
	role   string
}

// meshcoreBuild accumulates the assets belonging to one device+role.
type meshcoreBuild struct {
	app    *catalog.Artifact // ESP32 app image, offset 0x10000
	merged *catalog.Artifact // ESP32 full image, offset 0x0
	uf2    *catalog.Artifact // UF2 payload
	pkg    *catalog.Artifact // Nordic DFU package
}

// buildMeshCore produces the MeshCore half of the catalog.
//
// MeshCore tags one release per role (companion-v1.17.1, repeater-v1.17.1, …).
// Those are recombined into a single catalog release per version, with the
// role exposed as the build variant, because "MeshCore 1.17.1, repeater" is
// how an operator thinks about it.
func buildMeshCore(ctx context.Context, c *client, maxVersions int) (catalog.Project, []catalog.Device, error) {
	proj := catalog.Project{
		ID:       meshcoreProjID,
		Name:     "MeshCore",
		Homepage: meshcoreHome,
		Repo:     meshcoreRepo,
	}

	releases, err := c.releases(ctx, meshcoreRepo, 60)
	if err != nil {
		return proj, nil, err
	}

	type versionInfo struct {
		published time.Time
		channel   string
		tags      []string
		builds    map[variantKey]*meshcoreBuild
	}
	versions := map[string]*versionInfo{}

	for _, rel := range releases {
		for _, asset := range rel.Assets {
			m := meshcoreAsset.FindStringSubmatch(asset.Name)
			if m == nil {
				continue
			}
			fields := namedGroups(meshcoreAsset, m)

			version := fields["version"]
			vi := versions[version]
			if vi == nil {
				vi = &versionInfo{
					published: rel.PublishedAt,
					channel:   rel.channel(),
					builds:    map[variantKey]*meshcoreBuild{},
				}
				versions[version] = vi
			}
			// Keep the newest publish time across the role tags making up
			// this version, and let any prerelease tag mark the whole version.
			if rel.PublishedAt.After(vi.published) {
				vi.published = rel.PublishedAt
			}
			if rel.Prerelease {
				vi.channel = "alpha"
			}
			if !contains(vi.tags, rel.TagName) {
				vi.tags = append(vi.tags, rel.TagName)
			}

			key := variantKey{device: fields["device"], role: fields["role"]}
			b := vi.builds[key]
			if b == nil {
				b = &meshcoreBuild{}
				vi.builds[key] = b
			}

			art := catalog.Artifact{
				Name: asset.Name,
				Size: asset.Size,
				URL:  asset.URL,
			}
			switch {
			case fields["ext"] == "uf2":
				art.Role = catalog.RoleUF2
				b.uf2 = &art
			case fields["ext"] == "zip":
				art.Role = catalog.RolePackage
				b.pkg = &art
			case fields["merged"] != "":
				art.Role = catalog.RoleMerged
				off := uint32(factoryFlashOff)
				art.Offset = &off
				b.merged = &art
			default:
				art.Role = catalog.RoleApp
				off := uint32(updateFlashOff)
				art.Offset = &off
				b.app = &art
			}
		}
	}

	// Newest versions first, capped.
	ordered := make([]string, 0, len(versions))
	for v := range versions {
		ordered = append(ordered, v)
	}
	sort.Slice(ordered, func(i, j int) bool {
		return versions[ordered[i]].published.After(versions[ordered[j]].published)
	})
	if len(ordered) > maxVersions {
		ordered = ordered[:maxVersions]
	}

	devices := map[string]catalog.Device{}

	for _, version := range ordered {
		vi := versions[version]
		var builds []catalog.Build

		for key, mb := range vi.builds {
			build, dev, ok := assembleMeshCoreBuild(key, mb)
			if !ok {
				continue
			}
			if _, exists := devices[dev.ID]; !exists {
				devices[dev.ID] = dev
			}
			builds = append(builds, build)
		}
		if len(builds) == 0 {
			continue
		}

		sort.Slice(builds, func(i, j int) bool {
			if builds[i].DeviceID != builds[j].DeviceID {
				return builds[i].DeviceID < builds[j].DeviceID
			}
			return builds[i].Variant < builds[j].Variant
		})
		sort.Strings(vi.tags)

		proj.Releases = append(proj.Releases, catalog.Release{
			Version:     version,
			Tag:         strings.Join(vi.tags, ","),
			Channel:     vi.channel,
			PublishedAt: vi.published,
			Builds:      builds,
		})
		log.Printf("meshcore %s: %d builds across %d tags", version, len(builds), len(vi.tags))
	}

	return proj, mapValues(devices), nil
}

// assembleMeshCoreBuild turns collected assets into one catalog build.
func assembleMeshCoreBuild(key variantKey, mb *meshcoreBuild) (catalog.Build, catalog.Device, bool) {
	// Fold onto the shared id so both projects' firmware lands on one board.
	id := canonicalDeviceID(slugify(key.device))

	dev := catalog.Device{
		ID:     id,
		Name:   strings.ReplaceAll(key.device, "_", " "),
		Vendor: vendorOf(key.device),
	}

	build := catalog.Build{
		DeviceID: id,
		Variant:  key.role,
	}

	switch {
	case mb.uf2 != nil:
		// UF2 is the primary path for nRF52 here. The DFU package is carried
		// alongside so the flash layer can fall back to serial DFU on a
		// machine where mass storage never mounts.
		dev.Platform = "nrf52840"
		applyUF2Hints(&dev)
		build.Method = catalog.MethodUF2
		build.Artifacts = append(build.Artifacts, *mb.uf2)
		if mb.pkg != nil {
			build.Artifacts = append(build.Artifacts, *mb.pkg)
		}

	case mb.pkg != nil:
		dev.Platform = "nrf52840"
		build.Method = catalog.MethodNRFDFU
		build.Artifacts = append(build.Artifacts, *mb.pkg)

	case mb.app != nil || mb.merged != nil:
		dev.Platform = "esp32"
		build.Method = catalog.MethodESP32
		if mb.app != nil {
			build.Artifacts = append(build.Artifacts, *mb.app)
		}
		if mb.merged != nil {
			build.Artifacts = append(build.Artifacts, *mb.merged)
		}

	default:
		return build, dev, false
	}

	return build, dev, len(build.Artifacts) > 0
}

// vendorOf guesses the maker from the leading token of a MeshCore asset name.
func vendorOf(device string) string {
	known := map[string]string{
		"heltec":    "Heltec",
		"lilygo":    "LilyGo",
		"ebyte":     "Ebyte",
		"seeed":     "Seeed Studio",
		"rak":       "RAK Wireless",
		"gat562":    "GAT",
		"elecrow":   "Elecrow",
		"promicro":  "ProMicro",
		"xiao":      "Seeed Studio",
		"m5stack":   "M5Stack",
		"thinknode": "Elecrow",
	}
	first, _, _ := strings.Cut(strings.ToLower(device), "_")
	if v, ok := known[first]; ok {
		return v
	}
	return ""
}

func namedGroups(re *regexp.Regexp, match []string) map[string]string {
	out := map[string]string{}
	for i, name := range re.SubexpNames() {
		if name != "" && i < len(match) {
			out[name] = match[i]
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
