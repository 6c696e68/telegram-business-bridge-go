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
