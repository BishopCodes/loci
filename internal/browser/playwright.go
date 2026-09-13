// Package browser wraps Playwright (playwright-go) as the escalation layer for
// bot walls and captchas. It never imports other dsearch packages.
package browser

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	playwright "github.com/mxschmitt/playwright-go"

	"dsearch/internal/config"
)

// Result is a rendered page.
type Result struct {
	FinalURL string
	HTML     string
}

// Backend renders pages with a real browser.
type Backend interface {
	// Available reports whether a browser can currently be launched.
	Available() bool
	// Render loads url and returns its post-JS HTML. When headed is true the
	// browser opens visibly so a human can solve a captcha.
	Render(ctx context.Context, url string, headed bool) (*Result, error)
}

// ErrUnavailable is returned when the browser layer cannot run.
var ErrUnavailable = errors.New("browser backend unavailable (run `dsearch browser install`)")

// Playwright implements Backend via playwright-go.
type Playwright struct {
	cfg      *config.Config
	log      *slog.Logger
	mu       sync.Mutex
	pw       *playwright.Playwright
	chromium playwright.BrowserType
}

// NewPlaywright constructs the backend. Launch is lazy; constructor never fails.
func NewPlaywright(cfg *config.Config, log *slog.Logger) *Playwright {
	return &Playwright{cfg: cfg, log: log}
}

// Available reports whether the playwright runtime started at least once.
func (p *Playwright) Available() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pw != nil {
		return true
	}
	// cheap probe: driver or cached browsers present
	if dir, err := os.UserCacheDir(); err == nil {
		if _, err := os.Stat(filepath.Join(dir, "ms-playwright")); err == nil {
			return true
		}
	}
	return false
}

func (p *Playwright) ensure(ctx context.Context) error {
	if p.pw != nil {
		return nil
	}
	if v, ok := os.LookupEnv("PLAYWRIGHT_BROWSERS_PATH"); !ok || v == "" {
		if dir, err := os.UserCacheDir(); err == nil {
			os.Setenv("PLAYWRIGHT_BROWSERS_PATH", filepath.Join(dir, "ms-playwright"))
		}
	}
	pw, err := playwright.Run()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	p.pw = pw
	p.chromium = pw.Chromium
	return nil
}

// Render launches chromium, blocks asset routes, loads url, returns HTML.
func (p *Playwright) Render(ctx context.Context, rawurl string, headed bool) (*Result, error) {
	if err := p.ensure(ctx); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	args := []string{
		"--no-sandbox",
		"--disable-blink-features=AutomationControlled",
		"--disable-dev-shm-usage",
	}
	var browser playwright.Browser
	var context playwright.BrowserContext
	var err error

	// In solve mode prefer a persistent profile so trust cookies accumulate.
	if headed {
		if err := os.MkdirAll(p.cfg.ProfileDir(), 0o700); err == nil {
			context, err = p.chromium.LaunchPersistentContext(p.cfg.ProfileDir(),
				playwright.BrowserTypeLaunchPersistentContextOptions{
					Headless:  playwright.Bool(false),
					Args:      args,
					UserAgent: playwright.String(p.cfg.HTTP.UserAgent),
					Viewport:  &playwright.Size{Width: 1366, Height: 900},
				})
			if err == nil {
				defer context.Close()
			} else {
				context = nil
			}
		}
	}
	if context == nil {
		browser, err = p.chromium.Launch(playwright.BrowserTypeLaunchOptions{
			Headless: playwright.Bool(!headed),
			Args:     args,
		})
		if err != nil {
			return nil, fmt.Errorf("launch chromium: %w", err)
		}
		defer browser.Close()
		context, err = browser.NewContext(playwright.BrowserNewContextOptions{
			UserAgent: playwright.String(p.cfg.HTTP.UserAgent),
			Viewport:  &playwright.Size{Width: 1366, Height: 900},
		})
		if err != nil {
			return nil, fmt.Errorf("new context: %w", err)
		}
		defer context.Close()
	}

	if p.cfg.Browser.BlockAssets == nil || *p.cfg.Browser.BlockAssets {
		if err := context.Route("**/*", func(route playwright.Route) {
			switch route.Request().ResourceType() {
			case "script", "stylesheet", "image", "font", "media", "beacon":
				_ = route.Abort()
			default:
				_ = route.Continue()
			}
		}); err != nil {
			return nil, fmt.Errorf("route setup: %w", err)
		}
	}

	page, err := context.NewPage()
	if err != nil {
		return nil, fmt.Errorf("new page: %w", err)
	}
	navTimeout := float64((p.cfg.HTTP.TimeoutSeconds + 15) * 1000)
	_, err = page.Goto(rawurl, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(navTimeout),
	})
	if err != nil {
		return nil, fmt.Errorf("navigate %s: %w", rawurl, err)
	}

	if headed {
		// give the human time to solve the captcha, polling for load stability
		deadline := time.Now().Add(time.Duration(p.cfg.Browser.SolveTimeout) * time.Second)
		for time.Now().Before(deadline) {
			if err := page.WaitForLoadState(playwright.PageWaitForLoadStateOptions{
				State:   playwright.LoadStateNetworkidle,
				Timeout: playwright.Float(5000),
			}); err == nil {
				break
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Second):
			}
		}
	} else {
		_ = page.WaitForLoadState(playwright.PageWaitForLoadStateOptions{
			State:   playwright.LoadStateNetworkidle,
			Timeout: playwright.Float(8000),
		})
	}

	html, err := page.Content()
	if err != nil {
		return nil, fmt.Errorf("read content: %w", err)
	}
	return &Result{FinalURL: page.URL(), HTML: html}, nil
}

// Install drives `playwright install` (driver + browsers) for setup/doctor.
func Install(progress func(msg string)) error {
	if progress != nil {
		progress("installing playwright driver and browsers (downloads ~350MB)…")
	}
	opts := &playwright.RunOptions{}
	if progress != nil {
		opts.Stdout = &printer{fn: progress}
		opts.Stderr = &printer{fn: progress}
	}
	return playwright.Install(opts)
}

type printer struct{ fn func(string) }

func (p *printer) Write(b []byte) (int, error) {
	if s := strings.TrimSpace(string(b)); s != "" {
		p.fn(s)
	}
	return len(b), nil
}
