package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func businessSends(h *harness) []map[string]any {
	var out []map[string]any
	for _, s := range h.tg.sends() {
		if s["business_connection_id"] == testConn {
			out = append(out, s)
		}
	}
	return out
}

func llmSaw(h *harness, needle string) bool {
	h.llm.mu.Lock()
	defer h.llm.mu.Unlock()
	for _, call := range h.llm.seen {
		for _, m := range call {
			if strings.Contains(m.Content, needle) {
				return true
			}
		}
	}
	return false
}

// The owner's rule (screenshot 2026-11-09): counting from the owner's last
// activity, only messages that arrive after owner_away_minutes are answered,
// immediately. Messages sent while the owner was online are never answered
// later by the bot.
func TestPresenceOnlyMessagesAfterAwayAreAnswered(t *testing.T) {
	h := newHarness(nil)                                       // defaults: owner_away_minutes = 30, follow-ups off
	h.b.handleUpdate(biz(testConn, 690, testOwner, "chào bố")) // 22:12 owner active
	h.clock.Advance(1 * time.Minute)                           // 22:13
	if got := h.b.handleUpdate(biz(testConn, 690, 690, "vừa làm vừa mò")); got != "muted" {
		t.Fatalf("message while the owner is around got %q", got)
	}
	if got := h.b.handleUpdate(biz(testConn, 691, 691, "ở chat khác")); got != "owner_online" {
		t.Fatalf("other chat while the owner is around got %q", got)
	}

	h.clock.Advance(29 * time.Minute) // 22:42: owner silent for 30 minutes
	if n := h.b.sweepPending(); n != 0 {
		t.Fatalf("old messages must never be answered later, sweep started %d", n)
	}
	settle()
	if n := len(businessSends(h)); n != 0 {
		t.Fatalf("bot answered messages sent while the owner was online: %d", n)
	}

	// A new message after the away threshold is answered immediately.
	if got := h.b.handleUpdate(biz(testConn, 690, 690, "đkm")); got != "queued" {
		t.Fatalf("message after the owner went away got %q", got)
	}
	waitFor(t, "immediate reply", func() bool { return len(businessSends(h)) == 1 })
	settle()
	if n := len(businessSends(h)); n != 1 {
		t.Fatalf("exactly one reply expected, got %d", n)
	}
}

// Opt-in (followup_max_min > 0): customers left waiting are answered once the
// owner has been away long enough.
func TestPresenceOptInFollowupAnswersWaitingCustomers(t *testing.T) {
	h := newHarness(func(c *Config) { c.FollowupMaxMin = 120 })
	if got := h.b.handleUpdate(biz(testConn, 700, testOwner, "chào chị")); got != "owner_takeover" {
		t.Fatalf("got %q", got)
	}
	if got := h.b.handleUpdate(biz(testConn, 701, 701, "shop ơi còn hàng không")); got != "owner_online" {
		t.Fatalf("customer in another chat got %q, want owner_online", got)
	}

	h.clock.Advance(29 * time.Minute)
	if n := h.b.sweepPending(); n != 0 {
		t.Fatalf("follow-up started %d jobs before the owner was away", n)
	}
	settle()
	if n := len(businessSends(h)); n != 0 {
		t.Fatalf("bot replied while the owner was online: %d", n)
	}

	h.clock.Advance(2 * time.Minute) // 31 minutes without owner activity
	if n := h.b.sweepPending(); n != 1 {
		t.Fatalf("follow-up jobs = %d, want 1", n)
	}
	waitFor(t, "follow-up reply", func() bool { return len(businessSends(h)) == 1 })
	if got := businessSends(h)[0]["chat_id"].(float64); got != 701 {
		t.Fatalf("follow-up went to chat %v", got)
	}
	if !llmSaw(h, "còn hàng không") {
		t.Fatal("the waiting question must be in the follow-up context")
	}

	// Nothing left to answer: another sweep is a no-op.
	settle()
	if n := h.b.sweepPending(); n != 0 {
		t.Fatalf("second sweep started %d jobs", n)
	}
}

func TestPresenceOwnerAnsweredMeansNoFollowup(t *testing.T) {
	h := newHarness(nil)
	h.b.handleUpdate(biz(testConn, 710, testOwner, "đang online"))
	h.b.handleUpdate(biz(testConn, 711, 711, "cho mình hỏi"))
	h.b.handleUpdate(biz(testConn, 711, testOwner, "dạ anh hỏi gì ạ")) // owner answers
	h.clock.Advance(31 * time.Minute)
	if n := h.b.sweepPending(); n != 0 {
		t.Fatalf("follow-up must not answer a chat the owner already answered, started %d", n)
	}
}

