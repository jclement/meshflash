package plan

import (
	"errors"
	"testing"
	"time"

	"github.com/jclement/meshflash/internal/bindings"
	"github.com/jclement/meshflash/internal/catalog"
	"github.com/jclement/meshflash/internal/config"
	"github.com/jclement/meshflash/internal/device"
	"github.com/jclement/meshflash/internal/fingerprint"
)

func off(v uint32) *uint32 { return &v }

// testCatalog mirrors the real shape: one board both projects target, one
// board with two MeshCore role variants, and two releases.
func testCatalog() *catalog.Catalog {
	esp := catalog.Build{
		DeviceID: "heltec-v3",
		Method:   catalog.MethodESP32,
		Artifacts: []catalog.Artifact{
			{Role: catalog.RoleApp, Name: "app.bin", Offset: off(0x10000), URL: "https://x/app.bin"},
			{Role: catalog.RoleMerged, Name: "factory.bin", Offset: off(0x0), URL: "https://x/factory.bin"},
		},
	}
	uf2 := catalog.Build{
		DeviceID:  "rak4631",
		Method:    catalog.MethodUF2,
		Artifacts: []catalog.Artifact{{Role: catalog.RoleUF2, Name: "fw.uf2", URL: "https://x/fw.uf2"}},
	}

	return &catalog.Catalog{
		SchemaVersion: catalog.SchemaVersion,
		Devices: []catalog.Device{
			{ID: "heltec-v3", Name: "Heltec V3", Platform: "esp32s3"},
			{ID: "rak4631", Name: "RAK WisBlock 4631", Platform: "nrf52840",
				UF2Board: []string{"nrf52840-rak4631"}},
		},
		Projects: []catalog.Project{
			{
				ID: "meshtastic", Name: "Meshtastic", Repo: "meshtastic/firmware",
				Releases: []catalog.Release{
					{
						Version: "2.7.0", Tag: "v2.7.0", Channel: "stable",
						PublishedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
						Builds:      []catalog.Build{esp, uf2},
					},
					{
						Version: "2.8.0-beta", Tag: "v2.8.0-beta", Channel: "alpha",
						PublishedAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
						Builds:      []catalog.Build{esp, uf2},
					},
				},
			},
			{
				ID: "meshcore", Name: "MeshCore", Repo: "meshcore-dev/MeshCore",
				Releases: []catalog.Release{
					{
						Version: "1.17.1", Tag: "companion-v1.17.1", Channel: "stable",
						PublishedAt: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
						Builds: []catalog.Build{
							withVariant(uf2, "companion_radio_ble"),
							withVariant(uf2, "companion_radio_usb"),
						},
					},
				},
			},
		},
	}
}

func withVariant(b catalog.Build, v string) catalog.Build {
	b.Variant = v
	return b
}

// uf2Target is a board sitting in its bootloader, which is the only case where
// hardware identifies itself unambiguously.
func uf2Target() device.Target {
	vol := device.Volume{
		Path:  "/Volumes/RAK4631",
		Label: "RAK4631",
		Info:  device.UF2Info{BoardID: "nrf52840-rak4631", Fields: map[string]string{"Board-ID": "nrf52840-rak4631"}},
	}
	return device.Target{
		Volume: &vol,
		Candidates: []device.Candidate{{
			DeviceID: "rak4631", Name: "RAK WisBlock 4631",
			Confidence: device.ConfidenceExact, Reason: "INFO_UF2.TXT Board-ID nrf52840-rak4631",
		}},
	}
}

// bridgeTarget is the common bad case: a shared USB-UART bridge matching many
// boards, identifying nothing.
func bridgeTarget() device.Target {
	p := device.Port{Name: "/dev/ttyUSB0", IsUSB: true, VID: "10c4", PID: "ea60"}
	return device.Target{
		Port: &p,
		Candidates: []device.Candidate{
			{DeviceID: "heltec-v3", Name: "Heltec V3", Confidence: device.ConfidencePossible},
			{DeviceID: "tbeam", Name: "T-Beam", Confidence: device.ConfidencePossible},
		},
	}
}

