// Command catalog-gen builds meshflash's catalog.json from upstream releases.
//
// This is the only component that knows how Meshtastic and MeshCore publish
// firmware. It runs on a schedule in CI and publishes catalog.json as a
// release asset; clients consume that artifact and never scrape upstream
// themselves. When an upstream renames an asset, this breaks in CI where it
// can be fixed once — not on an offline machine in a field.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/jclement/meshflash/internal/catalog"
)

func main() {
	out := flag.String("out", "catalog.json", "where to write the catalog")
	cacheDir := flag.String("cache", ".catalog-cache", "directory for downloaded archives")
	maxReleases := flag.Int("releases", 3, "how many releases to keep per project")
	only := flag.String("only", "", "restrict to one project: meshtastic or meshcore")
	flag.Parse()

	log.SetFlags(log.Ltime)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, *out, *cacheDir, *maxReleases, *only); err != nil {
		log.Fatalf("catalog-gen: %v", err)
	}
}

func run(ctx context.Context, outPath, cacheDir string, maxReleases int, only string) error {
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return err
	}
	c := newClient()
	if c.token == "" {
		log.Printf("warning: GITHUB_TOKEN is not set; the anonymous rate limit will likely stop this run partway")
	}

	cat := &catalog.Catalog{
		SchemaVersion: catalog.SchemaVersion,
		GeneratedAt:   time.Now().UTC().Truncate(time.Second),
		Generator:     "catalog-gen",
	}
	devices := map[string]catalog.Device{}

	if only == "" || only == meshtasticProjID {
		proj, devs, err := buildMeshtastic(ctx, c, cacheDir, maxReleases)
		if err != nil {
			return fmt.Errorf("meshtastic: %w", err)
		}
		if len(proj.Releases) > 0 {
			cat.Projects = append(cat.Projects, proj)
		}
		mergeDevices(devices, devs)
	}

	if only == "" || only == meshcoreProjID {
		proj, devs, err := buildMeshCore(ctx, c, maxReleases)
		if err != nil {
			return fmt.Errorf("meshcore: %w", err)
		}
		if len(proj.Releases) > 0 {
			cat.Projects = append(cat.Projects, proj)
		}
		mergeDevices(devices, devs)
	}

	cat.Devices = mapValues(devices)
	applyUSBHints(cat.Devices)

	sort.Slice(cat.Devices, func(i, j int) bool { return cat.Devices[i].ID < cat.Devices[j].ID })

	if err := cat.Validate(); err != nil {
		return fmt.Errorf("generated catalog is invalid: %w", err)
	}

	if err := catalog.Save(outPath, cat); err != nil {
		return err
	}

	summarize(cat, outPath)
	return nil
}

func summarize(cat *catalog.Catalog, outPath string) {
	st, _ := os.Stat(outPath)
	log.Printf("wrote %s (%d bytes)", outPath, st.Size())
	log.Printf("  devices: %d", len(cat.Devices))
	for _, p := range cat.Projects {
		builds := 0
		for _, r := range p.Releases {
			builds += len(r.Builds)
		}
		log.Printf("  %s: %d releases, %d builds", p.ID, len(p.Releases), builds)
	}
}

// mergeDevices folds per-project device registries together, preferring the
// entry with the most information since Meshtastic supplies display names that
// MeshCore's filenames cannot.
func mergeDevices(dst map[string]catalog.Device, src []catalog.Device) {
	for _, d := range src {
		existing, ok := dst[d.ID]
		if !ok {
			dst[d.ID] = d
			continue
		}
		if existing.Vendor == "" {
			existing.Vendor = d.Vendor
		}
		if existing.Platform == "" {
			existing.Platform = d.Platform
		}
		if len(existing.UF2Board) == 0 {
			existing.UF2Board = d.UF2Board
		}
		if len(existing.UF2Volume) == 0 {
			existing.UF2Volume = d.UF2Volume
		}
		dst[d.ID] = existing
	}
}

func mapValues(m map[string]catalog.Device) []catalog.Device {
	out := make([]catalog.Device, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// slugify normalises a board name into a catalog id.
func slugify(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
