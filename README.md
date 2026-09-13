# loci

Key-free web search, clean content extraction, and **local hybrid retrieval** in one Go binary — with prompt-injection defenses in the data path, not in a disclaimer.

```
 ┌─ CLI ──────────────┐   ┌──────────────────────────────────────────────┐
 │ search fetch index │   │ search: DDG/Mojeek/SearXNG chain (HTTP-first, │
 │ crawl query serve  ├──►│ browser escalation on bot walls — no API keys)│
 └────────────────────┘   ├─ fetch: SSRF-guarded HTTP + playwright layer  │
 ┌─ MCP server ───────┐   ├─ extract: HTML→readability→Markdown, PDF, MD  │
 │ web_search  web_   │   ├─ sanitize: unicode hygiene, injection         │
 │ fetch  index query │   │   detector, URL defang, untrusted envelope    │
 │ web_status         │   ├─ store: SQLite — FTS5(BM25) + float32 cosine  │
 └────────────────────┘   │   vectors, RRF fusion, content-hash incremental│
                          └──────────────────────────────────────────────┘
```

## Why

Search providers want money per query. Most of the "internet" is already reachable with an HTTP GET, and the rest yields to a real browser. loci scrapes the public result pages directly (HTTP first, headless Chromium on bot walls, an opt-in *solve mode* where a visible browser waits for you to solve a captcha), extracts just the document — scripts and CSS are stripped, asset routes are blocked outright — and stores everything in a local index you can ask questions of, with no cloud round trip.

The indexing model follows the pattern [trailhq/Graft](https://github.com/trailhq/Graft) uses for codebases, applied to web content: content-hash keyed incremental ingestion, boundary-aware chunking, and hybrid keyword+semantic retrieval served over MCP.

## Quick start

```sh
make build                      # → bin/loci
./bin/loci doctor            # check store/browser/embedder/pdf tooling
./bin/loci search "golang mcp sdk"
./bin/loci index https://example.com/a-post
./bin/loci query "what did the post say about X"

# true-local embeddings (optional, recommended):
ollama pull nomic-embed-text
loci config   # then set embed.backend = "ollama" in the printed path
loci reindex --vectors       # re-embed already-stored chunks

# first-time browser layer (driver download; browsers reused from ~/.cache):
loci browser install
```

### MCP wiring (Claude Code / Cursor / any MCP host)

```json
{
  "mcpServers": {
    "loci": { "command": "/absolute/path/to/bin/loci", "args": ["serve"] }
  }
}
```

Tools: `web_search`, `web_fetch`, `web_index`, `web_query`, `web_status`.

## Security model (read this)

Web pages are adversarial input. loci assumes anything a page says may be a
prompt-injection attempt aimed at the *agent* that eventually reads it.

1. **Structural quarantine.** All web content exits loci inside a per-call salted
   `<untrusted-<random> source="…" fetched="…" sha256="…">` envelope, mirrored in
   typed structured output with `suspicion` metadata. The envelope tags found inside
   content are neutralized first, so a page cannot forge a closing tag and escape.
2. **Unicode hygiene.** NFKC normalization; zero-width, bidi-control and tag
   characters removed; control chars stripped.
3. **Detection, never deletion.** A weighted rule set (instruction overrides,
   fake role lines/tags, chat-template tokens, exfil phrasing, credential strings…)
   attaches `suspicion=none|low|medium|high` + signal names. Content is never
   silently modified beyond step 2.
4. **URL defanging.** URLs inside search snippets arrive as `hxxps://host[.]path` so
   agents don't reflexively fetch planted links; real URLs stay in provenance fields.
5. **SSRF guard.** Only `http(s)`, no URL credentials, ≤5 redirects each re-validated,
   dial-time rejection of private/loopback/link-local/metadata addresses
   (defeats DNS rebinding). `allow_private_host` exists for tests only.
6. **Browser discipline.** Assets (script/CSS/image/font) blocked by default;
   per-context isolation; downloads off; captchas are only solved by *you* in
   `solve` mode against a persistent profile — no captcha circumvention services.
7. **No interpreter authority.** loci itself never runs an LLM on fetched text;
   retrieval and interpretation stay separated by design.

Residual risk: envelope discipline depends on the host agent honoring it. That's
why the server `instructions`, tool descriptions, and every warning line restate it.

## Architecture notes

- **Search chain** (`internal/search`): `ddg-html → ddg-lite → mojeek → searxng → ddg-browser → bing-browser` (configurable). HTTP responses are classified (`ErrBotChallenge`: status 202/403/429 + `anomaly-modal`, `cf-challenge`, reCAPTCHA markers…) so escalation is evidence-driven, not timer-driven.
- **Browser layer** (`internal/browser`): `playwright-go` over cached Chromium. `auto` escalates per request; `force` skips HTTP; `off` disables; `solve` relaunches headed with a persistent profile and waits for you.
- **Extraction** (`internal/extract`): goquery-based noise stripping → `go-readability` main-content (full-body fallback) → in-house HTML→Markdown writer (`htmlmd`, no dead deps). PDF: `pdftotext` (poppler) when present, pure-Go fallback otherwise.
- **Store** (`internal/store`): pure-Go SQLite (`modernc.org/sqlite`, no CGO):
  `docs` (unique URL + sha256 — unchanged hashes make re-index a no-op), `chunks`,
  `chunks_fts` (FTS5 porter/BM25, trigger-synced), `vectors` (float32 blobs +
  in-memory cosine — exact, fast to ~10⁵ chunks; revisit beyond). Retrieval =
  BM25 ∪ cosine → **Reciprocal Rank Fusion**. Zero-vector state degrades to
  keyword-only transparently.

## CLI

| command | purpose |
|---|---|
| `search <q> [-n N] [--json]` | provider-chain web search |
| `fetch <url> [--save] [--browser]` | extract to Markdown (envelope-wrapped) |
| `index <url>… \| --from-file urls.txt` | incremental indexing |
| `crawl <seed> [--max N] [--same-domain]` | polite BFS crawl + index |
| `query <q> [-k N]` | hybrid retrieval over the store |
| `stats` / `reindex --vectors` | counters / re-embed without re-fetch |
| `doctor` | capability report (store, ollama/models, browser, pdftotext) |
| `serve` | MCP server on stdio |
| `browser install` | first-time playwright driver setup |

Config: `$XDG_CONFIG_HOME/loci/config.toml` (see `config.example.toml`), env overrides `LOCI_BROWSER`, `LOCI_DATA_DIR`, `LOCI_EMBED_*`, `LOCI_SEARX_URLS`, `LOCI_IGNORE_ROBOTS`.

## Known limits / roadmap

- No JavaScript-heavy SERP fallbacks beyond DDG/Bing pages; add more parsers per provider.
- No OCR for scanned PDFs (flagged `degraded`).
- Brute-force vectors until ~100k chunks; then consider sqlite-vec.
- `crawl` is BFS with a link budget; no sitemap/RSS-aware crawling yet.
- Captcha policy: human-in-the-loop only.

## Development

```sh
make test   # unit + httptest-backed integration, in-process MCP round-trip
make vet
```
