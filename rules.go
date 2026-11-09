package main

import (
	"context"
	"os"
	"regexp"
	"strings"
)

const rulesHeader = "\n\n[Long-term rules]\n"

var (
	reEmail  = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	reURL    = regexp.MustCompile(`(?i)https?://|www\.|\.(com|net|vn|org|io|biz)\b`)
	reDigits = regexp.MustCompile(`[0-9]{6,}`)
	reMoney  = regexp.MustCompile(`(?i)\d+[,.]?\d*\s*(vnd|đ|usd|\$|k\b|tr\b|triệu|tỷ)`)
	reHandle = regexp.MustCompile(`@[A-Za-z0-9_]{3,}`)
	reID     = regexp.MustCompile(`(?i)\bid\b[^0-9]{0,3}\d{3,}`)
)

// lineBad reports a sensitive-data policy violation in one candidate rule line.
func lineBad(line, ownerTag string, allowURL bool) bool {
	if reEmail.MatchString(line) || reDigits.MatchString(line) ||
		reMoney.MatchString(line) || reID.MatchString(line) {
		return true
	}
	if !allowURL && reURL.MatchString(line) {
		return true
	}
	for _, h := range reHandle.FindAllString(line, -1) {
		if h != ownerTag {
			return true
		}
	}
	return false
}

// sanitizeRules filters distiller output line by line. "" means nothing usable
// remained, or more than half the content lines were rejected (keep old file).
func sanitizeRules(text, ownerTag string, allowURL bool, limit int) string {
	lines := strings.Split(text, "\n")
	kept, rejected := 0, 0
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" || strings.HasPrefix(ln, "#") {
			out = append(out, ln)
			continue
		}
		if lineBad(ln, ownerTag, allowURL) {
			rejected++
			continue
		}
		kept++
		out = append(out, ln)
	}
	if kept == 0 || rejected*2 > kept+rejected {
		return ""
	}
	return truncRunes(strings.Join(out, "\n"), limit)
}

// blocksQuoted reports whether out contains a 40-rune block copied from any
// source conversation (windows at 0%, 25% and 50% of each source).
func blocksQuoted(out string, sources []string) bool {
	for _, src := range sources {
		r := []rune(src)
		n := len(r)
		if n < 80 {
			continue
		}
		for i := 0; i < 3; i++ {
			start := n * i / 4
			if start+40 > n {
				break
			}
			if strings.Contains(out, string(r[start:start+40])) {
				return true
			}
		}
	}
	return false
}

// parseDistillOutput normalises the distiller reply; unchanged=true keeps the
// current rules file untouched.
func parseDistillOutput(out string) (string, bool) {
	s := strings.TrimSpace(out)
	s = strings.TrimPrefix(s, "```markdown")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	if s == "" || s == "UNCHANGED" {
		return "", true
	}
	return s, false
}

// writeRulesAtomic writes rules.md via tmp+rename with 0600 permissions.
func writeRulesAtomic(path, text string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadRules reads the owner-editable rules file into memory (capped).
func (b *Bridge) loadRules() {
	if !b.rulesEnabled || b.cfg.RulesFile == "" {
		return
	}
	data, err := os.ReadFile(b.cfg.RulesFile)
	if err == nil {
		b.mu.Lock()
		b.rulesText = truncRunes(string(data), b.cfg.RulesMaxChars)
		b.mu.Unlock()
	}
}

// rulesOn reports whether rules are active at runtime.
func (b *Bridge) rulesOn() bool { return b.rulesEnabled && b.rulesRunOn }

// rulesNoteLocked returns the prompt block appended to the system prompt.
func (b *Bridge) rulesNoteLocked() string {
	if !b.rulesOn() || b.rulesText == "" {
		return ""
	}
	return rulesHeader + truncRunes(b.rulesText, b.cfg.RulesMaxChars)
}

const distillSystemPrompt = "You maintain the long-term rules file for an automatic Telegram assistant.\n" +
	"Input: the current rules and recent chats. Output: either the single word UNCHANGED or the complete new markdown file that\n" +
	"- keeps rules that are still true, removes stale or wrong ones, and adds only durable behavioural rules useful for FUTURE replies;\n" +
	"- never copies wording of the chats; contains no verbatim quotes;\n" +
	"- STRICTLY FORBIDDEN in the file: full names, phone numbers, emails, Telegram handles other than the owner tag,\n" +
	"  addresses, specific prices or amounts of money, public identifiers, and any verbatim message text.\n" +
	"Language: the owner's language (usually Vietnamese). No commentary, no code fences."

// buildDistillInputLocked compiles the distiller input from the most recently
// active chats (LRU front, up to 3) taking the newest 12 messages of each.
// Caller holds b.mu.
func (b *Bridge) buildDistillInputLocked() (string, []string) {
	var srcs []string
	var parts []string
	total := 0
	n := 0
	for el := b.lru.Front(); el != nil && n < 3; el = el.Next() {
		cs := el.Value.(*chatState)
		if len(cs.msgs) == 0 {
			continue
		}
		n++
		var sb strings.Builder
		sb.WriteString("--- chat ---\n")
		start := len(cs.msgs) - 12
		if start < 0 {
			start = 0
		}
		for _, msg := range cs.msgs[start:] {
			srcs = append(srcs, msg.Content)
			if total > 6000 {
				break
			}
			sb.WriteString(msg.Role)
			sb.WriteString(": ")
			sb.WriteString(msg.Content)
			sb.WriteString("\n")
			total += runeCount(msg.Content)
		}
		parts = append(parts, sb.String())
	}
	cur := b.rulesText
	if cur == "" {
		cur = "(none yet)"
	}
	return "=== CURRENT RULES ===\n" + cur + "\n\n=== RECENT CHATS ===\n" +
		strings.Join(parts, "\n"), srcs
}

// distillJob refreshes rules.md every rules_update_every successful replies.
// It shares the LLM semaphore, never retries, and never persists sensitive or
// verbatim content (sanitizer + quote guard).
func (b *Bridge) distillJob() {
	b.mu.Lock()
	input, srcs := b.buildDistillInputLocked()
	cur := b.rulesText
	b.mu.Unlock()

	msgs := []ChatMsg{
		{Role: "system", Content: distillSystemPrompt},
		{Role: "user", Content: input},
	}
	reply, err := b.callLLM(context.Background(), msgs)
	if err != nil || reply == "" {
		logf("WARN", "rules distill skipped: llm error")
		return
	}
	newText, unchanged := parseDistillOutput(reply)
	if unchanged {
		return
	}
	clean := sanitizeRules(newText, "@"+b.cfg.OwnerUsername, b.cfg.RulesAllowURL, b.cfg.RulesMaxChars)
	if clean == "" {
		logf("WARN", "rules distill rejected: sensitive or noisy output")
		return
	}
	if blocksQuoted(clean, srcs) {
		logf("WARN", "rules distill rejected: verbatim quote detected")
		return
	}
	if clean == cur || b.cfg.RulesFile == "" {
		return
	}
	if err := writeRulesAtomic(b.cfg.RulesFile, clean); err != nil {
		logf("WARN", "rules write failed")
		return
	}
	b.mu.Lock()
	b.rulesText = clean
	b.mu.Unlock()
	logf("INFO", "rules updated: %d chars", runeCount(clean))
}
