package search

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestCatalogPinsEveryFile(t *testing.T) {
	for _, e := range Catalog() {
		if e.ID == "" || e.Dims <= 0 {
			t.Fatalf("engine %q lacks an ID or dimension count", e.ID)
		}
		if len(e.Model.SHA256) != 64 || e.Model.URL == "" || e.Model.Name == "" {
			t.Fatalf("engine %q model is not fully pinned: %+v", e.ID, e.Model)
		}
		for _, platform := range []string{
			"windows/amd64", "windows/arm64",
			"linux/amd64", "linux/arm64",
			"darwin/amd64", "darwin/arm64",
		} {
			goos, goarch, _ := strings.Cut(platform, "/")
			r, ok := e.RuntimeFor(goos, goarch)
			if !ok {
				t.Fatalf("engine %q has no runtime for %s", e.ID, platform)
			}
			if len(r.ArchiveSHA256) != 64 || len(r.BinarySHA256) != 64 || r.Archive == "" || r.Binary == "" {
				t.Fatalf("engine %q runtime %s is not fully pinned: %+v", e.ID, platform, r)
			}
		}
	}
}

func TestFindRejectsUnknownModel(t *testing.T) {
	if _, err := Find("no-such-model"); err == nil {
		t.Fatal("Find accepted an unknown model ID")
	}
	if _, err := Find(MiniLM().ID); err != nil {
		t.Fatalf("Find rejected the catalog model: %v", err)
	}
}

// syntheticEngine builds a tiny engine whose files exist under root with
// matching hashes, so presence and verification can be tested without a real
// 50 MB download. The runtime map covers the host so Resolve works anywhere.
func syntheticEngine(t *testing.T, root string) Engine {
	t.Helper()
	e := MiniLM()
	e.ID = "test-model"
	e.Dims = 4
	e.Model = File{Name: "model.bin"}

	modelBytes := []byte("fake model weights")
	if err := os.MkdirAll(filepath.Dir(ModelPath(root, e)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ModelPath(root, e), modelBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	e.Model.SHA256 = sha256Hex(modelBytes)

	binBytes := []byte("fake server binary")
	if err := os.MkdirAll(RuntimeDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(RuntimeDir(root), "fake-server"), binBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	e.Runtimes = map[string]Runtime{
		HostKey(): {
			Archive:       "fake-runtime.tar.gz",
			ArchiveSHA256: sha256Hex([]byte("archive")),
			Binary:        "fake-server",
			BinarySHA256:  sha256Hex(binBytes),
		},
	}
	return e
}

func TestPresentAndVerify(t *testing.T) {
	root := t.TempDir()
	e := syntheticEngine(t, root)

	if !Present(root, e) {
		t.Fatal("a complete engine root was reported missing")
	}
	if err := Verify(root, e); err != nil {
		t.Fatalf("verify on intact files: %v", err)
	}

	// Tampering with either file must fail verification, not pass silently.
	if err := os.WriteFile(ModelPath(root, e), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Verify(root, e); !errors.Is(err, ErrChecksum) {
		t.Fatalf("tampered model returned %v, want ErrChecksum", err)
	}
}

func TestPresentRequiresBothFiles(t *testing.T) {
	root := t.TempDir()
	e := syntheticEngine(t, root)
	if err := os.Remove(ModelPath(root, e)); err != nil {
		t.Fatal(err)
	}
	if Present(root, e) {
		t.Fatal("a runtime without a model was reported present")
	}
}

func TestResolvePrefersBundledRoot(t *testing.T) {
	bundled := t.TempDir()
	download := t.TempDir()
	e := syntheticEngine(t, bundled)
	syntheticEngine(t, download)

	root, err := Resolve(Layout{BundledRoot: bundled, DownloadRoot: download}, e)
	if err != nil || root != bundled {
		t.Fatalf("Resolve picked %q (%v), want the bundled root", root, err)
	}

	root, err = Resolve(Layout{DownloadRoot: download}, e)
	if err != nil || root != download {
		t.Fatalf("Resolve picked %q (%v), want the download root", root, err)
	}

	if _, err := Resolve(Layout{}, e); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("empty layout returned %v, want ErrNotInstalled", err)
	}
}
