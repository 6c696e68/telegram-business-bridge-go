package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
)

type ChatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// Images are data: URLs sent to vision models with this message only.
	// They are never stored in history or on disk.
	Images []string `json:"-"`
}

// MarshalJSON emits the plain OpenAI form, or the multimodal content array
// when images are attached.
func (m ChatMsg) MarshalJSON() ([]byte, error) {
	if len(m.Images) == 0 {
		return json.Marshal(struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{m.Role, m.Content})
	}
	type urlPart struct {
		URL string `json:"url"`
	}
	type part struct {
		Type     string   `json:"type"`
		Text     string   `json:"text,omitempty"`
		ImageURL *urlPart `json:"image_url,omitempty"`
	}
	parts := []part{{Type: "text", Text: m.Content}}
	for _, u := range m.Images {
		parts = append(parts, part{Type: "image_url", ImageURL: &urlPart{URL: u}})
	}
	return json.Marshal(struct {
		Role    string `json:"role"`
		Content []part `json:"content"`
	}{m.Role, parts})
}

// LLMError never contains the API key or URL. Kind is one of:
// http | timeout | canceled | conn | read | bad_response
type LLMError struct {
	Status int
	Kind   string
	// detail is a bounded, lower-cased provider error body used only to
	// classify failures; it is never logged or shown.
	detail string
}

func (e *LLMError) Error() string {
	return fmt.Sprintf("llm error status=%d kind=%s", e.Status, e.Kind)
}

type LLM struct {
	url         string
	key         string
	models      []string
	hc          *http.Client
	MaxTokens   int     // <=0 keeps the built-in default
	Temperature float64 // 0 omits the field (provider default)
	lastOK      atomic.Int64
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
		url:       strings.TrimRight(apiBase, "/") + "/chat/completions",
		key:       key,
		models:    models,
		hc:        hc,
		MaxTokens: 1200,
	}
}

// walkOrder rotates the chain so the model that answered last time is tried
// first. Without this, every retry during a provider incident re-hits the
// failing primary and multiplies provider load.
func (l *LLM) walkOrder() []int {
	n := len(l.models)
	start := int(l.lastOK.Load())
	if start < 0 || start >= n {
		start = 0
	}
	order := make([]int, 0, n)
	for i := 0; i < n; i++ {
		order = append(order, (start+i)%n)
	}
	return order
}

// Chat sends an OpenAI-compatible chat completion request, advancing through
// the fallback chain on retryable provider failures (429/5xx only).
func (l *LLM) Chat(ctx context.Context, msgs []ChatMsg) (string, error) {
	order := l.walkOrder()
	var lastErr error
	for pos, idx := range order {
		reply, err := l.chatOnce(ctx, l.models[idx], msgs)
		if err == nil {
			l.lastOK.Store(int64(idx))
			return reply, nil
		}
		lastErr = err
		// Unified cancellation semantics for callers and tests.
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if pos == len(order)-1 {
			break
		}
		var le *LLMError
		if errors.As(err, &le) && le.Kind == "http" && (le.Status == 429 || le.Status >= 500) {
			continue
		}
		return "", err
	}
	return "", lastErr
}

func (l *LLM) chatOnce(ctx context.Context, model string, msgs []ChatMsg) (string, error) {
	maxTokens := l.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 1200
	}
	body, err := encodeChatRequest(model, msgs, maxTokens, l.Temperature)
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
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return "", &LLMError{Status: resp.StatusCode, Kind: "http", detail: strings.ToLower(string(raw))}
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

// encodeChatRequest writes the request into one pre-sized buffer. Image data
// URLs (base64, possibly megabytes) are copied once instead of going through
// json.Marshal's intermediate buffers, which keeps memory flat on small
// routers. The output is the same JSON the encoding/json form would produce.
func encodeChatRequest(model string, msgs []ChatMsg, maxTokens int, temperature float64) ([]byte, error) {
	size := 256 + len(model)
	for _, m := range msgs {
		size += len(m.Content)*2 + 64
		for _, u := range m.Images {
			size += len(u) + 64
		}
	}
	buf := bytes.NewBuffer(make([]byte, 0, size))
	str := func(v string) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		buf.Write(b)
		return nil
	}
	buf.WriteString(`{"model":`)
	if err := str(model); err != nil {
		return nil, err
	}
	buf.WriteString(`,"messages":[`)
	for i, m := range msgs {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(`{"role":`)
		if err := str(m.Role); err != nil {
			return nil, err
		}
		buf.WriteString(`,"content":`)
		if len(m.Images) == 0 {
			if err := str(m.Content); err != nil {
				return nil, err
			}
		} else {
			buf.WriteString(`[{"type":"text","text":`)
			if err := str(m.Content); err != nil {
				return nil, err
			}
			buf.WriteByte('}')
			for _, u := range m.Images {
				buf.WriteString(`,{"type":"image_url","image_url":{"url":`)
				if jsonSafe(u) {
					buf.WriteByte('"')
					buf.WriteString(u)
					buf.WriteByte('"')
				} else if err := str(u); err != nil {
					return nil, err
				}
				buf.WriteString(`}}`)
			}
			buf.WriteByte(']')
		}
		buf.WriteByte('}')
	}
	buf.WriteString(`],"max_tokens":`)
	buf.WriteString(strconv.Itoa(maxTokens))
	if temperature > 0 {
		buf.WriteString(`,"temperature":`)
		buf.WriteString(strconv.FormatFloat(temperature, 'f', -1, 64))
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// jsonSafe reports whether s can be emitted inside JSON quotes verbatim
// (true for data: URLs with base64 payloads).
func jsonSafe(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == '"' || c == '\\' || c >= 0x7f || c == '<' || c == '>' || c == '&' {
			return false
		}
	}
	return true
}
