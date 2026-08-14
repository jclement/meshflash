package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"time"

	"github.com/jclement/meshflash/internal/catalog"
	"github.com/jclement/meshflash/internal/device"
	"github.com/jclement/meshflash/internal/doctor"
	"github.com/jclement/meshflash/internal/fingerprint"
	"github.com/jclement/meshflash/internal/probe"
	"github.com/jclement/meshflash/internal/tui"
)

func (a *App) cmdDoctor(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	drivers := fs.Bool("drivers", false, "list drivers worth pre-installing")
	askFirmware := fs.Bool("firmware", false, "ask each attached board what firmware it is running")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *drivers {
		a.printDriverList()
		return nil
	}

	report := doctor.Run(doctor.Options{
		Paths:      a.Paths,
		Cfg:        a.Cfg,
		Catalog:    a.Catalog,
		CatalogErr: a.CatalogErr,
		Store:      a.Store,
	})

	if a.JSON {
		enc := json.NewEncoder(a.Out)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}

	a.printReport(report)

	if *askFirmware {
		a.printFirmware(ctx, report)
	} else if len(report.Targets) > 0 {
		fmt.Fprintln(a.Out, tui.Muted().Render("Run `meshflash doctor --firmware` to ask each board what it is running."))
	}
	return nil
}

// printFirmware asks each attached board what it is running.
//
// Not done by default: it opens the port and exchanges a few messages, and
// while both probes are read-only, `doctor` should stay a passive look at the
// system unless asked otherwise.
func (a *App) printFirmware(ctx context.Context, r doctor.Report) {
	fmt.Fprintln(a.Out)
	fmt.Fprintln(a.Out, tui.Heading().Render("Installed firmware"))

	asked := 0
	for _, t := range r.Targets {
		if t.Port == nil {
			continue
		}
		asked++
		fmt.Fprintf(a.Out, "  %s\n", tui.Selected().Render(t.Port.Name))

		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		fw, err := probe.Identify(pctx, probe.Options{Port: t.Port.Name})
		cancel()

		if err != nil {
			fmt.Fprintf(a.Out, "    %s\n", tui.Muted().Render(
				"did not answer — it may be in a bootloader, asleep, or running something else"))
			a.Log.Debug("firmware probe failed", "port", t.Port.Name, "error", err)
			continue
		}

		fmt.Fprintf(a.Out, "    %-12s %s\n", "firmware", tui.OK().Render(fw.Project+" "+fw.Version))
		if fw.Variant != "" {
			fmt.Fprintf(a.Out, "    %-12s %s\n", "variant", fw.Variant)
		}
		if fw.Board != "" {
			fmt.Fprintf(a.Out, "    %-12s %s\n", "board", fw.Board)
		}
		if fw.NodeName != "" {
			fmt.Fprintf(a.Out, "    %-12s %s\n", "node name", fw.NodeName)
		}
		if fw.Meshtastic != nil && fw.Meshtastic.HWModel != 0 {
			fmt.Fprintf(a.Out, "    %-12s %d\n", "hw_model", fw.Meshtastic.HWModel)
		}
	}
	if asked == 0 {
		fmt.Fprintln(a.Out, tui.Muted().Render("  no serial ports to ask"))
	}
}

