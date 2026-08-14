// Package bindings remembers what firmware meshflash last wrote to a given
// physical board.
//
// This is what makes `meshflash auto` possible. Board model detection from USB
// IDs is inherently ambiguous, but board *identity* is not: once an operator
// has told meshflash "this board is a RAK4631 running MeshCore companion BLE",
// that answer is recorded against the board's fingerprint and never has to be
// given again. Plugging the same board in later is enough to know exactly what
// to write.
package bindings

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/jclement/meshflash/internal/config"
	"github.com/jclement/meshflash/internal/fingerprint"
)

// FileName is the bindings file inside the meshflash home.
const FileName = "devices.json"

// schemaVersion guards the on-disk format.
const schemaVersion = 1

// Binding records one board and what belongs on it.
type Binding struct {
	// Fingerprint is the stable board identity.
	Fingerprint fingerprint.Fingerprint `json:"fingerprint"`

	// DeviceID is the catalog device this board was confirmed to be. This is
	// the value that is otherwise unknowable from USB enumeration alone.
	DeviceID string `json:"device_id"`

	// ProjectID and Variant record which firmware the operator chose.
	ProjectID string `json:"project_id"`
	Variant   string `json:"variant,omitempty"`

	// Nickname is an operator-supplied label, so a field kit can be described
	// in human terms ("repeater on the north tower").
	Nickname string `json:"nickname,omitempty"`

	// LastVersion is the firmware version last written by meshflash.
	LastVersion string `json:"last_version,omitempty"`
	// LastFlashed is when that happened.
	LastFlashed time.Time `json:"last_flashed,omitempty"`
	// FlashCount is how many times meshflash has written this board.
	FlashCount int `json:"flash_count,omitempty"`

	// Chip is what the transport reported, kept for display.
	Chip string `json:"chip,omitempty"`

	// Notes is free-form operator text.
	Notes string `json:"notes,omitempty"`
}

// Describe renders a short label for lists.
func (b Binding) Describe() string {
	name := b.Nickname
	if name == "" {
		name = b.DeviceID
	}
	if b.Variant != "" {
		return fmt.Sprintf("%s — %s/%s", name, b.ProjectID, b.Variant)
	}
	return fmt.Sprintf("%s — %s", name, b.ProjectID)
}

// file is the on-disk shape.
type file struct {
	SchemaVersion int       `json:"schema_version"`
	UpdatedAt     time.Time `json:"updated_at"`
	Bindings      []Binding `json:"bindings"`
}

// Store holds bindings loaded from disk.
type Store struct {
	path  string
	byKey map[string]Binding
}

// Load reads the bindings file, returning an empty store when absent.
func Load(paths config.Paths) (*Store, error) {
	p := filepath.Join(paths.Home, FileName)
	s := &Store{path: p, byKey: map[string]Binding{}}

	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}

	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	if f.SchemaVersion > schemaVersion {
		return nil, fmt.Errorf("%s was written by a newer meshflash (schema %d); run `meshflash upgrade`", p, f.SchemaVersion)
	}

	for _, b := range f.Bindings {
		if key := b.Fingerprint.Key(); key != "" {
			s.byKey[key] = b
		}
	}
	return s, nil
}

// Lookup finds the binding for a fingerprint.
func (s *Store) Lookup(fp fingerprint.Fingerprint) (Binding, bool) {
	if !fp.Valid() {
		return Binding{}, false
	}
	b, ok := s.byKey[fp.Key()]
	return b, ok
}

// Remember records or updates a binding after a successful flash.
func (s *Store) Remember(b Binding) error {
	key := b.Fingerprint.Key()
	if key == "" {
		return errors.New("cannot remember a board with no stable fingerprint")
	}

	// Preserve operator-supplied fields across re-flashes; only the firmware
	// facts are overwritten.
	if prev, ok := s.byKey[key]; ok {
		if b.Nickname == "" {
			b.Nickname = prev.Nickname
		}
		if b.Notes == "" {
			b.Notes = prev.Notes
		}
		b.FlashCount = prev.FlashCount
	}
	b.FlashCount++
	b.LastFlashed = time.Now().UTC().Truncate(time.Second)

	s.byKey[key] = b
	return nil
}

// Forget removes a binding.
func (s *Store) Forget(fp fingerprint.Fingerprint) bool {
	key := fp.Key()
	if _, ok := s.byKey[key]; !ok {
		return false
	}
	delete(s.byKey, key)
	return true
}

// SetNickname labels a board.
func (s *Store) SetNickname(fp fingerprint.Fingerprint, nickname string) error {
	key := fp.Key()
	b, ok := s.byKey[key]
	if !ok {
		return fmt.Errorf("no board is registered with fingerprint %s", fp)
	}
	b.Nickname = nickname
	s.byKey[key] = b
	return nil
}

// All returns every binding, most recently flashed first.
func (s *Store) All() []Binding {
	out := make([]Binding, 0, len(s.byKey))
	for _, b := range s.byKey {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastFlashed.Equal(out[j].LastFlashed) {
			return out[i].LastFlashed.After(out[j].LastFlashed)
		}
		return out[i].Fingerprint.Key() < out[j].Fingerprint.Key()
	})
	return out
}

// Len is how many boards are known.
func (s *Store) Len() int { return len(s.byKey) }

// Save writes the bindings file atomically.
func (s *Store) Save() error {
	f := file{
		SchemaVersion: schemaVersion,
		UpdatedAt:     time.Now().UTC().Truncate(time.Second),
		Bindings:      s.All(),
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("encode bindings: %w", err)
	}
	return config.WriteFileAtomic(s.path, append(data, '\n'), 0o644)
}

// Path is where the store persists.
func (s *Store) Path() string { return s.path }
