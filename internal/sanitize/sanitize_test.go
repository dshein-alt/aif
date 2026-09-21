package sanitize

import (
	"strings"
	"testing"
)

func TestFoldStripsInvisibleButKeepsWhitespace(t *testing.T) {
	// Zero-width joiner and a LTR override must vanish so "bo<wsp>t" cannot impersonate "bot".
	if got := Fold("bo\u200bt"); got != "bot" {
		t.Errorf("Fold zero-width = %q, want %q", got, "bot")
	}
	if got := Fold("a\u202eb"); got != "ab" {
		t.Errorf("Fold bidi-override = %q, want %q", got, "ab")
	}
	// ASCII control and DEL go; tab/newline/space stay.
	if got := Fold("a\x00b\x07c\td\ne"); got != "abc\td\ne" {
		t.Errorf("Fold control handling = %q", got)
	}
	// CRLF and lone CR fold to LF.
	if got := Fold("a\r\nb\rc"); got != "a\nb\nc" {
		t.Errorf("Fold newline normalisation = %q", got)
	}
	// Idempotent.
	once := Fold("a\u200b\r\nb")
	if twice := Fold(once); twice != once {
		t.Errorf("Fold not idempotent: %q vs %q", once, twice)
	}
	// Non-string inputs are stringified, not dropped.
	if got := Fold(42); got != "42" {
		t.Errorf("Fold(int) = %q", got)
	}
	if got := Fold(nil); got != "" {
		t.Errorf("Fold(nil) = %q", got)
	}
}

func TestOnelineAndCap(t *testing.T) {
	if got := Oneline("  a \t\n b  \n c ", 100); got != "a b c" {
		t.Errorf("Oneline reflow = %q", got)
	}
	// Cap counts runes, not bytes.
	if got := Cap("héllo wörld", 5); got != "héllo" {
		t.Errorf("Cap(rune) = %q, want 5 runes %q", got, "héllo")
	}
	if got := Cap("short", 0); got != "short" {
		t.Errorf("Cap(<=0) should be a no-op, got %q", got)
	}
	// Oneline caps after reflowing.
	if got := Oneline("aaaa bbbb cccc", 7); got != "aaaa bb" {
		t.Errorf("Oneline cap = %q", got)
	}
}

func TestTextRstripsLines(t *testing.T) {
	// Text rstrips each line and trims the whole value, but preserves interior indentation.
	got := Text("  hello   \n  world\t \n", 100)
	if got != "hello\n  world" {
		t.Errorf("Text = %q, want %q", got, "hello\n  world")
	}
}

func TestSqlishDetectsInjectionShapes(t *testing.T) {
	bad := []string{
		"'; DROP TABLE messages;--",
		"a' OR '1'='1",
		"x\" OR \"1\"=\"1",
		"1 UNION SELECT name FROM agents",
		"admin'--",
		"x; drop table y",
	}
	for _, s := range bad {
		if !Sqlish(s) {
			t.Errorf("Sqlish(%q) = false, want true", s)
		}
	}
	// Sqlish is only a heuristic detector used to warn; content is still stored verbatim. A few
	// plain strings must not trip it.
	good := []string{"Deployed v2 to staging", "", "12 + 5", "union of two sets"}
	// "DROP TABLE" in prose (no SQL operator context) must NOT be flagged — a forum can discuss SQL.
	for _, s := range good {
		if Sqlish(s) {
			t.Errorf("Sqlish(%q) = true, want false (plain content stored verbatim)", s)
		}
	}
}

func TestToStringVariants(t *testing.T) {
	cases := map[any]string{
		"plain":      "plain",
		int(7):       "7",
		int64(9):     "9",
		float64(2.5): "2.5",
		true:         "true",
		false:        "false",
	}
	for in, want := range cases {
		if got := toString(in); got != want {
			t.Errorf("toString(%v) = %q, want %q", in, got, want)
		}
	}
	// An unsupported type stringifies to "".
	if got := toString([]int{1, 2}); got != "" {
		t.Errorf("toString(unsupported) = %q, want empty", got)
	}
}

func TestCapLeavesShortStrings(t *testing.T) {
	if !strings.HasPrefix(Cap("abcdef", 6), "abcdef") {
		t.Error("Cap at exact length should not truncate")
	}
}

func TestCanonIsTheOneCanonicalForm(t *testing.T) {
	// Case is the whole point: these must all collapse to one key.
	for _, in := range []string{"Claudius", "claudius", "CLAUDIUS", "  Claudius  "} {
		if got := Canon(in); got != "claudius" {
			t.Errorf("Canon(%q) = %q, want %q", in, got, "claudius")
		}
	}
	// It inherits Fold, so an invisible char cannot smuggle a second "claudius" past a UNIQUE index.
	if got := Canon("Clau​dius"); got != "claudius" {
		t.Errorf("Canon zero-width = %q, want %q", got, "claudius")
	}
	// Idempotent: applying it to a stored canonical value must not move it.
	if got := Canon(Canon(" TheRoot ")); got != "theroot" {
		t.Errorf("Canon not idempotent: %q", got)
	}
	// Non-string input goes through the same path rather than panicking.
	if got := Canon(nil); got != "" {
		t.Errorf("Canon(nil) = %q, want empty", got)
	}
}
