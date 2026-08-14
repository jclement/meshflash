package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/jclement/meshflash/internal/catalog"
	"github.com/jclement/meshflash/internal/device"
	"github.com/jclement/meshflash/internal/fingerprint"
	"github.com/jclement/meshflash/internal/plan"
	"github.com/jclement/meshflash/internal/tui"
)

// cmdAuto flashes every attached board with whatever it was flashed with last
// time, which is the whole point of the fingerprint database: a field kit of
// twenty nodes becomes one command instead of twenty guided sessions.
func (a *App) cmdAuto(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("auto", flag.ContinueOnError)
	probe := fs.Bool("probe", false, "read the ESP32 MAC of unrecognised boards to identify them")
	version := fs.String("version", "", "flash a specific version instead of the newest cached")
	erase := fs.Bool("erase", false, "full chip erase first (ESP32 only)")
	verify := fs.Bool("verify", false, "read back and compare after writing")
	dryRun := fs.Bool("dry-run", false, "show what would be flashed and stop")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cat, err := a.requireCatalog()
	if err != nil {
		return err
	}

	if a.Bindings.Len() == 0 {
		fmt.Fprintln(a.Out, tui.Warn().Render("No boards have been flashed by meshflash yet."))
		fmt.Fprintln(a.Out, tui.Muted().Render("Run `meshflash flash` once per board. Each one is remembered, and after"))
		fmt.Fprintln(a.Out, tui.Muted().Render("that `meshflash auto` will handle it without any prompts."))
		fmt.Fprintln(a.Out)
	}

	det, detErrs := device.Detect()
	for _, e := range detErrs {
		a.Log.Warn("device detection", "error", e)
	}
	targets := device.Identify(det, cat)
	if len(targets) == 0 {
		return errors.New("no devices found.\n\nCheck the USB cable carries data, then run `meshflash doctor`")
	}

	req := plan.Request{Version: *version}

	var (
		plans   []*plan.Plan
		skipped []skippedTarget
	)

	for _, t := range targets {
		p, err := plan.ResolveAuto(cat, a.Bindings, t, req)
		if err == nil {
			plans = append(plans, p)
			continue
		}

		// An unknown ESP32 board can still be identified by reading its eFuse
		// MAC, but that costs a reset, so it stays opt-in.
		var unknown *plan.ErrUnknownBoard
		if *probe && errors.As(err, &unknown) && t.Port != nil {
			if p, perr := a.probeAndResolve(ctx, cat, t, req); perr == nil {
				plans = append(plans, p)
				continue
			} else {
				a.Log.Debug("probe did not identify board", "port", t.Port.Name, "error", perr)
			}
		}

		skipped = append(skipped, skippedTarget{target: t, err: err})
	}

	a.reportSkipped(skipped, *probe)

	if len(plans) == 0 {
		return errors.New("none of the attached boards could be identified automatically.\n\n" +
			"Run `meshflash flash` to flash one by hand — it will be remembered for next time")
	}

	fmt.Fprintln(a.Out)
	fmt.Fprintln(a.Out, tui.Heading().Render(fmt.Sprintf("Will flash %d board(s)", len(plans))))
	for _, p := range plans {
		fmt.Fprint(a.Out, describePlan(p))
	}

	if *dryRun {
		fmt.Fprintln(a.Out, tui.Muted().Render("\nDry run — nothing was written."))
		return nil
	}

	if !a.Yes && !a.confirm(fmt.Sprintf("\nFlash %d board(s)?", len(plans))) {
		return errors.New("cancelled")
	}

	return a.runPlans(ctx, plans, flashOptions{
		Erase:          *erase,
		Verify:         *verify,
		AutoBootloader: true,
		Remember:       true,
	})
}

type skippedTarget struct {
	target device.Target
	err    error
}

// probeAndResolve reads a board's eFuse MAC and retries identification.
func (a *App) probeAndResolve(ctx context.Context, cat *catalog.Catalog, t device.Target, req plan.Request) (*plan.Plan, error) {
	fmt.Fprintf(a.Out, "  %s probing %s for its MAC…\n", tui.Muted().Render(tui.GlyphInfo), t.Port.Name)

	pctx, cancel := context.WithTimeout(ctx, fingerprint.ProbeTimeout)
	defer cancel()

	fp, chip, err := fingerprint.ProbeESP32(pctx, t.Port.Name)
	if err != nil {
		return nil, err
	}
	a.Log.Info("probed board", "port", t.Port.Name, "chip", chip, "fingerprint", fp.String())

	b, ok := a.Bindings.Lookup(fp)
	if !ok {
		return nil, fmt.Errorf("board %s (%s) has not been flashed by meshflash before", fp, chip)
	}

	// Re-resolve with the identity the probe established.
	req.DeviceID = b.DeviceID
	req.ProjectID = b.ProjectID
	req.Variant = b.Variant

	p, err := plan.Resolve(cat, t, req)
	if err != nil {
		return nil, err
	}
	p.Reason = fmt.Sprintf("identified by eFuse MAC (%s)", fp)
	binding := b
	p.Binding = &binding
	p.Fingerprint = fp
	return p, nil
}

func (a *App) reportSkipped(skipped []skippedTarget, probed bool) {
	if len(skipped) == 0 {
		return
	}

	fmt.Fprintln(a.Out)
	fmt.Fprintln(a.Out, tui.Heading().Render(fmt.Sprintf("Skipping %d board(s)", len(skipped))))
	for _, s := range skipped {
		fmt.Fprintf(a.Out, "  %s %s\n", tui.Warn().Render(tui.GlyphWarn), s.target.Describe())

		var unknown *plan.ErrUnknownBoard
		switch {
		case errors.As(s.err, &unknown):
			if unknown.Fingerprintable {
				fmt.Fprintf(a.Out, "    %s\n", tui.Muted().Render(
					"meshflash has not flashed this board before. Run `meshflash flash` once and it will be remembered."))
			} else if !probed {
				fmt.Fprintf(a.Out, "    %s\n", tui.Muted().Render(
					"this board publishes no unique serial. Try `meshflash auto --probe` to read its MAC instead."))
			} else {
				fmt.Fprintf(a.Out, "    %s\n", tui.Muted().Render(
					"not recognised even after probing. Run `meshflash flash` once to register it."))
			}
		default:
			fmt.Fprintf(a.Out, "    %s\n", tui.Muted().Render(s.err.Error()))
		}
	}
	fmt.Fprintln(a.Out)
}
