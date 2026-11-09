package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func photo(ids ...string) []PhotoSize {
	var out []PhotoSize
	for i, id := range ids {
		side := 320 * (i + 1)
		out = append(out, PhotoSize{FileID: id, Width: side, Height: side, FileSize: int64(10000 * (i + 1))})
	}
	return out
}

func bizPhoto(chat int64, caption string, ids ...string) Update {
	u := biz(testConn, chat, chat, "")
	u.BusinessMessage.Caption = caption
	u.BusinessMessage.Photo = photo(ids...)
	return u
}

// lastLLMUserMsg returns the last user message of the last LLM request.
func lastLLMUserMsg(h *harness) ChatMsg {
	msgs := h.llm.lastSeen()
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i]
		}
	}
	return ChatMsg{}
}

// A customer photo with a caption reaches the model as an image plus text.
func TestCustomerPhotoIsSentToTheModel(t *testing.T) {
	h := newHarness(nil)
	h.b.handleUpdate(bizPhoto(950, "áo này còn size M không", "small", "big"))
	waitSent(t, h, 1)
	um := lastLLMUserMsg(h)
	if !strings.Contains(um.Content, "[Ảnh] áo này còn size M không") {
		t.Fatalf("content = %q", um.Content)
	}
	if len(um.Images) != 1 || !strings.HasPrefix(um.Images[0], "data:image/png;base64,") {
		t.Fatalf("images = %v", um.Images)
	}
	if got := h.tg.downloaded(); len(got) != 1 || got[0] != "big" {
		t.Fatalf("must download the largest size within limits, got %v", got)
	}
	// history never keeps the image itself
	h.b.mu.Lock()
	defer h.b.mu.Unlock()
	for _, m := range h.b.chats["b:"+testConn+":950"].msgs {
		if len(m.Images) > 0 {
			t.Fatal("images must not be stored in chat history")
		}
	}
}

func TestPhotoWithoutCaption(t *testing.T) {
	h := newHarness(nil)
	h.b.handleUpdate(bizPhoto(951, "", "p1"))
	waitSent(t, h, 1)
	if um := lastLLMUserMsg(h); um.Content != "[Gửi một ảnh]" || len(um.Images) != 1 {
		t.Fatalf("got %q images=%d", um.Content, len(um.Images))
	}
}

func TestImageDocumentIsRead(t *testing.T) {
	h := newHarness(nil)
	u := biz(testConn, 952, 952, "")
	u.BusinessMessage.Caption = "bảng giá"
	u.BusinessMessage.Document = &Document{FileID: "doc1", MimeType: "image/jpeg", FileSize: 50000}
	h.b.handleUpdate(u)
	waitSent(t, h, 1)
	if um := lastLLMUserMsg(h); len(um.Images) != 1 || !strings.Contains(um.Content, "[Ảnh] bảng giá") {
		t.Fatalf("image document not read: %q %d", um.Content, len(um.Images))
	}
	// a non-image document is not downloaded
	u2 := biz(testConn, 953, 953, "")
	u2.BusinessMessage.Document = &Document{FileID: "pdf1", MimeType: "application/pdf", FileSize: 50000}
	h.b.handleUpdate(u2)
	waitSent(t, h, 2)
	for _, id := range h.tg.downloaded() {
		if id == "pdf1" {
			t.Fatal("non-image documents must not be downloaded")
		}
	}
}

// A model without vision rejects images: the reply must still go out.
func TestVisionRejectedFallsBackToText(t *testing.T) {
	h := newHarness(nil)
	h.llm.mu.Lock()
	h.llm.errs = []error{&LLMError{Status: 400, Kind: "http"}}
	h.llm.mu.Unlock()
	h.b.handleUpdate(bizPhoto(954, "xem giúp", "p1"))
	waitSent(t, h, 1)
	if h.llm.calls() != 2 {
		t.Fatalf("expected a retry without images, calls = %d", h.llm.calls())
	}
	um := lastLLMUserMsg(h)
	if len(um.Images) != 0 || !strings.Contains(um.Content, "không đọc được ảnh") {
		t.Fatalf("retry must drop images and say so: %q %d", um.Content, len(um.Images))
	}
}

