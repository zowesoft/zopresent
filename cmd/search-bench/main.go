// Command search-bench measures local semantic search against the app's FTS
// baseline on a labeled query set, which is the prerequisite the tracker sets
// before any embedding model or vector storage is selected for F08.
//
// Each -engine spec launches an embedding sidecar (llama-server) over HTTP,
// embeds the installed translation's verses plus the song catalog, and ranks
// the labeled queries by cosine similarity. The FTS lane runs the production
// search path on the same queries. The report covers relevance (recall@k,
// MRR, split by query type), indexing cost, query latency percentiles, model
// size and sidecar memory — the four numbers the PRD requires before a model
// is chosen. The harness is pure Go; only the sidecar binary is per-OS.
//
// Usage:
//
//	go run ./cmd/search-bench -set benchmarks/search-set ^
//	  -core-db "%LOCALAPPDATA%\Zowesoft\Zopresent\zopresent.db" ^
//	  -translation-dir "%LOCALAPPDATA%\Zowesoft\Zopresent\translations" ^
//	  -engine "minilm=benchmarks\tools\llama-b12436\llama-server.exe -m benchmarks\models\all-MiniLM-L6-v2-Q8_0.gguf --embeddings --port {port} -c 8192 -b 8192 -ub 2048 --parallel 4 -t 4"
//
// With no -engine flags only the FTS baseline runs. See benchmarks/README.md.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"zopresent/internal/bible"
	"zopresent/internal/database"
	"zopresent/internal/songs"
)

type multiFlag struct{ values []string }

func (m *multiFlag) String() string { return strings.Join(m.values, ",") }
func (m *multiFlag) Set(v string) error {
	m.values = append(m.values, v)
	return nil
}

var verboseQueries bool

type latencyReport struct {
	P50MS float64 `json:"p50_ms"`
	P95MS float64 `json:"p95_ms"`
	MaxMS float64 `json:"max_ms"`
}

type modelInfo struct {
	Path   string  `json:"path"`
	SizeMB float64 `json:"size_mb"`
	Dims   int     `json:"dims"`
}

type indexInfo struct {
	Docs           int     `json:"docs"`
	MS             int64   `json:"ms"`
	DocsPerSec     float64 `json:"docs_per_sec"`
	VectorMB       float64 `json:"vector_mb"`
	PeakRSSMB      float64 `json:"peak_rss_mb"`
	QueryPeakRSSMB float64 `json:"query_peak_rss_mb"`
	Cache          string  `json:"cache"` // cold | warm
	CacheLoadMS    int64   `json:"cache_load_ms,omitempty"`
}

type laneReport struct {
	Name          string                   `json:"name"`
	Kind          string                   `json:"kind"` // fts | embedding
	Model         *modelInfo               `json:"model,omitempty"`
	Index         *indexInfo               `json:"index,omitempty"`
	Bible         *domainScore             `json:"bible"`
	Songs         *domainScore             `json:"songs,omitempty"`
	QueryMS       latencyReport            `json:"query_ms"`
	QueryMSByType map[string]latencyReport `json:"query_ms_by_type,omitempty"`
}

type benchReport struct {
	Translation string       `json:"translation"`
	Verses      int          `json:"verses"`
	SongDocs    int          `json:"song_docs"`
	Queries     int          `json:"queries"`
	Lanes       []laneReport `json:"lanes"`
}

