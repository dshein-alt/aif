package httpx

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidMCPOrigin(t *testing.T) {
	cases := []struct {
		origin, host string
		want         bool
	}{
		{"", "example.com", true},                   // non-browser client
		{"http://example.com", "example.com", true}, // same host
		{"https://example.com:8443", "example.com:8443", true},
		{"http://Example.COM", "example.com", true},      // case-insensitive
		{"http://example.com:9999", "example.com", true}, // port alone does not change the host
		{"http://evil.com", "example.com", false},        // cross-host
		{"http://example.com.evil.com", "example.com", false},
		{"null", "example.com", false},               // opaque origin
		{"not a url", "example.com", false},          // malformed
		{"file:///etc/passwd", "example.com", false}, // no host
		{"//example.com", "example.com", false},      // no scheme
	}
	for _, c := range cases {
		if got := validMCPOrigin(c.origin, c.host); got != c.want {
			t.Errorf("validMCPOrigin(%q, %q) = %v, want %v", c.origin, c.host, got, c.want)
		}
	}
}

func TestMCPResponseMode(t *testing.T) {
	cases := []struct {
		accept string
		want   string
	}{
		{"", "json"}, // missing = */*
		{"application/json", "json"},
		{"text/event-stream", "sse"},                    // SSE only
		{"application/json, text/event-stream", "json"}, // both: JSON wins
		{"text/event-stream, application/json", "json"},
		{"*/*", "json"},
		{"text/*", "sse"},
		{"application/json;q=0, text/event-stream", "sse"}, // q=0 excludes
		{"text/event-stream;q=0", ""},                      // nothing acceptable left
		{"text/html", ""},
		{"application/json; charset=utf-8", "json"},
		{"garbage;;;", ""},
	}
	for _, c := range cases {
		if got := mcpResponseMode(c.accept); got != c.want {
			t.Errorf("mcpResponseMode(%q) = %q, want %q", c.accept, got, c.want)
		}
	}
}

func TestWriteMCPEvent(t *testing.T) {
	rec := httptest.NewRecorder()
	payload := map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"ok": true}}
	writeMCPEvent(rec, payload)

	res := rec.Result()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if cc := res.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, "event: message\ndata: ") || !strings.HasSuffix(body, "\n\n") {
		t.Fatalf("bad SSE framing: %q", body)
	}
	data := strings.TrimSuffix(strings.TrimPrefix(body, "event: message\ndata: "), "\n\n")
	var decoded map[string]any
	if err := json.Unmarshal([]byte(data), &decoded); err != nil {
		t.Fatalf("SSE data is not JSON: %v", err)
	}
	if decoded["id"].(float64) != 1 {
		t.Errorf("round-trip id = %v, want 1", decoded["id"])
	}
}

func TestProtocolList(t *testing.T) {
	list := protocolList()
	if len(list) != len(protocols) {
		t.Fatalf("protocolList len = %d, want %d", len(list), len(protocols))
	}
	for i := 1; i < len(list); i++ {
		if list[i-1] >= list[i] {
			t.Errorf("protocolList not sorted: %v", list)
		}
	}
	for _, p := range list {
		if !protocols[p] {
			t.Errorf("protocolList has %q not in protocols map", p)
		}
	}
}

func TestOpenAPIServed(t *testing.T) {
	rec := httptest.NewRecorder()
	(&App{}).handleOpenAPI(rec, httptest.NewRequest("GET", "/openapi.yaml", nil))
	res := rec.Result()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/yaml" {
		t.Errorf("Content-Type = %q, want application/yaml", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"openapi: 3.0", "/api/op:", "/mcp:", "bearerAuth"} {
		if !strings.Contains(body, want) {
			t.Errorf("openapi.yaml missing %q", want)
		}
	}
}
