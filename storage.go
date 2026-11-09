package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	bucketMeta  = []byte("meta")
	bucketChats = []byte("chats")
)

const metaSchema = "1"

// chatRecord is the JSON blob stored under each chat key.
type chatRecord struct {
	Introduced bool      `json:"introduced"`
	Updated    int64     `json:"updated"`
	Msgs       []ChatMsg `json:"msgs"`
}

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

// chatRunes counts total content code points of a chat history.
func chatRunes(msgs []ChatMsg) int {
	total := 0
	for i := range msgs {
		total += runeCount(msgs[i].Content)
	}
	return total
}

// openDB opens the store, creates buckets, and runs the one-time migration
// from the legacy state.json. Failures set b.dbErr so main() can exit clearly;
// db underrun (nil) keeps everything running in memory-only mode.
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
		empty = tx.Bucket(bucketMeta).Stats().KeyN == 0 &&
			tx.Bucket(bucketChats).Stats().KeyN == 0
		return nil
	})
	// One-time migration from the legacy state.json only while the DB is empty.
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
		if mb == nil || len(mb.Get([]byte("schema"))) == 0 {
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

// loadLegacyState reads the old state.json in memory-only mode.
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

// saveStateLocked persists meta. DB mode just marks dirty (the flush worker
// does the IO); memory-only mode keeps the legacy atomic state.json write.
// Caller holds b.mu.
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
// only writer of the database (single-writer invariant).
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
			recs[k] = chatRecord{
				Introduced: cs.introduced,
				Updated:    cs.updated,
				Msgs:       append([]ChatMsg(nil), cs.msgs...),
			}
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

// flushAll snapshots every chat plus meta (shutdown path).
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

// flushLoop writes at a fixed cadence while anything is dirty; it is the only
// DB writer besides Close.
func (b *Bridge) flushLoop(ctx context.Context) {
	interval := time.Duration(b.cfg.FlushSec) * time.Second
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}
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

// Close flushes everything and closes the database. No-op in memory-only mode.
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
	all := make([]kvRec, 0, 32)
	_ = ck.ForEach(func(k, v []byte) error {
		var r chatRecord
		if json.Unmarshal(v, &r) == nil {
			all = append(all, kvRec{string(k), r})
		} else {
			all = append(all, kvRec{string(k), chatRecord{}}) // invalid → expired
		}
		return nil
	})
	survivors := make([]kvRec, 0, len(all))
	for _, it := range all {
		if it.rec.Updated >= cutoff {
			survivors = append(survivors, it)
		} else {
			_ = ck.Delete([]byte(it.key))
		}
	}
	extra := len(survivors) - cfg.MaxStoredChats
	if extra > 0 {
		sort.Slice(survivors, func(i, j int) bool {
			return survivors[i].rec.Updated > survivors[j].rec.Updated
		})
		for _, it := range survivors[len(survivors)-extra:] {
			_ = ck.Delete([]byte(it.key))
		}
	}
}

// restoreChats loads chat records newest-first into the LRU. RAM is capped by
// MaxChats (normal eviction); the disk copy stays until memory_days expires.
func (b *Bridge) restoreChats() {
	if b.db == nil {
		return
	}
	type kvRec struct {
		key string
		rec chatRecord
	}
	all := make([]kvRec, 0, 32)
	_ = b.db.View(func(tx *bolt.Tx) error {
		ck := tx.Bucket(bucketChats)
		if ck == nil {
			return nil
		}
		return ck.ForEach(func(k, v []byte) error {
			var r chatRecord
			if e := json.Unmarshal(v, &r); e == nil {
				all = append(all, kvRec{string(k), r})
			}
			return nil
		})
	})
	sort.Slice(all, func(i, j int) bool { return all[i].rec.Updated > all[j].rec.Updated })
	cutoff := b.now().Add(-time.Duration(b.cfg.MemoryDays) * 24 * time.Hour).Unix()
	b.mu.Lock()
	for _, it := range all {
		if it.rec.Updated < cutoff {
			b.deletedChats[it.key] = struct{}{}
			continue
		}
		cs := &chatState{
			key:        it.key,
			introduced: it.rec.Introduced,
			updated:    it.rec.Updated,
			msgs:       it.rec.Msgs,
		}
		if cs.msgs == nil {
			cs.msgs = []ChatMsg{}
		}
		cs.runes = chatRunes(cs.msgs)
		cs.el = b.lru.PushFront(cs)
		b.chats[it.key] = cs
	}
	b.mu.Unlock()
	// Keep RAM within MaxChats; evicted records stay on disk until they expire.
	b.mu.Lock()
	b.evictLocked("")
	b.mu.Unlock()
}
