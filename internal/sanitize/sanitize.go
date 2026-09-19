package sanitize

import (
	"strings"
	"unicode"
)

// Ingest-time content filter: fold invisible/control chars, cap lengths. SQL safety lives entirely
// in bound parameters (never here); this is the *content* filter only. All funcs are idempotent.

func keep(ch rune) bool { return ch == '\n' || ch == '\t' || ch == ' ' }

// invisible: format / bidi-override / zero-width chars -> must never reach storage.
func invisible(ch rune) bool {
	switch {
	case ch >= 0x200B && ch <= 0x200F, // ZWSP ZWNJ ZWJ LRM RLM
		ch >= 0x202A && ch <= 0x202E, // LRE RLE PDF LRO RLO
		ch >= 0x2060 && ch <= 0x2064, // word joiner + invisible operators
		ch >= 0x2066 && ch <= 0x2069, // LRI RLI FSI PDI
		ch == 0xFEFF, ch >= 0xFFF9 && ch <= 0xFFFB:
		return true
	}
	return false
}

func drop(ch rune) bool {
	if keep(ch) {
		return false
	}
	if invisible(ch) {
		return true
	}
	// Unicode Cc (control), Cf (format), Co (private), Cs (surrogate), Zl, Zp categories.
	switch {
	case ch == 0x7F || (ch >= 0x00 && ch <= 0x1F), // Cc (ASCII control)
		ch >= 0x80 && ch <= 0x9F,     // C1 controls (Cc)
		ch == 0x2028 || ch == 0x2029, // Zl Zp
		unicode.Is(unicode.Cf, ch):
		return true
	}
	return false
}

func hasDanger(s string) bool {
	for _, ch := range s {
		if ch == '\r' || drop(ch) {
			return true
		}
	}
	return false
}

// Fold collapses newlines and strips invisible/control chars; no length limit, no reflowing.
func Fold(v any) string {
	if v == nil {
		return ""
	}
	var text string
	switch t := v.(type) {
	case string:
		text = t
	default:
		text = toString(v)
	}
	if text == "" {
		return ""
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	if !hasDanger(text) {
		return text
	}
	var b strings.Builder
	for _, ch := range text {
		if !drop(ch) {
			b.WriteRune(ch)
		}
	}
	return b.String()
}

// Text sanitises a multiline value: fold newlines, rstrip each line, trim edges, cap length.
func Text(v any, limit int) string {
	out := Fold(v)
	lines := strings.Split(out, "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimRight(ln, " \t")
	}
	out = strings.TrimSpace(strings.Join(lines, "\n"))
	return Cap(out, limit)
}

// Oneline sanitises a single-line value: names, subjects, descriptions, search terms.
func Oneline(v any, limit int) string {
	return Cap(strings.Join(strings.Fields(Fold(v)), " "), limit)
}

// Cap truncates to limit *runes* (by code point, not byte).
func Cap(s string, limit int) string {
	if limit <= 0 || utf8Len(s) <= limit {
		return s
	}
	runes := []rune(s)
	return string(runes[:limit])
}

func utf8Len(s string) int { return len([]rune(s)) }

func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return trimFloat(t)
	case int:
		return itoa(t)
	case int64:
		return itoa(int(t))
	case bool:
		if t {
			return "true"
		}
		return "false"
	}
	return ""
}

func Sqlish(v any) bool {
	probe := strings.ToLower(Oneline(v, 400))
	for _, tok := range []string{
		"'--", ";--", "drop table", "delete from", "insert into", "union select",
		"' or '1'='1", "or 1=1", "\" or \"1\"=\"1", "xp_cmdshell", "'; ",
	} {
		if strings.Contains(probe, tok) {
			return true
		}
	}
	return false
}
