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

// recordMeta decodes only the timestamp; used by prune/restore scans so bulk
// scans do not allocate whole histories.
type recordMeta struct {
	Updated int64 `json:"updated"`
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

// markChatPendingLocked snapshots the chat for the next flush. Taking the copy
// at mutation time (not at flush time) guarantees an evicted chat is still
// persisted, and never reads a half-mutated history. Caller holds b.mu.
func (b *Bridge) markChatPendingLocked(key string, cs *chatState) {
	b.pendingChats[key] = chatRecord{
		Introduced: cs.introduced,
		Updated:    cs.updated,
		Msgs:       append([]ChatMsg(nil), cs.msgs...),
	}
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
	if b.persistent {
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

// flushNow writes pending meta/chats, deletions and pruning in one
// transaction. It is the only writer of the database besides boot-time setup.
// Pending state is restored on failure so nothing is silently lost.
func (b *Bridge) flushNow() error {
	b.flushMu.Lock()
	defer b.flushMu.Unlock()
	if b.db == nil {
		return nil
	}

	b.mu.Lock()
	wantMeta := b.dirtyMeta
	var ms metaSnapshot
	if wantMeta {
		b.dirtyMeta = false
		ms = metaSnapshotFromLocked(b)
	}
	pend := b.pendingChats
	b.pendingChats = map[string]chatRecord{}
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
		for k, r := range pend {
			v, e := json.Marshal(r)
			if e != nil {
				continue
			}
			if e := ck.Put([]byte(k), v); e != nil {
				return e
			}
		}
		for k := range del {
			if e := ck.Delete([]byte(k)); e != nil {
				return e
			}
		}
		pruneChatsTx(tx, b.now(), b.cfg)
		return nil
	})
	if err != nil {
		logf("WARN", "db flush: %s", err)
		// Requeue pending work; newer mutations (which set the maps again
		// while the tx ran) win over this stale snapshot.
		b.mu.Lock()
		if wantMeta {
			b.dirtyMeta = true
		}
		for k, v := range pend {
			if _, newer := b.pendingChats[k]; !newer {
				if _, gone := b.deletedChats[k]; !gone {
					b.pendingChats[k] = v
				}
			}
		}
		for k := range del {
			b.deletedChats[k] = struct{}{}
		}
		b.mu.Unlock()
	}
	return err
}

// flushAll snapshots every in-memory chat plus meta (shutdown path).
func (b *Bridge) flushAll() error {
	b.mu.Lock()
	b.dirtyMeta = true
	for k, cs := range b.chats {
		b.markChatPendingLocked(k, cs)
	}
	b.mu.Unlock()
	return b.flushNow()
}

// flushLoop writes at a fixed cadence while anything is pending; it is the
// only DB writer during normal operation.
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
			_ = b.flushNow()
		}
	}
}

// Close joins the write path: final flush, then detach and close the DB.
// Callers must stop the flush loop first (main joins it explicitly).
func (b *Bridge) Close() error {
	if !b.persistent {
		return nil
	}
	if err := b.flushAll(); err != nil {
		logf("WARN", "final flush failed: %s", err)
	}
	b.flushMu.Lock()
	db := b.db
	if db != nil {
		b.db = nil
	}
	b.flushMu.Unlock()
	if db == nil {
		return nil
	}
	return db.Close()
}

// pruneChatsTx deletes expired and surplus chat records inside tx, reading
// only each record's timestamp.
func pruneChatsTx(tx *bolt.Tx, now time.Time, cfg Config) {
	ck := tx.Bucket(bucketChats)
	if ck == nil {
		return
	}
	cutoff := now.Add(-time.Duration(cfg.MemoryDays) * 24 * time.Hour).Unix()
	type keyTS struct {
		key string
		ts  int64
	}
	all := make([]keyTS, 0, 64)
	_ = ck.ForEach(func(k, v []byte) error {
		var m recordMeta
		if json.Unmarshal(v, &m) != nil {
			all = append(all, keyTS{string(k), 0}) // invalid → expired
			return nil
		}
		all = append(all, keyTS{string(k), m.Updated})
		return nil
	})
	survivors := make([]keyTS, 0, len(all))
	for _, it := range all {
		if it.ts >= cutoff {
			survivors = append(survivors, it)
		} else {
			_ = ck.Delete([]byte(it.key))
		}
	}
	extra := len(survivors) - cfg.MaxStoredChats
	if extra > 0 {
		sort.Slice(survivors, func(i, j int) bool { return survivors[i].ts > survivors[j].ts })
		for _, it := range survivors[len(survivors)-extra:] {
			_ = ck.Delete([]byte(it.key))
		}
	}
}