func main() {
	setDir := flag.String("set", "benchmarks/search-set", "directory containing the query manifest.json")
	coreDB := flag.String("core-db", "", "core database holding the translation catalog and songs")
	translationDir := flag.String("translation-dir", "", "translations directory")
	translationID := flag.String("translation", "kjv", "translation id to index and search")
	var engineSpecs multiFlag
	flag.Var(&engineSpecs, "engine", "embedding sidecar spec name=bin args... {port}; repeatable")
	batchSize := flag.Int("batch", 256, "texts per embedding request")
	queryRuns := flag.Int("runs", 3, "timed runs per query (ranking is deterministic; all runs time)")
	depth := flag.Int("depth", 50, "FTS retrieval depth for ranking")
	cacheDir := flag.String("cache", "", "optional directory caching vectors between runs")
	outJSON := flag.String("out", "", "optional path for a JSON report")
	logDir := flag.String("log-dir", "benchmarks/dumps/search", "directory for sidecar logs")
	verbose := flag.Bool("v", false, "print per-query ranks and top hits")
	flag.Parse()
	verboseQueries = *verbose

	if *coreDB == "" || *translationDir == "" {
		exit("-core-db and -translation-dir are required (the benchmark reads the installed catalog)")
	}
	set, err := readQuerySet(filepath.Join(*setDir, "manifest.json"))
	if err != nil {
		exit(err.Error())
	}
	if len(set.Queries) == 0 {
		exit("query set declares no queries")
	}

	ctx := context.Background()
	bibleDocs, err := loadBibleCorpus(ctx, *coreDB, *translationDir, *translationID)
	if err != nil {
		exit(err.Error())
	}
	songDocs, err := loadSongCorpus(ctx, *coreDB)
	if err != nil {
		exit(err.Error())
	}

	golds := make(map[string][]goldMatch)
	overlaps := make(map[string]float64)
	semanticCount := map[string]int{}
	for _, q := range set.Queries {
		gold, err := parseGold(q)
		if err != nil {
			exit(err.Error())
		}
		if len(gold) == 0 && q.kind() != "trap" {
			exit(fmt.Sprintf("query %s: type %q needs gold entries", q.ID, q.kind()))
		}
		golds[q.ID] = gold
		if q.domain() == "bible" && q.kind() == "semantic" && len(gold) > 0 {
			overlaps[q.ID] = overlapFraction(q.Text, goldVerseTexts(gold, bibleDocs))
			semanticCount["bible"]++
		}
		if q.domain() == "song" && q.kind() == "semantic" {
			semanticCount["song"]++
		}
		if q.domain() == "song" && len(songDocs) == 0 {
			exit(fmt.Sprintf("query %s targets songs but the catalog is empty", q.ID))
		}
	}

	report := benchReport{
		Translation: *translationID,
		Verses:      len(bibleDocs),
		SongDocs:    len(songDocs),
		Queries:     len(set.Queries),
	}
	report.Lanes = append(report.Lanes, runFTSLane(ctx, *coreDB, *translationDir, *translationID, set, golds, overlaps, semanticCount, *depth))

	for _, spec := range engineSpecs.values {
		lane, err := runEmbedLane(ctx, spec, set, golds, overlaps, semanticCount, bibleDocs, songDocs, *batchSize, *queryRuns, *cacheDir, *logDir)
		if err != nil {
			exit(fmt.Sprintf("engine %s: %v", spec, err))
		}
		report.Lanes = append(report.Lanes, lane)
	}

	printReport(report)
	if *outJSON != "" {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			exit(err.Error())
		}
		if err := os.WriteFile(*outJSON, data, 0o644); err != nil {
			exit(fmt.Sprintf("write %s: %v", *outJSON, err))
		}
		fmt.Printf("report written to %s\n", *outJSON)
	}
}

func readQuerySet(path string) (querySet, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return querySet{}, err
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	var set querySet
	if err := json.Unmarshal(raw, &set); err != nil {
		return querySet{}, fmt.Errorf("%s: %w", path, err)
	}
	return set, nil
}

