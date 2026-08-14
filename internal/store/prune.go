package store

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/jclement/meshflash/internal/catalog"
)

// Usage summarises what the cache is holding.
type Usage struct {
	Downloads      int64
	Extracted      int64
	DownloadFiles  int
	ExtractedFiles int
}

// Total is the whole cache footprint in bytes.
func (u Usage) Total() int64 { return u.Downloads + u.Extracted }

// Usage measures the cache. Field units run on small disks, and the platform
// archives are large, so this is worth surfacing in `doctor`.
func (s *Store) Usage() (Usage, error) {
	var u Usage
	var err error
	if u.Downloads, u.DownloadFiles, err = dirSize(s.paths.DownloadDir()); err != nil {
		return u, err
	}
	if u.Extracted, u.ExtractedFiles, err = dirSize(s.paths.ExtractDir()); err != nil {
		return u, err
	}
	return u, nil
}

func dirSize(root string) (int64, int, error) {
	var total int64
	var count int
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		total += info.Size()
		count++
		return nil
	})
	if os.IsNotExist(err) {
		return 0, 0, nil
	}
	return total, count, err
}

// PruneArchives deletes downloaded source archives that are no longer needed.
//
// An archive is needed only while some artifact inside it is still waiting to
// be extracted. Once every member the operator wants is unpacked, the archive
// is dead weight — and it is the expensive half of the cache, since a 170 MB
// platform zip may yield a single 1.5 MB image.
//
// Extracted firmware is always kept: it is small, and it is the only thing
// required to flash offline.
func (s *Store) PruneArchives(keep []catalog.Artifact) (freed int64, removed int, err error) {
	wanted := map[string]bool{}
	for _, a := range keep {
		src := a.Source()
		if src == "" {
			continue
		}
		// A non-packed artifact *is* the download, so it must survive.
		if !a.Packed() {
			wanted[filepath.Base(s.archivePath(src))] = true
			continue
		}
		if !s.Cached(a) {
			wanted[filepath.Base(s.archivePath(src))] = true
		}
	}

	entries, err := os.ReadDir(s.paths.DownloadDir())
	if os.IsNotExist(err) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}

	for _, e := range entries {
		if e.IsDir() || wanted[e.Name()] {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		p := filepath.Join(s.paths.DownloadDir(), e.Name())
		if err := os.Remove(p); err != nil {
			s.log.Warn("could not remove cached archive", "file", e.Name(), "error", err)
			continue
		}
		freed += info.Size()
		removed++
	}
	return freed, removed, nil
}

// PruneExtracted removes extracted firmware directories not referenced by the
// supplied artifacts, which is how old releases age out.
func (s *Store) PruneExtracted(keep []catalog.Artifact) (freed int64, removed int, err error) {
	wanted := map[string]bool{}
	for _, a := range keep {
		if a.Packed() {
			wanted[urlKey(a.Archive)] = true
		}
	}

	entries, err := os.ReadDir(s.paths.ExtractDir())
	if os.IsNotExist(err) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}

	for _, e := range entries {
		if !e.IsDir() || wanted[e.Name()] {
			continue
		}
		p := filepath.Join(s.paths.ExtractDir(), e.Name())
		size, _, _ := dirSize(p)
		if err := os.RemoveAll(p); err != nil {
			s.log.Warn("could not remove extracted firmware", "dir", e.Name(), "error", err)
			continue
		}
		freed += size
		removed++
	}
	return freed, removed, nil
}

// WantedArtifacts collects every artifact implied by the operator's device and
// channel selection, newest KeepVersions releases per project.
//
// This is the function that decides how big the cache gets, so it is also
// where the "don't download 270 MB of firmware for a board you don't own"
// policy lives.
func WantedArtifacts(cat *catalog.Catalog, wantDevice func(string) bool, wantProject func(string) bool, wantChannel func(string) bool, keepVersions int) []catalog.Artifact {
	if keepVersions <= 0 {
		keepVersions = 1
	}
	var out []catalog.Artifact

	for pi := range cat.Projects {
		p := &cat.Projects[pi]
		if wantProject != nil && !wantProject(p.ID) {
			continue
		}

		releases := make([]*catalog.Release, 0, len(p.Releases))
		for ri := range p.Releases {
			r := &p.Releases[ri]
			if wantChannel != nil && !wantChannel(r.Channel) {
				continue
			}
			releases = append(releases, r)
		}
		sort.Slice(releases, func(i, j int) bool {
			return releases[i].PublishedAt.After(releases[j].PublishedAt)
		})
		if len(releases) > keepVersions {
			releases = releases[:keepVersions]
		}

		for _, r := range releases {
			for _, b := range r.Builds {
				if wantDevice != nil && !wantDevice(b.DeviceID) {
					continue
				}
				out = append(out, b.Artifacts...)
			}
		}
	}
	return out
}

// DownloadBytes is how much network transfer a set of artifacts implies.
//
// Artifacts packed into the same archive are counted once, because that is
// what actually gets fetched: one 170 MB esp32s3 zip serves every S3 board the
// operator selected.
func DownloadBytes(artifacts []catalog.Artifact) int64 {
	var total int64
	seen := map[string]bool{}
	for _, a := range artifacts {
		src := a.Source()
		if src == "" || seen[src] {
			continue
		}
		seen[src] = true
		total += a.DownloadSize()
	}
	return total
}

// FormatBytes renders a byte count for human consumption.
func FormatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
