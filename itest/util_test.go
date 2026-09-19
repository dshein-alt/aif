package itest

import (
	"strconv"
	"strings"
	"testing"

	"github.com/dshein-alt/aif/internal/harness"
)

// errCode returns the JSON "err" field (or "" when the body has none).
func errCode(r *harness.Resp) string { return r.Str("err") }

func eq[T comparable](t *testing.T, got, want T, ctx string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: got %v, want %v", ctx, got, want)
	}
}

func eqStr(t *testing.T, got, want, ctx string) {
	t.Helper()
	eq(t, got, want, ctx)
}

func has(t *testing.T, m map[string]any, key, ctx string) {
	t.Helper()
	if _, ok := m[key]; !ok {
		t.Fatalf("%s: expected key %q in %v", ctx, key, keys(m))
	}
}

func missing(t *testing.T, m map[string]any, key, ctx string) {
	t.Helper()
	if _, ok := m[key]; ok {
		t.Fatalf("%s: unexpected key %q in %v", ctx, key, keys(m))
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func contains(t *testing.T, s, sub, ctx string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Fatalf("%s: %q does not contain %q", ctx, s, sub)
	}
}

// mcp issues a tools/call over the MCP surface and returns the decoded JSON-RPC response.
func mcp(r *harness.Rig, token, tool string, args map[string]any, hdrs map[string]string) map[string]any {
	c := r.Client(token)
	for k, v := range hdrs {
		c = c.H(k, v)
	}
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args}}
	return c.Post("/mcp", body).JSON()
}

// mcpResult digs out result from a tools/call response.
func mcpResult(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	res, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("mcp result missing: %v", resp)
	}
	return res
}

func structured(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	sc, _ := result["structuredContent"].(map[string]any)
	return sc
}

// num coerces a decoded JSON number (always float64) to float64.
func num(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

// int64f coerces a decoded JSON number to int64.
func int64f(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int64:
		return x
	case int:
		return int64(x)
	}
	return 0
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func containsStr(xs []any, want string) bool {
	for _, x := range xs {
		if s, ok := x.(string); ok && s == want {
			return true
		}
	}
	return false
}

// hasError reports whether Validate() returned an ERROR-severity warning.
func hasError(warnings []string) bool {
	for _, w := range warnings {
		if strings.HasPrefix(w, "ERROR:") {
			return true
		}
	}
	return false
}

func contentText(result map[string]any) string {
	if c, ok := result["content"].([]any); ok && len(c) > 0 {
		if m, ok := c[0].(map[string]any); ok {
			if s, ok := m["text"].(string); ok {
				return s
			}
		}
	}
	if c, ok := result["content"].([]map[string]any); ok && len(c) > 0 {
		return c[0]["text"].(string)
	}
	return ""
}
