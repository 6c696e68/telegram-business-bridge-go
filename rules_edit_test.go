package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

const editPromptPrefix = "You edit the rule files"

func lastSentText(h *harness) string {
	sends := h.tg.sends()
	if len(sends) == 0 {
		return ""
	}
	text, _ := sends[len(sends)-1]["text"].(string)
	return text
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

// The owner's complaint: asking the bot to change the rules did nothing.
// A plain message in the private chat must edit the files for real.
func TestOwnerPlainMessageEditsRules(t *testing.T) {
	h, ai, pin := opsHarness(t)
	h.llm.fn = func(msgs []ChatMsg) (string, bool) {
		if strings.HasPrefix(msgs[0].Content, editPromptPrefix) {
			if !strings.Contains(msgs[1].Content, "bỏ quy tắc AI cũ, thêm: luôn hỏi số đơn hàng") ||
				!strings.Contains(msgs[1].Content, "Rule AI cũ") || !strings.Contains(msgs[1].Content, "không hứa hẹn") {
				return `{"reply":"thiếu ngữ cảnh","owner_rules":null,"ai_rules":null}`, true
			}
			return "```json\n" + `{"reply":"Dạ đã sửa.","owner_rules":"- Luôn xưng em, không hứa hẹn thay chủ.\n- Luôn hỏi số đơn hàng.","ai_rules":""}` + "\n```", true
		}
		return "câu trả lời như khách", false
	}
	if got := h.b.handleUpdate(dm(testOwner, testOwner, "bỏ quy tắc AI cũ, thêm: luôn hỏi số đơn hàng")); got != "owner_edit" {
		t.Fatalf("owner plain message got %q", got)
	}
	waitSent(t, h, 1)
	if got := readFile(t, pin); got != "- Luôn xưng em, không hứa hẹn thay chủ.\n- Luôn hỏi số đơn hàng." {
		t.Fatalf("owner rules file = %q", got)
	}
	if got := readFile(t, ai); got != "" {
		t.Fatalf("AI rules must be cleared, got %q", got)
	}
	reply := lastSentText(h)
	if !strings.Contains(reply, "Dạ đã sửa.") || !strings.Contains(reply, "P2. - Luôn hỏi số đơn hàng.") ||
		!strings.Contains(reply, "/rules undo") {
		t.Fatalf("confirmation must show the new rules: %q", reply)
	}
	if strings.Contains(reply, "câu trả lời như khách") {
		t.Fatal("the owner's message must not be answered like a customer chat")
	}

	// The new rule is used for the next customer reply.
	h.b.handleUpdate(dm(5150, 5150, "chào shop"))
	waitSent(t, h, 2)
	if !llmSaw(h, "Luôn hỏi số đơn hàng") {
		t.Fatal("edited owner rule must reach the reply prompt")
	}
}

func TestOwnerEditQuestionChangesNothing(t *testing.T) {
	h, ai, pin := opsHarness(t)
	h.llm.fn = func(msgs []ChatMsg) (string, bool) {
		if strings.HasPrefix(msgs[0].Content, editPromptPrefix) {
			return `{"reply":"Hiện có 1 quy tắc của anh và 1 quy tắc AI.","owner_rules":null,"ai_rules":null}`, true
		}
		return "ok", false
	}
	h.b.handleUpdate(dm(testOwner, testOwner, "đang có mấy quy tắc?"))
	waitSent(t, h, 1)
	if !strings.Contains(lastSentText(h), "Hiện có 1 quy tắc") {
		t.Fatalf("reply = %q", lastSentText(h))
	}
	if readFile(t, pin) != "- Luôn xưng em, không hứa hẹn thay chủ." || readFile(t, ai) != "- Rule AI cũ." {
		t.Fatal("a question must not change the files")
	}
	if _, err := os.Stat(pin + ".prev"); err == nil {
		t.Fatal("no backup expected when nothing changed")
	}
}

func TestOwnerEditBadAnswerChangesNothing(t *testing.T) {
	h, ai, pin := opsHarness(t)
	h.llm.fn = func(msgs []ChatMsg) (string, bool) {
		if strings.HasPrefix(msgs[0].Content, editPromptPrefix) {
			return "Dạ em đã sửa rồi ạ", true // not JSON: must not be trusted
		}
		return "ok", false
	}
	h.b.handleUpdate(dm(testOwner, testOwner, "sửa giúp"))
	waitSent(t, h, 1)
	if !strings.Contains(lastSentText(h), "chưa sửa gì") {
		t.Fatalf("the owner must be told nothing changed: %q", lastSentText(h))
	}
	if readFile(t, pin) != "- Luôn xưng em, không hứa hẹn thay chủ." || readFile(t, ai) != "- Rule AI cũ." {
		t.Fatal("files changed on an unparseable answer")
	}
}

func TestOwnerEditSensitiveAIRulesRejected(t *testing.T) {
	h, ai, pin := opsHarness(t)
	h.llm.fn = func(msgs []ChatMsg) (string, bool) {
		if strings.HasPrefix(msgs[0].Content, editPromptPrefix) {
			return `{"reply":"ok","owner_rules":"- Hotline của shop: 0901234567.","ai_rules":"- Gọi 0901234567 để đặt hàng."}`, true
		}
		return "ok", false
	}
	h.b.handleUpdate(dm(testOwner, testOwner, "thêm hotline"))
	waitSent(t, h, 1)
	if readFile(t, pin) != "- Hotline của shop: 0901234567." {
		t.Fatalf("owner rules are the owner's own content and must be saved: %q", readFile(t, pin))
	}
	if readFile(t, ai) != "- Rule AI cũ." {
		t.Fatalf("sensitive AI rules must be rejected: %q", readFile(t, ai))
	}
	if !strings.Contains(lastSentText(h), "dữ liệu nhạy cảm") {
		t.Fatalf("the owner must be told why: %q", lastSentText(h))
	}
}

func TestRulesExplicitCommandsAndUndo(t *testing.T) {
	h, ai, pin := opsHarness(t)

	h.b.handleUpdate(dm(testOwner, testOwner, "/rules add Luôn chào khách\nKhông báo giá qua chat"))
	waitSent(t, h, 1)
	if got := readFile(t, pin); got != "- Luôn xưng em, không hứa hẹn thay chủ.\n- Luôn chào khách\n- Không báo giá qua chat" {
		t.Fatalf("after add: %q", got)
	}

	h.b.handleUpdate(dm(testOwner, testOwner, "/rules del P1 A1"))
	waitSent(t, h, 2)
	if got := readFile(t, pin); got != "- Luôn chào khách\n- Không báo giá qua chat" {
		t.Fatalf("after del: %q", got)
	}
	if got := readFile(t, ai); got != "" {
		t.Fatalf("A1 must be deleted: %q", got)
	}

	h.b.handleUpdate(dm(testOwner, testOwner, "/rules undo"))
	waitSent(t, h, 3)
	if got := readFile(t, pin); got != "- Luôn xưng em, không hứa hẹn thay chủ.\n- Luôn chào khách\n- Không báo giá qua chat" {
		t.Fatalf("undo must restore owner rules: %q", got)
	}
	if got := readFile(t, ai); got != "- Rule AI cũ." {
		t.Fatalf("undo must restore AI rules: %q", got)
	}
	h.b.handleUpdate(dm(testOwner, testOwner, "/rules undo"))
	waitSent(t, h, 4)
	if !strings.Contains(lastSentText(h), "Không có gì để hoàn tác") {
		t.Fatalf("second undo: %q", lastSentText(h))
	}

	h.b.handleUpdate(dm(testOwner, testOwner, "/rules set # Bán hàng\n- Dòng một\n\n\n- Dòng hai"))
	waitSent(t, h, 5)
	if got := readFile(t, pin); got != "# Bán hàng\n- Dòng một\n\n- Dòng hai" {
		t.Fatalf("set must keep line breaks: %q", got)
	}

	h.b.handleUpdate(dm(testOwner, testOwner, "/rules del P9"))
	waitSent(t, h, 6)
	if !strings.Contains(lastSentText(h), "Không có dòng P9") {
		t.Fatalf("bad ref: %q", lastSentText(h))
	}
}

func TestRulesEditCommandRunsAsync(t *testing.T) {
	h, _, pin := opsHarness(t)
	h.llm.fn = func(msgs []ChatMsg) (string, bool) {
		if strings.HasPrefix(msgs[0].Content, editPromptPrefix) {
			return `{"reply":"xong","owner_rules":"- Mới.","ai_rules":null}`, true
		}
		return "ok", false
	}
	if got := h.b.handleUpdate(dm(testOwner, testOwner, "/rules edit thay hết bằng: Mới")); got != "owner_edit" {
		t.Fatalf("got %q", got)
	}
	waitSent(t, h, 1)
	if readFile(t, pin) != "- Mới." {
		t.Fatalf("pinned = %q", readFile(t, pin))
	}
}

// An owner edit made while a distill is thinking wins: the distill result,
// computed from the old rules, is discarded.
func TestOwnerEditBeatsInFlightDistill(t *testing.T) {
	h, ai, _ := opsHarness(t)
	gate := make(chan struct{})
	started := make(chan struct{}, 1)
	h.llm.fn = func(msgs []ChatMsg) (string, bool) {
		if strings.HasPrefix(msgs[0].Content, "You maintain the long-term rules") {
			select {
			case started <- struct{}{}:
			default:
			}
			<-gate
			return "- Rule distill cũ.", true
		}
		return "ok", false
	}
	h.b.handleUpdate(dm(840, 840, "một"))
	waitSent(t, h, 1)
	h.b.handleUpdate(dm(840, 840, "hai"))
	waitSent(t, h, 2)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("distill never started")
	}
	h.b.handleUpdate(dm(testOwner, testOwner, "/rules del A1"))
	waitSent(t, h, 3)
	close(gate)
	time.Sleep(100 * time.Millisecond)
	if got := readFile(t, ai); got != "" {
		t.Fatalf("stale distill overwrote the owner's edit: %q", got)
	}
}

func TestParseCommandKeepsLineBreaks(t *testing.T) {
	cmd, args := parseCommand("/Rules@example_bot set a\nb\n\nc  ")
	if cmd != "/rules" || args != "set a\nb\n\nc" {
		t.Fatalf("cmd=%q args=%q", cmd, args)
	}
	if cmd, args := parseCommand("/status"); cmd != "/status" || args != "" {
		t.Fatalf("cmd=%q args=%q", cmd, args)
	}
}

func TestParseOwnerEditTolerance(t *testing.T) {
	res, err := parseOwnerEdit("Đây:\n```json\n{\"reply\":\"ok\",\"owner_rules\":\"- a\",\"ai_rules\":null}\n```")
	if err != nil || res.Reply != "ok" || res.OwnerRules == nil || *res.OwnerRules != "- a" || res.AIRules != nil {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if _, err := parseOwnerEdit("không có json"); err == nil {
		t.Fatal("non-JSON must be an error")
	}
}
