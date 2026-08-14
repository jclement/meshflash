package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jclement/meshflash/internal/bindings"
	"github.com/jclement/meshflash/internal/catalog"
	"github.com/jclement/meshflash/internal/device"
	"github.com/jclement/meshflash/internal/fingerprint"
	"github.com/jclement/meshflash/internal/flash"
	"github.com/jclement/meshflash/internal/plan"
	"github.com/jclement/meshflash/internal/store"
	"github.com/jclement/meshflash/internal/tui"
)

// flashOptions are the knobs shared by `flash` and `auto`.
type flashOptions struct {
	Erase          bool
	Verify         bool
	AutoBootloader bool
	Remember       bool
	DryRun         bool
}

// runPlans executes a set of plans through the progress view and records the
// results as bindings.
func (a *App) runPlans(ctx context.Context, plans []*plan.Plan, opts flashOptions) error {
	if len(plans) == 0 {
		return errors.New("nothing to flash")
	}

	// Confirm destructive intent once, up front, rather than per board.
	if opts.Erase && !a.confirmErase(plans) {
		return errors.New("cancelled")
	}

	jobs := make([]tui.FlashJob, 0, len(plans))
	for _, p := range plans {
		jobs = append(jobs, a.buildJob(p, opts))
	}

	// The progress view repaints the whole terminal. Any log line written to
	// stderr while it does lands mid-frame and shreds the display, so the
	// console sink is silenced for the duration — everything still goes to the
	// session file, which is where flash detail belongs anyway.
	a.Session.MuteConsole(true)
	outcomes, err := tui.RunFlash(ctx, jobs)
	a.Session.MuteConsole(false)
	if err != nil {
		return err
	}

	return a.reportOutcomes(outcomes)
}

// buildJob wraps a plan as a TUI job.
func (a *App) buildJob(p *plan.Plan, opts flashOptions) tui.FlashJob {
	detail := fmt.Sprintf("%s %s", p.Project.Name, p.Release.Version)
	if p.Variant() != "" {
		detail += " · " + p.Variant()
	}
	detail += " · " + string(p.Build.Method)

	return tui.FlashJob{
		Title:  fmt.Sprintf("%s on %s", p.Device.Name, p.Target.Address()),
		Detail: detail,
		Run: func(ctx context.Context, onFlash flash.ProgressFunc, onStore store.ProgressFunc, onLog func(string)) (*flash.Result, error) {
			payloads, err := plan.LoadPayloads(ctx, a.Store, p, onStore)
			if err != nil {
				return nil, err
			}

			res, err := flash.Flash(ctx, flash.Request{
				Target:         p.Target,
				Device:         p.Device,
				Build:          p.Build,
				Payloads:       payloads,
				Erase:          opts.Erase,
				Verify:         opts.Verify,
				AutoBootloader: opts.AutoBootloader,
				Logger:         a.Log,
				Progress:       onFlash,
				OnManualPrompt: func(msg string) { onLog(msg) },
			})
			if err != nil {
				return nil, err
			}

			if opts.Remember {
				a.remember(p, res, onLog)
			}
			return res, nil
		},
	}
}

// remember records what was written so `meshflash auto` can repeat it.
//
// A board with no stable fingerprint cannot be remembered; that is reported
// rather than silently skipped, because it changes what `auto` can do later.
func (a *App) remember(p *plan.Plan, res *flash.Result, onLog func(string)) {
	fp := p.Fingerprint
	if !fp.Valid() {
		// A UF2 flash reboots the board into its application, where it may
		// finally publish a serial number. Re-scan and try once more.
		fp = rescanFingerprint(p)
	}
	if !fp.Valid() {
		onLog("Board has no unique serial, so it cannot be recognised automatically next time.")
		a.Log.Info("no stable fingerprint; not remembering board", "device", p.Device.ID)
		return
	}

	b := bindings.Binding{
		Fingerprint: fp,
		DeviceID:    p.Device.ID,
		ProjectID:   p.Project.ID,
		Variant:     p.Variant(),
		LastVersion: p.Release.Version,
		Chip:        res.Chip,
	}
	if p.Binding != nil {
		b.Nickname = p.Binding.Nickname
		b.Notes = p.Binding.Notes
	}

	if err := a.Bindings.Remember(b); err != nil {
		a.Log.Warn("could not remember board", "error", err)
		return
	}
	if err := a.Bindings.Save(); err != nil {
		a.Log.Warn("could not save bindings", "error", err)
		return
	}
	onLog("Remembered this board as " + p.Device.ID + "; `meshflash auto` will handle it next time.")
}

