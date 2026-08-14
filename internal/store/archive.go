package store

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// maxMemberBytes bounds a single extracted file. The largest thing meshflash
// pulls out of an upstream archive is a merged ESP32 image, a few megabytes.
// The cap turns a zip bomb into an error rather than a full disk.
const maxMemberBytes = 64 << 20

// extractMember copies one file out of a zip archive to dest.
//
// Member lookup tolerates upstream reshuffling: archives sometimes gain or
// lose a leading directory between releases, so an exact-path miss falls back
// to matching on base name.
func extractMember(archivePath, member, dest string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("open archive %s: %w", filepath.Base(archivePath), err)
	}
	defer zr.Close()

	f := findMember(zr, member)
	if f == nil {
		return fmt.Errorf("%s not found in %s", member, filepath.Base(archivePath))
	}
	if f.UncompressedSize64 > maxMemberBytes {
		return fmt.Errorf("%s is %d bytes, over the %d byte extraction limit",
			member, f.UncompressedSize64, maxMemberBytes)
	}

	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("read %s: %w", member, err)
	}
	defer rc.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dest), ".x-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	if _, err := io.Copy(tmp, io.LimitReader(rc, maxMemberBytes+1)); err != nil {
		return fmt.Errorf("extract %s: %w", member, err)
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, dest)
}

// findMember locates a zip entry by exact path, then by base name.
func findMember(zr *zip.ReadCloser, member string) *zip.File {
	want := sanitizeRel(member)
	for _, f := range zr.File {
		if sanitizeRel(f.Name) == want {
			return f
		}
	}
	base := path.Base(want)
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if path.Base(f.Name) == base {
			return f
		}
	}
	return nil
}

// ListArchive returns the file names inside a zip, used by catalog-gen and by
// `doctor` when explaining what a cached archive contains.
func ListArchive(archivePath string) ([]string, error) {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return nil, err
	}
	defer zr.Close()

	out := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		out = append(out, f.Name)
	}
	return out, nil
}

// IsArchive reports whether a URL or file name looks like a zip.
func IsArchive(name string) bool {
	return strings.EqualFold(filepath.Ext(name), ".zip")
}
