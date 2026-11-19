package search

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"zopresent/internal/archive"
)

func zipBytes(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func tarGzBytes(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// installFixture serves a tiny model and runtime archive from a local server
// and returns an engine pinned to them, so the installer is exercised without
// touching the real release URLs.
func installFixture(t *testing.T, archiveName string, archiveContent []byte) (*Installer, Engine, *atomic.Int64) {
	t.Helper()
	modelContent := []byte("fake model weights")
	binContent := []byte("fake server binary")

	var requests atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/model.gguf", func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write(modelContent)
	})
	mux.HandleFunc("/"+archiveName, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write(archiveContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	e := MiniLM()
	e.ID = "test-model"
	e.Model = File{Name: "model.gguf", SHA256: sha256Hex(modelContent), URL: srv.URL + "/model.gguf"}
	e.Runtimes = map[string]Runtime{
		HostKey(): {
			Archive:       archiveName,
			ArchiveSHA256: sha256Hex(archiveContent),
			Binary:        "fake-server",
			BinarySHA256:  sha256Hex(binContent),
		},
	}
	installer := NewInstaller()
	installer.RuntimeBase = srv.URL + "/"
	return installer, e, &requests
}

func TestInstallForPlacesZipRuntime(t *testing.T) {
	content := zipBytes(t, map[string][]byte{
		"fake-server":  []byte("fake server binary"),
		"fake-lib.dll": []byte("fake shared library"),
	})
	installer, e, _ := installFixture(t, "runtime.zip", content)
	root := t.TempDir()

	var events []Progress
	installer.Progress = func(p Progress) { events = append(events, p) }
	if err := installer.InstallFor(root, e, HostKey()); err != nil {
		t.Fatalf("install: %v", err)
	}

	bin, err := BinaryPath(root, e)
	if err != nil {
		t.Fatalf("locate binary: %v", err)
	}
	// The library must land beside the executable, not somewhere flattened.
	if _, err := os.Stat(filepath.Join(filepath.Dir(bin), "fake-lib.dll")); err != nil {
		t.Fatalf("library not beside binary: %v", err)
	}
	if _, err := os.Stat(ModelPath(root, e)); err != nil {
		t.Fatalf("model missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, NoticesFile)); err != nil {
		t.Fatalf("notices missing: %v", err)
	}
	if err := Verify(root, e); err != nil {
		t.Fatalf("verify after install: %v", err)
	}

	// Both files report progress, each finishing at its full size.
	seen := map[string]bool{}
	for _, p := range events {
		if p.Total != 2 {
			t.Fatalf("progress total = %d, want 2", p.Total)
		}
		if p.Bytes == p.TotalBytes && p.TotalBytes > 0 {
			seen[p.File] = true
		}
	}
	if !seen["model.gguf"] || !seen["runtime.zip"] {
		t.Fatalf("progress missed a file: %v", seen)
	}
}

func TestInstallForPlacesNestedTarGzRuntime(t *testing.T) {
	content := tarGzBytes(t, map[string][]byte{
		"llama-test/fake-server": []byte("fake server binary"),
		"llama-test/fake-lib.so": []byte("fake shared library"),
	})
	installer, e, _ := installFixture(t, "runtime.tar.gz", content)
	root := t.TempDir()

	if err := installer.InstallFor(root, e, HostKey()); err != nil {
		t.Fatalf("install: %v", err)
	}
	bin, err := BinaryPath(root, e)
	if err != nil {
		t.Fatalf("locate nested binary: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(bin), "fake-lib.so")); err != nil {
		t.Fatalf("library not beside nested binary: %v", err)
	}
	if !Present(root, e) {
		t.Fatal("installed engine not reported present")
	}
}

func TestInstallForSkipsIntactFiles(t *testing.T) {
	content := zipBytes(t, map[string][]byte{"fake-server": []byte("fake server binary")})
	installer, e, requests := installFixture(t, "runtime.zip", content)
	root := t.TempDir()

	if err := installer.InstallFor(root, e, HostKey()); err != nil {
		t.Fatalf("first install: %v", err)
	}
	requests.Store(0)
	if err := installer.InstallFor(root, e, HostKey()); err != nil {
		t.Fatalf("second install: %v", err)
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("re-install fetched %d files, want none", n)
	}
}

func TestInstallForRefusesBadHashes(t *testing.T) {
	content := zipBytes(t, map[string][]byte{"fake-server": []byte("fake server binary")})

	installer, e, _ := installFixture(t, "runtime.zip", content)
	e.Model.SHA256 = sha256Hex([]byte("not the served model"))
	root := t.TempDir()
	err := installer.InstallFor(root, e, HostKey())
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("bad model hash returned %v, want ErrChecksum", err)
	}
	if _, statErr := os.Stat(ModelPath(root, e)); statErr == nil {
		t.Fatal("a model that failed its hash was placed on disk")
	}

	installer, e, _ = installFixture(t, "runtime.zip", content)
	r := e.Runtimes[HostKey()]
	r.ArchiveSHA256 = sha256Hex([]byte("not the served archive"))
	e.Runtimes[HostKey()] = r
	root = t.TempDir()
	err = installer.InstallFor(root, e, HostKey())
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("bad archive hash returned %v, want ErrChecksum", err)
	}
	if _, findErr := archive.FindFile(RuntimeDir(root), "fake-server"); findErr == nil {
		t.Fatal("an archive that failed its hash was extracted")
	}
}
