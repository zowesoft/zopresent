package search

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"zopresent/internal/archive"
)

// Progress reports a download as it advances, so a ~50 MB fetch shows movement
// instead of a frozen button.
type Progress struct {
	File       string
	Done       int
	Total      int
	Bytes      int64
	TotalBytes int64
}

// Installer only places verified files; it never runs an engine.
type Installer struct {
	client *http.Client
	// RuntimeBase overrides the release download root; empty means the pinned
	// GitHub release.
	RuntimeBase string
	// Progress, when set, is called as a download advances.
	Progress func(Progress)
}

// NewInstaller returns an installer with no overall timeout: a model download
// on a slow link can outlast any default one.
func NewInstaller() *Installer {
	return &Installer{client: &http.Client{Timeout: 0}}
}

func (i *Installer) Install(root string, e Engine) error {
	return i.InstallFor(root, e, HostKey())
}

// InstallFor downloads only what is missing or fails its hash, so re-running
// it leaves an intact engine untouched.
func (i *Installer) InstallFor(root string, e Engine, platform string) error {
	goos, goarch, ok := strings.Cut(platform, "/")
	if !ok {
		return fmt.Errorf("invalid platform %q", platform)
	}
	r, ok := e.RuntimeFor(goos, goarch)
	if !ok {
		return ErrUnsupported
	}

	const total = 2
	modelDest := ModelPath(root, e)
	if verifyHash(modelDest, e.Model.SHA256) != nil {
		if err := i.downloadFile(e.Model.URL, modelDest, e.Model.SHA256, e.Model.Name, 0, total); err != nil {
			return err
		}
	}

	if err := i.installRuntime(root, e, r, 1, total); err != nil {
		return err
	}

	if err := writeNotices(root); err != nil {
		return err
	}
	return VerifyFor(root, e, goos, goarch)
}

// installRuntime extracts the whole release into the runtime directory. The
// server binary loads its libraries from beside itself (Windows DLL search,
// unix rpath), so the shipped layout is preserved rather than flattened.
func (i *Installer) installRuntime(root string, e Engine, r Runtime, done, total int) error {
	if bin, err := archive.FindFile(RuntimeDir(root), r.Binary); err == nil && verifyHash(bin, r.BinarySHA256) == nil {
		return nil
	}

	tmp, err := os.MkdirTemp("", "zopresent-embedding-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	archivePath := filepath.Join(tmp, r.Archive)
	base := i.RuntimeBase
	if base == "" {
		base = runtimeBase
	}
	if err := i.downloadFile(base+r.Archive, archivePath, r.ArchiveSHA256, r.Archive, done, total); err != nil {
		return err
	}

	dest := RuntimeDir(root)
	// A stale extraction from an interrupted install must not mix with the new
	// one; the directory is rebuilt from the verified archive alone.
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	switch {
	case strings.HasSuffix(r.Archive, ".zip"):
		err = archive.ExtractZip(archivePath, dest)
	case strings.HasSuffix(r.Archive, ".tar.gz"):
		err = archive.ExtractTarGz(archivePath, dest)
	default:
		err = fmt.Errorf("unsupported archive format %q", r.Archive)
	}
	if err != nil {
		return err
	}

	bin, err := archive.FindFile(dest, r.Binary)
	if err != nil {
		return fmt.Errorf("locate %s in %s: %w", r.Binary, r.Archive, err)
	}
	return verifyHash(bin, r.BinarySHA256)
}

// downloadFile writes to a temporary file first so a partial download never
// looks like a finished one, and verifies the pinned hash before placing it.
func (i *Installer) downloadFile(url, dest, want, name string, done, total int) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}

	resp, err := i.client.Get(url)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch %s: status %s", url, resp.Status)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), ".download-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	sum := sha256.New()
	wrapped := &progressWriter{
		w:      io.MultiWriter(tmp, sum),
		name:   name,
		report: i.Progress,
		done:   done,
		total:  total,
		size:   resp.ContentLength,
	}
	if _, err := io.Copy(wrapped, resp.Body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", dest, err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if want != "" && !strings.EqualFold(hex.EncodeToString(sum.Sum(nil)), want) {
		return fmt.Errorf("%w: %s", ErrChecksum, filepath.Base(dest))
	}
	_ = os.Remove(dest)
	if err := os.Rename(tmpName, dest); err != nil {
		// A rename cannot cross volumes; the temp file sits beside the dest
		// here, but a copy keeps the path honest on exotic filesystems.
		return copyFile(tmpName, dest)
	}
	return nil
}

func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// progressWriter emits at most once per whole percent, because a per-write
// callback would fire thousands of times for a model download.
type progressWriter struct {
	w             io.Writer
	name          string
	report        func(Progress)
	done          int
	total         int
	size          int64
	seen          int64
	lastPct       int
	lastEmitBytes int64
	reported      bool
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.seen += int64(n)
	if p.report == nil {
		return n, err
	}

	if p.size <= 0 {
		// Unknown length: emit roughly every megabyte rather than staying
		// silent, so a caller still sees movement on a server that omits it.
		if p.seen-p.lastEmitBytes >= 1<<20 {
			p.lastEmitBytes = p.seen
			p.report(Progress{File: p.name, Done: p.done, Total: p.total, Bytes: p.seen})
		}
		return n, err
	}

	pct := int(p.seen * 100 / p.size)
	if p.reported && pct == p.lastPct {
		return n, err
	}
	p.reported = true
	p.lastPct = pct
	p.report(Progress{
		File:       p.name,
		Done:       p.done,
		Total:      p.total,
		Bytes:      p.seen,
		TotalBytes: p.size,
	})
	return n, err
}

// An empty want means the file is not pinned and is accepted as-is.
func verifyHash(path, want string) error {
	if want == "" {
		return nil
	}
	got, err := hashFile(path)
	if err != nil {
		return err
	}
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("%w: %s", ErrChecksum, filepath.Base(path))
	}
	return nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}
