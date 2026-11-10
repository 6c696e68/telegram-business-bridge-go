package main

import "testing"

// photoUpdate is a business message carrying one photo and no text.
func photoUpdate(chat, from int64) Update {
	u := biz(testConn, chat, from, "")
	u.BusinessMessage.Photo = []PhotoSize{
		{FileID: "small", Width: 90, Height: 160},
		{FileID: "big", Width: 800, Height: 1200},
	}
	return u
}

// An image turn must reach the vision model, not the text-only primary one.
func TestImageTurnUsesVisionModel(t *testing.T) {
	h := newHarness(func(c *Config) { c.DebounceMS = 0 })
	vision := &fakeLLM{reply: "Đây là chart XAUUSD khung M1."}
	h.b.visionLLM = vision

	h.b.handleUpdate(photoUpdate(700, 700))
	waitSent(t, h, 1)

	if vision.calls() != 1 {
		t.Fatalf("vision model calls = %d, want 1", vision.calls())
	}
	if h.llm.calls() != 0 {
		t.Fatalf("the text-only model must not receive an image turn, got %d calls", h.llm.calls())
	}
	if got := h.tg.sends()[0]["text"]; got != "Đây là chart XAUUSD khung M1." {
		t.Fatalf("reply = %v", got)
	}
}

// A text turn keeps using the primary model even when a vision model exists.
func TestTextTurnKeepsPrimaryModel(t *testing.T) {
	h := newHarness(func(c *Config) { c.DebounceMS = 0 })
	vision := &fakeLLM{reply: "vision reply"}
	h.b.visionLLM = vision

	h.b.handleUpdate(dm(701, 701, "giá sao em"))
	waitSent(t, h, 1)

	if h.llm.calls() != 1 || vision.calls() != 0 {
		t.Fatalf("text turn used the wrong client: primary=%d vision=%d", h.llm.calls(), vision.calls())
	}
}

// Without vision_model nothing changes: the main client handles everything.
func TestNoVisionModelFallsBackToPrimary(t *testing.T) {
	h := newHarness(func(c *Config) { c.DebounceMS = 0 })

	h.b.handleUpdate(photoUpdate(702, 702))
	waitSent(t, h, 1)

	if h.llm.calls() != 1 {
		t.Fatalf("primary calls = %d, want 1", h.llm.calls())
	}
	if h.b.visionClient() != Completer(h.llm) {
		t.Fatal("visionClient must fall back to the primary client")
	}
}

// The owner's private chat also routes image turns to the vision model.
func TestOwnerImageUsesVisionModel(t *testing.T) {
	h := newHarness(func(c *Config) { c.DebounceMS = 0 })
	vision := &fakeLLM{reply: "chart này đang giảm."}
	h.b.visionLLM = vision

	u := dm(testOwner, testOwner, "")
	u.Message.Photo = []PhotoSize{{FileID: "p", Width: 800, Height: 1200}}
	h.b.handleUpdate(u)
	waitSent(t, h, 1)

	if vision.calls() != 1 {
		t.Fatalf("owner image turn must use the vision model, vision calls = %d", vision.calls())
	}
}
