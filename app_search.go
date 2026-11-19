package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"zopresent/internal/bible"
	"zopresent/internal/search"
)

// semanticProgressEvent shows movement during the model download and the index
// build; Phase tells the two apart.
const semanticProgressEvent = "semantic:progress"

// semanticConfigEvent refreshes both windows when an install or build finishes,
// so the meaning mode appears without an app restart.
const semanticConfigEvent = "semantic:config"

// SemanticDomainStatusView is one index domain's install/build state, so the
// Settings card can show and trigger scripture and songs independently.
type SemanticDomainStatusView struct {
	State       string `json:"state"`
	IndexedDocs int    `json:"indexedDocs"`
	PendingDocs int    `json:"pendingDocs"`
	StaleDocs   int    `json:"staleDocs"`
	TotalDocs   int    `json:"totalDocs"`
}

// SemanticStatusView is what Settings shows: whether meaning search can be
// installed, built or used right now.
type SemanticStatusView struct {
	Supported   bool   `json:"supported"`
	Installed   bool   `json:"installed"`
	ModelID     string `json:"modelId"`
	ModelName   string `json:"modelName"`
	Dims        int    `json:"dims"`
	State       string `json:"state"`
	IndexedDocs int    `json:"indexedDocs"`
	PendingDocs int    `json:"pendingDocs"`
	StaleDocs   int    `json:"staleDocs"`
	TotalDocs   int    `json:"totalDocs"`

	Bible SemanticDomainStatusView `json:"bible"`
	Songs SemanticDomainStatusView `json:"songs"`
}

type SemanticProgressView struct {
	Phase      string `json:"phase"`  // "download" | "index"
	Domain     string `json:"domain"` // "bible" | "song" for index progress
	File       string `json:"file"`
	Done       int    `json:"done"`
	Total      int    `json:"total"`
	Bytes      int64  `json:"bytes"`
	TotalBytes int64  `json:"totalBytes"`
}

// SemanticSearchRequest is one meaning-based query over scripture or songs.
type SemanticSearchRequest struct {
	Query         string `json:"query"`
	Domain        string `json:"domain"` // "bible" | "song"
	TranslationID string `json:"translationId"`
	Testament     string `json:"testament"`
	BookIDs       []int  `json:"bookIds"`
	Limit         int    `json:"limit"`
	Offset        int    `json:"offset"`
}

// SemanticHitView is a stored document with its similarity. The score ranks
// suggestions for the operator; it is not a detection confidence.
type SemanticHitView struct {
	Key    string  `json:"key"`
	Domain string  `json:"domain"`
	Score  float64 `json:"score"`
	Label  string  `json:"label"`
	Text   string  `json:"text"`

	TranslationID string `json:"translationId,omitempty"`
	BookID        int    `json:"bookId,omitempty"`
	Chapter       int    `json:"chapter,omitempty"`
	Verse         int    `json:"verse,omitempty"`
	Reference     string `json:"reference,omitempty"`
	Testament     string `json:"testament,omitempty"`

	SongID     string `json:"songId,omitempty"`
	SongTitle  string `json:"songTitle,omitempty"`
	SectionID  string `json:"sectionId,omitempty"`
	Author     string `json:"author,omitempty"`
	Copyright  string `json:"copyright,omitempty"`
	CCLINumber string `json:"ccliNumber,omitempty"`
	Source     string `json:"source,omitempty"`
	Position   int    `json:"position,omitempty"`
}

// semanticInstall guards the download so a second click cannot start a
// competing fetch.
var semanticInstall struct {
	mu      sync.Mutex
	running bool
}

func (a *App) GetSemanticStatus() (SemanticStatusView, error) {
	if a.semantic == nil {
		return SemanticStatusView{}, errStorageUnavailable
	}
	st, err := a.semantic.Status(a.context())
	if err != nil {
		return SemanticStatusView{}, err
	}
	return SemanticStatusView{
		Supported:   st.Supported,
		Installed:   st.Installed,
		ModelID:     st.ModelID,
		ModelName:   st.ModelName,
		Dims:        st.Dims,
		State:       st.State,
		IndexedDocs: st.IndexedDocs,
		PendingDocs: st.PendingDocs,
		StaleDocs:   st.StaleDocs,
		TotalDocs:   st.TotalDocs,
		Bible:       semanticDomainView(st.Bible),
		Songs:       semanticDomainView(st.Songs),
	}, nil
}

