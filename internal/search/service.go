package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"zopresent/internal/bible"
	"zopresent/internal/songs"
)

// Index states reported to the operator. "stale" means content changed since
// the last build; retrieval still answers from the stored snapshot until a
// rebuild refreshes it.
const (
	StateEmpty    = "empty"
	StateReady    = "ready"
	StateStale    = "stale"
	StateBuilding = "building"
)

// DomainStatus is the install/build state of one index domain. Scripture and
// songs are built separately, so each carries its own counts and state.
type DomainStatus struct {
	State       string
	IndexedDocs int
	PendingDocs int
	StaleDocs   int
	TotalDocs   int
}

type Status struct {
	Supported   bool
	Installed   bool
	ModelID     string
	ModelName   string
	Dims        int
	State       string
	IndexedDocs int
	PendingDocs int
	StaleDocs   int
	TotalDocs   int
	// Bible and Songs hold the per-domain breakdown. The combined fields above
	// keep reporting the whole library for callers that only show one badge.
	Bible DomainStatus
	Songs DomainStatus
}

// Request is one meaning-based query. Domain selects scripture or songs;
// scripture honors the same translation, testament and book controls as
// full-text search.
type Request struct {
	Query         string
	Domain        string
	TranslationID string
	Testament     string
	BookIDs       []int
	Limit         int
	Offset        int
}

// Hit is a stored document with its cosine similarity to the query. The score
// ranks suggestions; it is not a confidence and never projects by itself.
type Hit struct {
	Doc   Doc
	Score float64
}

// Service ties the engine, the index and retrieval together. The sidecar starts
// on first use and stops with the app; an operator who never searches by
// meaning never pays for it.
type Service struct {
	store        *Store
	translations *bible.SQLiteRepository
	songs        *songs.SQLiteRepository
	layout       Layout
	engine       Engine
	logDir       string

	serverMu sync.Mutex
	server   *Server

	mu       sync.Mutex
	cache    []Doc
	building map[string]bool
	cancel   map[string]context.CancelFunc
}

func NewService(core *sql.DB, translations *bible.SQLiteRepository, songList *songs.SQLiteRepository, layout Layout, logDir string) *Service {
	return &Service{
		store:        NewStore(core),
		translations: translations,
		songs:        songList,
		layout:       layout,
		engine:       MiniLM(),
		logDir:       logDir,
	}
}

// Status reports what the operator can do right now: install, build, or
// search. Counts come from comparing the live corpus against stored hashes,
// per domain, so an edited song shows as pending without the scripture index
// looking stale (and the reverse).
func (s *Service) Status(ctx context.Context) (Status, error) {
	st := Status{
		ModelID:   s.engine.ID,
		ModelName: s.engine.Name,
		Dims:      s.engine.Dims,
		State:     StateEmpty,
	}
	_, st.Supported = s.engine.HostRuntime()
	_, err := Resolve(s.layout, s.engine)
	st.Installed = err == nil

	building := s.buildingDomains()
	if len(building) > 0 {
		st.State = StateBuilding
	}
	if !st.Installed {
		return st, nil
	}

	for _, domain := range []string{DomainBible, DomainSong} {
		ds, err := s.domainStatus(ctx, domain, keysForDomain, building[domain])
		if err != nil {
			return st, err
		}
		if domain == DomainBible {
			st.Bible = ds
		} else {
			st.Songs = ds
		}
		st.IndexedDocs += ds.IndexedDocs
		st.PendingDocs += ds.PendingDocs
		st.StaleDocs += ds.StaleDocs
		st.TotalDocs += ds.TotalDocs
	}

	switch {
	case len(building) > 0:
		st.State = StateBuilding
	case st.IndexedDocs == 0:
		st.State = StateEmpty
	case st.PendingDocs > 0 || st.StaleDocs > 0:
		st.State = StateStale
	default:
		st.State = StateReady
	}
	return st, nil
}

// keysForDomain reads the stored hashes of one domain so the corpus diff never
// crosses domains.
func keysForDomain(ctx context.Context, store *Store, model, domain string) (map[string]string, error) {
	return store.KeysDomain(ctx, model, domain)
}

