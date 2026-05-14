package spam

import (
	"sync"
	"time"
)

// rateLimiter is a fixed-window, per-IP message-rate limiter. State is
// in memory: it is connection-time spam defense, so losing it on a
// restart only resets the counters.
type rateLimiter struct {
	limit  int
	window time.Duration
	now    func() time.Time // swapped out in tests

	mu      sync.Mutex
	windows map[string]*rateWindow
}

// rateWindow is one IP's counter for the current window.
type rateWindow struct {
	count int
	start time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		limit:   limit,
		window:  window,
		now:     time.Now,
		windows: make(map[string]*rateWindow),
	}
}

// allow records one message from ip and reports whether it is within the
// rate limit. The window is fixed: it resets the first time a message
// arrives after the previous window has elapsed.
func (r *rateLimiter) allow(ip string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	w := r.windows[ip]
	if w == nil || now.Sub(w.start) >= r.window {
		r.windows[ip] = &rateWindow{count: 1, start: now}
		return true
	}
	w.count++
	return w.count <= r.limit
}

// sweep drops counters whose window has elapsed, bounding memory use.
func (r *rateLimiter) sweep() {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	for ip, w := range r.windows {
		if now.Sub(w.start) >= r.window {
			delete(r.windows, ip)
		}
	}
}
