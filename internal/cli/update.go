package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jclement/meshflash/internal/buildinfo"
	"github.com/jclement/meshflash/internal/catalog"
	"github.com/jclement/meshflash/internal/store"
	"github.com/jclement/meshflash/internal/tui"
)

func (a *App) cmdUpdate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	catalogOnly := fs.Bool("catalog-only", false, "fetch the catalog but download no firmware")
	prune := fs.Bool("prune", false, "delete cached firmware that is no longer selected")
	force := fs.Bool("force", false, "re-download even when already cached")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if a.Offline {
		return fmt.Errorf("`update` needs a network but --offline was set")
	}

	// --- catalog ---------------------------------------------------------

	fmt.Fprintln(a.Out, tui.Muted().Render("Fetching catalog from "+a.Cfg.CatalogURL))

	var prevDigest string
	if raw, err := os.ReadFile(a.Paths.CatalogFile()); err == nil {
		prevDigest = catalog.Digest(raw)
	}

	cat, raw, err := catalog.Fetch(ctx, nil, a.Cfg.CatalogURL, buildinfo.UserAgent())
	if err != nil {
		return err
	}
	newDigest := catalog.Digest(raw)

	if newDigest == prevDigest {
		fmt.Fprintf(a.Out, "%s catalog unchanged (generated %s)\n",
			tui.OK().Render(tui.GlyphOK), cat.GeneratedAt.Format(time.RFC3339))
	} else {
		if err := os.WriteFile(a.Paths.CatalogFile(), raw, 0o644); err != nil {
			return fmt.Errorf("save catalog: %w", err)
		}
		fmt.Fprintf(a.Out, "%s catalog updated: %d devices, %d projects (generated %s)\n",
			tui.OK().Render(tui.GlyphOK), len(cat.DeviceIDs()), len(cat.Projects),
			cat.GeneratedAt.Format(time.RFC3339))
	}
	a.Catalog, a.CatalogErr = cat, nil

	if *catalogOnly {
		return nil
	}

	// --- firmware --------------------------------------------------------

	if len(a.Cfg.Devices) == 0 {
		fmt.Fprintln(a.Out)
		fmt.Fprintln(a.Out, tui.Warn().Render("No devices selected."))
		fmt.Fprintln(a.Out, tui.Muted().Render("Downloading firmware for every board would pull hundreds of megabytes."))
		fmt.Fprintln(a.Out, tui.Muted().Render("Run `meshflash configure` to pick the boards you carry, then run update again."))
		return nil
	}

	wanted := store.WantedArtifacts(cat,
		a.Cfg.WantsDevice, a.Cfg.WantsProject, a.Cfg.WantsChannel, a.Cfg.KeepVersions)
	if len(wanted) == 0 {
		fmt.Fprintln(a.Out, tui.Warn().Render("Nothing to download for the current selection."))
		return nil
	}

	// Report the transfer up front: the operator may be on a hotspot, and the
	// per-platform archives are large even when only one board is selected.
	var need []catalog.Artifact
	for _, art := range wanted {
		if !*force && a.Store.Cached(art) {
			continue
		}
		need = append(need, art)
	}
	needBytes := store.DownloadBytes(need)

	if len(need) == 0 {
		fmt.Fprintf(a.Out, "%s firmware cache is already up to date (%d artifacts)\n",
			tui.OK().Render(tui.GlyphOK), len(wanted))
	} else {
		fmt.Fprintf(a.Out, "\nFetching %d firmware artifacts", len(need))
		if needBytes > 0 {
			fmt.Fprintf(a.Out, " (about %s to download)", store.FormatBytes(needBytes))
		}
		fmt.Fprintln(a.Out)

		if err := a.fetchArtifacts(ctx, need); err != nil {
			return err
		}
		fmt.Fprintf(a.Out, "%s firmware cached for %d boards\n",
			tui.OK().Render(tui.GlyphOK), len(a.Cfg.Devices))
	}

	// --- prune -----------------------------------------------------------

	if *prune {
		if err := a.pruneCache(wanted); err != nil {
			return err
		}
	}

	u, err := a.Store.Usage()
	if err == nil {
		fmt.Fprintf(a.Out, "%s cache: %s firmware, %s source archives\n",
			tui.Muted().Render(tui.GlyphInfo),
			store.FormatBytes(u.Extracted), store.FormatBytes(u.Downloads))
		// Only suggest pruning when it would actually reclaim something.
		// After a prune the remaining downloads are standalone artifacts —
		// MeshCore ships .bin/.uf2/.zip directly — which *are* the firmware
		// and can never be removed, so repeating the hint would be wrong.
		if !*prune && a.prunableBytes(wanted) > 0 {
			fmt.Fprintln(a.Out, tui.Muted().Render("  Source archives can be removed with `meshflash update --prune` once firmware is extracted."))
		}
	}
	return nil
}

// prunableBytes reports how much `--prune` would currently reclaim.
func (a *App) prunableBytes(wanted []catalog.Artifact) int64 {
	var n int64
	seen := map[string]bool{}
	for _, art := range wanted {
		// Only packed artifacts have a separate archive to throw away, and
		// only once the member has been extracted.
		if !art.Packed() || seen[art.Archive] || !a.Store.Cached(art) {
			continue
		}
		seen[art.Archive] = true
		if a.Store.ArchiveCached(art) {
			n += art.ArchiveSize
		}
	}
	return n
}

// fetchArtifacts downloads and extracts, showing a single-line progress meter.
func (a *App) fetchArtifacts(ctx context.Context, artifacts []catalog.Artifact) error {
	var lastLine string
	render := func(p store.Progress) {
		bar := tui.Bar(24, p.Percent())
		line := fmt.Sprintf("\r  %s %s %-28s", bar, capitalizeStage(p.Stage), tui.Truncate(p.Name, 28))
		if p.Total > 0 {
			line += fmt.Sprintf(" %s / %s", store.FormatBytes(p.Current), store.FormatBytes(p.Total))
		}
		// Pad to clear the previous, possibly longer, line.
		for len(line) < len(lastLine) {
			line += " "
		}
		lastLine = line
		fmt.Fprint(a.Out, line)
	}

	for _, art := range artifacts {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := a.Store.Ensure(ctx, art, render); err != nil {
			fmt.Fprintln(a.Out)
			return fmt.Errorf("fetch %s: %w", art.Name, err)
		}
	}
	fmt.Fprintln(a.Out, "\r"+padTo("", len(lastLine))+"\r")
	return nil
}

func (a *App) pruneCache(keep []catalog.Artifact) error {
	freedArchives, nArchives, err := a.Store.PruneArchives(keep)
	if err != nil {
		return err
	}
	freedExtract, nExtract, err := a.Store.PruneExtracted(keep)
	if err != nil {
		return err
	}
	total := freedArchives + freedExtract
	if nArchives+nExtract == 0 {
		fmt.Fprintf(a.Out, "%s nothing to prune\n", tui.Muted().Render(tui.GlyphInfo))
		return nil
	}
	fmt.Fprintf(a.Out, "%s pruned %s (%d archives, %d firmware directories)\n",
		tui.OK().Render(tui.GlyphOK), store.FormatBytes(total), nArchives, nExtract)
	return nil
}

func capitalizeStage(s string) string {
	switch s {
	case "download":
		return "downloading"
	case "extract":
		return "extracting "
	case "verify":
		return "verifying  "
	default:
		return s
	}
}

func padTo(s string, n int) string {
	for len(s) < n {
		s += " "
	}
	return s
}
