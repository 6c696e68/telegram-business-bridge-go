package main

import (
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
)

func heapAlloc() uint64 {
	var m runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

func fillWorstCase(b *Bridge) {
	chunk := strings.Repeat("a", 399) // +1 byte suffix below = 400 bytes/runes per message
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := 0; i < 500; i++ {
		key := "d:" + strconv.Itoa(10000+i)
		for m := 0; m < 20; m++ {
			role := "user"
			if m%2 == 1 {
				role = "assistant"
			}
			// unique allocation per message so the measurement is honest
			content := chunk + string(rune('a'+(i+m)%26))
			b.rememberLocked(key, role, content)
		}
	}
}

// TestWorstCaseRamAndDisk pins the two hard limits on the reference workload:
// 500 chats at the 8000-rune history cap. Measured numbers are logged.
func TestWorstCaseRamAndDisk(t *testing.T) {
	prevGC := debug.SetGCPercent(50)
	defer debug.SetGCPercent(prevGC)
	prevLimit := debug.SetMemoryLimit(64 << 20)
	defer debug.SetMemoryLimit(prevLimit)

	dir := t.TempDir()
	cfg := dbTestConfig(dir, func(c *Config) {
		c.DBFile = filepath.Join(dir, "bridge.db")
		c.MaxChats = 500
		c.HistoryMessages = 20
		c.HistoryMaxChars = 8000
	})
	b := NewBridge(cfg, &fakeTG{}, &fakeLLM{})
	if b.db == nil || b.dbErr != nil {
		t.Fatalf("db: %v", b.dbErr)
	}
	base := heapAlloc()

	fillWorstCase(b)
	live := heapAlloc()

	if err := b.flushAll(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	fi, err := os.Stat(cfg.DBFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	b2 := NewBridge(cfg, &fakeTG{}, &fakeLLM{})
	defer func() { _ = b2.Close() }()
	restored := heapAlloc()
	b2.mu.Lock()
	n := len(b2.chats)
	b2.mu.Unlock()

	deltaLive := int64(live) - int64(base)
	deltaRestore := int64(restored) - int64(base)
	t.Logf("500 chats x 8000 runes: ram_live=+%.1fMB ram_restored=+%.1fMB db=%.2fMB chats=%d",
		float64(deltaLive)/(1<<20), float64(deltaRestore)/(1<<20),
		float64(fi.Size())/(1<<20), n)

	if n != 500 {
		t.Fatalf("restored chats = %d, want 500", n)
	}
	if deltaLive > 20<<20 {
		t.Fatalf("live heap above bound: %d bytes", deltaLive)
	}
	if deltaRestore > 20<<20 {
		t.Fatalf("restored heap above bound: %d bytes", deltaRestore)
	}
	if fi.Size() > 12<<20 {
		t.Fatalf("db above bound: %d bytes", fi.Size())
	}
}

// BenchmarkFlushWorstCase measures a full 500-chat snapshot write, the most
// expensive flush the worker can ever do.
func BenchmarkFlushWorstCase(b *testing.B) {
	dir := b.TempDir()
	cfg := dbTestConfig(dir, func(c *Config) {
		c.DBFile = filepath.Join(dir, "bridge.db")
		c.MaxChats = 500
		c.HistoryMessages = 20
		c.HistoryMaxChars = 8000
	})
	bridge := NewBridge(cfg, &fakeTG{}, &fakeLLM{})
	defer func() { _ = bridge.Close() }()
	fillWorstCase(bridge)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := bridge.flushAll(); err != nil {
			b.Fatal(err)
		}
	}
}
