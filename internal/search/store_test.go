package search

import (
	"context"
	"reflect"
	"testing"
)

func TestStoreRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	doc := testDoc("bible:kjv:19:23:1", "m1", "The LORD is my shepherd.", []float32{0.6, 0.8})
	doc.TranslationID = "kjv"
	doc.BookID, doc.Chapter, doc.Verse, doc.Testament = 19, 23, 1, "old"
	if err := store.Upsert(ctx, []Doc{doc}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	loaded, err := store.Load(ctx, "m1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("loaded %d docs, want 1", len(loaded))
	}
	got := loaded[0]
	if got.Key != doc.Key || got.Label != doc.Label || got.Text != doc.Text ||
		got.TranslationID != "kjv" || got.BookID != 19 || got.Testament != "old" {
		t.Fatalf("metadata lost in round trip: %+v", got)
	}
	if !reflect.DeepEqual(got.Vector, doc.Vector) {
		t.Fatalf("vector changed in round trip: %v", got.Vector)
	}

	// Upsert replaces the vector for the same key rather than duplicating it.
	doc.Vector = []float32{1, 0}
	doc.Hash = textHash("changed")
	if err := store.Upsert(ctx, []Doc{doc}); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	keys, err := store.Keys(ctx, "m1")
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	if len(keys) != 1 || keys[doc.Key] != textHash("changed") {
		t.Fatalf("keys after replace: %v", keys)
	}
}

func TestStorePruneRemovesOtherModelsAndStaleKeys(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	docs := []Doc{
		testDoc("keep:1", "m1", "a", []float32{1, 0}),
		testDoc("stale:1", "m1", "b", []float32{0, 1}),
		testDoc("other:1", "m0", "c", []float32{1, 1}),
	}
	if err := store.Upsert(ctx, docs); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	removed, err := store.Prune(ctx, "m1", DomainBible, map[string]struct{}{"keep:1": {}})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed %d rows, want 2", removed)
	}
	n, err := store.Count(ctx, "m1")
	if err != nil || n != 1 {
		t.Fatalf("count after prune: %d, %v", n, err)
	}
	n, err = store.Count(ctx, "m0")
	if err != nil || n != 0 {
		t.Fatalf("other model survived prune: %d, %v", n, err)
	}
}

func TestStoreDecodeRejectsWrongSize(t *testing.T) {
	if _, err := decodeVector([]byte{1, 2, 3}, 2); err == nil {
		t.Fatal("a truncated vector blob was accepted")
	}
}