func TestPresenceFollowupWindow(t *testing.T) {
	h := newHarness(func(c *Config) { c.FollowupMaxMin = 60 })
	h.b.handleUpdate(biz(testConn, 720, testOwner, "online"))
	h.b.handleUpdate(biz(testConn, 721, 721, "tin cũ"))
	h.clock.Advance(90 * time.Minute)
	if n := h.b.sweepPending(); n != 0 {
		t.Fatalf("messages older than followup_max_min must not be answered, started %d", n)
	}
}

func TestPresenceBotEchoIsNotOwnerActivity(t *testing.T) {
	h := newHarness(nil)
	echo := biz(testConn, 730, testOwner, "Dạ em là trợ lý AI")
	echo.BusinessMessage.SenderBusinessBot = &User{ID: 999, IsBot: true}
	if got := h.b.handleUpdate(echo); got != "" {
		t.Fatalf("bot echo handled as %q", got)
	}
	if got := h.b.handleUpdate(biz(testConn, 730, 730, "ok")); got != "queued" {
		t.Fatalf("customer got %q; the bot's own message must not mute the chat", got)
	}
}

func TestPresenceOwnerEditMarksActive(t *testing.T) {
	h := newHarness(nil)
	edit := Update{UpdateID: 1, EditedBusinessMsg: &Message{
		BusinessConnectionID: testConn,
		Chat:                 Chat{ID: 740, Type: "private"},
		From:                 &User{ID: testOwner},
		Text:                 "sửa tin",
	}}
	if got := h.b.handleUpdate(edit); got != "owner_active" {
		t.Fatalf("owner edit got %q", got)
	}
	if got := h.b.handleUpdate(biz(testConn, 741, 741, "hi")); got != "owner_online" {
		t.Fatalf("customer got %q after owner edit", got)
	}
	customerEdit := Update{UpdateID: 2, EditedBusinessMsg: &Message{
		BusinessConnectionID: testConn,
		Chat:                 Chat{ID: 742, Type: "private"},
		From:                 &User{ID: 742},
		Text:                 "khách sửa",
	}}
	if got := h.b.handleUpdate(customerEdit); got != "" {
		t.Fatalf("customer edit must be ignored, got %q", got)
	}
}

func TestPresenceHereAndAwayCommands(t *testing.T) {
	h := newHarness(nil)
	h.b.handleUpdate(dm(testOwner, testOwner, "/here"))
	waitSent(t, h, 1)
	if got := h.b.handleUpdate(biz(testConn, 750, 750, "alo")); got != "owner_online" {
		t.Fatalf("after /here got %q", got)
	}
	h.b.handleUpdate(dm(testOwner, testOwner, "/away"))
	waitSent(t, h, 2) // /here + /away confirmations
	settle()
	if n := len(businessSends(h)); n != 0 {
		t.Fatalf("/away must not answer messages sent before it, got %d", n)
	}
	if got := h.b.handleUpdate(biz(testConn, 751, 751, "hi")); got != "queued" {
		t.Fatalf("after /away got %q", got)
	}
	waitFor(t, "normal reply", func() bool { return len(businessSends(h)) == 1 })
}

func TestPresenceOwnerOnlineDuringLLMDropsReply(t *testing.T) {
	h := newHarness(func(c *Config) { c.FollowupMaxMin = 120 })
	gate := make(chan struct{})
	h.llm.block = gate
	if got := h.b.handleUpdate(biz(testConn, 760, 760, "hỏi nhanh")); got != "queued" {
		t.Fatalf("got %q", got)
	}
	waitFor(t, "llm inflight", func() bool { return h.llm.maxInflight() >= 1 })
	h.b.handleUpdate(biz(testConn, 761, testOwner, "mình đây")) // owner shows up elsewhere
	close(gate)
	settle()
	time.Sleep(50 * time.Millisecond)
	if n := len(businessSends(h)); n != 0 {
		t.Fatalf("reply must be dropped when the owner comes online mid-generation, got %d", n)
	}
	// The question stays pending and is answered once the owner leaves again.
	h.clock.Advance(31 * time.Minute)
	if n := h.b.sweepPending(); n != 1 {
		t.Fatalf("follow-up jobs = %d, want 1", n)
	}
	waitFor(t, "follow-up", func() bool { return len(businessSends(h)) == 1 })
}

func TestPresenceDirectChatsAreNotGated(t *testing.T) {
	h := newHarness(nil)
	h.b.handleUpdate(biz(testConn, 770, testOwner, "online"))
	if got := h.b.handleUpdate(dm(771, 771, "chào bot")); got != "queued" {
		t.Fatalf("direct chat with the bot got %q", got)
	}
	waitSent(t, h, 1)
}

