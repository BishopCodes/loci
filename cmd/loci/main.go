// loci: key-free web search, fetch and local hybrid search — CLI + MCP server.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"loci/internal/browser"
	"loci/internal/config"
	"loci/internal/embed"
	"loci/internal/mcpserver"
	"loci/internal/sanitize"
	"loci/internal/service"
)

var (
	flagConfig  string
	flagJSON    bool
	flagVerbose bool
	flagBrowser string
)

func main() {
	root := &cobra.Command{
		Use:   "loci",
		Short: "Key-free web search, fetch and local hybrid retrieval",
		Long: "loci searches the web without API keys (scraped providers with browser escalation),\n" +
			"extracts clean text/Markdown from pages and PDFs, indexes it locally with\n" +
			"BM25+vector hybrid search, and serves it over MCP. All fetched content is\n" +
			"treated as untrusted data: envelope-wrapped and injection-scanned.",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			initLogger()
		},
	}
	root.PersistentFlags().StringVar(&flagConfig, "config", "", "config file (default $XDG_CONFIG_HOME/loci/config.toml)")
	root.PersistentFlags().BoolVar(&flagJSON, "json", false, "machine-readable JSON output")
	root.PersistentFlags().BoolVarP(&flagVerbose, "verbose", "v", false, "verbose logging")
	root.PersistentFlags().StringVar(&flagBrowser, "browser", "", "override browser mode: auto|force|off|solve")

	root.AddCommand(
		searchCmd(), fetchCmd(), indexCmd(), crawlCmd(), queryCmd(),
		statsCmd(), reindexCmd(), doctorCmd(), serveCmd(), browserCmd(), configCmd(),
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func initLogger() {
	lvl := slog.LevelInfo
	if flagVerbose {
		lvl = slog.LevelDebug
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	slog.SetDefault(slog.New(h))
}

func loadConfig() (*config.Config, error) {
	cfg, err := config.Load(flagConfig)
	if err != nil {
		return nil, err
	}
	if flagBrowser != "" {
		cfg.Browser.Mode = flagBrowser
		if err := cfg.Validate(); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

func newService() (*service.Service, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	return service.New(cfg, slog.Default())
}

func emit(v any, text func(io.Writer)) {
	if flagJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(v)
		return
	}
	if text != nil {
		text(os.Stdout)
	}
}

// ---------------------------------------------------------------------------

func searchCmd() *cobra.Command {
	var n int
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search the web via scraped providers",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := newService()
			if err != nil {
				return err
			}
			defer svc.Close()
			res, err := svc.Search(cmd.Context(), strings.Join(args, " "), n)
			if err != nil {
				return err
			}
			emit(res, func(w io.Writer) {
				fmt.Fprintf(w, "provider: %s\n", res.Provider)
				for _, r := range res.Results {
					fmt.Fprintf(w, "%2d. %s\n    %s\n", r.Rank, r.Title, r.URL)
					if r.Snippet != "" {
						fmt.Fprintf(w, "    %s\n", r.Snippet)
					}
				}
				if w2 := sanitize.WarnSuspicion(res.Suspicion, nil); w2 != "" {
					fmt.Fprint(w, "\n"+w2)
				}
			})
			return nil
		},
	}
	cmd.Flags().IntVarP(&n, "num", "n", 0, "number of results")
	return cmd
}

// browserMode maps a --browser flag value to a config browser mode. The bool
// spellings are exactly the ones strconv.ParseBool accepts, so values written for
// the earlier bool flag keep working: truthy forces the browser, falsy means no
// override. Anything else is taken as a mode and validated downstream.
func browserMode(v string) string {
	switch v {
	case "", "false", "False", "FALSE", "f", "F", "0":
		return ""
	case "true", "True", "TRUE", "t", "T", "1":
		return config.BrowserForce
	}
	return v
}

func fetchCmd() *cobra.Command {
	var save bool
	var browser string
	cmd := &cobra.Command{
		Use:   "fetch <url>",
		Short: "Fetch and extract a URL as Markdown (or raw text)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if mode := browserMode(browser); mode != "" {
				cfg.Browser.Mode = mode
				if err := cfg.Validate(); err != nil {
					return err
				}
			}
			svc, err := service.New(cfg, slog.Default())
			if err != nil {
				return err
			}
			defer svc.Close()
			res, err := svc.FetchAndMaybeIndex(cmd.Context(), args[0], save)
			if err != nil {
				return err
			}
			emit(res, func(w io.Writer) {
				if w2 := sanitize.WarnSuspicion(res.Suspicion, res.Signals); w2 != "" {
					fmt.Fprint(w, w2)
				}
				if res.Warning != "" {
					fmt.Fprintf(w, "warning: %s\n", res.Warning)
				}
				fmt.Fprintln(w, res.Content)
			})
			return nil
		},
	}
	cmd.Flags().BoolVar(&save, "save", false, "also store the document in the local index")
	cmd.Flags().StringVar(&browser, "browser", "", "browser mode for this fetch: bare --browser forces it, or --browser=auto|force|off|solve")
	cmd.Flags().Lookup("browser").NoOptDefVal = "true"
	return cmd
}