func TestImageDownloadFailureStillReplies(t *testing.T) {
	h := newHarness(nil)
	h.tg.downloadErr = &NetError{Method: "file", Kind: "too_large"}
	h.b.handleUpdate(bizPhoto(955, "ảnh nè", "p1"))
	waitSent(t, h, 1)
	if um := lastLLMUserMsg(h); len(um.Images) != 0 || !strings.Contains(um.Content, "[Ảnh] ảnh nè") {
		t.Fatalf("got %q %d", um.Content, len(um.Images))
	}
}

func TestVisionDisabled(t *testing.T) {
	h := newHarness(func(c *Config) { c.Vision = false })
	h.b.handleUpdate(bizPhoto(956, "ảnh", "p1"))
	waitSent(t, h, 1)
	if len(h.tg.downloaded()) != 0 || len(lastLLMUserMsg(h).Images) != 0 {
		t.Fatal("vision=false must not download images")
	}
}

// Replies carry the quoted message so the model knows what is answered.
func TestReplyContextIsIncluded(t *testing.T) {
	h := newHarness(func(c *Config) { c.OwnerAwayMinutes = 0 })
	u := biz(testConn, 960, 960, "còn màu đen không?")
	u.BusinessMessage.ReplyToMessage = &Message{From: &User{ID: testOwner}, Text: "Áo thun 250k, có 3 màu"}
	h.b.handleUpdate(u)
	waitSent(t, h, 1)
	want := "[Đang trả lời tin nhắn của chủ: \"Áo thun 250k, có 3 màu\"]\ncòn màu đen không?"
	if um := lastLLMUserMsg(h); !strings.Contains(um.Content, want) {
		t.Fatalf("content = %q", um.Content)
	}

	// a selected quote wins over the full replied text
	u2 := biz(testConn, 961, 961, "cái này là sao")
	u2.BusinessMessage.ReplyToMessage = &Message{From: &User{ID: testOwner}, Text: "dòng 1. dòng 2 quan trọng"}
	u2.BusinessMessage.Quote = &TextQuote{Text: "dòng 2 quan trọng"}
	h.b.handleUpdate(u2)
	waitSent(t, h, 2)
	if um := lastLLMUserMsg(h); !strings.Contains(um.Content, "\"dòng 2 quan trọng\"") {
		t.Fatalf("quote ignored: %q", um.Content)
	}
}

func TestReplyToBotAndToPhoto(t *testing.T) {
	h := newHarness(nil)
	u := biz(testConn, 962, 962, "giá cái này?")
	u.BusinessMessage.ReplyToMessage = &Message{
		From:              &User{ID: testOwner},
		SenderBusinessBot: &User{ID: 999, IsBot: true},
		Caption:           "mẫu mới về",
		Photo:             photo("replied-photo"),
	}
	h.b.handleUpdate(u)
	waitSent(t, h, 1)
	um := lastLLMUserMsg(h)
	if !strings.Contains(um.Content, "[Đang trả lời tin nhắn của trợ lý AI: \"[Ảnh] mẫu mới về\"]") {
		t.Fatalf("content = %q", um.Content)
	}
	if len(um.Images) != 1 {
		t.Fatalf("the replied photo must be attached, got %d", len(um.Images))
	}
	if got := h.tg.downloaded(); len(got) != 1 || got[0] != "replied-photo" {
		t.Fatalf("downloads = %v", got)
	}
}

// Screenshot 2026-11-09 23:42: "phân tích ảnh này" with a photo in the
// owner's private chat answered "send me the picture".
func TestOwnerChatAnalysesPhoto(t *testing.T) {
	h, _, _ := opsHarness(t)
	var mu sync.Mutex
	var seen ChatMsg
	h.llm.fn = func(msgs []ChatMsg) (string, bool) {
		if strings.HasPrefix(msgs[0].Content, editPromptPrefix) {
			mu.Lock()
			seen = msgs[1]
			mu.Unlock()
			return `{"reply":"Ảnh là danh sách tài khoản ChatGPT giá 0.20-0.25$.","owner_rules":null,"ai_rules":null}`, true
		}
		return "ok", false
	}
	u := dm(testOwner, testOwner, "")
	u.Message.Caption = "phân tích ảnh này"
	u.Message.Photo = photo("owner-shot")
	if got := h.b.handleUpdate(u); got != "owner_edit" {
		t.Fatalf("got %q", got)
	}
	waitSent(t, h, 1)
	mu.Lock()
	defer mu.Unlock()
	if len(seen.Images) != 1 || !strings.Contains(seen.Content, "[Ảnh] phân tích ảnh này") {
		t.Fatalf("owner image not sent to the model: %q images=%d", seen.Content, len(seen.Images))
	}
	if !strings.Contains(lastSentText(h), "ChatGPT") {
		t.Fatalf("reply = %q", lastSentText(h))
	}
}

