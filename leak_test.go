package main

import "testing"

// sendsTo reports how many sendMessage calls targeted chatID. The fake stores
// params after a JSON round-trip, so the id may be float64 or int64.
func sendsTo(h *harness, chatID int64) int {
	n := 0
	for _, s := range h.tg.sends() {
		switch v := s["chat_id"].(type) {
		case int64:
			if v == chatID {
				n++
			}
		case float64:
			if int64(v) == chatID {
				n++
			}
		}
	}
	return n
}

// The reported leak: the owner typed /start in a customer chat and the bot
// answered with the command list inside that chat.
func TestCommandInCustomerChatRepliesPrivately(t *testing.T) {
	h := newHarness(nil)
	h.b.handleUpdate(biz(testConn, 800, testOwner, "/start"))
	waitSent(t, h, 1)
	settle()

	sends := h.tg.sends()
	if n := sendsTo(h, 800); n != 0 {
		t.Fatalf("the customer chat must never receive command output, got %d", n)
	}
	if n := sendsTo(h, testOwner); n != 1 {
		t.Fatalf("the owner must get the reply privately, got %d", n)
	}
	if _, has := sends[0]["business_connection_id"]; has {
		t.Fatalf("the private reply must not use the business connection: %v", sends[0])
	}
	if text, _ := sends[0]["text"].(string); text == "" {
		t.Fatal("empty reply")
	}
}

func TestUnknownCommandInCustomerChatDoesNotLeak(t *testing.T) {
	h := newHarness(nil)
	h.b.handleUpdate(biz(testConn, 801, testOwner, "/up"))
	waitSent(t, h, 1)
	settle()

	if n := sendsTo(h, 801); n != 0 {
		t.Fatalf("unknown command help leaked into the customer chat: %d", n)
	}
	if n := sendsTo(h, testOwner); n != 1 {
		t.Fatalf("owner private reply = %d", n)
	}
}

// The command must still take effect even though the answer goes elsewhere.
func TestPauseFromCustomerChatStillApplies(t *testing.T) {
	h := newHarness(nil)
	h.b.handleUpdate(biz(testConn, 802, testOwner, "/pause"))
	waitSent(t, h, 1)

	h.b.mu.Lock()
	paused := h.b.paused
	h.b.mu.Unlock()
	if !paused {
		t.Fatal("/pause typed in a customer chat must still pause the bot")
	}
	if n := sendsTo(h, 802); n != 0 {
		t.Fatalf("customer chat received the pause confirmation: %d", n)
	}
}

// In the owner's own chat nothing changes: the answer comes back there.
func TestCommandInOwnerDMStaysThere(t *testing.T) {
	h := newHarness(nil)
	h.b.handleUpdate(dm(testOwner, testOwner, "/status"))
	waitSent(t, h, 1)
	if n := sendsTo(h, testOwner); n != 1 {
		t.Fatalf("owner DM reply = %d", n)
	}
}

// /rules in a customer chat reads and edits files; those answers are for the
// owner only.
func TestRulesCommandInCustomerChatRepliesPrivately(t *testing.T) {
	h, _, _ := opsHarness(t)
	h.b.handleUpdate(biz(testConn, 803, testOwner, "/rules"))
	waitSent(t, h, 1)
	settle()

	if n := sendsTo(h, 803); n != 0 {
		t.Fatalf("rule output leaked into the customer chat: %d", n)
	}
	if n := sendsTo(h, testOwner); n != 1 {
		t.Fatalf("owner private rule reply = %d", n)
	}
}
