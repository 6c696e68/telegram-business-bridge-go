package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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

// The core requirement: the bot answers only after the owner has been
// inactive for owner_away_minutes, and then answers whoever was left waiting.
func TestPresenceBotWaitsUntilOwnerAwayThenFollowsUp(t *testing.T) {
	h := newHarness(nil) // owner_away_minutes = 30, followup_max_min = 120
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
	// /away confirmation + the immediate follow-up for chat 750
	waitFor(t, "follow-up after /away", func() bool { return len(businessSends(h)) == 1 })
	if got := h.b.handleUpdate(biz(testConn, 751, 751, "hi")); got != "queued" {
		t.Fatalf("after /away got %q", got)
	}
	waitFor(t, "normal reply", func() bool { return len(businessSends(h)) == 2 })
}

func TestPresenceOwnerOnlineDuringLLMDropsReply(t *testing.T) {
	h := newHarness(nil)
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
	cfg := dbTestConfig(dir, func(c *Config) { c.DBFile = filepath.Join(dir, "bridge.db") })

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
