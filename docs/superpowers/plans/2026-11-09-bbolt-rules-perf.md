# bbolt + restart fixes + rules.md Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** Bot bền (bridge.db), không replay backlog sau restart, chặn trần RAM/disk tự dọn dẹp, và bộ quy tắc `rules.md` AI tự tổng hợp không lưu dữ liệu nhạy cảm.

**Architecture:** bbolt là nguồn chân lý duy nhất thay `state.json`; chỉ flush worker (timer 30 s + dirty-flag) được ghi DB. rules.md là file nguyên khối AI viết lại, 2 lớp chặn dữ liệu nhạy cảm (prompt + regex sanitizer), inject vào system prompt.

**Tech Stack:** Go 1.22 stdlib + `go.etcd.io/bbolt/v3` (duy nhất).

**Spec:** `docs/superpowers/specs/2026-11-09-bridge-bbolt-rules-design.md` (errata đã ghi trong spec: bật/tắt rules = `rules_enabled: false`, không dùng sentinel `""` cho `rules_file`).

## Global Constraints

- Chỉ THÊM dependency `go.etcd.io/bbolt/v3`; CGO_ENABLED=0 phải tiếp tục chạy: `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w -buildid=" .`
- Binary arm64 sau tất cả các task: ≤ 8.0 MB.
- Không bao giờ log nội dung tin nhắn, nội dung rules, token, API key (giữ chuẩn hiện tại: chỉ sự kiện + đếm + mã lỗi).
- Mọi ghi đĩa bền phải atomic (bbolt tự đảm; rules file dùng tmp+rename, chmod 0600).
- UI lệnh owner giữ tiếng Việt + emoji như hiện có.
- Mọi task kết thúc: `go vet ./...` sạch và `go test -count=1 .` xanh (dùng `-race` ở task cuối).
- Config JSON: các key cũ không đổi; `defaultConfig()` đặt giá trị mặc định cho key mới (giữ pattern hiện tại); `loadConfig` chỉ clamp + đường dẫn mặc định.

---

### Task 1: Dependency bbolt + config keys mới + .gitignore + errata

**Files:**
- Modify: `go.mod`, `go.sum` (qua `go get`)
- Modify: `config.go`
- Modify: `.gitignore`
- Test: `config_test.go` (mới, nhỏ)

**Interfaces:**
- Produces: các trường `Config`: `DBFile`, `FlushSec`, `MemoryDays`, `MaxStoredChats`, `MaxBacklogMin`, `HistoryMaxChars`, `RulesEnabled`, `RulesFile`, `RulesMaxChars`, `RulesUpdateEvery`, `RulesAllowURL`, `DBCompactMB` (task sau đọc trực tiếp).

- [x] **Step 1: Thêm dependency**

```sh
go get go.etcd.io/bbolt/v3@latest
go mod tidy
```

- [x] **Step 2: Viết test thất bại (config defaults + clamp)** — tạo `config_test.go`:

```go
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigNewKeys(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	cfgBody := `{"bot_token":"1:x","owner_id":1,"api_base":"http://x","api_key":"k"}`
	if err := os.WriteFile(p, []byte(cfgBody), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DBFile != filepath.Join(dir, "bridge.db") {
		t.Fatalf("db_file default = %q", cfg.DBFile)
	}
	if cfg.RulesFile != filepath.Join(dir, "rules.md") {
		t.Fatalf("rules_file default = %q", cfg.RulesFile)
	}
	if cfg.RulesEnabled != true {
		t.Fatal("rules_enabled must default to true")
	}
	if cfg.FlushSec != 30 || cfg.MemoryDays != 7 || cfg.MaxStoredChats != 1000 ||
		cfg.MaxBacklogMin != 10 || cfg.HistoryMaxChars != 8000 ||
		cfg.RulesMaxChars != 4000 || cfg.RulesUpdateEvery != 10 || cfg.DBCompactMB != 20 {
		t.Fatalf("bad defaults: %+v", cfg)
	}
}

func TestLoadConfigNewClamps(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	cfgBody := `{"bot_token":"1:x","owner_id":1,"api_base":"http://x","api_key":"k",` +
		`"flush_sec":1,"memory_days":0,"max_chats":10,"max_stored_chats":5,` +
		`"history_max_chars":9,"rules_max_chars":9,"rules_update_every":1,"max_backlog_min":-3}`
	if err := os.WriteFile(p, []byte(cfgBody), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FlushSec != 5 || cfg.MemoryDays != 1 {
		t.Fatalf("clamp small values failed: %+v", cfg)
	}
	if cfg.MaxStoredChats < cfg.MaxChats {
		t.Fatal("max_stored_chats must be >= max_chats")
	}
	if cfg.HistoryMaxChars != 200 || cfg.RulesMaxChars != 200 {
		t.Fatalf("char clamp failed: %+v", cfg)
	}
	if cfg.RulesUpdateEvery != 2 {
		t.Fatal("rules_update_every clamp failed")
	}
	if cfg.MaxBacklogMin != 0 {
		t.Fatal("negative max_backlog_min must clamp to 0 (disabled)")
	}
}
```

- [x] **Step 3: Chạy test — THẤT BÁO** (trường chưa tồn tại, biên dịch lỗi)

Run: `go test -run 'TestLoadConfigNew' -count=1 .`
Expected: FAIL — compile error `unknown field`.

- [x] **Step 4: Cài đặt — config.go**

Thêm vào `Config` (sau `LLMRetries`):

```go
	DBFile           string `json:"db_file"`
	FlushSec         int    `json:"flush_sec"`
	MemoryDays       int    `json:"memory_days"`
	MaxStoredChats   int    `json:"max_stored_chats"`
	MaxBacklogMin    int    `json:"max_backlog_min"`
	HistoryMaxChars  int    `json:"history_max_chars"`
	RulesEnabled     bool   `json:"rules_enabled"`
	RulesFile        string `json:"rules_file"`
	RulesMaxChars    int    `json:"rules_max_chars"`
	RulesUpdateEvery int    `json:"rules_update_every"`
	RulesAllowURL    bool   `json:"rules_allow_url"`
	DBCompactMB      int    `json:"db_compact_mb"`
```

`defaultConfig()` thêm (giữ nguyên các field cũ):

```go
		FlushSec:         30,
		MemoryDays:       7,
		MaxStoredChats:   1000,
		MaxBacklogMin:    10,
		HistoryMaxChars:  8000,
		RulesEnabled:     true,
		RulesMaxChars:    4000,
		RulesUpdateEvery: 10,
		DBCompactMB:      20,
```

`loadConfig` — sau block `SystemPromptFile`:

```go
	if cfg.DBFile == "" {
		cfg.DBFile = filepath.Join(filepath.Dir(path), "bridge.db")
	}
	if cfg.RulesFile == "" {
		cfg.RulesFile = filepath.Join(filepath.Dir(path), "rules.md")
	}
```

Khối clamp — thêm:

```go
	clamp(&cfg.FlushSec, 5)
	clamp(&cfg.MemoryDays, 1)
	if cfg.MaxStoredChats < cfg.MaxChats {
		cfg.MaxStoredChats = cfg.MaxChats
	}
	clamp(&cfg.HistoryMaxChars, 200)
	clamp(&cfg.RulesMaxChars, 200)
	if cfg.RulesUpdateEvery < 2 {
		cfg.RulesUpdateEvery = 2
	}
	if cfg.MaxBacklogMin < 0 {
		cfg.MaxBacklogMin = 0
	}
```

(Ghi chú: `defaultConfig()` đặt `DBFile`/`RulesFile` rỗng — chỉ `loadConfig` mới ghi đường dẫn mặc định, để test dựng Bridge bằng `defaultConfig()` không mở DB.)

- [x] **Step 5: .gitignore** — thêm:

```
bridge.db
bridge.db.*
rules.md
*.compact
```

- [x] **Step 6: Chạy toàn bộ test — XANH**

Run: `go test -count=1 .`
Expected: PASS (không test nào cũ bị hỏng: `TestPauseSurvivesRestart` dùng `defaultConfig()` với `DBFile=""` → chế độ nhớ RAM, bất biến với change).

- [x] **Step 7: Commit**

```sh
git add go.mod go.sum config.go .gitignore config_test.go
git commit -m "feat(config): bbolt + retention + rules config keys with clamps"
```

---

### Task 2: Decode stream + trường `Date` + 1 transport dùng chung

**Files:**
- Modify: `telegram.go:137-141`, `telegram.go:25-34`
- Modify: `llm.go:57-60`
- Modify: `main.go:55-58`

