package fetch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"loci/internal/config"
)

func testCfg() *config.Config {
	cfg := config.Default()
	cfg.HTTP.MaxRPS = 0 // no artificial delay in tests
	cfg.HTTP.IgnoreRobots = true
	return cfg
}

func TestSSRFGuardBlocksPrivate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "should never be reached")
	}))
	defer srv.Close()
	f := New(testCfg(), discardLogger())
	_, err := f.Fetch(context.Background(), srv.URL) // 127.0.0.1
	if err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("expected SSRF block, got %v", err)
	}
}

func TestAllowPrivateOverrideWorks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<html><body>hi</body></html>")
	}))
	defer srv.Close()
	cfg := testCfg()
	cfg.HTTP.AllowPrivateHost = true
	f := New(cfg, discardLogger())
	doc, err := f.Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if doc.DocType != TypeHTML {
		t.Fatalf("doctype=%s", doc.DocType)
	}
}

func TestChallengeClassification(t *testing.T) {
	cases := map[int]string{
		http.StatusAccepted:  "<html>blocked</html>",
		http.StatusForbidden: "<html>cf-challenge</html>",
		http.StatusOK:        `<html><div class="g-recaptcha"></div></html>`,
	}
	for status, body := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(status)
			io.WriteString(w, body)
		}))
		cfg := testCfg()
		cfg.HTTP.AllowPrivateHost = true
		f := New(cfg, discardLogger())
		_, err := f.Fetch(context.Background(), srv.URL)
		if !errors.Is(err, ErrBotChallenge) {
			t.Errorf("status %d: expected ErrBotChallenge, got %v", status, err)
		}
		srv.Close()
	}
}

func TestSchemeRestriction(t *testing.T) {
	f := New(testCfg(), discardLogger())
	if _, err := f.Fetch(context.Background(), "file:///etc/passwd"); err == nil {
		t.Fatal("file:// must be rejected")
	}
	if _, err := f.Fetch(context.Background(), "http://user:pass@evil.example/"); err == nil {
		t.Fatal("credentials must be rejected")
	}
}

func TestBodyCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Repeat("x", 5000))
	}))
	defer srv.Close()
	cfg := testCfg()
	cfg.HTTP.AllowPrivateHost = true
	cfg.HTTP.MaxBytes = 1000
	f := New(cfg, discardLogger())
	if _, err := f.Fetch(context.Background(), srv.URL); err == nil {
		t.Fatal("expected cap error")
	}
}

func TestRobotsParsing(t *testing.T) {
	if !robotsDisallowsRoot("User-agent: *\nDisallow: /") {
		t.Error("wildcard disallow missed")
	}
	if robotsDisallowsRoot("User-agent: specialbot\nDisallow: /\n\nUser-agent: *\nAllow: /") {
		t.Error("specific-agent block wrongly applied")
	}
	if robotsDisallowsRoot("User-agent: *\nDisallow: /private") {
		t.Error("partial disallow misread as full")
	}
}

func TestUnsafeIP(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.0.0.5", "192.168.1.1", "169.254.169.254", "::1", "fe80::1"} {
		if !unsafeIP(parseIP(ip)) {
			t.Errorf("%s should be unsafe", ip)
		}
	}
	for _, ip := range []string{"93.184.216.34", "8.8.8.8"} {
		if unsafeIP(parseIP(ip)) {
			t.Errorf("%s should be safe", ip)
		}
	}
}
