package catalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// MaxCatalogBytes caps what we will read from a catalog URL. The real file is
// a few hundred KB; this stops a misconfigured URL from filling a field disk.
const MaxCatalogBytes = 32 << 20

// Load reads and validates a catalog from disk.
func Load(path string) (*Catalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read catalog: %w", err)
	}
	return Parse(data)
}

// Parse decodes and validates catalog bytes.
func Parse(data []byte) (*Catalog, error) {
	var c Catalog
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields() // a catalog we half-understand is worse than a clear error
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse catalog: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	c.normalize()
	return &c, nil
}

func (c *Catalog) normalize() {
	for i := range c.Devices {
		for j := range c.Devices[i].USB {
			c.Devices[i].USB[j] = c.Devices[i].USB[j].Normalize()
		}
	}
}

// Save writes a catalog to disk as indented JSON.
func Save(path string, c *Catalog) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode catalog: %w", err)
	}
	return writeAtomic(path, append(data, '\n'))
}

// Fetch downloads a catalog, validates it, and returns it alongside the raw
// bytes so the caller can persist exactly what was verified.
func Fetch(ctx context.Context, client *http.Client, url, userAgent string) (*Catalog, []byte, error) {
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/json")
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch catalog: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("fetch catalog: %s returned %s", url, resp.Status)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxCatalogBytes+1))
	if err != nil {
		return nil, nil, fmt.Errorf("read catalog body: %w", err)
	}
	if int64(len(data)) > MaxCatalogBytes {
		return nil, nil, fmt.Errorf("catalog exceeds %d bytes", MaxCatalogBytes)
	}

	c, err := Parse(data)
	if err != nil {
		return nil, nil, err
	}
	return c, data, nil
}

// Digest returns the SHA-256 of catalog bytes, used to report whether an
// `update` actually changed anything.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
