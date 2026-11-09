package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type ChatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// LLMError never contains the API key or URL. Kind is one of:
// http | timeout | canceled | conn | read | bad_response
type LLMError struct {
	Status int
	Kind   string
}

func (e *LLMError) Error() string {
	return fmt.Sprintf("llm error status=%d kind=%s", e.Status, e.Kind)
}

type LLM struct {
	url    string
	key    string
	models []string
	hc     *http.Client
}

// NewLLM keeps the single-model constructor used by tests and callers that
// do not need a fallback chain.
func NewLLM(apiBase, key, model string, hc *http.Client) *LLM {
	return NewLLMChain(apiBase, key, []string{model}, hc)
}

// NewLLMChain builds a client that tries models in order. A fallback is only
// attempted for retryable provider answers (429/5xx); config/auth errors stop
// immediately so a bad key cannot hammer every model.
func NewLLMChain(apiBase, key string, models []string, hc *http.Client) *LLM {
	if len(models) == 0 {
		models = []string{""}
	}
	return &LLM{
		url:    strings.TrimRight(apiBase, "/") + "/chat/completions",
		key:    key,
		models: models,
		hc:     hc,
	}
}

// Chat sends an OpenAI-compatible chat completion request, advancing through
// the fallback chain on retryable provider failures.
func (l *LLM) Chat(ctx context.Context, msgs []ChatMsg) (string, error) {
	var lastErr error
	for i, model := range l.models {
		reply, err := l.chatOnce(ctx, model, msgs)
		if err == nil {
			return reply, nil
		}
		lastErr = err
		if i == len(l.models)-1 {
			break
		}
		var le *LLMError
		if errors.As(err, &le) && le.Kind == "http" && (le.Status == 429 || le.Status >= 500) {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			continue
		}
		return "", err
	}
	return "", lastErr
}

func (l *LLM) chatOnce(ctx context.Context, model string, msgs []ChatMsg) (string, error) {
	body, err := json.Marshal(map[string]any{"model": model, "messages": msgs, "max_tokens": 1200})
	if err != nil {
		return "", &LLMError{Kind: "bad_response"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.url, bytes.NewReader(body))
	if err != nil {
		return "", &LLMError{Kind: "conn"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+l.key)
	resp, err := l.hc.Do(req)
	if err != nil {
		return "", &LLMError{Kind: netKind(err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", &LLMError{Status: resp.StatusCode, Kind: "http"}
	}
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
	return strings.TrimSpace(d.Choices[0].Message.Content), nil
}
