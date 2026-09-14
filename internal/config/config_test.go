package config

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// clearEnv pins every input the loader reads, so neither the developer's shell nor
// the real home directory can leak into these tests.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"LOCI_DATA_DIR", "LOCI_BROWSER", "LOCI_IGNORE_ROBOTS",
		"LOCI_EMBED_BACKEND", "LOCI_EMBED_MODEL", "LOCI_EMBED_URL", "LOCI_SEARX_URLS",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDefaultsFollowXDG(t *testing.T) {
	clearEnv(t)
	want := filepath.Join(os.Getenv("XDG_DATA_HOME"), "loci")
	if got := DefaultDataDir(); got != want {
		t.Fatalf("DefaultDataDir() = %q, want %q", got, want)
	}
	if got, want := Default().DBPath(), filepath.Join(want, "loci.db"); got != want {
		t.Fatalf("DBPath() = %q, want %q", got, want)
	}
}

// An empty data_dir in config.toml used to leave DataDir empty, which made the
// sqlite path relative and dropped the store in the process CWD.
func TestEmptyDataDirFallsBackToDefault(t *testing.T) {
	clearEnv(t)
	cfg, err := Load(writeConfig(t, "data_dir = \"\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(cfg.DataDir) {
		t.Fatalf("DataDir = %q, want an absolute path", cfg.DataDir)
	}
	if cfg.DataDir != DefaultDataDir() {
		t.Fatalf("DataDir = %q, want default %q", cfg.DataDir, DefaultDataDir())
	}
	if want := filepath.Join(DefaultDataDir(), "loci.db"); cfg.DBPath() != want {
		t.Fatalf("DBPath() = %q, want %q", cfg.DBPath(), want)
	}
}

// The shipped example config is the real historical trigger: it carries an explicit
// `data_dir = ""`. Loading it must still resolve to the default store location.
func TestShippedExampleConfigKeepsDefaultStore(t *testing.T) {
	clearEnv(t)
	const example = "../../config.example.toml"
	if _, err := os.Stat(example); err != nil {
		t.Fatalf("shipped example config missing: %v", err)
	}
	cfg, err := Load(example)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != DefaultDataDir() {
		t.Fatalf("DataDir = %q, want default %q", cfg.DataDir, DefaultDataDir())
	}
}

func TestPrecedenceFileThenEnv(t *testing.T) {
	clearEnv(t)
	fileDir := filepath.Join(t.TempDir(), "from-file")
	path := writeConfig(t, "data_dir = "+strconv.Quote(fileDir)+"\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != fileDir {
		t.Fatalf("DataDir = %q, want the file value %q", cfg.DataDir, fileDir)
	}

	envDir := filepath.Join(t.TempDir(), "from-env")
	t.Setenv("LOCI_DATA_DIR", envDir)
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != envDir {
		t.Fatalf("DataDir = %q, want the env value %q", cfg.DataDir, envDir)
	}
}

func TestEnvOverrides(t *testing.T) {
	clearEnv(t)
	dataDir := filepath.Join(t.TempDir(), "from-env")
	t.Setenv("LOCI_DATA_DIR", dataDir)
	t.Setenv("LOCI_BROWSER", "solve")
	t.Setenv("LOCI_IGNORE_ROBOTS", "true")
	t.Setenv("LOCI_EMBED_BACKEND", "ollama")
	t.Setenv("LOCI_EMBED_MODEL", "bge-m3")
	t.Setenv("LOCI_EMBED_URL", "http://127.0.0.1:9999")
	t.Setenv("LOCI_SEARX_URLS", "https://a.example, https://b.example")

	cfg, err := Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case cfg.DataDir != dataDir:
		t.Errorf("LOCI_DATA_DIR: DataDir = %q, want %q", cfg.DataDir, dataDir)
	case cfg.Browser.Mode != BrowserSolve:
		t.Errorf("LOCI_BROWSER: Mode = %q, want %q", cfg.Browser.Mode, BrowserSolve)
	case !cfg.HTTP.IgnoreRobots:
		t.Error("LOCI_IGNORE_ROBOTS: IgnoreRobots = false, want true")
	case cfg.Embed.Backend != EmbedOllama:
		t.Errorf("LOCI_EMBED_BACKEND: Backend = %q, want %q", cfg.Embed.Backend, EmbedOllama)
	case cfg.Embed.Model != "bge-m3":
		t.Errorf("LOCI_EMBED_MODEL: Model = %q", cfg.Embed.Model)
	case cfg.Embed.URL != "http://127.0.0.1:9999":
		t.Errorf("LOCI_EMBED_URL: URL = %q", cfg.Embed.URL)
	case len(cfg.Search.SearxURLs) != 2 || cfg.Search.SearxURLs[1] != "https://b.example":
		t.Errorf("LOCI_SEARX_URLS: SearxURLs = %#v, want 2 trimmed entries", cfg.Search.SearxURLs)
	}
}
