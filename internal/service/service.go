// Package service wires the subsystems into the operations the CLI and MCP
// server share.
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"

	"loci/internal/browser"
	"loci/internal/chunk"
	"loci/internal/config"
	"loci/internal/embed"
	"loci/internal/extract"
	"loci/internal/fetch"
	"loci/internal/sanitize"
	"loci/internal/search"
	"loci/internal/store"
)

// Service is the application facade.
type Service struct {
	Cfg      *config.Config
	Log      *slog.Logger
	Fetcher  *fetch.Fetcher
	Browser  browser.Backend
	Engine   *search.Engine
	Store    *store.DB
	Embedder embed.Embedder
}

// New builds the service from config.
func New(cfg *config.Config, log *slog.Logger) (*Service, error) {
	db, err := store.Open(cfg.DBPath())
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	emb, err := embed.New(cfg)
	if err != nil {
		db.Close()
		return nil, err
	}
	var br browser.Backend
	if cfg.Browser.Mode != config.BrowserOff {
		br = browser.NewPlaywright(cfg, log)
	}
	svc := &Service{
		Cfg: cfg, Log: log,
		Fetcher:  fetch.New(cfg, log),
		Browser:  br,
		Engine:   search.NewEngine(cfg, log),
		Store:    db,
		Embedder: emb,
	}
	svc.Engine.SetBrowser(br)
	return svc, nil
}

// Close releases resources.
func (s *Service) Close() error { return s.Store.Close() }

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

// SearchOutcome is the result of a web search with provenance and flags.
type SearchOutcome struct {
	Results    []search.Result `json:"results"`
	Provider   string          `json:"provider"`
	DegradedTo string          `json:"degraded_to,omitempty"`
	Suspicion  string          `json:"suspicion"`
	Signals    []string        `json:"signals,omitempty"`
}

// Search runs the provider chain and sanitizes snippets.
func (s *Service) Search(ctx context.Context, query string, n int) (*SearchOutcome, error) {
	results, provider, err := s.Engine.Search(ctx, query, n)
	if err != nil {
		return nil, err
	}
	worst := suspicionAccum{level: sanitize.SuspicionNone}
	for i := range results {
		t := sanitize.Text(results[i].Title)
		sn := sanitize.Text(results[i].Snippet)
		results[i].Title = sanitize.DefangURLs(cleanOneLine(t.Text))
		results[i].Snippet = sanitize.DefangURLs(cleanOneLine(sn.Text))
		// Score the joined text: some signals match across the title/snippet
		// boundary, and per-field scoring would under-report them.
		worst.add(sanitize.Detect(t.Text + "\n" + sn.Text))
	}
	return &SearchOutcome{Results: results, Provider: provider, Suspicion: worst.level, Signals: worst.signals}, nil
}

// ---------------------------------------------------------------------------
// Fetch (+optional index)
// ---------------------------------------------------------------------------

// FetchOutcome carries extracted content with its provenance.
type FetchOutcome struct {
	URL        string   `json:"url"`
	FinalURL   string   `json:"final_url"`
	Title      string   `json:"title,omitempty"`
	Content    string   `json:"content"`
	DocType    string   `json:"doc_type"`
	Extraction string   `json:"extraction"`
	FetchedAt  string   `json:"fetched_at"`
	SHA256     string   `json:"sha256"`
	Suspicion  string   `json:"suspicion"`
	Signals    []string `json:"signals,omitempty"`
	Warning    string   `json:"warning,omitempty"`
	Browser    bool     `json:"browser_used"`
	Indexed    bool     `json:"indexed"`
}

