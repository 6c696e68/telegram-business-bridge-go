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

const maxImagesPerRequest = 3

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
	body := strings.TrimSpace(m.body())
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
	rt := m.ReplyToMessage
	if rt == nil {
		return content
	}
	quoted := ""
	if m.Quote != nil && strings.TrimSpace(m.Quote.Text) != "" {
		quoted = strings.TrimSpace(m.Quote.Text)
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
	for _, id := range ids {
		dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		data, err := d.DownloadFile(dctx, id, imageMaxBytes)
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

// visionRejected reports a provider error that typically means "this model
// or payload cannot take images".
func visionRejected(err error) bool {
	var le *LLMError
	if !errors.As(err, &le) || le.Kind != "http" {
		return false
	}
	switch le.Status {
	case 400, 404, 413, 415, 422:
		return true
	}
	return false
}

const imageDroppedNote = "\n[Có ảnh đính kèm nhưng model AI hiện tại không đọc được ảnh.]"

// callLLMVision calls the model and, if it rejects the images, retries once
// without them so the conversation still gets an answer.
func (b *Bridge) callLLMVision(ctx context.Context, msgs []ChatMsg) (string, error) {
	out, err := b.callLLM(ctx, msgs)
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
	return b.callLLM(ctx, plain)
}
