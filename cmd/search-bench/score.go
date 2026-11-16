package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"zopresent/internal/database"
	"zopresent/internal/scripture"
	"zopresent/internal/songs"
)

// doc is one indexable unit: a Bible verse, a song title, or a lyric section.
type doc struct {
	text      string
	label     string // "John 3:16" or the song title
	bookID    int
	chapter   int
	verse     int
	testament string
	song      string // non-empty for song documents
}

type querySet struct {
	Name    string  `json:"name"`
	Queries []query `json:"queries"`
}

type query struct {
	ID        string   `json:"id"`
	Text      string   `json:"text"`
	Type      string   `json:"type"`             // semantic | keyword | trap
	Domain    string   `json:"domain,omitempty"` // bible (default) | song
	Gold      []string `json:"gold"`
	Testament string   `json:"testament,omitempty"`
	Books     []string `json:"books,omitempty"`
}

func (q query) domain() string {
	if q.Domain != "" {
		return q.Domain
	}
	return "bible"
}

func (q query) kind() string {
	if q.Type != "" {
		return q.Type
	}
	return "semantic"
}

// loadBibleCorpus reads every verse of the installed translation straight from
// its package database — the same rows the app's search and passage paths read.
func loadBibleCorpus(ctx context.Context, coreDB, translationDir, translationID string) ([]doc, error) {
	core, err := database.OpenReadOnly(ctx, coreDB)
	if err != nil {
		return nil, err
	}
	defer core.Close()

	var filename string
	err = core.QueryRowContext(ctx, `SELECT filename FROM bible_translations WHERE id = ?`, translationID).Scan(&filename)
	if err != nil {
		return nil, fmt.Errorf("translation %q is not installed: %w", translationID, err)
	}
	if filepath.Base(filename) != filename {
		return nil, fmt.Errorf("unsafe package filename %q", filename)
	}
	pkg, err := database.OpenReadOnly(ctx, filepath.Join(translationDir, filename))
	if err != nil {
		return nil, err
	}
	defer pkg.Close()

	rows, err := pkg.QueryContext(ctx, `
		SELECT b.id, b.name, b.testament, v.chapter, v.verse, v.text
		FROM verses v JOIN books b ON b.id = v.book_id
		ORDER BY b.id, v.chapter, v.verse`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var docs []doc
	for rows.Next() {
		var d doc
		var bookName string
		if err := rows.Scan(&d.bookID, &bookName, &d.testament, &d.chapter, &d.verse, &d.text); err != nil {
			return nil, err
		}
		d.label = fmt.Sprintf("%s %d:%d", bookName, d.chapter, d.verse)
		docs = append(docs, d)
	}
	return docs, rows.Err()
}

// loadSongCorpus indexes titles and lyric sections, matching what F08 will
// index: a title hit and a section hit both answer with their song.
func loadSongCorpus(ctx context.Context, coreDB string) ([]doc, error) {
	core, err := database.OpenReadOnly(ctx, coreDB)
	if err != nil {
		return nil, err
	}
	defer core.Close()

	list, err := songs.NewSQLiteRepository(core).List(ctx)
	if err != nil {
		return nil, err
	}
	var docs []doc
	for _, s := range list {
		docs = append(docs, doc{text: s.Title, label: s.Title, song: s.Title})
		for _, section := range s.Sections {
			docs = append(docs, doc{
				text:  s.Title + "\n" + section.Text,
				label: s.Title + " (" + section.Label + ")",
				song:  s.Title,
			})
		}
	}
	return docs, nil
}

// goldMatch reports whether a document satisfies one gold entry: a parsed
// reference for Bible queries, a song title for song queries.
type goldMatch struct {
	ref   scripture.Reference
	title string
}

func parseGold(q query) ([]goldMatch, error) {
	var out []goldMatch
	for _, entry := range q.Gold {
		if q.domain() == "song" {
			out = append(out, goldMatch{title: entry})
			continue
		}
		ref, err := scripture.Parse(entry)
		if err != nil {
			return nil, fmt.Errorf("query %s gold %q: %w", q.ID, entry, err)
		}
		out = append(out, goldMatch{ref: ref})
	}
	return out, nil
}

func (g goldMatch) matches(d doc) bool {
	if g.title != "" {
		return strings.EqualFold(d.song, g.title)
	}
	if d.bookID != g.ref.Book.ID || d.chapter != g.ref.Chapter {
		return false
	}
	end := g.ref.VerseEnd
	if end < g.ref.VerseStart {
		end = g.ref.VerseStart
	}
	return d.verse >= g.ref.VerseStart && d.verse <= end
}

// rankOfGold returns the 1-based position of the first gold match in a ranked
// document list, or 0 when no gold appears at all.
func rankOfGold(ranked []doc, gold []goldMatch) int {
	for i, d := range ranked {
		for _, g := range gold {
			if g.matches(d) {
				return i + 1
			}
		}
	}
	return 0
}

type typeScore struct {
	Queries  int     `json:"queries"`
	Recall1  float64 `json:"recall_at_1"`
	Recall5  float64 `json:"recall_at_5"`
	Recall10 float64 `json:"recall_at_10"`
	MRR      float64 `json:"mrr"`
}

type queryResult struct {
	ID        string  `json:"id"`
	Type      string  `json:"type"`
	Rank      int     `json:"rank"` // 0 = no gold within the ranked depth
	TopLabel  string  `json:"top_label,omitempty"`
	TopScore  float64 `json:"top_score,omitempty"`
	GoldScore float64 `json:"gold_score,omitempty"`
}

type trapResult struct {
	Query    string  `json:"query"`
	TopLabel string  `json:"top_label,omitempty"`
	TopScore float64 `json:"top_score,omitempty"`
	Returned int     `json:"returned"`
}

type domainScore struct {
	Overall     typeScore             `json:"overall"`
	ByType      map[string]*typeScore `json:"by_type"`
	Results     []queryResult         `json:"results,omitempty"`
	Traps       []trapResult          `json:"traps,omitempty"`
	MeanOverlap float64               `json:"mean_semantic_overlap,omitempty"`
	Depth       int                   `json:"rank_depth"`
}

func newDomainScore() *domainScore {
	return &domainScore{ByType: make(map[string]*typeScore)}
}

func (s *domainScore) add(q query, r queryResult, overlap float64, hasOverlap bool) {
	s.Results = append(s.Results, r)
	kinds := []*typeScore{&s.Overall}
	byType, ok := s.ByType[q.kind()]
	if !ok {
		byType = &typeScore{}
		s.ByType[q.kind()] = byType
	}
	kinds = append(kinds, byType)
	for _, score := range kinds {
		score.Queries++
		if r.Rank > 0 {
			if r.Rank <= 1 {
				score.Recall1++
			}
			if r.Rank <= 5 {
				score.Recall5++
			}
			if r.Rank <= 10 {
				score.Recall10++
			}
			score.MRR += 1.0 / float64(r.Rank)
		}
	}
	if hasOverlap {
		s.MeanOverlap += overlap
	}
}

func (s *domainScore) finish(semanticCount int) {
	finalise := func(score *typeScore) {
		if score.Queries == 0 {
			return
		}
		n := float64(score.Queries)
		score.Recall1 /= n
		score.Recall5 /= n
		score.Recall10 /= n
		score.MRR /= n
	}
	finalise(&s.Overall)
	for _, score := range s.ByType {
		finalise(score)
	}
	if semanticCount > 0 {
		s.MeanOverlap /= float64(semanticCount)
	}
}

// overlapFraction measures how much of a query's content vocabulary already
// appears in its gold verses. A semantic set worth calibrating on sits near
// zero: the model has to earn the match from meaning, not shared words.
func overlapFraction(queryText string, goldTexts []string) float64 {
	queryWords := contentWords(queryText)
	if len(queryWords) == 0 {
		return 0
	}
	goldWords := make(map[string]bool)
	for _, text := range goldTexts {
		for word := range contentWords(text) {
			goldWords[word] = true
		}
	}
	shared := 0
	for word := range queryWords {
		if goldWords[word] {
			shared++
		}
	}
	return float64(shared) / float64(len(queryWords))
}

var stopwords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true,
	"be": true, "but": true, "by": true, "for": true, "from": true, "has": true,
	"have": true, "he": true, "him": true, "his": true, "i": true, "in": true,
	"is": true, "it": true, "its": true, "me": true, "my": true, "not": true,
	"of": true, "on": true, "or": true, "our": true, "she": true, "so": true,
	"that": true, "the": true, "their": true, "them": true, "they": true,
	"this": true, "to": true, "unto": true, "upon": true, "was": true, "we": true,
	"were": true, "which": true, "who": true, "will": true, "with": true,
	"ye": true, "you": true, "your": true, "shall": true, "thou": true,
	"thy": true, "thine": true, "all": true, "any": true, "every": true,
	"when": true, "what": true, "how": true, "than": true, "then": true,
	"there": true, "these": true, "those": true, "through": true, "out": true,
	"up": true, "down": true, "no": true, "do": true, "does": true, "did": true,
	"can": true, "could": true, "would": true, "should": true, "may": true,
	"might": true, "must": true, "am": true, "been": true, "being": true,
	"because": true, "since": true, "while": true, "about": true, "into": true,
	"over": true, "after": true, "before": true, "between": true, "each": true,
	"other": true, "some": true, "such": true, "only": true, "own": true,
	"same": true, "too": true, "very": true, "just": true, "also": true,
}

