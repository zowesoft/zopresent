package search

import (
	"context"
	"path/filepath"
	"testing"

	"zopresent/internal/database"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	paths := database.Paths{Root: filepath.Join(t.TempDir(), "Zopresent")}
	if err := paths.Ensure(); err != nil {
		t.Fatalf("prepare data directories: %v", err)
	}
	db, err := database.Open(context.Background(), paths.CoreDatabase())
	if err != nil {
		t.Fatalf("open core database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := database.Migrate(context.Background(), db, paths.Backups()); err != nil {
		t.Fatalf("migrate core database: %v", err)
	}
	return NewStore(db)
}

// fakeEmbedder returns a deterministic vector per text and records what it was
// asked to embed, so incremental behaviour is observable without a runtime.
type fakeEmbedder struct {
	dims  int
	calls int
	texts []string
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.calls++
	f.texts = append(f.texts, texts...)
	out := make([][]float32, len(texts))
	for i, text := range texts {
		v := make([]float32, f.dims)
		for j := range v {
			v[j] = float32(byte(text[j%len(text)])) / 255
		}
		out[i] = normalize(v)
	}
	return out, nil
}

func testDoc(key, model, text string, vector []float32) Doc {
	return Doc{
		Key:    key,
		Domain: DomainBible,
		Model:  model,
		Dims:   len(vector),
		Hash:   textHash(text),
		Vector: vector,
		Label:  key,
		Text:   text,
	}
}
