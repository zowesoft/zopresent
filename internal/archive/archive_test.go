package archive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSafeJoinRefusesEscapes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "dest")
	for _, name := range []string{
		"../evil.txt",
		"a/../../evil.txt",
		"..",
		"/etc/passwd",
		"C:/Windows/evil.dll",
		`..\evil.txt`,
	} {
		if _, err := SafeJoin(root, name); err == nil {
			t.Fatalf("SafeJoin accepted escaping entry %q", name)
		}
	}
	for _, name := range []string{"server.exe", "lib/helper.dll", "./a.txt", "a/../b.txt"} {
		target, err := SafeJoin(root, name)
		if err != nil {
			t.Fatalf("SafeJoin refused benign entry %q: %v", name, err)
		}
		if target != root && !strings.HasPrefix(target, root+string(filepath.Separator)) {
			t.Fatalf("SafeJoin landed outside root: %q", target)
		}
	}
}

func writeZip(t *testing.T, entries map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "test.zip")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExtractZipPreservesLayout(t *testing.T) {
	path := writeZip(t, map[string]string{
		"llama-server.exe": "binary",
		"ggml.dll":         "library",
		"sub/other.dll":    "nested",
	})
	dest := t.TempDir()
	if err := ExtractZip(path, dest); err != nil {
		t.Fatalf("extract: %v", err)
	}
	for name, want := range map[string]string{
		"llama-server.exe": "binary",
		"ggml.dll":         "library",
		"sub/other.dll":    "nested",
	} {
		got, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(name)))
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q (%v), want %q", name, got, err, want)
		}
	}
}

func TestExtractZipRefusesEscapingEntry(t *testing.T) {
	path := writeZip(t, map[string]string{"../evil.txt": "payload"})
	base := t.TempDir()
	dest := filepath.Join(base, "dest")
	if err := ExtractZip(path, dest); err == nil {
		t.Fatal("a zip escaping its destination was extracted")
	}
	if _, err := os.Stat(filepath.Join(base, "evil.txt")); err == nil {
		t.Fatal("escaping entry wrote outside the destination")
	}
}

func TestExtractTarGzRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	entries := []struct {
		name     string
		content  string
		typeflag byte
	}{
		{"llama-b12436/", "", tar.TypeDir},
		{"llama-b12436/llama-server", "binary", tar.TypeReg},
		{"llama-b12436/libggml.so", "library", tar.TypeReg},
		{"llama-b12436/link", "", tar.TypeSymlink},
	}
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{
			Name:     e.name,
			Mode:     0o755,
			Size:     int64(len(e.content)),
			Typeflag: e.typeflag,
			Linkname: "/etc/passwd",
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "test.tar.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	dest := t.TempDir()
	if err := ExtractTarGz(path, dest); err != nil {
		t.Fatalf("extract: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "llama-b12436", "llama-server"))
	if err != nil || string(got) != "binary" {
		t.Fatalf("nested binary = %q (%v)", got, err)
	}
	// The symlink must be skipped, never followed or materialised.
	if _, err := os.Lstat(filepath.Join(dest, "llama-b12436", "link")); err == nil {
		t.Fatal("a symlink entry was materialised")
	}
}

func TestExtractTarRefusesEscapingEntry(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "../evil.txt", Mode: 0o644, Size: 7, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	base := t.TempDir()
	dest := filepath.Join(base, "dest")
	if err := ExtractTar(&buf, dest); err == nil {
		t.Fatal("a tar escaping its destination was extracted")
	}
	if _, err := os.Stat(filepath.Join(base, "evil.txt")); err == nil {
		t.Fatal("escaping entry wrote outside the destination")
	}
}

func TestFindFileSearchesNestedTree(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "llama-b12436")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(nested, "llama-server")
	if err := os.WriteFile(want, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := FindFile(root, "llama-server")
	if err != nil || got != want {
		t.Fatalf("FindFile = %q (%v), want %q", got, err, want)
	}
	if _, err := FindFile(root, "missing"); err == nil {
		t.Fatal("FindFile invented a missing file")
	}
}
