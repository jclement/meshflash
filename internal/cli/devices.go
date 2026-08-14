package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jclement/meshflash/internal/device"
	"github.com/jclement/meshflash/internal/fingerprint"
	"github.com/jclement/meshflash/internal/tui"
)

func (a *App) cmdDevices(ctx context.Context, args []string) error {
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
		args = args[1:]
	}

	switch sub {
	case "list":
		return a.devicesList()
	case "name":
		if len(args) < 2 {
			return fmt.Errorf("usage: meshflash devices name <fingerprint> <nickname>")
		}
		return a.devicesName(args[0], strings.Join(args[1:], " "))
	case "forget":
		if len(args) < 1 {
			return fmt.Errorf("usage: meshflash devices forget <fingerprint>")
		}
		return a.devicesForget(args[0])
	default:
		return fmt.Errorf("unknown subcommand %q; try list, name or forget", sub)
	}
}

func (a *App) devicesList() error {
	all := a.Bindings.All()

	if a.JSON {
		enc := json.NewEncoder(a.Out)
		enc.SetIndent("", "  ")
		return enc.Encode(all)
	}

	if len(all) == 0 {
		fmt.Fprintln(a.Out, tui.Warn().Render("No boards remembered yet."))
		fmt.Fprintln(a.Out, tui.Muted().Render("Flash a board with `meshflash flash` and it will be recorded here."))
		return nil
	}

	// Mark which remembered boards are plugged in right now.
	det, _ := device.Detect()
	attached := map[string]string{}
	for _, t := range device.Identify(det, a.Catalog) {
		if fp := fingerprint.FromTarget(t); fp.Valid() {
			attached[fp.Key()] = t.Address()
		}
	}

	fmt.Fprintln(a.Out, tui.Title().Render(fmt.Sprintf(" %d remembered boards ", len(all))))
	fmt.Fprintln(a.Out)

	for _, b := range all {
		key := b.Fingerprint.Key()
		name := b.Nickname
		if name == "" {
			name = b.DeviceID
		}

		marker := tui.Muted().Render(tui.GlyphPending)
		suffix := ""
		if addr, ok := attached[key]; ok {
			marker = tui.OK().Render(tui.GlyphOK)
			suffix = tui.OK().Render("  attached at " + addr)
		}

		fmt.Fprintf(a.Out, "%s %s%s\n", marker, tui.Selected().Render(name), suffix)
		fmt.Fprintf(a.Out, "    %-12s %s\n", "fingerprint", tui.Muted().Render(key))
		fmt.Fprintf(a.Out, "    %-12s %s\n", "board", b.DeviceID)

		fw := b.ProjectID
		if b.Variant != "" {
			fw += " · " + b.Variant
		}
		if b.LastVersion != "" {
			fw += " · " + b.LastVersion
		}
		fmt.Fprintf(a.Out, "    %-12s %s\n", "firmware", fw)

		if !b.LastFlashed.IsZero() {
			fmt.Fprintf(a.Out, "    %-12s %s %s\n", "last flashed",
				b.LastFlashed.Local().Format("2006-01-02 15:04"),
				tui.Muted().Render(fmt.Sprintf("(%s ago, %d times total)",
					humanDuration(time.Since(b.LastFlashed)), b.FlashCount)))
		}
		if b.Notes != "" {
			fmt.Fprintf(a.Out, "    %-12s %s\n", "notes", tui.Muted().Render(b.Notes))
		}
		fmt.Fprintln(a.Out)
	}

	fmt.Fprintln(a.Out, tui.Muted().Render("Give a board a name:  meshflash devices name <fingerprint> \"north tower repeater\""))
	fmt.Fprintln(a.Out, tui.Muted().Render("Flash every attached board:  meshflash auto"))
	return nil
}

func (a *App) devicesName(key, nickname string) error {
	fp, ok := fingerprint.ParseKey(key)
	if !ok {
		return fmt.Errorf("%q is not a fingerprint; run `meshflash devices` to see them", key)
	}
	if err := a.Bindings.SetNickname(fp, nickname); err != nil {
		return err
	}
	if err := a.Bindings.Save(); err != nil {
		return err
	}
	fmt.Fprintf(a.Out, "%s named %s %q\n", tui.OK().Render(tui.GlyphOK), key, nickname)
	return nil
}

func (a *App) devicesForget(key string) error {
	fp, ok := fingerprint.ParseKey(key)
	if !ok {
		return fmt.Errorf("%q is not a fingerprint; run `meshflash devices` to see them", key)
	}
	if !a.Bindings.Forget(fp) {
		return fmt.Errorf("no board is registered with fingerprint %s", key)
	}
	if err := a.Bindings.Save(); err != nil {
		return err
	}
	fmt.Fprintf(a.Out, "%s forgot %s\n", tui.OK().Render(tui.GlyphOK), key)
	return nil
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "moments"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}
