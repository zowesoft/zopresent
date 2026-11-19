package search

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"zopresent/internal/bible"
	"zopresent/internal/database"
	"zopresent/internal/songs"
)

func pending(key, text string) PendingDoc {
	return PendingDoc{Key: key, Domain: DomainBible, Label: key, Text: text, Hash: textHash(text)}
}

func TestBuildIndexIsIncremental(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	embedder := &fakeEmbedder{dims: 4}

	docs := []PendingDoc{pending("a", "alpha"), pending("b", "beta"), pending("c", "gamma")}
	var lastDone, lastTotal int
	progress := func(done, total int) { lastDone, lastTotal = done, total }
	if err := BuildIndex(ctx, store, embedder, "m1", 4, DomainBible, docs, progress); err != nil {
		t.Fatalf("first build: %v", err)
	}
	if embedder.calls != 1 || len(embedder.texts) != 3 {
		t.Fatalf("first build embedded %d texts in %d calls", len(embedder.texts), embedder.calls)
	}
	if lastDone != 3 || lastTotal != 3 {
		t.Fatalf("progress ended at %d/%d", lastDone, lastTotal)
	}

	// Nothing changed: a rebuild embeds nothing.
	embedder.calls, embedder.texts = 0, nil
	if err := BuildIndex(ctx, store, embedder, "m1", 4, DomainBible, docs, nil); err != nil {
		t.Fatalf("unchanged rebuild: %v", err)
	}
	if embedder.calls != 0 {
		t.Fatalf("unchanged rebuild embedded %d texts", len(embedder.texts))
	}

	// One edited document and one deletion: only the edit is re-embedded,
	// and the deleted row is pruned.
	docs[1].Text = "beta edited"
	docs[1].Hash = textHash(docs[1].Text)
	docs = docs[:2]
	embedder.calls, embedder.texts = 0, nil
	if err := BuildIndex(ctx, store, embedder, "m1", 4, DomainBible, docs, nil); err != nil {
		t.Fatalf("incremental rebuild: %v", err)
	}
	if len(embedder.texts) != 1 || embedder.texts[0] != "beta edited" {
		t.Fatalf("incremental rebuild embedded %v", embedder.texts)
	}
	keys, err := store.Keys(ctx, "m1")
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("stale document survived: %v", keys)
	}
}

func TestBuildIndexCancelResumes(t *testing.T) {
	store := newTestStore(t)
	embedder := &fakeEmbedder{dims: 4}

	// More than one batch, cancelled after the first: the stored batch
	// survives and the next build embeds only the remainder.
	docs := make([]PendingDoc, embedBatch+10)
	for i := range docs {
		docs[i] = pending(string(rune('a'+i%26))+string(rune('0'+i/26)), "text")
	}
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel once the first batch has landed.
	progress := func(done, total int) {
		if done >= embedBatch {
			cancel()
		}
	}
	err := BuildIndex(ctx, store, embedder, "m1", 4, DomainBible, docs, progress)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled build returned %v, want context.Canceled", err)
	}
	n, err := store.Count(context.Background(), "m1")
	if err != nil || n != embedBatch {
		t.Fatalf("stored %d docs after cancel, want %d (%v)", n, embedBatch, err)
	}

	embedder.calls, embedder.texts = 0, nil
	if err := BuildIndex(context.Background(), store, embedder, "m1", 4, DomainBible, docs, nil); err != nil {
		t.Fatalf("resume build: %v", err)
	}
	if len(embedder.texts) != 10 {
		t.Fatalf("resume embedded %d texts, want the remaining 10", len(embedder.texts))
	}
}

func TestBuildIndexRejectsWrongDims(t *testing.T) {
	store := newTestStore(t)
	embedder := &fakeEmbedder{dims: 3}
	err := BuildIndex(context.Background(), store, embedder, "m1", 4, DomainBible, []PendingDoc{pending("a", "alpha")}, nil)
	if err == nil {
		t.Fatal("a vector with the wrong dimension count was stored")
	}
}

