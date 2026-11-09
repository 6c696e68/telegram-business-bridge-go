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
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type Message struct {
	BusinessConnectionID string      `json:"business_connection_id"`
	Chat                 Chat        `json:"chat"`
	From                 *User       `json:"from"`
	Date                 int64       `json:"date"`
	SenderBusinessBot    *User       `json:"sender_business_bot"`
	Text                 string      `json:"text"`
	Caption              string      `json:"caption"`
	Photo                []PhotoSize `json:"photo"`
	Document             *Document   `json:"document"`
	Sticker              *Sticker    `json:"sticker"`
	Quote                *TextQuote  `json:"quote"`
	ReplyToMessage       *Message    `json:"reply_to_message"`
}

type PhotoSize struct {
	FileID   string `json:"file_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FileSize int64  `json:"file_size"`
}

type Document struct {
	FileID   string `json:"file_id"`
	MimeType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
}

type Sticker struct {
	Emoji string `json:"emoji"`
}

// TextQuote is the part of the replied-to message the user selected.
type TextQuote struct {
	Text string `json:"text"`
}

const (
	photoMaxSide     = 1280    // vision models gain little from bigger images
	photoPreferBytes = 2 << 20 // prefer sizes below this
	imageMaxBytes    = 4 << 20 // hard cap for any downloaded image
)

// imageFileID returns the file to send to a vision model: the largest photo
// size within photoMaxSide/photoPreferBytes (else the smallest size), or an
// image sent as a document. "" when the message carries no usable image.
func (m *Message) imageFileID() string {
	if m == nil {
		return ""
	}
	if len(m.Photo) > 0 {
		best := -1
		for i, p := range m.Photo {
			if p.Width > photoMaxSide || p.Height > photoMaxSide || p.FileSize > photoPreferBytes {
				continue
			}
			if best < 0 || p.Width*p.Height > m.Photo[best].Width*m.Photo[best].Height {
				best = i
			}
		}
		if best < 0 {
			best = 0 // Telegram lists sizes smallest first
		}
		return m.Photo[best].FileID
	}
	if d := m.Document; d != nil && strings.HasPrefix(strings.ToLower(d.MimeType), "image/") &&
		d.FileSize <= imageMaxBytes {
		return d.FileID
	}
	return ""
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
	base     string
	fileBase string
	hc       *http.Client
}

func NewTelegram(apiBase, token string, hc *http.Client) *Telegram {
	root := strings.TrimRight(apiBase, "/")
	return &Telegram{base: root + "/bot" + token + "/", fileBase: root + "/file/bot" + token + "/", hc: hc}
}

// fileDownloader is implemented by Telegram; the bridge uses it for images.
type fileDownloader interface {
	DownloadFile(ctx context.Context, fileID string, max int64) ([]byte, error)
}

// DownloadFile fetches a file sent to the bot (getFile + file URL), refusing
// anything larger than max. Errors never contain the URL (it holds the token).
func (t *Telegram) DownloadFile(ctx context.Context, fileID string, max int64) ([]byte, error) {
	var f struct {
		FilePath string `json:"file_path"`
		FileSize int64  `json:"file_size"`
	}
	if err := t.Call(ctx, "getFile", map[string]string{"file_id": fileID}, &f); err != nil {
		return nil, err
	}
	if f.FilePath == "" {
		return nil, &NetError{Method: "getFile", Kind: "no_path"}
	}
	if f.FileSize > max {
		return nil, &NetError{Method: "getFile", Kind: "too_large"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.fileBase+f.FilePath, nil)
	if err != nil {
		return nil, &NetError{Method: "file", Kind: "request"}
	}
	resp, err := t.hc.Do(req)
	if err != nil {
		return nil, &NetError{Method: "file", Kind: netKind(err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &TGError{Method: "file", Code: resp.StatusCode, Desc: "download failed"}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, &NetError{Method: "file", Kind: "read"}
	}
	if int64(len(data)) > max {
		return nil, &NetError{Method: "file", Kind: "too_large"}
	}
	return data, nil
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
