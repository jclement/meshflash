// Package plan turns "this board is plugged in" into "write exactly these
// bytes to these offsets".
//
// It is the join point between the four things meshflash knows: what is
// attached (device), what firmware exists (catalog), what is on disk (store),
// and what this particular board was last flashed with (bindings).
package plan

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jclement/meshflash/internal/bindings"
	"github.com/jclement/meshflash/internal/catalog"
	"github.com/jclement/meshflash/internal/device"
	"github.com/jclement/meshflash/internal/fingerprint"
	"github.com/jclement/meshflash/internal/store"
)

// Plan is a fully resolved flash operation.
type Plan struct {
	Target      device.Target
	Device      catalog.Device
	Project     *catalog.Project
	Release     *catalog.Release
	Build       catalog.Build
	Fingerprint fingerprint.Fingerprint

	// Binding is the remembered record for this board, when one exists.
	Binding *bindings.Binding

	// Reason explains how this plan was arrived at, so `auto` can show its
	// working rather than silently flashing something.
	Reason string
}

// Summary renders a one-line description.
func (p Plan) Summary() string {
	v := p.Variant()
	if v != "" {
		v = " " + v
	}
	return fmt.Sprintf("%s → %s %s%s (%s)",
		p.Target.Describe(), p.Project.Name, p.Release.Version, v, p.Build.Method)
}

// Variant is the build variant, or "" when the project ships one build.
func (p Plan) Variant() string { return p.Build.Variant }

// Artifacts lists what must be present locally to run this plan.
func (p Plan) Artifacts() []catalog.Artifact { return p.Build.Artifacts }

// Request describes what the caller wants, with empty fields meaning "decide
// for me".
type Request struct {
	DeviceID  string
	ProjectID string
	Variant   string
	Version   string
	Channel   string
}

// ErrAmbiguous means the request matched more than one build and the caller
// must narrow it. It carries the choices so a UI can present them.
type ErrAmbiguous struct {
	What    string
	Choices []string
}

func (e *ErrAmbiguous) Error() string {
	return fmt.Sprintf("%s is ambiguous; choose one of: %s", e.What, strings.Join(e.Choices, ", "))
}

// ErrUnknownBoard means the attached board could not be identified and has no
// remembered binding.
type ErrUnknownBoard struct {
	Target device.Target
	// Fingerprintable reports whether the board has a stable identity, and so
	// whether flashing it once would let `auto` handle it next time.
	Fingerprintable bool
}

func (e *ErrUnknownBoard) Error() string {
	return fmt.Sprintf("cannot identify %s", e.Target.Describe())
}

// Option is one firmware a device can be flashed with: a project, a variant
// and the release those resolve to.
type Option struct {
	ProjectID   string
	ProjectName string
	Variant     string
	Version     string
	Method      catalog.Method
}

// Key is the stable identifier a picker returns.
func (o Option) Key() string { return o.ProjectID + "\x00" + o.Variant }

// Label renders the choice for a human.
func (o Option) Label() string {
	if o.Variant == "" {
		return o.ProjectName
	}
	return o.ProjectName + " · " + o.Variant
}

// Options lists every firmware available for a device, newest release per
// project and variant.
//
// This is what makes switching firmware possible: a board remembered as
// running Meshtastic can be shown the MeshCore builds that also target it,
// rather than being silently re-flashed with what it had last time.
func Options(cat *catalog.Catalog, deviceID string, req Request) []Option {
	var out []Option

	for pi := range cat.Projects {
		p := &cat.Projects[pi]
		if req.ProjectID != "" && p.ID != req.ProjectID {
			continue
		}
		rel, err := pickRelease(p, deviceID, req)
		if err != nil {
			continue
		}
		for _, b := range rel.BuildsForDevice(deviceID) {
			out = append(out, Option{
				ProjectID:   p.ID,
				ProjectName: p.Name,
				Variant:     b.Variant,
				Version:     rel.Version,
				Method:      b.Method,
			})
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].ProjectID != out[j].ProjectID {
			return out[i].ProjectID < out[j].ProjectID
		}
		return out[i].Variant < out[j].Variant
	})
	return out
}

