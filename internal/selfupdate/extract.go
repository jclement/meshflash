package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

// binaryName is what the archive contains.
func binaryName() string {
	if runtime.GOOS == "windows" {
		return "meshflash.exe"
	}
	return "meshflash"
}

// maxBinaryBytes bounds extraction of the replacement binary.
const maxBinaryBytes = 256 << 20

// extractBinary pulls the meshflash executable out of a release archive and
// returns the path it was written to.
func extractBinary(archive, destDir string) (string, error) {
	dest := filepath.Join(destDir, binaryName())

	switch {
	case strings.HasSuffix(archive, ".zip"):
		return dest, extractFromZip(archive, dest)
	case strings.HasSuffix(archive, ".tar.gz"), strings.HasSuffix(archive, ".tgz"):
		return dest, extractFromTarGz(archive, dest)
	default:
		return "", fmt.Errorf("unsupported release archive format: %s", filepath.Base(archive))
	}
}

func extractFromZip(archive, dest string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return fmt.Errorf("open release archive: %w", err)
	}
	defer zr.Close()

	want := binaryName()
	for _, f := range zr.File {
		if path.Base(f.Name) != want || f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		defer rc.Close()
		return writeFile(dest, rc)
	}
	return fmt.Errorf("release archive does not contain %s", want)
}

func extractFromTarGz(archive, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return fmt.Errorf("open release archive: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("decompress release archive: %w", err)
	}
	defer gz.Close()

	want := binaryName()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read release archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg || path.Base(hdr.Name) != want {
			continue
		}
		return writeFile(dest, tr)
	}
	return fmt.Errorf("release archive does not contain %s", want)
}

func writeFile(dest string, src io.Reader) error {
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer out.Close()

	n, err := io.Copy(out, io.LimitReader(src, maxBinaryBytes+1))
	if err != nil {
		return err
	}
	if n > maxBinaryBytes {
		return fmt.Errorf("replacement binary exceeds %d bytes", maxBinaryBytes)
	}
	if n == 0 {
		return fmt.Errorf("replacement binary is empty")
	}
	return out.Sync()
}
