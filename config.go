package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Config mirrors the Python bridge's config.json so the same file works for both.
type Config struct {
	BotToken         string   `json:"bot_token"`
	TelegramBase     string   `json:"telegram_base"` // override only for tests
	APIBase          string   `json:"api_base"`
	APIKey           string   `json:"api_key"`
	Model            string   `json:"model"`
	OwnerID          int64    `json:"owner_id"`
	OwnerUsername    string   `json:"owner_username"`
	OwnerMuteMinutes int      `json:"owner_mute_minutes"`
	HistoryMessages  int      `json:"history_messages"`
	PerUserPerHour   int      `json:"per_user_per_hour"`
	DailyReplyCap    int      `json:"daily_reply_cap"`
	LLMTimeoutSec    int      `json:"llm_timeout"`
	MaxInputChars    int      `json:"max_input_chars"`
	MaxReplyChars    int      `json:"max_reply_chars"`
	SystemPromptFile string   `json:"system_prompt_file"`
	StateFile        string   `json:"state_file"`
	MaxConcurrentLLM int      `json:"max_concurrent_llm"`
	MaxChats         int      `json:"max_chats"`
	LLMRetries       int      `json:"llm_retries"`
	FallbackModels   []string `json:"fallback_models"`
	ShutdownDrainSec int      `json:"shutdown_drain_sec"`
	DebounceMS       int      `json:"debounce_ms"`
	DebounceMaxMS    int      `json:"debounce_max_ms"`
	OwnerDebounceMS  int      `json:"owner_debounce_ms"`
	Vision           bool     `json:"vision"`
	VisionModel      string   `json:"vision_model"` // empty = the main model handles images too
	VisionAPIBase    string   `json:"vision_api_base"`
	VisionAPIKey     string   `json:"vision_api_key"`
	TranscribeModel  string   `json:"transcribe_model"` // empty = voice notes are not transcribed
	TranscribeBase   string   `json:"transcribe_api_base"`
	TranscribeKey    string   `json:"transcribe_api_key"`
	MaxVoiceSec      int      `json:"max_voice_sec"`
	QuoteURL         string   `json:"quote_url"` // empty = the price tool is off
	QuoteTimeoutMS   int      `json:"quote_timeout_ms"`
	QuoteCacheSec    int      `json:"quote_cache_sec"`
	OwnerAwayMinutes int      `json:"owner_away_minutes"`
	FollowupMaxMin   int      `json:"followup_max_min"`
	MaxTokens        int      `json:"max_tokens"`
	Temperature      float64  `json:"temperature"`
	DBFile           string   `json:"db_file"`
	FlushSec         int      `json:"flush_sec"`
	MemoryDays       int      `json:"memory_days"`
	MaxStoredChats   int      `json:"max_stored_chats"`
	MaxBacklogMin    int      `json:"max_backlog_min"`
	HistoryMaxChars  int      `json:"history_max_chars"`
	RulesEnabled     bool     `json:"rules_enabled"`
	RulesFile        string   `json:"rules_file"`
	RulesPinnedFile  string   `json:"rules_pinned_file"`
	RulesMaxChars    int      `json:"rules_max_chars"`
	RulesUpdateEvery int      `json:"rules_update_every"`
	StatsLogMin      int      `json:"stats_log_min"`
	HealthAddr       string   `json:"health_addr"`
	RulesAllowURL    bool     `json:"rules_allow_url"`
	DBCompactMB      int      `json:"db_compact_mb"`
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
		ShutdownDrainSec: 15,
		DebounceMS:       700,
		DebounceMaxMS:    2500,
		OwnerDebounceMS:  1500,
		Vision:           true,
		MaxVoiceSec:      120,
		QuoteTimeoutMS:   3000,
		QuoteCacheSec:    10,
		OwnerAwayMinutes: 30,
		FollowupMaxMin:   0, // opt-in: answering messages sent while the owner was online
		MaxTokens:        1200,
		StatsLogMin:      60,
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
	if cfg.RulesPinnedFile == "" {
		cfg.RulesPinnedFile = filepath.Join(filepath.Dir(path), "rules.pinned.md")
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
	if cfg.ShutdownDrainSec < 0 {
		cfg.ShutdownDrainSec = 0
	}
	if cfg.StatsLogMin < 0 {
		cfg.StatsLogMin = 0
	}
	if cfg.DebounceMS < 0 {
		cfg.DebounceMS = 0
	}
	if cfg.DebounceMaxMS < 0 {
		cfg.DebounceMaxMS = 0
	}
	if cfg.DebounceMaxMS > 0 && cfg.DebounceMaxMS < cfg.DebounceMS {
		cfg.DebounceMaxMS = cfg.DebounceMS
	}
	if cfg.OwnerDebounceMS < 0 {
		cfg.OwnerDebounceMS = 0
	}
	if cfg.OwnerAwayMinutes < 0 {
		cfg.OwnerAwayMinutes = 0
	}
	if cfg.FollowupMaxMin < 0 {
		cfg.FollowupMaxMin = 0
	}
	clamp(&cfg.MaxVoiceSec, 1)
	if cfg.MaxVoiceSec > 300 {
		cfg.MaxVoiceSec = 300
	}
	clamp(&cfg.QuoteTimeoutMS, 200)
	if cfg.QuoteTimeoutMS > 15000 {
		cfg.QuoteTimeoutMS = 15000
	}
	if cfg.QuoteCacheSec < 0 {
		cfg.QuoteCacheSec = 0
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = 1200
	} else if cfg.MaxTokens < 16 {
		cfg.MaxTokens = 16
	}
	if cfg.Temperature < 0 {
		cfg.Temperature = 0
	}
	if cfg.Temperature > 2 {
		cfg.Temperature = 2
	}
	return cfg, nil
}

// modelChain returns the primary model followed by configured fallbacks,
// de-duplicated while preserving order.
func (c Config) modelChain() []string {
	seen := make(map[string]bool, len(c.FallbackModels)+1)
	chain := make([]string, 0, len(c.FallbackModels)+1)
	add := func(m string) {
		if m == "" || seen[m] {
			return
		}
		seen[m] = true
		chain = append(chain, m)
	}
	add(c.Model)
	for _, m := range c.FallbackModels {
		add(m)
	}
	return chain
}