// Resolve builds a plan for one target from an explicit request.
func Resolve(cat *catalog.Catalog, t device.Target, req Request) (*Plan, error) {
	dev, err := resolveDevice(cat, t, req.DeviceID)
	if err != nil {
		return nil, err
	}

	proj, rel, build, err := resolveBuild(cat, dev.ID, req)
	if err != nil {
		return nil, err
	}

	return &Plan{
		Target:      t,
		Device:      dev,
		Project:     proj,
		Release:     rel,
		Build:       build,
		Fingerprint: fingerprint.FromTarget(t),
		Reason:      "explicit selection",
	}, nil
}

// ResolveAuto builds a plan using only what is already known about the board.
//
// It succeeds when either the board carries a remembered binding, or it
// identified itself unambiguously (a UF2 bootloader's Board-ID) and only one
// project targets it. Anything less returns ErrUnknownBoard rather than
// guessing — writing the wrong firmware is far worse than asking.
func ResolveAuto(cat *catalog.Catalog, bind *bindings.Store, t device.Target, req Request) (*Plan, error) {
	fp := fingerprint.FromTarget(t)

	if b, ok := bind.Lookup(fp); ok {
		p, err := planFromBinding(cat, t, fp, b, req)
		if err != nil {
			return nil, err
		}
		return p, nil
	}

	// No memory of this board. Fall back to self-identification, which only
	// UF2 bootloaders provide.
	if t.Resolved() {
		c, _ := t.BestCandidate()
		dev, ok := cat.DeviceByID(c.DeviceID)
		if !ok {
			return nil, fmt.Errorf("catalog has no device %q", c.DeviceID)
		}
		proj, rel, build, err := resolveBuild(cat, dev.ID, req)
		if err != nil {
			return nil, err
		}
		return &Plan{
			Target:      t,
			Device:      dev,
			Project:     proj,
			Release:     rel,
			Build:       build,
			Fingerprint: fp,
			Reason:      "board identified itself: " + c.Reason,
		}, nil
	}

	return nil, &ErrUnknownBoard{Target: t, Fingerprintable: fp.Valid()}
}

// planFromBinding rebuilds a plan from a remembered board.
func planFromBinding(cat *catalog.Catalog, t device.Target, fp fingerprint.Fingerprint, b bindings.Binding, req Request) (*Plan, error) {
	dev, ok := cat.DeviceByID(b.DeviceID)
	if !ok {
		return nil, fmt.Errorf("this board is remembered as %q, which is no longer in the catalog; re-flash it explicitly to update the record", b.DeviceID)
	}

	// The binding supplies the answers the operator already gave; an explicit
	// request still wins, so `auto --version` can roll a fleet forward.
	eff := req
	if eff.ProjectID == "" {
		eff.ProjectID = b.ProjectID
	}
	if eff.Variant == "" {
		eff.Variant = b.Variant
	}

	proj, rel, build, err := resolveBuild(cat, dev.ID, eff)
	if err != nil {
		return nil, err
	}

	reason := fmt.Sprintf("remembered board (%s)", fp)
	if b.LastVersion != "" {
		reason += ", last flashed " + b.LastVersion
	}

	binding := b
	return &Plan{
		Target:      t,
		Device:      dev,
		Project:     proj,
		Release:     rel,
		Build:       build,
		Fingerprint: fp,
		Binding:     &binding,
		Reason:      reason,
	}, nil
}

// resolveDevice determines which catalog device a target is.
func resolveDevice(cat *catalog.Catalog, t device.Target, deviceID string) (catalog.Device, error) {
	if deviceID != "" {
		d, ok := cat.DeviceByID(deviceID)
		if !ok {
			return catalog.Device{}, fmt.Errorf("unknown device %q; run `meshflash configure` to see the list", deviceID)
		}
		return d, nil
	}

	if t.Resolved() {
		c, _ := t.BestCandidate()
		d, ok := cat.DeviceByID(c.DeviceID)
		if !ok {
			return catalog.Device{}, fmt.Errorf("catalog has no device %q", c.DeviceID)
		}
		return d, nil
	}

	choices := make([]string, 0, len(t.Candidates))
	for _, c := range t.Candidates {
		choices = append(choices, c.DeviceID)
	}
	if len(choices) == 0 {
		return catalog.Device{}, &ErrUnknownBoard{Target: t, Fingerprintable: fingerprint.FromTarget(t).Valid()}
	}
	return catalog.Device{}, &ErrAmbiguous{What: "device", Choices: choices}
}