// runFTSLane scores the production full-text path on the same queries, so the
// embedding lanes are measured against what the app already ships.
func runFTSLane(ctx context.Context, coreDB, translationDir, translationID string, set querySet, golds map[string][]goldMatch, overlaps map[string]float64, semanticCount map[string]int, depth int) laneReport {
	core, err := database.OpenReadOnly(ctx, coreDB)
	if err != nil {
		exit(err.Error())
	}
	defer core.Close()
	bibleRepo := bible.NewSQLiteRepository(core, translationDir)
	songRepo := songs.NewSQLiteRepository(core)

	lane := laneReport{Name: "fts", Kind: "fts", Bible: newDomainScore(), Songs: newDomainScore()}
	lane.Bible.Depth = depth
	lane.Songs.Depth = depth
	var samples []float64
	byKind := make(map[string][]float64)

	// Warmup: the first queries pay for SQLite page cache and FTS index load,
	// which says nothing about steady-state service use.
	for i := 0; i < 3; i++ {
		if _, err := bibleRepo.Search(ctx, bible.SearchRequest{TranslationID: translationID, Text: "god love world", Mode: bible.SearchWords, Limit: depth}); err != nil {
			exit(fmt.Sprintf("fts warmup: %v", err))
		}
		if _, err := songRepo.Search(ctx, "grace", depth); err != nil {
			exit(fmt.Sprintf("fts song warmup: %v", err))
		}
	}

	for _, q := range set.Queries {
		gold := golds[q.ID]
		started := time.Now()
		var ranked []doc
		var returned int
		if q.domain() == "song" {
			hits, err := songRepo.Search(ctx, q.Text, depth)
			if err != nil {
				exit(fmt.Sprintf("fts song query %s: %v", q.ID, err))
			}
			returned = len(hits)
			for _, hit := range hits {
				ranked = append(ranked, doc{song: hit.Song.Title, label: hit.Song.Title})
			}
		} else {
			req := bible.SearchRequest{
				TranslationID: translationID, Text: q.Text, Mode: bible.SearchWords, Limit: depth,
			}
			if q.Testament != "" {
				req.Testament = bible.Testament(q.Testament)
			}
			for _, name := range q.Books {
				if book, ok := bible.LookupBook(name); ok {
					req.BookIDs = append(req.BookIDs, book.ID)
				}
			}
			results, err := bibleRepo.Search(ctx, req)
			if err != nil {
				exit(fmt.Sprintf("fts query %s: %v", q.ID, err))
			}
			returned = len(results)
			for _, result := range results {
				ranked = append(ranked, doc{bookID: result.Book.ID, chapter: result.Chapter, verse: result.Verse, label: result.Reference()})
			}
		}
		elapsedMS := float64(time.Since(started).Microseconds()) / 1000.0
		samples = append(samples, elapsedMS)
		byKind[q.kind()] = append(byKind[q.kind()], elapsedMS)

		score := lane.Bible
		if q.domain() == "song" {
			score = lane.Songs
		}
		if q.kind() == "trap" {
			trap := trapResult{Query: q.Text, Returned: returned}
			if len(ranked) > 0 {
				trap.TopLabel = ranked[0].label
			}
			score.Traps = append(score.Traps, trap)
			continue
		}
		overlap, hasOverlap := overlaps[q.ID]
		result := queryResult{ID: q.ID, Type: q.kind(), Rank: rankOfGold(ranked, gold)}
		if len(ranked) > 0 {
			result.TopLabel = ranked[0].label
		}
		score.add(q, result, overlap, hasOverlap)
	}
	lane.Bible.finish(semanticCount["bible"])
	lane.Songs.finish(semanticCount["song"])
	lane.QueryMS = latencyOf(samples)
	lane.QueryMSByType = latencyByKind(byKind)
	return lane
}

