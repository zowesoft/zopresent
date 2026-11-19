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

func verse(key string, vector []float32, translation string, bookID int, testament string) Doc {
	d := testDoc(key, "m1", key, vector)
	d.TranslationID = translation
	d.BookID = bookID
	d.Testament = testament
	return d
}

func TestRankHitsOrdersFiltersAndPages(t *testing.T) {
	q := []float32{1, 0}
	docs := []Doc{
		verse("b", normalize([]float32{0, 1}), "kjv", 19, "old"),
		verse("a", normalize([]float32{1, 0}), "kjv", 43, "new"),
		verse("c", normalize([]float32{0.6, 0.8}), "kjv", 43, "new"),
		verse("f", normalize([]float32{1, 0}), "kjv", 43, "new"),
		verse("other-t", normalize([]float32{1, 0}), "asv", 43, "new"),
		{Key: "song", Domain: DomainSong, Vector: normalize([]float32{1, 0})},
	}
	req := Request{Domain: DomainBible, TranslationID: "kjv"}

	hits := rankHits(docs, q, req, 10, 0)
	wantOrder := []string{"a", "f", "c", "b"}
	if len(hits) != len(wantOrder) {
		t.Fatalf("got %d hits, want %d", len(hits), len(wantOrder))
	}
	for i, key := range wantOrder {
		if hits[i].Doc.Key != key {
			t.Fatalf("hit %d is %q, want %q", i, hits[i].Doc.Key, key)
		}
	}
	if hits[0].Score < hits[2].Score || hits[2].Score < hits[3].Score {
		t.Fatalf("scores not descending: %v", hits)
	}

	page := rankHits(docs, q, req, 2, 2)
	if len(page) != 2 || page[0].Doc.Key != "c" || page[1].Doc.Key != "b" {
		t.Fatalf("second page wrong: %+v", page)
	}
	if got := rankHits(docs, q, req, 2, 99); len(got) != 0 {
		t.Fatalf("offset past the end returned %d hits", len(got))
	}

	newTestament := rankHits(docs, q, Request{Domain: DomainBible, TranslationID: "kjv", Testament: "new"}, 10, 0)
	if len(newTestament) != 3 {
		t.Fatalf("testament filter kept %d hits, want 3", len(newTestament))
	}
	oneBook := rankHits(docs, q, Request{Domain: DomainBible, TranslationID: "kjv", BookIDs: []int{19}}, 10, 0)
	if len(oneBook) != 1 || oneBook[0].Doc.Key != "b" {
		t.Fatalf("book filter wrong: %+v", oneBook)
	}
	songHits := rankHits(docs, q, Request{Domain: DomainSong}, 10, 0)
	if len(songHits) != 1 || songHits[0].Doc.Key != "song" {
		t.Fatalf("song domain wrong: %+v", songHits)
	}
}

func TestSearchValidatesBeforeTouchingTheEngine(t *testing.T) {
	svc := &Service{store: newTestStore(t), engine: MiniLM()}
	ctx := context.Background()

	cases := []Request{
		{Query: "  ", Domain: DomainSong},
		{Query: "grace", Domain: "unknown"},
		{Query: "grace", Domain: DomainBible},
	}
	for _, req := range cases {
		if _, err := svc.Search(ctx, req); err == nil {
			t.Fatalf("Search accepted invalid request %+v", req)
		}
	}

	// Valid requests with no engine on disk must explain, not start anything.
	if _, err := svc.Search(ctx, Request{Query: "grace", Domain: DomainSong}); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("missing engine returned %v, want ErrNotInstalled", err)
	}
	if _, err := svc.Search(ctx, Request{Query: "grace", Domain: DomainBible, TranslationID: "kjv"}); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("missing engine returned %v, want ErrNotInstalled", err)
	}
}

func TestSearchReportsEmptyIndex(t *testing.T) {
	root := t.TempDir()
	e := syntheticEngine(t, root)
	svc := &Service{store: newTestStore(t), engine: e, layout: Layout{DownloadRoot: root}}
	_, err := svc.Search(context.Background(), Request{Query: "grace", Domain: DomainSong})
	if !errors.Is(err, ErrIndexEmpty) {
		t.Fatalf("empty index returned %v, want ErrIndexEmpty", err)
	}
}

