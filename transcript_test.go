package main

import (
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const summaryPrefix = "Bạn tóm tắt một cuộc trò chuyện"

func summaryLLM(h *harness) (*sync.Mutex, *[]string) {
	var mu sync.Mutex
	var inputs []string
	h.llm.fn = func(msgs []ChatMsg) (string, bool) {
		if strings.HasPrefix(msgs[0].Content, summaryPrefix) {
			mu.Lock()
			inputs = append(inputs, msgs[1].Content)
			mu.Unlock()
			return "1. Khách cần hỏi giá\n2. ...\n3. (không có)\n4. Báo giá cho khách", true
		}
		return "Dạ em trả lời", false
	}
	return &mu, &inputs
}

func ownerSends(h *harness) []map[string]any {
	var out []map[string]any
	for _, s := range h.tg.sends() {
		if id, _ := s["chat_id"].(float64); int64(id) == testOwner {
			if _, biz := s["business_connection_id"]; !biz {
				out = append(out, s)
			}
		}
	}
	return out
}

func namedBiz(chat int64, first, text string) Update {
	u := biz(testConn, chat, chat, text)
	u.BusinessMessage.From.FirstName = first
	return u
}

// /tomtat typed inside a customer chat: the summary goes to the owner's
// private chat, never into the customer chat.
func TestSummaryFromBusinessChatGoesToOwner(t *testing.T) {
	h := newHarness(nil)
	mu, inputs := summaryLLM(h)
	h.b.handleUpdate(namedBiz(900, "Lan", "shop ơi áo này giá bao nhiêu"))
	waitFor(t, "bot reply", func() bool { return len(businessSends(h)) == 1 })
	h.b.handleUpdate(biz(testConn, 900, testOwner, "dạ 250k chị nhé"))
	settle()

	before := len(businessSends(h))
	if got := h.b.handleUpdate(biz(testConn, 900, testOwner, "/tomtat")); got != "owner_summary" {
		t.Fatalf("got %q", got)
	}
	waitFor(t, "summary to owner", func() bool { return len(ownerSends(h)) == 1 })
	if n := len(businessSends(h)); n != before {
		t.Fatalf("nothing may be sent into the customer chat, got %d new", n-before)
	}
	text, _ := ownerSends(h)[0]["text"].(string)
	if !strings.Contains(text, "Tóm tắt chat với Lan") || !strings.Contains(text, "3 tin") ||
		!strings.Contains(text, "Báo giá cho khách") {
		t.Fatalf("summary message = %q", text)
	}
	mu.Lock()
	in := (*inputs)[0]
	mu.Unlock()
	for _, want := range []string{"Khách: shop ơi áo này giá bao nhiêu", "Trợ lý AI: Dạ em trả lời", "Chủ: dạ 250k chị nhé"} {
		if !strings.Contains(in, want) {
			t.Fatalf("transcript missing %q:\n%s", want, in)
		}
	}
	if strings.Contains(in, "/tomtat") {
		t.Fatal("owner commands must not be part of the transcript")
	}
}

func TestSummaryListAndPick(t *testing.T) {
	h := newHarness(func(c *Config) { c.OwnerAwayMinutes = 0 })
	_, inputs := summaryLLM(h)
	h.b.handleUpdate(namedBiz(910, "Minh", "tin của Minh"))
	waitFor(t, "reply to Minh", func() bool { return len(businessSends(h)) == 1 })
	settle()
	h.clock.Advance(time.Minute)
	h.b.handleUpdate(namedBiz(911, "Hoa", "tin của Hoa"))
	waitFor(t, "reply to Hoa", func() bool { return len(businessSends(h)) == 2 })
	settle()

	h.b.handleUpdate(dm(testOwner, testOwner, "/tomtat"))
	waitFor(t, "list", func() bool { return len(ownerSends(h)) == 1 })
	list, _ := ownerSends(h)[0]["text"].(string)
	if !strings.Contains(list, "1. Hoa") || !strings.Contains(list, "2. Minh") {
		t.Fatalf("list must be newest first with names: %q", list)
	}
	h.b.handleUpdate(dm(testOwner, testOwner, "/tomtat 2"))
	waitFor(t, "summary", func() bool { return len(ownerSends(h)) == 2 })
	if !strings.Contains((*inputs)[0], "tin của Minh") || strings.Contains((*inputs)[0], "tin của Hoa") {
		t.Fatalf("/tomtat 2 must summarise Minh's chat only: %q", (*inputs)[0])
	}
	h.b.handleUpdate(dm(testOwner, testOwner, "/tomtat 9"))
	waitFor(t, "bad index", func() bool { return len(ownerSends(h)) == 3 })
	if txt, _ := ownerSends(h)[2]["text"].(string); !strings.Contains(txt, "Không có chat số 9") {
		t.Fatalf("bad index reply: %q", txt)
	}
}

func TestTranscriptKeepsLast100AndCapsLength(t *testing.T) {
	var rec trRecord
	for i := 0; i < 130; i++ {
		rec.append("", []trEntry{{R: "c", T: int64(i), X: "tin " + strconv.Itoa(i)}})
	}
	if len(rec.Msgs) != transcriptKeep || rec.Msgs[0].X != "tin 30" || rec.Updated != 129 {
		t.Fatalf("len=%d first=%q updated=%d", len(rec.Msgs), rec.Msgs[0].X, rec.Updated)
	}
	var big trRecord
	for i := 0; i < 100; i++ {
		big.append("", []trEntry{{R: "c", T: int64(i), X: strings.Repeat("x", transcriptMsgRunes)}})
	}
	total := 0
	for _, e := range big.Msgs {
		total += runeCount(e.X)
	}
	if total > transcriptMaxRunes {
		t.Fatalf("per-chat cap exceeded: %d", total)
	}

	h := newHarness(nil)
	h.b.mu.Lock()
	h.b.recordTranscriptLocked("d:1", "c", "", strings.Repeat("y", 2000))
	got := h.b.trMem["d:1"].Msgs[0].X
	h.b.mu.Unlock()
	if runeCount(got) != transcriptMsgRunes {
		t.Fatalf("per-message cap = %d", runeCount(got))
	}
}

func TestTranscriptPersistsForgetsAndExpires(t *testing.T) {
	dir := t.TempDir()
	mk := func() *harness {
		return newHarness(func(c *Config) {
			c.DBFile = filepath.Join(dir, "bridge.db")
			c.OwnerAwayMinutes = 0
		})
	}
	h := mk()
	h.b.handleUpdate(namedBiz(920, "An", "xin chào shop"))
	waitFor(t, "reply", func() bool { return len(businessSends(h)) == 1 })
	settle()
	if err := h.b.Close(); err != nil {
		t.Fatal(err)
	}

	h2 := mk()
	defer func() { _ = h2.b.Close() }()
	mu, inputs := summaryLLM(h2)
	h2.b.handleUpdate(dm(testOwner, testOwner, "/tomtat 1")) // no list shown yet this run
	waitFor(t, "summary after restart", func() bool { return len(ownerSends(h2)) == 1 })
	mu.Lock()
	in := (*inputs)[0]
	mu.Unlock()
	if !strings.Contains(in, "Khách: xin chào shop") || !strings.Contains(in, "Trợ lý AI:") {
		t.Fatalf("transcript lost across restart: %q", in)
	}

	key := "b:" + testConn + ":920"
	h2.b.handleUpdate(biz(testConn, 920, testOwner, "/forget"))
	settle()
	if _, ok := h2.b.loadTranscript(key); ok {
		t.Fatal("/forget must delete the transcript")
	}

	// expiry: an old transcript disappears with the hourly prune
	h2.b.mu.Lock()
	h2.b.recordTranscriptLocked("b:"+testConn+":921", "c", "Cũ", "tin cũ")
	h2.b.mu.Unlock()
	_ = h2.b.flushNow()
	h2.clock.Advance(8 * 24 * time.Hour)
	h2.b.mu.Lock()
	h2.b.lastPrune = time.Time{}
	h2.b.dirtyMeta = true
	h2.b.mu.Unlock()
	_ = h2.b.flushNow()
	if _, ok := h2.b.loadTranscript("b:" + testConn + ":921"); ok {
		t.Fatal("transcripts must expire after memory_days")
	}
}

func TestGroupMessagesAreNotTranscribed(t *testing.T) {
	h := newHarness(nil)
	u := Update{UpdateID: 1, Message: &Message{
		Chat: Chat{ID: -100, Type: "group"},
		From: &User{ID: 77},
		Text: "@example_bot hello",
	}}
	h.b.handleUpdate(u)
	waitSent(t, h, 1)
	h.b.mu.Lock()
	_, ok := h.b.trMem["d:-100"]
	h.b.mu.Unlock()
	if ok {
		t.Fatal("group chats must not be transcribed")
	}
}

func TestSummaryEmptyAndLLMError(t *testing.T) {
	h := newHarness(nil)
	h.b.handleUpdate(dm(testOwner, testOwner, "/tomtat"))
	waitSent(t, h, 1)
	if !strings.Contains(lastSentText(h), "Chưa có cuộc trò chuyện nào") {
		t.Fatalf("empty list: %q", lastSentText(h))
	}
	h.b.handleUpdate(dm(930, 930, "hi"))
	waitSent(t, h, 2)
	h.llm.mu.Lock()
	h.llm.errs = []error{&LLMError{Status: 400, Kind: "http"}}
	h.llm.mu.Unlock()
	h.b.handleUpdate(dm(testOwner, testOwner, "/tomtat 1"))
	waitSent(t, h, 3)
	if !strings.Contains(lastSentText(h), "chưa tóm tắt được") {
		t.Fatalf("llm error: %q", lastSentText(h))
	}
}