func (s *Service) domainStatus(ctx context.Context, domain string, keys func(context.Context, *Store, string, string) (map[string]string, error), building bool) (DomainStatus, error) {
	ds := DomainStatus{State: StateEmpty}

	indexed, err := s.store.CountDomain(ctx, s.engine.ID, domain)
	if err != nil {
		return ds, err
	}
	ds.IndexedDocs = indexed

	docs, err := EnumerateDomain(ctx, domain, s.translations, s.songs)
	if err != nil {
		return ds, err
	}
	ds.TotalDocs = len(docs)
	stored, err := keys(ctx, s.store, s.engine.ID, domain)
	if err != nil {
		return ds, err
	}
	current := make(map[string]struct{}, len(docs))
	for _, d := range docs {
		current[d.Key] = struct{}{}
		if hash, ok := stored[d.Key]; !ok || hash != d.Hash {
			ds.PendingDocs++
		}
	}
	for key := range stored {
		if _, ok := current[key]; !ok {
			ds.StaleDocs++
		}
	}

	switch {
	case building:
		ds.State = StateBuilding
	case indexed == 0:
		ds.State = StateEmpty
	case ds.PendingDocs > 0 || ds.StaleDocs > 0:
		ds.State = StateStale
	default:
		ds.State = StateReady
	}
	return ds, nil
}

// Install downloads the model and runtime into the writable root. It runs on
// the caller's goroutine; the app wraps it in a background task with progress.
func (s *Service) Install(ctx context.Context, progress func(Progress)) error {
	if s.layout.DownloadRoot == "" {
		return errors.New("no writable engine location is available")
	}
	installer := NewInstaller()
	installer.Progress = progress
	return installer.Install(s.layout.DownloadRoot, s.engine)
}

// Building reports whether any index build is in flight.
func (s *Service) Building() bool {
	return len(s.buildingDomains()) > 0
}

// BuildingDomain reports whether the named domain is mid-build.
func (s *Service) BuildingDomain(domain string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.building[domain]
}

func (s *Service) buildingDomains() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]bool, len(s.building))
	for domain, busy := range s.building {
		if busy {
			out[domain] = true
		}
	}
	return out
}

// Build refreshes one domain's index incrementally; "" means both. The
// caller's context cancels the build; CancelBuild is the handle for the
// operator's stop button. Documents already stored survive a cancel, so the
// next build resumes rather than starting over. Building scripture never
// embeds, prunes or blocks the song index, and the reverse.
func (s *Service) Build(ctx context.Context, domain string, progress func(done, total int)) error {
	switch domain {
	case "", DomainBible, DomainSong:
	default:
		return fmt.Errorf("unknown search domain %q", domain)
	}
	domains := []string{DomainBible, DomainSong}
	if domain != "" {
		domains = []string{domain}
	}

	s.mu.Lock()
	for _, d := range domains {
		if s.building[d] {
			s.mu.Unlock()
			return fmt.Errorf("the %s index build is already running", d)
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	if s.building == nil {
		s.building = make(map[string]bool, 2)
	}
	if s.cancel == nil {
		s.cancel = make(map[string]context.CancelFunc, 2)
	}
	for _, d := range domains {
		s.building[d] = true
		s.cancel[d] = cancel
	}
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		for _, d := range domains {
			delete(s.building, d)
			delete(s.cancel, d)
		}
		s.mu.Unlock()
	}()

	server, err := s.ensureServer(ctx)
	if err != nil {
		return err
	}
	for _, d := range domains {
		docs, err := EnumerateDomain(ctx, d, s.translations, s.songs)
		if err != nil {
			return err
		}
		if err := BuildIndex(ctx, s.store, server, s.engine.ID, s.engine.Dims, d, docs, progress); err != nil {
			return err
		}
	}

	s.mu.Lock()
	s.cache = nil
	s.mu.Unlock()
	return nil
}

// CancelBuild stops an in-flight build; "" cancels every domain that is
// building at the moment.
func (s *Service) CancelBuild(domain string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if domain == "" {
		for _, cancel := range s.cancel {
			cancel()
		}
		return
	}
	if cancel, ok := s.cancel[domain]; ok {
		cancel()
	}
}

// Search ranks stored documents by meaning. Exact/reference search stays a
// separate mode; nothing here rewrites text or projects a result.
func (s *Service) Search(ctx context.Context, req Request) ([]Hit, error) {
	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, errors.New("search text is required")
	}
	switch req.Domain {
	case DomainBible:
		if strings.TrimSpace(req.TranslationID) == "" {
			return nil, errors.New("translation is required")
		}
	case DomainSong:
	default:
		return nil, fmt.Errorf("unknown search domain %q", req.Domain)
	}
	if _, err := Resolve(s.layout, s.engine); err != nil {
		return nil, err
	}
	indexed, err := s.store.CountDomain(ctx, s.engine.ID, req.Domain)
	if err != nil {
		return nil, err
	}
	if indexed == 0 {
		label := "scripture"
		if req.Domain == DomainSong {
			label = "song"
		}
		return nil, fmt.Errorf("%w — the %s index has not been built yet", ErrIndexEmpty, label)
	}

	limit := req.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	offset := max(req.Offset, 0)

	server, err := s.ensureServer(ctx)
	if err != nil {
		return nil, err
	}
	docs, err := s.loadCache(ctx)
	if err != nil {
		return nil, err
	}
	vectors, err := server.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	return rankHits(docs, vectors[0], req, limit, offset), nil
}

