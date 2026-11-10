package main

import (
	"strings"
	"testing"
)

// The /tomtat output that used to arrive as literal "**bold**".
func TestTelegramHTMLSummary(t *testing.T) {
	in := "📝 Tóm tắt chat với Tuấn Minh (100 tin):\n\n" +
		"1. **Khách cần gì**\n" +
		"- Nhờ chủ xem trang và hỏi tình hình trade.\n" +
		"- Đang code app khác.\n\n" +
		"4. **Việc còn tồn đọng**\n" +
		"- (không có)"
	got, ok := telegramHTML(in)
	if !ok {
		t.Fatal("summary must convert")
	}
	for _, want := range []string{"<b>Khách cần gì</b>", "<b>Việc còn tồn đọng</b>", "• Nhờ chủ xem trang"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "**") {
		t.Fatalf("asterisks leaked:\n%s", got)
	}
}

func TestTelegramHTMLPlainStaysPlain(t *testing.T) {
	in := "Dạ em chào anh, giá vàng đang 4044.47 ạ."
	got, ok := telegramHTML(in)
	if ok || got != in {
		t.Fatalf("plain text must not be touched: ok=%v %q", ok, got)
	}
}

func TestTelegramHTMLUnbalancedFallsBack(t *testing.T) {
	// An odd number of ** would produce an unclosed tag; Telegram would reject
	// the message, so the original text must be kept.
	in := "giá **đang tăng"
	if got, ok := telegramHTML(in); ok || got != in {
		t.Fatalf("unbalanced bold must stay plain: ok=%v %q", ok, got)
	}
}

func TestTelegramHTMLEscapesUserText(t *testing.T) {
	in := "**a < b & c**"
	got, ok := telegramHTML(in)
	if !ok {
		t.Fatal("must convert")
	}
	if !strings.Contains(got, "&lt;") || !strings.Contains(got, "&amp;") {
		t.Fatalf("user text was not escaped: %q", got)
	}
}

func TestTelegramHTMLInlineCodeAndHeading(t *testing.T) {
	got, ok := telegramHTML("# Tiêu đề\nchạy `go test` trước")
	if !ok {
		t.Fatal("must convert")
	}
	if !strings.Contains(got, "<b>Tiêu đề</b>") || !strings.Contains(got, "<code>go test</code>") {
		t.Fatalf("bad conversion: %q", got)
	}
}

func TestSendMsgUsesHTML(t *testing.T) {
	h := newHarness(nil)
	h.b.sendMsg(900, "**đậm** và `code`", "")
	s := h.tg.sends()
	if len(s) != 1 {
		t.Fatalf("sends = %d", len(s))
	}
	if s[0]["parse_mode"] != "HTML" {
		t.Fatalf("parse_mode = %v", s[0]["parse_mode"])
	}
	text, _ := s[0]["text"].(string)
	if !strings.Contains(text, "<b>đậm</b>") || strings.Contains(text, "**") {
		t.Fatalf("text = %q", text)
	}
}

func TestSendMsgFallsBackToPlainOnReject(t *testing.T) {
	h := newHarness(nil)
	h.tg.sendErrs = []error{&TGError{Method: "sendMessage", Code: 400, Desc: "can't parse entities"}}

	if !h.b.sendMsg(901, "**đậm**", "") {
		t.Fatal("the plain fallback must still deliver the reply")
	}
	s := h.tg.sends()
	if len(s) != 2 {
		t.Fatalf("sends = %d, want 2 (html then plain)", len(s))
	}
	if _, has := s[0]["parse_mode"]; !has {
		t.Fatalf("first attempt must be HTML: %v", s[0])
	}
	if _, has := s[1]["parse_mode"]; has {
		t.Fatalf("fallback must not set parse_mode: %v", s[1])
	}
	if s[1]["text"] != "**đậm**" {
		t.Fatalf("fallback text = %v", s[1]["text"])
	}
}

func TestSendMsgPlainHasNoParseMode(t *testing.T) {
	h := newHarness(nil)
	h.b.sendMsg(902, "Dạ em chào anh", "")
	s := h.tg.sends()
	if _, has := s[0]["parse_mode"]; has {
		t.Fatalf("plain replies must not set parse_mode: %v", s[0])
	}
}
