package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Config mirrors the Python bridge's config.json so the same file works for both.
type Config struct {
	BotToken         string `json:"bot_token"`
	TelegramBase     string `json:"telegram_base"` // override only for tests
	APIBase          string `json:"api_base"`
	APIKey           string `json:"api_key"`
	Model            string `json:"model"`
	OwnerID          int64  `json:"owner_id"`
	OwnerUsername    string `json:"owner_username"`
	OwnerMuteMinutes int    `json:"owner_mute_minutes"`
	HistoryMessages  int    `json:"history_messages"`
	PerUserPerHour   int    `json:"per_user_per_hour"`
	DailyReplyCap    int    `json:"daily_reply_cap"`
	LLMTimeoutSec    int    `json:"llm_timeout"`
	MaxInputChars    int    `json:"max_input_chars"`
	MaxReplyChars    int    `json:"max_reply_chars"`
	SystemPromptFile string `json:"system_prompt_file"`
	StateFile        string `json:"state_file"`
	MaxConcurrentLLM int    `json:"max_concurrent_llm"`
	MaxChats         int    `json:"max_chats"`
	LLMRetries       int    `json:"llm_retries"`
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
}

func defaultConfig() Config {
	return Config{
		TelegramBase:     "https://api.telegram.org",
		Model:            "gpt-6-luna",
		OwnerMuteMinutes: 30,
		HistoryMessages:  20,
		PerUserPerHour:   20,
		DailyReplyCap:    500,
		LLMTimeoutSec:    120,
		MaxInputChars:    4000,
		MaxReplyChars:    3500,
		MaxConcurrentLLM: 8,
		MaxChats:         500,
		LLMRetries:       2,
		FlushSec:         30,
		MemoryDays:       7,
		MaxStoredChats:   1000,
		MaxBacklogMin:    10,
		HistoryMaxChars:  8000,
		RulesEnabled:     true,
		RulesMaxChars:    4000,
		RulesUpdateEvery: 10,
		DBCompactMB:      20,
	}
}

// loadConfig reads config.json. Error messages never include file contents.
func loadConfig(path string) (Config, error) {
	cfg := defaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, errors.New("config.json is not valid JSON")
	}
	if cfg.BotToken == "" {
		return cfg, errors.New("bot_token missing")
	}
	if cfg.OwnerID <= 0 {
		return cfg, errors.New("owner_id missing")
	}
	if cfg.APIBase == "" || cfg.APIKey == "" {
		return cfg, errors.New("api_base/api_key missing")
	}
	if cfg.OwnerUsername == "" {
		cfg.OwnerUsername = "owner"
	}
	if cfg.SystemPromptFile == "" {
		cfg.SystemPromptFile = filepath.Join(filepath.Dir(path), "system_prompt.txt")
	}
	if cfg.DBFile == "" {
		cfg.DBFile = filepath.Join(filepath.Dir(path), "bridge.db")
	}
	if cfg.RulesFile == "" {
		cfg.RulesFile = filepath.Join(filepath.Dir(path), "rules.md")
	}
	if cfg.TelegramBase == "" {
		cfg.TelegramBase = "https://api.telegram.org"
	}
	clamp := func(v *int, min int) {
		if *v < min {
			*v = min
		}
	}
	clamp(&cfg.MaxConcurrentLLM, 1)
	clamp(&cfg.MaxChats, 1)
	clamp(&cfg.HistoryMessages, 1)
	clamp(&cfg.LLMTimeoutSec, 1)
	clamp(&cfg.LLMRetries, 0)
	clamp(&cfg.MaxInputChars, 1)
	clamp(&cfg.MaxReplyChars, 1)
	clamp(&cfg.FlushSec, 5)
	clamp(&cfg.MemoryDays, 1)
	if cfg.MaxStoredChats < cfg.MaxChats {
		cfg.MaxStoredChats = cfg.MaxChats
	}
	if cfg.MaxBacklogMin < 0 {
		cfg.MaxBacklogMin = 0
	}
	clamp(&cfg.HistoryMaxChars, 200)
	clamp(&cfg.RulesMaxChars, 200)
	if cfg.RulesUpdateEvery < 2 {
		cfg.RulesUpdateEvery = 2
	}
	return cfg, nil
}
