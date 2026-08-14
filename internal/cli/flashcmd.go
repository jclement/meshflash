package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"sort"
	"strings"

	"github.com/jclement/meshflash/internal/catalog"
	"github.com/jclement/meshflash/internal/device"
	"github.com/jclement/meshflash/internal/plan"
	"github.com/jclement/meshflash/internal/tui"
)

func (a *App) cmdFlash(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("flash", flag.ContinueOnError)
	port := fs.String("port", "", "serial port or bootloader volume")
	deviceID := fs.String("device", "", "catalog device id")
	projectID := fs.String("project", "", "meshtastic or meshcore")
	variant := fs.String("variant", "", "build variant")
	version := fs.String("version", "", "firmware version")
	erase := fs.Bool("erase", false, "full chip erase first (ESP32 only)")
	verify := fs.Bool("verify", false, "read back and compare after writing")
	noAuto := fs.Bool("no-auto-bootloader", false, "do not reboot into the bootloader automatically")
	remember := fs.Bool("remember", true, "record this board for `meshflash auto`")
	auto := fs.Bool("auto", false, "use the firmware this board had last time, without prompting")
	dryRun := fs.Bool("dry-run", false, "show what would be flashed and stop")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cat, err := a.requireCatalog()
	if err != nil {
		return err
	}

	det, detErrs := device.Detect()
	for _, e := range detErrs {
		a.Log.Warn("device detection", "error", e)
	}
	targets := device.Identify(det, cat)
	if len(targets) == 0 {
		return errors.New("no devices found.\n\nCheck the USB cable carries data, then run `meshflash doctor`")
	}

	target, err := a.chooseTarget(targets, *port)
	if err != nil {
		return err
	}

	req := plan.Request{
		DeviceID:  *deviceID,
		ProjectID: *projectID,
		Variant:   *variant,
		Version:   *version,
	}

	p, err := a.resolveInteractively(cat, target, req, *auto)
	if err != nil {
		return err
	}

	fmt.Fprintln(a.Out)
	fmt.Fprintln(a.Out, tui.Heading().Render("Ready to flash"))
	fmt.Fprint(a.Out, describePlan(p))

	if *dryRun {
		fmt.Fprintln(a.Out, tui.Muted().Render("\nDry run — nothing was written."))
		return nil
	}

	return a.runPlans(ctx, []*plan.Plan{p}, flashOptions{
		Erase:          *erase,
		Verify:         *verify,
		AutoBootloader: !*noAuto,
		Remember:       *remember,
	})
}

// chooseTarget picks the device to flash, prompting when there is more than one.
func (a *App) chooseTarget(targets []device.Target, want string) (device.Target, error) {
	if want != "" {
		for _, t := range targets {
			if t.Address() == want || (t.Port != nil && t.Port.Name == want) {
				return t, nil
			}
		}
		available := make([]string, 0, len(targets))
		for _, t := range targets {
			available = append(available, t.Address())
		}
		return device.Target{}, fmt.Errorf("no attached device at %q.\n\nAttached: %s",
			want, strings.Join(available, ", "))
	}

	if len(targets) == 1 {
		return targets[0], nil
	}

	choices := make([]tui.Choice, 0, len(targets))
	for _, t := range targets {
		detail := ""
		if c, ok := t.BestCandidate(); ok {
			if t.Resolved() {
				detail = c.Name
			} else {
				detail = fmt.Sprintf("%d possible boards", len(t.Candidates))
			}
		} else {
			detail = "unidentified"
		}
		if t.InBootloader() {
			detail += " · in bootloader"
		}
		choices = append(choices, tui.Choice{Key: t.Address(), Title: t.Describe(), Detail: detail})
	}

	key, err := a.pick("Choose a device", "Which board do you want to flash?", choices)
	if err != nil {
		return device.Target{}, err
	}
	for _, t := range targets {
		if t.Address() == key {
			return t, nil
		}
	}
	return device.Target{}, fmt.Errorf("device %q disappeared", key)
}

// resolveInteractively resolves a plan, prompting to break each ambiguity.
//
// The prompts are the whole point: USB enumeration usually cannot tell a
// Heltec V3 from a T-Beam, so meshflash asks rather than guessing — and then
// remembers the answer so it never has to ask about that board again.
func (a *App) resolveInteractively(cat *catalog.Catalog, target device.Target, req plan.Request, auto bool) (*plan.Plan, error) {
	// A remembered board resolves without any prompting under --auto. Without
	// it, recognition only supplies the default: re-flashing a node with
	// different firmware is a normal thing to want, and silently repeating
	// last time's choice makes that impossible.
	if req.DeviceID == "" && req.ProjectID == "" {
		if p, err := plan.ResolveAuto(cat, a.Bindings, target, req); err == nil {
			if auto {
				fmt.Fprintf(a.Out, "%s %s\n", tui.OK().Render(tui.GlyphOK),
					tui.Muted().Render("Recognised: "+p.Reason))
				return p, nil
			}
			return a.chooseFirmware(cat, target, req, p)
		}
	}

	for attempt := 0; attempt < 4; attempt++ {
		p, err := plan.Resolve(cat, target, req)
		if err == nil {
			return p, nil
		}

		var ambiguous *plan.ErrAmbiguous
		var unknown *plan.ErrUnknownBoard

		switch {
		case errors.As(err, &ambiguous):
			choice, perr := a.promptAmbiguity(cat, target, ambiguous)
			if perr != nil {
				return nil, perr
			}
			switch ambiguous.What {
			case "device":
				req.DeviceID = choice
			case "project":
				req.ProjectID = choice
			case "variant":
				req.Variant = choice
			}

		case errors.As(err, &unknown):
			// Nothing about the board is knowable, so offer the whole list.
			choice, perr := a.promptDeviceList(cat, target)
			if perr != nil {
				return nil, perr
			}
			req.DeviceID = choice

		default:
			return nil, err
		}
	}
	return nil, errors.New("could not resolve which firmware to write")
}

