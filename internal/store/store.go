// Package store manages the on-disk firmware cache.
//
// The cache is what makes offline field flashing work: `meshflash update`
// fills it while a network is available, and `meshflash flash` reads only from
// it. Upstream ships per-platform archives (a single 170 MB esp32s3 zip serves
// dozens of boards), so the store downloads an archive once and extracts just
// the members the operator's device selection actually needs.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/jclement/meshflash/internal/catalog"
	"github.com/jclement/meshflash/internal/config"
)

// ErrNotCached means an artifact is needed but absent, and no network is
// available or allowed. This is the error the field workflow must produce a
// good message for.
var ErrNotCached = errors.New("firmware is not in the local cache")

// Store reads and writes the firmware cache.
type Store struct {
	paths  config.Paths
	client *http.Client
	log    *slog.Logger

	// Offline refuses any network access, so a field unit fails fast and
	// clearly rather than hanging on a dead link.
	Offline bool
	// UserAgent identifies meshflash to GitHub.
	UserAgent string
}

// New creates a Store rooted at the given paths.
func New(paths config.Paths, log *slog.Logger) *Store {
	if log == nil {
		log = slog.Default()
	}
	return &Store{
		paths: paths,
		log:   log,
		client: &http.Client{
			// No overall timeout: a 170 MB archive on a field connection can
			// legitimately take a long time. Stalls are caught by the
			// per-read deadline in the transport instead.
			Transport: &http.Transport{
				ResponseHeaderTimeout: 60 * time.Second,
				TLSHandshakeTimeout:   30 * time.Second,
			},
		},
	}
}

// Progress reports long-running work to the UI.
type Progress struct {
	Stage   string // "download", "extract", "verify"
	Name    string
	Current int64
	Total   int64
}

// Percent returns completion as 0..100, or -1 when the total is unknown.
func (p Progress) Percent() float64 {
	if p.Total <= 0 {
		return -1
	}
	return float64(p.Current) / float64(p.Total) * 100
}

// ProgressFunc receives Progress updates.
type ProgressFunc func(Progress)

// --- cache paths ----------------------------------------------------------

// urlKey derives a stable directory name from a URL. Upstream file names
// repeat across releases, so the URL is what actually identifies a download.
func urlKey(u string) string {
	sum := sha256.Sum256([]byte(u))
	return hex.EncodeToString(sum[:])[:16]
}

// archivePath is where a downloaded file lands.
func (s *Store) archivePath(u string) string {
	base := sanitize(path.Base(u))
	if base == "" || base == "." {
		base = "download"
	}
	return filepath.Join(s.paths.DownloadDir(), urlKey(u)+"-"+base)
}

// memberPath is where an extracted archive member lands.
func (s *Store) memberPath(archiveURL, member string) string {
	return filepath.Join(s.paths.ExtractDir(), urlKey(archiveURL), filepath.FromSlash(sanitizeRel(member)))
}

// LocalPath returns where an artifact's usable bytes live once cached.
func (s *Store) LocalPath(a catalog.Artifact) string {
	if a.Packed() {
		return s.memberPath(a.Archive, a.ArchivePath)
	}
	return s.archivePath(a.URL)
}

// Cached reports whether an artifact is ready to flash from disk.
func (s *Store) Cached(a catalog.Artifact) bool {
	st, err := os.Stat(s.LocalPath(a))
	return err == nil && !st.IsDir() && st.Size() > 0
}

// ArchiveCached reports whether the source archive an artifact was extracted
// from is still on disk, and therefore whether pruning would reclaim anything.
func (s *Store) ArchiveCached(a catalog.Artifact) bool {
	if !a.Packed() {
		return false
	}
	st, err := os.Stat(s.archivePath(a.Archive))
	return err == nil && !st.IsDir() && st.Size() > 0
}

// --- fetching -------------------------------------------------------------

// Ensure makes an artifact available locally and returns its path.
func (s *Store) Ensure(ctx context.Context, a catalog.Artifact, onProgress ProgressFunc) (string, error) {
	local := s.LocalPath(a)
	if s.Cached(a) {
		if err := s.verify(local, a, onProgress); err == nil {
			return local, nil
		}
		// A checksum mismatch means a torn or corrupted cache entry. Drop it
		// and re-fetch rather than flashing bad bytes onto a device.
		s.log.Warn("cached artifact failed verification; re-fetching", "artifact", a.Name)
		_ = os.Remove(local)
	}

	if s.Offline {
		return "", fmt.Errorf("%w: %s (run `meshflash update` while online)", ErrNotCached, a.Name)
	}

	if a.Packed() {
		if err := s.ensureExtracted(ctx, a, onProgress); err != nil {
			return "", err
		}
	} else {
		if _, err := s.download(ctx, a.URL, a.Size, onProgress); err != nil {
			return "", err
		}
	}

	if err := s.verify(local, a, onProgress); err != nil {
		return "", err
	}
	return local, nil
}