func runEmbedLane(ctx context.Context, spec string, set querySet, golds map[string][]goldMatch, overlaps map[string]float64, semanticCount map[string]int, bibleDocs, songDocs []doc, batchSize, queryRuns int, cacheDir, logDir string) (laneReport, error) {
	engine, err := parseSpec(spec)
	if err != nil {
		return laneReport{}, err
	}
	if err := engine.start(logDir); err != nil {
		engine.close()
		return laneReport{}, err
	}
	defer engine.close()

	modelPath := modelPathOf(engine.args)
	lane := laneReport{Name: engine.name, Kind: "embedding", Bible: newDomainScore(), Songs: newDomainScore()}
	lane.Bible.Depth = len(bibleDocs)
	lane.Songs.Depth = len(songDocs)
	lane.Model = &modelInfo{Path: modelPath, Dims: 0}
	if info, err := os.Stat(modelPath); err == nil {
		lane.Model.SizeMB = float64(info.Size()) / (1 << 20)
	}

	cache := newVectorCache(cacheDir, engine.name, modelPath, bibleDocs, songDocs)
	var bibleVectors, songVectors [][]float32
	index := &indexInfo{Docs: len(bibleDocs) + len(songDocs), Cache: "cold"}
	if cache != nil {
		loadStart := time.Now()
		b, s, dims, err := cache.load(engine.name, len(bibleDocs), len(songDocs))
		if err == nil {
			bibleVectors, songVectors = b, s
			engine.dims = dims
			index.Cache = "warm"
			index.CacheLoadMS = int64(time.Since(loadStart) / time.Millisecond)
		}
	}
	if index.Cache == "cold" {
		started := time.Now()
		bibleVectors, err = embedAll(engine, bibleDocs, engine.docPrefix, batchSize)
		if err != nil {
			return laneReport{}, err
		}
		songVectors, err = embedAll(engine, songDocs, engine.docPrefix, batchSize)
		if err != nil {
			return laneReport{}, err
		}
		index.MS = int64(time.Since(started) / time.Millisecond)
		if cache != nil {
			if err := cache.save(engine.name, bibleVectors, songVectors, engine.dims); err != nil {
				fmt.Fprintf(os.Stderr, "warning: cache save: %v\n", err)
			}
		}
	}
	lane.Model.Dims = engine.dims
	index.DocsPerSec = float64(index.Docs) / (float64(index.MS) / 1000.0)
	if index.Cache == "warm" {
		index.DocsPerSec = 0
	}
	index.VectorMB = float64(index.Docs*engine.dims*4) / (1 << 20)
	index.PeakRSSMB = float64(engine.indexPeak.Load()) / 1024.0
	lane.Index = index

	engine.phase.Store(1)
	var samples []float64
	byKind := make(map[string][]float64)
	for _, q := range set.Queries {
		docs, vectors := bibleDocs, bibleVectors
		score := lane.Bible
		if q.domain() == "song" {
			docs, vectors = songDocs, songVectors
			score = lane.Songs
		}
		keep := filterFor(q)
		gold := golds[q.ID]

		var ranked []doc
		var scores []float64
		for run := 0; run < queryRuns; run++ {
			started := time.Now()
			vecs, err := engine.embed([]string{engine.queryPrefix + q.Text})
			if err != nil {
				return laneReport{}, fmt.Errorf("query %s: %w", q.ID, err)
			}
			ranked, scores = rankByCosine(vecs[0], docs, vectors, keep)
			elapsedMS := float64(time.Since(started).Microseconds()) / 1000.0
			samples = append(samples, elapsedMS)
			byKind[q.kind()] = append(byKind[q.kind()], elapsedMS)
		}

		if q.kind() == "trap" {
			trap := trapResult{Query: q.Text, Returned: len(ranked)}
			if len(ranked) > 0 {
				trap.TopLabel = ranked[0].label
				trap.TopScore = scores[0]
			}
			score.Traps = append(score.Traps, trap)
			continue
		}
		overlap, hasOverlap := overlaps[q.ID]
		result := queryResult{ID: q.ID, Type: q.kind(), Rank: rankOfGold(ranked, gold)}
		if len(ranked) > 0 {
			result.TopLabel = ranked[0].label
			result.TopScore = scores[0]
			if result.Rank > 0 {
				result.GoldScore = scores[result.Rank-1]
			}
		}
		score.add(q, result, overlap, hasOverlap)
	}
	lane.Bible.finish(semanticCount["bible"])
	lane.Songs.finish(semanticCount["song"])
	lane.QueryMS = latencyOf(samples)
	lane.QueryMSByType = latencyByKind(byKind)
	index.QueryPeakRSSMB = float64(engine.queryPeak.Load()) / 1024.0
	return lane, nil
}

