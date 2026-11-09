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

// modelAwareServer records the model of each request and answers per-model.
func modelAwareServer(t *testing.T, answer func(model string) (int, string)) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		seen = append(seen, req.Model)
		mu.Unlock()
		code, body := answer(req.Model)
		w.WriteHeader(code)
		if code == http.StatusOK {
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"`+body+`"}}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"error":"boom"}`)
	}))
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func TestLLMFallbackOnRetryableStatus(t *testing.T) {
	srv, seen := modelAwareServer(t, func(model string) (int, string) {
		if model == "primary" {
			return http.StatusTooManyRequests, ""
		}
		return http.StatusOK, "từ model dự phòng"
	})
	defer srv.Close()

	llm := NewLLMChain(srv.URL, "k", []string{"primary", "backup"}, srv.Client())
	reply, err := llm.Chat(context.Background(), []ChatMsg{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if reply != "từ model dự phòng" {
		t.Fatalf("reply = %q", reply)
	}
	models := seen()
	if len(models) != 2 || models[0] != "primary" || models[1] != "backup" {
		t.Fatalf("fallback order wrong: %v", models)
	}
}

func TestLLMFallbackSkippedOnConfigError(t *testing.T) {
	srv, seen := modelAwareServer(t, func(model string) (int, string) {
		return http.StatusBadRequest, ""
	})
	defer srv.Close()

	llm := NewLLMChain(srv.URL, "k", []string{"primary", "backup"}, srv.Client())
	if _, err := llm.Chat(context.Background(), []ChatMsg{{Role: "user", Content: "hi"}}); err == nil {
		t.Fatal("expected error")
	}
	if models := seen(); len(models) != 1 {
		t.Fatalf("non-retryable errors must not walk the chain: %v", models)
	}
}

func TestLLMFallbackKeepsSingleModelBehaviour(t *testing.T) {
	srv, seen := modelAwareServer(t, func(model string) (int, string) {
		return http.StatusOK, "ok"
	})
	defer srv.Close()
	llm := NewLLM(srv.URL, "k", "only-model", srv.Client())
	if _, err := llm.Chat(context.Background(), []ChatMsg{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatal(err)
	}
	if models := seen(); len(models) != 1 || models[0] != "only-model" {
		t.Fatalf("models = %v", models)
	}
}

func TestConfigModelChain(t *testing.T) {
	cfg := defaultConfig()
	cfg.Model = "a"
	cfg.FallbackModels = []string{"b", "a", "", "c"}
	got := strings.Join(cfg.modelChain(), ",")
	if got != "a,b,c" {
		t.Fatalf("chain = %q", got)
	}
}

func TestConfigDrainClamp(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	body := `{"bot_token":"1:x","owner_id":1,"api_base":"http://x","api_key":"k","shutdown_drain_sec":-5,"fallback_models":["m2"]}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ShutdownDrainSec != 0 {
		t.Fatalf("negative drain must clamp to 0, got %d", cfg.ShutdownDrainSec)
	}
	if len(cfg.FallbackModels) != 1 || cfg.FallbackModels[0] != "m2" {
		t.Fatalf("fallback models not parsed: %v", cfg.FallbackModels)
	}
}

func TestDrainWaitsForInflightReply(t *testing.T) {
	h := newHarness(nil)
	gate := make(chan struct{})
	h.llm.block = gate
	h.b.handleUpdate(dm(500, 500, "hello"))
	waitFor(t, "llm inflight", func() bool { return h.llm.maxInflight() >= 1 })

	if h.b.Drain(30 * time.Millisecond) {
		t.Fatal("drain must time out while a reply is still in flight")
	}
	close(gate)
	if !h.b.Drain(2 * time.Second) {
		t.Fatal("drain must finish after the reply completes")
	}
	waitSent(t, h, 1)
}

func TestStatusShowsStorageAndRules(t *testing.T) {
	dir := t.TempDir()
	h := newHarness(func(c *Config) { c.DBFile = filepath.Join(dir, "bridge.db") })
	defer func() { _ = h.b.Close() }()
	h.b.handleUpdate(dm(testOwner, testOwner, "/status"))
	waitSent(t, h, 1)
	text, _ := h.tg.sends()[0]["text"].(string)
	if !strings.Contains(text, "Lưu trữ:") {
		t.Fatalf("status missing storage line: %q", text)
	}
	if !strings.Contains(text, "bridge.db") {
		t.Fatalf("status missing db name: %q", text)
	}
	if !strings.Contains(text, "Rules:") {
		t.Fatalf("status missing rules line: %q", text)
	}
}
