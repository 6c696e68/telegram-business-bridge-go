package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

const (
	testOwner = 1000000001
	testConn  = "c1"
)

func TestMain(m *testing.M) {
	logger.SetOutput(silence)
	os.Exit(m.Run())
}

// ---------- fakes ----------

type tgCall struct {
	Method string
	Params map[string]any
}

type fakeTG struct {
	mu       sync.Mutex
	calls    []tgCall
	conns    map[string]BusinessConnection
	sendErrs []error
}

func (f *fakeTG) Call(ctx context.Context, method string, params any, out any) error {
	raw, _ := json.Marshal(params)
	var p map[string]any
	_ = json.Unmarshal(raw, &p)

	f.mu.Lock()
	f.calls = append(f.calls, tgCall{method, p})
	switch method {
	case "getBusinessConnection":
		id, _ := p["business_connection_id"].(string)
		bc, ok := f.conns[id]
		f.mu.Unlock()
		if !ok {
			return &TGError{Method: method, Code: 400, Desc: "Bad Request: connection not found"}
		}
		if out != nil {
			data, _ := json.Marshal(bc)
			return json.Unmarshal(data, out)
		}
		return nil
	case "sendMessage":
		if len(f.sendErrs) > 0 {
			e := f.sendErrs[0]
			f.sendErrs = f.sendErrs[1:]
			f.mu.Unlock()
			return e
		}
	}
	f.mu.Unlock()
	return nil
}

func (f *fakeTG) sends() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, c := range f.calls {
		if c.Method == "sendMessage" {
			out = append(out, c.Params)
		}
	}
	return out
}

func (f *fakeTG) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.Method == method {
			n++
		}
	}
	return n
}

type fakeLLM struct {
	mu       sync.Mutex
	reply    string
	errs     []error
	seen     [][]ChatMsg
	delay    time.Duration
	block    chan struct{}
	inflight int
	maxIn    int
}

func (f *fakeLLM) Chat(ctx context.Context, msgs []ChatMsg) (string, error) {
	f.mu.Lock()
	f.seen = append(f.seen, append([]ChatMsg(nil), msgs...))
	f.inflight++
	if f.inflight > f.maxIn {
		f.maxIn = f.inflight
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inflight--
		f.mu.Unlock()
	}()
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.errs) > 0 {
		e := f.errs[0]
		f.errs = f.errs[1:]
		return "", e
	}
	return f.reply, nil
}

func (f *fakeLLM) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.seen)
}

func (f *fakeLLM) lastSeen() []ChatMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen[len(f.seen)-1]
}

func (f *fakeLLM) maxInflight() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxIn
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type sleepLog struct {
	mu sync.Mutex
	d  []time.Duration
}

func (s *sleepLog) record(ctx context.Context, d time.Duration) {
	s.mu.Lock()
	s.d = append(s.d, d)
	s.mu.Unlock()
}

func (s *sleepLog) all() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.d...)
}

// ---------- harness ----------

type harness struct {
	b      *Bridge
	tg     *fakeTG
	llm    *fakeLLM
	clock  *fakeClock
	sleeps *sleepLog
}

func ptr[T any](v T) *T { return &v }

func ownerConn(id string, owner int64, enabled, canReply bool) BusinessConnection {
	return BusinessConnection{
		ID:        id,
		User:      &User{ID: owner},
		IsEnabled: ptr(enabled),
		Rights:    &BizRights{CanReply: ptr(canReply)},
	}
}

func newHarness(mut func(*Config)) *harness {
	cfg := defaultConfig()
	cfg.BotToken = "123:TESTTOKEN"
	cfg.OwnerID = testOwner
	cfg.OwnerUsername = "example_owner"
	cfg.APIBase = "http://fake"
	cfg.APIKey = "k"
	if mut != nil {
		mut(&cfg)
	}
	h := &harness{
		tg: &fakeTG{conns: map[string]BusinessConnection{
			testConn: ownerConn(testConn, testOwner, true, true),
		}},
		llm:    &fakeLLM{reply: "Dạ em trả lời"},
		clock:  &fakeClock{t: time.Date(2026, 11, 9, 12, 0, 0, 0, time.Local)},
		sleeps: &sleepLog{},
	}
	h.b = NewBridge(cfg, h.tg, h.llm)
	h.b.now = h.clock.Now
	h.b.sleep = h.sleeps.record
	h.b.day = h.clock.Now().Format("2006-01-02")
	h.b.botID = 999
	h.b.botUsername = "example_bot"
	return h
}

