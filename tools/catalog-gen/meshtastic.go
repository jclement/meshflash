package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jclement/meshflash/internal/catalog"
)

const (
	meshtasticRepo   = "meshtastic/firmware"
	hardwareListURL  = "https://raw.githubusercontent.com/meshtastic/web-flasher/main/public/data/hardware-list.json"
	meshtasticHome   = "https://meshtastic.org"
	updateFlashOff   = 0x10000 // app0; device-update.sh writes here and preserves NVS
	factoryFlashOff  = 0x0     // device-install.sh erases then writes the factory image here
	meshtasticProjID = "meshtastic"
)

// hardwareEntry mirrors an element of the web flasher's hardware-list.json,
// which is the closest thing Meshtastic has to a canonical board registry.
type hardwareEntry struct {
	HWModel           int      `json:"hwModel"`
	HWModelSlug       string   `json:"hwModelSlug"`
	PlatformIOTarget  string   `json:"platformioTarget"`
	Architecture      string   `json:"architecture"`
	ActivelySupported bool     `json:"activelySupported"`
	DisplayName       string   `json:"displayName"`
	Tags              []string `json:"tags"`
}

// mtManifest is the per-device .mt.json sidecar shipped inside each firmware
// archive. It is authoritative for flash offsets, so meshflash never has to
// hardcode a partition layout or parse the install script.
type mtManifest struct {
	Version          string `json:"version"`
	PlatformIOTarget string `json:"platformioTarget"`
	MCU              string `json:"mcu"`
	Architecture     string `json:"architecture"`
	DisplayName      string `json:"displayName"`
	Files            []struct {
		Name     string `json:"name"`
		MD5      string `json:"md5"`
		Bytes    int64  `json:"bytes"`
		PartName string `json:"part_name"`
	} `json:"files"`
	Part []struct {
		Name   string `json:"name"`
		Type   string `json:"type"`
		Offset string `json:"offset"`
		Size   string `json:"size"`
	} `json:"part"`
}

// partitionOffset resolves a partition name to its flash address.
func (m mtManifest) partitionOffset(name string) (uint32, bool) {
	for _, p := range m.Part {
		if p.Name != name {
			continue
		}
		v, err := strconv.ParseUint(strings.TrimPrefix(p.Offset, "0x"), 16, 32)
		if err != nil {
			return 0, false
		}
		return uint32(v), true
	}
	return 0, false
}

// buildMeshtastic produces the Meshtastic half of the catalog.
func buildMeshtastic(ctx context.Context, c *client, cacheDir string, maxReleases int) (catalog.Project, []catalog.Device, error) {
	proj := catalog.Project{
		ID:       meshtasticProjID,
		Name:     "Meshtastic",
		Homepage: meshtasticHome,
		Repo:     meshtasticRepo,
	}

	hardware, err := fetchHardwareList(ctx, c)
	if err != nil {
		return proj, nil, err
	}

	releases, err := c.releases(ctx, meshtasticRepo, 30)
	if err != nil {
		return proj, nil, err
	}

	devices := map[string]catalog.Device{}
	kept := 0

	for _, rel := range releases {
		if kept >= maxReleases {
			break
		}
		version := strings.TrimPrefix(rel.TagName, "v")

		builds, err := meshtasticBuilds(ctx, c, cacheDir, rel, hardware, devices)
		if err != nil {
			log.Printf("meshtastic %s: %v", rel.TagName, err)
			continue
		}
		if len(builds) == 0 {
			continue
		}

		proj.Releases = append(proj.Releases, catalog.Release{
			Version:     version,
			Tag:         rel.TagName,
			Channel:     rel.channel(),
			PublishedAt: rel.PublishedAt,
			Builds:      builds,
		})
		kept++
		log.Printf("meshtastic %s: %d builds", rel.TagName, len(builds))
	}

	return proj, mapValues(devices), nil
}

// meshtasticBuilds walks the firmware archives in one release.
func meshtasticBuilds(ctx context.Context, c *client, cacheDir string, rel ghRelease, hardware map[string]hardwareEntry, devices map[string]catalog.Device) ([]catalog.Build, error) {
	var builds []catalog.Build

	for _, asset := range rel.Assets {
		// Only the firmware archives matter. debug-elfs, source zips and the
		// platformio dependency bundles are large and irrelevant.
		if !strings.HasPrefix(asset.Name, "firmware-") || !strings.HasSuffix(asset.Name, ".zip") {
			continue
		}

		local := filepath.Join(cacheDir, asset.Name)
		if err := c.download(ctx, asset.URL, local, asset.Size); err != nil {
			log.Printf("  %s: %v", asset.Name, err)
			continue
		}

		got, err := buildsFromArchive(local, asset, hardware, devices)
		if err != nil {
			log.Printf("  %s: %v", asset.Name, err)
			continue
		}
		builds = append(builds, got...)
	}
	return builds, nil
}

