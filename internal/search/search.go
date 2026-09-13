// Package search implements key-free web search by scraping a chain of
// providers (HTTP first, browser escalation on bot walls).
package search

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"dsearch/internal/browser"
	"dsearch/internal/config"
	"dsearch/internal/fetch"
)

// Result is one normalized search hit.
type Result struct {
	Title    string `json:"title"`
	URL      string `json:"url"`
	Snippet  string `json:"snippet"`
	Provider string `json:"provider"`
	Rank     int    `json:"rank"`
}

// Provider searches the web for one query strategy.
type Provider interface {
	Name() string
	Search(ctx context.Context, query string, n int) ([]Result, error)
}

// ErrAllProvidersFailed wraps the last provider error.
var ErrAllProvidersFailed = errors.New("all search providers failed")

// Engine runs the configured provider chain.
type Engine struct {
	cfg       *config.Config
	httpProv  []Provider
	br        browser.Backend
	serpFetch *fetch.Fetcher
	log       *slog.Logger
}

// NewEngine assembles providers per config.Search.ProviderOrder.
func NewEngine(cfg *config.Config, log *slog.Logger) *Engine {
	e := &Engine{cfg: cfg, log: log}
	e.serpFetch = fetch.New(cfg, log)
	for _, name := range cfg.Search.ProviderOrder {
		switch name {
		case "ddg-html":
			e.httpProv = append(e.httpProv, &serpProvider{name: name, engine: e,
				url:   func(q string) string { return "https://html.duckduckgo.com/html/?q=" + q },
				parse: parseDDGHTML})
		case "ddg-lite":
			e.httpProv = append(e.httpProv, &serpProvider{name: name, engine: e,
				url:   func(q string) string { return "https://lite.duckduckgo.com/lite/?q=" + q },
				parse: parseDDGLite})
		case "mojeek":
			e.httpProv = append(e.httpProv, &serpProvider{name: name, engine: e,
				url:   func(q string) string { return "https://www.mojeek.com/search?q=" + q },
				parse: parseMojeek})
		case "searxng":
			for _, base := range cfg.Search.SearxURLs {
				b := strings.TrimRight(base, "/")
				e.httpProv = append(e.httpProv, &searxProvider{name: "searxng:" + hostOf(b), base: b, client: &http.Client{Timeout: 10 * time.Second}})
			}
		case "ddg-browser":
			e.httpProv = append(e.httpProv, &browserProvider{name: name, engine: e,
				url:   func(q string) string { return "https://html.duckduckgo.com/html/?q=" + q },
				parse: parseDDGHTML})
		case "bing-browser":
			e.httpProv = append(e.httpProv, &browserProvider{name: name, engine: e,
				url:   func(q string) string { return "https://www.bing.com/search?q=" + q },
				parse: parseBing})
		}
	}
	return e
}

// SetBrowser wires the browser backend (nil disables browser providers).
func (e *Engine) SetBrowser(b browser.Backend) { e.br = b }

// Search walks the chain; on ErrBotChallenge it proceeds to the next provider
// (browser providers are last in the order, giving HTTP-first escalation).
func (e *Engine) Search(ctx context.Context, query string, n int) ([]Result, string, error) {
	if n <= 0 {
		n = e.cfg.Search.MaxResults
	}
	var lastErr error
	for _, p := range e.httpProv {
		if _, ok := p.(*browserProvider); ok && (e.br == nil || !e.br.Available() ||
			e.cfg.Browser.Mode == config.BrowserOff) {
			lastErr = fmt.Errorf("%s: browser disabled/unavailable", p.Name())
			continue
		}
		results, err := p.Search(ctx, query, n)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", p.Name(), err)
			e.log.Debug("provider failed", "provider", p.Name(), "err", err)
			continue
		}
		if len(results) == 0 {
			lastErr = fmt.Errorf("%s: 0 results", p.Name())
			continue
		}
		return dedupe(results, n), p.Name(), nil
	}
	if lastErr == nil {
		lastErr = errors.New("no providers configured")
	}
	return nil, "", fmt.Errorf("%w: %w", ErrAllProvidersFailed, lastErr)
}

// ---------------------------------------------------------------------------

type serpProvider struct {
	name   string
	engine *Engine
	url    func(q string) string
	parse  func(body string) []Result
}

func (p *serpProvider) Name() string { return p.name }

func (p *serpProvider) Search(ctx context.Context, query string, n int) ([]Result, error) {
	doc, err := p.engine.serpFetch.Fetch(ctx, p.url(escapeQuery(query)))
	if err != nil {
		return nil, err
	}
	res := p.parse(string(doc.Body))
	for i := range res {
		res[i].Provider = p.name
	}
	return res, nil
}

type browserProvider struct {
	name   string
	engine *Engine
	url    func(q string) string
	parse  func(body string) []Result
}

func (p *browserProvider) Name() string { return p.name }

func (p *browserProvider) Search(ctx context.Context, query string, n int) ([]Result, error) {
	res, err := p.engine.br.Render(ctx, p.url(escapeQuery(query)), p.engine.cfg.Browser.Mode == config.BrowserSolve)
	if err != nil {
		return nil, err
	}
	if fetch.LooksLikeChallenge(res.HTML) {
		return nil, fmt.Errorf("%w (browser SERP page still challenged)", fetch.ErrBotChallenge)
	}
	out := p.parse(res.HTML)
	for i := range out {
		out[i].Provider = p.name
	}
	return out, nil
}

func escapeQuery(q string) string {
	return strings.ReplaceAll(urlEncode(q), " ", "+")
}

func hostOf(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	if i := strings.IndexByte(u, '/'); i >= 0 {
		u = u[:i]
	}
	return u
}

// dedupe keeps first occurrence per normalized URL.
func dedupe(results []Result, n int) []Result {
	seen := map[string]bool{}
	var out []Result
	for _, r := range results {
		key := normalizeURL(r.URL)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		r.Rank = len(out) + 1
		out = append(out, r)
		if len(out) >= n {
			break
		}
	}
	return out
}
