// Package config owns meshflash's on-disk home and operator settings.
//
// Everything lives under a single directory (default ~/.meshflash, overridable
// with MESHFLASH_HOME) rather than scattered across XDG config/cache/data. A
// field kit is often prepared on one machine and copied to another — one
// self-contained, relocatable directory makes that a plain file copy.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DefaultCatalogURL is where `meshflash update` fetches the firmware index.
//
// It is a release asset rather than a raw branch file so the operator can pin
// and roll back, and so a bad generator run does not immediately reach clients.
//
// The asset hangs off a dedicated `catalog` tag rather than /releases/latest/,
// because "latest" tracks the newest non-prerelease release — which is a
// meshflash version, not a catalog, and would shadow this the moment one ships.
const DefaultCatalogURL = "https://github.com/jclement/meshflash/releases/download/catalog/catalog.json"

// Paths resolves the layout of a meshflash home directory.
type Paths struct {
	Home string
}

// Discover returns the active home, honouring MESHFLASH_HOME.
func Discover() (Paths, error) {
	if h := os.Getenv("MESHFLASH_HOME"); h != "" {
		abs, err := filepath.Abs(h)
		if err != nil {
			return Paths{}, fmt.Errorf("resolve MESHFLASH_HOME: %w", err)
		}
		return Paths{Home: abs}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("locate home directory: %w", err)
	}
	return Paths{Home: filepath.Join(home, ".meshflash")}, nil
}

func (p Paths) ConfigFile() string  { return filepath.Join(p.Home, "config.json") }
func (p Paths) CatalogFile() string { return filepath.Join(p.Home, "catalog.json") }
func (p Paths) LogDir() string      { return filepath.Join(p.Home, "logs") }
func (p Paths) CacheDir() string    { return filepath.Join(p.Home, "cache") }
func (p Paths) DownloadDir() string { return filepath.Join(p.CacheDir(), "downloads") }
func (p Paths) ExtractDir() string  { return filepath.Join(p.CacheDir(), "firmware") }

// EnsureDirs creates the directory skeleton.
func (p Paths) EnsureDirs() error {
	for _, d := range []string{p.Home, p.LogDir(), p.DownloadDir(), p.ExtractDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", d, err)
		}
	}
	return nil
}

// Config is the operator's persisted settings.
type Config struct {
	// CatalogURL is where `update` pulls the index from. Overridable so a site
	// can host its own vetted catalog on an internal mirror.
	CatalogURL string `json:"catalog_url"`

	// Devices is the subset of hardware to keep firmware for. Empty means "all",
	// which is rarely what you want: a full Meshtastic release is ~270 MB of
	// platform zips. `meshflash configure` writes this list.
	Devices []string `json:"devices"`

	// Projects limits which upstreams to track. Empty means all.
	Projects []string `json:"projects,omitempty"`

	// Channels selects release channels to cache, e.g. ["stable"].
	Channels []string `json:"channels,omitempty"`

	// KeepVersions is how many releases per project to retain on disk.
	KeepVersions int `json:"keep_versions"`

	// EraseByDefault makes a full-chip erase the default for ESP32 flashes.
	// Left false deliberately: erasing wipes NVS, which on Meshtastic destroys
	// the node's private key and breaks remote admin and PKC DMs.
	EraseByDefault bool `json:"erase_by_default"`
}

// Default returns settings for a fresh install.
func Default() Config {
	return Config{
		CatalogURL:   DefaultCatalogURL,
		Channels:     []string{"stable"},
		KeepVersions: 2,
	}
}

// Load reads config.json, returning defaults when it does not exist yet.
func Load(p Paths) (Config, error) {
	c := Default()
	data, err := os.ReadFile(p.ConfigFile())
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("parse %s: %w", p.ConfigFile(), err)
	}
	if c.CatalogURL == "" {
		c.CatalogURL = DefaultCatalogURL
	}
	if c.KeepVersions <= 0 {
		c.KeepVersions = 2
	}
	c.Devices = dedupeSorted(c.Devices)
	return c, nil
}

// Save writes config.json atomically so an interrupted write cannot leave a
// field unit with an unparseable config.
func Save(p Paths, c Config) error {
	if err := p.EnsureDirs(); err != nil {
		return err
	}
	c.Devices = dedupeSorted(c.Devices)
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	data = append(data, '\n')
	return WriteFileAtomic(p.ConfigFile(), data, 0o644)
}

// WriteFileAtomic writes via a temp file in the same directory then renames.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// WantsDevice reports whether a device is in the operator's selection.
func (c Config) WantsDevice(id string) bool {
	if len(c.Devices) == 0 {
		return true
	}
	for _, d := range c.Devices {
		if d == id {
			return true
		}
	}
	return false
}

// WantsProject reports whether an upstream is tracked.
func (c Config) WantsProject(id string) bool {
	if len(c.Projects) == 0 {
		return true
	}
	for _, p := range c.Projects {
		if p == id {
			return true
		}
	}
	return false
}

// WantsChannel reports whether a release channel is cached.
func (c Config) WantsChannel(ch string) bool {
	if len(c.Channels) == 0 {
		return true
	}
	for _, c2 := range c.Channels {
		if strings.EqualFold(c2, ch) {
			return true
		}
	}
	return false
}

func dedupeSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
