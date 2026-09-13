package fetch

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"loci/internal/config"
	"golang.org/x/net/publicsuffix"
)

// robotsCache caches robots.txt per registrable domain with a 1h TTL.
// Search-engine SERP hosts are allowlisted: their anti-bot systems, not
// robots intent, govern scripted access to result pages.
type robotsCache struct {
	cfg     *config.Config
	mu      sync.Mutex
	entries map[string]*robotsEntry
	client  *http.Client
}

type robotsEntry struct {
	disallowAll bool
	expiry      time.Time
}

var serpHosts = []string{"duckduckgo.com", "mojeek.com", "bing.com"}

func newRobotsCache(cfg *config.Config) *robotsCache {
	return &robotsCache{
		cfg:     cfg,
		entries: map[string]*robotsEntry{},
		client:  &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{ForceAttemptHTTP2: true}},
	}
}

func (rc *robotsCache) allowed(ctx context.Context, u *url.URL) error {
	if rc.cfg.HTTP.IgnoreRobots {
		return nil
	}
	for _, h := range serpHosts {
		if strings.EqualFold(strings.TrimSuffix(strings.ToLower(u.Hostname()), "."), h) ||
			strings.HasSuffix(strings.ToLower(u.Hostname()), "."+h) {
			return nil
		}
	}
	key := siteKey(u.Hostname())
	rc.mu.Lock()
	e, ok := rc.entries[key]
	rc.mu.Unlock()
	if ok && e.expiry.After(time.Now()) {
		return robotsVerdict(e, u.Hostname())
	}

	disallow := false
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s://%s/robots.txt", u.Scheme, u.Host), nil)
	if err == nil {
		req.Header.Set("User-Agent", rc.cfg.HTTP.UserAgent)
		resp, err := rc.client.Do(req)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				buf := make([]byte, 0, 8192)
				tmp := make([]byte, 4096)
				for len(buf) < 64*1024 {
					n, rerr := resp.Body.Read(tmp)
					buf = append(buf, tmp[:n]...)
					if rerr != nil {
						break
					}
				}
				disallow = robotsDisallowsRoot(string(buf))
			}
		}
	}
	rc.mu.Lock()
	rc.entries[key] = &robotsEntry{disallowAll: disallow, expiry: time.Now().Add(time.Hour)}
	rc.mu.Unlock()
	return robotsVerdict(&robotsEntry{disallowAll: disallow}, u.Hostname())
}

// robotsDisallowsRoot parses user-agent blocks and reports whether the
// wildcard (or any) agent faces "Disallow: /".
func robotsDisallowsRoot(robots string) bool {
	inBlock := false // current block applies to us ("*" or unrecognized agent treated leniently)
	sawDisallowRoot := false
	for _, raw := range strings.Split(robots, "\n") {
		line := raw
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		kv := strings.SplitN(line, ":", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(kv[0]))
		val := strings.TrimSpace(kv[1])
		switch key {
		case "user-agent":
			v := strings.ToLower(val)
			inBlock = v == "*" // specific-agent blocks are ignored (lenient v1)
		case "disallow":
			if inBlock && (val == "/" || val == "/*") {
				sawDisallowRoot = true
			}
		}
	}
	return sawDisallowRoot
}

func robotsVerdict(e *robotsEntry, host string) error {
	if e.disallowAll {
		return fmt.Errorf("%w: robots.txt of %s disallows fetching", ErrBlocked, host)
	}
	return nil
}

func siteKey(host string) string {
	et, _ := publicsuffix.PublicSuffix(host)
	if strings.HasSuffix(strings.ToLower(host), "."+strings.ToLower(et)) || strings.EqualFold(host, et) {
		return strings.ToLower(et)
	}
	return strings.ToLower(host)
}
