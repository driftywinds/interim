package server

import (
	"math"
	"sync"
	"time"
)

type rateWindow struct {
	count int
	reset time.Time
}

type rateLimiter struct {
	mu      sync.Mutex
	windows map[string]*rateWindow
	lastGC  time.Time
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

	if now.Sub(l.lastGC) > 10*time.Minute || len(l.windows) > 10000 {
		for k, w := range l.windows {
			if now.After(w.reset) {
				delete(l.windows, k)
			}
		}
		l.lastGC = now
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