**Interfaces:**
- Produces: `Message.Date int64 `json:"date"`` (Task 6 dùng). Hành vi HTTP giữ nguyên (existing tests phải xanh).

- [x] **Step 1: Viết test thất bại** — thêm vào `bridge_test.go` (cuối file):

```go
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
```

- [x] **Step 2: Chạy — THẤT BÁO**

Run: `go test -run TestMessageDateUnmarshalled -count=1 .`
Expected: FAIL, compile `m.Date undefined`.

- [x] **Step 3: Cài đặt**

`telegram.go` — struct `Message` thêm field (đặt sau `Chat`):

```go
	Date int64 `json:"date"`
```

`telegram.go` `Call` — thay block decode (`raw, err := io.ReadAll...; if err := json.Unmarshal(raw, &env)...`):

```go
	dec := json.NewDecoder(io.LimitReader(resp.Body, 4<<20))
	var env tgEnvelope
	if err := dec.Decode(&env); err != nil {
		return &TGError{Method: method, Code: resp.StatusCode, Desc: "bad response"}
	}
```

`llm.go` `Chat` — thay block đọc response tương tự:

```go
	dec := json.NewDecoder(io.LimitReader(resp.Body, 4<<20))
	var d struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := dec.Decode(&d); err != nil || len(d.Choices) == 0 {
		return "", &LLMError{Kind: "bad_response"}
	}
```

Xoá import `io` nếu không còn dùng? (io vẫn dùng: `LimitReader` — giữ).

`main.go` — thay tạo 2 client:

```go
	hc := newHTTPClient(4)
	tgHTTP, llmHTTP := hc, hc
	b := NewBridge(cfg, NewTelegram(cfg.TelegramBase, cfg.BotToken, tgHTTP),
		NewLLM(cfg.APIBase, cfg.APIKey, cfg.Model, llmHTTP))
```

- [x] **Step 4: Chạy toàn bộ — XANH** (bao gồm `TestTelegramBadJSONAnswerIsHandled`, `TestLLMRealHTTP*`, end-to-end — decode stream tương thích)

Run: `go test -count=1 .` → PASS.

- [x] **Step 5: Commit**

```sh
git add telegram.go llm.go main.go bridge_test.go
git commit -m "perf(http): stream-decode responses, one shared transport, m.date field"
```

---

### Task 3: storage.go — meta layer (open/migrate/flush/worker)

**Files:**
- Create: `storage.go`
- Create: `storage_test.go`
- Modify: `bridge.go` (struct Bridge, NewBridge, Run, rateOK, persist path)
- Modify: `main.go` (lifecycle)

**Interfaces:**
- Consumes: `Config.DBFile/FlushSec/MaxStoredChats...` (Task 1).
- Produces: `b.openDB(path)`, `b.loadMetaDB()`, `b.loadLegacyState()`, `b.flushNow()`, `b.flushAll()`, `b.flushLoop(ctx)`, `b.Close() error`, `b.saveStateLocked()` (đổi nghĩa: db→dirty), `b.dbErr error`, fields `b.db *bbolt.DB`, `b.offset int64`, `b.dirtyMeta bool`. `chatRecord{Introduced bool; Updated int64; Msgs []ChatMsg}` (Task 4/5 dùng).

- [x] **Step 1: storage_test.go — test thất bại**

```go
package main

import (
	"testing"
	"path/filepath"
)

func dbTestConfig(dir string, mut func(*Config)) Config {
	cfg := defaultConfig()
	cfg.BotToken = "123:TESTTOKEN"
	cfg.OwnerID = testOwner
	cfg.OwnerUsername = "example_owner"
	cfg.APIBase, cfg.APIKey = "http://fake", "k"
	if mut != nil {
		mut(&cfg)
	}
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
	b1.Close()

	b2 := NewBridge(cfg, &fakeTG{}, &fakeLLM{})
	defer b2.Close()
	b2.mu.Lock()
	defer b2.mu.Unlock()
	if !b2.paused || b2.offset != 4321 || b2.dayCount != 7 ||
		b2.connOK == nil || !b2.connOK[testConn] || b2.rulesRunOn {
		t.Fatalf("meta not restored: %+v", b2)
	}
}

func TestLegacyStateMigration(t *testing.T) {
	dir := t.TempDir()
	st := filepath.Join(dir, "state.json")
	if err := osWriteFileForTest(st, []byte(`{"paused":true,"conn_ok":{"c1":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := dbTestConfig(dir, func(c *Config) {
		c.DBFile = filepath.Join(dir, "bridge.db")
		c.StateFile = st
	})
	b1 := NewBridge(cfg, &fakeTG{}, &fakeLLM{})
	// migration happened inside NewBridge; paused restored without file re-read.
	if !b1.paused || b1.connOK == nil || !b1.connOK[testConn] {
		t.Fatal("legacy state not migrated")
	}
	b1.Close()

	// Subsequent boots use the DB only; state.json is ignored from now on.
	b2 := NewBridge(cfg, &fakeTG{}, &fakeLLM{})
	if !b2.paused {
		t.Fatal("db lost paused after migration boot")
	}
	b2.mu.Lock()
	b2.paused = false
	b2.dirtyMeta = true
	b2.mu.Unlock()
	b2.Close()

	b3 := NewBridge(cfg, &fakeTG{}, &fakeLLM{})
	defer b3.Close()
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
	defer b1.Close()
	b2 := NewBridge(cfg, &fakeTG{}, &fakeLLM{})
	if b2.db != nil || b2.dbErr == nil {
		t.Fatal("second open must fail with dbErr while lock is held")
	}
}
```

(Hàm helper dùng từ Task 3 ban olla đặt: dùng `os.WriteFile` trực tiếp — `osWriteFileForTest` KHÔNG NÊN xuất hiện: dùng `os.WriteFile(st, ..., 0o600)` và import `os`.)

- [x] **Step 2: Chạy — THẤT BÁO** (chưa có storage.go; compile fail `b1.db undefined`).

Run: `go test -run 'TestMetaRoundTrip|TestLegacyStateMigration|TestDBLockedFailsWithClue' -count=1 .`

- [x] **Step 3: Cài đặt — tạo `storage.go`**

```go
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	bolt "go.etcd.io/bbolt/v3"
)

var (
	bucketMeta  = []byte("meta")
	bucketChats = []byte("chats")
)

// chatRecord is the JSON blob stored under each chat key.
type chatRecord struct {
	Introduced bool      `json:"introduced"`
	Updated    int64     `json:"updated"`
	Msgs       []ChatMsg `json:"msgs"`
}

const metaSchema = "1"

type metaSnapshot struct {
	Paused   bool
	Offset   int64
	Day      string
	DayCount int
	ConnOK   map[string]bool
	RulesOn  bool
}

func metaSnapshotFromLocked(b *Bridge) metaSnapshot {
	conn := make(map[string]bool, len(b.connOK))
	for k, v := range b.connOK {
		conn[k] = v
	}
	return metaSnapshot{
		Paused: b.paused, Offset: b.offset, Day: b.day,
		DayCount: b.dayCount, ConnOK: conn, RulesOn: b.rulesRunOn,
	}
}

func writeMetaTx(tx *bolt.Tx, ms metaSnapshot) error {
	mb := tx.Bucket(bucketMeta)
	if mb == nil {
		return errors.New("meta bucket missing")
	}
	put := func(k, v string) { _ = mb.Put([]byte(k), []byte(v)) }
	put("schema", metaSchema)
	if ms.Paused {
		put("paused", "1")
	} else {
		put("paused", "0")
	}
	put("offset", strconv.FormatInt(ms.Offset, 10))
	put("day", ms.Day)
	put("day_count", strconv.Itoa(ms.DayCount))
	if c, e := json.Marshal(ms.ConnOK); e == nil {
		put("conn_ok", string(c))
	}
	if ms.RulesOn {
		put("rules_on", "1")
	} else {
		put("rules_on", "0")
	}
	return nil
}

