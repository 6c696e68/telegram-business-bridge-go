package main

import (
	"bytes"
	"context"
	"encoding/json"
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
	url   string
	key   string
	model string
	hc    *http.Client
}

func NewLLM(apiBase, key, model string, hc *http.Client) *LLM {
	return &LLM{url: strings.TrimRight(apiBase, "/") + "/chat/completions", key: key, model: model, hc: hc}
}

// Chat sends an OpenAI-compatible chat completion request.
func (l *LLM) Chat(ctx context.Context, msgs []ChatMsg) (string, error) {
	body, err := json.Marshal(map[string]any{"model": l.model, "messages": msgs, "max_tokens": 1200})
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
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", &LLMError{Kind: "read"}
	}
	if resp.StatusCode != http.StatusOK {
		return "", &LLMError{Status: resp.StatusCode, Kind: "http"}
	}
	var d struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &d); err != nil || len(d.Choices) == 0 {
		return "", &LLMError{Kind: "bad_response"}
	}
	return strings.TrimSpace(d.Choices[0].Message.Content), nil
}
