// Package search owns meaning-based scripture and song discovery: the local
// embedding engine, a versioned index over stored text, and cosine retrieval.
// A similarity score is a suggestion to the operator, never a detection
// confidence, and nothing here projects on its own.
package search

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"zopresent/internal/archive"
)

var (
	// ErrNotInstalled means no engine root holds the model and runtime yet.
	ErrNotInstalled = errors.New("the semantic model is not installed — install it in Settings")
	// ErrUnsupported means this platform has no published runtime.
	ErrUnsupported = errors.New("no embedding runtime is available for this platform")
	// ErrChecksum is returned when a file does not match its pinned hash; the
	// caller must treat it as unusable rather than run it.
	ErrChecksum = errors.New("file checksum does not match")
	// ErrIndexEmpty means the model is installed but nothing is indexed yet.
	ErrIndexEmpty = errors.New("the semantic index is empty — build it in Settings")
)

// RuntimeTag pins the llama.cpp release every platform fetches. The benchmark
// measured this build; a bump is a deliberate re-measurement, not a drift.
const RuntimeTag = "b12436"

const (
	runtimeBase = "https://github.com/ggml-org/llama.cpp/releases/download/" + RuntimeTag + "/"
	modelURL    = "https://huggingface.co/second-state/All-MiniLM-L6-v2-Embedding-GGUF/resolve/main/all-MiniLM-L6-v2-Q8_0.gguf"
)

// Engine is one embedding model plus the per-OS runtime that serves it. ID
// doubles as the model version stored beside every vector, so a model change
// rebuilds the index instead of ranking new queries against old vectors.
type Engine struct {
	ID          string
	Name        string
	Description string
	Dims        int
	Model       File
	Runtimes    map[string]Runtime
}

// Runtime is a whole release archive: llama-server ships with a tree of DLLs
// or shared libraries that must stay beside it, so the installer extracts
// everything rather than a hand-picked subset.
type Runtime struct {
	Archive       string
	ArchiveSHA256 string
	Binary        string
	BinarySHA256  string
}

func (r Runtime) URL() string { return runtimeBase + r.Archive }

type File struct {
	Name   string
	SHA256 string
	URL    string
}

func Key(goos, goarch string) string { return goos + "/" + goarch }

func HostKey() string { return Key(runtime.GOOS, runtime.GOARCH) }

func (e Engine) RuntimeFor(goos, goarch string) (Runtime, bool) {
	r, ok := e.Runtimes[Key(goos, goarch)]
	return r, ok
}

func (e Engine) HostRuntime() (Runtime, bool) {
	return e.RuntimeFor(runtime.GOOS, runtime.GOARCH)
}

// Catalog lists the supported models. MiniLM is the benchmark's choice on the
// limited-spec Windows baseline: it recovers the most meaning-based queries
// inside the latency budget at the smallest install cost. See
// benchmarks/README.md for the measurements behind it.
func Catalog() []Engine { return []Engine{MiniLM()} }