func semanticDomainView(ds search.DomainStatus) SemanticDomainStatusView {
	return SemanticDomainStatusView{
		State:       ds.State,
		IndexedDocs: ds.IndexedDocs,
		PendingDocs: ds.PendingDocs,
		StaleDocs:   ds.StaleDocs,
		TotalDocs:   ds.TotalDocs,
	}
}

// InstallSemanticEngine returns once the download starts; the outcome arrives
// as semantic:config.
func (a *App) InstallSemanticEngine() error {
	if a.semantic == nil {
		return errStorageUnavailable
	}
	semanticInstall.mu.Lock()
	if semanticInstall.running {
		semanticInstall.mu.Unlock()
		return errInstallInProgress
	}
	semanticInstall.running = true
	semanticInstall.mu.Unlock()

	go a.runSemanticInstall()
	return nil
}

func (a *App) runSemanticInstall() {
	defer func() {
		semanticInstall.mu.Lock()
		semanticInstall.running = false
		semanticInstall.mu.Unlock()
	}()

	err := a.semantic.Install(a.context(), func(p search.Progress) {
		a.dispatchToWindow(settingsWindow, semanticProgressEvent, SemanticProgressView{
			Phase:      "download",
			File:       p.File,
			Done:       p.Done,
			Total:      p.Total,
			Bytes:      p.Bytes,
			TotalBytes: p.TotalBytes,
		})
	})
	a.finishSemantic(err)
}

// BuildSemanticIndex refreshes one domain's index in the background; an empty
// domain builds both. Already-stored documents survive a stop, so the next
// build resumes rather than restarting.
func (a *App) BuildSemanticIndex(domain string) error {
	if a.semantic == nil {
		return errStorageUnavailable
	}
	switch domain {
	case "", search.DomainBible, search.DomainSong:
	default:
		return errors.New("unknown search domain")
	}
	if domain != "" && a.semantic.BuildingDomain(domain) {
		return errors.New("the index build is already running")
	}
	go a.runSemanticBuild(domain)
	return nil
}

func (a *App) runSemanticBuild(domain string) {
	err := a.semantic.Build(a.context(), domain, func(done, total int) {
		a.dispatchToWindow(settingsWindow, semanticProgressEvent, SemanticProgressView{
			Phase:  "index",
			Domain: domain,
			Done:   done,
			Total:  total,
		})
	})
	a.finishSemantic(err)
}

// StopSemanticIndex cancels an in-flight build at the next batch boundary;
// an empty domain stops whichever build is running.
func (a *App) StopSemanticIndex(domain string) error {
	if a.semantic == nil {
		return errStorageUnavailable
	}
	a.semantic.CancelBuild(domain)
	return nil
}

func (a *App) finishSemantic(err error) {
	message := ""
	if err != nil && !errors.Is(err, context.Canceled) {
		message = err.Error()
	}
	data := map[string]any{
		"ok":        err == nil,
		"cancelled": errors.Is(err, context.Canceled),
		"error":     message,
	}
	a.dispatchToWindow(settingsWindow, semanticConfigEvent, data)
	a.dispatchToWindow(studioWindow, semanticConfigEvent, data)
}

// SearchSemantic ranks stored scripture or song text by meaning. Nothing here
// projects: the operator previews and sends live exactly as with text search.
func (a *App) SearchSemantic(req SemanticSearchRequest) ([]SemanticHitView, error) {
	if a.semantic == nil {
		return nil, errStorageUnavailable
	}
	ctx := a.context()
	hits, err := a.semantic.Search(ctx, search.Request{
		Query:         req.Query,
		Domain:        req.Domain,
		TranslationID: req.TranslationID,
		Testament:     req.Testament,
		BookIDs:       req.BookIDs,
		Limit:         req.Limit,
		Offset:        req.Offset,
	})
	if err != nil {
		return nil, err
	}

	views := make([]SemanticHitView, 0, len(hits))
	for _, hit := range hits {
		view := SemanticHitView{
			Key:    hit.Doc.Key,
			Domain: hit.Doc.Domain,
			Score:  hit.Score,
			Label:  hit.Doc.Label,
			Text:   hit.Doc.Text,
		}
		switch hit.Doc.Domain {
		case search.DomainBible:
			book, _ := bible.BookByID(hit.Doc.BookID)
			view.TranslationID = hit.Doc.TranslationID
			view.BookID = hit.Doc.BookID
			view.Chapter = hit.Doc.Chapter
			view.Verse = hit.Doc.Verse
			view.Reference = hit.Doc.Label
			view.Testament = hit.Doc.Testament
			view.Label = book.Name
		case search.DomainSong:
			a.fillSongHit(ctx, &view, hit.Doc)
		}
		views = append(views, view)
	}
	return views, nil
}