// FetchAndMaybeIndex fetches and extracts a URL; indexes when forced or
// configured auto-index.
func (s *Service) FetchAndMaybeIndex(ctx context.Context, rawurl string, index bool) (*FetchOutcome, error) {
	doc, err := s.Fetcher.FetchWithBrowser(ctx, rawurl, s.Browser)
	if err != nil {
		return nil, err
	}
	ex, err := extract.FromDoc(doc)
	if err != nil {
		return nil, err
	}
	res := sanitize.Text(ex.Markdown)
	out := &FetchOutcome{
		URL: doc.URL, FinalURL: doc.FinalURL, Title: ex.Title,
		Content: res.Text, DocType: doc.DocType, Extraction: ex.Extraction,
		FetchedAt: now(), SHA256: hashBytes(doc.Body), Suspicion: res.Suspicion,
		Signals: res.Signals, Warning: ex.Warning, Browser: doc.BrowserUsed,
	}
	if index || s.Cfg.AutoIndex {
		st, ierr := s.IndexDoc(ctx, doc, ex)
		if ierr == nil && st != nil {
			out.Indexed = true
		} else if ierr != nil {
			s.Log.Warn("auto-index failed", "url", rawurl, "err", ierr)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Index
// ---------------------------------------------------------------------------

// IndexOutcome reports incremental indexing state.
type IndexOutcome struct {
	URL     string `json:"url"`
	Status  string `json:"status"` // stored | unchanged | error
	Chunks  int    `json:"chunks"`
	DocID   int64  `json:"doc_id,omitempty"`
	Warning string `json:"warning,omitempty"`
}

// IndexURL fetches and indexes one URL.
func (s *Service) IndexURL(ctx context.Context, rawurl string) (*IndexOutcome, error) {
	doc, err := s.Fetcher.FetchWithBrowser(ctx, rawurl, s.Browser)
	if err != nil {
		return nil, err
	}
	ex, err := extract.FromDoc(doc)
	if err != nil {
		return nil, err
	}
	return s.IndexDoc(ctx, doc, ex)
}

// IndexDoc stores an already-extracted document; the content hash decides
// whether re-chunking/re-embedding is needed at all.
func (s *Service) IndexDoc(ctx context.Context, doc *fetch.Doc, ex *extract.Document) (*IndexOutcome, error) {
	clean := sanitize.Text(ex.Markdown)
	linksJSON, _ := json.Marshal(map[string]any{"links": ex.Links})
	docID, changed, err := s.Store.UpsertDoc(store.DocInput{
		URL: doc.FinalURL, FinalURL: doc.FinalURL, DocType: doc.DocType,
		SHA: hashBytes(doc.Body), Etag: doc.Etag, Title: ex.Title,
		Extraction: ex.Extraction, Warning: cleanAndJoin(clean.Signals, " | "), MetaJSON: string(linksJSON),
	})
	if err != nil {
		return nil, err
	}
	out := &IndexOutcome{URL: doc.FinalURL, Status: "unchanged", DocID: docID, Warning: ex.Warning}
	if !changed {
		return out, nil
	}
	out.Status = "stored"
	pieces := chunk.Split(clean.Text, s.Cfg.Index.ChunkRunes, s.Cfg.Index.ChunkOverlap)
	texts := make([]string, len(pieces))
	starts := make([]int, len(pieces))
	for i, p := range pieces {
		texts[i] = p.Text
		starts[i] = p.StartByte
	}
	if err := s.Store.ReplaceChunks(docID, texts, starts); err != nil {
		return nil, err
	}
	out.Chunks = len(pieces)
	if err := s.vectorize(ctx, docID); err != nil {
		return out, fmt.Errorf("indexed %d chunks but embedding failed: %w", len(pieces), err)
	}
	return out, nil
}

func (s *Service) vectorize(ctx context.Context, docID int64) error {
	if s.Embedder == nil {
		return nil
	}
	ids, texts, err := s.Store.DocChunks(docID)
	if err != nil || len(ids) == 0 {
		return err
	}
	vecs := map[int64][]float32{}
	for i := 0; i < len(texts); i += 32 {
		end := i + 32
		if end > len(texts) {
			end = len(texts)
		}
		batch, err := s.Embedder.Embed(ctx, texts[i:end])
		if err != nil {
			return err
		}
		for j, v := range batch {
			vecs[ids[i+j]] = v
		}
	}
	m, _, _, _ := s.Store.VectorMeta()
	if m != "" && m != s.Embedder.Model() {
		return fmt.Errorf("store has vectors from model %q, configured %q; run `loci reindex --vectors`", m, s.Embedder.Model())
	}
	return s.Store.SaveVectors(s.Embedder.Model(), vecs)
}

// ReindexVectors re-embeds all stored chunks without re-fetching.
func (s *Service) ReindexVectors(ctx context.Context) error {
	if s.Embedder == nil {
		return fmt.Errorf("no embedding backend configured (embed.backend=none)")
	}
	ids, texts, err := s.Store.AllChunkIDs()
	if err != nil {
		return err
	}
	vecs := map[int64][]float32{}
	for i := 0; i < len(texts); i += 32 {
		end := i + 32
		if end > len(texts) {
			end = len(texts)
		}
		batch, err := s.Embedder.Embed(ctx, texts[i:end])
		if err != nil {
			return err
		}
		for j, v := range batch {
			vecs[ids[i+j]] = v
		}
	}
	return s.Store.SaveVectors(s.Embedder.Model(), vecs)
}

// ---------------------------------------------------------------------------
// Query
// ---------------------------------------------------------------------------

// QueryOutcome wraps hybrid retrieval results.
type QueryOutcome struct {
	Chunks    []store.Hit `json:"chunks"`
	Mode      string      `json:"mode"` // hybrid | keyword-only
	Suspicion string      `json:"suspicion"`
	Signals   []string    `json:"signals,omitempty"`
}

// Query runs hybrid retrieval, wrapping each chunk in the untrusted envelope.
func (s *Service) Query(ctx context.Context, q string, k int) (*QueryOutcome, error) {
	if k <= 0 {
		k = 8
	}
	keywordIDs, err := s.Store.KeywordSearch(ctx, q, 50)
	if err != nil {
		return nil, err
	}
	mode := "keyword-only"
	var vectorIDs []int64
	if s.Embedder != nil {
		vecs, err := s.Embedder.Embed(ctx, []string{q})
		if err != nil {
			return nil, fmt.Errorf("query embedding failed (set embed.backend = \"none\" to stay keyword-only): %w", err)
		}
		vectorIDs, err = s.Store.VectorSearch(ctx, vecs[0], 50)
		if err != nil {
			return nil, err
		}
		mode = "hybrid"
	}
	hits := store.Hybrid(keywordIDs, vectorIDs, 60, k)
	full, err := s.Store.Enrich(ids(hits))
	if err != nil {
		return nil, err
	}
	byID := map[int64]store.Hit{}
	for _, h := range full {
		byID[h.ChunkID] = h
	}
	worst := suspicionAccum{level: sanitize.SuspicionNone}
	for i := range hits {
		src := byID[hits[i].ChunkID]
		hits[i].Text = sanitize.Envelope(sanitize.CleanUnicode(src.Text), src.URL)
		hits[i].URL, hits[i].Title, hits[i].DocType = src.URL, src.Title, src.DocType
		hits[i].FetchedAt, hits[i].SHA = src.FetchedAt, src.SHA
		worst.add(sanitize.Detect(src.Text))
	}
	return &QueryOutcome{Chunks: hits, Mode: mode, Suspicion: worst.level, Signals: worst.signals}, nil
}

func ids(hits []store.Hit) []int64 {
	out := make([]int64, len(hits))
	for i, h := range hits {
		out[i] = h.ChunkID
	}
	return out
}

// ---------------------------------------------------------------------------
// Crawl
// ---------------------------------------------------------------------------

// CrawlOutcome summarizes a crawl.
type CrawlOutcome struct {
	Fetched   int      `json:"fetched"`
	Unchanged int      `json:"unchanged"`
	Failed    []string `json:"failed,omitempty"`
}

// Crawl breadth-first explores links from a seed URL, same registrable domain,
// stopping after maxPages.
func (s *Service) Crawl(ctx context.Context, seed string, maxPages int, sameDomain bool) (*CrawlOutcome, error) {
	if maxPages <= 0 {
		maxPages = 25
	}
	seedHost, err := domainOf(seed)
	if err != nil {
		return nil, err
	}
	visited := map[string]bool{}
	queue := []string{seed}
	out := &CrawlOutcome{}
	for len(queue) > 0 && out.Fetched+out.Unchanged < maxPages {
		u := queue[0]
		queue = queue[1:]
		if visited[u] {
			continue
		}
		visited[u] = true
		doc, err := s.Fetcher.FetchWithBrowser(ctx, u, s.Browser)
		if err != nil {
			out.Failed = append(out.Failed, fmt.Sprintf("%s: %v", u, err))
			continue
		}
		ex, err := extract.FromDoc(doc)
		if err != nil {
			out.Failed = append(out.Failed, fmt.Sprintf("%s: %v", u, err))
			continue
		}
		res, err := s.IndexDoc(ctx, doc, ex)
		if err != nil {
			out.Failed = append(out.Failed, fmt.Sprintf("%s: %v", u, err))
			continue
		}
		if res.Status == "stored" {
			out.Fetched++
		} else {
			out.Unchanged++
		}
		// queue links breadth-first, deterministic order
		links := append([]string{}, ex.Links...)
		sort.Strings(links)
		for _, l := range links {
			l = strings.SplitN(l, "#", 2)[0]
			if visited[l] {
				continue
			}
			host, err := domainOf(l)
			if err != nil {
				continue
			}
			if sameDomain && host != seedHost {
				continue
			}
			queue = append(queue, l)
		}
	}
	return out, nil
}

func domainOf(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	et, _ := publicsuffix.EffectiveTLDPlusOne(u.Hostname())
	return strings.ToLower(et), nil
}

// ---------------------------------------------------------------------------
// Stats
// ---------------------------------------------------------------------------

// Stats is machine-readable application state.
type Stats struct {
	DataDir      string `json:"data_dir"`
	DBPath       string `json:"db_path"`
	Docs         int    `json:"docs"`
	Chunks       int    `json:"chunks"`
	Vectors      int    `json:"vectors"`
	VectorModel  string `json:"vector_model,omitempty"`
	EmbedBackend string `json:"embed_backend"`
	EmbedModel   string `json:"embed_model,omitempty"`
	BrowserMode  string `json:"browser_mode"`
}

// Stats gathers counters.
func (s *Service) Stats() (*Stats, error) {
	docs, chunks, vectors, err := s.Store.Counts()
	if err != nil {
		return nil, err
	}
	model, _, _, _ := s.Store.VectorMeta()
	st := &Stats{
		DataDir: s.Cfg.DataDir, DBPath: s.Cfg.DBPath(),
		Docs: docs, Chunks: chunks, Vectors: vectors, VectorModel: model,
		EmbedBackend: s.Cfg.Embed.Backend, BrowserMode: s.Cfg.Browser.Mode,
	}
	if s.Embedder != nil {
		st.EmbedModel = s.Embedder.Model()
	}
	return st, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// suspicionAccum folds many detections into the one level to report, plus the
// signal names that earned it: a higher level replaces the list, an equal level
// merges into it. Without the list, WarnSuspicion prints "(high): ." and the
// reader loses which rules fired.
type suspicionAccum struct {
	level   string
	signals []string
}

func (a *suspicionAccum) add(level string, hits []string) {
	if rank(level) > rank(a.level) {
		a.level, a.signals = level, nil
	}
	if rank(level) != rank(a.level) {
		return
	}
	for _, h := range hits {
		if !slices.Contains(a.signals, h) {
			a.signals = append(a.signals, h)
		}
	}
}

func rank(level string) int {
	switch level {
	case sanitize.SuspicionHigh:
		return 3
	case sanitize.SuspicionMedium:
		return 2
	case sanitize.SuspicionLow:
		return 1
	}
	return 0
}

func cleanOneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func cleanAndJoin(parts []string, sep string) string { return strings.Join(parts, sep) }