// rescanFingerprint looks for the board again after a flash, since a device
// that was in a bootloader may expose a different identity once running.
func rescanFingerprint(p *plan.Plan) fingerprint.Fingerprint {
	// Give the device time to reboot and re-enumerate.
	time.Sleep(2 * time.Second)

	det, _ := device.Detect()
	for _, t := range device.Identify(det, nil) {
		if t.Address() == p.Target.Address() {
			if fp := fingerprint.FromTarget(t); fp.Valid() {
				return fp
			}
		}
	}
	// Fall back to any single newly-visible port with a usable serial.
	for _, port := range det.Ports {
		if fp := fingerprint.FromPort(port); fp.Valid() {
			return fp
		}
	}
	return fingerprint.Fingerprint{}
}

func (a *App) confirmErase(plans []*plan.Plan) bool {
	anyESP := false
	for _, p := range plans {
		if p.Build.Method == catalog.MethodESP32 {
			anyESP = true
		}
	}
	if !anyESP {
		return true // erase is a no-op for UF2 and DFU
	}

	fmt.Fprintln(a.Out)
	fmt.Fprintln(a.Out, tui.Danger().Render(
		tui.Warn().Render("Full chip erase requested.")+"\n\n"+
			"This wipes NVS along with the firmware. On Meshtastic that destroys the\n"+
			"node's private key: it comes back with a new identity, remote admin stops\n"+
			"working, and encrypted direct messages to it fail until peers re-learn it.\n\n"+
			"Region, channels and all settings are lost too."))
	fmt.Fprintln(a.Out)
	return a.confirm("Erase and flash anyway?")
}

// reportOutcomes prints the post-flash summary and returns an error if any job
// failed, so the exit code reflects reality.
func (a *App) reportOutcomes(outcomes []tui.JobOutcome) error {
	var failed int

	fmt.Fprintln(a.Out)
	for _, o := range outcomes {
		if o.Err != nil {
			failed++
			fmt.Fprintf(a.Out, "%s %s\n", tui.Error().Render(tui.GlyphFail), o.Title)
			fmt.Fprintln(a.Out, tui.Indent(o.Err.Error(), "    "))
			continue
		}

		detail := ""
		if o.Result != nil {
			detail = fmt.Sprintf("%s in %s",
				store.FormatBytes(o.Result.BytesWritten), o.Duration.Round(time.Millisecond))
			if o.Result.Chip != "" {
				detail = o.Result.Chip + " · " + detail
			}
		}
		fmt.Fprintf(a.Out, "%s %s  %s\n", tui.OK().Render(tui.GlyphOK), o.Title, tui.Muted().Render(detail))

		if o.Result != nil {
			for _, w := range o.Result.Warnings {
				fmt.Fprintf(a.Out, "    %s %s\n", tui.Muted().Render(tui.GlyphInfo), tui.Muted().Render(w))
			}
		}
	}

	fmt.Fprintln(a.Out)
	if failed > 0 {
		fmt.Fprintln(a.Out, tui.Muted().Render("Full log: "+a.Session.Path))
		return fmt.Errorf("%d of %d boards failed to flash", failed, len(outcomes))
	}

	fmt.Fprintln(a.Out, tui.OK().Render(fmt.Sprintf("%s %d of %d boards flashed.", tui.GlyphOK, len(outcomes), len(outcomes))))
	fmt.Fprintln(a.Out, tui.Muted().Render("Give the board a few seconds to reboot before connecting to it."))
	return nil
}

// describePlan renders a plan for a dry run or confirmation.
func describePlan(p *plan.Plan) string {
	s := fmt.Sprintf("  %s %s\n", tui.Selected().Render(p.Device.Name), tui.Muted().Render("("+p.Device.ID+")"))
	s += fmt.Sprintf("    %-12s %s\n", "target", p.Target.Describe())
	s += fmt.Sprintf("    %-12s %s %s\n", "firmware", p.Project.Name, p.Release.Version)
	if p.Variant() != "" {
		s += fmt.Sprintf("    %-12s %s\n", "variant", p.Variant())
	}
	s += fmt.Sprintf("    %-12s %s\n", "method", string(p.Build.Method))
	s += fmt.Sprintf("    %-12s %s\n", "why", tui.Muted().Render(p.Reason))
	return s
}