func newTestService(t *testing.T, layout Layout, engine Engine) (*Service, *bible.SQLiteRepository, *songs.SQLiteRepository) {
	t.Helper()
	paths := database.Paths{Root: filepath.Join(t.TempDir(), "Zopresent")}
	if err := paths.Ensure(); err != nil {
		t.Fatalf("prepare data directories: %v", err)
	}
	db, err := database.Open(context.Background(), paths.CoreDatabase())
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := database.Migrate(context.Background(), db, paths.Backups()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	translations := bible.NewSQLiteRepository(db, paths.Translations())
	songList := songs.NewSQLiteRepository(db)
	svc := &Service{
		store:        NewStore(db),
		translations: translations,
		songs:        songList,
		layout:       layout,
		engine:       engine,
		logDir:       t.TempDir(),
	}
	return svc, translations, songList
}

func TestStatusTracksInstallBuildAndEdits(t *testing.T) {
	ctx := context.Background()

	// Nothing installed: the operator is told to install, nothing else runs.
	svc, _, _ := newTestService(t, Layout{}, MiniLM())
	st, err := svc.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Installed || st.State != StateEmpty {
		t.Fatalf("uninstalled status wrong: %+v", st)
	}

	root := t.TempDir()
	e := syntheticEngine(t, root)
	svc, translations, songList := newTestService(t, Layout{DownloadRoot: root}, e)

	if _, err := translations.Install(ctx, bible.TranslationData{
		TranslationMeta: bible.TranslationMeta{ID: "kjv", Name: "KJV Test", Abbreviation: "KJV", Language: "en", Version: "1.0.0"},
		Verses:          []bible.Verse{{BookID: 43, Chapter: 3, Number: 16, Text: "For God so loved the world."}},
	}); err != nil {
		t.Fatalf("install translation: %v", err)
	}
	if _, err := songList.Create(ctx, songs.NewSong{
		Title:    "Amazing Grace",
		Sections: []songs.NewSection{{Label: "Verse 1", Text: "Amazing grace, how sweet the sound"}},
	}); err != nil {
		t.Fatalf("create song: %v", err)
	}

	st, err = svc.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.Installed || st.State != StateEmpty || st.TotalDocs != 3 || st.PendingDocs != 3 {
		t.Fatalf("pre-build status wrong: %+v", st)
	}

	docs, err := Enumerate(ctx, translations, songList)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	if err := BuildIndex(ctx, svc.store, &fakeEmbedder{dims: 4}, e.ID, 4, "", docs, nil); err != nil {
		t.Fatalf("build: %v", err)
	}
	st, err = svc.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.State != StateReady || st.IndexedDocs != 3 || st.PendingDocs != 0 || st.StaleDocs != 0 {
		t.Fatalf("post-build status wrong: %+v", st)
	}

	// A new song makes the index stale without anyone rebuilding by hand.
	if _, err := songList.Create(ctx, songs.NewSong{
		Title:    "It Is Well",
		Sections: []songs.NewSection{{Label: "Verse 1", Text: "When peace like a river attendeth my way"}},
	}); err != nil {
		t.Fatalf("create song: %v", err)
	}
	st, err = svc.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.State != StateStale || st.PendingDocs != 2 {
		t.Fatalf("post-edit status wrong: %+v", st)
	}
}

func TestStatusAndBuildArePerDomain(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	e := syntheticEngine(t, root)
	svc, translations, songList := newTestService(t, Layout{DownloadRoot: root}, e)

	if _, err := translations.Install(ctx, bible.TranslationData{
		TranslationMeta: bible.TranslationMeta{ID: "kjv", Name: "KJV Test", Abbreviation: "KJV", Language: "en", Version: "1.0.0"},
		Verses:          []bible.Verse{{BookID: 43, Chapter: 3, Number: 16, Text: "For God so loved the world."}},
	}); err != nil {
		t.Fatalf("install translation: %v", err)
	}
	if _, err := songList.Create(ctx, songs.NewSong{
		Title:    "Amazing Grace",
		Sections: []songs.NewSection{{Label: "Verse 1", Text: "Amazing grace, how sweet the sound"}},
	}); err != nil {
		t.Fatalf("create song: %v", err)
	}

	// Build scripture only. Its domain reports ready; songs stay empty.
	bibleDocs, err := EnumerateDomain(ctx, DomainBible, translations, songList)
	if err != nil {
		t.Fatalf("enumerate bible: %v", err)
	}
	if err := BuildIndex(ctx, svc.store, &fakeEmbedder{dims: 4}, e.ID, 4, DomainBible, bibleDocs, nil); err != nil {
		t.Fatalf("build bible: %v", err)
	}
	st, err := svc.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Bible.State != StateReady || st.Bible.IndexedDocs != 1 || st.Bible.PendingDocs != 0 {
		t.Fatalf("bible status wrong: %+v", st.Bible)
	}
	if st.Songs.State != StateEmpty || st.Songs.TotalDocs != 2 || st.Songs.PendingDocs != 2 {
		t.Fatalf("song status wrong: %+v", st.Songs)
	}
	if st.State != StateStale || st.PendingDocs != 2 {
		t.Fatalf("combined status wrong: %+v", st)
	}

	// Editing a song leaves scripture ready and only marks songs pending.
	if _, err := songList.Create(ctx, songs.NewSong{
		Title:    "It Is Well",
		Sections: []songs.NewSection{{Label: "Verse 1", Text: "When peace like a river attendeth my way"}},
	}); err != nil {
		t.Fatalf("create song: %v", err)
	}
	st, err = svc.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Bible.State != StateReady || st.Bible.PendingDocs != 0 {
		t.Fatalf("song edit marked scripture stale: %+v", st.Bible)
	}
	if st.Songs.State != StateEmpty || st.Songs.PendingDocs != 4 {
		t.Fatalf("song status after edit wrong: %+v", st.Songs)
	}

	// Editing scripture alone marks only scripture pending.
	if _, err := translations.Install(ctx, bible.TranslationData{
		TranslationMeta: bible.TranslationMeta{ID: "asv", Name: "ASV Test", Abbreviation: "ASV", Language: "en", Version: "1.0.0"},
		Verses:          []bible.Verse{{BookID: 19, Chapter: 23, Number: 1, Text: "The LORD is my shepherd."}},
	}); err != nil {
		t.Fatalf("install second translation: %v", err)
	}
	st, err = svc.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Bible.State != StateStale || st.Bible.PendingDocs != 1 {
		t.Fatalf("scripture edit status wrong: %+v", st.Bible)
	}
	if st.Bible.IndexedDocs != 1 || st.Bible.TotalDocs != 2 {
		t.Fatalf("bible counts wrong: %+v", st.Bible)
	}
}

func TestBuildRejectsUnknownDomain(t *testing.T) {
	svc, _, _ := newTestService(t, Layout{}, MiniLM())
	if err := svc.Build(context.Background(), "nope", nil); err == nil {
		t.Fatal("Build accepted an unknown domain")
	}
}
