package notifier

import (
	"testing"
	"time"
)

func TestSubscribeAndNotify(t *testing.T) {
	h := NewHub()
	sub := h.Subscribe(42)
	defer sub.Close()

	h.Notify(42)
	select {
	case <-sub.C():
	case <-time.After(time.Second):
		t.Fatal("notification did not arrive")
	}
}

func TestNotifyIsScopedToTheMailbox(t *testing.T) {
	h := NewHub()
	subA := h.Subscribe(1)
	subB := h.Subscribe(2)
	defer subA.Close()
	defer subB.Close()

	h.Notify(2)
	select {
	case <-subB.C():
	case <-time.After(time.Second):
		t.Fatal("subscriber on mailbox 2 missed its notification")
	}
	select {
	case <-subA.C():
		t.Fatal("subscriber on mailbox 1 received a notification meant for mailbox 2")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestNotifyDoesNotBlock(t *testing.T) {
	h := NewHub()
	sub := h.Subscribe(7)
	defer sub.Close()

	// Fire many times without draining. The hub must not deadlock or
	// block — extra notifications coalesce into a single pending wake.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			h.Notify(7)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Notify blocked when the subscriber's buffer was full")
	}
	// One wake-up is buffered.
	select {
	case <-sub.C():
	default:
		t.Fatal("expected at least one pending wake-up")
	}
}

func TestCloseRemovesTheSubscription(t *testing.T) {
	h := NewHub()
	sub := h.Subscribe(9)
	sub.Close()

	// A second Close is a no-op (does not panic).
	sub.Close()

	// Notify finds no subscriber and is a no-op.
	h.Notify(9)
	select {
	case <-sub.C():
		// Buffer might still hold an old wake-up — that's fine; the
		// point of the test is that Close did not leave the hub
		// referencing this subscription internally.
	default:
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[9]; ok {
		t.Fatal("hub still has a subscription set for mailbox 9 after Close")
	}
}
