package notify

import (
	"context"
	"sync"
	"testing"
)

// TestNotifierSetTransportsSwapsFanOut pins the reload seam: a daemon reload
// replaces the transport set in place, so the notifier the engine and the
// dead-man monitor already hold starts publishing to the new set without a
// restart. The old transport receives nothing after the swap.
func TestNotifierSetTransportsSwapsFanOut(t *testing.T) {
	ctx := context.Background()
	old := &captureTransport{}
	fresh := &captureTransport{}

	n := New(nil, old)
	if got := n.Transports(); got != 1 {
		t.Fatalf("Transports() = %d, want 1", got)
	}
	if err := n.Notify(ctx, Notification{Title: "before"}); err != nil {
		t.Fatalf("notify before swap: %v", err)
	}

	if got := n.SetTransports(fresh, nil); got != 1 {
		t.Fatalf("SetTransports returned %d, want 1 (the nil transport is dropped)", got)
	}
	if err := n.Notify(ctx, Notification{Title: "after"}); err != nil {
		t.Fatalf("notify after swap: %v", err)
	}

	if len(old.got) != 1 || old.got[0].Title != "before" {
		t.Fatalf("old transport received %+v, want only the pre-swap notification", old.got)
	}
	if len(fresh.got) != 1 || fresh.got[0].Title != "after" {
		t.Fatalf("new transport received %+v, want only the post-swap notification", fresh.got)
	}
}

// TestNotifierSetTransportsCanClear: a reload that finds no sealed transport
// clears the fan-out. Publishing is then a clean no-op, not an error — the
// documented "working daemon, silent phone" posture.
func TestNotifierSetTransportsCanClear(t *testing.T) {
	sink := &captureTransport{}
	n := New(nil, sink)

	if got := n.SetTransports(); got != 0 {
		t.Fatalf("SetTransports() returned %d, want 0", got)
	}
	if err := n.Notify(context.Background(), Notification{Title: "quiet"}); err != nil {
		t.Fatalf("notify with no transports: %v", err)
	}
	if len(sink.got) != 0 {
		t.Fatalf("cleared notifier still published: %+v", sink.got)
	}
}

// TestNotifierSetTransportsRace hammers the swap against publishes. It asserts
// nothing beyond not tripping the race detector: a publish must read its slice
// under the same lock the swap writes under.
func TestNotifierSetTransportsRace(_ *testing.T) {
	n := New(nil, &countingTransport{})
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				n.SetTransports(&countingTransport{})
				n.SetTransports()
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = n.Notify(ctx, Notification{Title: "race"})
				_ = n.Transports()
			}
		}()
	}
	wg.Wait()
}

// countingTransport is a concurrency-safe transport, so the swap race test
// exercises the notifier's own locking rather than a test transport's.
type countingTransport struct {
	mu sync.Mutex
	n  int
}

func (c *countingTransport) Name() string { return "counting" }
func (c *countingTransport) Publish(context.Context, Notification) error {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return nil
}