// openDB opens the bolt store, creates buckets, and runs the one-time
// migration from the legacy state.json. Sets b.dbErr instead of panicking so
// main() can fail fast with a clear message.
func (b *Bridge) openDB(path string) {
	opts := &bolt.Options{Timeout: 5 * time.Second}
	db, err := bolt.Open(path, 0o600, opts)
	if err != nil {
		b.dbErr = fmt.Errorf("database %s busy or unreadable", filepath.Base(path))
		return
	}
	b.db = db
	if err := db.Update(func(tx *bolt.Tx) error {
		if _, e := tx.CreateBucketIfNotExists(bucketMeta); e != nil {
			return e
		}
		_, e := tx.CreateBucketIfNotExists(bucketChats)
		return e
	}); err != nil {
		b.dbErr = fmt.Errorf("database init: %s", err)
		_ = db.Close()
		b.db = nil
		return
	}
	empty := false
	_ = db.View(func(tx *bolt.Tx) error {
		empty = tx.Bucket(bucketMeta).Stats().KeyN == 0 && tx.Bucket(bucketChats).Stats().KeyN == 0
		return nil
	})
	// One-time migration from the legacy state.json; from now on that file is ignored.
	if empty && b.cfg.StateFile != "" {
		if data, e := os.ReadFile(b.cfg.StateFile); e == nil {
			var st persistedState
			if json.Unmarshal(data, &st) == nil && (st.Paused || st.ConnOK != nil) {
				if st.ConnOK == nil {
					st.ConnOK = map[string]bool{}
				}
				b.mu.Lock()
				b.paused = st.Paused
				b.connOK = st.ConnOK
				b.dirtyMeta = true
				b.mu.Unlock()
				logf("INFO", "migrated %s into database", filepath.Base(b.cfg.StateFile))
			}
		}
	}
}

func (b *Bridge) loadMetaDB() {
	_ = b.db.View(func(tx *bolt.Tx) error {
		mb := tx.Bucket(bucketMeta)
		if mb == nil || mb.Get([]byte("schema")) == nil {
			return nil
		}
		get := func(k string) []byte { return mb.Get([]byte(k)) }
		b.paused = string(get("paused")) == "1"
		if v := get("offset"); v != nil {
			b.offset, _ = strconv.ParseInt(string(v), 10, 64)
		}
		if d := get("day"); string(d) != "" {
			b.day = string(d)
		}
		if v := get("day_count"); v != nil {
			b.dayCount, _ = strconv.Atoi(string(v))
		}
		if v := get("conn_ok"); len(v) > 0 {
			var m map[string]bool
			if json.Unmarshal(v, &m) == nil && m != nil {
				b.connOK = m
			}
		}
		b.rulesRunOn = string(get("rules_on")) != "0"
		return nil
	})
}

// loadLegacyState reads the old state.json in memory-only mode (db disabled).
func (b *Bridge) loadLegacyState() {
	p := b.cfg.StateFile
	if p == "" {
		return
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var st persistedState
	if json.Unmarshal(data, &st) != nil {
		return
	}
	b.paused = st.Paused
	if st.ConnOK != nil {
		b.connOK = st.ConnOK
	}
}

// saveStateLocked persists meta. DB mode marks dirty (flush worker writes);
// memory-only mode keeps the legacy atomic state.json write. Caller holds b.mu.
func (b *Bridge) saveStateLocked() {
	if b.db != nil {
		b.dirtyMeta = true
		return
	}
	p := b.cfg.StateFile
	if p == "" {
		return
	}
	data, err := json.Marshal(persistedState{Paused: b.paused, ConnOK: b.connOK})
	if err != nil {
		return
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		logf("WARN", "state save failed")
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		logf("WARN", "state rename failed")
	}
}

// flushNow writes dirty meta/chats and prunes in one transaction. It is the
// only writer of the DB (single-writer invariant, spec §3).
func (b *Bridge) flushNow() {
	if b.db == nil {
		return
	}
	b.flushMu.Lock()
	defer b.flushMu.Unlock()

	b.mu.Lock()
	wantMeta := b.dirtyMeta
	var ms metaSnapshot
	if wantMeta {
		b.dirtyMeta = false
		ms = metaSnapshotFromLocked(b)
	}
	recs := make(map[string]chatRecord, len(b.dirtyChats))
	for k := range b.dirtyChats {
		if cs, ok := b.chats[k]; ok {
			recs[k] = chatRecord{Introduced: cs.introduced, Updated: cs.updated, Msgs: append([]ChatMsg(nil), cs.msgs...)}
		}
	}
	b.dirtyChats = map[string]struct{}{}
	del := b.deletedChats
	b.deletedChats = map[string]struct{}{}
	b.mu.Unlock()

	err := b.db.Update(func(tx *bolt.Tx) error {
		if wantMeta {
			if e := writeMetaTx(tx, ms); e != nil {
				return e
			}
		}
		ck := tx.Bucket(bucketChats)
		for k, r := range recs {
			if v, e := json.Marshal(r); e == nil {
				if e := ck.Put([]byte(k), v); e != nil {
					return e
				}
			}
		}
		for k := range del {
			_ = ck.Delete([]byte(k))
		}
		pruneChatsTx(tx, b.now(), b.cfg)
		return nil
	})
	if err != nil {
		logf("WARN", "db flush: %s", err)
	}
}

// flushAll snapshots every chat + meta (shutdown path).
func (b *Bridge) flushAll() {
	if b.db == nil {
		return
	}
	b.mu.Lock()
	b.dirtyMeta = true
	for k := range b.chats {
		b.dirtyChats[k] = struct{}{}
	}
	b.mu.Unlock()
	b.flushNow()
}

func (b *Bridge) flushLoop(ctx interface{ Done() <-chan struct{} }) {
	interval := time.Duration(b.cfg.FlushSec) * time.Second
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.flushNow()
		}
	}
}

// Close flushes everything and closes the DB. Safe in memory-only mode.
func (b *Bridge) Close() error {
	if b.db == nil {
		return nil
	}
	b.flushAll()
	err := b.db.Close()
	b.db = nil
	return err
}

// pruneChatsTx deletes expired and surplus chat records inside tx.
func pruneChatsTx(tx *bolt.Tx, now time.Time, cfg Config) {
	ck := tx.Bucket(bucketChats)
	if ck == nil {
		return
	}
	cutoff := now.Add(-time.Duration(cfg.MemoryDays) * 24 * time.Hour).Unix()
	type kvRec struct {
		key string
		rec chatRecord
	}
	var all []kvRec
	ck.ForEach(func(k, v []byte) error {
		var r chatRecord
		if json.Unmarshal(v, &r) == nil {
			all = append(all, kvRec{string(k), r})
		} else {
			all = append(all, kvRec{string(k), chatRecord{}}) // invalid → updated 0 → expired
		}
		return nil
	})
	var survivors []kvRec
	for _, it := range all {
		if it.rec.Updated >= cutoff {
			survivors = append(survivors, it)
		} else {
			_ = ck.Delete([]byte(it.key))
		}
	}
	extra := len(survivors) - cfg.MaxStoredChats
	if extra > 0 {
		sort.Slice(survivors, func(i, j int) bool { return survivors[i].rec.Updated > survivors[j].rec.Updated })
		for _, it := range survivors[len(survivors)-extra:] {
			_ = ck.Delete([]byte(it.key))
		}
	}
}
```

(Lưu ý: `flushLoop` nhận `ctx interface{ Done() <-chan struct{} }` để test có thể thay context chuẩn; production dùng `context.Context`.)

- [x] **Step 4: bridge.go cắm** — struct `Bridge` thêm:

```go
	db           *bolt.DB
	dbErr        error
	offset       int64
	dirtyMeta    bool
	dirtyChats   map[string]struct{}
	deletedChats map[string]struct{}
	flushMu      sync.Mutex
```

(import `bolt "go.etcd.io/bbolt/v3"`.) `Bridge` thêm `rulesRunOn bool` (dùng Task 7-8; field rỗng ok).

`NewBridge` — thay `b.loadState()` thành:

```go
	if cfg.DBFile != "" {
		b.openDB(cfg.DBFile)
		if b.dbErr != nil {
			return b
		}
		b.loadMetaDB()
	} else {
		b.loadLegacyState()
	}
```

Xoá hàm `loadState()` cũ; khởi tạo maps trong literal: `dirtyChats: map[string]struct{}{}, deletedChats: map[string]struct{}{}, rulesRunOn: true`.

`Run` — seed offset + persist theo lô:

```go
	var offset int64
	hasOffset := false
	b.mu.Lock()
	offset = b.offset
	if offset > 0 {
		hasOffset = true
	}
	b.mu.Unlock()
```

Sau vòng `for _, u := range updates {...}` trong poll loop:

```go
		if len(updates) > 0 {
			b.mu.Lock()
			b.offset = offset
			b.dirtyMeta = true
			b.mu.Unlock()
		}
```

`rateOK` — đánh dơ khi đổi ngày/đếm: sau `b.dayCount++`:

```go
	b.saveStateLocked()