func indexCmd() *cobra.Command {
	var fromFile string
	cmd := &cobra.Command{
		Use:   "index <url>…",
		Short: "Fetch URLs and add them to the local index",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			urls := args
			if fromFile != "" {
				b, err := os.ReadFile(fromFile)
				if err != nil {
					return err
				}
				for _, l := range strings.Split(string(b), "\n") {
					if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
						urls = append(urls, l)
					}
				}
			}
			if len(urls) == 0 {
				return errors.New("no urls given (use args or --from-file)")
			}
			svc, err := newService()
			if err != nil {
				return err
			}
			defer svc.Close()
			var outcomes []service.IndexOutcome
			var errs []error
			for _, u := range urls {
				res, err := svc.IndexURL(cmd.Context(), u)
				if err != nil {
					errs = append(errs, fmt.Errorf("%s: %w", u, err))
					continue
				}
				outcomes = append(outcomes, *res)
				if !flagJSON {
					fmt.Printf("%-8s %s (%d chunks)\n", res.Status, res.URL, res.Chunks)
				}
			}
			if flagJSON {
				emit(outcomes, nil)
			}
			return errors.Join(errs...)
		},
	}
	cmd.Flags().StringVar(&fromFile, "from-file", "", "read URLs from a file (one per line)")
	return cmd
}

func crawlCmd() *cobra.Command {
	var maxPages int
	var sameDomain bool
	cmd := &cobra.Command{
		Use:   "crawl <seed-url>",
		Short: "Breadth-first crawl and index pages from a seed URL",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := newService()
			if err != nil {
				return err
			}
			defer svc.Close()
			res, err := svc.Crawl(cmd.Context(), args[0], maxPages, sameDomain)
			if err != nil {
				return err
			}
			emit(res, func(w io.Writer) {
				fmt.Fprintf(w, "indexed %d pages (%d unchanged), %d failures\n",
					res.Fetched, res.Unchanged, len(res.Failed))
				for _, f := range res.Failed {
					fmt.Fprintln(w, "  !", f)
				}
			})
			return nil
		},
	}
	cmd.Flags().IntVar(&maxPages, "max", 25, "maximum number of pages")
	cmd.Flags().BoolVar(&sameDomain, "same-domain", true, "stay within the seed's registrable domain")
	return cmd
}

func queryCmd() *cobra.Command {
	var k int
	cmd := &cobra.Command{
		Use:   "query <question>",
		Short: "Hybrid keyword+vector retrieval over the local index",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := newService()
			if err != nil {
				return err
			}
			defer svc.Close()
			res, err := svc.Query(cmd.Context(), strings.Join(args, " "), k)
			if err != nil {
				return err
			}
			emit(res, func(w io.Writer) {
				fmt.Fprintf(w, "mode: %s\n", res.Mode)
				for i, h := range res.Chunks {
					fmt.Fprintf(w, "\n── %d. %s  [%s score=%.4f]\n", i+1, h.Title, h.MatchedBy, h.Score)
					fmt.Fprintf(w, "   %s\n", h.URL)
					preview := h.Text
					if len([]rune(preview)) > 600 {
						preview = string([]rune(preview)[:600]) + "…"
					}
					for _, line := range strings.Split(preview, "\n") {
						fmt.Fprintf(w, "   %s\n", line)
					}
				}
				if w2 := sanitize.WarnSuspicion(res.Suspicion, nil); w2 != "" {
					fmt.Fprint(w, "\n"+w2)
				}
			})
			return nil
		},
	}
	cmd.Flags().IntVarP(&k, "top", "k", 8, "number of chunks to return")
	return cmd
}

func statsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stats",
		Short: "Show index statistics",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := newService()
			if err != nil {
				return err
			}
			defer svc.Close()
			st, err := svc.Stats()
			if err != nil {
				return err
			}
			emit(st, func(w io.Writer) {
				fmt.Fprintf(w, "db: %s\ndocs: %d\nchunks: %d\nvectors: %d (%s)\nembed: %s %s\nbrowser: %s\n",
					st.DBPath, st.Docs, st.Chunks, st.Vectors, orNone(st.VectorModel),
					st.EmbedBackend, orNone(st.EmbedModel), st.BrowserMode)
			})
			return nil
		},
	}
}

