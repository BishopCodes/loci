package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

func docFromHTML(body string) (*goquery.Document, error) {
	return goquery.NewDocumentFromReader(strings.NewReader(body))
}

// parseDDGHTML parses html.duckduckgo.com/html SERPs. Result links are wrapped
// through /l/?uddg=<encoded>; unwrap them.
func parseDDGHTML(body string) []Result {
	d, err := docFromHTML(body)
	if err != nil {
		return nil
	}
	var out []Result
	d.Find("div.result, li.result").Each(func(_ int, res *goquery.Selection) {
		a := res.Find("a.result__a").First()
		if a.Length() == 0 {
			return
		}
		href, _ := a.Attr("href")
		href = unwrapDDG(href)
		title := strings.TrimSpace(a.Text())
		snippet := strings.TrimSpace(res.Find(".result__snippet").First().Text())
		if href == "" || title == "" {
			return
		}
		if snippet == title {
			snippet = ""
		} else if strings.HasPrefix(snippet, title) {
			snippet = strings.TrimSpace(snippet[len(title):])
		}
		out = append(out, Result{Title: title, URL: href, Snippet: snippet})
	})
	if len(out) == 0 {
		// some responses use the lite markup under /html
		out = parseDDGLite(body)
	}
	return out
}

func unwrapDDG(href string) string {
	if u, err := url.Parse(href); err == nil {
		if v := u.Query().Get("uddg"); v != "" {
			return v
		}
	}
	return href
}

// parseDDGLite parses lite.duckduckgo.com.
func parseDDGLite(body string) []Result {
	d, err := docFromHTML(body)
	if err != nil {
		return nil
	}
	var out []Result
	d.Find("a.result-link").Each(func(_ int, s *goquery.Selection) {
		href, _ := s.Attr("href")
		href = unwrapDDG(href)
		title := strings.TrimSpace(s.Text())
		if href == "" || title == "" {
			return
		}
		out = append(out, Result{Title: title, URL: href})
	})
	return out
}

// parseMojeek parses mojeek.com SERPs.
func parseMojeek(body string) []Result {
	d, err := docFromHTML(body)
	if err != nil {
		return nil
	}
	var out []Result
	d.Find("ul.results-list li").Each(func(_ int, li *goquery.Selection) {
		a := li.Find("a.ob").First()
		if a.Length() == 0 {
			a = li.Find("a[href]").First()
		}
		href, _ := a.Attr("href")
		title := strings.TrimSpace(a.Text())
		snippet := strings.TrimSpace(li.Find("p.s").First().Text())
		if href == "" || title == "" {
			return
		}
		out = append(out, Result{Title: title, URL: href, Snippet: snippet})
	})
	return out
}

// parseBing parses www.bing.com SERPs (browser path).
func parseBing(body string) []Result {
	d, err := docFromHTML(body)
	if err != nil {
		return nil
	}
	var out []Result
	d.Find("li.b_algo").Each(func(_ int, li *goquery.Selection) {
		a := li.Find("h2 a").First()
		href, _ := a.Attr("href")
		title := strings.TrimSpace(a.Text())
		snippet := strings.TrimSpace(li.Find("div.b_caption p, p").First().Text())
		if href == "" || title == "" || strings.HasPrefix(href, "http://bing.com") {
			return
		}
		out = append(out, Result{Title: title, URL: href, Snippet: snippet})
	})
	return out
}

// ---------------------------------------------------------------------------
// searxng JSON provider
// ---------------------------------------------------------------------------

type searxProvider struct {
	name   string
	base   string
	client *http.Client
}

func (p *searxProvider) Name() string { return p.name }

type searxResp struct {
	Results []struct {
		Title   string   `json:"title"`
		URL     string   `json:"url"`
		Content string   `json:"content"`
		Engines []string `json:"engines"`
	} `json:"results"`
	Error string `json:"error"`
}

func (p *searxProvider) Search(ctx context.Context, query string, n int) ([]Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		p.base+"/search?format=json&q="+url.QueryEscape(query), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("json api disabled on this instance (http %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	var sr searxResp
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&sr); err != nil {
		return nil, fmt.Errorf("not a searxng json response: %w", err)
	}
	if sr.Error != "" {
		return nil, fmt.Errorf("searxng: %s", sr.Error)
	}
	var out []Result
	for _, r := range sr.Results {
		if r.URL == "" {
			continue
		}
		out = append(out, Result{Title: r.Title, URL: r.URL, Snippet: r.Content, Provider: p.name})
		if len(out) >= n {
			break
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------

func normalizeURL(u string) string {
	parsed, err := url.Parse(u)
	if err != nil {
		return strings.ToLower(u)
	}
	host := strings.TrimPrefix(strings.ToLower(parsed.Host), "www.")
	path := strings.TrimRight(parsed.Path, "/")
	q := ""
	if parsed.RawQuery != "" {
		q = "?" + parsed.RawQuery
	}
	return host + path + q
}

func urlEncode(s string) string {
	return url.QueryEscape(s)
}