func TestResolvePrefersStableOverNewerAlpha(t *testing.T) {
	cat := testCatalog()
	p, err := Resolve(cat, uf2Target(), Request{ProjectID: "meshtastic"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// 2.8.0-beta is newer but is on the alpha channel, so stable must win.
	if p.Release.Version != "2.7.0" {
		t.Errorf("picked %s, want the stable 2.7.0", p.Release.Version)
	}
	if p.Device.ID != "rak4631" {
		t.Errorf("device = %s, want rak4631", p.Device.ID)
	}
	if p.Build.Method != catalog.MethodUF2 {
		t.Errorf("method = %s, want uf2", p.Build.Method)
	}
}

func TestResolveExplicitVersionOverridesChannel(t *testing.T) {
	cat := testCatalog()
	p, err := Resolve(cat, uf2Target(), Request{ProjectID: "meshtastic", Version: "2.8.0-beta"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if p.Release.Version != "2.8.0-beta" {
		t.Errorf("picked %s, want 2.8.0-beta", p.Release.Version)
	}
}

func TestResolveAmbiguousProject(t *testing.T) {
	cat := testCatalog()
	// Both projects target rak4631, so the project must be asked for.
	_, err := Resolve(cat, uf2Target(), Request{})
	var amb *ErrAmbiguous
	if !errors.As(err, &amb) {
		t.Fatalf("expected ErrAmbiguous, got %v", err)
	}
	if amb.What != "project" {
		t.Errorf("ambiguity is %q, want project", amb.What)
	}
	if len(amb.Choices) != 2 {
		t.Errorf("choices = %v, want both projects", amb.Choices)
	}
}

func TestResolveAmbiguousVariant(t *testing.T) {
	cat := testCatalog()
	_, err := Resolve(cat, uf2Target(), Request{ProjectID: "meshcore"})
	var amb *ErrAmbiguous
	if !errors.As(err, &amb) {
		t.Fatalf("expected ErrAmbiguous, got %v", err)
	}
	if amb.What != "variant" {
		t.Errorf("ambiguity is %q, want variant", amb.What)
	}
}

func TestResolveVariantSelects(t *testing.T) {
	cat := testCatalog()
	p, err := Resolve(cat, uf2Target(), Request{ProjectID: "meshcore", Variant: "companion_radio_usb"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if p.Variant() != "companion_radio_usb" {
		t.Errorf("variant = %q", p.Variant())
	}
}

// A board behind a shared bridge must never be silently resolved: guessing
// between a Heltec V3 and a T-Beam would write the wrong firmware.
func TestResolveRefusesToGuessBehindSharedBridge(t *testing.T) {
	cat := testCatalog()
	_, err := Resolve(cat, bridgeTarget(), Request{ProjectID: "meshtastic"})
	var amb *ErrAmbiguous
	if !errors.As(err, &amb) || amb.What != "device" {
		t.Fatalf("expected a device ambiguity, got %v", err)
	}
}

func TestResolveAutoUsesBinding(t *testing.T) {
	cat := testCatalog()
	store := newTestBindings(t)

	fp := fingerprint.Fingerprint{Kind: fingerprint.KindUSBSerial, Value: "d8f3a1029b44"}
	if err := store.Remember(bindings.Binding{
		Fingerprint: fp,
		DeviceID:    "heltec-v3",
		ProjectID:   "meshtastic",
		LastVersion: "2.6.0",
	}); err != nil {
		t.Fatal(err)
	}

	// Same ambiguous bridge as before, but now with a serial number the store
	// recognises — which is exactly what should break the tie.
	target := bridgeTarget()
	target.Port.SerialNumber = "D8F3A1029B44"

	p, err := ResolveAuto(cat, store, target, Request{})
	if err != nil {
		t.Fatalf("ResolveAuto: %v", err)
	}
	if p.Device.ID != "heltec-v3" {
		t.Errorf("device = %s, want heltec-v3 from the binding", p.Device.ID)
	}
	if p.Project.ID != "meshtastic" {
		t.Errorf("project = %s", p.Project.ID)
	}
	if p.Binding == nil {
		t.Error("plan should carry the binding it came from")
	}
}

func TestResolveAutoUnknownBoard(t *testing.T) {
	cat := testCatalog()
	store := newTestBindings(t)

	_, err := ResolveAuto(cat, store, bridgeTarget(), Request{})
	var unknown *ErrUnknownBoard
	if !errors.As(err, &unknown) {
		t.Fatalf("expected ErrUnknownBoard, got %v", err)
	}
	// A CP2102 with no serial has no stable identity, so `auto` must say so
	// rather than implying a plain re-flash would help.
	if unknown.Fingerprintable {
		t.Error("a board with no usable serial must not be reported as fingerprintable")
	}
}

// A board that identifies itself is safe to resolve without any history,
// provided only one project targets it.
func TestResolveAutoUsesSelfIdentification(t *testing.T) {
	cat := testCatalog()
	// Drop MeshCore so exactly one project targets the board.
	cat.Projects = cat.Projects[:1]

	store := newTestBindings(t)
	p, err := ResolveAuto(cat, store, uf2Target(), Request{})
	if err != nil {
		t.Fatalf("ResolveAuto: %v", err)
	}
	if p.Device.ID != "rak4631" {
		t.Errorf("device = %s", p.Device.ID)
	}
	if p.Reason == "" {
		t.Error("plan should explain why it chose this")
	}
}

func newTestBindings(t *testing.T) *bindings.Store {
	t.Helper()
	s, err := bindings.Load(pathsIn(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func pathsIn(dir string) config.Paths { return config.Paths{Home: dir} }

// A board remembered as running one firmware must still be offered the others,
// or switching between Meshtastic and MeshCore would be impossible.
func TestOptionsListsEveryFirmwareForADevice(t *testing.T) {
	cat := testCatalog()

	opts := Options(cat, "rak4631", Request{})
	if len(opts) != 3 {
		t.Fatalf("got %d options, want 3 (meshtastic + two meshcore roles): %+v", len(opts), opts)
	}

	seen := map[string]bool{}
	for _, o := range opts {
		seen[o.Key()] = true
		if o.Version == "" || o.Method == "" {
			t.Errorf("option %+v is missing a version or method", o)
		}
	}
	for _, want := range []string{
		"meshtastic\x00",
		"meshcore\x00companion_radio_ble",
		"meshcore\x00companion_radio_usb",
	} {
		if !seen[want] {
			t.Errorf("options are missing %q", want)
		}
	}
}

func TestOptionsRespectsProjectFilter(t *testing.T) {
	cat := testCatalog()
	opts := Options(cat, "rak4631", Request{ProjectID: "meshcore"})
	if len(opts) != 2 {
		t.Fatalf("got %d options, want the 2 meshcore roles", len(opts))
	}
	for _, o := range opts {
		if o.ProjectID != "meshcore" {
			t.Errorf("project filter leaked %s", o.ProjectID)
		}
	}
}

// A device only one project targets has nothing to switch to.
func TestOptionsForSingleProjectDevice(t *testing.T) {
	cat := testCatalog()
	opts := Options(cat, "heltec-v3", Request{})
	if len(opts) != 1 || opts[0].ProjectID != "meshtastic" {
		t.Fatalf("got %+v, want just the meshtastic build", opts)
	}
}
