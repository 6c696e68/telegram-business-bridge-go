package main

import (
	"context"
	"fmt"
	"runtime"
	"time"
)

// runtimeStats is the non-sensitive health snapshot exposed by the optional
// health endpoint and the periodic stats log. It never contains chat content.
type runtimeStats struct {
	RSSBytes    uint64 `json:"rss_bytes"`
	HeapBytes   uint64 `json:"heap_bytes"`
	Goroutines  int    `json:"goroutines"`
	DBBytes     int64  `json:"db_bytes"`
	ChatsRAM    int    `json:"chats_ram"`
	ChatsDB     int    `json:"chats_db"`
	DayCount    int    `json:"messages_today"`
	RepliesSent int    `json:"replies_sent"`
	RulesChars  int    `json:"rules_chars"`
	Paused      bool   `json:"paused"`
	RulesOn     bool   `json:"rules_on"`
	DBError     bool   `json:"db_error"`
}

func (b *Bridge) snapshotRuntime() runtimeStats {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	dbBytes, chatsDB := b.storageStats()
	b.mu.Lock()
	st := runtimeStats{
		RSSBytes:    processRSS(),
		HeapBytes:   ms.HeapAlloc,
		Goroutines:  runtime.NumGoroutine(),
		DBBytes:     dbBytes,
		ChatsRAM:    len(b.chats),
		ChatsDB:     chatsDB,
		DayCount:    b.dayCount,
		RepliesSent: b.repliesSent,
		RulesChars:  runeCount(b.rulesText) + runeCount(b.rulesPinned),
		Paused:      b.paused,
		RulesOn:     b.rulesOn(),
		DBError:     b.dbErr != nil,
	}
	b.mu.Unlock()
	return st
}

func (st runtimeStats) line() string {
	return fmt.Sprintf("rss_mb=%.1f heap_mb=%.1f goroutines=%d db_mb=%.2f chats_ram=%d chats_db=%d messages_today=%d replies_sent=%d rules_chars=%d paused=%t",
		float64(st.RSSBytes)/(1<<20), float64(st.HeapBytes)/(1<<20),
		st.Goroutines, float64(st.DBBytes)/(1<<20),
		st.ChatsRAM, st.ChatsDB, st.DayCount, st.RepliesSent, st.RulesChars, st.Paused)
}

// statsLoop logs one non-sensitive metric line every stats_log_min minutes
// (0 disables it). Numbers only: no message text, tokens or keys.
func (b *Bridge) statsLoop(ctx context.Context) {
	if b.cfg.StatsLogMin <= 0 {
		return
	}
	t := time.NewTicker(time.Duration(b.cfg.StatsLogMin) * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			logf("INFO", "stats %s", b.snapshotRuntime().line())
		}
	}
}