func (a *App) printReport(r doctor.Report) {
	fmt.Fprintln(a.Out, tui.Title().Render(" meshflash doctor "))
	fmt.Fprintln(a.Out)

	for _, c := range r.Checks {
		fmt.Fprintf(a.Out, "%s %-26s %s\n", tui.StatusGlyph(string(c.Status)), c.Name, c.Detail)
		if c.Fix != "" {
			fmt.Fprintf(a.Out, "  %s\n", tui.Muted().Render(c.Fix))
		}
	}

	fmt.Fprintln(a.Out)
	fmt.Fprintln(a.Out, tui.Heading().Render("Attached devices"))
	if len(r.Targets) == 0 {
		fmt.Fprintln(a.Out, tui.Muted().Render("  none"))
	}

	for _, t := range r.Targets {
		fmt.Fprintf(a.Out, "\n  %s\n", tui.Selected().Render(t.Describe()))

		if t.Port != nil {
			p := t.Port
			if p.Product != "" {
				fmt.Fprintf(a.Out, "    %-12s %s\n", "product", p.Product)
			}
			if p.SerialNumber != "" {
				fmt.Fprintf(a.Out, "    %-12s %s\n", "serial", p.SerialNumber)
			}
			if name := device.BridgeName(usbOf(*p)); name != "" {
				fmt.Fprintf(a.Out, "    %-12s %s\n", "interface", name)
			}
		}
		if t.Volume != nil && !t.Volume.Info.Empty() {
			v := t.Volume
			if v.Info.Model != "" {
				fmt.Fprintf(a.Out, "    %-12s %s\n", "model", v.Info.Model)
			}
			if v.Info.BoardID != "" {
				fmt.Fprintf(a.Out, "    %-12s %s\n", "board-id", v.Info.BoardID)
			}
			if v.Info.Bootloader != "" {
				fmt.Fprintf(a.Out, "    %-12s %s\n", "bootloader", v.Info.Bootloader)
			}
		}

		// The fingerprint is what `auto` keys on, so surfacing it here makes
		// clear which boards will be recognised next time.
		fp := fingerprint.FromTarget(t)
		if fp.Valid() {
			line := fp.String()
			if b, ok := a.Bindings.Lookup(fp); ok {
				line += tui.OK().Render("  known: " + b.Describe())
			} else {
				line += tui.Muted().Render("  (not yet flashed by meshflash)")
			}
			fmt.Fprintf(a.Out, "    %-12s %s\n", "fingerprint", line)
		} else {
			fmt.Fprintf(a.Out, "    %-12s %s\n", "fingerprint",
				tui.Muted().Render("none — this board publishes no unique serial; `meshflash auto --probe` can read its MAC instead"))
		}

		switch {
		case len(t.Candidates) == 0:
			fmt.Fprintf(a.Out, "    %-12s %s\n", "identified",
				tui.Muted().Render("no — you will be asked which board this is"))
		case t.Resolved():
			c := t.Candidates[0]
			fmt.Fprintf(a.Out, "    %-12s %s %s\n", "identified",
				tui.OK().Render(c.Name), tui.Muted().Render("("+c.Reason+")"))
		default:
			fmt.Fprintf(a.Out, "    %-12s %s\n", "candidates",
				tui.Muted().Render(fmt.Sprintf("%d possible (%s)", len(t.Candidates), t.Candidates[0].Reason)))
			for i, c := range t.Candidates {
				if i >= 5 {
					fmt.Fprintf(a.Out, "      %s\n", tui.Muted().Render(fmt.Sprintf("… and %d more", len(t.Candidates)-i)))
					break
				}
				fmt.Fprintf(a.Out, "      %s %s\n", tui.Muted().Render("·"), c.Name)
			}
		}
	}

	// Every mounted volume that was looked at and turned down. When a board is
	// sitting in its bootloader and meshflash cannot see it, this is the line
	// that says why.
	if len(r.RejectedVolumes) > 0 {
		fmt.Fprintln(a.Out)
		fmt.Fprintln(a.Out, tui.Heading().Render("Other mounted volumes"))
		for _, v := range r.RejectedVolumes {
			fmt.Fprintf(a.Out, "  %s %s\n", tui.Muted().Render(tui.GlyphPending), v.Path)
			fmt.Fprintf(a.Out, "      %s\n", tui.Muted().Render(v.Reason))
		}
	}

	if len(r.MissingDrivers) > 0 {
		fmt.Fprintln(a.Out)
		fmt.Fprintln(a.Out, tui.Heading().Render("Drivers in use"))
		for _, d := range r.MissingDrivers {
			fmt.Fprintf(a.Out, "  %s\n    %s\n    %s\n", d.Chip, tui.Muted().Render(d.Why), tui.Info().Render(d.URL))
		}
	}

	fmt.Fprintln(a.Out)
	switch r.Worst() {
	case doctor.StatusFail:
		fmt.Fprintln(a.Out, tui.Error().Render("Something needs fixing before you can flash."))
	case doctor.StatusWarn:
		fmt.Fprintln(a.Out, tui.Warn().Render("Usable, but see the warnings above."))
	default:
		fmt.Fprintln(a.Out, tui.OK().Render("Ready to flash."))
	}
	fmt.Fprintln(a.Out, tui.Muted().Render("Run `meshflash doctor --drivers` for the drivers worth pre-installing on a field machine."))
}

func (a *App) printDriverList() {
	fmt.Fprintln(a.Out, tui.Title().Render(" USB-UART drivers "))
	fmt.Fprintln(a.Out)
	fmt.Fprintln(a.Out, tui.Muted().Render("Install these on any machine that will flash boards without a network."))
	fmt.Fprintln(a.Out, tui.Muted().Render("On Windows a board with no driver never appears as a serial port at all."))
	fmt.Fprintln(a.Out)

	for _, d := range doctor.CommonDrivers() {
		fmt.Fprintf(a.Out, "  %s\n", tui.Selected().Render(d.Chip))
		fmt.Fprintf(a.Out, "    %s\n", tui.Muted().Render(d.Why))
		fmt.Fprintf(a.Out, "    %s\n\n", tui.Info().Render(d.URL))
	}
}

func usbOf(p device.Port) catalog.USBID { return catalog.USBID{VID: p.VID, PID: p.PID} }