// MiniLM is all-MiniLM-L6-v2 quantized to Q8_0: 24 MB on disk, 384 dimensions,
// no query prefix. Hashes are pinned from the b12436 release assets.
func MiniLM() Engine {
	return Engine{
		ID:          "minilm-l6-v2-q8",
		Name:        "MiniLM meaning search",
		Description: "Local meaning-based search over scripture and songs. Small and fast on modest hardware; keywords stay with full-text search.",
		Dims:        384,
		Model: File{
			Name:   "all-MiniLM-L6-v2-Q8_0.gguf",
			SHA256: "263215c3cadd6e16740741a7624ab4cbb6c8e777688bd5331ecfbf5681c2f8ed",
			URL:    modelURL,
		},
		Runtimes: map[string]Runtime{
			Key("windows", "amd64"): {
				Archive:       "llama-" + RuntimeTag + "-bin-win-cpu-x64.zip",
				ArchiveSHA256: "61b6e12d67b77fe9c5d218d2aa5ff60fb14fd45bc071f65b569dc7fe557ecdb4",
				Binary:        "llama-server.exe",
				BinarySHA256:  "7bb5b16a787edb4d8f22b8e5a6855421b72cc5aeea76dcba159c7356aabf9201",
			},
			Key("windows", "arm64"): {
				Archive:       "llama-" + RuntimeTag + "-bin-win-cpu-arm64.zip",
				ArchiveSHA256: "a5c20227572512028c9b1ca830d4fca89b2b094645252cf89f0de8f7e07fdb7a",
				Binary:        "llama-server.exe",
				BinarySHA256:  "3598da7d6936493698c7ff9cff28686d0e57bccb759725dbc38c95e6262b45b6",
			},
			Key("linux", "amd64"): {
				Archive:       "llama-" + RuntimeTag + "-bin-ubuntu-x64.tar.gz",
				ArchiveSHA256: "9e4222da66fde18ed47e95b2ed7e20d7b8c5868f8d01093bda5499407e5fe53d",
				Binary:        "llama-server",
				BinarySHA256:  "ea00dd1b651d69c18ff483dfc9e47a512e8ae3df7ca42ad9195499afb0726cd5",
			},
			Key("linux", "arm64"): {
				Archive:       "llama-" + RuntimeTag + "-bin-ubuntu-arm64.tar.gz",
				ArchiveSHA256: "4755f1ddb690372365410658b40839bc6d5a3f182c8cad3c613458f0688726c4",
				Binary:        "llama-server",
				BinarySHA256:  "d202c18e434857f57623b4aaa60f8bde27c414aa4f25ea73a2f4d8e351fb0635",
			},
			Key("darwin", "arm64"): {
				Archive:       "llama-" + RuntimeTag + "-bin-macos-arm64.tar.gz",
				ArchiveSHA256: "47e595dbf6c04b3765a489d800576fe2181460b86eb4b9b728a5a5ac75c8125e",
				Binary:        "llama-server",
				BinarySHA256:  "685941b8df982e4e205fd20492f9aaa9418f2d1224ba3d311a686abd0edb0439",
			},
			Key("darwin", "amd64"): {
				Archive:       "llama-" + RuntimeTag + "-bin-macos-x64.tar.gz",
				ArchiveSHA256: "896535b456078a7300e4f7cee87466c6cb87aa31aa2138eaa1c6032bc60c2b48",
				Binary:        "llama-server",
				BinarySHA256:  "cd361b0b72ac997347217d92fee1a67adf8a8259c9e5085f0c4c285279c3fb30",
			},
		},
	}
}

func Find(id string) (Engine, error) {
	for _, engine := range Catalog() {
		if engine.ID == id {
			return engine, nil
		}
	}
	return Engine{}, fmt.Errorf("unknown semantic model %q", id)
}

// Layout mirrors the speech engine roots: a bundled tree beside the app and a
// writable per-user download tree.
type Layout struct {
	BundledRoot  string
	DownloadRoot string
}

// RuntimeDir holds the extracted release. The Windows zip is flat while the
// unix tarballs nest under a versioned folder, so the binary is located by
// name inside this directory rather than at a fixed path.
func RuntimeDir(root string) string {
	return filepath.Join(root, "bin", "llama-"+RuntimeTag)
}

func ModelPath(root string, e Engine) string {
	return filepath.Join(root, "models", e.ID, e.Model.Name)
}

// BinaryPath locates llama-server inside the extracted runtime tree.
func BinaryPath(root string, e Engine) (string, error) {
	r, ok := e.HostRuntime()
	if !ok {
		return "", ErrUnsupported
	}
	return archive.FindFile(RuntimeDir(root), r.Binary)
}

func Present(root string, e Engine) bool {
	return PresentFor(root, e, runtime.GOOS, runtime.GOARCH)
}

func PresentFor(root string, e Engine, goos, goarch string) bool {
	r, ok := e.RuntimeFor(goos, goarch)
	if !ok {
		return false
	}
	if _, err := archive.FindFile(RuntimeDir(root), r.Binary); err != nil {
		return false
	}
	_, err := os.Stat(ModelPath(root, e))
	return err == nil
}

func Verify(root string, e Engine) error {
	return VerifyFor(root, e, runtime.GOOS, runtime.GOARCH)
}

// VerifyFor checks the pinned hashes of the binary and model. A failed check
// means the files are corrupt or were tampered with and must not be run.
func VerifyFor(root string, e Engine, goos, goarch string) error {
	r, ok := e.RuntimeFor(goos, goarch)
	if !ok {
		return ErrUnsupported
	}
	bin, err := archive.FindFile(RuntimeDir(root), r.Binary)
	if err != nil {
		return err
	}
	if err := verifyHash(bin, r.BinarySHA256); err != nil {
		return err
	}
	return verifyHash(ModelPath(root, e), e.Model.SHA256)
}

// Resolve picks the root that holds a usable engine, preferring the bundled
// tree so a shipped install never depends on a download.
func Resolve(layout Layout, e Engine) (string, error) {
	switch {
	case layout.BundledRoot != "" && Present(layout.BundledRoot, e):
		return layout.BundledRoot, nil
	case layout.DownloadRoot != "" && Present(layout.DownloadRoot, e):
		return layout.DownloadRoot, nil
	default:
		return "", ErrNotInstalled
	}
}
