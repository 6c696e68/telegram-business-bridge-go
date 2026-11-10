package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Images and reply context.
//
// Photos (and images sent as files) are downloaded on demand, passed to the
// model as data: URLs together with the message, and never stored: history,
// transcripts and the DB only keep the text marker "[Ảnh] caption". When a
// message replies to another one, the quoted content (and its image) is
// included so the model knows what is being answered.

const (
	maxImagesPerRequest = 3
	// imageBudgetBytes caps the raw image bytes of one request: base64 and the
	// request body roughly triple it, and only one image request is held in
	// memory at a time (visionSem), so the peak stays far below the router's
	// 64 MiB Go memory limit. Real photos are 100-400 KB.
	imageBudgetBytes = 3 << 20
)

// markerNeutral rewrites text that imitates the bridge's own markers, so a
// customer cannot forge "[Đang trả lời tin nhắn của chủ: …]".
func markerNeutral(s string) string {
	for _, pair := range [][2]string{
		{"[Đang trả lời", "(Đang trả lời"},
		{"[Ảnh]", "(Ảnh)"},
		{"[Voice]", "(Voice)"}, // a customer must not forge a transcript marker
		{"[Gửi một ảnh]", "(Gửi một ảnh)"},
		{"[Sticker ", "(Sticker "},
		{"[nội dung không phải văn bản]", "(nội dung không phải văn bản)"},
	} {
		s = strings.ReplaceAll(s, pair[0], pair[1])
	}
	return s
}

// senderLabel names the author of a replied-to message for the model.
func (b *Bridge) senderLabel(m *Message) string {
	switch {
	case m == nil || m.From == nil:
		return "người gửi"
	case m.SenderBusinessBot != nil:
		return "trợ lý AI"
	case m.From.ID == b.cfg.OwnerID:
		return "chủ"
	case b.botID != 0 && m.From.ID == b.botID:
		return "trợ lý AI"
	case m.From.IsBot:
		return "bot"
	}
	if n := displayName(m.From); n != "" {
		return "khách (" + n + ")"
	}
	return "khách"
}

// messageContent renders the body of a message for the model and the history.
func messageContent(m *Message) string {
	body := markerNeutral(strings.TrimSpace(m.body()))
	switch {
	case m.imageFileID() != "" && body != "":
		return "[Ảnh] " + body
	case m.imageFileID() != "":
		return "[Gửi một ảnh]"
	case body != "":
		return body
	case m.Sticker != nil:
		return strings.TrimSpace("[Sticker " + m.Sticker.Emoji + "]")
	}
	return ""
}

// promptText renders a message with its reply context, e.g.
//
//	[Đang trả lời tin nhắn của chủ: "giá 250k"]
//	[Ảnh] còn màu đen không?
func (b *Bridge) promptText(m *Message) string {
	content := messageContent(m)
	// With transcription on, a voice note becomes a marker the worker replaces
	// with the transcript before the turn is built.
	if content == "" && b.transcribeOn() && m.voiceFileID() != "" {
		content = voiceMarker
	}
	rt := m.ReplyToMessage
	if rt == nil {
		return content
	}
	quoted := ""
	if m.Quote != nil && strings.TrimSpace(m.Quote.Text) != "" {
		quoted = markerNeutral(strings.TrimSpace(m.Quote.Text))
	} else {
		quoted = messageContent(rt)
	}
	if quoted == "" {
		quoted = "[nội dung không phải văn bản]"
	}
	ctx := fmt.Sprintf("[Đang trả lời tin nhắn của %s: \"%s\"]", b.senderLabel(rt), truncRunes(quoted, 300))
	if content == "" {
		return ctx
	}
	return ctx + "\n" + content
}

// messageImages lists the image files of a message and of the message it
// replies to (the latter first, so the model sees what is being answered).
func (b *Bridge) messageImages(m *Message) []string {
	if !b.cfg.Vision {
		return nil
	}
	var ids []string
	if id := m.ReplyToMessage.imageFileID(); id != "" {
		ids = append(ids, id)
	}
	if id := m.imageFileID(); id != "" {
		ids = append(ids, id)
	}
	return ids
}

// addImagesLocked queues image files on a chat, keeping the newest ones.
// Caller holds b.mu.
func addImagesLocked(cs *chatState, ids []string) {
	if len(ids) == 0 {
		return
	}
	cs.images = append(cs.images, ids...)
	if over := len(cs.images) - maxImagesPerRequest; over > 0 {
		cs.images = append([]string(nil), cs.images[over:]...)
	}
}

