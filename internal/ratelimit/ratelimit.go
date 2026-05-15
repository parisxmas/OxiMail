// Package ratelimit is OxiMail's brute-force shield over the
// authentication entry points (SMTP submission AUTH, IMAP LOGIN /
// AUTHENTICATE, webmail POST /api/login).
//
// The model is a leaky-bucket counter of *failed* attempts keyed by
// client IP. Successful logins do not touch the bucket, so a
// legitimate user with the right password never feels the limit.
// Failed attempts increment the bucket; the bucket drains at a fixed
// rate. When the bucket reaches the threshold, further attempts are
// rejected outright (the auth code is not even consulted, which also
// shields the bcrypt cost) until the counter drains below threshold.
//
// Callers use the limiter like:
//
//	if limiter.Blocked(ip) { return errTooManyAttempts }
//	if err := authenticate(...); err != nil {
//	    limiter.RecordFailure(ip)
//	    return errAuthFailed
//	}
package ratelimit

import (
	"sync"
	"time"
)

// DefaultThreshold is the number of failed attempts allowed before an
// IP is blocked.
const DefaultThreshold = 10

// DefaultDecay is how long it takes one accumulated failure to drain
// off. With the default threshold of 10 that means a fully-loaded
// bucket clears in DefaultDecay × DefaultThreshold (= 60 seconds).
const DefaultDecay = 6 * time.Second

// Limiter is a per-key leaky-bucket failure counter. Safe for use by
// many goroutines.
//
// Internally it tracks an integer failure count and a future "next
// drain" time. Each time the bucket is examined, any drain ticks that
// have elapsed are applied (one drain tick removes one failure). The
// model deliberately avoids floating-point arithmetic so the
// at-threshold comparison is exact.
type Limiter struct {
	threshold int
	decay     time.Duration

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	count     int
	nextDrain time.Time
}

// New returns a limiter with the given threshold and per-token decay.
// A zero or negative threshold disables the limiter — Blocked always
// returns false.
func New(threshold int, decay time.Duration) *Limiter {
	return &Limiter{
		threshold: threshold,
		decay:     decay,
		buckets:   map[string]*bucket{},
	}
}

// NewDefault returns a limiter with the standard settings.
func NewDefault() *Limiter {
	return New(DefaultThreshold, DefaultDecay)
}

// Blocked reports whether key has exhausted its failure budget. An
// empty key (e.g. an IP we could not parse) is never blocked.
func (l *Limiter) Blocked(key string) bool {
	if l == nil || l.threshold <= 0 || key == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		return false
	}
	l.drain(b)
	return b.count >= l.threshold
}

// RecordFailure debits one token from key's bucket. Call this after an
// authentication attempt fails; do NOT call it on success.
func (l *Limiter) RecordFailure(key string) {
	if l == nil || l.threshold <= 0 || key == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{}
		l.buckets[key] = b
	} else {
		l.drain(b)
	}
	if b.count == 0 {
		// First failure in a fresh window — schedule the first drain.
		b.nextDrain = time.Now().Add(l.decay)
	}
	b.count++
}

// Sweep drops buckets that have fully drained, freeing memory. It is
// safe to call from a background goroutine; the limiter remains usable
// throughout.
func (l *Limiter) Sweep() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, b := range l.buckets {
		l.drain(b)
		if b.count == 0 {
			delete(l.buckets, key)
		}
	}
}

// drain removes one failure for every l.decay that has elapsed since
// the next scheduled drain, advancing nextDrain accordingly. The
// caller must hold l.mu.
func (l *Limiter) drain(b *bucket) {
	if l.decay <= 0 || b.count == 0 {
		return
	}
	now := time.Now()
	for b.count > 0 && !now.Before(b.nextDrain) {
		b.count--
		b.nextDrain = b.nextDrain.Add(l.decay)
	}
}
