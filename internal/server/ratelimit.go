package server

import (
	"math"
	"sync"
	"time"
)

const maxRateLimitWindows = 20000

type rateWindow struct {
	count int
	reset time.Time
}

type rateLimiter struct {
	mu      sync.Mutex
	windows map[string]*rateWindow
	lastGC  time.Time
}

// fixedRateLimiter protects a single global counter that must not be evicted by
// a flood of unrelated keys. It is used for the single admin account.
type fixedRateLimiter struct {
	mu     sync.Mutex
	window rateWindow
}

func (l *fixedRateLimiter) allow(limit int, window time.Duration) (bool, time.Duration) {
	if limit <= 0 {
		return true, 0
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.window.reset.IsZero() || now.After(l.window.reset) {
		l.window = rateWindow{count: 1, reset: now.Add(window)}
		return true, 0
	}
	if l.window.count >= limit {
		return false, time.Until(l.window.reset)
	}
	l.window.count++
	return true, 0
}

func (l *fixedRateLimiter) reset() {
	l.mu.Lock()
	l.window = rateWindow{}
	l.mu.Unlock()
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{
		windows: make(map[string]*rateWindow),
		lastGC:  time.Now(),
	}
}

// allow implements a fixed-window limiter. It returns false with the time
// remaining before the window resets.
func (l *rateLimiter) allow(key string, limit int, window time.Duration) (bool, time.Duration) {
	if limit <= 0 {
		return true, 0
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	if now.Sub(l.lastGC) > 10*time.Minute {
		for k, w := range l.windows {
			if now.After(w.reset) {
				delete(l.windows, k)
			}
		}
		l.lastGC = now
	}
	for len(l.windows) >= maxRateLimitWindows {
		l.evictOldestLocked()
	}

	entry, ok := l.windows[key]
	if !ok || now.After(entry.reset) {
		l.windows[key] = &rateWindow{count: 1, reset: now.Add(window)}
		return true, 0
	}
	if entry.count >= limit {
		return false, time.Until(entry.reset)
	}
	entry.count++
	return true, 0
}

// evictOldestLocked removes the entry closest to expiry. Random eviction let a
// key flood reset unrelated counters; auth counters use a separate limiter, so
// even a full general-purpose map cannot evict them.
func (l *rateLimiter) evictOldestLocked() {
	var oldestKey string
	var oldest time.Time
	for key, entry := range l.windows {
		if oldestKey == "" || entry.reset.Before(oldest) {
			oldestKey, oldest = key, entry.reset
		}
	}
	if oldestKey != "" {
		delete(l.windows, oldestKey)
	}
}

func (l *rateLimiter) reset(key string) {
	l.mu.Lock()
	delete(l.windows, key)
	l.mu.Unlock()
}

func retrySeconds(d time.Duration) int {
	if d <= 0 {
		return 1
	}
	return int(math.Ceil(d.Seconds()))
}
