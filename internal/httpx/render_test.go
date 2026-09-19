package httpx

import (
	"strings"
	"testing"
)

func TestCompactStaysLiteralAndTight(t *testing.T) {
	// No spaces, HTML left literal, non-ASCII not escaped.
	if got := string(Compact(map[string]any{"a": 1})); got != `{"a":1}` {
		t.Errorf("Compact spacing = %q", got)
	}
	if got := string(Compact("héllo")); got != `"héllo"` {
		t.Errorf("Compact escaped non-ASCII: %q", got)
	}
	if got := string(Compact("<a>&b")); !strings.Contains(got, "<a>&b") {
		t.Errorf("Compact escaped HTML: %q", got)
	}
}

func TestCellTypeRendering(t *testing.T) {
	cases := map[any]string{
		nil:          "",
		true:         "1",
		false:        "0",
		"a\tb\rc":    "a b c", // tabs/CR scrubbed to spaces
		float64(2.5): "2.5",
		int64(7):     "7",
		int(3):       "3",
	}
	for in, want := range cases {
		if got := cell(in); got != want {
			t.Errorf("cell(%v) = %q, want %q", in, got, want)
		}
	}
	// A structured value is rendered as its compact JSON.
	if got := cell([]any{1, 2}); got != `[1,2]` {
		t.Errorf("cell(slice) = %q", got)
	}
}

func TestScrubNewlinesAreLiteral(t *testing.T) {
	// Embedded newlines become a literal backslash-n so a cell can never break the TSV row.
	if got := scrub("line1\nline2"); got != `line1\nline2` {
		t.Errorf("scrub = %q", got)
	}
}

func TestToTSVDictSection(t *testing.T) {
	payload := map[string]any{"rows": []any{
		map[string]any{"y": 2, "x": 1},
		map[string]any{"x": 3}, // missing y
	}}
	got := ToTSV(payload, "")
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if lines[0] != "#rows" {
		t.Fatalf("section header = %q", lines[0])
	}
	// Columns are the sorted union of keys.
	if lines[1] != "x\ty" {
		t.Errorf("column header = %q, want %q", lines[1], "x\ty")
	}
	if lines[2] != "1\t2" {
		t.Errorf("row 1 = %q", lines[2])
	}
	if lines[3] != "3\t" { // missing y renders as an empty cell
		t.Errorf("row 2 = %q, want %q", lines[3], "3\t")
	}
}

func TestToTSVScalarSection(t *testing.T) {
	got := ToTSV([]any{"a", "b"}, "nums")
	want := "#nums\na\nb\n"
	if got != want {
		t.Errorf("ToTSV scalar = %q, want %q", got, want)
	}
	// An empty list still emits its header and nothing else.
	if got := ToTSV(map[string]any{"e": []any{}}, ""); got != "#e\n" {
		t.Errorf("ToTSV empty list = %q", got)
	}
}

func TestToTSVScalarValue(t *testing.T) {
	if got := ToTSV(map[string]any{"k": 5}, ""); got != "k\t5\n" {
		t.Errorf("ToTSV scalar value = %q", got)
	}
}

func TestToJSONL(t *testing.T) {
	rows := []any{map[string]any{"a": 1}, map[string]any{"b": 2}}
	if got := ToJSONL(rows, ""); got != "{\"a\":1}\n{\"b\":2}\n" {
		t.Errorf("ToJSONL list = %q", got)
	}
	// Named section is picked from a map payload.
	got := ToJSONL(map[string]any{"ms": []any{map[string]any{"i": 9}}}, "ms")
	if got != "{\"i\":9}\n" {
		t.Errorf("ToJSONL named = %q", got)
	}
	if got := ToJSONL([]any{}, ""); got != "" {
		t.Errorf("ToJSONL empty = %q, want empty", got)
	}
}

func TestRenderMediaTypes(t *testing.T) {
	cases := map[string]string{
		"":      "application/json",
		"json":  "application/json",
		"tsv":   "text/tab-separated-values; charset=utf-8",
		"jsonl": "application/x-ndjson; charset=utf-8",
	}
	for format, want := range cases {
		_, media := Render([]any{map[string]any{"x": 1}}, format, "s")
		if media != want {
			t.Errorf("Render(%q) media = %q, want %q", format, media, want)
		}
	}
}