func TestPresenceDisabledKeepsPerChatBehaviour(t *testing.T) {
	h := newHarness(func(c *Config) { c.OwnerAwayMinutes = 0 })
	h.b.handleUpdate(biz(testConn, 780, testOwner, "online"))
	if got := h.b.handleUpdate(biz(testConn, 781, 781, "hi")); got != "queued" {
		t.Fatalf("got %q with presence disabled", got)
	}
	if got := h.b.handleUpdate(biz(testConn, 780, 780, "hi")); got != "muted" {
		t.Fatalf("per-chat takeover must still mute, got %q", got)
	}
}

func TestPresenceStatusLine(t *testing.T) {
	h := newHarness(nil)
	h.b.handleUpdate(biz(testConn, 790, testOwner, "online"))
	h.b.handleUpdate(biz(testConn, 791, 791, "chờ"))
	h.b.handleUpdate(dm(testOwner, testOwner, "/status"))
	waitSent(t, h, 1)
	text, _ := h.tg.sends()[0]["text"].(string)
	if !strings.Contains(text, "🟢 online") || !strings.Contains(text, "Chat chờ trả lời: 1") {
		t.Fatalf("status missing presence details: %q", text)
	}
	h.clock.Advance(45 * time.Minute)
	h.b.mu.Lock()
	line := h.b.presenceLineLocked()
	h.b.mu.Unlock()
	if !strings.Contains(line, "offline 45 phút") {
		t.Fatalf("presence line = %q", line)
	}
}

func TestPresenceSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	cfg := dbTestConfig(dir, func(c *Config) {
		c.DBFile = filepath.Join(dir, "bridge.db")
		c.FollowupMaxMin = 120
	})

	b1 := NewBridge(cfg, &fakeTG{conns: map[string]BusinessConnection{
		testConn: ownerConn(testConn, testOwner, true, true),
	}}, &fakeLLM{reply: "ok"})
	b1.mu.Lock()
	seeded := b1.ownerSeen
	b1.mu.Unlock()
	if seeded.IsZero() {
		t.Fatal("first boot must start conservatively with the owner treated as online")
	}
	if got := b1.handleUpdate(biz(testConn, 800, 800, "khách chờ")); got != "owner_online" {
		t.Fatalf("got %q", got)
	}
	if err := b1.Close(); err != nil {
		t.Fatal(err)
	}

	tg2 := &fakeTG{conns: map[string]BusinessConnection{testConn: ownerConn(testConn, testOwner, true, true)}}
	b2 := NewBridge(cfg, tg2, &fakeLLM{reply: "ok"})
	defer func() { _ = b2.Close() }()
	b2.mu.Lock()
	restored := b2.ownerSeen
	cs := b2.chats["b:"+testConn+":800"]
	pending := cs != nil && cs.pendingLocked() && cs.chatID == 800 && cs.conn == testConn
	b2.mu.Unlock()
	if restored.Unix() != seeded.Unix() {
		t.Fatalf("owner presence not restored: %v vs %v", restored, seeded)
	}
	if !pending {
		t.Fatal("the waiting customer must survive a restart")
	}

	b2.mu.Lock()
	b2.ownerSeen = time.Time{} // owner away
	b2.mu.Unlock()
	if n := b2.sweepPending(); n != 1 {
		t.Fatalf("follow-up after restart = %d, want 1", n)
	}
	waitFor(t, "follow-up after restart", func() bool { return len(tg2.sends()) == 1 })
}

// ---------- startup robustness ----------

type flakyStartTG struct {
	mu        sync.Mutex
	failures  int
	getMe     int
	cancelRun context.CancelFunc
}

func (f *flakyStartTG) Call(ctx context.Context, method string, params any, out any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch method {
	case "getMe":
		f.getMe++
		if f.getMe <= f.failures {
			return &NetError{Method: method, Kind: "conn"}
		}
		return nil
	case "getUpdates":
		f.cancelRun()
		return &NetError{Method: method, Kind: "canceled"}
	}
	return nil
}