// resolveBuild picks the project, release and build for a device.
func resolveBuild(cat *catalog.Catalog, deviceID string, req Request) (*catalog.Project, *catalog.Release, catalog.Build, error) {
	// Which projects ship firmware for this board at all?
	var projects []*catalog.Project
	for i := range cat.Projects {
		p := &cat.Projects[i]
		if req.ProjectID != "" && p.ID != req.ProjectID {
			continue
		}
		if projectTargets(p, deviceID) {
			projects = append(projects, p)
		}
	}

	switch {
	case len(projects) == 0:
		if req.ProjectID != "" {
			return nil, nil, catalog.Build{}, fmt.Errorf("%s has no firmware for %s", req.ProjectID, deviceID)
		}
		return nil, nil, catalog.Build{}, fmt.Errorf("no project in the catalog has firmware for %s", deviceID)
	case len(projects) > 1:
		names := make([]string, 0, len(projects))
		for _, p := range projects {
			names = append(names, p.ID)
		}
		sort.Strings(names)
		return nil, nil, catalog.Build{}, &ErrAmbiguous{What: "project", Choices: names}
	}
	proj := projects[0]

	rel, err := pickRelease(proj, deviceID, req)
	if err != nil {
		return nil, nil, catalog.Build{}, err
	}

	builds := rel.BuildsForDevice(deviceID)
	if req.Variant != "" {
		filtered := builds[:0:0]
		for _, b := range builds {
			if strings.EqualFold(b.Variant, req.Variant) {
				filtered = append(filtered, b)
			}
		}
		if len(filtered) == 0 {
			return nil, nil, catalog.Build{}, fmt.Errorf("%s %s has no %q build for %s",
				proj.ID, rel.Version, req.Variant, deviceID)
		}
		builds = filtered
	}

	switch {
	case len(builds) == 0:
		return nil, nil, catalog.Build{}, fmt.Errorf("%s %s has no build for %s", proj.ID, rel.Version, deviceID)
	case len(builds) > 1:
		names := make([]string, 0, len(builds))
		for _, b := range builds {
			names = append(names, b.Label())
		}
		sort.Strings(names)
		return nil, nil, catalog.Build{}, &ErrAmbiguous{What: "variant", Choices: names}
	}

	return proj, rel, builds[0], nil
}

// pickRelease chooses a release, honouring an explicit version or falling back
// to the newest one that actually has a build for this board.
func pickRelease(proj *catalog.Project, deviceID string, req Request) (*catalog.Release, error) {
	if req.Version != "" {
		r, ok := proj.ReleaseByVersion(req.Version)
		if !ok {
			return nil, fmt.Errorf("%s has no release %q in the catalog", proj.ID, req.Version)
		}
		if len(r.BuildsForDevice(deviceID)) == 0 {
			return nil, fmt.Errorf("%s %s has no build for %s", proj.ID, r.Version, deviceID)
		}
		return r, nil
	}

	candidates := make([]*catalog.Release, 0, len(proj.Releases))
	for i := range proj.Releases {
		r := &proj.Releases[i]
		if req.Channel != "" && !strings.EqualFold(r.Channel, req.Channel) {
			continue
		}
		if len(r.BuildsForDevice(deviceID)) > 0 {
			candidates = append(candidates, r)
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("%s has no release with a build for %s", proj.ID, deviceID)
	}

	sort.Slice(candidates, func(i, j int) bool {
		// Prefer stable, then newest.
		si, sj := candidates[i].Channel == "stable", candidates[j].Channel == "stable"
		if si != sj {
			return si
		}
		return candidates[i].PublishedAt.After(candidates[j].PublishedAt)
	})
	return candidates[0], nil
}

func projectTargets(p *catalog.Project, deviceID string) bool {
	for i := range p.Releases {
		if len(p.Releases[i].BuildsForDevice(deviceID)) > 0 {
			return true
		}
	}
	return false
}

// LoadPayloads fetches every artifact a plan needs and returns them keyed by
// artifact name, ready to hand to the flash layer.
func LoadPayloads(ctx context.Context, s *store.Store, p *Plan, onProgress store.ProgressFunc) (map[string][]byte, error) {
	out := make(map[string][]byte, len(p.Build.Artifacts))
	for _, a := range p.Build.Artifacts {
		data, err := s.Read(ctx, a, onProgress)
		if err != nil {
			if errors.Is(err, store.ErrNotCached) {
				return nil, fmt.Errorf("%w\n\nRun `meshflash update` while online to cache firmware for %s", err, p.Device.ID)
			}
			return nil, err
		}
		out[a.Name] = data
	}
	return out, nil
}
