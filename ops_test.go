package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func opsHarness(t *testing.T) (*harness, string, string) {
	dir := t.TempDir()
	ai := filepath.Join(dir, "rules.md")
	pin := filepath.Join(dir, "rules.pinned.md")
	if err := os.WriteFile(ai, []byte("- Rule AI cũ."), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pin, []byte("- Luôn xưng em, không hứa hẹn thay chủ."), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(func(c *Config) {
		c.RulesFile = ai
		c.RulesPinnedFile = pin
		c.RulesUpdateEvery = 2
	})
	return h, ai, pin
}

func TestPinnedRulesInjectedAndNeverRewritten(t *testing.T) {
	h, ai, pin := opsHarness(t)
	h.llm.fn = func(msgs []ChatMsg) (string, bool) {
		if strings.HasPrefix(msgs[0].Content, "You maintain the long-term rules") {
			return "- Rule AI mới.", true
		}
		return "ok", false
	}
	h.b.handleUpdate(dm(620, 620, "chào"))
	waitSent(t, h, 1)

	// Pinned block must be injected verbatim into the system prompt.
	h.llm.mu.Lock()
	injected := false
	for _, seen := range h.llm.seen {
		if len(seen) > 0 && strings.Contains(seen[0].Content, "[Owner rules") &&
			strings.Contains(seen[0].Content, "không hứa hẹn thay chủ") &&
			strings.Contains(seen[0].Content, "Rule AI cũ") {
			injected = true
			break
		}
	}
	h.llm.mu.Unlock()
	if !injected {
		t.Fatal("pinned rules not injected next to AI rules")
	}

	h.b.handleUpdate(dm(620, 620, "tin hai"))
	waitSent(t, h, 2)
	waitFor(t, "rules rewritten", func() bool {
		data, _ := os.ReadFile(ai)
		return strings.Contains(string(data), "Rule AI mới")
	})
	pinData, err := os.ReadFile(pin)
	if err != nil {
		t.Fatal(err)
	}
	if string(pinData) != "- Luôn xưng em, không hứa hẹn thay chủ." {
		t.Fatalf("pinned file was rewritten: %q", pinData)
	}
	h.b.mu.Lock()
	pinned := h.b.rulesPinned
	h.b.mu.Unlock()
	if !strings.Contains(pinned, "không hứa hẹn") {
		t.Fatal("pinned rules lost from memory")
	}
}

func TestRulesViewShowsPinned(t *testing.T) {
	h, _, _ := opsHarness(t)
	h.b.handleUpdate(dm(testOwner, testOwner, "/rules"))
	waitSent(t, h, 1)
	text, _ := h.tg.sends()[0]["text"].(string)
	if !strings.Contains(text, "rules.pinned.md") || !strings.Contains(text, "không hứa hẹn") {
		t.Fatalf("rules view missing pinned section: %q", text)
	}
}

func TestHealthHandler(t *testing.T) {
	h := newHarness(nil)
	rec := httptest.NewRecorder()
	h.b.healthHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["status"] != "ok" {
		t.Fatalf("status field = %v", payload["status"])
	}
	if payload["goroutines"].(float64) < 1 || payload["heap_bytes"].(float64) < 1 {
		t.Fatalf("snapshot missing numbers: %v", payload)
	}
	for _, banned := range []string{"text", "content", "messages", "token"} {
		if _, ok := payload[banned]; ok {
			t.Fatalf("health payload leaks a sensitive field %q", banned)
		}
	}

	rec = httptest.NewRecorder()
	h.b.healthHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/healthz", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST must be rejected, got %d", rec.Code)
	}
}

func TestRuntimeStatsLine(t *testing.T) {
	h := newHarness(nil)
	st := h.b.snapshotRuntime()
	line := st.line()
	for _, want := range []string{"rss_mb=", "heap_mb=", "goroutines=", "db_mb=", "chats_ram=", "replied_today="} {
		if !strings.Contains(line, want) {
			t.Fatalf("stats line missing %q: %s", want, line)
		}
	}
	if processRSS() == 0 {
		t.Fatal("processRSS must report a positive value")
	}
}

func TestConfigOpsDefaults(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	body := `{"bot_token":"1:x","owner_id":1,"api_base":"http://x","api_key":"k","stats_log_min":-5}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RulesPinnedFile != filepath.Join(dir, "rules.pinned.md") {
		t.Fatalf("pinned default = %q", cfg.RulesPinnedFile)
	}
	if cfg.StatsLogMin != 0 {
		t.Fatalf("negative stats_log_min must clamp to 0, got %d", cfg.StatsLogMin)
	}
	if cfg.HealthAddr != "" {
		t.Fatalf("health endpoint must default to off, got %q", cfg.HealthAddr)
	}

	cfg2, err := loadConfigWithDefaults(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.StatsLogMin != 60 {
		t.Fatalf("stats default = %d, want 60", cfg2.StatsLogMin)
	}
}

func loadConfigWithDefaults(t *testing.T, dir string) (Config, error) {
	t.Helper()
	p := filepath.Join(dir, "config2.json")
	body := `{"bot_token":"1:x","owner_id":1,"api_base":"http://x","api_key":"k"}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return loadConfig(p)
}