func TestEnumerateCoversTranslationsAndSongs(t *testing.T) {
	ctx := context.Background()
	paths := database.Paths{Root: filepath.Join(t.TempDir(), "Zopresent")}
	if err := paths.Ensure(); err != nil {
		t.Fatalf("prepare data directories: %v", err)
	}
	db, err := database.Open(ctx, paths.CoreDatabase())
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := database.Migrate(ctx, db, paths.Backups()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	translations := bible.NewSQLiteRepository(db, paths.Translations())
	if _, err := translations.Install(ctx, bible.TranslationData{
		TranslationMeta: bible.TranslationMeta{
			ID: "kjv", Name: "KJV Test", Abbreviation: "KJV", Language: "en", Version: "1.0.0",
		},
		Verses: []bible.Verse{
			{BookID: 19, Chapter: 23, Number: 1, Text: "The LORD is my shepherd; I shall not want."},
			{BookID: 43, Chapter: 3, Number: 16, Text: "For God so loved the world."},
		},
	}); err != nil {
		t.Fatalf("install translation: %v", err)
	}

	songList := songs.NewSQLiteRepository(db)
	if _, err := songList.Create(ctx, songs.NewSong{
		Title:    "Amazing Grace",
		Sections: []songs.NewSection{{Label: "Verse 1", Text: "Amazing grace, how sweet the sound"}},
	}); err != nil {
		t.Fatalf("create song: %v", err)
	}

	docs, err := Enumerate(ctx, translations, songList)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	// 2 verses + 1 title + 1 section.
	if len(docs) != 4 {
		t.Fatalf("enumerated %d docs, want 4", len(docs))
	}
	byKey := make(map[string]PendingDoc, len(docs))
	for _, d := range docs {
		byKey[d.Key] = d
	}
	psalm, ok := byKey["bible:kjv:19:23:1"]
	if !ok || psalm.Label != "Psalms 23:1" || psalm.Testament != "old" || psalm.Domain != DomainBible {
		t.Fatalf("verse doc wrong: %+v", psalm)
	}
	var title, section *PendingDoc
	for i := range docs {
		if docs[i].Domain == DomainSong {
			if docs[i].SectionID == "" {
				title = &docs[i]
			} else {
				section = &docs[i]
			}
		}
	}
	if title == nil || title.Text != "Amazing Grace" {
		t.Fatalf("title doc wrong: %+v", title)
	}
	if section == nil || section.Text != "Amazing Grace\nAmazing grace, how sweet the sound" ||
		section.Label != "Amazing Grace (Verse 1)" {
		t.Fatalf("section doc wrong: %+v", section)
	}
}

func TestBuildIndexDomainsAreIndependent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	embedder := &fakeEmbedder{dims: 4}

	bibleDocs := []PendingDoc{pending("b1", "alpha"), pending("b2", "beta")}
	songDocs := []PendingDoc{
		{Key: "s1", Domain: DomainSong, Label: "s1", Text: "grace", Hash: textHash("grace")},
	}

	// Building scripture alone must not touch song rows, even though the song
	// corpus (the documents passed in) says nothing about them.
	if err := BuildIndex(ctx, store, embedder, "m1", 4, DomainBible, bibleDocs, nil); err != nil {
		t.Fatalf("build bible: %v", err)
	}
	n, err := store.CountDomain(ctx, "m1", DomainBible)
	if err != nil || n != 2 {
		t.Fatalf("bible count after build: %d, %v", n, err)
	}

	if err := BuildIndex(ctx, store, embedder, "m1", 4, DomainSong, songDocs, nil); err != nil {
		t.Fatalf("build songs: %v", err)
	}
	n, err = store.CountDomain(ctx, "m1", DomainSong)
	if err != nil || n != 1 {
		t.Fatalf("song count after build: %d, %v", n, err)
	}

	// Rebuilding scripture must not prune the song row: prune is domain-scoped.
	if err := BuildIndex(ctx, store, embedder, "m1", 4, DomainBible, bibleDocs, nil); err != nil {
		t.Fatalf("rebuild bible: %v", err)
	}
	n, err = store.CountDomain(ctx, "m1", DomainSong)
	if err != nil || n != 1 {
		t.Fatalf("song row was pruned by a scripture build: %d, %v", n, err)
	}

	// Dropping a bible document prunes only that key.
	if err := BuildIndex(ctx, store, embedder, "m1", 4, DomainBible, bibleDocs[:1], nil); err != nil {
		t.Fatalf("bible prune build: %v", err)
	}
	keys, err := store.KeysDomain(ctx, "m1", DomainBible)
	if err != nil || len(keys) != 1 {
		t.Fatalf("bible keys after prune: %v, %v", keys, err)
	}
	songKeys, err := store.KeysDomain(ctx, "m1", DomainSong)
	if err != nil || len(songKeys) != 1 {
		t.Fatalf("song keys after bible prune: %v, %v", songKeys, err)
	}
}

func TestEnumerateDomainSelectsOnlyItsCorpus(t *testing.T) {
	ctx := context.Background()
	paths := database.Paths{Root: filepath.Join(t.TempDir(), "Zopresent")}
	if err := paths.Ensure(); err != nil {
		t.Fatalf("prepare data directories: %v", err)
	}
	db, err := database.Open(ctx, paths.CoreDatabase())
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := database.Migrate(ctx, db, paths.Backups()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	translations := bible.NewSQLiteRepository(db, paths.Translations())
	if _, err := translations.Install(ctx, bible.TranslationData{
		TranslationMeta: bible.TranslationMeta{ID: "kjv", Name: "KJV Test", Abbreviation: "KJV", Language: "en", Version: "1.0.0"},
		Verses:          []bible.Verse{{BookID: 43, Chapter: 3, Number: 16, Text: "For God so loved the world."}},
	}); err != nil {
		t.Fatalf("install translation: %v", err)
	}
	songList := songs.NewSQLiteRepository(db)
	if _, err := songList.Create(ctx, songs.NewSong{
		Title:    "Amazing Grace",
		Sections: []songs.NewSection{{Label: "Verse 1", Text: "Amazing grace, how sweet the sound"}},
	}); err != nil {
		t.Fatalf("create song: %v", err)
	}

	bibleDocs, err := EnumerateDomain(ctx, DomainBible, translations, songList)
	if err != nil {
		t.Fatalf("enumerate bible: %v", err)
	}
	if len(bibleDocs) != 1 || bibleDocs[0].Domain != DomainBible {
		t.Fatalf("bible enumeration wrong: %+v", bibleDocs)
	}
	songDocs, err := EnumerateDomain(ctx, DomainSong, translations, songList)
	if err != nil {
		t.Fatalf("enumerate songs: %v", err)
	}
	if len(songDocs) != 2 || songDocs[0].Domain != DomainSong {
		t.Fatalf("song enumeration wrong: %+v", songDocs)
	}
	if _, err := EnumerateDomain(ctx, "nope", translations, songList); err == nil {
		t.Fatal("unknown domain was accepted")
	}
}
