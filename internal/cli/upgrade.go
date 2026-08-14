package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/jclement/meshflash/internal/buildinfo"
	"github.com/jclement/meshflash/internal/selfupdate"
	"github.com/jclement/meshflash/internal/store"
	"github.com/jclement/meshflash/internal/tui"
)

func (a *App) cmdUpgrade(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	checkOnly := fs.Bool("check", false, "report whether an update exists and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if a.Offline {
		return errors.New("`upgrade` needs a network but --offline was set")
	}

	// Clear any binary left behind by a previous Windows upgrade.
	selfupdate.CleanupStale()

	fmt.Fprintf(a.Out, "%s current version %s\n", tui.Muted().Render(tui.GlyphInfo), buildinfo.Version)

	rel, err := selfupdate.Check(ctx, nil)
	if err != nil {
		return err
	}

	if rel.Version == buildinfo.Version {
		fmt.Fprintf(a.Out, "%s already up to date\n", tui.OK().Render(tui.GlyphOK))
		return nil
	}

	fmt.Fprintf(a.Out, "%s %s is available (published %s)\n",
		tui.OK().Render(tui.GlyphOK), tui.Selected().Render(rel.Version),
		rel.PublishedAt.Format("2006-01-02"))
	if rel.Notes != "" {
		fmt.Fprintln(a.Out)
		fmt.Fprintln(a.Out, tui.Indent(tui.Truncate(rel.Notes, 2000), "  "))
		fmt.Fprintln(a.Out)
	}

	if *checkOnly {
		fmt.Fprintln(a.Out, tui.Muted().Render("Run `meshflash upgrade` to install it."))
		return nil
	}

	if !buildinfo.IsRelease() {
		return selfupdate.ErrDevBuild
	}
	if !a.confirm(fmt.Sprintf("Install %s?", rel.Version)) {
		return errors.New("cancelled")
	}

	var lastPct int64 = -1
	err = selfupdate.Apply(ctx, nil, rel, func(cur, total int64) {
		if total <= 0 {
			return
		}
		pct := cur * 100 / total
		if pct == lastPct {
			return
		}
		lastPct = pct
		fmt.Fprintf(a.Out, "\r  %s %3d%%  %s / %s   ",
			tui.Bar(24, float64(pct)), pct, store.FormatBytes(cur), store.FormatBytes(total))
	})
	fmt.Fprintln(a.Out)
	if err != nil {
		return err
	}

	fmt.Fprintf(a.Out, "%s upgraded to %s\n", tui.OK().Render(tui.GlyphOK), rel.Version)
	fmt.Fprintln(a.Out, tui.Muted().Render("Run `meshflash update` to refresh the firmware catalog for this version."))
	return nil
}
