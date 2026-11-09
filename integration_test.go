package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const integToken = "123:TESTTOKEN"

// fakeTelegramServer speaks the Bot API over real HTTP on loopback.
type fakeTelegramServer struct {
	*httptest.Server
	mu        sync.Mutex
	sends     []map[string]any
	offsets   []float64
	getUpd    int
	chatAct   int
	first     []string // raw JSON updates for the first getUpdates
	firstErr  string   // if set, first getUpdates returns this error body
	sendDelay time.Duration
}

func newFakeTelegram(t *testing.T, first []string) *fakeTelegramServer {
	f := &fakeTelegramServer{first: first}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/bot"+integToken+"/") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		write := func(s string) { _, _ = io.WriteString(w, s) }
		switch method {
		case "getMe":
			write(`{"ok":true,"result":{"id":999,"username":"example_bot"}}`)
		case "deleteWebhook", "sendChatAction":
			f.mu.Lock()
			if method == "sendChatAction" {
				f.chatAct++
			}
			f.mu.Unlock()
			write(`{"ok":true,"result":true}`)
		case "getBusinessConnection":
			write(`{"ok":true,"result":{"id":"c1","user":{"id":1000000001},"is_enabled":true,"rights":{"can_reply":true}}}`)
		case "sendMessage":
			if f.sendDelay > 0 {
				time.Sleep(f.sendDelay)
			}
			f.mu.Lock()
			f.sends = append(f.sends, p)
			f.mu.Unlock()
			write(`{"ok":true,"result":{"message_id":1}}`)
		case "getUpdates":
			f.mu.Lock()
			f.getUpd++
			n := f.getUpd
			if off, ok := p["offset"].(float64); ok {
				f.offsets = append(f.offsets, off)
			}
			firstErr := f.firstErr
			f.mu.Unlock()
			if n == 1 && firstErr != "" {
				write(firstErr)
				return
			}
			if n == 1 || (n == 2 && firstErr != "") {
				if len(f.first) > 0 {
					write(`{"ok":true,"result":[` + strings.Join(f.first, ",") + `]}`)
					return
				}
			}
			// Long-poll stand-in: wait briefly, then return nothing.
			select {
			case <-r.Context().Done():
			case <-time.After(30 * time.Millisecond):
			}
			write(`{"ok":true,"result":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeTelegramServer) sentSnapshot() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.sends...)
}

func (f *fakeTelegramServer) getUpdatesCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getUpd
}

type fakeLLMServer struct {
	*httptest.Server
	mu       sync.Mutex
	auth     []string
	statuses []int // responses to give in order before success
	delay    time.Duration
	inflight int
	maxIn    int
}

func (f *fakeLLMServer) maxInflight() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxIn
}

func newFakeLLM(t *testing.T, statuses []int) *fakeLLMServer {
	f := &fakeLLMServer{statuses: statuses}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		_, _ = io.ReadAll(r.Body)
		f.mu.Lock()
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		code := http.StatusOK
		if len(f.statuses) > 0 {
			code = f.statuses[0]
			f.statuses = f.statuses[1:]
		}
		delay := f.delay
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
		if delay > 0 {
			time.Sleep(delay)
		}
		if code != http.StatusOK {
			w.WriteHeader(code)
			_, _ = io.WriteString(w, `{"error":{"message":"busy"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"Dạ em trả lời"}}]}`)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeLLMServer) authHeaders() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.auth...)
}

func integConfig(tgURL, llmURL string) Config {
	cfg := defaultConfig()
	cfg.BotToken = integToken
	cfg.TelegramBase = tgURL
	cfg.APIBase = llmURL + "/v1"
	cfg.APIKey = "sk-test-KEY"
	cfg.OwnerID = testOwner
	cfg.OwnerUsername = "example_owner"
	return cfg
}

const bizUpdates1 = `{"update_id":101,"business_connection":{"id":"c1","user":{"id":1000000001},"is_enabled":true,"rights":{"can_reply":true}}}`
const bizUpdates2 = `{"update_id":102,"business_message":{"business_connection_id":"c1","chat":{"id":555,"type":"private"},"from":{"id":555,"is_bot":false},"text":"alo anh có đó ko"}}`

// ---------- end-to-end over real HTTP ----------

func TestIntegrationPollLoopEndToEnd(t *testing.T) {
	tg := newFakeTelegram(t, []string{bizUpdates1, bizUpdates2})
	llm := newFakeLLM(t, nil)
	cfg := integConfig(tg.URL, llm.URL)
	b := NewBridge(cfg, NewTelegram(cfg.TelegramBase, cfg.BotToken, newHTTPClient(8)),
		NewLLM(cfg.APIBase, cfg.APIKey, cfg.Model, newHTTPClient(8)))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()

	waitFor(t, "sendMessage over HTTP", func() bool { return len(tg.sentSnapshot()) == 1 })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned error on shutdown: %v", err)
	}

	s := tg.sentSnapshot()[0]
	if s["business_connection_id"] != "c1" || s["chat_id"].(float64) != 555 {
		t.Fatalf("bad send: %v", s)
	}
	if txt, _ := s["text"].(string); txt == "" {
		t.Fatal("empty reply text")
	}
	for _, a := range llm.authHeaders() {
		if a != "Bearer sk-test-KEY" {
			t.Fatalf("LLM auth header wrong (len %d)", len(a))
		}
	}
	tg.mu.Lock()
	offsets := append([]float64(nil), tg.offsets...)
	tg.mu.Unlock()
	if len(offsets) < 1 || offsets[len(offsets)-1] != 103 {
		t.Fatalf("offset not advanced past update 102: %v", offsets)
	}
}

func TestIntegration409IsRecovered(t *testing.T) {
	tg := newFakeTelegram(t, []string{bizUpdates1, bizUpdates2})
	tg.firstErr = `{"ok":false,"error_code":409,"description":"Conflict: terminated by other getUpdates request"}`
	llm := newFakeLLM(t, nil)
	cfg := integConfig(tg.URL, llm.URL)
	sl := &sleepLog{}
	b := NewBridge(cfg, NewTelegram(cfg.TelegramBase, cfg.BotToken, newHTTPClient(8)),
		NewLLM(cfg.APIBase, cfg.APIKey, cfg.Model, newHTTPClient(8)))
	b.sleep = sl.record

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()
	waitFor(t, "reply after 409", func() bool { return len(tg.sentSnapshot()) == 1 })
	cancel()
	<-done

	found := false
	for _, d := range sl.all() {
		if d == 15*time.Second {
			found = true
		}
	}
	if !found {
		t.Fatalf("409 did not back off 15s, sleeps=%v", sl.all())
	}
}

func TestIntegrationBadTokenFailsFast(t *testing.T) {
	tg := newFakeTelegram(t, nil)
	cfg := integConfig(tg.URL, "http://127.0.0.1:1")
	cfg.BotToken = "999:WRONG"
	b := NewBridge(cfg, NewTelegram(cfg.TelegramBase, cfg.BotToken, newHTTPClient(2)),
		NewLLM(cfg.APIBase, cfg.APIKey, cfg.Model, newHTTPClient(2)))
	err := b.Run(context.Background())
	if err == nil || strings.Contains(err.Error(), "WRONG") {
		t.Fatalf("want error without token in it, got %v", err)
	}
}

// ---------- real HTTP LLM client ----------

func TestLLMRealHTTPRetryThenSuccess(t *testing.T) {
	llm := newFakeLLM(t, []int{http.StatusTooManyRequests, http.StatusBadGateway})
	cfg := integConfig("http://unused", llm.URL)
	sl := &sleepLog{}
	b := NewBridge(cfg, &fakeTG{}, NewLLM(cfg.APIBase, cfg.APIKey, cfg.Model, newHTTPClient(4)))
	b.sleep = sl.record
	reply, err := b.callLLM(context.Background(), []ChatMsg{{Role: "user", Content: "hi"}})
	if err != nil || reply != "Dạ em trả lời" {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
	if n := len(llm.authHeaders()); n != 3 {
		t.Fatalf("requests = %d, want 3", n)
	}
	if got := sl.all(); len(got) != 2 || got[0] != 3*time.Second || got[1] != 6*time.Second {
		t.Fatalf("backoff = %v", got)
	}
}

func TestLLMRealHTTPBadRequestNotRetried(t *testing.T) {
	llm := newFakeLLM(t, []int{http.StatusBadRequest})
	cfg := integConfig("http://unused", llm.URL)
	b := NewBridge(cfg, &fakeTG{}, NewLLM(cfg.APIBase, cfg.APIKey, cfg.Model, newHTTPClient(4)))
	_, err := b.callLLM(context.Background(), []ChatMsg{{Role: "user", Content: "hi"}})
	var le *LLMError
	if err == nil || !errors.As(err, &le) || le.Status != 400 {
		t.Fatalf("err=%v", err)
	}
	if n := len(llm.authHeaders()); n != 1 {
		t.Fatalf("400 was retried: %d requests", n)
	}
}

func TestLLMTimeoutIsRetriedAndSanitized(t *testing.T) {
	llm := newFakeLLM(t, nil)
	llm.delay = 200 * time.Millisecond
	cfg := integConfig("http://unused", llm.URL)
	cfg.LLMTimeoutSec = 1
	cfg.LLMRetries = 0
	b := NewBridge(cfg, &fakeTG{}, NewLLM(cfg.APIBase, cfg.APIKey, cfg.Model, newHTTPClient(4)))
	b.sleep = func(context.Context, time.Duration) {}
	llm.delay = 1500 * time.Millisecond
	_, err := b.callLLM(context.Background(), []ChatMsg{{Role: "user", Content: "hi"}})
	if err == nil {
		t.Fatal("expected timeout")
	}
	if strings.Contains(err.Error(), "sk-test") || strings.Contains(err.Error(), "http") {
		t.Fatalf("error leaks details: %v", err)
	}
}

func TestTelegramBadJSONAnswerIsHandled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "<html>bad gateway</html>")
	}))
	defer srv.Close()
	tg := NewTelegram(srv.URL, "1:x", newHTTPClient(2))
	err := tg.Call(context.Background(), "getMe", nil, nil)
	var te *TGError
	if !errors.As(err, &te) || te.Code != 502 {
		t.Fatalf("err=%v", err)
	}
}

// ---------- fuzzing: arbitrary updates must never panic ----------

func FuzzHandleUpdate(f *testing.F) {
	f.Add([]byte(bizUpdates1))
	f.Add([]byte(bizUpdates2))
	f.Add([]byte(`{"update_id":1,"message":{"chat":{"id":1,"type":"group"},"text":"@example_bot"}}`))
	f.Add([]byte(`{"update_id":2,"business_message":{"business_connection_id":"c1","chat":{"id":1,"type":"private"},"text":"/pause"}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var u Update
		if json.Unmarshal(data, &u) != nil {
			return
		}
		h := newHarness(nil)
		h.b.safeHandle(u)
		settle()
	})
}