func reindexCmd() *cobra.Command {
	var vectors bool
	cmd := &cobra.Command{
		Use:   "reindex",
		Short: "Re-embed stored chunks (no re-fetching)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !vectors {
				return errors.New("nothing to do: use --vectors")
			}
			svc, err := newService()
			if err != nil {
				return err
			}
			defer svc.Close()
			if err := svc.ReindexVectors(cmd.Context()); err != nil {
				return err
			}
			fmt.Println("vectors rebuilt")
			return nil
		},
	}
	cmd.Flags().BoolVar(&vectors, "vectors", false, "recompute embeddings from stored chunks")
	return cmd
}

func doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check the environment (store, embedder, browser, pdftotext)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			report := map[string]any{}

			st, err := func() (any, error) {
				svc, err := service.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
				if err != nil {
					return map[string]any{"ok": false, "err": err.Error()}, nil
				}
				defer svc.Close()
				s, err := svc.Stats()
				if err != nil {
					return map[string]any{"ok": false, "err": err.Error()}, nil
				}
				return map[string]any{"ok": true, "db": s.DBPath, "docs": s.Docs, "chunks": s.Chunks}, nil
			}()
			if err == nil {
				report["store"] = st
			}

			switch cfg.Embed.Backend {
			case config.EmbedNone:
				report["embed"] = map[string]any{"ok": true, "mode": "none (keyword-only search)"}
			case config.EmbedOllama:
				names, err := embed.OllamaModels(ctx, cfg.Embed.URL)
				if err != nil {
					report["embed"] = map[string]any{"ok": false, "err": err.Error(),
						"hint": fmt.Sprintf("start ollama at %s", cfg.Embed.URL)}
					break
				}
				found := false
				for _, n := range names {
					if strings.HasPrefix(n, cfg.Embed.Model) {
						found = true
					}
				}
				if !found {
					report["embed"] = map[string]any{"ok": false,
						"err":  fmt.Sprintf("model %q not present locally", cfg.Embed.Model),
						"hint": "run: ollama pull " + cfg.Embed.Model}
				} else {
					report["embed"] = map[string]any{"ok": true, "model": cfg.Embed.Model}
				}
			default:
				emb, _ := embed.New(cfg)
				if emb == nil {
					report["embed"] = map[string]any{"ok": false}
				} else {
					v, err := emb.Embed(ctx, []string{"ping"})
					report["embed"] = map[string]any{"ok": err == nil, "dim": len(v[0]), "err": errStr(err)}
				}
			}

			br := map[string]any{"mode": cfg.Browser.Mode}
			if cfg.Browser.Mode != config.BrowserOff {
				p := browser.NewPlaywright(cfg, slog.Default())
				br["available"] = p.Available()
				if !br["available"].(bool) {
					br["hint"] = "run: loci browser install"
				}
			}
			report["browser"] = br

			if _, err := os.Stat(filepath.Join(cfg.DataDir, "profile")); err == nil {
				report["profile"] = cfg.ProfileDir()
			}
			chk := func(name string) { _, err := lookPath(name); report[name] = err == nil }
			chk("pdftotext")
			chk("chromium")

			emit(report, func(w io.Writer) {
				b, _ := json.MarshalIndent(report, "", "  ")
				fmt.Fprintln(w, string(b))
			})
			return nil
		},
	}
}

func serveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the MCP server over stdio",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// stdout belongs to MCP; logs go to stderr always
			slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
			svc, err := newService()
			if err != nil {
				return err
			}
			defer svc.Close()
			slog.Info("loci mcp server started", "tools", 5)
			return mcpserver.New(svc).Run(cmd.Context())
		},
	}
}

func browserCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "browser", Short: "Manage the playwright browser layer"}
	install := &cobra.Command{
		Use:   "install",
		Short: "Install the playwright driver and chromium (first-time setup)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return browser.Install(func(msg string) { fmt.Fprintln(os.Stderr, msg) })
		},
	}
	cmd.AddCommand(install)
	return cmd
}

func configCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "config",
		Short: "Show effective configuration and its file location",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			path := flagConfig
			if path == "" {
				path = config.DefaultConfigPath()
			}
			out := struct {
				Path string         `json:"path"`
				Cfg  *config.Config `json:"config"`
			}{Path: path, Cfg: cfg}
			emit(out, func(w io.Writer) {
				b, _ := json.MarshalIndent(out, "", "  ")
				fmt.Fprintln(w, string(b))
				fmt.Fprintf(w, "\n# create/edit %s to change settings\n", path)
			})
			return nil
		},
	}
}

// ---------------------------------------------------------------------------

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func lookPath(name string) (string, error) {
	for _, dir := range strings.Split(os.Getenv("PATH"), ":") {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not found", name)
}
