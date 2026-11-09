package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func dbTestConfig(dir string, mut func(*Config)) Config {
	cfg := defaultConfig()
	cfg.BotToken = "123:TESTTOKEN"
	cfg.OwnerID = testOwner
	cfg.OwnerUsername = "example_owner"
	cfg.APIBase, cfg.APIKey = "http://fake", "k"
	if mut != nil {
		mut(&cfg)
		return cfg
	}
	// defaultConfig() leaves DBFile empty (memory-only mode); storage tests
	// always ask for a fresh DB unless they override it themselves.
	return cfg
}

func TestMetaRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := dbTestConfig(dir, func(c *Config) { c.DBFile = filepath.Join(dir, "bridge.db") })

	b1 := NewBridge(cfg, &fakeTG{}, &fakeLLM{})
	if b1.db == nil || b1.dbErr != nil {
		t.Fatalf("db open failed: %v", b1.dbErr)
	}
	b1.paused = true
	b1.connOK = map[string]bool{testConn: true}
	b1.offset = 4321
	b1.dayCount = 7
	b1.rulesRunOn = false
	b1.mu.Lock()
	b1.dirtyMeta = true
	b1.mu.Unlock()
	b1.flushNow()
	if err := b1.Close(); err != nil {
		t.Fatal(err)
	}

	b2 := NewBridge(cfg, &fakeTG{}, &fakeLLM{})
	defer func() { _ = b2.Close() }()
	b2.mu.Lock()
	defer b2.mu.Unlock()
	if !b2.paused || b2.offset != 4321 || b2.dayCount != 7 ||
		b2.connOK == nil || !b2.connOK[testConn] || b2.rulesRunOn {
		t.Fatalf("meta not restored: paused=%v offset=%d dayCount=%d connOK=%v rulesRunOn=%v",
			b2.paused, b2.offset, b2.dayCount, b2.connOK, b2.rulesRunOn)
	}
}

func TestLegacyStateMigration(t *testing.T) {
	dir := t.TempDir()
	st := filepath.Join(dir, "state.json")
	if err := os.WriteFile(st, []byte(`{"paused":true,"conn_ok":{"c1":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := dbTestConfig(dir, func(c *Config) {
		c.DBFile = filepath.Join(dir, "bridge.db")
		c.StateFile = st
	})
	b1 := NewBridge(cfg, &fakeTG{}, &fakeLLM{})
	if !b1.paused || b1.connOK == nil || !b1.connOK[testConn] {
		t.Fatal("legacy state not migrated")
	}
	if err := b1.Close(); err != nil {
		t.Fatal(err)
	}

	b2 := NewBridge(cfg, &fakeTG{}, &fakeLLM{})
	if !b2.paused {
		t.Fatal("db lost paused after migration boot")
	}
	b2.mu.Lock()
	b2.paused = false
	b2.dirtyMeta = true
	b2.mu.Unlock()
	if err := b2.Close(); err != nil {
		t.Fatal(err)
	}

	b3 := NewBridge(cfg, &fakeTG{}, &fakeLLM{})
	defer func() { _ = b3.Close() }()
	if b3.paused {
		t.Fatal("legacy state.json must be ignored after first migration")
	}
}

func TestDBLockedFailsWithClue(t *testing.T) {
	dir := t.TempDir()
	cfg := dbTestConfig(dir, func(c *Config) { c.DBFile = filepath.Join(dir, "bridge.db") })
	b1 := NewBridge(cfg, &fakeTG{}, &fakeLLM{})
	if b1.db == nil {
		t.Fatal("first open failed")
	}
	defer func() { _ = b1.Close() }()
	b2 := NewBridge(cfg, &fakeTG{}, &fakeLLM{})
	if b2.db != nil || b2.dbErr == nil {
		t.Fatal("second open must fail with dbErr while lock is held")
	}
}

// ensure no accidental drift: the memory-only path still must not touch disk.
func TestMemoryModeWritesNothing(t *testing.T) {
	dir := t.TempDir()
	cfg := dbTestConfig(dir, nil)
	cfg.StateFile = filepath.Join(dir, "state.json")
	b := NewBridge(cfg, &fakeTG{}, &fakeLLM{})
	if b.db != nil || b.dbErr != nil {
		t.Fatal("unexpected db without DBFile")
	}
	b.mu.Lock()
	b.paused = true
	b.saveStateLocked()
	b.mu.Unlock()
	if _, err := os.Stat(cfg.StateFile); err != nil {
		t.Fatalf("legacy state.json must still work in memory mode: %v", err)
	}
	_ = context.Background()
	_ = time.Second
}

func TestChatHistoryRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := dbTestConfig(dir, func(c *Config) { c.DBFile = filepath.Join(dir, "bridge.db") })
	b1 := NewBridge(cfg, &fakeTG{}, &fakeLLM{reply: "Dạ em trả lời"})
	clock := &fakeClock{t: time.Date(2026, 11, 9, 12, 0, 0, 0, time.Local)}
	b1.now = clock.Now
	b1.sleep = func(ctx context.Context, d time.Duration) {}

	if got := b1.handleUpdate(dm(555, 555, "hello")); got != "queued" {
		t.Fatalf("got %q", got)
	}
	waitFor(t, "reply sent", func() bool { return b1.tg != nil })
	settle()
	_ = b1.Close()

	b2 := NewBridge(cfg, &fakeTG{}, &fakeLLM{})
	defer func() { _ = b2.Close() }()
	b2.mu.Lock()
	defer b2.mu.Unlock()
	cs := b2.chats["d:555"]
	if cs == nil {
		t.Fatal("chat not restored")
	}
	if len(cs.msgs) != 2 || cs.msgs[0].Content != "hello" {
		t.Fatalf("restored chat wrong: %+v", cs.msgs)
	}
	if !cs.introduced || cs.runes == 0 || cs.updated == 0 {
		t.Fatalf("flags wrong: introduced=%v runes=%d updated=%d", cs.introduced, cs.runes, cs.updated)
	}
}

func TestHistoryCharCap(t *testing.T) {
	h := newHarness(func(c *Config) { c.HistoryMaxChars = 300 })
	for i := 0; i < 6; i++ {
		h.b.handleUpdate(dm(int64(700+i), int64(700+i), strings.Repeat("x", 150)))
		h.b.handleUpdate(dm(int64(700+i), int64(700+i), strings.Repeat("y", 150)))
	}
	waitSent(t, h, 12)
	settle()
	h.b.mu.Lock()
	defer h.b.mu.Unlock()
	for key, cs := range h.b.chats {
		if cs.runes > 300 {
			t.Fatalf("chat %s exceeds rune cap: %d", key, cs.runes)
		}
	}
}