// fillSongHit reads the live library so a hit shows the stored lyrics and
// attribution, not the embedded text. A song deleted since the last build
// falls back to the indexed snapshot until a rebuild prunes it.
func (a *App) fillSongHit(ctx context.Context, view *SemanticHitView, doc search.Doc) {
	view.SongID = doc.SongID
	view.SectionID = doc.SectionID
	view.SongTitle = doc.Label
	if doc.SectionID != "" {
		song, section, err := a.songs.Section(ctx, doc.SongID, doc.SectionID)
		if err == nil {
			view.SongTitle = song.Title
			view.Label = section.Label
			view.Text = section.Text
			view.Position = section.Position
			view.Author = song.Author
			view.Copyright = song.Copyright
			view.CCLINumber = song.CCLINumber
			view.Source = song.Source
			return
		}
	} else if song, err := a.songs.Get(ctx, doc.SongID); err == nil {
		view.SongTitle = song.Title
		view.Text = song.Title
		view.Author = song.Author
		view.Copyright = song.Copyright
		view.CCLINumber = song.CCLINumber
		view.Source = song.Source
		return
	}
	view.Text = doc.Text
}

// searchBibleMeaning answers the Bible workspace's meaning mode through the
// same response shape as text search, so preview and send-live work unchanged.
func (a *App) searchBibleMeaning(ctx context.Context, translationID, query string, request BibleSearchRequest) (BibleSearchResponse, error) {
	if a.semantic == nil {
		return BibleSearchResponse{}, errStorageUnavailable
	}
	hits, err := a.semantic.Search(ctx, search.Request{
		Query:         query,
		Domain:        search.DomainBible,
		TranslationID: translationID,
		Testament:     request.Testament,
		BookIDs:       request.BookIDs,
		Limit:         request.Limit,
		Offset:        request.Offset,
	})
	if err != nil {
		return BibleSearchResponse{}, err
	}
	views := make([]BibleVerse, 0, len(hits))
	for _, hit := range hits {
		book, _ := bible.BookByID(hit.Doc.BookID)
		views = append(views, BibleVerse{
			BookID:     hit.Doc.BookID,
			Book:       book.Name,
			Chapter:    hit.Doc.Chapter,
			Verse:      hit.Doc.Verse,
			Reference:  hit.Doc.Label,
			Text:       hit.Doc.Text,
			Testament:  hit.Doc.Testament,
			Similarity: hit.Score,
		})
	}
	return BibleSearchResponse{Kind: "semantic", Results: views, Total: len(views)}, nil
}

// SemanticComponentView is one fetched component and its license.
type SemanticComponentView struct {
	Name     string `json:"name"`
	License  string `json:"license"`
	Homepage string `json:"homepage"`
}

type SemanticAttributionView struct {
	Components  []SemanticComponentView `json:"components"`
	NoticesPath string                  `json:"noticesPath"`
}

func (a *App) GetSemanticAttribution() SemanticAttributionView {
	components := search.Components()
	views := make([]SemanticComponentView, 0, len(components))
	for _, c := range components {
		views = append(views, SemanticComponentView{Name: c.Name, License: c.License, Homepage: c.Homepage})
	}

	layout := a.semanticLayout()
	noticesPath := filepath.Join(layout.BundledRoot, search.NoticesFile)
	if _, err := os.Stat(noticesPath); err != nil {
		noticesPath = filepath.Join(layout.DownloadRoot, search.NoticesFile)
		if _, err := os.Stat(noticesPath); err != nil {
			noticesPath = ""
		}
	}
	return SemanticAttributionView{Components: views, NoticesPath: noticesPath}
}

// semanticLayout shares the speech engine roots: one engines tree holds both
// kinds of sidecar, each under its own subdirectory.
func (a *App) semanticLayout() search.Layout {
	layout := search.Layout{}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		layout.BundledRoot = filepath.Join(dir, "engines")
		if _, err := os.Stat(layout.BundledRoot); err != nil {
			lib := filepath.Join(dir, "..", "lib", "zopresent", "engines")
			if info, err := os.Stat(lib); err == nil && info.IsDir() {
				layout.BundledRoot = lib
			}
		}
	}
	if paths, err := a.paths(); err == nil {
		layout.DownloadRoot = filepath.Join(paths.Root, "engines")
	}
	return layout
}