func TestRunRetriesGetMeWhileNetworkIsDown(t *testing.T) {
	h := newHarness(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tg := &flakyStartTG{failures: 2, cancelRun: cancel}
	h.b.tg = tg
	done := make(chan error, 1)
	go func() { done <- h.b.Run(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run must survive transient getMe failures, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run hung")
	}
	tg.mu.Lock()
	defer tg.mu.Unlock()
	if tg.getMe != 3 {
		t.Fatalf("getMe attempts = %d, want 3", tg.getMe)
	}
}

func TestRunFailsFastOnRejectedToken(t *testing.T) {
	h := newHarness(nil)
	h.b.tg = senderFunc(func(ctx context.Context, method string, params any, out any) error {
		return &TGError{Method: method, Code: 401, Desc: "Unauthorized"}
	})
	err := h.b.Run(context.Background())
	var te *TGError
	if err == nil || !errors.As(err, &te) || te.Code != 401 {
		t.Fatalf("want a 401 error, got %v", err)
	}
}

type senderFunc func(ctx context.Context, method string, params any, out any) error

func (f senderFunc) Call(ctx context.Context, method string, params any, out any) error {
	return f(ctx, method, params, out)
}

func TestFollowupFailureDoesNotBurnQuotaAndBacksOff(t *testing.T) {
	h := newHarness(func(c *Config) { c.FollowupMaxMin = 120 })
	h.b.handleUpdate(biz(testConn, 810, testOwner, "online"))
	h.b.handleUpdate(biz(testConn, 811, 811, "khách chờ"))
	h.clock.Advance(31 * time.Minute)

	llmDown := &LLMError{Status: 400, Kind: "http"} // non-retryable: fails at once
	h.llm.mu.Lock()
	h.llm.errs = []error{llmDown}
	h.llm.mu.Unlock()
	if n := h.b.sweepPending(); n != 1 {
		t.Fatalf("first follow-up attempt = %d, want 1", n)
	}
	waitFor(t, "failed attempt done", func() bool {
		h.b.mu.Lock()
		defer h.b.mu.Unlock()
		return h.b.chats["b:"+testConn+":811"].active == 0
	})
	h.b.mu.Lock()
	day, hits := h.b.dayCount, len(h.b.userHits[811])
	h.b.mu.Unlock()
	if day != 0 || hits != 0 {
		t.Fatalf("a failed follow-up consumed quota: day=%d hits=%d", day, hits)
	}
	if n := h.b.sweepPending(); n != 0 {
		t.Fatalf("retry must back off, started %d", n)
	}
	h.clock.Advance(61 * time.Second)
	if n := h.b.sweepPending(); n != 1 {
		t.Fatalf("retry after backoff = %d, want 1", n)
	}
	waitFor(t, "delivered follow-up", func() bool { return len(businessSends(h)) == 1 })
	settle()
	h.b.mu.Lock()
	day, hits = h.b.dayCount, len(h.b.userHits[811])
	h.b.mu.Unlock()
	if day != 1 || hits != 1 {
		t.Fatalf("a delivered follow-up must consume exactly one slot: day=%d hits=%d", day, hits)
	}
}

func TestDeliveredReplyIsPersistedImmediately(t *testing.T) {
	dir := t.TempDir()
	h := newHarness(func(c *Config) { c.DBFile = filepath.Join(dir, "bridge.db") })
	defer func() { _ = h.b.Close() }()
	h.b.mu.Lock()
	h.b.ownerSeen = time.Time{} // owner away
	h.b.mu.Unlock()
	h.b.handleUpdate(biz(testConn, 820, 820, "hỏi giá"))
	waitFor(t, "reply", func() bool { return len(businessSends(h)) == 1 })
	key := []byte("b:" + testConn + ":820")
	waitFor(t, "record persisted as answered", func() bool {
		answered := false
		_ = h.b.db.View(func(tx *bolt.Tx) error {
			raw := tx.Bucket(bucketChats).Get(key)
			if raw == nil {
				return nil
			}
			var rec chatRecord
			if json.Unmarshal(raw, &rec) == nil && !rec.Pending && len(rec.Msgs) == 2 {
				answered = true
			}
			return nil
		})
		return answered
	})
}

func TestPendingChatsAreNotEvicted(t *testing.T) {
	h := newHarness(func(c *Config) {
		c.MaxChats = 1
		c.FollowupMaxMin = 120
	})
	h.b.handleUpdate(biz(testConn, 830, testOwner, "online"))
	h.b.handleUpdate(biz(testConn, 831, 831, "khách A"))
	h.b.handleUpdate(biz(testConn, 832, 832, "khách B"))
	h.b.mu.Lock()
	_, a := h.b.chats["b:"+testConn+":831"]
	_, bb := h.b.chats["b:"+testConn+":832"]
	h.b.mu.Unlock()
	if !a || !bb {
		t.Fatalf("waiting chats must not be evicted: A=%v B=%v", a, bb)
	}
	h.clock.Advance(31 * time.Minute)
	if n := h.b.sweepPending(); n != 2 {
		t.Fatalf("follow-ups = %d, want 2", n)
	}
	waitFor(t, "both answered", func() bool { return len(businessSends(h)) == 2 })
}
