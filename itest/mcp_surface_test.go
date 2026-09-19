package itest

import (
	"strings"
	"testing"

	"github.com/dshein-alt/aif/internal/harness"
)

func rpcResult(t *testing.T, resp *harness.Resp) map[string]any {
	t.Helper()
	resp.MustOK()
	j := resp.JSON()
	res, ok := j["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in %s", resp.Text())
	}
	return res
}

func TestMCPSurface(t *testing.T) {
	r := harness.New(t, false)
	alice := r.Join("alice")
	call := func(body map[string]any) *harness.Resp { return r.Admin.Post("/mcp", body) }

	// --- initialize + JSON-RPC ping ---
	init := rpcResult(t, call(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"}))
	if _, ok := init["protocolVersion"].(string); !ok || init["serverInfo"] == nil {
		t.Errorf("initialize result = %v", init)
	}
	rpcResult(t, call(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "ping"}))

	// --- tools/list ---
	tools := rpcResult(t, call(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/list"}))
	tl, _ := tools["tools"].([]any)
	if len(tl) < 10 {
		t.Fatalf("tools/list returned %d tools", len(tl))
	}
	names := map[string]bool{}
	for _, it := range tl {
		names[it.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"post", "who", "threads", "vote", "batch"} {
		if !names[want] {
			t.Errorf("tools/list missing %q", want)
		}
	}

	// --- tools/call a read-only tool (structuredContent + content text) ---
	tc := rpcResult(t, call(map[string]any{"jsonrpc": "2.0", "id": 4, "method": "tools/call",
		"params": map[string]any{"name": "who", "arguments": map[string]any{}}}))
	if tc["isError"] != false || tc["structuredContent"] == nil {
		t.Errorf("tools/call who = %v", tc)
	}
	if content, _ := tc["content"].([]any); len(content) == 0 {
		t.Error("tools/call who returned no content block")
	}

	// --- tools/call a WRITE tool, identity pulled from the agent/me/as arg ---
	wr := rpcResult(t, call(map[string]any{"jsonrpc": "2.0", "id": 5, "method": "tools/call",
		"params": map[string]any{"name": "post", "arguments": map[string]any{"me": "alice", "subject": "Via MCP", "b": "hello mcp"}}}))
	if wr["isError"] != false || wr["structuredContent"] == nil {
		t.Errorf("tools/call post = %v", wr)
	}
	tid := wr["structuredContent"].(map[string]any)["t"]
	// the agent identity was honoured: alice can see it as her own thread
	if sub := alice.Op("sub", map[string]any{"t": tid}); sub.Code != 200 {
		t.Errorf("sub to MCP-created thread failed: %s", sub.Text())
	}

	// --- resources/list + resources/read ---
	rl := rpcResult(t, call(map[string]any{"jsonrpc": "2.0", "id": 6, "method": "resources/list"}))
	if res, _ := rl["resources"].([]any); len(res) != 2 {
		t.Errorf("resources/list = %v, want 2", rl)
	}
	read := rpcResult(t, call(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "resources/read",
		"params": map[string]any{"uri": "aif://skill"}}))
	if contents, _ := read["contents"].([]any); len(contents) == 0 ||
		!strings.Contains(contents[0].(map[string]any)["text"].(string), "Authorization: Bearer") {
		t.Errorf("resources/read skill = %v", read)
	}
	lim := rpcResult(t, call(map[string]any{"jsonrpc": "2.0", "id": 8, "method": "resources/read",
		"params": map[string]any{"uri": "aif://limits"}}))
	if contents, _ := lim["contents"].([]any); len(contents) == 0 ||
		!strings.Contains(contents[0].(map[string]any)["text"].(string), "max_file_bytes") {
		t.Errorf("resources/read limits = %v", lim)
	}

	// --- prompts/list + prompts/get ---
	pl := rpcResult(t, call(map[string]any{"jsonrpc": "2.0", "id": 9, "method": "prompts/list"}))
	if pr, _ := pl["prompts"].([]any); len(pr) < 1 {
		t.Errorf("prompts/list = %v, want >=1", pl)
	}
	pg := rpcResult(t, call(map[string]any{"jsonrpc": "2.0", "id": 10, "method": "prompts/get",
		"params": map[string]any{"name": "aif-agent", "arguments": map[string]any{"name": "alice"}}}))
	if msgs, _ := pg["messages"].([]any); len(msgs) == 0 {
		t.Errorf("prompts/get aif-agent = %v", pg)
	}

	// --- JSON-RPC error mapping ---
	if e := call(map[string]any{"jsonrpc": "2.0", "id": 11, "method": "nope"}); e.JSON()["error"] == nil ||
		e.JSON()["error"].(map[string]any)["code"].(float64) != -32601 {
		t.Errorf("unknown method = %s", e.Text())
	}
	// an unknown tool comes back as an errored tool result (MCP convention), not a JSON-RPC error
	if e := call(map[string]any{"jsonrpc": "2.0", "id": 12, "method": "tools/call",
		"params": map[string]any{"name": "ghost"}}); rpcResult(t, e)["isError"] != true {
		t.Errorf("unknown tool should be an errored result: %s", e.Text())
	}
	// an unknown resource is a JSON-RPC error (-32002)
	if e := call(map[string]any{"jsonrpc": "2.0", "id": 13, "method": "resources/read",
		"params": map[string]any{"uri": "aif://nope"}}); e.JSON()["error"].(map[string]any)["code"].(float64) != -32002 {
		t.Errorf("unknown resource = %s, want -32002", e.Text())
	}

	// --- GET /mcp is refused (request/response only) ---
	if g := r.Admin.Get("/mcp"); g.Code != 405 || g.Str("err") != "no_stream" {
		t.Errorf("GET /mcp = %d %s, want 405 no_stream", g.Code, g.Text())
	}
}
