package spam

import (
	"sync"
	"time"
)

// greylister implements greylisting: the first time a (sender IP, MAIL
// FROM) pair is seen, the message is temporarily rejected. A legitimate
// MTA retries after a short delay and is then let through; much spam
// software never retries at all.
//
// State is in memory — losing it on a restart just re-greylists, which
// is harmless.
//
// TODO: move the tuple store to OxiMem so a multi-instance deployment
// shares one greylist; and key on the recipient too (classic
// greylisting uses the full IP / sender / recipient triplet, while this
// keys on IP + sender, so one delivery whitelists the pair for every
// recipient).
type greylister struct {
	delay      time.Duration    // a pending tuple must wait at least this long
	pendingTTL time.Duration    // forget un-retried tuples after this
	passedTTL  time.Duration    // re-greylist a passed tuple after this idle time
	now        func() time.Time // swapped out in tests

	mu     sync.Mutex
	tuples map[string]*greylistEntry
}

// greylistEntry is the state for one (IP, sender) tuple.
type greylistEntry struct {
	firstSeen time.Time
	lastSeen  time.Time
	passed    bool
}

func newGreylister(delay time.Duration) *greylister {
	return &greylister{
		delay:      delay,
		pendingTTL: 24 * time.Hour,
		passedTTL:  30 * 24 * time.Hour,
		now:        time.Now,
		tuples:     make(map[string]*greylistEntry),
	}
}

// check records a delivery attempt from (ip, mailFrom) and returns the
// verdict for it: Greylist until the tuple has been seen, waited out the
// delay, and retried; Accept thereafter.
func (g *greylister) check(ip, mailFrom string) Verdict {
	key := ip + "\x00" + mailFrom

	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.now()
	e := g.tuples[key]
	switch {
	case e == nil:
		// First contact — record it and ask the sender to retry.
		g.tuples[key] = &greylistEntry{firstSeen: now, lastSeen: now}
		return Greylist
	case e.passed:
		e.lastSeen = now
		return Accept
	case now.Sub(e.firstSeen) >= g.delay:
		// Retried, and the delay has elapsed — let it through from now on.
		e.passed = true
		e.lastSeen = now
		return Accept
	default:
		// Retried, but too soon.
		e.lastSeen = now
		return Greylist
	}
}

// sweep drops tuples that have aged out: pending ones that were never
// retried, and passed ones that have been idle a long time.
func (g *greylister) sweep() {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.now()
	for k, e := range g.tuples {
		if e.passed {
			if now.Sub(e.lastSeen) > g.passedTTL {
				delete(g.tuples, k)
			}
		} else if now.Sub(e.firstSeen) > g.pendingTTL {
			delete(g.tuples, k)
		}
	}
}
