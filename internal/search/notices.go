package search

import (
	"os"
	"path/filepath"
	"strings"
)

// NoticesFile sits beside the downloaded engine. It is named apart from the
// speech notices so installing one engine never overwrites the other's.
const NoticesFile = "THIRD-PARTY-NOTICES-EMBEDDING.txt"

// Component is one redistributed-or-fetched piece and its license.
type Component struct {
	Name     string
	License  string
	Homepage string
}

// Components lists what the embedding engine is made of. The files are fetched
// from their upstream releases at the operator's request; the licenses travel
// with those downloads and are named here so Settings can show them.
func Components() []Component {
	return []Component{
		{
			Name:     "llama.cpp (llama-server runtime)",
			License:  "MIT",
			Homepage: "https://github.com/ggml-org/llama.cpp",
		},
		{
			Name:     "all-MiniLM-L6-v2 embedding model",
			License:  "Apache-2.0",
			Homepage: "https://huggingface.co/sentence-transformers/all-MiniLM-L6-v2",
		},
		{
			Name:     "GGUF conversion of all-MiniLM-L6-v2 (Q8_0)",
			License:  "Apache-2.0 (model); conversion by second-state",
			Homepage: "https://huggingface.co/second-state/All-MiniLM-L6-v2-Embedding-GGUF",
		},
		{
			Name:     "LLVM OpenMP runtime (libomp, Windows build)",
			License:  "Apache-2.0 WITH LLVM-exception",
			Homepage: "https://openmp.llvm.org",
		},
	}
}

// Notices is the text written beside the engine and shown in Settings.
func Notices() string {
	var b strings.Builder
	b.WriteString("Zopresent semantic search — third-party notices\n")
	b.WriteString("================================================\n\n")
	b.WriteString("The embedding runtime and model are downloaded from their upstream\n")
	b.WriteString("releases when the operator installs them. Each component's license\n")
	b.WriteString("applies to the files fetched from its homepage.\n\n")
	for _, c := range Components() {
		b.WriteString("- " + c.Name + "\n")
		b.WriteString("  License: " + c.License + "\n")
		b.WriteString("  Homepage: " + c.Homepage + "\n\n")
	}
	b.WriteString("llama.cpp MIT license text:\n\n")
	b.WriteString(mitText)
	return b.String()
}

const mitText = `MIT License

Copyright (c) 2023-2024 The ggml authors

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
`

func writeNotices(root string) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, NoticesFile), []byte(Notices()), 0o644)
}
