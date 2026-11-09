package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Owner editing of the rule files from Telegram.
//
// rules.pinned.md holds the owner's own rules: injected verbatim, never
// rewritten by the distiller, and binding for it. rules.md holds the rules the
// distiller learned. The owner can edit both with explicit /rules commands or
// in plain language in the private chat with the bot; every edit keeps a
// one-level backup for /rules undo.

const ownerEditPrompt = "You edit the rule files of an automatic Telegram assistant on behalf of its owner.\n" +
	"OWNER RULES (rules.pinned.md) are the owner's own instructions. AI RULES (rules.md) were learned automatically.\n" +
	"Apply the owner's request exactly. Answer ONLY with one JSON object, no code fences:\n" +
	"{\"reply\": \"short confirmation or answer in the owner's language\", " +
	"\"owner_rules\": \"complete new owner rules text\" or null, \"ai_rules\": \"complete new AI rules text\" or null}\n" +
	"- null means that file stays unchanged; an empty string clears it.\n" +
	"- New or changed rules the owner asks for go into owner_rules (one '- ' bullet per rule) unless the owner explicitly targets the AI rules.\n" +
	"- To remove or correct a rule that lives in the AI rules, edit ai_rules.\n" +
	"- Keep every line the owner did not ask to change exactly as it is. Never invent rules the owner did not ask for.\n" +
	"- If the message is a question rather than an edit, answer it in reply and set both files to null."

type ownerEditResult struct {
	Reply      string  `json:"reply"`
	OwnerRules *string `json:"owner_rules"`
	AIRules    *string `json:"ai_rules"`
}

// parseOwnerEdit extracts the JSON object from an LLM answer, tolerating code
// fences or text around it.
func parseOwnerEdit(out string) (ownerEditResult, error) {
	var res ownerEditResult
	i, j := strings.Index(out, "{"), strings.LastIndex(out, "}")
	if i < 0 || j <= i {
		return res, errors.New("no json object")
	}
	if err := json.Unmarshal([]byte(out[i:j+1]), &res); err != nil {
		return res, err
	}
	return res, nil
}