func contentWords(text string) map[string]bool {
	out := make(map[string]bool)
	for _, word := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) {
		if !stopwords[word] {
			out[word] = true
		}
	}
	return out
}

// goldVerseTexts collects the stored text of a query's gold verses so overlap
// can be measured against what the index actually holds.
func goldVerseTexts(gold []goldMatch, docs []doc) []string {
	var texts []string
	for _, d := range docs {
		for _, g := range gold {
			if g.matches(d) {
				texts = append(texts, d.text)
				break
			}
		}
	}
	return texts
}

func percentilesMS(samples []float64) (p50, p95, max float64) {
	if len(samples) == 0 {
		return 0, 0, 0
	}
	sorted := append([]float64(nil), samples...)
	sort.Float64s(sorted)
	pick := func(fraction float64) float64 {
		return sorted[int(fraction*float64(len(sorted)-1))]
	}
	return pick(0.50), pick(0.95), sorted[len(sorted)-1]
}

// rankByCosine orders the candidate documents against a normalised query
// vector. Brute force is deliberate: at corpus size it keeps the relevance
// numbers exact, so the benchmark measures the model, not an ANN shortcut.
func rankByCosine(queryVec []float32, docs []doc, vectors [][]float32, keep func(doc) bool) ([]doc, []float64) {
	type scored struct {
		index int
		score float64
	}
	list := make([]scored, 0, len(docs))
	for i, d := range docs {
		if keep != nil && !keep(d) {
			continue
		}
		var dot float64
		vec := vectors[i]
		for j, v := range queryVec {
			dot += float64(v) * float64(vec[j])
		}
		list = append(list, scored{i, dot})
	}
	sort.Slice(list, func(a, b int) bool { return list[a].score > list[b].score })
	ranked := make([]doc, len(list))
	scores := make([]float64, len(list))
	for i, item := range list {
		ranked[i] = docs[item.index]
		scores[i] = item.score
	}
	return ranked, scores
}

