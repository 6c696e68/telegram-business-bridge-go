package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	bolt "go.etcd.io/bbolt"
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
	mu          sync.Mutex
	calls       []tgCall
	conns       map[string]BusinessConnection
	sendErrs    []error
	files       map[string][]byte // file_id -> content for DownloadFile
	downloads   []string
	downloadErr error
}

// fakePNG is enough for http.DetectContentType to say image/png.
var fakePNG = append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)

func (f *fakeTG) DownloadFile(ctx context.Context, fileID string, max int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.downloads = append(f.downloads, fileID)
	if f.downloadErr != nil {
		return nil, f.downloadErr
	}
	if data, ok := f.files[fileID]; ok {
		if int64(len(data)) > max {
			return nil, &NetError{Method: "file", Kind: "too_large"}
		}
		return data, nil
	}
	return fakePNG, nil
}

func (f *fakeTG) downloaded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.downloads...)
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
	replies  []string
	callIdx  int
	fn       func(msgs []ChatMsg) (string, bool)
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
	if len(f.errs) > 0 {
		e := f.errs[0]
		f.errs = f.errs[1:]
		f.mu.Unlock()
		return "", e
	}
	fn := f.fn
	reply := f.reply
	queued := false
	if f.callIdx < len(f.replies) {
		reply = f.replies[f.callIdx]
		f.callIdx++
		queued = true
	}
	f.mu.Unlock()
	// fn runs outside the lock: tests may block it to simulate a slow distill.
	if fn != nil {
		if out, ok := fn(msgs); ok {
			return out, nil
		}
	}
	if queued || reply != "" {
		return reply, nil
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
	cfg.DebounceMS = 0 // tests opt in explicitly; keeps the suite fast
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
	// The owner being active anywhere silences every business chat.
	if got := h.b.handleUpdate(biz(testConn, 333, 333, "hi")); got != "owner_online" {
		t.Fatalf("other chat got %q", got)
	}
	settle()
	if n := len(h.tg.sends()); n != 0 {
		t.Fatalf("no business reply may be sent while the owner is online, got %d", n)
	}
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
		ReplyToMessage: &Message{From: &User{ID: 999}}}}); got != "queued" {
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
	if got := h.b.handleUpdate(dm(4242, 4242, "   ")); got != "queued" {
		t.Fatalf("customer got %q", got)
	}
	waitSent(t, h, 1)
	// The owner's private chat is a control panel: blank text gets the hint.
	if got := h.b.handleUpdate(dm(testOwner, testOwner, "   ")); got != "owner_cmd" {
		t.Fatalf("owner got %q", got)
	}
	waitSent(t, h, 2)
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

func TestBacklogMessagesAreSkipped(t *testing.T) {
	h := newHarness(nil)
	old := biz(testConn, 800, 800, "hello")
	old.BusinessMessage.Date = h.clock.Now().Add(-2 * time.Hour).Unix()
	if got := h.b.handleUpdate(old); got != "stale" {
		t.Fatalf("got %q", got)
	}
	if h.llm.calls() != 0 {
		t.Fatal("LLM must not be called for stale messages")
	}
	h.b.mu.Lock()
	quota := h.b.dayCount
	h.b.mu.Unlock()
	if quota != 0 {
		t.Fatal("stale messages must not consume quota")
	}
	fresh := biz(testConn, 801, 801, "hi")
	fresh.BusinessMessage.Date = h.clock.Now().Unix()
	if got := h.b.handleUpdate(fresh); got != "queued" {
		t.Fatalf("got %q", got)
	}
	waitSent(t, h, 1)
}

func TestBacklogDisabledAllowsOldMessages(t *testing.T) {
	h := newHarness(func(c *Config) { c.MaxBacklogMin = 0 })
	old := biz(testConn, 802, 802, "hello")
	old.BusinessMessage.Date = h.clock.Now().Add(-24 * time.Hour).Unix()
	if got := h.b.handleUpdate(old); got != "queued" {
		t.Fatalf("got %q", got)
	}
}

// ---------- rules loop ----------

func rulesHarness(t *testing.T, mut func(*Config)) (*harness, string) {
	dir := t.TempDir()
	rf := filepath.Join(dir, "rules.md")
	if mut == nil {
		mut = func(*Config) {}
	}
	h := newHarness(func(c *Config) {
		c.RulesFile = rf
		c.RulesUpdateEvery = 2
		mut(c)
	})
	return h, rf
}

func TestRulesInjectionAndDistill(t *testing.T) {
	h, rf := rulesHarness(t, nil)
	h.llm.fn = func(msgs []ChatMsg) (string, bool) {
		if strings.HasPrefix(msgs[0].Content, "You maintain the long-term rules") {
			return "- Khách hay hỏi về bảo hành sản phẩm.", true
		}
		return "Dạ em trả lời", false
	}
	h.b.handleUpdate(dm(600, 600, "msg1"))
	waitSent(t, h, 1)
	h.b.handleUpdate(dm(600, 600, "msg2"))
	waitSent(t, h, 2)
	waitFor(t, "rules file", func() bool { _, err := os.Stat(rf); return err == nil })
	data, err := os.ReadFile(rf)
	if err != nil || !strings.Contains(string(data), "bảo hành") {
		t.Fatalf("rules file wrong: %v %q", err, data)
	}
	// the file is written first, the in-memory copy right after it
	waitFor(t, "rulesText updated", func() bool {
		h.b.mu.Lock()
		defer h.b.mu.Unlock()
		return h.b.rulesText != ""
	})
	// next reply must inject the rules block
	h.b.handleUpdate(dm(600, 600, "msg3"))
	waitSent(t, h, 3)
	h.llm.mu.Lock()
	injected := false
	for _, seen := range h.llm.seen {
		if len(seen) > 0 && strings.Contains(seen[0].Content, "[Long-term rules]") {
			injected = true
			break
		}
	}
	h.llm.mu.Unlock()
	if !injected {
		t.Fatal("rules block not injected into system prompt")
	}
}

func TestRulesDistillWaitsForThreshold(t *testing.T) {
	h, rf := rulesHarness(t, nil)
	h.llm.fn = func(msgs []ChatMsg) (string, bool) {
		if strings.HasPrefix(msgs[0].Content, "You maintain the long-term rules") {
			return "- Rule.", true
		}
		return "ok", false
	}
	h.b.handleUpdate(dm(601, 601, "one"))
	waitSent(t, h, 1)
	settle()
	if _, err := os.Stat(rf); err == nil {
		t.Fatal("distill must not fire before rules_update_every replies")
	}
}

func TestRulesOffStopsDistill(t *testing.T) {
	h, rf := rulesHarness(t, nil)
	h.llm.fn = func(msgs []ChatMsg) (string, bool) {
		if strings.HasPrefix(msgs[0].Content, "You maintain the long-term rules") {
			return "- Rule.", true
		}
		return "ok", false
	}
	h.b.handleUpdate(dm(testOwner, testOwner, "/rules off"))
	settle()
	for i := 0; i < 4; i++ {
		h.b.handleUpdate(dm(602, 602, "x"))
	}
	settle()
	if _, err := os.Stat(rf); err == nil {
		t.Fatal("distill must not run while rules are off")
	}
	h.b.mu.Lock()
	on := h.b.rulesOn()
	h.b.mu.Unlock()
	if on {
		t.Fatal("rulesOn must be false after /rules off")
	}
}

func TestRulesVerbatimQuoteRejected(t *testing.T) {
	h, rf := rulesHarness(t, nil)
	h.llm.fn = func(msgs []ChatMsg) (string, bool) {
		if strings.HasPrefix(msgs[0].Content, "You maintain the long-term rules") {
			return "- " + strings.Repeat("A", 40), true
		}
		return "ok", false
	}
	longText := strings.Repeat("A", 120) + " " + strings.Repeat("B", 120)
	h.b.handleUpdate(dm(603, 603, longText))
	waitSent(t, h, 1)
	h.b.handleUpdate(dm(603, 603, "second"))
	waitSent(t, h, 2)
	settle()
	if _, err := os.Stat(rf); err == nil {
		t.Fatal("verbatim quote must not be persisted")
	}
}

func TestRulesViewAndReset(t *testing.T) {
	h, rf := rulesHarness(t, nil)
	if err := os.WriteFile(rf, []byte("- Rule A"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.b.loadRules()
	h.b.handleUpdate(dm(testOwner, testOwner, "/rules"))
	waitSent(t, h, 1)
	sends := h.tg.sends()
	if !strings.Contains(sends[0]["text"].(string), "Rule A") {
		t.Fatalf("rules view missing content: %v", sends[0]["text"])
	}
	h.b.handleUpdate(dm(testOwner, testOwner, "/rules reset"))
	waitSent(t, h, 2)
	if _, err := os.Stat(rf); err == nil {
		t.Fatal("rules reset must delete the file")
	}
	h.b.mu.Lock()
	rt := h.b.rulesText
	h.b.mu.Unlock()
	if rt != "" {
		t.Fatalf("rulesText must be cleared, got %q", rt)
	}
}

func TestOwnerCommandInsideBusinessChat(t *testing.T) {
	h := newHarness(nil)
	got := h.b.handleUpdate(biz(testConn, 2000, testOwner, "/status"))
	if got != "owner_cmd" {
		t.Fatalf("got %q", got)
	}
	sends := h.tg.sends()
	if len(sends) == 0 || sends[0]["business_connection_id"] != testConn {
		t.Fatal("reply must go through the business connection")
	}
}

func TestForgetClearsChat(t *testing.T) {
	dir := t.TempDir()
	h := newHarness(func(c *Config) {
		c.DBFile = filepath.Join(dir, "bridge.db")
		c.OwnerAwayMinutes = 0 // presence is covered by presence_test.go
	})
	defer func() { _ = h.b.Close() }()
	h.b.handleUpdate(biz(testConn, 901, 901, "hi"))
	waitSent(t, h, 1)
	settle()
	key := "b:" + testConn + ":901"
	h.b.mu.Lock()
	_, ok := h.b.chats[key]
	h.b.mu.Unlock()
	if !ok {
		t.Fatal("chat must exist before forget")
	}
	if got := h.b.handleUpdate(biz(testConn, 901, testOwner, "/forget")); got != "owner_cmd" {
		t.Fatalf("got %q", got)
	}
	h.b.mu.Lock()
	_, still := h.b.chats[key]
	_, deleted := h.b.deletedChats[key]
	h.b.mu.Unlock()
	if still {
		t.Fatal("chat not removed from RAM")
	}
	if !deleted {
		t.Fatal("delete not queued for DB")
	}
	if err := h.b.flushNow(); err != nil {
		t.Fatal(err)
	}
	found := false
	_ = h.b.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket(bucketChats).Get([]byte(key)) != nil {
			found = true
		}
		return nil
	})
	if found {
		t.Fatal("forgotten chat resurrected on disk")
	}
}

func TestOwnerTextStillMutesChat(t *testing.T) {
	h := newHarness(nil)
	if got := h.b.handleUpdate(biz(testConn, 902, testOwner, "mình tự trả lời nha")); got != "owner_takeover" {
		t.Fatalf("got %q", got)
	}
	if got := h.b.handleUpdate(biz(testConn, 902, 902, "hello?")); got != "muted" {
		t.Fatalf("got %q", got)
	}
}

func TestForgetInDirectChat(t *testing.T) {
	h := newHarness(nil)
	h.b.handleUpdate(dm(777, 777, "hello"))
	waitSent(t, h, 1)
	settle()
	if got := h.b.handleUpdate(dm(testOwner, testOwner, "/forget")); got != "owner_cmd" {
		t.Fatalf("got %q", got)
	}
	h.b.mu.Lock()
	_, still := h.b.chats["d:"+strconv.FormatInt(testOwner, 10)]
	h.b.mu.Unlock()
	if still {
		t.Fatal("owner DM context not cleared")
	}
}

func TestDistillSingleFlight(t *testing.T) {
	h, rf := rulesHarness(t, nil)
	var mu sync.Mutex
	calls := 0
	gate := make(chan struct{})
	h.llm.fn = func(msgs []ChatMsg) (string, bool) {
		if strings.HasPrefix(msgs[0].Content, "You maintain the long-term rules") {
			mu.Lock()
			calls++
			mu.Unlock()
			<-gate
			return "- Rule once.", true
		}
		return "Được", false
	}
	for i := 0; i < 4; i++ {
		h.b.handleUpdate(dm(610, 610, "msg"))
		waitSent(t, h, i+1)
	}
	waitFor(t, "first distill started", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return calls >= 1
	})
	settle() // let the second threshold fire while the first job is blocked
	close(gate)
	waitFor(t, "rules file", func() bool { _, err := os.Stat(rf); return err == nil })
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Fatalf("distill must be single-flight, ran %d times", got)
	}
}