// chooseFirmware offers every firmware that targets a recognised board, with
// what it ran last time first and marked.
func (a *App) chooseFirmware(cat *catalog.Catalog, target device.Target, req plan.Request, known *plan.Plan) (*plan.Plan, error) {
	options := plan.Options(cat, known.Device.ID, req)
	if len(options) <= 1 {
		// Nothing to switch to; take the recognised answer.
		fmt.Fprintf(a.Out, "%s %s\n", tui.OK().Render(tui.GlyphOK),
			tui.Muted().Render("Recognised: "+known.Reason))
		return known, nil
	}

	lastKey := known.Project.ID + "\x00" + known.Variant()

	// Put the previous firmware at the top so enter repeats it.
	sort.SliceStable(options, func(i, j int) bool {
		return options[i].Key() == lastKey && options[j].Key() != lastKey
	})

	choices := make([]tui.Choice, 0, len(options))
	for _, o := range options {
		detail := o.Version + " · " + string(o.Method)
		if o.Key() == lastKey {
			detail += "  ← currently installed"
		}
		choices = append(choices, tui.Choice{Key: o.Key(), Title: o.Label(), Detail: detail})
	}

	name := known.Device.Name
	if b, ok := a.Bindings.Lookup(known.Fingerprint); ok && b.Nickname != "" {
		name = b.Nickname + " (" + known.Device.Name + ")"
	}

	key, err := a.pick("Which firmware?",
		fmt.Sprintf("%s — last flashed with %s.", name, known.Project.Name), choices)
	if err != nil {
		return nil, err
	}

	for _, o := range options {
		if o.Key() != key {
			continue
		}
		req.DeviceID = known.Device.ID
		req.ProjectID = o.ProjectID
		req.Variant = o.Variant

		p, err := plan.Resolve(cat, target, req)
		if err != nil {
			return nil, err
		}
		p.Fingerprint = known.Fingerprint
		p.Binding = known.Binding
		if o.Key() == lastKey {
			p.Reason = known.Reason
		} else {
			p.Reason = fmt.Sprintf("switching from %s to %s", known.Project.Name, o.Label())
		}
		return p, nil
	}
	return nil, fmt.Errorf("firmware choice %q disappeared", key)
}

func (a *App) promptAmbiguity(cat *catalog.Catalog, target device.Target, amb *plan.ErrAmbiguous) (string, error) {
	choices := make([]tui.Choice, 0, len(amb.Choices))
	for _, key := range amb.Choices {
		c := tui.Choice{Key: key, Title: key}
		if amb.What == "device" {
			if d, ok := cat.DeviceByID(key); ok {
				c.Title = d.Name
				c.Detail = d.ID + " · " + d.Platform
			}
			// Show why this board is a candidate at all.
			for _, cand := range target.Candidates {
				if cand.DeviceID == key {
					c.Detail += " · " + cand.Confidence.String()
				}
			}
		}
		choices = append(choices, c)
	}

	prompt := map[string]string{
		"device":  "USB enumeration cannot tell these boards apart. Which one is it?",
		"project": "Which firmware do you want on this board?",
		"variant": "Which build variant?",
	}[amb.What]

	return a.pick("Choose a "+amb.What, prompt, choices)
}

// promptDeviceList offers every device the catalog knows, filtered to the
// operator's selection when they have one.
func (a *App) promptDeviceList(cat *catalog.Catalog, target device.Target) (string, error) {
	ids := cat.DeviceIDs()

	var choices []tui.Choice
	for _, id := range ids {
		if len(a.Cfg.Devices) > 0 && !a.Cfg.WantsDevice(id) {
			continue
		}
		d, ok := cat.DeviceByID(id)
		if !ok {
			continue
		}
		choices = append(choices, tui.Choice{
			Key:    id,
			Title:  d.Name,
			Detail: id,
			Group:  d.Platform,
		})
	}

	// If the selection filtered everything out, fall back to the full list
	// rather than presenting an empty picker.
	if len(choices) == 0 {
		for _, id := range ids {
			d, _ := cat.DeviceByID(id)
			choices = append(choices, tui.Choice{Key: id, Title: d.Name, Detail: id, Group: d.Platform})
		}
	}
	sort.SliceStable(choices, func(i, j int) bool {
		if choices[i].Group != choices[j].Group {
			return choices[i].Group < choices[j].Group
		}
		return choices[i].Title < choices[j].Title
	})

	return a.pick("Which board is this?",
		fmt.Sprintf("%s did not identify itself.", target.Describe()), choices)
}

// pick runs an interactive chooser with console logging silenced, so a log
// line cannot land in the middle of the rendered list.
func (a *App) pick(title, prompt string, choices []tui.Choice) (string, error) {
	a.Session.MuteConsole(true)
	defer a.Session.MuteConsole(false)
	return tui.Pick(title, prompt, choices)
}