func TestOwnerChatPhotoWithoutCaption(t *testing.T) {
	h, _, _ := opsHarness(t)
	h.llm.fn = func(msgs []ChatMsg) (string, bool) {
		if strings.HasPrefix(msgs[0].Content, editPromptPrefix) && len(msgs[1].Images) == 1 {
			return `{"reply":"Đây là ảnh chụp màn hình.","owner_rules":null,"ai_rules":null}`, true
		}
		return `{"reply":"không thấy ảnh","owner_rules":null,"ai_rules":null}`, true
	}
	u := dm(testOwner, testOwner, "")
	u.Message.Photo = photo("p")
	h.b.handleUpdate(u)
	waitSent(t, h, 1)
	if !strings.Contains(lastSentText(h), "ảnh chụp màn hình") {
		t.Fatalf("reply = %q", lastSentText(h))
	}
}

func TestImagesAreNeverPersisted(t *testing.T) {
	dir := t.TempDir()
	h := newHarness(func(c *Config) {
		c.DBFile = filepath.Join(dir, "bridge.db")
		c.OwnerAwayMinutes = 0
	})
	defer func() { _ = h.b.Close() }()
	h.b.handleUpdate(bizPhoto(970, "ảnh", "p1"))
	waitSent(t, h, 1)
	settle()
	if err := h.b.flushNow(); err != nil {
		t.Fatal(err)
	}
	_ = h.b.db.View(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bucketChats, bucketTranscripts} {
			_ = tx.Bucket(name).ForEach(func(k, v []byte) error {
				if strings.Contains(string(v), "base64") || strings.Contains(string(v), "image_url") {
					t.Fatalf("image data stored in %s", name)
				}
				return nil
			})
		}
		return nil
	})
}

func TestChatMsgJSON(t *testing.T) {
	plain, _ := json.Marshal(ChatMsg{Role: "user", Content: "hi"})
	if string(plain) != `{"role":"user","content":"hi"}` {
		t.Fatalf("plain = %s", plain)
	}
	multi, _ := json.Marshal(ChatMsg{Role: "user", Content: "xem", Images: []string{"data:image/png;base64,AA=="}})
	want := `{"role":"user","content":[{"type":"text","text":"xem"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]}`
	if string(multi) != want {
		t.Fatalf("multimodal = %s", multi)
	}
	var back ChatMsg
	if err := json.Unmarshal(plain, &back); err != nil || back.Content != "hi" {
		t.Fatalf("stored records must still decode: %v %+v", err, back)
	}
}

func TestImageFileIDSelection(t *testing.T) {
	m := &Message{Photo: []PhotoSize{
		{FileID: "s", Width: 90, Height: 90, FileSize: 1000},
		{FileID: "m", Width: 800, Height: 800, FileSize: 90000},
		{FileID: "xl", Width: 2560, Height: 2560, FileSize: 3 << 20},
	}}
	if got := m.imageFileID(); got != "m" {
		t.Fatalf("got %q", got)
	}
	huge := &Message{Photo: []PhotoSize{{FileID: "only", Width: 4000, Height: 4000, FileSize: 9 << 20}}}
	if got := huge.imageFileID(); got != "only" {
		t.Fatalf("fallback must use the smallest size, got %q", got)
	}
	var nilMsg *Message
	if nilMsg.imageFileID() != "" {
		t.Fatal("nil message has no image")
	}
	big := &Message{Document: &Document{FileID: "d", MimeType: "image/png", FileSize: 20 << 20}}
	if big.imageFileID() != "" {
		t.Fatal("oversize image documents must be skipped")
	}
}

