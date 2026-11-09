package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizeAcceptsCleanRule(t *testing.T) {
	in := "# Về chủ\n- Chủ thích trả lời ngắn gọn, tiếng Việt thân thiện.\n"
	got := sanitizeRules(in, "@owner", false, 1000)
	if got != in {
		t.Fatalf("clean rules must pass intact, got %q", got)
	}
}

func TestSanitizeDropsSensitiveLine(t *testing.T) {
	in := "- Khách hay hỏi về bảo hành sản phẩm.\n" +
		"- SĐT chủ là 0901234567.\n" +
		"- Gửi mail lienhe@shop.vn nhé.\n" +
		"- Giá combo hiện nay là 500k.\n" +
		"- Xem chi tiết tại https://example.com/sale.\n"
	got := sanitizeRules(in, "@owner", false, 1000)
	if got != "" {
		t.Fatalf("expected whole-file reject (over half bad), got %q", got)
	}
}

func TestSanitizeKeepsMajorityClean(t *testing.T) {
	in := "- Hỗ trợ khách trong giờ hành chính.\n" +
		"- Khách hay hỏi về bảo hành sản phẩm.\n" +
		"- Không public hotline 0901234567.\n"
	got := sanitizeRules(in, "@owner", false, 1000)
	if strings.Contains(got, "0901234567") {
		t.Fatalf("phone leaked: %q", got)
	}
	if !strings.Contains(got, "bảo hành") {
		t.Fatalf("benign lines dropped: %q", got)
	}
}

func TestSanitizeHandlesTelegramHandle(t *testing.T) {
	in := "- Chỉ nhắn xử lý qua @owner.\n- Tránh tương tác với @someone_else.\n"
	got := sanitizeRules(in, "@owner", false, 1000)
	if strings.Contains(got, "@someone_else") {
		t.Fatalf("foreign handle leaked: %q", got)
	}
	if !strings.Contains(got, "@owner") {
		t.Fatalf("owner tag must stay: %q", got)
	}
}

func TestSanitizeURLToggle(t *testing.T) {
	line := "- Tham chiếu https://example.com/manual.\n"
	if got := sanitizeRules(line, "@owner", false, 1000); got != "" {
		t.Fatalf("URL must be blocked by default, got %q", got)
	}
	if got := sanitizeRules(line, "@owner", true, 1000); got != line {
		t.Fatalf("URL must be allowed when enabled, got %q", got)
	}
}

func TestSanitizeCapsLength(t *testing.T) {
	long := "- " + strings.Repeat("a", 500) + "\n"
	got := sanitizeRules(long, "@owner", false, 100)
	if runeCount(got) > 100 {
		t.Fatalf("cap not enforced: %d", runeCount(got))
	}
}

func TestRulesRejectVerbatimQuote(t *testing.T) {
	source := strings.Repeat("abcdefgh", 40) // 320 runes
	r := []rune(source)
	verbatim := string(r[80:120])
	if !blocksQuoted(verbatim, []string{source}) {
		t.Fatal("40-rune verbatim block must be detected")
	}
	if blocksQuoted("quy tắc thuần túy không trích lời", []string{source}) {
		t.Fatal("clean output must not be flagged")
	}
	if blocksQuoted("bất kỳ", []string{"short"}) {
		t.Fatal("short sources must be ignored")
	}
}

func TestParseDistillOutput(t *testing.T) {
	if s, un := parseDistillOutput(" UNCHANGED "); !un || s != "" {
		t.Fatal("sentinel not honored")
	}
	if s, un := parseDistillOutput("```markdown\n- Rule A\n```"); un || s != "- Rule A" {
		t.Fatalf("fence not stripped: %q", s)
	}
	if _, un := parseDistillOutput(""); !un {
		t.Fatal("empty means unchanged (nothing new)")
	}
	if s, un := parseDistillOutput("# Về chủ\n- Rule A"); un || s != "# Về chủ\n- Rule A" {
		t.Fatalf("plain markdown altered: %q", s)
	}
}

func TestWriteRulesAtomic(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rules.md")
	if err := writeRulesAtomic(p, "# A\n- giữ nguyên"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p)
	if err != nil || string(data) != "# A\n- giữ nguyên" {
		t.Fatalf("write/read mismatch: %v %q", err, data)
	}
	info, err := os.Stat(p)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("rules file must be 0600, got %v", info.Mode())
	}
}

func TestSanitizeHeadingValidated(t *testing.T) {
	in := "## Hotline 0901234567\n- Khách hay hỏi về bảo hành.\n"
	got := sanitizeRules(in, "@owner", false, 1000)
	if strings.Contains(got, "0901234567") {
		t.Fatalf("heading bypassed sanitizer: %q", got)
	}
	if !strings.Contains(got, "bảo hành") {
		t.Fatalf("benign line lost: %q", got)
	}
}

func TestSanitizeSpacedPhone(t *testing.T) {
	in := "- Gọi 090 123 4567 nhé.\n- Hỗ trợ trong giờ hành chính.\n"
	got := sanitizeRules(in, "@owner", false, 1000)
	if strings.Contains(got, "090") {
		t.Fatalf("spaced phone leaked: %q", got)
	}
}

func TestSanitizeSecrets(t *testing.T) {
	in := "- Password: hunter2xyz\n- Trả lời lịch sự.\n"
	got := sanitizeRules(in, "@owner", false, 1000)
	if strings.Contains(strings.ToLower(got), "password") {
		t.Fatalf("secret leaked: %q", got)
	}
}

func TestBlocksQuotedSliding(t *testing.T) {
	source := strings.Repeat("abcdefghij", 20) // 200 runes
	r := []rune(source)
	for _, start := range []int{0, 5, 97, 100, 133, 160} {
		if start+quoteWindow > len(r) {
			continue
		}
		if !blocksQuoted(string(r[start:start+quoteWindow]), []string{source}) {
			t.Fatalf("window at %d not detected", start)
		}
	}
}

func TestRulesSourceDistillGuardOnShortChat(t *testing.T) {
	// sources shorter than the window are ignored (nothing to compare)
	if blocksQuoted("short", []string{"tiny"}) {
		t.Fatal("short sources must not match")
	}
}

func TestSanitizeColonlessSecretAndBlob(t *testing.T) {
	in := "- Dùng token sk abcdefghijklmnop1234567890 nhé.\n- JWT eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcdef1234567890.\n- Trả lời ngắn gọn.\n- Hỗ trợ khách trong giờ hành chính.\n"
	got := sanitizeRules(in, "@owner", false, 1000)
	if strings.Contains(got, "abcdefghijklmnop") || strings.Contains(got, "eyJhbGci") {
		t.Fatalf("secret-shaped token leaked: %q", got)
	}
	if !strings.Contains(got, "ngắn gọn") {
		t.Fatalf("benign line dropped: %q", got)
	}
}
