package main

import "strings"

// Telegram formatting.
//
// Models answer in Markdown, but sendMessage without parse_mode shows the
// asterisks literally ("**Khách cần gì**"). This converts the small subset that
// Telegram actually renders into HTML: bold, italic, inline code, code blocks,
// headings and bullets. Anything that cannot be converted safely is sent as
// plain text, so a formatting bug can never cost a reply.

// telegramHTML converts Markdown to the HTML subset Telegram accepts. ok is
// false when the text changed nothing, or when the result would not be safe to
// send as HTML: the caller then sends the original text without parse_mode.
func telegramHTML(in string) (string, bool) {
	if in == "" || !looksMarkdown(in) {
		return in, false
	}
	lines := strings.Split(in, "\n")
	out := make([]string, 0, len(lines))
	changed := false
	inFence := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			changed = true
			if inFence {
				out = append(out, "</pre>")
			} else {
				out = append(out, "<pre>")
			}
			inFence = !inFence
			continue
		}
		if inFence {
			out = append(out, escapeHTML(line))
			continue
		}

		esc := escapeHTML(line)

		// Headings become a bold line.
		if h, ok := heading(esc); ok {
			out = append(out, "<b>"+h+"</b>")
			changed = true
			continue
		}
		// Bullets render as a dot; Telegram has no list markup.
		if strings.HasPrefix(esc, "- ") || strings.HasPrefix(esc, "* ") {
			esc = "• " + esc[2:]
			changed = true
		} else if strings.HasPrefix(esc, "– ") {
			esc = "• " + esc[len("– "):]
			changed = true
		}

		converted, ch := inlineHTML(esc)
		if ch {
			changed = true
		}
		out = append(out, converted)
	}
	if inFence {
		return in, false // unbalanced code fence: keep the original
	}

	res := strings.Join(out, "\n")
	if !changed || !tagsBalanced(res) {
		return in, false
	}
	return res, true
}

// looksMarkdown is a cheap pre-check so plain replies take the fast path.
func looksMarkdown(s string) bool {
	return strings.ContainsAny(s, "*`#_")
}

// heading strips the leading hashes of an escaped Markdown heading.
func heading(line string) (string, bool) {
	rest := strings.TrimLeft(line, "#")
	if len(rest) == len(line) || !strings.HasPrefix(rest, " ") {
		return "", false
	}
	body := strings.TrimSpace(rest)
	if body == "" {
		return "", false
	}
	return body, true
}

// inlineHTML converts **bold**, __bold__, *italic*, _italic_ and `code`.
func inlineHTML(s string) (string, bool) {
	var b strings.Builder
	changed := false
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "**"):
			if j := strings.Index(s[i+2:], "**"); j > 0 {
				b.WriteString("<b>")
				b.WriteString(s[i+2 : i+2+j])
				b.WriteString("</b>")
				i += 2 + j + 2
				changed = true
				continue
			}
		case strings.HasPrefix(s[i:], "__"):
			if j := strings.Index(s[i+2:], "__"); j > 0 {
				b.WriteString("<b>")
				b.WriteString(s[i+2 : i+2+j])
				b.WriteString("</b>")
				i += 2 + j + 2
				changed = true
				continue
			}
		case s[i] == '`':
			if j := strings.IndexByte(s[i+1:], '`'); j > 0 {
				b.WriteString("<code>")
				b.WriteString(s[i+1 : i+1+j])
				b.WriteString("</code>")
				i += 1 + j + 1
				changed = true
				continue
			}
		case s[i] == '*':
			// A lone asterisk must start a word and close on a boundary, so
			// "5*3" or "**" fragments are never turned into italics.
			if j := strings.IndexByte(s[i+1:], '*'); j > 0 && startsToken(s, i) && endsToken(s, i+1+j) {
				b.WriteString("<i>")
				b.WriteString(s[i+1 : i+1+j])
				b.WriteString("</i>")
				i += 1 + j + 1
				changed = true
				continue
			}
		case s[i] == '_':
			if j := strings.IndexByte(s[i+1:], '_'); j > 0 && startsToken(s, i) && endsToken(s, i+1+j) {
				b.WriteString("<i>")
				b.WriteString(s[i+1 : i+1+j])
				b.WriteString("</i>")
				i += 1 + j + 1
				changed = true
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String(), changed
}

func startsToken(s string, i int) bool {
	return i == 0 || s[i-1] == ' ' || s[i-1] == '(' || s[i-1] == '\t'
}

func endsToken(s string, i int) bool {
	return i+1 >= len(s) || s[i+1] == ' ' || s[i+1] == '.' || s[i+1] == ',' ||
		s[i+1] == '!' || s[i+1] == '?' || s[i+1] == ')' || s[i+1] == ':' || s[i+1] == ';'
}

// escapeHTML makes user and model text safe inside a Telegram HTML message.
func escapeHTML(s string) string {
	if !strings.ContainsAny(s, "&<>") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// tagsBalanced reports whether every tag this converter emits is closed.
func tagsBalanced(s string) bool {
	for _, tag := range []string{"b", "i", "code", "pre"} {
		if strings.Count(s, "<"+tag+">") != strings.Count(s, "</"+tag+">") {
			return false
		}
	}
	return true
}
