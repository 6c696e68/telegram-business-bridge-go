package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// toolLLM scripts tool-call rounds on top of the harness fake.
type toolLLM struct {
	*fakeLLM
	tmu       sync.Mutex
	steps     []ChatResult
	errAtStep int
	err       error
	calls     int
	seenTools [][]Tool
	seenMsgs  []ChatMsg
}

func (f *toolLLM) ChatTools(ctx context.Context, msgs []ChatMsg, tools []Tool) (ChatResult, error) {
	f.tmu.Lock()
	defer f.tmu.Unlock()
	f.calls++
	f.seenMsgs = append([]ChatMsg(nil), msgs...)
	f.seenTools = append(f.seenTools, tools)
	if f.errAtStep > 0 && f.calls == f.errAtStep {
		return ChatResult{}, f.err
	}
	if f.calls <= len(f.steps) {
		return f.steps[f.calls-1], nil
	}
	return ChatResult{Content: "ok"}, nil
}

func quoteCall(symbol string) ToolCall {
	var c ToolCall
	c.ID = "call_1"
	c.Type = "function"
	c.Function.Name = quoteToolName
	c.Function.Arguments = `{"symbol":"` + symbol + `"}`
	return c
}

func quoteServer(t *testing.T, hits *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"symbol": r.URL.Query().Get("symbol"), "bid": 4044.47, "ask": 4044.63,
		})
	}))
}

func TestQuoteToolRoundTrip(t *testing.T) {
	hits := 0
	qsrv := quoteServer(t, &hits)
	defer qsrv.Close()

	h := newHarness(func(c *Config) {
		c.DebounceMS = 0
		c.QuoteURL = qsrv.URL + "/quote"
	})
	tl := &toolLLM{fakeLLM: h.llm, steps: []ChatResult{
		{ToolCalls: []ToolCall{quoteCall("XAUUSD")}},
		{Content: "Giá XAUUSD đang 4044.47 / 4044.63."},
	}}
	h.b.llm = tl

	h.b.handleUpdate(dm(680, 680, "giá vàng sao em"))
	waitSent(t, h, 1)

	if hits != 1 {
		t.Fatalf("quote endpoint hits = %d, want 1", hits)
	}
	if got := h.tg.sends()[0]["text"]; got != "Giá XAUUSD đang 4044.47 / 4044.63." {
		t.Fatalf("reply = %v", got)
	}
	if tl.calls != 2 {
		t.Fatalf("llm calls = %d, want 2 (lookup + final)", tl.calls)
	}
	if len(tl.seenTools[0]) != 1 || tl.seenTools[0][0].Function.Name != quoteToolName {
		t.Fatalf("tool schema not sent: %+v", tl.seenTools[0])
	}
	if !strings.Contains(tl.seenMsgs[0].Content, "get_quote") {
		t.Fatal("system prompt must explain the tool and forbid unverified prices")
	}
	// The second request must carry the lookup result so the model can quote it.
	var toolMsg string
	for _, m := range tl.seenMsgs {
		if m.Role == "tool" {
			toolMsg = m.Content
		}
	}
	if !strings.Contains(toolMsg, "4044.47") || !strings.Contains(toolMsg, `"ok":`) {
		t.Fatalf("tool result missing from the second request: %q", toolMsg)
	}
}

func TestQuoteToolOffByDefault(t *testing.T) {
	h := newHarness(func(c *Config) { c.DebounceMS = 0 })
	tl := &toolLLM{fakeLLM: h.llm}
	h.b.llm = tl

	h.b.handleUpdate(dm(681, 681, "giá vàng sao em"))
	waitSent(t, h, 1)

	if tl.calls != 0 {
		t.Fatalf("tool path must stay off without quote_url, got %d calls", tl.calls)
	}
	if h.llm.calls() != 1 {
		t.Fatalf("plain chat path calls = %d", h.llm.calls())
	}
}

func TestQuoteCacheAndErrors(t *testing.T) {
	hits := 0
	qsrv := quoteServer(t, &hits)
	defer qsrv.Close()
	h := newHarness(func(c *Config) { c.QuoteURL = qsrv.URL + "/quote"; c.QuoteCacheSec = 10 })

	first := h.b.quoteLookup(context.Background(), "XAUUSD")
	second := h.b.quoteLookup(context.Background(), "XAUUSD")
	if first != second || !strings.Contains(first, "4044.47") {
		t.Fatalf("cache miss: %q vs %q", first, second)
	}
	if hits != 1 {
		t.Fatalf("cached lookup still hit the endpoint: %d", hits)
	}
	if got := h.b.quoteLookup(context.Background(), "bad symbol!"); !strings.Contains(got, "invalid symbol") {
		t.Fatalf("invalid symbol = %q", got)
	}
	if hits != 1 {
		t.Fatalf("invalid symbol must not reach the endpoint: %d", hits)
	}
}

func TestQuoteFailureKeepsTheAnswer(t *testing.T) {
	hits := 0
	qsrv := quoteServer(t, &hits)
	defer qsrv.Close()
	h := newHarness(func(c *Config) {
		c.DebounceMS = 0
		c.QuoteURL = qsrv.URL + "/quote"
	})
	tl := &toolLLM{
		fakeLLM:   h.llm,
		steps:     []ChatResult{{Content: "Để em kiểm tra giá.", ToolCalls: []ToolCall{quoteCall("XAUUSD")}}},
		errAtStep: 2,
		err:       errors.New("provider down"),
	}
	h.b.llm = tl

	h.b.handleUpdate(dm(682, 682, "giá vàng sao em"))
	waitSent(t, h, 1)
	if got := h.tg.sends()[0]["text"]; got != "Để em kiểm tra giá." {
		t.Fatalf("a failed tool round must fall back to the first answer, got %v", got)
	}
}
