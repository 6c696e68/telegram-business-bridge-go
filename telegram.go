package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

type User struct {
	ID    int64 `json:"id"`
	IsBot bool  `json:"is_bot"`
}

type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type Message struct {
	BusinessConnectionID string `json:"business_connection_id"`
	Chat                 Chat   `json:"chat"`
	From                 *User  `json:"from"`
	Date                 int64  `json:"date"`
	SenderBusinessBot    *User  `json:"sender_business_bot"`
	Text                 string `json:"text"`
	Caption              string `json:"caption"`
	ReplyToMessage       *struct {
		From *User `json:"from"`
	} `json:"reply_to_message"`
}

// body mirrors Python's `m.get("text") or m.get("caption")`.
func (m *Message) body() string {
	if m.Text != "" {
		return m.Text
	}
	return m.Caption
}

type BizRights struct {
	CanReply *bool `json:"can_reply"`
}

type BusinessConnection struct {
	ID        string     `json:"id"`
	User      *User      `json:"user"`
	IsEnabled *bool      `json:"is_enabled"`
	CanReply  *bool      `json:"can_reply"`
	Rights    *BizRights `json:"rights"`
}

type Update struct {
	UpdateID           int64               `json:"update_id"`
	Message            *Message            `json:"message"`
	BusinessMessage    *Message            `json:"business_message"`
	EditedBusinessMsg  *Message            `json:"edited_business_message"`
	BusinessConnection *BusinessConnection `json:"business_connection"`
}

// TGError is an error answer from the Telegram Bot API (ok=false).
type TGError struct {
	Method     string
	Code       int
	Desc       string
	RetryAfter int
}

func (e *TGError) Error() string {
	return fmt.Sprintf("%s failed: %d %s", e.Method, e.Code, e.Desc)
}

// NetError is a transport failure. It deliberately carries no URL, because the
// URL contains the bot token.
type NetError struct {
	Method string
	Kind   string // timeout | canceled | conn | read
}

func (e *NetError) Error() string {
	return fmt.Sprintf("%s network error: %s", e.Method, e.Kind)
}

func netKind(err error) string {
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return "timeout"
	}
	return "conn"
}

type Telegram struct {
	base string
	hc   *http.Client
}

func NewTelegram(apiBase, token string, hc *http.Client) *Telegram {
	return &Telegram{base: strings.TrimRight(apiBase, "/") + "/bot" + token + "/", hc: hc}
}

type tgEnvelope struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Parameters  *struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// Call posts params as JSON and decodes "result" into out (if non-nil).
func (t *Telegram) Call(ctx context.Context, method string, params any, out any) error {
	body := []byte("{}")
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return &NetError{Method: method, Kind: "encode"}
		}
		body = b
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.base+method, bytes.NewReader(body))
	if err != nil {
		return &NetError{Method: method, Kind: "request"}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.hc.Do(req)
	if err != nil {
		return &NetError{Method: method, Kind: netKind(err)}
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(io.LimitReader(resp.Body, 4<<20))
	var env tgEnvelope
	if err := dec.Decode(&env); err != nil {
		return &TGError{Method: method, Code: resp.StatusCode, Desc: "bad response"}
	}
	if !env.OK {
		code := env.ErrorCode
		if code == 0 {
			code = resp.StatusCode
		}
		e := &TGError{Method: method, Code: code, Desc: env.Description}
		if env.Parameters != nil {
			e.RetryAfter = env.Parameters.RetryAfter
		}
		return e
	}
	if out != nil && len(env.Result) > 0 {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}
