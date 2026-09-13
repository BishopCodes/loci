// Package config loads loci configuration from TOML, environment and defaults.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Browser modes for the escalation layer.
const (
	BrowserAuto  = "auto"  // HTTP first, escalate to headless browser on bot walls
	BrowserForce = "force" // always use headless browser
	BrowserOff   = "off"   // never use a browser
	BrowserSolve = "solve" // like auto, but relaunch headed so a human can solve captchas
)

// Embedding backends.
const (
	EmbedNone         = "none"
	EmbedOllama       = "ollama"
	EmbedOpenAICompat = "openai-compat"
)

// HTTP holds fetching policy.
type HTTP struct {
	UserAgent        string  `toml:"user_agent"`
	MaxBytes         int64   `toml:"max_bytes"`
	TimeoutSeconds   int     `toml:"timeout_seconds"`
	MaxRPS           float64 `toml:"max_rps"`
	IgnoreRobots     bool    `toml:"ignore_robots"`
	AllowPrivateHost bool    `toml:"allow_private_host"` // testing escape hatch only
}

// Browser holds playwright policy.
type Browser struct {
	Mode         string `toml:"mode"`          // auto | force | off | solve
	SolveTimeout int    `toml:"solve_timeout"` // seconds to wait for a human in solve mode
	BlockAssets  *bool  `toml:"block_assets"`  // abort script/css/image routes (default true)
}

// Embed holds embedding policy.
type Embed struct {
	Backend string `toml:"backend"` // none | ollama | openai-compat
	Model   string `toml:"model"`
	URL     string `toml:"url"`
	APIKey  string `toml:"api_key"`
}

// Search holds provider chain configuration.
type Search struct {
	ProviderOrder []string `toml:"provider_order"` // ddg-html, ddg-lite, mojeek, searxng, ddg-browser, bing-browser
	SearxURLs     []string `toml:"searx_urls"`
	MaxResults    int      `toml:"max_results"`
}

// Index holds chunking/indexing knobs.
type Index struct {
	ChunkRunes   int `toml:"chunk_runes"`
	ChunkOverlap int `toml:"chunk_overlap"`
}

// Config is the whole application configuration.
type Config struct {
	DataDir   string  `toml:"data_dir"`
	HTTP      HTTP    `toml:"http"`
	Browser   Browser `toml:"browser"`
	Embed     Embed   `toml:"embed"`
	Search    Search  `toml:"search"`
	Index     Index   `toml:"index"`
	AutoIndex bool    `toml:"auto_index_on_fetch"` // MCP web_fetch also stores content
}

// Default returns configuration with sane defaults applied.
func Default() *Config {
	block := true
	return &Config{
		DataDir: DefaultDataDir(),
		HTTP: HTTP{
			UserAgent:      "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/139.0.0.0 Safari/537.36",
			MaxBytes:       10 << 20,
			TimeoutSeconds: 15,
			MaxRPS:         0.5,
			IgnoreRobots:   false,
		},
		Browser: Browser{Mode: BrowserAuto, SolveTimeout: 120, BlockAssets: &block},
		Embed:   Embed{Backend: EmbedNone, Model: "nomic-embed-text", URL: "http://127.0.0.1:11434"},
		Search: Search{
			ProviderOrder: []string{"ddg-html", "ddg-lite", "mojeek", "searxng", "ddg-browser", "bing-browser"},
			SearxURLs:     nil,
			MaxResults:    10,
		},
		Index: Index{ChunkRunes: 3200, ChunkOverlap: 400},
	}
}

// DefaultDataDir follows XDG.
func DefaultDataDir() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "loci")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".loci-data"
	}
	return filepath.Join(home, ".local", "share", "loci")
}

// DefaultConfigDir returns the directory holding config.toml.
func DefaultConfigDir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "loci")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".loci"
	}
	return filepath.Join(home, ".config", "loci")
}

// DefaultConfigPath is the config.toml location.
func DefaultConfigPath() string { return filepath.Join(DefaultConfigDir(), "config.toml") }

// ProfileDir is the persistent chromium profile used for captcha persistence.
func (c *Config) ProfileDir() string { return filepath.Join(c.DataDir, "profile") }

// DBPath is the sqlite file location.
func (c *Config) DBPath() string { return filepath.Join(c.DataDir, "loci.db") }

// Load resolves configuration: defaults, then optional TOML file, then env overrides.
func Load(path string) (*Config, error) {
	cfg := Default()
	if path == "" {
		path = DefaultConfigPath()
	}
	if _, err := os.Stat(path); err == nil {
		if _, err := toml.DecodeFile(path, cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	cfg.applyEnv()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate checks consistency and normalizes enums.
func (c *Config) Validate() error {
	switch strings.ToLower(c.Browser.Mode) {
	case BrowserAuto, BrowserForce, BrowserOff, BrowserSolve:
		c.Browser.Mode = strings.ToLower(c.Browser.Mode)
	default:
		return fmt.Errorf("browser.mode must be auto|force|off|solve, got %q", c.Browser.Mode)
	}
	switch strings.ToLower(c.Embed.Backend) {
	case EmbedNone:
		c.Embed.Backend = EmbedNone
	case EmbedOllama, EmbedOpenAICompat:
		c.Embed.Backend = strings.ToLower(c.Embed.Backend)
	default:
		return fmt.Errorf("embed.backend must be none|ollama|openai-compat, got %q", c.Embed.Backend)
	}
	if c.Embed.Backend != EmbedNone && c.Embed.Model == "" {
		return errors.New("embed.model required when an embedding backend is configured")
	}
	if c.HTTP.MaxBytes <= 0 {
		c.HTTP.MaxBytes = 10 << 20
	}
	if c.Index.ChunkRunes <= 0 {
		c.Index.ChunkRunes = 3200
	}
	if c.Index.ChunkOverlap >= c.Index.ChunkRunes {
		return fmt.Errorf("index.chunk_overlap (%d) must be smaller than chunk_runes (%d)", c.Index.ChunkOverlap, c.Index.ChunkRunes)
	}
	return nil
}

func (c *Config) applyEnv() {
	if v := os.Getenv("LOCI_DATA_DIR"); v != "" {
		c.DataDir = v
	}
	if v := os.Getenv("LOCI_BROWSER"); v != "" {
		c.Browser.Mode = v
	}
	if v := os.Getenv("LOCI_IGNORE_ROBOTS"); v != "" {
		c.HTTP.IgnoreRobots = truthy(v)
	}
	if v := os.Getenv("LOCI_EMBED_BACKEND"); v != "" {
		c.Embed.Backend = v
	}
	if v := os.Getenv("LOCI_EMBED_MODEL"); v != "" {
		c.Embed.Model = v
	}
	if v := os.Getenv("LOCI_EMBED_URL"); v != "" {
		c.Embed.URL = v
	}
	if v := os.Getenv("LOCI_SEARX_URLS"); v != "" {
		c.Search.SearxURLs = splitComma(v)
	}
}

func truthy(v string) bool {
	b, _ := strconv.ParseBool(strings.ToLower(v))
	return b
}

func splitComma(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
