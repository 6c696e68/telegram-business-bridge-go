package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The owner's private chat used to answer one message at a time: four messages
// typed in a row produced four bubbles. A burst must become one turn.
func TestOwnerBurstGetsOneReply(t *testing.T) {
	h := newHarness(func(c *Config) { c.DebounceMS = 150; c.OwnerDebounceMS = 150 })
	h.b.sleep = func(ctx context.Context, d time.Duration) {
		select {
		case <-time.After(d):
		case <-ctx.Done():
		}
	}

	h.b.handleUpdate(dm(testOwner, testOwner, "tôi là linh"))
	h.b.handleUpdate(dm(testOwner, testOwner, "bạn là ai"))
	h.b.handleUpdate(dm(testOwner, testOwner, "giúp tôi với"))

	waitSent(t, h, 1)
	settle()
	if n := len(h.tg.sends()); n != 1 {
		t.Fatalf("an owner burst must yield one reply, got %d", n)
	}

	h.llm.mu.Lock()
	defer h.llm.mu.Unlock()
	if len(h.llm.seen) != 1 {
		t.Fatalf("llm calls = %d, want 1", len(h.llm.seen))
	}
	joined := ""
	for _, m := range h.llm.seen[0] {
		joined += m.Content + "\n"
	}
	for _, want := range []string{"tôi là linh", "bạn là ai", "giúp tôi với"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("merged turn missing %q", want)
		}
	}
}

// Messages sent one at a time, after each answer, still get one answer each.
func TestOwnerSequentialMessagesStaySeparate(t *testing.T) {
	h := newHarness(func(c *Config) { c.DebounceMS = 0; c.OwnerDebounceMS = 0 })

	h.b.handleUpdate(dm(testOwner, testOwner, "một"))
	waitSent(t, h, 1)
	h.b.handleUpdate(dm(testOwner, testOwner, "hai"))
	waitSent(t, h, 2)

	if n := len(h.tg.sends()); n != 2 {
		t.Fatalf("separate turns must stay separate, got %d sends", n)
	}
}

// A message typed while the model is still thinking joins the next turn instead
// of being dropped.
func TestOwnerMessageDuringTurnIsAnswered(t *testing.T) {
	h := newHarness(func(c *Config) { c.DebounceMS = 0; c.OwnerDebounceMS = 0 })
	release := make(chan struct{})
	h.llm.block = release

	h.b.handleUpdate(dm(testOwner, testOwner, "câu một"))
	waitFor(t, "first llm call", func() bool { return h.llm.calls() == 1 })
	h.b.handleUpdate(dm(testOwner, testOwner, "câu hai"))
	close(release)

	waitSent(t, h, 2)
	h.b.mu.Lock()
	queue := len(h.b.ownerEditQueue)
	h.b.mu.Unlock()
	if queue != 0 {
		t.Fatalf("owner queue must drain, %d left", queue)
	}
}