// ---------- load test: go test -run TestLoad -v with LOAD_N=600 ----------

func TestLoad(t *testing.T) {
	if os.Getenv("LOAD_N") == "" {
		t.Skip("set LOAD_N to run the load test")
	}
	n := 0
	fmt.Sscanf(os.Getenv("LOAD_N"), "%d", &n)
	llmDelay := 1 * time.Second

	tg := newFakeTelegram(t, nil)
	llm := newFakeLLM(t, nil)
	llm.delay = llmDelay
	cfg := integConfig(tg.URL, llm.URL)
	cfg.DailyReplyCap = 1000000
	cfg.PerUserPerHour = 100
	cfg.MaxConcurrentLLM = 8
	cfg.MaxChats = 500
	b := NewBridge(cfg, NewTelegram(cfg.TelegramBase, cfg.BotToken, newHTTPClient(32)),
		NewLLM(cfg.APIBase, cfg.APIKey, cfg.Model, newHTTPClient(32)))

	t0 := time.Now()
	for i := 0; i < n; i++ {
		b.handleUpdate(dm(int64(10000+i), int64(10000+i), strings.Repeat("hello ", 50)))
	}
	dispatch := time.Since(t0)
	waitFor2 := time.Now().Add(10 * time.Minute)
	for len(tg.sentSnapshot()) < n && time.Now().Before(waitFor2) {
		time.Sleep(100 * time.Millisecond)
	}
	elapsed := time.Since(t0)
	expected := time.Duration((n+cfg.MaxConcurrentLLM-1)/cfg.MaxConcurrentLLM) * llmDelay

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fmt.Printf("LOAD N=%d dispatch=%v total=%v expected~%v sent=%d llm_max_concurrent=%d chats_kept=%d goroutines=%d heap_inuse=%dKB sys=%dKB\n",
		n, dispatch.Round(time.Millisecond), elapsed.Round(time.Millisecond), expected.Round(time.Second),
		len(tg.sentSnapshot()), llm.maxInflight(), len(b.chatKeys()), runtime.NumGoroutine(),
		ms.HeapInuse/1024, ms.Sys/1024)
	if rss := procRSS(); rss != "" {
		fmt.Println("LOAD VmHWM/VmRSS:", rss)
	}
	if len(tg.sentSnapshot()) != n {
		t.Fatalf("only %d of %d replies sent", len(tg.sentSnapshot()), n)
	}
	if m := llm.maxInflight(); m > cfg.MaxConcurrentLLM {
		t.Fatalf("concurrency cap violated: %d", m)
	}
}

func procRSS() string {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return ""
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VmHWM:") || strings.HasPrefix(line, "VmRSS:") {
			out = append(out, strings.Join(strings.Fields(line), " "))
		}
	}
	return strings.Join(out, " | ")
}
