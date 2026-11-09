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
	return cfg, nil
}
