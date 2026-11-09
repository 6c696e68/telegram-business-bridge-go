package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDebounceCoalescesBurst(t *testing.T) {
	h := newHarness(func(c *Config) { c.DebounceMS = 150 })
	// the harness normally stubs sleep for speed; this test needs real timing
	h.b.sleep = func(ctx context.Context, d time.Duration) {
		select {
		case <-time.After(d):
		case <-ctx.Done():
		}
	}
	h.b.handleUpdate(dm(630, 630, "một"))
	h.b.handleUpdate(dm(630, 630, "hai"))
	h.b.handleUpdate(dm(630, 630, "ba"))
	waitSent(t, h, 1)
	time.Sleep(300 * time.Millisecond)
	if n := len(h.tg.sends()); n != 1 {
		t.Fatalf("a burst must yield exactly one reply, got %d", n)
	}
	h.b.mu.Lock()
	accepted, sent := h.b.dayCount, h.b.repliesSent
	h.b.mu.Unlock()
	if accepted != 3 || sent != 1 {
		t.Fatalf("counter drift: accepted=%d replies=%d, want 3/1", accepted, sent)
	}
	h.llm.mu.Lock()
	defer h.llm.mu.Unlock()
	if len(h.llm.seen) == 0 {
		t.Fatal("llm never called")
	}
	joined := ""
	for _, m := range h.llm.seen[0] {
		joined += m.Content + "\n"
	}
	for _, want := range []string{"một", "hai", "ba"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("merged context missing %q", want)
		}
	}
}

func TestDebounceDisabledKeepsPerMessageReplies(t *testing.T) {
	h := newHarness(func(c *Config) { c.DebounceMS = 0 })
	h.b.handleUpdate(dm(631, 631, "một"))
	waitSent(t, h, 1)
	h.b.handleUpdate(dm(631, 631, "hai"))
	waitSent(t, h, 2)
}

func TestStatsCommand(t *testing.T) {
	h := newHarness(nil)
	h.b.handleUpdate(dm(testOwner, testOwner, "/stats"))
	waitSent(t, h, 1)
	text, _ := h.tg.sends()[0]["text"].(string)
	for _, want := range []string{"RSS", "DB", "Uptime", "Rules:"} {
		if !strings.Contains(text, want) {
			t.Fatalf("stats output missing %q: %q", want, text)
		}
	}
}

func TestLLMRequestOptions(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	llm := NewLLM(srv.URL, "k", "m", srv.Client())
	llm.MaxTokens = 321
	llm.Temperature = 0.3
	if _, err := llm.Chat(context.Background(), []ChatMsg{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if bodies[0]["max_tokens"].(float64) != 321 {
		t.Fatalf("max_tokens = %v", bodies[0]["max_tokens"])
	}
	if bodies[0]["temperature"].(float64) != 0.3 {
		t.Fatalf("temperature = %v", bodies[0]["temperature"])
	}
}

func TestLLMDefaultOptionsOmitTemperature(t *testing.T) {
	var mu sync.Mutex
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Unlock()
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()
	if _, err := NewLLM(srv.URL, "k", "m", srv.Client()).Chat(context.Background(), []ChatMsg{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if body["max_tokens"].(float64) != 1200 {
		t.Fatalf("default max_tokens = %v", body["max_tokens"])
	}
	if _, ok := body["temperature"]; ok {
		t.Fatal("temperature must be omitted at its zero value")
	}
}

func TestConfigReplyTuning(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	body := `{"bot_token":"1:x","owner_id":1,"api_base":"http://x","api_key":"k",` +
		`"debounce_ms":-1,"max_tokens":1,"temperature":5}`
	if err := writeTemp(p, body); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DebounceMS != 0 || cfg.Temperature != 2 {
		t.Fatalf("clamps applied wrong: %+v", cfg)
	}
	// small explicit caps are raised to the floor; 0 means "use the default"
	if cfg.MaxTokens != 16 {
		t.Fatalf("max_tokens floor wrong: %d", cfg.MaxTokens)
	}

	cfg2, err := loadConfigWithDefaults(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.DebounceMS != 700 || cfg2.MaxTokens != 1200 || cfg2.Temperature != 0 {
		t.Fatalf("defaults wrong: debounce=%d max_tokens=%d temp=%v",
			cfg2.DebounceMS, cfg2.MaxTokens, cfg2.Temperature)
	}
}

func writeTemp(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o600)
}

func TestTakeoverDuringDebounceSuppressesReply(t *testing.T) {
	h := newHarness(func(c *Config) { c.DebounceMS = 80 })
	h.b.sleep = func(ctx context.Context, d time.Duration) {
		select {
		case <-time.After(d):
		case <-ctx.Done():
		}
	}
	// Customer message starts the debounce; the owner types immediately after.
	h.b.handleUpdate(biz(testConn, 640, 640, "khách hỏi"))
	h.b.handleUpdate(biz(testConn, 640, testOwner, "để tôi trả lời"))
	time.Sleep(200 * time.Millisecond)
	if n := len(h.tg.sends()); n != 0 {
		t.Fatalf("reply must be suppressed during takeover, got %d sends", n)
	}
	// Still muted: another customer message must not reply.
	if got := h.b.handleUpdate(biz(testConn, 640, 640, "còn đó không?")); got != "muted" {
		t.Fatalf("got %q", got)
	}
	// /resume hands over: the question left waiting during the takeover gets
	// exactly one follow-up reply, then new messages are answered normally.
	h.b.handleUpdate(dm(testOwner, testOwner, "/resume"))
	waitSent(t, h, 2) // the /resume confirmation + the follow-up
	time.Sleep(150 * time.Millisecond)
	followups := 0
	for _, snd := range h.tg.sends() {
		if snd["business_connection_id"] == testConn {
			followups++
		}
	}
	if followups != 1 {
		t.Fatalf("expected exactly one follow-up after resume, got %d", followups)
	}
	h.b.handleUpdate(biz(testConn, 640, 640, "alo"))
	waitSent(t, h, 3)
	time.Sleep(150 * time.Millisecond)
	if n := len(h.tg.sends()); n != 3 {
		t.Fatalf("expected one more reply for the new message, got %d sends", n)
	}
}