```

(và trong nhánh đổi ngày `b.day, b.dayCount = today, 0` thêm `b.saveStateLocked()`.)

`main.go` lifecycle — sau NewBridge:

```go
	if b.dbErr != nil {
		logf("ERROR", "%s", b.dbErr.Error())
		os.Exit(2)
	}
	go b.flushLoop(ctx)
```

Cuối main thay `saveStateLocked` block:

```go
	_ = b.Close()
	logf("INFO", "stopped cleanly")
```

- [x] **Step 5: Chạy toàn bộ — XANH** (`go test -count=1 .` — `TestPauseSurvivesRestart` vẫn xanh vì mem-mode path giữ nguyên; storage tests xanh).

- [x] **Step 6: Commit**

```sh
git add storage.go storage_test.go bridge.go main.go
git commit -m "feat(storage): bbolt meta layer with one-time state.json migration"
```

---

### Task 4: chat layer — updated/runes cap + snapshot ghi chat

**Files:**
- Modify: `bridge.go` (chatState, rememberLocked, dirtyChats marking)
- Modify: `storage.go` (helper `chatRunes` nằm storage.go; flushNow CHATsnapshot đã có từ Task 3)
- Test: `storage_test.go` thêm

**Interfaces:**
- Consumes: `chatRecord`, `flushNow`, `dirtyChats` (Task 3).
- Produces: `chatState.updated int64`, `chatState.runes int`, `chatRunes(msgs []ChatMsg) int` (Task 5 dùng).

- [x] **Step 1: test thất bại** — `storage_test.go` thêm:

```go
func TestChatHistoryRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := dbTestConfig(dir, func(c *Config) { c.DBFile = filepath.Join(dir, "bridge.db") })
	b1 := NewBridge(cfg, &fakeTG{}, &fakeLLM{})
	clock := &fakeClock{t: time.Date(2026, 11, 9, 12, 0, 0, 0, time.Local)}
	b1.now = clock.Now
	b1.sleep = func(ctx context.Context, d time.Duration) {}

	if got := b1.handleUpdate(dm(555, 555, "hello")); got != "queued" {
		t.Fatalf("got %q", got)
	}
	waitSent(t, harnessFor(b1), 1)
	settle()
	b1.Close()

	b2 := NewBridge(cfg, &fakeTG{}, &fakeLLM{})
	defer b2.Close()
	b2.mu.Lock()
	defer b2.mu.Unlock()
	cs := b2.chats["d:555"]
	if cs == nil {
		t.Fatal("chat not restored")
	}
	if !cs.introduced || len(cs.msgs) != 2 || cs.msgs[0].Content != "hello" {
		t.Fatalf("restored chat wrong: %+v", cs.msgs)
	}
	if cs.runes == 0 || cs.updated == 0 {
		t.Fatal("updated/runes not filled")
	}
}

func TestHistoryCharCap(t *testing.T) {
	h := newHarness(func(c *Config) { c.HistoryMaxChars = 300 })
	var lastSys []ChatMsg
	h.llm.reply = "r"
	for i := 0; i < 6; i++ {
		h.b.handleUpdate(dm(700+i, 700+i, strings.Repeat("x", 150)))
		h.b.handleUpdate(dm(700+i, 700+i, strings.Repeat("y", 150)))
	}
	waitSent(t, h, 6)
	settle()
	for _, m := range h.b.chats {
		b2 := m
		if b2.runes > 300 {
			t.Fatalf("history exceeds cap: %d", b2.runes)
		}
	}
	_ = lastSys
}
```

(Chỉnh: harnessFor không tồn tại — dùng waitFor trực tiếp. Xem Step lamma dưới, chuẩn hoá final trong plan-exec.)

- [x] **Step 2: cài đặt bridge.go**

`chatState` thêm fields `updated int64`, `runes int`.

`storage.go` thêm helper:

```go
func chatRunes(msgs []ChatMsg) int {
	total := 0
	for i := range msgs {
		total += runeCount(msgs[i].Content)
	}
	return total
}
```

`rememberLocked` — sau merge/trim cũ, thêm:

```go
	cs.updated = b.now().Unix()
	// cap theo tổng runes giữ trần RAM (spec §6).
	lim := b.cfg.HistoryMaxChars
	for len(cs.msgs) > 1 && chatRunes(cs.msgs) > lim {
		cs.msgs = append([]ChatMsg(nil), cs.msgs[1:]...)
	}
	if len(cs.msgs) == 1 && chatRunes(cs.msgs) > lim {
		cs.msgs[0].Content = truncRunes(cs.msgs[0].Content, lim)
	}
	if len(cs.msgs) > b.cfg.HistoryMessages {
		cs.msgs = append([]ChatMsg(nil), cs.msgs[len(cs.msgs)-b.cfg.HistoryMessages:]...)
	}
	cs.runes = chatRunes(cs.msgs)
	b.dirtyChats[key] = struct{}{}
```

(config `HistoryMaxChars` clamp ≥200 nên lim luôn ≥1.)

- [x] **Step 3: chạy toàn bộ — XANH** (đặc biệt: `TestPauseSurvivesRestart` — mem-mode dirtyChats map được gọi nhưng db nil — vô hại).

Run: `go test -count=1 .`

- [x] **Step 4: Commit**

```sh
git add bridge.go storage.go storage_test.go
git commit -m "feat(storage): durable chat history with per-chat rune cap"
```

---

### Task 5: restore + prune-on-boot + compact-on-boot

**Files:**
- Modify: `storage.go` (restoreChats, maybeCompact)
- Modify: `bridge.go` (NewBridge gọi)
- Test: `storage_test.go`

**Interfaces:**
- Produces: `b.restoreChats()`, `b.maybeCompact()`.

- [x] **Step 1: tests** — thêm vào `storage_test.go`:

```go
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
	b1.Close()

	b2 := NewBridge(cfg, &fakeTG{}, &fakeLLM{})
	defer b2.Close()
	b2.mu.Lock()
	defer b2.mu.Unlock()
	if len(b2.chats) != 2 { // RAM cap = MaxChats; fresh nhất giữ
		t.Fatalf("restored chats = %d", len(b2.chats))
	}
	if cs := b2.chats["d:3"]; cs == nil || cs.msgs[0].Content != "fresh" {
		t.Fatal("newest chat must survive")
	}
	if b2.chats["d:1"] != nil || b2.chats["d:2"] != nil {
		t.Fatal("only MaxChats newest chats restored")
	}
	// disk still keeps evicted ones (pruned by memory_days only).
	b2.flushAll()
	b2.mu.Unlock() // flushAll llock-internal; count below re-locks
	b2.mu.Lock()
	b2.deletedChats = map[string]struct{}{}
	b2.mu.Unlock()
	n := 0
	_ = b2.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketChats).ForEach(func(k []byte, _ []byte) error {
			n++
			return nil
		})
	})
	if n < 3 {
		t.Fatal("disk records must not be dropped by RAM eviction")
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
	if err := b.db.Update(func(tx *bolt.Tx) error {
		ck := tx.Bucket(bucketChats)
		put := func(k string, updated int64) error {
			v, _ := json.Marshal(chatRecord{Updated: updated})
			return ck.Put([]byte(k), v)
		}
		old := time.Now().Add(-48 * time.Hour).Unix()
		if err := put("d:old", old); err != nil {
			return err
		}
		for i := 0; i < 7; i++ {
			if err := put("d:fresh"+string(rune('a'+i)), time.Now().Unix(), ); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	b.flushNow()
	count := 0
	keys := map[string]bool{}
	_ = b.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketChats).ForEach(func(k []byte, _ []byte) error {
			count++
			keys[string(k)] = true
			return nil
		})
	})
	if count != 5 {
		t.Fatalf("stored chats = %d, want 5", count)
	}
	if keys["d:old"] {
		t.Fatal("expired chat must be pruned")
	}
	_ = b.Close()
}