// Real HTTP: getFile + file download, and the token never leaks into errors.
func TestTelegramDownloadFile(t *testing.T) {
	const token = "123:SECRETTOKEN"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bot" + token + "/getFile":
			_, _ = io.WriteString(w, `{"ok":true,"result":{"file_path":"photos/a.jpg","file_size":8}}`)
		case "/file/bot" + token + "/photos/a.jpg":
			_, _ = w.Write([]byte("\xff\xd8\xff\xe0JFIF"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	tg := NewTelegram(srv.URL, token, srv.Client())
	data, err := tg.DownloadFile(context.Background(), "x", 1024)
	if err != nil || http.DetectContentType(data) != "image/jpeg" {
		t.Fatalf("data=%q err=%v", data, err)
	}
	if _, err := tg.DownloadFile(context.Background(), "x", 4); err == nil {
		t.Fatal("max size must be enforced")
	} else if strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("token leaked: %v", err)
	}
}

// /tomtat review: a failed flush keeps transcript lines (in order) unless the
// chat was forgotten meanwhile.
func TestTranscriptRequeueAndForget(t *testing.T) {
	dir := t.TempDir()
	cfg := dbTestConfig(dir, func(c *Config) { c.DBFile = filepath.Join(dir, "bridge.db") })
	b := NewBridge(cfg, &fakeTG{}, &fakeLLM{})
	b.mu.Lock()
	b.recordTranscriptLocked("d:1", "c", "A", "một")
	b.recordTranscriptLocked("d:2", "c", "B", "hai")
	b.mu.Unlock()

	// make the next write fail: swap in a read-only handle
	b.flushMu.Lock()
	_ = b.db.Close()
	ro, err := bolt.Open(cfg.DBFile, 0o600, &bolt.Options{ReadOnly: true})
	if err != nil {
		b.flushMu.Unlock()
		t.Fatal(err)
	}
	b.db = ro
	b.flushMu.Unlock()
	if err := b.flushNow(); err == nil {
		t.Fatal("expected a failed flush")
	}

	b.mu.Lock()
	b.deletedChats["d:1"] = struct{}{} // /forget d:1 after the failure
	b.forgetTranscriptLocked("d:1")
	b.recordTranscriptLocked("d:2", "c", "", "ba")
	_, d1 := b.trPending["d:1"]
	d2 := b.trPending["d:2"].entries
	b.mu.Unlock()
	if d1 {
		t.Fatal("a forgotten chat must not be requeued")
	}
	if len(d2) != 2 || d2[0].X != "hai" || d2[1].X != "ba" {
		t.Fatalf("requeued lines must keep order: %+v", d2)
	}

	// back to a writable DB: everything lands, d:1 stays gone
	b.flushMu.Lock()
	_ = b.db.Close()
	rw, err := bolt.Open(cfg.DBFile, 0o600, nil)
	if err != nil {
		b.flushMu.Unlock()
		t.Fatal(err)
	}
	b.db = rw
	b.flushMu.Unlock()
	defer func() { _ = b.Close() }()
	if _, ok := b.loadTranscript("d:1"); ok {
		t.Fatal("forgotten transcript resurrected")
	}
	rec, ok := b.loadTranscript("d:2")
	if !ok || len(rec.Msgs) != 2 || rec.Msgs[1].X != "ba" || rec.Title != "B" {
		t.Fatalf("d:2 transcript = %+v", rec)
	}
}

func TestTranscriptPendingIsBounded(t *testing.T) {
	dir := t.TempDir()
	cfg := dbTestConfig(dir, func(c *Config) { c.DBFile = filepath.Join(dir, "bridge.db") })
	b := NewBridge(cfg, &fakeTG{}, &fakeLLM{})
	defer func() { _ = b.Close() }()
	b.mu.Lock()
	for i := 0; i < 500; i++ {
		b.recordTranscriptLocked("d:1", "c", "", "x")
	}
	n := len(b.trPending["d:1"].entries)
	b.mu.Unlock()
	if n != transcriptKeep {
		t.Fatalf("queued lines = %d, want %d", n, transcriptKeep)
	}
}
