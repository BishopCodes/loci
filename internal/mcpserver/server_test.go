package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"dsearch/internal/config"
	"dsearch/internal/search"
	"dsearch/internal/service"
)

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	content := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, `<html><head><title>Test page</title></head><body><h1>Heading</h1>
		<p>`+`Some article body text repeated to be long enough for extraction. `+`</p></body></html>`)
	}))
	t.Cleanup(content.Close)

	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.HTTP.AllowPrivateHost = true
	cfg.HTTP.IgnoreRobots = true
	cfg.HTTP.MaxRPS = 0
	cfg.Browser.Mode = config.BrowserOff
	cfg.Search.ProviderOrder = nil // hermetic: no live providers in tests

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc, err := service.New(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	svc.Engine = search.NewEngine(cfg, log)
	t.Cleanup(func() { svc.Close() })
	return New(svc), content
}

func connect(t *testing.T, srv *Server) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() { done <- srv.MCP().Run(ctx, st) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func TestToolsListed(t *testing.T) {
	srv, _ := newTestServer(t)
	cs := connect(t, srv)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"web_search": false, "web_fetch": false, "web_index": false, "web_query": false, "web_status": false}
	for _, tl := range res.Tools {
		if _, ok := want[tl.Name]; ok {
			want[tl.Name] = true
		}
	}
	for k, v := range want {
		if !v {
			t.Errorf("tool missing from tools/list: %s", k)
		}
	}
}

func TestWebFetchEnvelopesContent(t *testing.T) {
	srv, content := newTestServer(t)
	cs := connect(t, srv)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "web_fetch",
		Arguments: map[string]any{"url": content.URL + "/p", "index": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("tool error: %v", res.Content)
	}
	// structured content carries provenance + envelope
	b, _ := json.Marshal(res.StructuredContent)
	var out struct {
		Content   string `json:"content"`
		Suspicion string `json:"suspicion"`
		FinalURL  string `json:"final_url"`
		SHA256    string `json:"sha256"`
		Indexed   bool   `json:"indexed"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode structured: %v: %s", err, b)
	}
	if out.FinalURL == "" || out.SHA256 == "" || !out.Indexed {
		t.Fatalf("bad structured out: %+v", out)
	}
	if !contains(out.Content, "<untrusted-") {
		t.Fatalf("content not envelope-wrapped: %.120s", out.Content)
	}
	if out.Suspicion == "" {
		t.Fatal("missing suspicion annotation")
	}
	// web_query should now retrieve the stored page
	q, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "web_query",
		Arguments: map[string]any{"query": "article body text"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if q.IsError {
		t.Fatalf("query error: %v", q.Content)
	}
	qb, _ := json.Marshal(q.StructuredContent)
	if !contains(string(qb), "article body text") {
		t.Fatalf("query did not retrieve indexed content: %s", qb)
	}
}

func TestWebSearchHermeticError(t *testing.T) {
	srv, _ := newTestServer(t)
	cs := connect(t, srv)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "web_search",
		Arguments: map[string]any{"query": "anything"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("expected provider error with empty chain")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