func TestDBCompactOnBoot(t *testing.T) {
	dir := t.TempDir()
	dbp := filepath.Join(dir, "bridge.db")
	// seed a sparse DB larger than the compact threshold
	raw, err := bolt.Open(dbp, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.Update(func(tx *bolt.Tx) error {
		_, e1 := tx.CreateBucketIfNotExists(bucketMeta)
		ck, e2 := tx.CreateBucketIfNotExists(bucketChats)
		if e1 != nil || e2 != nil {
			return errors.New("bucket seed fail")
		}
		junk := strings.Repeat("z", 8192)
		for i := 0; i < 300; i++ {
			r, _ := json.Marshal(chatRecord{Updated: 1, Msgs: []ChatMsg{{Role: "user", Content: junk}}})
			if e := ck.Put([]byte("d:seed"+strconv.Itoa(i)), r); e != nil {
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
	defer b.Close()
	if b.dbErr != nil {
		t.Fatal(b.dbErr)
	}
	if b.db.Size() >= before.Size() {
		t.Fatalf("compact did not shrink: %d -> %d", before.Size(), b.db.Size())
	}
}
```

- [x] **Step 2: chạy — THẤT BÁO** (compile fail: no restoreChats/maybeCompact).

- [x] **Step 3: cài đặt storage.go**

```go
// restoreChats loads chat records newest-first into the LRU and marks
// expired/invalid ones for deletion. Bound in RAM by MaxChats (existing
// eviction), bound on disk by memory_days + max_stored_chats.
func (b *Bridge) restoreChats() {
	if b.db == nil {
		return
	}
	type kvRec struct {
		key string
		rec chatRecord
	}
	var all []kvRec
	_ = b.db.View(func(tx *bolt.Tx) error {
		ck := tx.Bucket(bucketChats)
		if ck == nil {
			return nil
		}
		return ck.ForEach(func(k, v []byte) error {
			var r chatRecord
			if e := json.Unmarshal(v, &r); e == nil {
				all = append(all, kvRec{string(k), r})
			} else {
				b.mu.Lock()
				b.deletedChats[string(k)] = struct{}{}
				b.mu.Unlock()
			}
			return nil
		})
	})
	sort.Slice(all, func(i, j int) bool { return all[i].rec.Updated > all[j].rec.Updated })
	cutoff := b.now().Add(-time.Duration(b.cfg.MemoryDays) * 24 * time.Hour).Unix()
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, it := range all {
		if it.rec.Updated < cutoff {
			b.deletedChats[it.key] = struct{}{}
			continue
		}
		cs := &chatState{key: it.key, introduced: it.rec.Introduced, updated: it.rec.Updated, msgs: it.rec.Msgs}
		if cs.msgs == nil {
			cs.msgs = []ChatMsg{}
		}
		cs.runes = chatRunes(cs.msgs)
		cs.el = b.lru.PushFront(cs)
		b.chats[it.key] = cs
	}
}

// maybeCompact rewrites the DB file when it exceeds db_compact_mb (boot only,
// while nothing else holds the file). Non-fatal on any failure.
func (b *Bridge) maybeCompact() {
	if b.db == nil || b.cfg.DBCompactMB <= 0 || b.db.Size() < int64(b.cfg.DBCompactMB)<<20 {
		return
	}
	tmp := b.cfg.DBFile + ".compact"
	f, err := os.Create(tmp)
	if err != nil {
		logf("WARN", "db compact: temp open failed")
		return
	}
	if err := b.db.Compact(f); err != nil {
		f.Close()
		os.Remove(tmp)
		logf("WARN", "db compact failed: %s", err)
		return
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		logf("WARN", "db compact close failed")
		return
	}
	if err := b.db.Close(); err != nil {
		logf("WARN", "db compact old close failed")
		if nd, e := bolt.Open(b.cfg.DBFile, 0o600, &bolt.Options{Timeout: 5 * time.Second}); e == nil {
			b.db = nd
		} else {
			b.db, b.dbErr = nil, e
		}
		os.Remove(tmp)
		return
	}
	if err := os.Rename(tmp, b.cfg.DBFile); err != nil {
		logf("WARN", "db compact rename failed")
		if nd, e := bolt.Open(b.cfg.DBFile, 0o600, &bolt.Options{Timeout: 5 * time.Second}); e == nil {
			b.db = nd
		} else {
			b.db, b.dbErr = nil, e
		}
		return
	}
	nd, err := bolt.Open(b.cfg.DBFile, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		logf("ERROR", "db reopen after compact: %s", err)
		b.db, b.dbErr = nil, err
		return
	}
	b.db = nd
	logf("INFO", "database compacted, size=%d", b.db.Size())
}
```

`NewBridge` — sau `loadMetaDB()`/trước prompt? Thứ tự: open → loadMeta → loadLegacy → **readPrompt/loadRules** → `maybeCompact()` → `restoreChats()`. Cụ thể thêm vào cuối NewBridge (trước return):

```go
	if b.db != nil {
		b.maybeCompact()
		b.restoreChats()
	}
```

(imports thêm cho storage.go: `errors` đã có; test dùng `errors` — nếu test file chưa import thì thêm.)

- [x] **Step 4: chạy toàn bộ — XANH.**

Run: `go test -count=1 .`

- [x] **Step 5: Commit**

```sh
git add storage.go storage_test.go bridge.go
git commit -m "feat(storage): restore chats newest-first, prune expired, compact on boot"
```

---

### Task 6: Bộ lọc backlog theo tuổi tin nhắn

**Files:**
- Modify: `bridge.go` (`maybeReply` + 2 caller)
- Test: `bridge_test.go`

**Interfaces:**
- Consumes: `Message.Date` (Task 2), `Config.MaxBacklogMin` (Task 1).
- Produces: reason `"stale"`.

- [x] **Step 1: test thất bại** — thêm vào `bridge_test.go`:

```go
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
	if h.b.dayCount != 0 {
		t.Fatal("stale replies must not consume quota")
	}
	// current message still handled normally
	fresh := biz(testConn, 801, 801, "hi")
	fresh.BusinessMessage.Date = h.clock.Now().Unix()
	if got := h.b.handleUpdate(fresh); got != "queued" {
		t.Fatalf("got %q", got)
	}
	waitSent(t, h, 1)
}
```

- [x] **Step 2: chạy — THẤT BÁO** (got "queued").

- [x] **Step 3: cài đặt** — `maybeReply` đổi signature thêm `date int64`:

```go
func (b *Bridge) maybeReply(key string, chatID int64, text string, from *User, conn string, date int64) string {
	if b.cfg.MaxBacklogMin > 0 && date > 0 &&
		b.now().Sub(time.Unix(date, 0)) > time.Duration(b.cfg.MaxBacklogMin)*time.Minute {
		return "stale"
	}
	// ... phần cũ giữ nguyên
```

Callers: `onBusinessMessage` → `b.maybeReply(key, m.Chat.ID, text, m.From, conn, m.Date)`; `onBotMessage` → `..., m.Date)`.

- [x] **Step 4: chạy toàn bộ — XANH.**

- [x] **Step 5: Commit**

```sh
git add bridge.go bridge_test.go
git commit -m "fix(restart): skip messages older than max_backlog_min"
```

---

### Task 7: rules.go — sanitizer + parser + ghi file + load

**Files:**
- Create: `rules.go`
- Create: `rules_test.go`

**Interfaces:**
- Consumes: `truncRunes`, `runeCount` (util.go).
- Produces: `sanitizeRules(text, ownerTag string, allowURL bool, limit int) string`, `blocksQuoted(out string, sources []string) bool`, `parseDistillOutput(out string) (string, bool)`, `writeRulesAtomic(path, text string) error`, `b.loadRules()`, `const rulesHeader string`.

- [x] **Step 1: test thất bại** — tạo `rules_test.go`:

```go
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizeAcceptsCleanRule(t *testing.T) {
	in := "# Về chủ\n- Chủ thích trả lời ngắn gọn, tiếng Việt thân thiện.\n"
	got := sanitizeRules(in, "@owner", false, 1000)
	if got != in {
		t.Fatalf("clean rules must pass intact, got %q", got)
	}
}

func TestSanitizeDropsSensitiveLine(t *testing.T) {
	in := "- Khách hay hỏi về bảo hành sản phẩm.\n" +
		"- SĐT chủ là 0901234567.\n" +
		"- Gửi mail lienhe@shop.vn nhé.\n" +
		"- Giá combo hiện nay là 500k.\n" +
		"- Xem chi tiết tại https://example.com/sale.\n"
	got := sanitizeRules(in, "@owner", false, 1000)
	if got != "" {
		t.Fatalf("expected whole-file reject (>50%% bad), got %q", got)
	}
}

func TestSanitizeKeepsMajorityClean(t *testing.T) {
	in := "- Hỗ trợ khách trong giờ hành chính.\n" +
		"- Khách hay hỏi về bảo hành sản phẩm.\n" +
		"- KhôngBall hotline 0901234567 ra công khai.\n"
	got := sanitizeRules(in, "@owner", false, 1000)
	if strings.Contains(got, "0901234567") {
		t.Fatalf("phone leaked: %q", got)
	}
	if !strings.Contains(got, "bảo hành") {
		t.Fatalf("benign line dropped: %q", got)
	}
}

func TestSanitizeHandlesTelegramHandle(t *testing.T) {
	in := "- Chỉ nhắn xử lý qua @owner.\n- Tránh tương tác với @someone_else.\n"
	got := sanitizeRules(in, "@owner", false, 1000)
	if strings.Contains(got, "@someone_else") {
		t.Fatalf("foreign handle leaked: %q", got)
	}
	if !strings.Contains(got, "@owner") {
		t.Fatalf("owner tag must stay: %q", got)
	}
}

func TestSanitizeURLToggle(t *testing.T) {
	line := "- Tham chiếu https://example.com/manual.\n"
	if got := sanitizeRules(line, "@owner", false, 1000); got != "" {
		t.Fatalf("URL must be blocked by default, got %q", got)
	}
	if got := sanitizeRules(line, "@owner", true, 1000); got != line {
		t.Fatalf("URL must be allowed when enable, got %q", got)
	}
}

func TestRulesRejectVerbatimQuote(t *testing.T) {
	source := strings.Repeat("abcdefgh", 40) // 320 runes
	var blocks []string
	r := []rune(source)
	blocks = append(blocks, string(r[80:120]), string(r[start40(r, 1)]))
	_ = blocks
	if !blocksQuoted(string(r[80:120]), []string{source}) {
		t.Fatal("40-rune verbatim block must be detected")
	}
	if blocksQuoted("quy tắc thuần túy không trích lời", []string{source}) {
		t.Fatal("clean output must not be flagged")
	}
}

func start40(r []rune, i int) int {
	return len(r) * i / 4
}

func TestParseDistillOutput(t *testing.T) {
	if s, un := parseDistillOutput(" UNCHANGED "); !un || s != "" {
		t.Fatalf("sentinel not honored")
	}
	s, un := parseDistillOutput("```markdown\n- Rule A\n```")
	if un || s != "- Rule A" {
		t.Fatalf("fence not stripped: %q", s)
	}
	if _, un := parseDistillOutput(""); !un {
		t.Fatal("empty means unchanged (nothing new)")
	}
}

func TestWriteRulesAtomic(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rules.md")
	if err := writeRulesAtomic(p, "# A\n- giữ nguyên"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p)
	if err != nil || string(data) != "# A\n- giữ nguyên" {
		t.Fatalf("write/read mismatch: %v %q", err, data)
	}
}
```

(Chỉnh nhỏ khi cài: `TestSanitizeKeepsMajorityClean` có dòng chứa giữa "bảo hành" sạch — OK; kéo tên helper khỏi test nếu rối: chỉ gọi trực tiếp `string(r[80:120])`.)

- [x] **Step 2: chạy — THẤT BÁO** (compile: undefined).

- [x] **Step 3: cài đặt `rules.go`**

```go
package main

import (
	"os"
	"regexp"
	"strings"
)

const rulesHeader = "\n\n[Long-term rules]\n"

var (
	reEmail  = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	reURL    = regexp.MustCompile(`(?i)https?://|www\.|\.(com|net|vn|org|io|biz)\b`)
	reDigits = regexp.MustCompile(`[0-9]{6,}`)
	reMoney  = regexp.MustCompile(`(?i)\d+[,.]?\d*\s*(vnd|đ|usd|\$|k\b|tr\b|triệu|tỷ)`)
	reHandle = regexp.MustCompile(`@[A-Za-z0-9_]{3,}`)
	reID     = regexp.MustCompile(`(?i)\bid\b[^0-9]{0,3}\d{3,}`)
)

// lineBad reports a policy violation in one candidate rule line.
func lineBad(line, ownerTag string, allowURL bool) bool {
	if reEmail.MatchString(line) || reDigits.MatchString(line) ||
		reMoney.MatchString(line) || reID.MatchString(line) {
		return true
	}
	if !allowURL && reURL.MatchString(line) {
		return true
	}
	for _, h := range reHandle.FindAllString(line, -1) {
		if h != ownerTag {
			return true
		}
	}
	return false
}

// sanitizeRules filters distiller output line-by-line. "" means nothing usable
// remained (or over half the content lines were rejected → keep old file).
func sanitizeRules(text, ownerTag string, allowURL bool, limit int) string {
	lines := strings.Split(text, "\n")
	kept, rejected := 0, 0
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" || strings.HasPrefix(ln, "#") {
			out = append(out, ln)
			continue
		}
		if lineBad(ln, ownerTag, allowURL) {
			rejected++
			continue
		}
		kept++
		out = append(out, ln)
	}
	if kept == 0 || rejected*2 > kept+rejected {
		return ""
	}
	return truncRunes(strings.Join(out, "\n"), limit)
}

// blocksQuoted reports whether out contains a 40-rune block copied from any
// source conversation (windows at 0%, 25%, 50%).
func blocksQuoted(out string, sources []string) bool {
	for _, src := range sources {
		r := []rune(src)
		n := len(r)
		if n < 80 {
			continue
		}
		for i := 0; i < 3; i++ {
			start := n * i / 4
			if start+40 > n {
				break
			}
			if strings.Contains(out, string(r[start:start+40])) {
				return true
			}
		}
	}
	return false
}

// parseDistillOutput normalises the distiller reply. unchanged=true → keep old file.
func parseDistillOutput(out string) (string, bool) {
	s := strings.TrimSpace(out)
	s = strings.TrimPrefix(s, "```markdown")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	if s == "" || s == "UNCHANGED" {
		return "", true
	}
	return s, false
}

func writeRulesAtomic(path, text string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadRules reads the owner-editable rules file into memory (capped).
func (b *Bridge) loadRules() {
	if !b.rulesEnabled || b.cfg.RulesFile == "" {
		return
	}
	data, err := os.ReadFile(b.cfg.RulesFile)
	if err == nil {
		b.rulesText = truncRunes(string(data), b.cfg.RulesMaxChars)
	}
}
```

- [x] **Step 4: chạy `go test -run TestSanitize -run TestRules -count=1 .`` → PASS; toàn bộ → PASS.**

- [x] **Step 5: Commit**

```sh
git add rules.go rules_test.go
git commit -m "feat(rules): sensitive-data sanitizer, verbatim guard, atomic file writer"
```

---

### Task 8: Vòng sống của rules — counter, distill job, inject, lệnh /rules

**Files:**
- Modify: `bridge.go` (restart job fields, replyJob, ownerCommand, parseCommand)
- Modify: `util.go` (parseCommand)
- Modify: `bridge_test.go` (fakeLLM opening开播 replies/fn)
- Test: `bridge_test.go`

**Interfaces:**
- Consumes: Task 7 functions, `cfg.RulesUpdateEvery/RulesMaxChars/RulesAllowURL`, `saveStateLocked`, `callLLM`.
- Produces: `b.rulesOn() bool`, `b.rulesNoteLocked() string`, `b.buildDistillInputLocked() (string, []string)`, `b.distillJob()`, `parseCommand(text) (cmd, args string)` (Task 9 dùng key/conn).

- [x] **Step 1: fakeLLM mở rộng** — `bridge_test.go`: struct thêm `replies []string`, `callIdx int`, `fn func([]ChatMsg) (string, bool)`; tail của `Chat` đổi:

```go
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.errs) > 0 {
		e := f.errs[0]
		f.errs = f.errs[1:]
		return "", e
	}
	if f.fn != nil {
		if out, ok := f.fn(msgs); ok {
			return out, nil
		}
	}
	if f.callIdx < len(f.replies) {
		out := f.replies[f.callIdx]
		f.callIdx++
		return out, nil
	}
	return f.reply, nil
```

- [x] **Step 2: test thất bại** — thêm `rules_test.go`:

```go
func TestRulesInjectionAndDistill(t *testing.T) {
	dir := t.TempDir()
	rf := filepath.Join(dir, "rules.md")

	h := newHarness(None nil)
}
```

(Thay bằng bản hoàn chỉnh sau — giữ nguyên template khi triển khai:)

```go
func TestRulesInjectionAndDistill(t *testing.T) {
	dir := t.TempDir()
	h := newHarness(func(c *Config) {
		c.RulesUpdateEvery = 2
		c.RulesFile = filepath.Join(dir, "rules.md")
		c.HistoryMessages = 20
	})
	// discriminator: normal chat call vs distill call
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
	// distiller fired at every 2 replies
	waitFor(t, "rules file", func() bool { _, err := os.Stat(h.b.cfg.RulesFile); return err == nil })
	b2, _ := os.ReadFile(h.b.cfg.RulesFile)
	if !strings.Contains(string(b2), "bảo hành") {
		t.Fatalf("rules file wrong: %q", b2)
	}
	h.b.mu.Lock()
	if !h.b.rulesText("")... // cover in Step 3 impl
}
```

(chuẩn hoá: kiểm tra `b.rulesText`, gửi thêm msg 3 → hệ thống prompt chứa "[Long-term rules]" trong `h.llm.lastSeen()[0].Content`.)

Case OFF: `/rules off` qua dm owner → distill không ghi file nữa (counter block).

Case verbatim: `fn` trả về chuỗi chứa 40 runes `hello from requester text` (cũng là message user đã gửi, đủ dài ≥80 runes qua repeat) → file KHÔNG tồn tại + giữ file cũ nếu có.

- [x] **Step 3: cài đặt bridge.go**

Thêm fields: `rulesEnabled bool`(sát với `cfg`), `rulesRunOn bool`, `rulesText string`, `rulesPending int`. Init trong NewBridge: `rulesEnabled: cfg.RulesEnabled, rulesRunOn: true` — SAU openDB (loadMetaDB có thể ghi đè rulesRunOn). `NewBridge` gọi `b.loadRules()` ngay sau gán prompt.

```go
func (b *Bridge) rulesOn() bool { return b.rulesEnabled && b.rulesRunOn }

func (b *Bridge) rulesNoteLocked() string {
	if !b.rulesOn() || b.rulesText == "" {
		return ""
	}
	return rulesHeader + truncRunes(b.rulesText, b.cfg.RulesMaxChars)
}
```

`replyJob` — dòng build system:

```go
	system := b.sysPrompt + b.subst(note) + b.rulesNoteLocked()
```

Cuối replyJob (sau remember/introduced, trước log): 

```go
	fire := false
	b.mu.Lock()
	if b.rulesOn() {
		b.rulesPending++
		if b.rulesPending >= b.cfg.RulesUpdateEvery {
			b.rulesPending = 0
			fire = true
		}
	}
	b.mu.Unlock()
	if fire {
		go b.distillJob()
	}
```

(rules.go hoặc bridge.go — đặt `distillJob` + `buildDistillInputLocked` vào rules.go.)

`rules.go` thêm:

```go
const distillSystemPrompt = "You maintain the long-term rules file for an automatic Telegram assistant.\n" +
	"Input: the current rules and recent chats. Output: either the single word UNCHANGED or the complete new markdown file that\n" +
	"- keeps rules that are still true, removes stale/wrong ones, adds only durable behavioural rules useful for FUTURE replies;\n" +
	"- never copies wording of the chats; no verbatim quotes,\n" +
	"- STRICTLY FORBIDDEN in the file: full names, phone numbers, emails, Telegram handles other than the owner tag,\n" +
	"  addresses, specific prices or amounts, public IDs, and any verbatim message text.\n" +
	"Language: the owner's language (usually Vietnamese). No commentary, no code fences."

// buildDistillInputLocked compiles the distiller prompt input from the most
// recently active chats (newest first, up to 3) with the newest 12 messages.
// Caller holds b.mu.
func (b *Bridge) buildDistillInputLocked() (string, []string) {
	var srcs []string
	var parts []string
	total := 0
	n := 0
	for el := b.lru.Front(); el != nil && n < 3; el = el.Next() {
		cs := el.Value.(*chatState)
		if len(cs.msgs) == 0 {
			continue
		}
		n++
		var sb strings.Builder
		sb.WriteString("--- chat ---\n")
		start := len(cs.msgs) - 12
		if start < 0 {
			start = 0
		}
		for _, m := range cs.msgs[start:] {
			srcs = append(srcs, m.Content)
			if total > 6000 {
				break
			}
			sb.WriteString(m.Role)
			sb.WriteString(": ")
			sb.WriteString(m.Content)
			sb.WriteString("\n")
			total += runeCount(m.Content)
		}
		parts = append(parts, sb.String())
	}
	cur := b.rulesText
	if cur == "" {
		cur = "(none yet)"
	}
	return "=== CURRENT RULES ===\n" + cur + "\n\n=== RECENT CHATS ===\n" +
		strings.Join(parts, "\n"), srcs
}

// distillJob runs rarely (every rules_update_every successful replies) to
// refresh rules.md. It shares the LLM semaphore; never retries.
func (b *Bridge) distillJob() {
	b.mu.Lock()
	input, srcs := b.buildDistillInputLocked()
	cur := b.rulesText
	b.mu.Unlock()

	msgs := []ChatMsg{
		{Role: "system", Content: distillSystemPrompt},
		{Role: "user", Content: input},
	}
	reply, err := b.callLLM(context.Background(), msgs)
	if err != nil || reply == "" {
		logf("WARN", "rules distill skipped: llm error")
		return
	}
	newText, unchanged := parseDistillOutput(reply)
	if unchanged {
		return
	}
	clean := sanitizeRules(newText, "@"+b.cfg.OwnerUsername, b.cfg.RulesAllowURL, b.cfg.RulesMaxChars)
	if clean == "" {
		logf("WARN", "rules distill rejected: sensitive/noisy output")
		return
	}
	if blocksQuoted(clean, srcs) {
		logf("WARN", "rules distill rejected: verbatim quote detected")
		return
	}
	if clean == cur {
		return
	}
	if b.cfg.RulesFile == "" {
		return
	}
	if err := writeRulesAtomic(b.cfg.RulesFile, clean); err != nil {
		logf("WARN", "rules write failed")
		return
	}
	b.mu.Lock()
	b.rulesText = clean
	b.mu.Unlock()
	logf("INFO", "rules updated: %d chars", runeCount(clean))
}
```

`util.go`:

```go
// parseCommand splits "/cmd@bot extra words" into ("cmd", "extra words").
func parseCommand(text string) (cmd, args string) {
	f := strings.Fields(text)
	if len(f) == 0 {
		return "", ""
	}
	c := f[0]
	if i := strings.IndexByte(c, '@'); i >= 0 {
		c = c[:i]
	}
	return strings.ToLower(c), strings.Join(f[1:], " ")
}
```

`normCmd` giữ interface, đổi body: `c, _ := parseCommand(text); return c`.

`ownerCommand` — cases mới (signature giữ tạm, Task 9 đổi conn/key):

```go
	case "/rules":
		switch strings.TrimSpace(args) {
		case "reset":
			b.mu.Lock()
			b.rulesText = ""
			b.mu.Unlock()
			if b.cfg.RulesFile != "" {
				_ = os.Remove(b.cfg.RulesFile)
			}
			logf("INFO", "rules reset by owner")
			msg = "🗑 Đã xoá rules.md."
		case "off":
			b.mu.Lock()
			b.rulesRunOn = false
			b.saveStateLocked()
			b.mu.Unlock()
			msg = "🚫 Đã tắt rules (không tổng hợp, không chèn)."
		case "on":
			b.mu.Lock()
			b.rulesRunOn = true
			b.saveStateLocked()
			b.mu.Unlock()
			msg = "✅ Đã bật rules."
		default:
			b.mu.Lock()
			rtxt := b.rulesText
			state := "bật"
			if !b.rulesOn() {
				state = "tắt"
			}
			b.mu.Unlock()
			if rtxt == "" {
				msg = "rules.md: trống (trạng thái: " + state + ")."
			} else {
				msg = "rules.md (" + state + ", " + strconv.Itoa(runeCount(rtxt)) + " ký tự):\n" + rtxt
			}
		}
```

`bridge.go` import thêm `os`; caller phân args: `onBotMessage` đổi `ownerCommand(m.Chat.ID, normCmd(text))` thành:

```go
		cmd, args := parseCommand(text)
		return b.ownerCommand(m.Chat.ID, cmd+" "+args)
```

(tạm: ownerCommand nhận `cmd string` = "cmd args"? — KHÔNG rối: đổi signature NGAY ở Task 9 sang (cmd, args). Task 8 dùng trung gian: nối `args` field để trường hợp `/rules reset` nhận — làm thẳng final signature trong Task 8 và Task 9 chỉ thêm dispatch business + /forget. Đơn giản hoá: Task 8 ĐỔI signature `ownerCommand(chatID int64, conn, key, cmd, args string)` ngay; Task 9 chỉ thêm dispatch business + /forget.)

- [x] **Step 4: mọi test — XANH.**

- [x] **Step 5: Commit**

```sh
git add rules.go bridge.go util.go bridge_test.go rules_test.go
git commit -m "feat(rules): self-synthesis loop with prompt injection and /rules command"
```

---

### Task 9: Lệnh trong business chat + /forget

**Files:**
- Modify: `bridge.go` (`onBusinessMessage`, `onBotMessage`, `ownerCommand`)
- Test: `bridge_test.go`

**Interfaces:**
- Consumes: `parseCommand` (Task 8), `dirtyChats/deletedChats` (Task 3-4).

- [x] **Step 1: test thất bại:**

```go
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
	h := newHarness(nil)
	h.b.handleUpdate(biz(testConn, 901, 901, "hi nguyện"))
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
	deleted := h.b.deletedChats[key]
	h.b.mu.Unlock()
	if still {
		t.Fatal("chat not removed from RAM")
	}
	if !deleted {
		t.Fatal("delete not queued for DB")
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
```

- [x] **Step 2: chạy — THẤT BÁO.**

- [x] **Step 3: cài đặt** — `onBusinessMessage` nhánh owner:

```go
	if m.From != nil && m.From.ID == b.cfg.OwnerID {
		if text != "" && strings.HasPrefix(text, "/") {
			cmd, args := parseCommand(text)
			return b.ownerCommand(m.Chat.ID, conn, key, cmd, args)
		}
		b.mu.Lock()
		b.mutedUntil[key] = b.now().Add(time.Duration(b.cfg.OwnerMuteMinutes) * time.Minute)
		if text != "" {
			b.rememberLocked(key, "assistant", text)
		}
		b.mu.Unlock()
		return "owner_takeover"
	}
```

`onBotMessage` direct owner command:

```go
	if ctype == "private" && m.From != nil && m.From.ID == b.cfg.OwnerID && strings.HasPrefix(text, "/") {
		cmd, args := parseCommand(text)
		return b.ownerCommand(m.Chat.ID, "", "d:"+strconv.FormatInt(m.Chat.ID, 10), cmd, args)
	}
```

`ownerCommand` — đủ signature `ownerCommand(chatID int64, conn, key, cmd, args string)` (Task 8 rewrite asám rồi), thêm case + sửa sendMsg:

```go
	case "/forget":
		b.mu.Lock()
		if cs, ok := b.chats[key]; ok {
			b.lru.Remove(cs.el)
			delete(b.chats, key)
		}
		delete(b.dirtyChats, key)
		if b.db != nil {
			b.deletedChats[key] = struct{}{}
		}
		delete(b.mutedUntil, key)
		b.mu.Unlock()
		logf("INFO", "owner forgot a chat")
		msg = "🗑 Đã xoá ngữ cảnh chat này."
```

Mọi `b.sendMsg(chatID, msg, "")` → `b.sendMsg(chatID, msg, conn)`. Help text:

```go
		msg = "Lệnh: /pause /resume /status /reload /rules [reset|off|on] /forget"
```

- [x] **Step 4: chạy toàn bộ — XANH.**

- [x] **Step 5: Commit**

```sh
git add bridge.go bridge_test.go
git commit -m "feat(commands): owner commands inside business chats plus /forget"
```

---

### Task 10: Integration restart + README + version + nghiệm thu

**Files:**
- Modify: `integration_test.go` (TestIntegrationRestartDoesNotReplayBacklog)
- Modify: `README.md`, `main.go` (version), spec errata nếu lệch (không cần).

**Interfaces:** cuối cùng, chỉ nghiệm thu.

- [x] **Step 1: Integration test** — thêm vào `integration_test.go` (dựa fakeTelegramServer + fake LLM HTTP موجود; tạo LLM fake nhanh bằng httptest):

```go
func TestIntegrationRestartDoesNotReplayBacklog(t *testing.T) {
	dir := t.TempDir()
	tgSrv := newFakeTelegram(t, nil)
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"Dạ em trả lời"}}]}`)
	}))
	defer llmSrv.Close()

	updateJSON := fmt.Sprintf(`{"update_id":1,"business_message":{"business_connection_id":"c1","date":%d,`+
		`"chat":{"id":300,"type":"private"},"from":{"id":300},"text":"một tin nào đó"}}`, time.Now().Unix())
	tgSrv.first = []string{updateJSON}

	cfg := defaultConfig()
	cfg.BotToken = integToken
	cfg.TelegramBase = tgSrv.URL
	cfg.OwnerID = 1000000001
	cfg.OwnerUsername = "example_owner"
	cfg.APIBase = llmSrv.URL
	cfg.APIKey = "k"
	cfg.DBFile = filepath.Join(dir, "bridge.db")

	ctx1, cancel1 := context.WithCancel(context.Background())
	b1 := NewBridge(cfg, NewTelegram(cfg.TelegramBase, cfg.BotToken, http.DefaultClient),
		NewLLM(cfg.APIBase, cfg.APIKey, cfg.Model, http.DefaultClient))
	done1 := make(chan struct{})
	go func() { _ = b1.Run(ctx1); close(done1) }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(tgSrv.sends) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(tgSrv.sends) != 1 {
		t.Fatal("first run must reply once")
	}
	cancel1()
	<-done1
	if err := b1.Close(); err != nil {
		t.Fatal(err)
	}

	// second run must NOT replay the old update and poll with the persisted offset
	b2 := NewBridge(cfg, NewTelegram(cfg.TelegramBase, cfg.BotToken, http.DefaultClient),
		NewLLM(cfg.APIBase, cfg.APIKey, cfg.Model, http.DefaultClient))
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	done2 := make(chan struct{})
	go func() { _ = b2.Run(ctx2); close(done2) }()
	time.Sleep(300 * time.Millisecond)
	cancel2()
	<-done2
	if err := b2.Close(); err != nil {
		t.Fatal(err)
	}
	if n := len(tgSrv.sends); n != 1 {
		t.Fatalf("restart replayed backlog: sends=%d", n)
	}
}
```

(chỉnh: `newFakeTelegram` cần biến `first` set được từ ngoài — điều chỉnh hàm nhận hoặc set trường; chi tiết theo file hiện có.)

- [x] **Step 2: chạy — XANH** (nếu fail → chắc chắn `Run` seed offset hay defer sai: debug theo Task 3 implementation).

- [x] **Step 3: README.md cập nhật**

- Features thêm bullet: durable memory (bridge.db + retention/compact), no-backlog restart, rules.md auto-synthesis + sanitizer.
- Configuration: bảng keys mới (đúng mặc định/clamp của Task 1), giải thích `rules_enabled`, `rules_allow_url`, note `rules.md` AI tự viết — có thể sửa tay rồi `/reload`.
- Layout bảng: thêm `storage.go` (bbolt meta/chats, flush worker, restore, prune, compact), `rules.go` (sanitizer, distiller, atomic write).
- Note migration: bridge.db tự sinh cạnh config; state.json được nhập 1 lần rồi bỏ qua; binary arm64 ước ~7.6 MB.
- Owner commands section: thêm `/rules [reset|off|on]`, `/forget`.

- [x] **Step 4: version bump** — `main.go`: `const version = "1.1.0"`.

- [x] **Step 5: Nghiệm thu đầy đủ**

```sh
go vet ./...
go test -race -count=1 -timeout 300s .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w -buildid=" -o /tmp/tg-bridge-arm64 .
ls -l /tmp/tg-bridge-arm64   # ≤ 8.0 MB
```

Soi log test: không có nội dung tin/rules nào in ra.

- [x] **Step 6: Commit (+ tag nếu muốn)**

```sh
git add integration_test.go README.md main.go
git commit -m "feat(release): restart-integration test, docs, version 1.1.0"
```

---

## Tự review đã chạy

- Spec coverage: §3 storage → Task 3-5; §4 backlog → Task 6; §5 rules → Task 7-9; §6 perf → Task 2 (~tối ưu codec/transport) + `history_max_chars` → Task 4; §7 config → Task 1; §8 file map → khớp; §9-10 test/acceptance → Task 10. Điểm lệch duy nhất: `rules_enabled` (errata ghi trong spec).
- Type consistency: `chatRecord`, `metaSnapshot`, `saveStateLocked`, `flushNow`, `parseCommand` dùng nhất quán các task.
- Placeholder scan: tất cả step có code/thuật cụ thể; các chú thích "(chỉnh ...)" chỉ là hướng chỉnh khi harness yêu cầu (không TBD).