// downloadImages fetches images as data: URLs. Failures are skipped and logged
// without content; the reply still goes out without that image.
func (b *Bridge) downloadImages(ctx context.Context, ids []string) []string {
	d, ok := b.tg.(fileDownloader)
	if !ok || len(ids) == 0 {
		return nil
	}
	if len(ids) > maxImagesPerRequest {
		ids = ids[len(ids)-maxImagesPerRequest:]
	}
	var urls []string
	var total int64
	for _, id := range ids {
		left := int64(imageBudgetBytes) - total
		if left <= 0 {
			logf("WARN", "image download skipped: request image budget used")
			break
		}
		limit := int64(imageMaxBytes)
		if left < limit {
			limit = left
		}
		dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		data, err := d.DownloadFile(dctx, id, limit)
		cancel()
		if err != nil {
			logf("WARN", "image download skipped: %v", err)
			continue
		}
		mime := http.DetectContentType(data)
		if !strings.HasPrefix(mime, "image/") {
			logf("WARN", "image download skipped: not an image (%s)", mime)
			continue
		}
		total += int64(len(data)) // only accepted images use the budget
		urls = append(urls, "data:"+mime+";base64,"+base64.StdEncoding.EncodeToString(data))
	}
	return urls
}

// attachImages puts images on the last user message of a request copy.
func attachImages(msgs []ChatMsg, urls []string) {
	if len(urls) == 0 {
		return
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			msgs[i].Images = urls
			return
		}
	}
}

func hasImages(msgs []ChatMsg) bool {
	for _, m := range msgs {
		if len(m.Images) > 0 {
			return true
		}
	}
	return false
}

// nonImageErrorHints name failures a text-only retry cannot fix (overlong
// context, quota, auth, missing model), so they must not cost a second call.
var nonImageErrorHints = []string{
	"context length", "maximum context", "context window", "max_tokens", "max tokens",
	"token limit", "too many tokens", "insufficient_quota", "quota", "billing",
	"authentication", "api key", "invalid_api_key", "model_not_found", "no such model",
}

// visionRejected reports whether a failed image request is worth retrying
// without the images. 413/415 always are; 400/404/422 are, unless the error
// body names a text-side cause a retry cannot fix.
func visionRejected(err error) bool {
	var le *LLMError
	if !errors.As(err, &le) || le.Kind != "http" {
		return false
	}
	switch le.Status {
	case 413, 415:
		return true
	case 400, 404, 422:
		if strings.TrimSpace(le.detail) == "" {
			return true
		}
		for _, h := range nonImageErrorHints {
			if strings.Contains(le.detail, h) {
				return false
			}
		}
		return true
	}
	return false
}

// callWithImages downloads the chat's images and calls the model. Only one
// image-bearing request is prepared and held in memory at a time; text-only
// requests never wait for it.
func (b *Bridge) callWithImages(ctx context.Context, msgs []ChatMsg, imageIDs []string) (string, error) {
	if len(imageIDs) == 0 || !b.cfg.Vision {
		return b.callLLM(ctx, msgs)
	}
	select {
	case b.visionSem <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-b.visionSem }()
	attachImages(msgs, b.downloadImages(ctx, imageIDs))
	return b.callLLMVision(ctx, msgs)
}

const imageDroppedNote = "\n[Có ảnh đính kèm nhưng model AI hiện tại không đọc được ảnh.]"

// callLLMVision calls the model and, if it rejects the images, retries once
// without them so the conversation still gets an answer.
// visionClient is the client used for turns that carry images: the dedicated
// vision model when configured, else the main one.
func (b *Bridge) visionClient() Completer {
	if b.visionLLM != nil {
		return b.visionLLM
	}
	return b.llm
}

func (b *Bridge) callLLMVision(ctx context.Context, msgs []ChatMsg) (string, error) {
	out, err := b.callLLMWith(ctx, b.visionClient(), msgs)
	if err == nil || !hasImages(msgs) || !visionRejected(err) || ctx.Err() != nil {
		return out, err
	}
	logf("WARN", "model rejected images, retrying without them")
	plain := make([]ChatMsg, len(msgs))
	for i, m := range msgs {
		if len(m.Images) > 0 {
			m.Content += imageDroppedNote
			m.Images = nil
		}
		plain[i] = m
	}
	return b.callLLMWith(ctx, b.visionClient(), plain)
}