// rankHits scores every document that passes the query's filters and returns
// the requested page, best first. Both sides are L2-normalized, so the dot
// product is the cosine similarity.
func rankHits(docs []Doc, q []float32, req Request, limit, offset int) []Hit {
	bookFilter := make(map[int]struct{}, len(req.BookIDs))
	for _, id := range req.BookIDs {
		bookFilter[id] = struct{}{}
	}

	hits := make([]Hit, 0, len(docs))
	for _, d := range docs {
		if d.Domain != req.Domain {
			continue
		}
		if req.Domain == DomainBible {
			if d.TranslationID != req.TranslationID {
				continue
			}
			if req.Testament != "" && d.Testament != req.Testament {
				continue
			}
			if len(bookFilter) > 0 {
				if _, ok := bookFilter[d.BookID]; !ok {
					continue
				}
			}
		}
		hits = append(hits, Hit{Doc: d, Score: dot(q, d.Vector)})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Doc.Key < hits[j].Doc.Key
	})
	if offset >= len(hits) {
		return []Hit{}
	}
	end := min(offset+limit, len(hits))
	return hits[offset:end]
}

// Close stops the sidecar. The index stays on disk for the next run.
func (s *Service) Close() {
	s.serverMu.Lock()
	defer s.serverMu.Unlock()
	if s.server != nil {
		s.server.Stop()
		s.server = nil
	}
}

func (s *Service) ensureServer(ctx context.Context) (*Server, error) {
	s.serverMu.Lock()
	defer s.serverMu.Unlock()
	if s.server != nil && !s.server.Exited() {
		return s.server, nil
	}
	if s.server != nil {
		s.server.Stop()
		s.server = nil
	}
	root, err := Resolve(s.layout, s.engine)
	if err != nil {
		return nil, err
	}
	bin, err := BinaryPath(root, s.engine)
	if err != nil {
		return nil, err
	}
	server, err := StartServer(ctx, bin, ModelPath(root, s.engine), filepath.Join(s.logDir, "llama-server.log"))
	if err != nil {
		return nil, err
	}
	s.server = server
	return server, nil
}

func (s *Service) loadCache(ctx context.Context) ([]Doc, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache == nil {
		docs, err := s.store.Load(ctx, s.engine.ID)
		if err != nil {
			return nil, err
		}
		s.cache = docs
	}
	return s.cache, nil
}

func dot(a, b []float32) float64 {
	var sum float64
	for i := range a {
		if i >= len(b) {
			break
		}
		sum += float64(a[i]) * float64(b[i])
	}
	return sum
}
