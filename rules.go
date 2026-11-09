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
	reSpaced = regexp.MustCompile(`(?:\+?\d[ .\-]?){9,}`)
	reMoney  = regexp.MustCompile(`(?i)\d+[,.]?\d*\s*(vnd|đ|usd|\$|k\b|tr\b|triệu|tỷ)`)
	reHandle = regexp.MustCompile(`@[A-Za-z0-9_]{3,}`)
	reID     = regexp.MustCompile(`(?i)\bid\b[^0-9]{0,3}\d{3,}`)
	reSecret = regexp.MustCompile(`(?i)(mật\s*khẩu|password|passwd|api[_ ]?key|secret|token)[^\n]{0,12}?[A-Za-z0-9+/=_\-\.]{8,}`)
	reBlob   = regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}|[A-Za-z0-9+/=_\-]{32,}`)
)

// lineBad reports a sensitive-data policy violation in one candidate rule line.
func lineBad(line, ownerTag string, allowURL bool) bool {
	if reEmail.MatchString(line) || reDigits.MatchString(line) ||
		reSpaced.MatchString(line) || reMoney.MatchString(line) ||
		reID.MatchString(line) || reSecret.MatchString(line) ||
		reBlob.MatchString(line) {
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
// Headings are validated too: they can carry the same sensitive payload.
func sanitizeRules(text, ownerTag string, allowURL bool, limit int) string {
	lines := strings.Split(text, "\n")
	kept, rejected := 0, 0
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			out = append(out, ln)
			continue
		}
		if strings.HasPrefix(ln, "#") {
			if lineBad(ln, ownerTag, allowURL) {
				rejected++
				continue
			}
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

// quoteWindow is the sliding-window size used to detect copied text. The
// window advances one rune at a time so any 32+ rune verbatim copy is caught.
const quoteWindow = 32

// blocksQuoted reports whether out contains a near-verbatim block copied from
// any source conversation (every 32-rune window of every source).
func blocksQuoted(out string, sources []string) bool {
	for _, src := range sources {
		r := []rune(src)
		n := len(r)
		if n < quoteWindow {
			continue
		}
		for start := 0; start+quoteWindow <= n; start++ {
			if strings.Contains(out, string(r[start:start+quoteWindow])) {
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

// loadRules reads both the AI-maintained rules file and the owner-pinned file
// into memory (each capped). The pinned file is never written by the program.
func (b *Bridge) loadRules() {
	if !b.rulesEnabled {
		return
	}
	var ai, pinned string
	if b.cfg.RulesFile != "" {
		if data, err := os.ReadFile(b.cfg.RulesFile); err == nil {
			ai = truncRunes(string(data), b.cfg.RulesMaxChars)
		}
	}
	if b.cfg.RulesPinnedFile != "" {
		if data, err := os.ReadFile(b.cfg.RulesPinnedFile); err == nil {
			pinned = truncRunes(string(data), b.cfg.RulesMaxChars)
		}
	}
	b.mu.Lock()
	b.rulesText = ai
	b.rulesPinned = pinned
	b.mu.Unlock()
}

// rulesOn reports whether rules are active at runtime.
func (b *Bridge) rulesOn() bool { return b.rulesEnabled && b.rulesRunOn }

// rulesNoteLocked returns the prompt blocks appended to the system prompt.
// The owner-pinned block is injected verbatim and is never synthesised away.
func (b *Bridge) rulesNoteLocked() string {
	if !b.rulesOn() {
		return ""
	}
	note := ""
	if b.rulesText != "" {
		note += rulesHeader + truncRunes(b.rulesText, b.cfg.RulesMaxChars)
	}
	if b.rulesPinned != "" {
		note += "\n\n[Owner rules — always follow, never modify]\n" +
			truncRunes(b.rulesPinned, b.cfg.RulesMaxChars)
	}
	return note
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
// It is single-flight, shares the LLM semaphore, never retries, and never
// persists sensitive or verbatim content. A generation counter invalidates the
// write when /rules reset|off or /forget runs while the LLM is thinking.
func (b *Bridge) distillJob() {
	b.mu.Lock()
	if b.distillActive || !b.rulesOn() {
		b.mu.Unlock()
		return
	}
	b.distillActive = true
	gen := b.rulesGen
	input, srcs := b.buildDistillInputLocked()
	cur := b.rulesText
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.distillActive = false
		b.mu.Unlock()
	}()

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
	if b.cfg.RulesFile == "" {
		return
	}

	// Abort if the owner reset/disabled rules or forgot a chat meanwhile.
	b.mu.Lock()
	stillWanted := b.rulesOn() && b.rulesGen == gen && clean != cur
	b.mu.Unlock()
	if !stillWanted {
		return
	}
	if err := writeRulesAtomic(b.cfg.RulesFile, clean); err != nil {
		logf("WARN", "rules write failed")
		return
	}
	b.mu.Lock()
	if b.rulesGen != gen {
		// reset/off/forget landed while the write was in flight: undo it.
		b.mu.Unlock()
		_ = os.Remove(b.cfg.RulesFile)
		return
	}
	b.rulesText = clean
	b.mu.Unlock()
	logf("INFO", "rules updated: %d chars", runeCount(clean))
}
