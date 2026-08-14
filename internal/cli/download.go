package cli

import (
	"context"
	"flag"
	"fmt"

	"github.com/jclement/meshflash/internal/catalog"
	"github.com/jclement/meshflash/internal/store"
	"github.com/jclement/meshflash/internal/tui"
)

// cmdDownload fetches firmware into the cache without touching the catalog.
//
// `update` refreshes the catalog and then fills the cache, which is the right
// thing when you have a network and want everything current. This is the other
// half on its own: top up the cache for what is already selected, or pull one
// specific board, without re-fetching the index. Useful on a metered link, and
// useful for pulling a board you are about to be handed without disturbing a
// catalog version you have deliberately pinned.
func (a *App) cmdDownload(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("download", flag.ContinueOnError)
	deviceID := fs.String("device", "", "download for one device instead of the whole selection")
	all := fs.Bool("all", false, "download for every device in the catalog (very large)")
	force := fs.Bool("force", false, "re-download even when already cached")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cat, err := a.requireCatalog()
	if err != nil {
		return err
	}
	if a.Offline {
		return fmt.Errorf("`download` needs a network but --offline was set")
	}

	wantDevice := a.Cfg.WantsDevice
	scope := fmt.Sprintf("%d selected boards", len(a.Cfg.Devices))

	switch {
	case *deviceID != "":
		dev, ok := cat.DeviceByID(*deviceID)
		if !ok {
			return fmt.Errorf("unknown device %q; run `meshflash configure` to see the list", *deviceID)
		}
		// DeviceByID follows renames, so compare against the canonical id.
		wantDevice = func(id string) bool { return id == dev.ID }
		scope = dev.Name

	case *all:
		wantDevice = func(string) bool { return true }
		scope = "every board in the catalog"

	case len(a.Cfg.Devices) == 0:
		return fmt.Errorf("no devices selected.\n\n" +
			"Run `meshflash configure` to pick the boards you carry, or pass --device to fetch one")
	}

	wanted := store.WantedArtifacts(cat, wantDevice, a.Cfg.WantsProject, a.Cfg.WantsChannel, a.Cfg.KeepVersions)
	if len(wanted) == 0 {
		return fmt.Errorf("no firmware matches that selection")
	}

	need := a.missingArtifacts(wanted, *force)
	if len(need) == 0 {
		fmt.Fprintf(a.Out, "%s firmware for %s is already cached (%d artifacts)\n",
			tui.OK().Render(tui.GlyphOK), scope, len(wanted))
		return nil
	}

	fmt.Fprintf(a.Out, "Fetching %d artifacts for %s (about %s to download)\n",
		len(need), scope, tui.Selected().Render(store.FormatBytes(store.DownloadBytes(need))))

	if err := a.fetchArtifacts(ctx, need); err != nil {
		return err
	}
	fmt.Fprintf(a.Out, "%s cached firmware for %s\n", tui.OK().Render(tui.GlyphOK), scope)

	a.printCacheUsage(wanted, false)
	return nil
}

// printCacheUsage reports what the cache is holding, and whether pruning would
// reclaim anything.
func (a *App) printCacheUsage(wanted []catalog.Artifact, justPruned bool) {
	u, err := a.Store.Usage()
	if err != nil {
		return
	}
	fmt.Fprintf(a.Out, "%s cache: %s firmware, %s source archives\n",
		tui.Muted().Render(tui.GlyphInfo),
		store.FormatBytes(u.Extracted), store.FormatBytes(u.Downloads))
	if !justPruned && a.prunableBytes(wanted) > 0 {
		fmt.Fprintln(a.Out, tui.Muted().Render(
			"  `meshflash update --prune` reclaims the source archives once firmware is extracted."))
	}
}