// buildsFromArchive reads every .mt.json in a firmware zip and emits one build
// per device.
func buildsFromArchive(localPath string, asset ghAsset, hardware map[string]hardwareEntry, devices map[string]catalog.Device) ([]catalog.Build, error) {
	zr, err := zip.OpenReader(localPath)
	if err != nil {
		return nil, err
	}
	defer zr.Close()

	// Index members so an artifact can be matched to its compressed entry
	// without a second pass, and so sizes come from the archive itself.
	members := map[string]*zip.File{}
	for _, f := range zr.File {
		members[path.Base(f.Name)] = f
	}

	var builds []catalog.Build
	for _, f := range zr.File {
		if !strings.HasSuffix(f.Name, ".mt.json") {
			continue
		}
		var m mtManifest
		if err := readJSONMember(f, &m); err != nil {
			log.Printf("  %s: %v", f.Name, err)
			continue
		}
		if m.PlatformIOTarget == "" {
			continue
		}

		b, dev, ok := buildFromManifest(m, asset, members, hardware)
		if !ok {
			continue
		}
		if _, exists := devices[dev.ID]; !exists {
			devices[dev.ID] = dev
		}
		builds = append(builds, b)
	}
	return builds, nil
}

// buildFromManifest turns one .mt.json into a catalog build plus its device.
func buildFromManifest(m mtManifest, asset ghAsset, members map[string]*zip.File, hardware map[string]hardwareEntry) (catalog.Build, catalog.Device, bool) {
	id := m.PlatformIOTarget
	hw := hardware[id]

	dev := catalog.Device{
		ID:       id,
		Name:     firstNonEmpty(hw.DisplayName, m.DisplayName, id),
		Platform: firstNonEmpty(m.MCU, hw.Architecture),
	}
	if len(hw.Tags) > 0 {
		dev.Vendor = hw.Tags[0]
	}
	if !hw.ActivelySupported && hw.PlatformIOTarget != "" {
		dev.Notes = "not actively supported upstream"
	}

	artifact := func(name string, role catalog.Role, offset *uint32) (catalog.Artifact, bool) {
		f, ok := members[name]
		if !ok {
			return catalog.Artifact{}, false
		}
		return catalog.Artifact{
			Role:        role,
			Name:        name,
			Size:        int64(f.UncompressedSize64),
			Archive:     asset.URL,
			ArchivePath: f.Name,
			ArchiveSize: asset.Size,
			Offset:      offset,
		}, true
	}

	build := catalog.Build{DeviceID: id}

	// nRF52 and RP2040 ship a .uf2 and nothing else.
	if uf2 := findFile(m, ".uf2"); uf2 != "" {
		a, ok := artifact(uf2, catalog.RoleUF2, nil)
		if !ok {
			return build, dev, false
		}
		build.Method = catalog.MethodUF2
		build.Artifacts = []catalog.Artifact{a}
		if dev.Platform == "" {
			dev.Platform = "nrf52840"
		}
		applyUF2Hints(&dev)
		return build, dev, true
	}

	// ESP32: carry both the app image and the factory image so the flash layer
	// can honour --erase without a second catalog entry.
	//
	//   app0 at 0x10000 leaves NVS intact, so the node keeps its identity.
	//   the factory image at 0x0 spans NVS and therefore wipes it.
	build.Method = catalog.MethodESP32

	appOffset := uint32(updateFlashOff)
	if off, ok := m.partitionOffset("app"); ok {
		appOffset = off
	}
	factoryOffset := uint32(factoryFlashOff)

	appName := findAppImage(m)
	if appName == "" {
		return build, dev, false
	}
	app, ok := artifact(appName, catalog.RoleApp, &appOffset)
	if !ok {
		return build, dev, false
	}
	build.Artifacts = append(build.Artifacts, app)

	if factory := findFile(m, ".factory.bin"); factory != "" {
		if a, ok := artifact(factory, catalog.RoleMerged, &factoryOffset); ok {
			build.Artifacts = append(build.Artifacts, a)
		}
	}

	// The filesystem image only matters for a factory install; an update that
	// rewrote it would discard whatever the node had stored.
	for _, f := range m.Files {
		if f.PartName != "spiffs" {
			continue
		}
		if off, ok := m.partitionOffset(f.PartName); ok {
			if a, ok := artifact(f.Name, catalog.RoleLittleFS, &off); ok {
				build.Artifacts = append(build.Artifacts, a)
			}
		}
	}

	return build, dev, len(build.Artifacts) > 0
}

// findAppImage returns the OTA application image: the plain .bin carrying the
// app0 partition name, never the merged factory image.
func findAppImage(m mtManifest) string {
	for _, f := range m.Files {
		if f.PartName == "app0" && strings.HasSuffix(f.Name, ".bin") && !strings.HasSuffix(f.Name, ".factory.bin") {
			return f.Name
		}
	}
	// Older manifests omit part_name; fall back to the naming convention.
	for _, f := range m.Files {
		if strings.HasSuffix(f.Name, ".bin") &&
			!strings.HasSuffix(f.Name, ".factory.bin") &&
			strings.HasPrefix(f.Name, "firmware-") {
			return f.Name
		}
	}
	return ""
}

func findFile(m mtManifest, suffix string) string {
	for _, f := range m.Files {
		if strings.HasSuffix(f.Name, suffix) {
			return f.Name
		}
	}
	return ""
}

func readJSONMember(f *zip.File, out any) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, 1<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func fetchHardwareList(ctx context.Context, c *client) (map[string]hardwareEntry, error) {
	var list []hardwareEntry
	if err := c.getJSON(ctx, hardwareListURL, &list); err != nil {
		return nil, fmt.Errorf("fetch hardware list: %w", err)
	}
	out := make(map[string]hardwareEntry, len(list))
	for _, e := range list {
		if e.PlatformIOTarget != "" {
			out[e.PlatformIOTarget] = e
		}
	}
	log.Printf("meshtastic hardware list: %d boards", len(out))
	return out, nil
}