func TestDistillAbortedByReset(t *testing.T) {
	h, rf := rulesHarness(t, nil)
	gate := make(chan struct{})
	started := make(chan struct{}, 1)
	h.llm.fn = func(msgs []ChatMsg) (string, bool) {
		if strings.HasPrefix(msgs[0].Content, "You maintain the long-term rules") {
			select {
			case started <- struct{}{}:
			default:
			}
			<-gate
			return "- Should not persist.", true
		}
		return "ok", false
	}
	h.b.handleUpdate(dm(611, 611, "m1"))
	waitSent(t, h, 1)
	h.b.handleUpdate(dm(611, 611, "m2"))
	waitSent(t, h, 2)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("distill never started")
	}
	h.b.handleUpdate(dm(testOwner, testOwner, "/rules reset"))
	close(gate)
	settle()
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(rf); err == nil {
		t.Fatal("stale distill must not write after /rules reset")
	}
	h.b.mu.Lock()
	rt := h.b.rulesText
	h.b.mu.Unlock()
	if rt != "" {
		t.Fatalf("rulesText must stay cleared, got %q", rt)
	}
}

func TestEvictedChatStillFlushed(t *testing.T) {
	dir := t.TempDir()
	h := newHarness(func(c *Config) {
		c.DBFile = filepath.Join(dir, "bridge.db")
		c.MaxChats = 1
	})
	defer func() { _ = h.b.Close() }()
	h.b.handleUpdate(dm(1001, 1001, "first chat message"))
	waitSent(t, h, 1)
	h.b.handleUpdate(dm(1002, 1002, "second chat message"))
	waitSent(t, h, 2)
	settle()
	h.b.mu.Lock()
	_, inRAM := h.b.chats["d:1001"]
	h.b.mu.Unlock()
	if inRAM {
		t.Fatal("expected first chat evicted from RAM")
	}
	if err := h.b.flushNow(); err != nil {
		t.Fatal(err)
	}
	if n := countBucket(t, h.b, bucketChats); n != 2 {
		t.Fatalf("evicted chat lost on disk: %d records, want 2", n)
	}
}
