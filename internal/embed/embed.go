// Package embed provides pluggable local embedding backends.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"loci/internal/config"
)

// Embedder produces vectors for text batches.
type Embedder interface {
	Model() string
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// Probe describes capability for `loci doctor`.
type Probe struct {
	Backend string
	Model   string
	OK      bool
	Detail  string
}

// New builds the configured embedder; returns nil when backend is none.
func New(cfg *config.Config) (Embedder, error) {
	switch cfg.Embed.Backend {
	case config.EmbedNone:
		return nil, nil
	case config.EmbedOllama:
		return &ollama{url: strings.TrimRight(cfg.Embed.URL, "/"), model: cfg.Embed.Model}, nil
	case config.EmbedOpenAICompat:
		return &openaiCompat{url: strings.TrimRight(cfg.Embed.URL, "/"), model: cfg.Embed.Model, key: cfg.Embed.APIKey}, nil
	}
	return nil, fmt.Errorf("unknown embed backend %q", cfg.Embed.Backend)
}

// ---------------------------------------------------------------------------

type ollama struct {
	url   string
	model string
}

func (o *ollama) Model() string { return o.model }

func (o *ollama) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	body, _ := json.Marshal(map[string]any{"model": o.model, "input": texts})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.url+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama unreachable at %s: %w", o.url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama embed: http %d: %s", resp.StatusCode, snippet(raw))
	}
	var out struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("ollama embed decode: %w", err)
	}
	if len(out.Embeddings) != len(texts) {
		return nil, fmt.Errorf("ollama embed: got %d vectors for %d inputs", len(out.Embeddings), len(texts))
	}
	return out.Embeddings, nil
}

// ollamaModels lists local models (doctor).
func OllamaModels(ctx context.Context, url string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(url, "/")+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	var names []string
	for _, m := range out.Models {
		names = append(names, m.Name)
	}
	return names, nil
}

// ---------------------------------------------------------------------------

type openaiCompat struct {
	url   string
	model string
	key   string
}

func (o *openaiCompat) Model() string { return o.model }

func (o *openaiCompat) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	body, _ := json.Marshal(map[string]any{"model": o.model, "input": texts})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.url+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.key != "" {
		req.Header.Set("Authorization", "Bearer "+o.key)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding endpoint unreachable at %s: %w", o.url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embeddings: http %d: %s", resp.StatusCode, snippet(raw))
	}
	var out struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("embeddings decode: %w", err)
	}
	vecs := make([][]float32, len(texts))
	for _, d := range out.Data {
		if d.Index >= 0 && d.Index < len(vecs) {
			vecs[d.Index] = d.Embedding
		}
	}
	for i, v := range vecs {
		if v == nil {
			return nil, fmt.Errorf("embeddings: missing vector for input %d", i)
		}
	}
	return vecs, nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// Batches splits texts into groups of at most n.
func Batches(texts []string, n int) [][]string {
	if n <= 0 {
		n = 32
	}
	var out [][]string
	for i := 0; i < len(texts); i += n {
		end := i + n
		if end > len(texts) {
			end = len(texts)
		}
		out = append(out, texts[i:end])
	}
	return out
}
