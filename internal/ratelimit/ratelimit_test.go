package ratelimit

import (
	"testing"
	"time"
)

func TestBlocksAtTheThreshold(t *testing.T) {
	l := New(3, time.Hour) // decay is effectively disabled for the test
	for i := 0; i < 3; i++ {
		if l.Blocked("ip") {
			t.Fatalf("blocked before reaching the threshold (i=%d)", i)
		}
		l.RecordFailure("ip")
	}
	if !l.Blocked("ip") {
		t.Fatal("not blocked after 3 failures with a threshold of 3")
	}
}

func TestPerKeyIsolation(t *testing.T) {
	l := New(2, time.Hour)
	l.RecordFailure("a")
	l.RecordFailure("a")
	if !l.Blocked("a") {
		t.Fatal("a should be blocked")
	}
	if l.Blocked("b") {
		t.Fatal("b should not be affected by a's failures")
	}
}

func TestDecayDrainsTheBucket(t *testing.T) {
	// Decay one token per millisecond, so the bucket drains during the
	// 30 ms sleep below.
	l := New(3, time.Millisecond)
	l.RecordFailure("ip")
	l.RecordFailure("ip")
	l.RecordFailure("ip")
	if !l.Blocked("ip") {
		t.Fatal("expected to be blocked right after 3 failures")
	}
	time.Sleep(30 * time.Millisecond)
	if l.Blocked("ip") {
		t.Fatal("expected the bucket to drain after 30 ms")
	}
}

func TestEmptyKeyIsNeverBlocked(t *testing.T) {
	l := New(1, time.Hour)
	l.RecordFailure("")
	l.RecordFailure("")
	if l.Blocked("") {
		t.Fatal("an empty key should never be blocked")
	}
}

func TestZeroThresholdDisablesTheLimiter(t *testing.T) {
	l := New(0, time.Hour)
	for i := 0; i < 100; i++ {
		l.RecordFailure("ip")
	}
	if l.Blocked("ip") {
		t.Fatal("a limiter with threshold 0 must allow every attempt")
	}
}

func TestNilReceiverIsSafe(t *testing.T) {
	var l *Limiter
	// Must not panic on any operation.
	l.RecordFailure("ip")
	if l.Blocked("ip") {
		t.Fatal("a nil limiter must report not blocked")
	}
	l.Sweep()
}

func TestSweepPrunesDrainedBuckets(t *testing.T) {
	l := New(5, time.Millisecond)
	l.RecordFailure("ip")
	time.Sleep(5 * time.Millisecond) // let it drain to 0
	l.Sweep()
	l.mu.Lock()
	_, present := l.buckets["ip"]
	l.mu.Unlock()
	if present {
		t.Fatal("Sweep did not drop a drained bucket")
	}
}
