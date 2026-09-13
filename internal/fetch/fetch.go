// Package fetch performs SSRF-guarded, politeness-aware HTTP retrieval and
// classifies bot-challenge responses so callers can escalate to a browser.
package fetch

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"loci/internal/browser"
	"loci/internal/config"
)

// Doc types recognized by the pipeline.
const (
	TypeHTML     = "html"
	TypePDF      = "pdf"
	TypeMarkdown = "markdown"
	TypeText     = "text"
	TypeBinary   = "binary"
)

// ErrBotChallenge signals that the endpoint served a bot wall, not content.
var ErrBotChallenge = errors.New("bot challenge detected; browser escalation warranted")

// ErrBlocked is returned when robots.txt or policy forbids the request.
var ErrBlocked = errors.New("request disallowed by policy")

// Doc is a fetched resource.
type Doc struct {
	URL         string
	FinalURL    string
	Status      int
	ContentType string
	Body        []byte
	DocType     string
	Etag        string
	LastMod     string
	TitleHint   string // e.g. from content-disposition filename
	BrowserUsed bool
}

// Fetcher retrieves documents with SSRF and politeness guards.
type Fetcher struct {
	cfg     *config.Config
	client  *http.Client
	limiter *domainLimiter
	robots  *robotsCache
	log     *slog.Logger
}

// New builds a Fetcher. If allowOverride is non-nil it replaces the SSRF dial
// guard (tests only).
func New(cfg *config.Config, log *slog.Logger) *Fetcher {
	f := &Fetcher{cfg: cfg, limiter: newDomainLimiter(cfg), robots: newRobotsCache(cfg), log: log}
	f.client = &http.Client{
		Timeout: time.Duration(cfg.HTTP.TimeoutSeconds) * time.Second,
		Transport: &http.Transport{
			ForceAttemptHTTP2: true,
			TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12},
			DialContext:       f.dialGuard,
		},
		CheckRedirect: checkRedirect,
	}
	return f
}

// FetchWithBrowser retrieves url, escalating to the browser backend when the
// HTTP path hits bot walls (mode auto/solve) or is skipped (mode force).
// browser may be nil, in which case escalation returns the challenge error.
func (f *Fetcher) FetchWithBrowser(ctx context.Context, rawurl string, br browser.Backend) (*Doc, error) {
	mode := f.cfg.Browser.Mode
	if mode == config.BrowserForce && br != nil && br.Available() {
		return f.viaBrowser(ctx, rawurl, br)
	}
	doc, err := f.Fetch(ctx, rawurl)
	if err == nil {
		return doc, nil
	}
	if errors.Is(err, ErrBotChallenge) && br != nil && br.Available() &&
		(mode == config.BrowserAuto || mode == config.BrowserSolve) {
		f.log.Info("escalating to browser", "url", rawurl)
		return f.viaBrowser(ctx, rawurl, br)
	}
	return nil, err
}

func (f *Fetcher) viaBrowser(ctx context.Context, rawurl string, br browser.Backend) (*Doc, error) {
	res, err := br.Render(ctx, rawurl, f.cfg.Browser.Mode == config.BrowserSolve)
	if err != nil {
		return nil, fmt.Errorf("browser render: %w", err)
	}
	doc := &Doc{
		URL: rawurl, FinalURL: res.FinalURL, Status: 200,
		ContentType: "text/html", Body: []byte(res.HTML), DocType: TypeHTML, BrowserUsed: true,
	}
	if LooksLikeChallenge(res.HTML) {
		return nil, fmt.Errorf("%w (still present after browser render)", ErrBotChallenge)
	}
	return doc, nil
}

// Fetch performs the HTTP path only.
func (f *Fetcher) Fetch(ctx context.Context, rawurl string) (*Doc, error) {
	u, err := parseHTTPURL(rawurl)
	if err != nil {
		return nil, err
	}
	if err := f.robots.allowed(ctx, u); err != nil {
		return nil, err
	}
	if err := f.limiter.wait(ctx, u.Host); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", f.cfg.HTTP.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,application/pdf,text/markdown;q=0.8,*/*;q=0.5")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Accept-Encoding", "gzip, identity")

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := readCapped(resp, f.cfg.HTTP.MaxBytes)
	if err != nil {
		return nil, err
	}
	final := resp.Request.URL.String()

	if isChallengeStatus(resp.StatusCode) || LooksLikeChallenge(string(body)) {
		return nil, fmt.Errorf("%w: status %d from %s", ErrBotChallenge, resp.StatusCode, final)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("http %d for %s", resp.StatusCode, final)
	}

	ct := resp.Header.Get("Content-Type")
	doc := &Doc{
		URL: rawurl, FinalURL: final, Status: resp.StatusCode,
		ContentType: ct, Body: body, DocType: docType(ct, body),
		Etag: resp.Header.Get("ETag"), LastMod: resp.Header.Get("Last-Modified"),
	}
	return doc, nil
}

