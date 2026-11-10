package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Price tool.
//
// The single tool the bridge exposes is a quote lookup. It exists because a
// model reading a chart screenshot will confidently restate numbers it cannot
// verify; with the tool configured the prompt forbids quoting a price it did
// not just fetch. Off unless quote_url is set, so a default deployment behaves
// exactly as before.

const (
	quoteToolName = "get_quote"
	quoteMaxBody  = 8 << 10
	quoteMaxChars = 800
	quoteCacheMax = 32
)

// quoteNote is appended to the system prompt while the tool is enabled.
const quoteNote = "\n\n[Tools] You can call get_quote(symbol) to read a live price. " +
	"Never state a specific price you did not receive from that tool in this turn. " +
	"A chart screenshot is a picture, not a price source: describe it, do not read numbers off it. " +
	"If the tool fails, or the symbol is unclear, say you cannot check the price right now and ask for the exact symbol."

var symbolPattern = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,24}$`)

type quoteEntry struct {
	text string
	at   time.Time
}

// quoteOn reports whether the price tool is configured.
func (b *Bridge) quoteOn() bool { return strings.TrimSpace(b.cfg.QuoteURL) != "" }

// quoteTools is the tool schema sent with the request.
func quoteTools() []Tool {
	return []Tool{{
		Type: "function",
		Function: ToolFunction{
			Name: quoteToolName,
			Description: "Read the current price of a symbol (gold, forex, crypto). " +
				"Call it before stating any price.",
			Parameters: json.RawMessage(`{"type":"object","properties":{` +
				`"symbol":{"type":"string","description":"symbol, e.g. XAUUSD"}},` +
				`"required":["symbol"]}`),
		},
	}}
}

// answerTurn runs one model turn, executing at most one round of tool calls.
// Images and tools are never mixed in one request: the vision path already has
// its own retry shape, and a chart turn must not end up quoting a number.
func (b *Bridge) answerTurn(ctx context.Context, msgs []ChatMsg, imageIDs []string) (string, error) {
	tc, withTools := b.llm.(ToolCompleter)
	if !b.quoteOn() || !withTools || len(imageIDs) > 0 || hasImages(msgs) {
		return b.callWithImages(ctx, msgs, imageIDs)
	}
	tools := quoteTools()
	res, err := tc.ChatTools(ctx, msgs, tools)
	if err != nil {
		return "", err
	}
	if len(res.ToolCalls) == 0 {
		return res.Content, nil
	}
	next := append([]ChatMsg(nil), msgs...)
	next = append(next, ChatMsg{Role: "assistant", Content: res.Content, ToolCalls: res.ToolCalls})
	for _, call := range res.ToolCalls {
		next = append(next, ChatMsg{
			Role:       "tool",
			ToolCallID: call.ID,
			Name:       call.Function.Name,
			Content:    b.runTool(ctx, call),
		})
	}
	final, err := tc.ChatTools(ctx, next, tools)
	if err != nil || strings.TrimSpace(final.Content) == "" {
		// The lookup failed after the model already had something to say:
		// a text answer beats silence.
		return res.Content, nil
	}
	return final.Content, nil
}

// runTool executes one tool call. Unknown tools answer with JSON the model can
// read, never with an error that would abort the turn.
func (b *Bridge) runTool(ctx context.Context, call ToolCall) string {
	switch call.Function.Name {
	case quoteToolName:
		var args struct {
			Symbol string `json:"symbol"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return `{"ok":false,"error":"bad arguments"}`
		}
		return b.quoteLookup(ctx, strings.TrimSpace(args.Symbol))
	}
	return `{"ok":false,"error":"unknown tool"}`
}

// quoteLookup fetches the owner's quote endpoint for one symbol. Results are
// cached briefly so a burst does not hammer the endpoint.
func (b *Bridge) quoteLookup(ctx context.Context, symbol string) string {
	if !symbolPattern.MatchString(symbol) {
		return `{"ok":false,"error":"invalid symbol"}`
	}
	if text := b.cachedQuote(symbol); text != "" {
		return text
	}
	target := b.cfg.QuoteURL
	if strings.Contains(target, "{symbol}") {
		target = strings.ReplaceAll(target, "{symbol}", url.QueryEscape(symbol))
	} else {
		sep := "?"
		if strings.Contains(target, "?") {
			sep = "&"
		}
		target += sep + "symbol=" + url.QueryEscape(symbol)
	}
	tctx, cancel := context.WithTimeout(ctx, time.Duration(b.cfg.QuoteTimeoutMS)*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(tctx, http.MethodGet, target, nil)
	if err != nil {
		return `{"ok":false,"error":"bad quote_url"}`
	}
	req.Header.Set("Accept", "application/json")
	resp, err := b.media.Do(req)
	if err != nil {
		logf("WARN", "quote lookup failed: %v", err)
		return `{"ok":false,"error":"quote unavailable"}`
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf(`{"ok":false,"error":"quote http %d"}`, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, quoteMaxBody))
	if err != nil {
		return `{"ok":false,"error":"quote read failed"}`
	}
	text := strings.TrimSpace(string(data))
	text = truncRunes(text, quoteMaxChars)
	// Wrap the answer so its shape matches the error answers ("ok":false).
	if json.Valid([]byte(text)) {
		text = `{"ok":true,"data":` + text + `}`
	} else if wrapped, err := json.Marshal(map[string]any{"ok": true, "text": text}); err == nil {
		text = string(wrapped)
	}
	b.storeQuote(symbol, text)
	return text
}

func (b *Bridge) cachedQuote(symbol string) string {
	if b.cfg.QuoteCacheSec <= 0 {
		return ""
	}
	b.quoteMu.Lock()
	defer b.quoteMu.Unlock()
	e, ok := b.quoteCache[symbol]
	if !ok || b.now().Sub(e.at) > time.Duration(b.cfg.QuoteCacheSec)*time.Second {
		delete(b.quoteCache, symbol)
		return ""
	}
	return e.text
}

func (b *Bridge) storeQuote(symbol, text string) {
	if b.cfg.QuoteCacheSec <= 0 || text == "" {
		return
	}
	now := b.now()
	b.quoteMu.Lock()
	defer b.quoteMu.Unlock()
	if b.quoteCache == nil {
		b.quoteCache = map[string]quoteEntry{}
	}
	if len(b.quoteCache) >= quoteCacheMax {
		for k := range b.quoteCache { // cheapest possible eviction: drop one
			delete(b.quoteCache, k)
			break
		}
	}
	b.quoteCache[symbol] = quoteEntry{text: text, at: now}
}