// restoreChats loads the newest MaxChats chats from disk into the LRU. The
// first pass reads timestamps only; full histories are decoded solely for the
// chats that fit in RAM, so restore memory stays bounded by MaxChats.
func (b *Bridge) restoreChats() {
	if b.db == nil {
		return
	}
	type keyTS struct {
		key string
		ts  int64
	}
	all := make([]keyTS, 0, 64)
	_ = b.db.View(func(tx *bolt.Tx) error {
		ck := tx.Bucket(bucketChats)
		if ck == nil {
			return nil
		}
		return ck.ForEach(func(k, v []byte) error {
			var m recordMeta
			if e := json.Unmarshal(v, &m); e != nil {
				b.mu.Lock()
				b.deletedChats[string(k)] = struct{}{}
				b.mu.Unlock()
				return nil
			}
			all = append(all, keyTS{string(k), m.Updated})
			return nil
		})
	})
	cutoff := b.now().Add(-time.Duration(b.cfg.MemoryDays) * 24 * time.Hour).Unix()
	sort.Slice(all, func(i, j int) bool { return all[i].ts > all[j].ts })
	fresh := all[:0]
	for _, it := range all {
		if it.ts < cutoff {
			b.mu.Lock()
			b.deletedChats[it.key] = struct{}{}
			b.mu.Unlock()
			continue
		}
		fresh = append(fresh, it)
	}
	if len(fresh) > b.cfg.MaxChats {
		fresh = fresh[:b.cfg.MaxChats]
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	_ = b.db.View(func(tx *bolt.Tx) error {
		ck := tx.Bucket(bucketChats)
		if ck == nil {
			return nil
		}
		// oldest first so the newest ends at the LRU front
		for i := len(fresh) - 1; i >= 0; i-- {
			raw := ck.Get([]byte(fresh[i].key))
			if raw == nil {
				continue
			}
			var rec chatRecord
			if e := json.Unmarshal(raw, &rec); e != nil {
				continue
			}
			cs := &chatState{
				key:        fresh[i].key,
				introduced: rec.Introduced,
				updated:    rec.Updated,
				msgs:       rec.Msgs,
			}
			if cs.msgs == nil {
				cs.msgs = []ChatMsg{}
			}
			cs.runes = chatRunes(cs.msgs)
			cs.el = b.lru.PushFront(cs)
			b.chats[cs.key] = cs
		}
		return nil
	})
}

// maybeCompact rewrites the DB when it exceeds db_compact_mb (boot only, while
// nothing else holds the file). Any failure is non-fatal: the old file stays.
func (b *Bridge) maybeCompact() {
	if b.db == nil || b.cfg.DBCompactMB <= 0 {
		return
	}
	fi, err := os.Stat(b.cfg.DBFile)
	if err != nil || fi.Size() < int64(b.cfg.DBCompactMB)<<20 {
		return
	}
	tmp := b.cfg.DBFile + ".compact"
	_ = os.Remove(tmp)
	dst, err := bolt.Open(tmp, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		logf("WARN", "db compact: temp open failed")
		return
	}
	if err := bolt.Compact(dst, b.db, 0); err != nil {
		_ = dst.Close()
		_ = os.Remove(tmp)
		logf("WARN", "db compact failed: %s", err)
		return
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(tmp)
		logf("WARN", "db compact close failed")
		return
	}
	if err := b.db.Close(); err != nil {
		logf("WARN", "db compact old close failed")
		b.reopenDB()
		_ = os.Remove(tmp)
		return
	}
	if err := os.Rename(tmp, b.cfg.DBFile); err != nil {
		logf("WARN", "db compact rename failed")
		b.reopenDB()
		return
	}
	if nd, err := bolt.Open(b.cfg.DBFile, 0o600, &bolt.Options{Timeout: 5 * time.Second}); err == nil {
		b.db = nd
		sz, _ := os.Stat(b.cfg.DBFile)
		logf("INFO", "database compacted, size=%d", sz.Size())
	} else {
		logf("ERROR", "db reopen after compact failed")
		b.db = nil
		b.dbErr = err
	}
}

// reopenDB reopens the current DB file after a failed compact swap.
func (b *Bridge) reopenDB() {
	nd, err := bolt.Open(b.cfg.DBFile, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		b.db = nil
		b.dbErr = err
		return
	}
	b.db = nd
}
