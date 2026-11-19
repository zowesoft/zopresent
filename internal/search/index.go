package search

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"zopresent/internal/bible"
	"zopresent/internal/songs"
)

// Embedder turns texts into vectors. The sidecar implements it; tests supply
// deterministic fakes so indexing and ranking run without a runtime binary.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// PendingDoc is a corpus document before embedding: everything needed to key,
// hash and label it. The hash is over the exact text that gets embedded, so a
// rebuild re-embeds only what changed.
type PendingDoc struct {
	Key           string
	Domain        string
	Label         string
	Text          string
	Hash          string
	TranslationID string
	BookID        int
	Chapter       int
	Verse         int
	Testament     string
	SongID        string
	SectionID     string
}

const (
	DomainBible = "bible"
	DomainSong  = "song"
)

// Enumerate lists every document the index should hold: each verse of each
// installed translation, plus song titles and lyric sections. Song text is
// title + section so a lyric hit carries its song's name into the embedding,
// matching what the benchmark measured.
func Enumerate(ctx context.Context, translations *bible.SQLiteRepository, songList *songs.SQLiteRepository) ([]PendingDoc, error) {
	docs, err := EnumerateDomain(ctx, DomainBible, translations, songList)
	if err != nil {
		return nil, err
	}
	songDocs, err := EnumerateDomain(ctx, DomainSong, translations, songList)
	if err != nil {
		return nil, err
	}
	return append(docs, songDocs...), nil
}

// DomainFilter reports which domains a build or query covers. Scripture and
// songs are embedded and pruned independently so each can be built on its own
// schedule and one domain's rebuild never waits on the other's corpus.
func DomainFilter(domain string) func(string) bool {
	switch domain {
	case DomainBible, DomainSong:
		return func(d string) bool { return d == domain }
	default:
		return func(string) bool { return true }
	}
}

// EnumerateDomain lists the documents of one domain; "" means every domain.
func EnumerateDomain(ctx context.Context, domain string, translations *bible.SQLiteRepository, songList *songs.SQLiteRepository) ([]PendingDoc, error) {
	if domain == DomainSong {
		return enumerateSongs(ctx, songList)
	}
	if domain != "" && domain != DomainBible {
		return nil, fmt.Errorf("unknown search domain %q", domain)
	}
	docs, err := enumerateBible(ctx, translations)
	if err != nil || domain != "" {
		return docs, err
	}
	songDocs, err := enumerateSongs(ctx, songList)
	if err != nil {
		return nil, err
	}
	return append(docs, songDocs...), nil
}

func enumerateBible(ctx context.Context, translations *bible.SQLiteRepository) ([]PendingDoc, error) {
	var docs []PendingDoc

	list, err := translations.ListTranslations(ctx)
	if err != nil {
		return nil, err
	}
	for _, translation := range list {
		verses, err := translations.AllVerses(ctx, translation.ID)
		if err != nil {
			return nil, err
		}
		for _, v := range verses {
			book, ok := bible.BookByID(v.BookID)
			if !ok {
				return nil, fmt.Errorf("translation %s: unknown book id %d", translation.ID, v.BookID)
			}
			docs = append(docs, PendingDoc{
				Key:           fmt.Sprintf("bible:%s:%d:%d:%d", translation.ID, v.BookID, v.Chapter, v.Number),
				Domain:        DomainBible,
				Label:         fmt.Sprintf("%s %d:%d", book.Name, v.Chapter, v.Number),
				Text:          v.Text,
				Hash:          textHash(v.Text),
				TranslationID: translation.ID,
				BookID:        v.BookID,
				Chapter:       v.Chapter,
				Verse:         v.Number,
				Testament:     string(book.Testament),
			})
		}
	}
	return docs, nil
}

func enumerateSongs(ctx context.Context, songList *songs.SQLiteRepository) ([]PendingDoc, error) {
	list, err := songList.List(ctx)
	if err != nil {
		return nil, err
	}
	var docs []PendingDoc
	for _, s := range list {
		docs = append(docs, PendingDoc{
			Key:    "song:" + s.ID + ":title",
			Domain: DomainSong,
			Label:  s.Title,
			Text:   s.Title,
			Hash:   textHash(s.Title),
			SongID: s.ID,
		})
		for _, section := range s.Sections {
			text := s.Title + "\n" + section.Text
			docs = append(docs, PendingDoc{
				Key:       "song:" + s.ID + ":section:" + section.ID,
				Domain:    DomainSong,
				Label:     s.Title + " (" + section.Label + ")",
				Text:      text,
				Hash:      textHash(text),
				SongID:    s.ID,
				SectionID: section.ID,
			})
		}
	}
	return docs, nil
}

func textHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// embedBatch matches the benchmark's batch size: large enough to keep the
// runtime fed, small enough that progress moves and a cancel lands promptly.
const embedBatch = 256

// BuildIndex embeds the documents whose text is new or changed, then prunes
// what no longer belongs in the given domain. It is incremental by
// construction: an interrupted build resumes from the rows already stored, and
// a song edit costs a few documents rather than the whole library again.
func BuildIndex(ctx context.Context, store *Store, embedder Embedder, model string, dims int, domain string, docs []PendingDoc, progress func(done, total int)) error {
	existing, err := store.KeysDomain(ctx, model, domain)
	if err != nil {
		return err
	}

	var pending []PendingDoc
	keep := make(map[string]struct{}, len(docs))
	for _, d := range docs {
		keep[d.Key] = struct{}{}
		if hash, ok := existing[d.Key]; !ok || hash != d.Hash {
			pending = append(pending, d)
		}
	}

	total := len(pending)
	done := 0
	if progress != nil {
		progress(done, total)
	}
	for start := 0; start < total; start += embedBatch {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(start+embedBatch, total)
		batch := pending[start:end]

		texts := make([]string, len(batch))
		for i, d := range batch {
			texts[i] = d.Text
		}
		vectors, err := embedder.Embed(ctx, texts)
		if err != nil {
			return fmt.Errorf("embed batch at %d: %w", start, err)
		}
		if len(vectors) != len(batch) {
			return fmt.Errorf("embed batch at %d: got %d vectors for %d texts", start, len(vectors), len(batch))
		}

		upserts := make([]Doc, len(batch))
		for i, d := range batch {
			if len(vectors[i]) != dims {
				return fmt.Errorf("embed batch at %d: vector %d has %d dims, want %d", start, i, len(vectors[i]), dims)
			}
			upserts[i] = Doc{
				Key:           d.Key,
				Domain:        d.Domain,
				Model:         model,
				Dims:          dims,
				Hash:          d.Hash,
				Vector:        vectors[i],
				Label:         d.Label,
				Text:          d.Text,
				TranslationID: d.TranslationID,
				BookID:        d.BookID,
				Chapter:       d.Chapter,
				Verse:         d.Verse,
				Testament:     d.Testament,
				SongID:        d.SongID,
				SectionID:     d.SectionID,
			}
		}
		if err := store.Upsert(ctx, upserts); err != nil {
			return err
		}
		done = end
		if progress != nil {
			progress(done, total)
		}
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	_, err = store.Prune(ctx, model, domain, keep)
	return err
}
