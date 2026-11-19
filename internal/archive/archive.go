// Package archive unpacks release archives for engine installers. An archive
// is untrusted input: every member is constrained to the destination directory
// and anything that is not a regular file or directory is skipped, so a hostile
// download stays inert.
package archive

import (
	"archive/tar"
	"archive/zip"
	"compress/bzip2"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ExtractTarBz2 unpacks a .tar.bz2 release archive into dest.
func ExtractTarBz2(archive, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	return ExtractTar(bzip2.NewReader(f), dest)
}

// ExtractTarGz unpacks a .tar.gz release archive into dest.
func ExtractTarGz(archive, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	return ExtractTar(gz, dest)
}

// ExtractTar writes a tar stream into dest, preserving the shipped layout so a
// binary keeps its relative library paths.
func ExtractTar(r io.Reader, dest string) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	root, err := filepath.Abs(dest)
	if err != nil {
		return err
	}

	tr := tar.NewReader(r)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}

		target, err := SafeJoin(root, header.Name)
		if err != nil {
			return err
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := writeFile(target, tr, os.FileMode(header.Mode)&0o777); err != nil {
				return err
			}
		default:
			// Symlinks, devices and the like are not part of a release we
			// consume; skipping them keeps a hostile archive inert.
		}
	}
}

// ExtractZip unpacks a .zip release archive into dest, preserving the shipped
// layout. Windows runtime DLLs must land beside their executable, so the whole
// archive is written rather than a hand-picked subset.
func ExtractZip(archive, dest string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer zr.Close()

	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	root, err := filepath.Abs(dest)
	if err != nil {
		return err
	}

	for _, f := range zr.File {
		target, err := SafeJoin(root, f.Name)
		if err != nil {
			return err
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if f.Mode()&os.ModeSymlink != 0 {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = writeFile(target, rc, f.Mode()&0o777)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func writeFile(target string, r io.Reader, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if mode == 0 {
		mode = 0o644
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// SafeJoin resolves name under root and refuses anything that lands outside it.
// Archive entry names are POSIX-style regardless of host OS, so both "/" and
// the host separator are treated as absolute-path markers.
func SafeJoin(root, name string) (string, error) {
	cleaned := filepath.Clean(filepath.FromSlash(name))
	if strings.HasPrefix(cleaned, "/") || strings.HasPrefix(cleaned, "\\") ||
		filepath.IsAbs(cleaned) || filepath.VolumeName(cleaned) != "" {
		return "", fmt.Errorf("archive entry escapes destination: %q", name)
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("archive entry escapes destination: %q", name)
	}
	target := filepath.Join(root, cleaned)
	// Belt and braces: the joined path must still sit under root.
	if target != root && !strings.HasPrefix(target, root+string(filepath.Separator)) {
		return "", fmt.Errorf("archive entry escapes destination: %q", name)
	}
	return target, nil
}

// FindFile locates the first regular file named name anywhere under root. The
// release archives nest the members under a versioned folder, so the search is
// by file name rather than a fixed path.
func FindFile(root, name string) (string, error) {
	var found string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && entry.Name() == name {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("%s not found in archive", name)
	}
	return found, nil
}
