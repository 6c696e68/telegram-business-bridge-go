package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
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
	tg1 := &fakeTG{}
	b1 := NewBridge(cfg, tg1, &fakeLLM{reply: "Dạ em trả lời"})
	clock := &fakeClock{t: time.Date(2026, 11, 9, 12, 0, 0, 0, time.Local)}
	b1.now = clock.Now
	b1.sleep = func(ctx context.Context, d time.Duration) {}

	if got := b1.handleUpdate(dm(555, 555, "hello")); got != "queued" {
		t.Fatalf("got %q", got)
	}
	waitFor(t, "reply sent", func() bool { return len(tg1.sends()) >= 1 })
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

func countBucket(t *testing.T, b *Bridge, name []byte) int {
	t.Helper()
	n := 0
	_ = b.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(name).ForEach(func(_ []byte, _ []byte) error {
			n++
			return nil
		})
	})
	return n
}

func TestRestoreOrderAndCap(t *testing.T) {
	dir := t.TempDir()
	cfg := dbTestConfig(dir, func(c *Config) {
		c.DBFile = filepath.Join(dir, "bridge.db")
		c.MaxChats = 2
	})
	b1 := NewBridge(cfg, &fakeTG{}, &fakeLLM{})
	if err := b1.db.Update(func(tx *bolt.Tx) error {
		ck := tx.Bucket(bucketChats)
		put := func(k string, updated int64, text string) error {
			v, _ := json.Marshal(chatRecord{Introduced: true, Updated: updated, Msgs: []ChatMsg{{Role: "user", Content: text}}})
			return ck.Put([]byte(k), v)
		}
		if err := put("d:1", time.Now().Add(-2*time.Hour).Unix(), "old1"); err != nil {
			return err
		}
		if err := put("d:2", time.Now().Add(-1*time.Hour).Unix(), "old2"); err != nil {
			return err
		}
		return put("d:3", time.Now().Unix(), "fresh")
	}); err != nil {
		t.Fatal(err)
	}
	_ = b1.Close()

	b2 := NewBridge(cfg, &fakeTG{}, &fakeLLM{})
	defer func() { _ = b2.Close() }()
	b2.mu.Lock()
	if len(b2.chats) != 2 {
		b2.mu.Unlock()
		t.Fatalf("restored chats = %d, want 2", len(b2.chats))
	}
	for k, v := range b2.chats {
		t.Logf("chat %s updated=%d msgs=%d", k, v.updated, len(v.msgs))
	}
	if cs := b2.chats["d:3"]; cs == nil || len(cs.msgs) == 0 || cs.msgs[0].Content != "fresh" {
		b2.mu.Unlock()
		t.Fatalf("newest chat must survive restore")
	}
	b2.mu.Unlock()
	if n := countBucket(t, b2, bucketChats); n != 3 {
		t.Fatalf("disk records = %d, want 3 (RAM eviction must not drop disk)", n)
	}
}

func TestPruneExpiredAndCapStored(t *testing.T) {
	dir := t.TempDir()
	cfg := dbTestConfig(dir, func(c *Config) {
		c.DBFile = filepath.Join(dir, "bridge.db")
		c.MemoryDays = 1
		c.MaxChats = 5
		c.MaxStoredChats = 5
	})
	b := NewBridge(cfg, &fakeTG{}, &fakeLLM{})
	defer func() { _ = b.Close() }()
	if err := b.db.Update(func(tx *bolt.Tx) error {
		ck := tx.Bucket(bucketChats)
		put := func(k string, updated int64) error {
			v, _ := json.Marshal(chatRecord{Updated: updated})
			return ck.Put([]byte(k), v)
		}
		if err := put("d:old", time.Now().Add(-48*time.Hour).Unix()); err != nil {
			return err
		}
		for i := 0; i < 7; i++ {
			if err := put("d:fresh"+string(rune('a'+i)), time.Now().Add(time.Duration(i)*time.Minute).Unix()); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	b.flushNow()
	if n := countBucket(t, b, bucketChats); n != 5 {
		t.Fatalf("stored chats = %d, want 5", n)
	}
	hasOld := false
	_ = b.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket(bucketChats).Get([]byte("d:old")) != nil {
			hasOld = true
		}
		return nil
	})
	if hasOld {
		t.Fatal("expired chat must be pruned")
	}
}

func TestDBCompactOnBoot(t *testing.T) {
	dir := t.TempDir()
	dbp := filepath.Join(dir, "bridge.db")
	raw, err := bolt.Open(dbp, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.Update(func(tx *bolt.Tx) error {
		ck, e := tx.CreateBucketIfNotExists(bucketChats)
		if e != nil {
			return e
		}
		_, _ = tx.CreateBucketIfNotExists(bucketMeta)
		junk := strings.Repeat("z", 8192)
		for i := 0; i < 300; i++ {
			r, _ := json.Marshal(chatRecord{Updated: 1, Msgs: []ChatMsg{{Role: "user", Content: junk}}})
			if e := ck.Put([]byte("d:seed"+strconv.Itoa(i)), r); e != nil {
				return e
			}
		}
		// delete most rows to leave free pages behind (what compaction reclaims)
		for i := 50; i < 300; i++ {
			if e := ck.Delete([]byte("d:seed" + strconv.Itoa(i))); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	before, _ := os.Stat(dbp)

	cfg := dbTestConfig(dir, func(c *Config) {
		c.DBFile = dbp
		c.DBCompactMB = 1
	})
	b := NewBridge(cfg, &fakeTG{}, &fakeLLM{})
	defer func() { _ = b.Close() }()
	if b.dbErr != nil {
		t.Fatal(b.dbErr)
	}
	after, _ := os.Stat(dbp)
	bi, ok1 := before.Sys().(*syscall.Stat_t)
	ai, ok2 := after.Sys().(*syscall.Stat_t)
	if ok1 && ok2 && bi.Ino == ai.Ino {
		t.Fatal("compact did not swap in a fresh file (same inode)")
	}
	if after.Size() > before.Size() {
		t.Fatalf("compact must not grow the file: %d -> %d", before.Size(), after.Size())
	}
	if countBucket(t, b, bucketChats) == 0 {
		t.Fatal("compact lost chat records")
	}
}

func TestFailedFlushRequeuesPending(t *testing.T) {
	dir := t.TempDir()
	cfg := dbTestConfig(dir, func(c *Config) { c.DBFile = filepath.Join(dir, "bridge.db") })
	b := NewBridge(cfg, &fakeTG{}, &fakeLLM{})
	// Swap in a read-only handle so every write transaction fails.
	b.flushMu.Lock()
	_ = b.db.Close()
	ro, err := bolt.Open(cfg.DBFile, 0o600, &bolt.Options{ReadOnly: true})
	if err != nil {
		b.flushMu.Unlock()
		t.Fatal(err)
	}
	b.db = ro
	b.flushMu.Unlock()

	b.mu.Lock()
	b.dirtyMeta = true
	b.rememberLocked("d:42", "user", "hello")
	b.mu.Unlock()

	if err := b.flushNow(); err == nil {
		t.Fatal("expected a failed flush on a read-only database")
	}
	b.mu.Lock()
	_, pending := b.pendingChats["d:42"]
	meta := b.dirtyMeta
	b.mu.Unlock()
	if !pending || !meta {
		t.Fatalf("pending work must be requeued: pending=%v meta=%v", pending, meta)
	}
	_ = b.Close()
}