func biz(conn string, chat, from int64, text string) Update {
	return Update{UpdateID: chat, BusinessMessage: &Message{
		BusinessConnectionID: conn,
		Chat:                 Chat{ID: chat, Type: "private"},
		From:                 &User{ID: from},
		Text:                 text,
	}}
}

func dm(chat, from int64, text string) Update {
	return Update{UpdateID: chat, Message: &Message{
		Chat: Chat{ID: chat, Type: "private"},
		From: &User{ID: from},
		Text: text,
	}}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func waitSent(t *testing.T, h *harness, n int) {
	t.Helper()
	waitFor(t, "sendMessage x"+string(rune('0'+n)), func() bool { return len(h.tg.sends()) >= n })
}

func settle() { time.Sleep(60 * time.Millisecond) }

// ---------- port of the Python test suite ----------

func TestBusinessReplyUsesConnection(t *testing.T) {
	h := newHarness(nil)
	if got := h.b.handleUpdate(biz(testConn, 111, 111, "hello")); got != "queued" {
		t.Fatalf("got %q", got)
	}
	waitSent(t, h, 1)
	s := h.tg.sends()[0]
	if s["business_connection_id"] != testConn || s["chat_id"].(float64) != 111 {
		t.Fatalf("bad send params: %v", s)
	}
}

func TestForeignConnectionRejected(t *testing.T) {
	h := newHarness(nil)
	h.tg.conns["c2"] = ownerConn("c2", 555, true, true)
	if got := h.b.handleUpdate(biz("c2", 111, 111, "hello")); got != "conn_rejected" {
		t.Fatalf("got %q", got)
	}
	settle()
	if len(h.tg.sends()) != 0 {
		t.Fatal("must not send through foreign connection")
	}
}

func TestConnectionWithoutReplyRightRejected(t *testing.T) {
	h := newHarness(nil)
	h.tg.conns["c3"] = ownerConn("c3", testOwner, true, false)
	if got := h.b.handleUpdate(biz("c3", 111, 111, "hi")); got != "conn_rejected" {
		t.Fatalf("got %q", got)
	}
}

func TestDisabledConnectionRejected(t *testing.T) {
	h := newHarness(nil)
	h.tg.conns["c4"] = ownerConn("c4", testOwner, false, true)
	if got := h.b.handleUpdate(biz("c4", 111, 111, "hi")); got != "conn_rejected" {
		t.Fatalf("got %q", got)
	}
}

func TestOwnerTypingMutesChat(t *testing.T) {
	h := newHarness(nil)
	if got := h.b.handleUpdate(biz(testConn, 111, testOwner, "để anh trả lời")); got != "owner_takeover" {
		t.Fatalf("got %q", got)
	}
	if got := h.b.handleUpdate(biz(testConn, 111, 111, "ok anh")); got != "muted" {
		t.Fatalf("got %q", got)
	}
	if got := h.b.handleUpdate(biz(testConn, 333, 333, "hi")); got != "queued" {
		t.Fatalf("other chat got %q", got)
	}
	waitSent(t, h, 1)
}

func TestMuteExpires(t *testing.T) {
	h := newHarness(nil)
	h.b.handleUpdate(biz(testConn, 111, testOwner, "x"))
	h.clock.Advance(31 * time.Minute)
	if got := h.b.handleUpdate(biz(testConn, 111, 111, "y")); got != "queued" {
		t.Fatalf("got %q", got)
	}
	waitSent(t, h, 1)
}

func TestPauseResumeOwnerOnly(t *testing.T) {
	h := newHarness(nil)
	if got := h.b.handleUpdate(dm(testOwner, testOwner, "/pause")); got != "owner_cmd" {
		t.Fatalf("got %q", got)
	}
	if got := h.b.handleUpdate(biz(testConn, 111, 111, "hello")); got != "paused" {
		t.Fatalf("got %q", got)
	}
	if got := h.b.handleUpdate(dm(222, 222, "/resume")); got != "paused" {
		t.Fatalf("stranger /resume should be a normal message while paused, got %q", got)
	}
	h.b.mu.Lock()
	stillPaused := h.b.paused
	h.b.mu.Unlock()
	if !stillPaused {
		t.Fatal("stranger resumed the bot")
	}
	h.b.handleUpdate(dm(testOwner, testOwner, "/resume"))
	h.b.mu.Lock()
	defer h.b.mu.Unlock()
	if h.b.paused {
		t.Fatal("owner /resume did not resume")
	}
}

func TestOwnerCommandWithBotSuffix(t *testing.T) {
	h := newHarness(nil)
	if got := h.b.handleUpdate(dm(testOwner, testOwner, "/status@example_bot")); got != "owner_cmd" {
		t.Fatalf("got %q", got)
	}
	waitFor(t, "status reply", func() bool {
		s := h.tg.sends()
		return len(s) == 1 && strings.Contains(s[0]["text"].(string), "đang chạy")
	})
}

func TestStrangerCommandsAreNormalMessages(t *testing.T) {
	h := newHarness(nil)
	if got := h.b.handleUpdate(dm(222, 222, "/status")); got != "queued" {
		t.Fatalf("stranger /status got %q, want queued", got)
	}
	waitSent(t, h, 1)
	if strings.Contains(h.tg.sends()[0]["text"].(string), "Trạng thái") {
		t.Fatal("stranger received owner status")
	}
}

func TestDirectDMReplyHasNoConnection(t *testing.T) {
	h := newHarness(nil)
	h.b.handleUpdate(dm(222, 222, "hi"))
	waitSent(t, h, 1)
	if _, has := h.tg.sends()[0]["business_connection_id"]; has {
		t.Fatal("direct reply carried a business_connection_id")
	}
}

func TestGroupNeedsMentionOrReply(t *testing.T) {
	h := newHarness(nil)
	if got := h.b.handleUpdate(Update{UpdateID: 1, Message: &Message{
		Chat: Chat{ID: -100, Type: "supergroup"}, From: &User{ID: 5}, Text: "hi all"}}); got != "" {
		t.Fatalf("unaddressed group msg got %q", got)
	}
	if got := h.b.handleUpdate(Update{UpdateID: 2, Message: &Message{
		Chat: Chat{ID: -100, Type: "supergroup"}, From: &User{ID: 5}, Text: "hi @example_bot"}}); got != "queued" {
		t.Fatalf("mention got %q", got)
	}
	if got := h.b.handleUpdate(Update{UpdateID: 3, Message: &Message{
		Chat: Chat{ID: -5, Type: "group"}, From: &User{ID: 6}, Text: "and you?",
		ReplyToMessage: &struct {
			From *User `json:"from"`
		}{From: &User{ID: 999}}}}); got != "queued" {
		t.Fatalf("reply-to-bot got %q", got)
	}
	waitSent(t, h, 2)
}

func TestPerUserRateLimit(t *testing.T) {
	h := newHarness(func(c *Config) { c.PerUserPerHour = 2 })
	var got []string
	for i := 0; i < 3; i++ {
		got = append(got, h.b.handleUpdate(dm(int64(100+i), 7, "m")))
	}
	if got[0] != "queued" || got[1] != "queued" || got[2] != "rate_limited" {
		t.Fatalf("got %v", got)
	}
	waitSent(t, h, 2)
}

func TestDailyCap(t *testing.T) {
	h := newHarness(func(c *Config) { c.DailyReplyCap = 1 })
	if got := h.b.handleUpdate(dm(1, 1, "a")); got != "queued" {
		t.Fatalf("got %q", got)
	}
	if got := h.b.handleUpdate(dm(2, 2, "b")); got != "rate_limited" {
		t.Fatalf("got %q", got)
	}
	waitSent(t, h, 1)
}

func TestDailyCapResetsNextDay(t *testing.T) {
	h := newHarness(func(c *Config) { c.DailyReplyCap = 1 })
	h.b.mu.Lock()
	ok1 := h.b.rateOK(1)
	ok2 := h.b.rateOK(2)
	h.b.mu.Unlock()
	h.clock.Advance(24 * time.Hour)
	h.b.mu.Lock()
	ok3 := h.b.rateOK(3)
	h.b.mu.Unlock()
	if !ok1 || ok2 || !ok3 {
		t.Fatalf("cap sequence wrong: %v %v %v", ok1, ok2, ok3)
	}
}

func TestHistoryAndSystemPrompt(t *testing.T) {
	h := newHarness(nil)
	h.b.handleUpdate(dm(1, 1, "first"))
	waitSent(t, h, 1)
	h.b.handleUpdate(dm(1, 1, "second"))
	waitSent(t, h, 2)
	msgs := h.llm.seen[1]
	var roles []string
	for _, m := range msgs {
		roles = append(roles, m.Role)
	}
	if strings.Join(roles, ",") != "system,user,assistant,user" {
		t.Fatalf("roles = %v", roles)
	}
	if !strings.Contains(msgs[0].Content, "@example_owner") {
		t.Fatal("owner tag missing from system prompt")
	}
}

func TestBotsIgnored(t *testing.T) {
	h := newHarness(nil)
	u := dm(1, 1, "hi")
	u.Message.From.IsBot = true
	if got := h.b.handleUpdate(u); got != "" {
		t.Fatalf("bot message got %q", got)
	}
}

func TestOwnerReplyKeptInHistory(t *testing.T) {
	h := newHarness(nil)
	h.b.handleUpdate(biz(testConn, 111, 111, "hỏi"))
	waitSent(t, h, 1)
	h.b.handleUpdate(biz(testConn, 111, testOwner, "anh trả lời tay"))
	h.clock.Advance(time.Hour)
	h.b.handleUpdate(biz(testConn, 111, 111, "hỏi tiếp"))
	waitSent(t, h, 2)
	msgs := h.llm.lastSeen()
	var roles []string
	var sawOwner bool
	for _, m := range msgs {
		roles = append(roles, m.Role)
		if m.Role == "assistant" && strings.Contains(m.Content, "anh trả lời tay") {
			sawOwner = true
		}
	}
	if strings.Join(roles, ",") != "system,user,assistant,user" || !sawOwner {
		t.Fatalf("roles=%v sawOwner=%v", roles, sawOwner)
	}
}

func TestIntroOnlyOnFirstReplyPerChat(t *testing.T) {
	h := newHarness(nil)
	h.b.handleUpdate(dm(7, 7, "hi"))
	waitSent(t, h, 1)
	if !strings.Contains(h.llm.seen[0][0].Content, "FIRST reply") {
		t.Fatal("first reply missing intro instruction")
	}
	h.b.handleUpdate(dm(7, 7, "again"))
	waitSent(t, h, 2)
	if !strings.Contains(h.llm.seen[1][0].Content, "ALREADY introduced") {
		t.Fatal("second reply should not re-introduce")
	}
	h.b.handleUpdate(dm(8, 8, "other chat"))
	waitSent(t, h, 3)
	if !strings.Contains(h.llm.seen[2][0].Content, "FIRST reply") {
		t.Fatal("new chat should get intro")
	}
}

func TestPromptFileAndReload(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(p, []byte("PROMPT-V1 {owner_tag}"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(func(c *Config) { c.SystemPromptFile = p })
	h.b.mu.Lock()
	h.b.sysPrompt, _ = h.b.readPrompt()
	h.b.mu.Unlock()
	h.b.handleUpdate(dm(1, 1, "hi"))
	waitSent(t, h, 1)
	if !strings.Contains(h.llm.seen[0][0].Content, "PROMPT-V1 @example_owner") {
		t.Fatal("v1 prompt not used")
	}
	os.WriteFile(p, []byte("PROMPT-V2"), 0o600)
	if got := h.b.handleUpdate(dm(testOwner, testOwner, "/reload")); got != "owner_cmd" {
		t.Fatalf("got %q", got)
	}
	h.b.handleUpdate(dm(2, 2, "hi"))
	waitSent(t, h, 3)
	if !strings.Contains(h.llm.lastSeen()[0].Content, "PROMPT-V2") {
		t.Fatal("reload did not apply")
	}
	if got := h.b.handleUpdate(dm(9, 9, "/reload")); got != "queued" {
		t.Fatalf("stranger /reload got %q", got)
	}
}

func TestEmptyLLMReplySendsNothing(t *testing.T) {
	h := newHarness(nil)
	h.llm.reply = ""
	h.b.handleUpdate(dm(1, 1, "hi"))
	waitFor(t, "llm call", func() bool { return h.llm.calls() == 1 })
	settle()
	if len(h.tg.sends()) != 0 {
		t.Fatal("empty reply was sent")
	}
}

// ---------- concurrency, eviction, memory bounds ----------

func TestLLMConcurrencyCapped(t *testing.T) {
	h := newHarness(func(c *Config) { c.MaxConcurrentLLM = 2 })
	h.llm.delay = 80 * time.Millisecond
	for i := 0; i < 6; i++ {
		h.b.handleUpdate(dm(int64(10+i), int64(10+i), "hi"))
	}
	waitSent(t, h, 6)
	if m := h.llm.maxInflight(); m > 2 {
		t.Fatalf("max concurrent LLM = %d, want <= 2", m)
	}
}

func TestLRUEvictsOldestChats(t *testing.T) {
	h := newHarness(func(c *Config) { c.MaxChats = 3 })
	for i := 1; i <= 5; i++ {
		h.b.handleUpdate(biz(testConn, int64(i), testOwner, "x"))
	}
	got := h.b.chatKeys()
	want := []string{"b:c1:3", "b:c1:4", "b:c1:5"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("keys = %v, want %v", got, want)
	}
}

func TestRecentChatNotEvicted(t *testing.T) {
	h := newHarness(func(c *Config) { c.MaxChats = 2 })
	h.b.handleUpdate(biz(testConn, 1, testOwner, "a"))
	h.b.handleUpdate(biz(testConn, 2, testOwner, "b"))
	h.b.handleUpdate(biz(testConn, 1, testOwner, "a2")) // touch 1
	h.b.handleUpdate(biz(testConn, 3, testOwner, "c"))
	got := h.b.chatKeys()
	if strings.Join(got, ",") != "b:c1:1,b:c1:3" {
		t.Fatalf("keys = %v", got)
	}
}

func TestActiveChatNeverEvicted(t *testing.T) {
	h := newHarness(func(c *Config) { c.MaxChats = 2 })
	h.llm.block = make(chan struct{})
	h.b.handleUpdate(dm(1, 1, "slow")) // reply in flight, chat d:1 active
	waitFor(t, "llm started", func() bool { return h.llm.calls() == 1 })
	h.b.handleUpdate(biz(testConn, 10, testOwner, "a"))
	h.b.handleUpdate(biz(testConn, 11, testOwner, "b")) // would evict d:1 under naive LRU
	got := h.b.chatKeys()
	if strings.Join(got, ",") != "b:c1:11,d:1" {
		t.Fatalf("keys = %v; in-flight chat was evicted", got)
	}
	close(h.llm.block)
	waitSent(t, h, 1)
}

func TestStaleUserHitsPruned(t *testing.T) {
	h := newHarness(func(c *Config) { c.MaxChats = 2 })
	h.b.mu.Lock()
	for u := int64(1); u <= 3; u++ {
		h.b.rateOK(u)
	}
	h.b.mu.Unlock()
	h.clock.Advance(2 * time.Hour)
	h.b.mu.Lock()
	h.b.rateOK(99)
	keys := []int64{}
	for k := range h.b.userHits {
		keys = append(keys, k)
	}
	h.b.mu.Unlock()
	if len(keys) != 1 || keys[0] != 99 {
		t.Fatalf("userHits keys = %v, want [99]", keys)
	}
}

// ---------- retries & failure handling ----------

func TestLLMRetryOn429ThenSuccess(t *testing.T) {
	h := newHarness(nil)
	h.llm.errs = []error{&LLMError{Status: 429, Kind: "http"}}
	h.b.handleUpdate(dm(1, 1, "hi"))
	waitSent(t, h, 1)
	if h.llm.calls() != 2 || len(h.sleeps.all()) != 1 {
		t.Fatalf("calls=%d sleeps=%v", h.llm.calls(), h.sleeps.all())
	}
}

func TestLLMNonRetryableNotRetried(t *testing.T) {
	h := newHarness(nil)
	h.llm.errs = []error{&LLMError{Status: 400, Kind: "http"}}
	h.b.handleUpdate(dm(1, 1, "hi"))
	waitFor(t, "llm call", func() bool { return h.llm.calls() == 1 })
	settle()
	if h.llm.calls() != 1 || len(h.tg.sends()) != 0 {
		t.Fatalf("calls=%d sends=%d", h.llm.calls(), len(h.tg.sends()))
	}
}

func TestLLMRetriesExhausted(t *testing.T) {
	h := newHarness(func(c *Config) { c.LLMRetries = 2 })
	h.llm.errs = []error{
		&LLMError{Status: 503, Kind: "http"},
		&LLMError{Status: 503, Kind: "http"},
		&LLMError{Status: 503, Kind: "http"},
	}
	h.b.handleUpdate(dm(1, 1, "hi"))
	waitFor(t, "3 attempts", func() bool { return h.llm.calls() == 3 })
	settle()
	if len(h.sleeps.all()) != 2 || len(h.tg.sends()) != 0 {
		t.Fatalf("sleeps=%v sends=%d", h.sleeps.all(), len(h.tg.sends()))
	}
}

func TestSendHonoursRetryAfter(t *testing.T) {
	h := newHarness(nil)
	h.tg.sendErrs = []error{&TGError{Method: "sendMessage", Code: 429, RetryAfter: 2}}
	h.b.handleUpdate(dm(1, 1, "hi"))
	waitFor(t, "two sendMessage attempts", func() bool { return len(h.tg.sends()) == 2 })
	found := false
	for _, d := range h.sleeps.all() {
		if d == 2*time.Second {
			found = true
		}
	}
	if !found {
		t.Fatalf("retry_after not honoured, sleeps=%v", h.sleeps.all())
	}
}

func TestReplyLengthCapped(t *testing.T) {
	h := newHarness(nil)
	h.llm.reply = strings.Repeat("a", 5000)
	h.b.handleUpdate(dm(1, 1, "hi"))
	waitSent(t, h, 1)
	if n := utf8.RuneCountInString(h.tg.sends()[0]["text"].(string)); n != 3500 {
		t.Fatalf("reply runes = %d, want 3500", n)
	}
}

func TestEmojiReplyStaysUnderTelegramLimit(t *testing.T) {
	h := newHarness(nil)
	h.llm.reply = strings.Repeat("😀", 3500)
	h.b.handleUpdate(dm(1, 1, "hi"))
	waitSent(t, h, 1)
	units := 0
	for _, r := range h.tg.sends()[0]["text"].(string) {
		if r >= 0x10000 {
			units += 2
		} else {
			units++
		}
	}
	if units > 4000 {
		t.Fatalf("utf16 units = %d, Telegram limit is 4096", units)
	}
}

func TestNonTextMessageGetsPlaceholder(t *testing.T) {
	h := newHarness(nil)
	h.b.handleUpdate(dm(1, 1, ""))
	waitFor(t, "llm call", func() bool { return h.llm.calls() == 1 })
	last := h.llm.lastSeen()
	if !strings.Contains(last[len(last)-1].Content, "không phải văn bản") {
		t.Fatalf("placeholder missing: %q", last[len(last)-1].Content)
	}
}

func TestWhitespaceOnlyTextDoesNotPanic(t *testing.T) {
	h := newHarness(nil)
	if got := h.b.handleUpdate(dm(testOwner, testOwner, "   ")); got != "queued" {
		t.Fatalf("got %q", got)
	}
	waitSent(t, h, 1)
}

func TestSafeHandleRecoversFromPanic(t *testing.T) {
	h := newHarness(nil)
	h.b.llm = &panicLLM{}
	if res := h.b.safeHandle(dm(1, 1, "hi")); res != "queued" {
		t.Fatalf("handle should not panic, got %q", res)
	}
	settle()
}

type panicLLM struct{}

func (panicLLM) Chat(ctx context.Context, msgs []ChatMsg) (string, error) { panic("boom") }

// ---------- state, security, helpers ----------

func TestPauseSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state.json")
	cfg := defaultConfig()
	cfg.OwnerID = testOwner
	cfg.OwnerUsername = "x"
	cfg.StateFile = state
	cfg.APIBase, cfg.APIKey = "http://fake", "k"

	b1 := NewBridge(cfg, &fakeTG{}, &fakeLLM{})
	b1.handleUpdate(dm(testOwner, testOwner, "/pause"))
	b2 := NewBridge(cfg, &fakeTG{}, &fakeLLM{})
	if !b2.paused {
		t.Fatal("pause not restored after restart")
	}
	info, err := os.Stat(state)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state file perms = %o, want 600", info.Mode().Perm())
	}
}

func TestTokenNeverAppearsInErrors(t *testing.T) {
	const secret = "999999:SUPERSECRETTOKEN"
	tg := NewTelegram("http://127.0.0.1:1", secret, &http.Client{})
	err := tg.Call(context.Background(), "getMe", nil, nil)
	if err == nil {
		t.Fatal("expected connection error")
	}
	if strings.Contains(err.Error(), "SUPERSECRET") || strings.Contains(err.Error(), "999999") {
		t.Fatalf("token leaked into error: %v", err)
	}
	var ne *NetError
	if !errors.As(err, &ne) {
		t.Fatalf("expected NetError, got %T", err)
	}
}

func TestTruncRunesKeepsCharacters(t *testing.T) {
	if got := truncRunes("Xin chào thế giới", 5); got != "Xin c" {
		t.Fatalf("got %q", got)
	}
	th := truncRunes("สวัสดีครับ", 3)
	if !utf8.ValidString(th) || utf8.RuneCountInString(th) != 3 {
		t.Fatalf("thai truncation broke characters: %q", th)
	}
	if truncRunes("ab", 0) != "" {
		t.Fatal("n=0 should be empty")
	}
}

func TestTruncUTF16(t *testing.T) {
	if got := truncUTF16("😀😀😀", 3); got != "😀" {
		t.Fatalf("got %q", got)
	}
	if got := truncUTF16("abc", 10); got != "abc" {
		t.Fatalf("got %q", got)
	}
}

func TestNormCmd(t *testing.T) {
	cases := map[string]string{
		"/Status":                "/status",
		"/pause@example_bot now": "/pause",
		"   ":                    "",
		"":                       "",
		"/start":                 "/start",
	}
	for in, want := range cases {
		if got := normCmd(in); got != want {
			t.Errorf("normCmd(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadConfigValidation(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	os.WriteFile(p, []byte(`{"bot_token":"","owner_id":1,"api_base":"x","api_key":"y"}`), 0o600)
	if _, err := loadConfig(p); err == nil {
		t.Fatal("missing bot_token should fail")
	}
	os.WriteFile(p, []byte(`{"bot_token":"t","owner_id":1,"api_base":"x","api_key":"y","max_concurrent_llm":0}`), 0o600)
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxConcurrentLLM != 1 || cfg.OwnerUsername != "owner" ||
		cfg.SystemPromptFile != filepath.Join(dir, "system_prompt.txt") {
		t.Fatalf("defaults/clamps wrong: %+v", cfg)
	}
}

func TestMessageDateUnmarshalled(t *testing.T) {
	var m Message
	body := []byte(`{"date":1731180000,"chat":{"id":1,"type":"private"},"from":{"id":7},"text":"x"}`)
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if m.Date != 1731180000 {
		t.Fatalf("date = %d", m.Date)
	}
}
