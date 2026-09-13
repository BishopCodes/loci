package fetch

import (
	"context"
	"sync"
	"time"

	"loci/internal/config"
)

// domainLimiter is a per-domain token bucket (simple leaky scheduling).
type domainLimiter struct {
	mu       sync.Mutex
	interval time.Duration
	next     map[string]time.Time
}

func newDomainLimiter(cfg *config.Config) *domainLimiter {
	var interval time.Duration
	if cfg.HTTP.MaxRPS > 0 {
		interval = time.Duration(float64(time.Second) / cfg.HTTP.MaxRPS)
	}
	return &domainLimiter{interval: interval, next: map[string]time.Time{}}
}

func (l *domainLimiter) wait(ctx context.Context, host string) error {
	if l.interval <= 0 {
		return nil
	}
	l.mu.Lock()
	t, ok := l.next[host]
	now := time.Now()
	if !ok || t.Before(now) {
		l.next[host] = now.Add(l.interval)
		l.mu.Unlock()
		return nil
	}
	l.next[host] = t.Add(l.interval)
	l.mu.Unlock()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Until(t)):
		return nil
	}
}