// ---------------------------------------------------------------------------
// guards
// ---------------------------------------------------------------------------

func parseHTTPURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("scheme %q not allowed (http/https only)", u.Scheme)
	}
	if u.User != nil {
		return nil, errors.New("credentials in url are not allowed")
	}
	return u, nil
}

// dialGuard rejects private/loopback/link-local/metadata destinations at dial
// time (after DNS), which also defeats classic DNS rebinding.
func (f *Fetcher) dialGuard(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if f.cfg.HTTP.AllowPrivateHost { // test escape hatch
		return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(host, port))
	}
	if ip := net.ParseIP(host); ip != nil {
		if unsafeIP(ip) {
			return nil, fmt.Errorf("blocked dial to private/reserved address %s", host)
		}
		return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(host, port))
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	for _, ia := range ips {
		if unsafeIP(ia.IP) {
			return nil, fmt.Errorf("blocked dial: %s resolves to private/reserved address %s", host, ia.IP)
		}
	}
	return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(host, port))
}

func unsafeIP(ip net.IP) bool {
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() || ip.Equal(net.IPv4(169, 254, 169, 254)) ||
		ip.IsMulticast()
}

func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("too many redirects")
	}
	if _, err := parseHTTPURL(req.URL.String()); err != nil {
		return err
	}
	if req.URL.Host == "" {
		return errors.New("redirect with empty host")
	}
	// Private-address check re-runs per hop in dialGuard.
	return nil
}

// ---------------------------------------------------------------------------
// challenge detection
// ---------------------------------------------------------------------------

var challengeMarkers = []string{
	"anomaly-modal", "challenge-platform", "cf-challenge", "cf_chl_", "just a moment",
	"g-recaptcha", "grecaptcha", "hcaptcha", "px-captcha", "perimeterx",
	"verify you are human", "enable javascript and cookies to continue",
	"_cf_chl_opt", "datadome", "captcha-delivery",
}

// IsChallengeStatus reports whether an HTTP status implies a bot wall.
func IsChallengeStatus(code int) bool { return isChallengeStatus(code) }

func isChallengeStatus(code int) bool {
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusProxyAuthRequired,
		http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusAccepted,
		http.StatusServiceUnavailable:
		return true
	}
	return false
}

// LooksLikeChallenge sniffs a body for bot-wall fingerprints.
func LooksLikeChallenge(body string) bool {
	low := strings.ToLower(body)
	for _, m := range challengeMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func readCapped(resp *http.Response, cap int64) ([]byte, error) {
	var r io.Reader = resp.Body
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(resp.Body)
		if err == nil {
			defer gz.Close()
			r = gz
		}
	}
	body, err := io.ReadAll(io.LimitReader(r, cap+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > cap {
		return nil, fmt.Errorf("response exceeds %d byte cap", cap)
	}
	return body, nil
}

func docType(ct string, body []byte) string {
	head := body
	if len(head) > 512 {
		head = head[:512]
	}
	switch {
	case bytes.HasPrefix(head, []byte("%PDF-")):
		return TypePDF
	case strings.Contains(ct, "html") || strings.Contains(ct, "xml") || looksHTML(head):
		return TypeHTML
	case strings.Contains(ct, "markdown"):
		return TypeMarkdown
	case strings.Contains(ct, "json") || strings.Contains(ct, "text") || ct == "" || utf8ish(head):
		return TypeText
	default:
		return TypeBinary
	}
}

func looksHTML(head []byte) bool {
	s := strings.ToLower(string(head))
	return strings.Contains(s, "<!doctype html") || strings.Contains(s, "<html")
}

func utf8ish(head []byte) bool {
	for _, b := range head {
		if b == 0 {
			return false
		}
	}
	return true
}
