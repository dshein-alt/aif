// Package httpx is the HTTP transport: the agent REST surface, the hand-rolled MCP endpoint,
// the /ui human view, and the renderers that turn op payloads into JSON/TSV/JSONL bodies.
package httpx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Compact serialises a value as tight JSON (no spaces).
// HTML-significant chars are left unescaped and non-ASCII stays literal (emoji/UTF-8 pass through).
func Compact(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimRight(buf.Bytes(), "\n")
}

func cell(value any) string {
	switch t := value.(type) {
	case nil:
		return ""
	case bool:
		if t {
			return "1"
		}
		return "0"
	case string:
		return scrub(t)
	case float64:
		return scrub(strconv.FormatFloat(t, 'g', -1, 64))
	case int64:
		return scrub(strconv.FormatInt(t, 10))
	case int:
		return scrub(strconv.Itoa(t))
	default:
		return scrub(string(Compact(value)))
	}
}

func scrub(s string) string {
	s = strings.ReplaceAll(s, "\t", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", "\\n")
	return s
}

// ToTSV renders a payload as TSV sections: "#key" headers, one section per list in payload.
func ToTSV(payload any, section string) string {
	var lines []string
	var items [][2]any
	if list, ok := asList(payload); ok {
		items = [][2]any{{section, list}}
	} else if m, ok := payload.(map[string]any); ok {
		// Preserve key order deterministically by sorting keys; TSV sections are keyed by name so map
		// order is cosmetic, but a stable order keeps the output reproducible.
		for _, k := range sortedKeys(m) {
			items = append(items, [2]any{k, m[k]})
		}
	}
	for _, it := range items {
		key := it[0].(string)
		value := it[1]
		if list, ok := asList(value); ok {
			lines = append(lines, "#"+key)
			if len(list) == 0 {
				continue
			}
			if _, isDict := list[0].(map[string]any); isDict {
				var cols []string
				seen := map[string]bool{}
				for _, row := range list {
					rm, _ := row.(map[string]any)
					for _, col := range rowKeys(rm) {
						if !seen[col] {
							seen[col] = true
							cols = append(cols, col)
						}
					}
				}
				lines = append(lines, strings.Join(cols, "\t"))
				for _, row := range list {
					rm, _ := row.(map[string]any)
					cells := make([]string, len(cols))
					for i, col := range cols {
						cells[i] = cell(rm[col])
					}
					lines = append(lines, strings.Join(cells, "\t"))
				}
			} else {
				for _, v := range list {
					lines = append(lines, cell(v))
				}
			}
		} else if m, ok := value.(map[string]any); ok {
			lines = append(lines, "#"+key)
			for _, k := range rowKeys(m) {
				lines = append(lines, k+"\t"+cell(m[k]))
			}
		} else {
			lines = append(lines, key+"\t"+cell(value))
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

// ToJSONL renders the first (or named) list of a payload as one JSON object per line.
func ToJSONL(payload any, section string) string {
	var rows []any
	if list, ok := asList(payload); ok {
		rows = list
	} else if m, ok := payload.(map[string]any); ok {
		if section != "" {
			if list, ok := asList(m[section]); ok {
				rows = list
			}
		}
		if rows == nil {
			for _, k := range sortedKeys(m) {
				if list, ok := asList(m[k]); ok {
					rows = list
					break
				}
			}
		}
	}
	var out strings.Builder
	for i, row := range rows {
		if i > 0 {
			out.WriteByte('\n')
		}
		out.Write(Compact(row))
	}
	if len(rows) > 0 {
		out.WriteByte('\n')
	}
	return out.String()
}

// Render returns (body, mediaType) for the requested output format.
func Render(payload any, format, section string) ([]byte, string) {
	format = strings.ToLower(format)
	if format == "" {
		format = "json"
	}
	switch format {
	case "tsv":
		return []byte(ToTSV(payload, section)), "text/tab-separated-values; charset=utf-8"
	case "jsonl":
		return []byte(ToJSONL(payload, section)), "application/x-ndjson; charset=utf-8"
	default:
		return Compact(payload), "application/json"
	}
}

func asList(v any) ([]any, bool) {
	if list, ok := v.([]any); ok {
		return list, true
	}
	return nil, false
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// sort.Strings
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// rowKeys keeps map keys in a stable order (alphabetical) for column/section output.
func rowKeys(m map[string]any) []string { return sortedKeys(m) }

var _ = fmt.Sprintf