// vectorCache persists embeddings keyed by model identity and corpus content,
// so repeated benchmark runs measure query behaviour without paying the
// indexing cost again. Cold vs warm is reported, never hidden.
type vectorCache struct {
	dir         string
	fingerprint string
}

func newVectorCache(dir, engineName, modelPath string, bibleDocs, songDocs []doc) *vectorCache {
	if dir == "" {
		return nil
	}
	hash := sha256.New()
	fmt.Fprintf(hash, "%s|%s\n", engineName, modelPath)
	if info, err := os.Stat(modelPath); err == nil {
		fmt.Fprintf(hash, "%d|%d\n", info.Size(), info.ModTime().UnixNano())
	}
	for _, d := range bibleDocs {
		fmt.Fprintf(hash, "b:%s\n", d.text)
	}
	for _, d := range songDocs {
		fmt.Fprintf(hash, "s:%s\n", d.text)
	}
	return &vectorCache{dir: dir, fingerprint: hex.EncodeToString(hash.Sum(nil))[:16]}
}

func (c *vectorCache) path(engineName string) string {
	return filepath.Join(c.dir, engineName+"-"+c.fingerprint+".zsb")
}

func (c *vectorCache) load(engineName string, wantBible, wantSongs int) (bible, songVectors [][]float32, dims int, err error) {
	raw, err := os.ReadFile(c.path(engineName))
	if err != nil {
		return nil, nil, 0, err
	}
	if len(raw) < 16 || string(raw[:4]) != "ZSB1" {
		return nil, nil, 0, fmt.Errorf("bad cache header")
	}
	dims = int(binary.LittleEndian.Uint32(raw[4:8]))
	nBible := int(binary.LittleEndian.Uint32(raw[8:12]))
	nSongs := int(binary.LittleEndian.Uint32(raw[12:16]))
	if nBible != wantBible || nSongs != wantSongs || dims <= 0 {
		return nil, nil, 0, fmt.Errorf("cache shape mismatch")
	}
	if len(raw) < 16+(nBible+nSongs)*dims*4 {
		return nil, nil, 0, fmt.Errorf("cache truncated")
	}
	read := func(offset, count int) [][]float32 {
		out := make([][]float32, count)
		for i := range out {
			vec := make([]float32, dims)
			base := 16 + (offset+i)*dims*4
			for j := range vec {
				vec[j] = math.Float32frombits(binary.LittleEndian.Uint32(raw[base+j*4:]))
			}
			out[i] = vec
		}
		return out
	}
	return read(0, nBible), read(nBible, nSongs), dims, nil
}

func (c *vectorCache) save(engineName string, bible, songVectors [][]float32, dims int) error {
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return err
	}
	buf := make([]byte, 16+(len(bible)+len(songVectors))*dims*4)
	copy(buf, "ZSB1")
	binary.LittleEndian.PutUint32(buf[4:8], uint32(dims))
	binary.LittleEndian.PutUint32(buf[8:12], uint32(len(bible)))
	binary.LittleEndian.PutUint32(buf[12:16], uint32(len(songVectors)))
	offset := 16
	for _, vec := range append(append([][]float32(nil), bible...), songVectors...) {
		for _, v := range vec {
			binary.LittleEndian.PutUint32(buf[offset:], math.Float32bits(v))
			offset += 4
		}
	}
	return os.WriteFile(c.path(engineName), buf, 0o644)
}