// Read returns an artifact's contents, fetching it if necessary.
func (s *Store) Read(ctx context.Context, a catalog.Artifact, onProgress ProgressFunc) ([]byte, error) {
	p, err := s.Ensure(ctx, a, onProgress)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

// ensureExtracted downloads the containing archive if needed, then pulls out
// the one member the artifact refers to.
func (s *Store) ensureExtracted(ctx context.Context, a catalog.Artifact, onProgress ProgressFunc) error {
	archive, err := s.download(ctx, a.Archive, 0, onProgress)
	if err != nil {
		return err
	}
	dest := s.memberPath(a.Archive, a.ArchivePath)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}

	if onProgress != nil {
		onProgress(Progress{Stage: "extract", Name: a.Name})
	}
	return extractMember(archive, a.ArchivePath, dest)
}

// download fetches a URL into the cache, skipping the transfer when the file
// is already present at the expected size.
func (s *Store) download(ctx context.Context, url string, expectSize int64, onProgress ProgressFunc) (string, error) {
	dest := s.archivePath(url)

	if st, err := os.Stat(dest); err == nil && st.Size() > 0 {
		if expectSize == 0 || st.Size() == expectSize {
			return dest, nil
		}
		s.log.Warn("cached download has unexpected size; re-fetching",
			"file", filepath.Base(dest), "have", st.Size(), "want", expectSize)
	}

	if s.Offline {
		return "", fmt.Errorf("%w: %s", ErrNotCached, path.Base(url))
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	if s.UserAgent != "" {
		req.Header.Set("User-Agent", s.UserAgent)
	}

	// Info would fight with the caller's progress bar on the console; the file
	// log records this at debug level either way.
	s.log.Debug("downloading", "url", url)
	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: %s", url, resp.Status)
	}

	total := resp.ContentLength
	if total <= 0 {
		total = expectSize
	}

	// Write to a temp file in the same directory so an interrupted download
	// never leaves a truncated file that looks cached.
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".dl-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op after a successful rename
	}()

	name := path.Base(url)
	counter := &progressWriter{
		total: total,
		onUpdate: func(n int64) {
			if onProgress != nil {
				onProgress(Progress{Stage: "download", Name: name, Current: n, Total: total})
			}
		},
	}

	if _, err := io.Copy(io.MultiWriter(tmp, counter), resp.Body); err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return "", err
	}

	s.log.Debug("downloaded", "file", name, "bytes", counter.n)
	return dest, nil
}

// verify checks an artifact's SHA-256 when the catalog supplies one.
func (s *Store) verify(path string, a catalog.Artifact, onProgress ProgressFunc) error {
	if a.SHA256 == "" {
		return nil
	}
	if onProgress != nil {
		onProgress(Progress{Stage: "verify", Name: a.Name})
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, a.SHA256) {
		return fmt.Errorf("checksum mismatch for %s: got %s, catalog says %s", a.Name, got, a.SHA256)
	}
	return nil
}

// progressWriter counts bytes and throttles callbacks to a readable rate.
type progressWriter struct {
	n        int64
	total    int64
	last     time.Time
	onUpdate func(int64)
}

func (w *progressWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	// A 170 MB download would otherwise emit tens of thousands of updates.
	if now := time.Now(); now.Sub(w.last) > 100*time.Millisecond || w.n == w.total {
		w.last = now
		if w.onUpdate != nil {
			w.onUpdate(w.n)
		}
	}
	return len(p), nil
}

// sanitize strips path separators from a file name component.
func sanitize(s string) string {
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, `\`, "_")
	s = strings.ReplaceAll(s, "..", "_")
	return strings.TrimSpace(s)
}

// sanitizeRel makes an archive-relative path safe to join onto the cache root,
// defeating zip-slip style entries.
func sanitizeRel(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	parts := strings.Split(path.Clean("/"+p), "/")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			continue
		}
		out = append(out, part)
	}
	return strings.Join(out, "/")
}