// cleanRulesText normalises rules text: strips code fences and trailing
// spaces, drops blank-line runs, and caps the size.
func cleanRulesText(s string, limit int) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```markdown")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	lines := strings.Split(strings.TrimSpace(s), "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		ln = strings.TrimRight(ln, " \t\r")
		if ln == "" && (len(out) == 0 || out[len(out)-1] == "") {
			continue
		}
		out = append(out, ln)
	}
	return truncRunes(strings.TrimSpace(strings.Join(out, "\n")), limit)
}

// ruleLines returns the non-empty lines of a rules text.
func ruleLines(text string) []string {
	var out []string
	for _, ln := range strings.Split(text, "\n") {
		if strings.TrimSpace(ln) != "" {
			out = append(out, strings.TrimRight(ln, " \t\r"))
		}
	}
	return out
}

// normRule makes a line comparable regardless of bullet and punctuation.
func normRule(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimLeft(s, "-*•+ ")
	return strings.TrimRight(s, " .!;,")
}

// dropOwnerDuplicates removes AI rule lines that merely repeat an owner rule.
func dropOwnerDuplicates(ai, owner string) string {
	if owner == "" || ai == "" {
		return ai
	}
	seen := map[string]bool{}
	for _, ln := range ruleLines(owner) {
		seen[normRule(ln)] = true
	}
	var kept []string
	for _, ln := range strings.Split(ai, "\n") {
		if strings.TrimSpace(ln) != "" && seen[normRule(ln)] {
			continue
		}
		kept = append(kept, ln)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// rulesView renders both rule files with P#/A# line numbers for editing.
func (b *Bridge) rulesView() string {
	b.mu.Lock()
	owner, ai := b.rulesPinned, b.rulesText
	state := "bật"
	if !b.rulesOn() {
		state = "tắt"
	}
	b.mu.Unlock()

	var sb strings.Builder
	sb.WriteString("📌 Quy tắc của bạn (AI không bao giờ sửa):\n")
	if lines := ruleLines(owner); len(lines) == 0 {
		sb.WriteString("(trống)\n")
	} else {
		for i, ln := range lines {
			fmt.Fprintf(&sb, "P%d. %s\n", i+1, ln)
		}
	}
	fmt.Fprintf(&sb, "\n🤖 Quy tắc AI tự học (%s):\n", state)
	if lines := ruleLines(ai); len(lines) == 0 {
		sb.WriteString("(trống)\n")
	} else {
		for i, ln := range lines {
			fmt.Fprintf(&sb, "A%d. %s\n", i+1, ln)
		}
	}
	sb.WriteString("\n✏️ Nhắn tự nhiên cho bot để sửa, hoặc: /rules add … · /rules del P1 A2 · /rules set … · /rules undo")
	return truncUTF16(sb.String(), telegramUnitLimit)
}

func writeOrRemove(path, text string) error {
	if strings.TrimSpace(text) == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return writeRulesAtomic(path, text+"\n")
}

// applyRulesEdit writes the owner's new rule texts (nil = unchanged) with a
// one-level backup for /rules undo, then invalidates any in-flight distill.
func (b *Bridge) applyRulesEdit(owner, ai *string) error {
	if owner != nil && b.cfg.RulesPinnedFile == "" {
		return errors.New("rules_pinned_file chưa được cấu hình")
	}
	if ai != nil && b.cfg.RulesFile == "" {
		return errors.New("rules_file chưa được cấu hình")
	}
	b.rulesFileMu.Lock()
	defer b.rulesFileMu.Unlock()

	b.mu.Lock()
	curOwner, curAI := b.rulesPinned, b.rulesText
	b.mu.Unlock()

	if b.cfg.RulesPinnedFile != "" {
		if err := writeRulesAtomic(b.cfg.RulesPinnedFile+".prev", curOwner); err != nil {
			return err
		}
	}
	if b.cfg.RulesFile != "" {
		if err := writeRulesAtomic(b.cfg.RulesFile+".prev", curAI); err != nil {
			return err
		}
	}
	if owner != nil {
		if err := writeOrRemove(b.cfg.RulesPinnedFile, *owner); err != nil {
			return err
		}
	}
	if ai != nil {
		if err := writeOrRemove(b.cfg.RulesFile, *ai); err != nil {
			return err
		}
	}
	b.mu.Lock()
	if owner != nil {
		b.rulesPinned = *owner
	}
	if ai != nil {
		b.rulesText = *ai
	}
	b.rulesGen++ // a distill computed from the old rules must not overwrite this
	b.mu.Unlock()
	logf("INFO", "rules edited by owner")
	return nil
}

// rulesUndo restores the rule files saved by the last owner edit.
func (b *Bridge) rulesUndo() (bool, error) {
	b.rulesFileMu.Lock()
	defer b.rulesFileMu.Unlock()
	restored := false
	var owner, ai *string
	if b.cfg.RulesPinnedFile != "" {
		if data, err := os.ReadFile(b.cfg.RulesPinnedFile + ".prev"); err == nil {
			v := strings.TrimSpace(string(data))
			owner = &v
		}
	}
	if b.cfg.RulesFile != "" {
		if data, err := os.ReadFile(b.cfg.RulesFile + ".prev"); err == nil {
			v := strings.TrimSpace(string(data))
			ai = &v
		}
	}
	if owner != nil {
		if err := writeOrRemove(b.cfg.RulesPinnedFile, *owner); err != nil {
			return false, err
		}
		_ = os.Remove(b.cfg.RulesPinnedFile + ".prev")
		restored = true
	}
	if ai != nil {
		if err := writeOrRemove(b.cfg.RulesFile, *ai); err != nil {
			return false, err
		}
		_ = os.Remove(b.cfg.RulesFile + ".prev")
		restored = true
	}
	if restored {
		b.mu.Lock()
		if owner != nil {
			b.rulesPinned = *owner
		}
		if ai != nil {
			b.rulesText = *ai
		}
		b.rulesGen++
		b.mu.Unlock()
		logf("INFO", "rules edit undone by owner")
	}
	return restored, nil
}

// splitSub separates a sub-command from its raw payload (newlines preserved).
func splitSub(args string) (string, string) {
	args = strings.TrimSpace(args)
	if args == "" {
		return "", ""
	}
	for i, r := range args {
		if r == ' ' || r == '\n' || r == '\t' || r == '\r' {
			return strings.ToLower(args[:i]), strings.TrimSpace(args[i:])
		}
	}
	return strings.ToLower(args), ""
}

// rulesCommand handles /rules sub-commands. It returns the reply text and
// whether an asynchronous job took over the reply.
func (b *Bridge) rulesCommand(chatID int64, conn, args string) (string, bool) {
	sub, rest := splitSub(args)
	switch sub {
	case "", "view", "show", "xem":
		return b.rulesView(), false
	case "reset":
		b.rulesFileMu.Lock()
		b.mu.Lock()
		b.rulesText = ""
		b.rulesGen++
		b.mu.Unlock()
		if b.cfg.RulesFile != "" {
			_ = os.Remove(b.cfg.RulesFile)
		}
		b.rulesFileMu.Unlock()
		logf("INFO", "rules reset by owner")
		return "🗑 Đã xoá quy tắc AI tự học (quy tắc của bạn giữ nguyên).", false
	case "off":
		b.mu.Lock()
		b.rulesRunOn = false
		b.rulesGen++
		b.saveStateLocked()
		b.mu.Unlock()
		return "🚫 Đã tắt quy tắc AI tự học (quy tắc của bạn vẫn áp dụng).", false
	case "on":
		b.mu.Lock()
		b.rulesRunOn = true
		b.saveStateLocked()
		b.mu.Unlock()
		return "✅ Đã bật quy tắc AI tự học.", false
	case "add", "them", "thêm":
		if rest == "" {
			return "Cú pháp: /rules add <quy tắc> (mỗi dòng một quy tắc)", false
		}
		b.mu.Lock()
		owner := b.rulesPinned
		b.mu.Unlock()
		var added []string
		for _, ln := range ruleLines(rest) {
			ln = strings.TrimSpace(ln)
			if !strings.HasPrefix(ln, "-") && !strings.HasPrefix(ln, "#") && !strings.HasPrefix(ln, "*") {
				ln = "- " + ln
			}
			added = append(added, ln)
		}
		next := strings.TrimSpace(strings.TrimSpace(owner) + "\n" + strings.Join(added, "\n"))
		if runeCount(next) > b.cfg.RulesMaxChars {
			return fmt.Sprintf("⚠️ Quá dài (tối đa %d ký tự). Xoá bớt bằng /rules del.", b.cfg.RulesMaxChars), false
		}
		if err := b.applyRulesEdit(&next, nil); err != nil {
			return "⚠️ Không lưu được: " + err.Error(), false
		}
		return "✅ Đã thêm.\n\n" + b.rulesView(), false
	case "del", "xoa", "xoá":
		return b.rulesDelete(rest), false
	case "set":
		if rest == "" {
			return "Cú pháp: /rules set <toàn bộ quy tắc của bạn> (giữ nguyên xuống dòng). Dùng /rules del để xoá từng dòng.", false
		}
		next := cleanRulesText(rest, b.cfg.RulesMaxChars)
		if err := b.applyRulesEdit(&next, nil); err != nil {
			return "⚠️ Không lưu được: " + err.Error(), false
		}
		return "✅ Đã thay toàn bộ quy tắc của bạn.\n\n" + b.rulesView(), false
	case "undo", "hoantac":
		ok, err := b.rulesUndo()
		switch {
		case err != nil:
			return "⚠️ Hoàn tác lỗi: " + err.Error(), false
		case !ok:
			return "Không có gì để hoàn tác.", false
		}
		return "↩️ Đã hoàn tác lần sửa gần nhất.\n\n" + b.rulesView(), false
	case "edit", "sua", "sửa":
		if rest == "" {
			return "Cú pháp: /rules edit <yêu cầu>, ví dụ: /rules edit bỏ quy tắc về giá", false
		}
		b.startOwnerEdit(chatID, conn, rest)
		return "", true
	}
	return "Lệnh rules: /rules (xem) · add · del P1 A2 · set · edit <yêu cầu> · undo · reset · off · on\n" +
		"Hoặc nhắn tự nhiên cho bot, ví dụ: \"thêm quy tắc: luôn hỏi số đơn hàng\".", false
}

// rulesDelete removes lines referenced as P<n> (owner) or A<n> (AI).
func (b *Bridge) rulesDelete(refs string) string {
	fields := strings.FieldsFunc(refs, func(r rune) bool { return r == ' ' || r == ',' || r == '\n' || r == ';' })
	if len(fields) == 0 {
		return "Cú pháp: /rules del P1 A2 (xem số dòng bằng /rules)"
	}
	b.mu.Lock()
	ownerLines, aiLines := ruleLines(b.rulesPinned), ruleLines(b.rulesText)
	b.mu.Unlock()
	dropP, dropA := map[int]bool{}, map[int]bool{}
	for _, f := range fields {
		f = strings.ToUpper(strings.TrimSpace(f))
		if len(f) < 2 {
			return "⚠️ Không hiểu \"" + f + "\". Dùng dạng P1 hoặc A2."
		}
		n, err := strconv.Atoi(f[1:])
		switch {
		case err != nil || n < 1:
			return "⚠️ Không hiểu \"" + f + "\". Dùng dạng P1 hoặc A2."
		case f[0] == 'P' && n <= len(ownerLines):
			dropP[n-1] = true
		case f[0] == 'A' && n <= len(aiLines):
			dropA[n-1] = true
		default:
			return "⚠️ Không có dòng " + f + ". Xem lại bằng /rules."
		}
	}
	keep := func(lines []string, drop map[int]bool) string {
		var out []string
		for i, ln := range lines {
			if !drop[i] {
				out = append(out, ln)
			}
		}
		return strings.Join(out, "\n")
	}
	var owner, ai *string
	if len(dropP) > 0 {
		v := keep(ownerLines, dropP)
		owner = &v
	}
	if len(dropA) > 0 {
		v := keep(aiLines, dropA)
		ai = &v
	}
	if err := b.applyRulesEdit(owner, ai); err != nil {
		return "⚠️ Không lưu được: " + err.Error()
	}
	return fmt.Sprintf("🗑 Đã xoá %d dòng.\n\n%s", len(dropP)+len(dropA), b.rulesView())
}

// startOwnerEdit runs a natural-language edit in the background so the poll
// loop never waits on the LLM.
func (b *Bridge) startOwnerEdit(chatID int64, conn, request string) {
	b.jobWG.Add(1)
	go func() {
		defer b.jobWG.Done()
		defer func() {
			if r := recover(); r != nil {
				logf("ERROR", "owner edit crashed: %v", r)
			}
		}()
		b.ownerEditJob(chatID, conn, request)
	}()
}

func (b *Bridge) ownerEditJob(chatID int64, conn, request string) {
	if !b.rulesEnabled {
		b.sendMsg(chatID, "⚠️ Tính năng rules đang tắt trong config (rules_enabled).", conn)
		return
	}
	b.mu.Lock()
	owner, ai := b.rulesPinned, b.rulesText
	b.mu.Unlock()
	orNone := func(s string) string {
		if strings.TrimSpace(s) == "" {
			return "(empty)"
		}
		return s
	}
	input := "=== OWNER RULES (rules.pinned.md) ===\n" + orNone(owner) +
		"\n\n=== AI RULES (rules.md) ===\n" + orNone(ai) +
		"\n\n=== OWNER REQUEST ===\n" + truncRunes(request, 2000)

	ctx, cancel := context.WithCancel(context.Background())
	go b.typingLoop(ctx, chatID, conn)
	out, err := b.callLLM(ctx, []ChatMsg{
		{Role: "system", Content: ownerEditPrompt},
		{Role: "user", Content: input},
	})
	cancel()
	if err != nil || strings.TrimSpace(out) == "" {
		logf("WARN", "owner edit: llm error: %v", err)
		b.sendMsg(chatID, "⚠️ AI đang lỗi nên chưa sửa gì. Thử lại sau, hoặc dùng /rules add · del · set.", conn)
		return
	}
	res, err := parseOwnerEdit(out)
	if err != nil {
		b.sendMsg(chatID, "⚠️ AI trả lời không đúng định dạng nên chưa sửa gì. Thử nói rõ hơn, hoặc dùng /rules add · del · set.", conn)
		return
	}

	var newOwner, newAI *string
	var notes []string
	if res.OwnerRules != nil {
		v := cleanRulesText(*res.OwnerRules, b.cfg.RulesMaxChars)
		if v != strings.TrimSpace(owner) {
			newOwner = &v
		}
	}
	if res.AIRules != nil {
		v := cleanRulesText(*res.AIRules, b.cfg.RulesMaxChars)
		if v != "" {
			clean := sanitizeRules(v, "@"+b.cfg.OwnerUsername, b.cfg.RulesAllowURL, b.cfg.RulesMaxChars)
			if clean == "" {
				notes = append(notes, "⚠️ Không đổi quy tắc AI vì nội dung mới có dữ liệu nhạy cảm (SĐT, email, link…). Thông tin kiểu này hãy đưa vào quy tắc của bạn.")
				v = strings.TrimSpace(ai)
			} else {
				if clean != v {
					notes = append(notes, "ℹ️ Đã bỏ các dòng có dữ liệu nhạy cảm khỏi quy tắc AI.")
				}
				v = clean
			}
		}
		if v != strings.TrimSpace(ai) {
			newAI = &v
		}
	}

	reply := strings.TrimSpace(truncRunes(res.Reply, 1000))
	if newOwner == nil && newAI == nil {
		if reply == "" {
			reply = "Không có gì thay đổi."
		}
		if len(notes) > 0 {
			reply += "\n\n" + strings.Join(notes, "\n")
		}
		b.sendMsg(chatID, truncUTF16(reply, telegramUnitLimit), conn)
		return
	}
	if err := b.applyRulesEdit(newOwner, newAI); err != nil {
		b.sendMsg(chatID, "⚠️ Không lưu được: "+err.Error(), conn)
		return
	}
	if reply == "" {
		reply = "✅ Đã sửa."
	}
	if len(notes) > 0 {
		reply += "\n" + strings.Join(notes, "\n")
	}
	reply += "\n\n" + b.rulesView() + "\n↩️ Sai thì gõ /rules undo."
	b.sendMsg(chatID, truncUTF16(reply, telegramUnitLimit), conn)
}