// embedAll streams the corpus through the sidecar in batches, reporting
// progress on stderr so a long cold index is visibly alive.
func embedAll(engine *embedEngine, docs []doc, prefix string, batchSize int) ([][]float32, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	out := make([][]float32, 0, len(docs))
	started := time.Now()
	for start := 0; start < len(docs); start += batchSize {
		end := start + batchSize
		if end > len(docs) {
			end = len(docs)
		}
		texts := make([]string, end-start)
		for i, d := range docs[start:end] {
			texts[i] = prefix + d.text
		}
		vecs, err := engine.embed(texts)
		if err != nil {
			return nil, fmt.Errorf("embed docs %d-%d: %w", start, end, err)
		}
		out = append(out, vecs...)
		if end%2048 < batchSize || end == len(docs) {
			elapsed := time.Since(started).Seconds()
			fmt.Fprintf(os.Stderr, "  %s: embedded %d/%d (%.0f docs/s)\n", engine.name, end, len(docs), float64(end)/elapsed)
		}
	}
	return out, nil
}

func filterFor(q query) func(doc) bool {
	if q.Testament == "" && len(q.Books) == 0 {
		return nil
	}
	bookIDs := make(map[int]bool)
	for _, name := range q.Books {
		if book, ok := bible.LookupBook(name); ok {
			bookIDs[book.ID] = true
		}
	}
	return func(d doc) bool {
		if q.Testament != "" && !strings.EqualFold(d.testament, q.Testament) {
			return false
		}
		if len(bookIDs) > 0 && !bookIDs[d.bookID] {
			return false
		}
		return true
	}
}

func modelPathOf(args []string) string {
	for i, arg := range args {
		if (arg == "-m" || arg == "--model") && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(arg, "-m=") {
			return strings.TrimPrefix(arg, "-m=")
		}
		if strings.HasPrefix(arg, "--model=") {
			return strings.TrimPrefix(arg, "--model=")
		}
	}
	return ""
}

func latencyOf(samples []float64) latencyReport {
	p50, p95, max := percentilesMS(samples)
	return latencyReport{P50MS: p50, P95MS: p95, MaxMS: max}
}

func latencyByKind(byKind map[string][]float64) map[string]latencyReport {
	out := make(map[string]latencyReport, len(byKind))
	for kind, samples := range byKind {
		out[kind] = latencyOf(samples)
	}
	return out
}

