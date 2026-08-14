package cli

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"strings"

	"github.com/jclement/meshflash/internal/config"
	"github.com/jclement/meshflash/internal/device"
	"github.com/jclement/meshflash/internal/store"
	"github.com/jclement/meshflash/internal/tui"
)

func (a *App) cmdConfigure(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("configure", flag.ContinueOnError)
	add := fs.String("add", "", "comma-separated device ids to add")
	remove := fs.String("remove", "", "comma-separated device ids to remove")
	list := fs.Bool("list", false, "print the current selection and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cat, err := a.requireCatalog()
	if err != nil {
		return err
	}

	if *list {
		a.printSelection()
		return nil
	}

	// Non-interactive edits, for scripting a field kit.
	if *add != "" || *remove != "" {
		return a.editSelection(*add, *remove)
	}

	// Highlight what is plugged in right now.
	det, _ := device.Detect()
	attached := map[string]bool{}
	for _, t := range device.Identify(det, cat) {
		for _, c := range t.Candidates {
			attached[c.DeviceID] = true
		}
	}

	rows := tui.BuildDeviceRows(cat, attached)
	chosen, err := tui.Configure(rows, a.Cfg.Devices)
	if err != nil {
		return err
	}

	a.Cfg.Devices = chosen
	if err := config.Save(a.Paths, a.Cfg); err != nil {
		return err
	}

	fmt.Fprintf(a.Out, "%s saved %d devices to %s\n",
		tui.OK().Render(tui.GlyphOK), len(chosen), a.Paths.ConfigFile())
	a.printSelection()

	if len(chosen) > 0 {
		fmt.Fprintln(a.Out)
		fmt.Fprintln(a.Out, tui.Muted().Render("Run `meshflash update` to download firmware for these boards."))
	}
	return nil
}

func (a *App) editSelection(add, remove string) error {
	cat, err := a.requireCatalog()
	if err != nil {
		return err
	}

	sel := map[string]bool{}
	for _, id := range a.Cfg.Devices {
		sel[id] = true
	}

	var unknown []string
	for _, id := range splitList(add) {
		if _, ok := cat.DeviceByID(id); !ok {
			unknown = append(unknown, id)
			continue
		}
		sel[id] = true
	}
	for _, id := range splitList(remove) {
		delete(sel, id)
	}

	if len(unknown) > 0 {
		return fmt.Errorf("unknown device ids: %s\n\nRun `meshflash configure` to browse the list",
			strings.Join(unknown, ", "))
	}

	out := make([]string, 0, len(sel))
	for id := range sel {
		out = append(out, id)
	}
	sort.Strings(out)

	a.Cfg.Devices = out
	if err := config.Save(a.Paths, a.Cfg); err != nil {
		return err
	}
	a.printSelection()
	return nil
}

func (a *App) printSelection() {
	if len(a.Cfg.Devices) == 0 {
		fmt.Fprintln(a.Out, tui.Warn().Render("No devices selected."))
		fmt.Fprintln(a.Out, tui.Muted().Render("Run `meshflash configure` to choose the boards you carry."))
		return
	}

	fmt.Fprintln(a.Out)
	fmt.Fprintln(a.Out, tui.Heading().Render(fmt.Sprintf("Selected devices (%d)", len(a.Cfg.Devices))))
	for _, id := range a.Cfg.Devices {
		name := id
		platform := ""
		if a.Catalog != nil {
			if d, ok := a.Catalog.DeviceByID(id); ok {
				name, platform = d.Name, d.Platform
			}
		}
		fmt.Fprintf(a.Out, "  %s %-24s %s %s\n",
			tui.OK().Render(tui.GlyphOK), id, name, tui.Muted().Render(platform))
	}

	if a.Catalog != nil {
		wanted := store.WantedArtifacts(a.Catalog,
			a.Cfg.WantsDevice, a.Cfg.WantsProject, a.Cfg.WantsChannel, a.Cfg.KeepVersions)
		fmt.Fprintf(a.Out, "\n  %s\n", tui.Muted().Render(fmt.Sprintf(
			"%d artifacts across the newest %d releases, about %s to download",
			len(wanted), a.Cfg.KeepVersions, store.FormatBytes(store.DownloadBytes(wanted)))))
	}
}

func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
