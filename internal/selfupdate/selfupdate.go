// Package selfupdate implements `meshflash upgrade`.
//
// The awkward part is Windows, which refuses to overwrite a running
// executable. The portable solution is to rename the running binary out of the
// way first — renaming a running image is permitted — then move the new one
// into place, so the swap is atomic from the caller's point of view.
package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jclement/meshflash/internal/buildinfo"
)

// Release describes an available meshflash version.
type Release struct {
	Version     string
	Tag         string
	PublishedAt time.Time
	Notes       string
	AssetURL    string
	AssetName   string
	AssetSize   int64
	ChecksumURL string
}

// ErrUpToDate means the running binary is already current.
var ErrUpToDate = errors.New("already running the latest version")

// ErrDevBuild means self-update was declined because this is a local build.
var ErrDevBuild = errors.New("this is a development build; upgrade skipped so a local binary is not overwritten")

type ghRelease struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
		Size int64  `json:"size"`
	} `json:"assets"`
}

// Check queries GitHub for the newest release and the asset for this platform.
func Check(ctx context.Context, client *http.Client) (*Release, error) {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", buildinfo.Repo)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", buildinfo.UserAgent())

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("check for updates: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("no releases published for %s yet", buildinfo.Repo)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("check for updates: GitHub returned %s", resp.Status)
	}

	var gr ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&gr); err != nil {
		return nil, fmt.Errorf("parse release response: %w", err)
	}

	rel := &Release{
		Version:     strings.TrimPrefix(gr.TagName, "v"),
		Tag:         gr.TagName,
		PublishedAt: gr.PublishedAt,
		Notes:       gr.Body,
	}

	want := AssetName(rel.Version)
	for _, a := range gr.Assets {
		switch {
		case a.Name == want:
			rel.AssetURL, rel.AssetName, rel.AssetSize = a.URL, a.Name, a.Size
		case a.Name == "checksums.txt":
			rel.ChecksumURL = a.URL
		}
	}
	if rel.AssetURL == "" {
		return nil, fmt.Errorf("release %s has no build for %s (looked for %s)", rel.Tag, buildinfo.Platform(), want)
	}
	return rel, nil
}

// AssetName is the release asset for the running platform. Release workflows
// must produce exactly these names.
func AssetName(version string) string {
	ext := ".tar.gz"
	if runtime.GOOS == "windows" {
		ext = ".zip"
	}
	return fmt.Sprintf("meshflash_%s_%s_%s%s", version, runtime.GOOS, runtime.GOARCH, ext)
}

// Apply downloads a release and replaces the running executable.
func Apply(ctx context.Context, client *http.Client, rel *Release, onProgress func(cur, total int64)) error {
	if !buildinfo.IsRelease() {
		return ErrDevBuild
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate the running binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}

	// Staging next to the target keeps the final move on one filesystem, which
	// is what makes it atomic.
	dir := filepath.Dir(self)
	tmpDir, err := os.MkdirTemp(dir, ".meshflash-upgrade-*")
	if err != nil {
		return fmt.Errorf("create staging directory next to %s: %w (is the install location writable?)", self, err)
	}
	defer os.RemoveAll(tmpDir)

	archive := filepath.Join(tmpDir, rel.AssetName)
	if err := download(ctx, client, rel.AssetURL, archive, rel.AssetSize, onProgress); err != nil {
		return err
	}

	if rel.ChecksumURL != "" {
		if err := verifyChecksum(ctx, client, rel, archive); err != nil {
			return err
		}
	}

	binary, err := extractBinary(archive, tmpDir)
	if err != nil {
		return err
	}
	if err := os.Chmod(binary, 0o755); err != nil {
		return err
	}

	return replaceExecutable(self, binary)
}

// replaceExecutable swaps the new binary in, tolerating the fact that Windows
// will not let a running image be overwritten.
func replaceExecutable(self, replacement string) error {
	backup := self + ".old"
	_ = os.Remove(backup)

	// Renaming the running image is allowed on every supported platform.
	if err := os.Rename(self, backup); err != nil {
		return fmt.Errorf("move the current binary aside: %w", err)
	}

	if err := os.Rename(replacement, self); err != nil {
		// Put things back rather than leaving the user with no meshflash.
		if rbErr := os.Rename(backup, self); rbErr != nil {
			return fmt.Errorf("install new binary: %w (and restoring the old one failed: %v; it is at %s)", err, rbErr, backup)
		}
		return fmt.Errorf("install new binary: %w", err)
	}

	// On Unix the old inode can go now. On Windows the file is still mapped by
	// this process, so deletion fails; leave it and clean up on the next run.
	if err := os.Remove(backup); err != nil && runtime.GOOS != "windows" {
		return nil // the upgrade succeeded; a stale .old file is cosmetic
	}
	return nil
}

// CleanupStale removes a .old binary left behind by a Windows upgrade.
func CleanupStale() {
	self, err := os.Executable()
	if err != nil {
		return
	}
	_ = os.Remove(self + ".old")
}

func download(ctx context.Context, client *http.Client, url, dest string, expectSize int64, onProgress func(cur, total int64)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", buildinfo.UserAgent())

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", url, resp.Status)
	}

	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()

	total := resp.ContentLength
	if total <= 0 {
		total = expectSize
	}
	var written int64
	buf := make([]byte, 64<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return werr
			}
			written += int64(n)
			if onProgress != nil {
				onProgress(written, total)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("download %s: %w", url, err)
		}
	}
	return f.Sync()
}

// verifyChecksum checks the archive against the release's checksums.txt.
func verifyChecksum(ctx context.Context, client *http.Client, rel *Release, archive string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rel.ChecksumURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", buildinfo.UserAgent())

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch checksums: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch checksums: %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}

	var want string
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == rel.AssetName {
			want = fields[0]
			break
		}
	}
	if want == "" {
		return fmt.Errorf("checksums.txt has no entry for %s", rel.AssetName)
	}

	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("checksum mismatch for %s: got %s, expected %s — refusing to install", rel.AssetName, got, want)
	}
	return nil
}