func printReport(report benchReport) {
	fmt.Printf("search-bench: %s — %d verses, %d song docs, %d queries\n\n",
		report.Translation, report.Verses, report.SongDocs, report.Queries)
	for _, lane := range report.Lanes {
		fmt.Printf("== %s (%s) ==\n", lane.Name, lane.Kind)
		if lane.Model != nil {
			fmt.Printf("  model  %s (%.1f MB, %d dims)\n", lane.Model.Path, lane.Model.SizeMB, lane.Model.Dims)
		}
		if lane.Index != nil {
			cache := lane.Index.Cache
			if lane.Index.Cache == "warm" {
				cache = fmt.Sprintf("warm, loaded in %dms", lane.Index.CacheLoadMS)
			}
			fmt.Printf("  index  %d docs in %.1fs (%.0f docs/s), vectors %.1f MB, peak RSS %.0f MB [%s]\n",
				lane.Index.Docs, float64(lane.Index.MS)/1000.0, lane.Index.DocsPerSec,
				lane.Index.VectorMB, lane.Index.PeakRSSMB, cache)
		}
		printDomain("bible", lane.Bible)
		if lane.Songs != nil && lane.Songs.Overall.Queries+len(lane.Songs.Traps) > 0 {
			printDomain("songs", lane.Songs)
		}
		fmt.Printf("  query  p50 %.1fms  p95 %.1fms  max %.1fms\n", lane.QueryMS.P50MS, lane.QueryMS.P95MS, lane.QueryMS.MaxMS)
		for _, kind := range []string{"semantic", "keyword", "trap"} {
			if lat, ok := lane.QueryMSByType[kind]; ok {
				fmt.Printf("         %-8s p50 %.1fms  p95 %.1fms\n", kind, lat.P50MS, lat.P95MS)
			}
		}
		if lane.Index != nil {
			fmt.Printf("         query peak RSS %.0f MB\n", lane.Index.QueryPeakRSSMB)
		}
		fmt.Println()
	}

	fmt.Println("summary  semantic recall@10 / keyword recall@10 / query p95 / index time / peak RSS")
	for _, lane := range report.Lanes {
		semantic, keyword := 0.0, 0.0
		if t := lane.Bible.ByType["semantic"]; t != nil {
			semantic = t.Recall10 * 100
		}
		if t := lane.Bible.ByType["keyword"]; t != nil {
			keyword = t.Recall10 * 100
		}
		indexTime, rss := "-", "-"
		if lane.Index != nil {
			indexTime = fmt.Sprintf("%.0fs", float64(lane.Index.MS)/1000.0)
			rss = fmt.Sprintf("%.0fMB", lane.Index.PeakRSSMB)
		}
		fmt.Printf("  %-14s %5.1f%%  %5.1f%%  %7.1fms  %8s  %8s\n",
			lane.Name, semantic, keyword, lane.QueryMS.P95MS, indexTime, rss)
	}
}

func printDomain(name string, score *domainScore) {
	if score.Overall.Queries == 0 && len(score.Traps) == 0 {
		return
	}
	fmt.Printf("  %-6s recall@1 %.1f%%  @5 %.1f%%  @10 %.1f%%  MRR %.3f  (%d queries, depth %d)\n",
		name, score.Overall.Recall1*100, score.Overall.Recall5*100, score.Overall.Recall10*100,
		score.Overall.MRR, score.Overall.Queries, score.Depth)
	for _, kind := range []string{"semantic", "keyword"} {
		if t := score.ByType[kind]; t != nil {
			fmt.Printf("         %-8s @10 %.1f%%  MRR %.3f  (n=%d)\n", kind, t.Recall10*100, t.MRR, t.Queries)
		}
	}
	if name == "bible" && score.MeanOverlap > 0 {
		fmt.Printf("         semantic query-gold word overlap %.1f%%\n", score.MeanOverlap*100)
	}
	if verboseQueries {
		for _, r := range score.Results {
			rank := "miss"
			if r.Rank > 0 {
				rank = fmt.Sprintf("%d", r.Rank)
			}
			if r.TopScore > 0 {
				fmt.Printf("           %-22s %-8s rank %-5s top %-24s %.3f  gold %.3f\n",
					r.ID, r.Type, rank, r.TopLabel, r.TopScore, r.GoldScore)
			} else {
				fmt.Printf("           %-22s %-8s rank %-5s top %s\n", r.ID, r.Type, rank, r.TopLabel)
			}
		}
	}
	for _, trap := range score.Traps {
		if trap.TopLabel != "" {
			fmt.Printf("         trap %q -> top %q (score %.3f, %d returned)\n", trap.Query, trap.TopLabel, trap.TopScore, trap.Returned)
		} else {
			fmt.Printf("         trap %q -> nothing returned\n", trap.Query)
		}
	}
}

func exit(msg string) {
	fmt.Fprintln(os.Stderr, "search-bench: "+msg)
	os.Exit(1)
}
