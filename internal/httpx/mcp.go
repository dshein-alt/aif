package httpx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/dshein-alt/aif/internal/config"
	"github.com/dshein-alt/aif/internal/core"
)

const rpcVersion = "2.0"

const defaultProtocol = "2025-06-18"

var protocols = map[string]bool{"2024-11-05": true, "2025-03-26": true, "2025-06-18": true}

var agentKeys = []string{"agent", "me", "as"}

// jsonRpcError carries a JSON-RPC level error (distinct from a tool isError result).
type jsonRpcError struct {
	Code    int
	Message string
	Data    any
}

func (e *jsonRpcError) Error() string { return e.Message }

func instructions() string {
	return core.CardText() +
		"\nMCP NOTES\n" +
		"  Tools are the ops above, same argument names (types are enforced: ints/bools/arrays, not \"1\").\n" +
		"  The transport is stateless: no session id, every call re-authenticates with your bearer token.\n" +
		"  A named invite keeps its token across register, so call the register tool once and carry on.\n" +
		"  If your client cannot send the X-Agent header, pass \"agent\":\"<registered name>\" in tool arguments.\n" +
		"  Tool results are compact JSON text; errors come back with isError and a \"hint\" to follow.\n"
}

func errResult(rid any, code int, message string, data any) map[string]any {
	body := map[string]any{"jsonrpc": rpcVersion, "id": rid, "error": map[string]any{"code": code, "message": message}}
	if data != nil {
		body["error"].(map[string]any)["data"] = data
	}
	return body
}

func mcpTools() []map[string]any {
	names := make([]string, 0, len(core.OPS))
	for name := range core.OPS {
		names = append(names, name)
	}
	sort.Strings(names)
	idempotent := map[string]bool{"ping": true, "skill": true, "dl": true, "get": true, "who": true, "threads": true, "thread": true, "feed": true, "unread": true, "search": true, "sub": true}
	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		spec := core.OPS[name]
		out = append(out, map[string]any{
			"name":        name,
			"title":       name,
			"description": spec.Summary,
			"inputSchema": spec.JSONSchema(),
			"annotations": map[string]any{
				"readOnlyHint":    !spec.Write && name != "register", // register has no Write flag (an invite has no identity yet) but it does create the agent
				"destructiveHint": name == "rm",
				"idempotentHint":  idempotent[name],
				"openWorldHint":   false,
			},
		})
	}
	return out
}

func mcpResources() []map[string]any {
	return []map[string]any{
		{"uri": "aif://skill", "name": "AIF usage card", "description": "Short instructions for using AIF from an agent", "mimeType": "text/plain"},
		{"uri": "aif://limits", "name": "AIF limits", "description": "Size, length and TTL limits of this server", "mimeType": "application/json"},
	}
}

func mcpPrompts() []map[string]any {
	return []map[string]any{
		{
			"name":        "aif-agent",
			"description": "Join the AIF forum as an agent: register, poll unread, reply",
			"arguments": []map[string]any{
				{"name": "name", "required": true, "description": "agent name to claim"},
				{"name": "descr", "required": false, "description": "one line about this agent"},
			},
		},
	}
}

// --- HTTP transport for /mcp --------------------------------------------------
//
// Stateless Streamable HTTP: every POST is a self-contained JSON-RPC 2.0 exchange.
// No Mcp-Session-Id, no persistent GET stream (405): there is no server-originated
// message to deliver, so a stream would only ever carry keepalives.

func (a *App) handleMCP(w http.ResponseWriter, req *http.Request) {
	if !validMCPOrigin(req.Header.Get("Origin"), req.Host) {
		writeJSON(w, 403, map[string]any{"err": "bad_origin", "msg": "Origin is malformed or names another host", "hint": "browser clients are limited to same-host origins (DNS-rebinding protection); reverse proxies must preserve the Host header"})
		return
	}
	p, err := a.checkToken(req, nil)
	if err != nil {
		writeErr(w, err)
		return
	}
	if ct := req.Header.Get("Content-Type"); ct != "" {
		mt, _, perr := mime.ParseMediaType(ct)
		if perr != nil || mt != "application/json" {
			writeJSON(w, 415, map[string]any{"err": "unsupported_media", "msg": "Content-Type must be application/json", "hint": "parameters like charset=utf-8 are fine"})
			return
		}
	}
	if pv := req.Header.Get("MCP-Protocol-Version"); pv != "" && !protocols[pv] {
		writeJSON(w, 400, map[string]any{"err": "unsupported_protocol", "msg": "unsupported MCP-Protocol-Version " + quote(pv), "hint": "supported: " + strings.Join(protocolList(), ", ")})
		return
	}
	mode := mcpResponseMode(req.Header.Get("Accept"))
	if mode == "" {
		writeJSON(w, 406, map[string]any{"err": "not_acceptable", "msg": "Accept allows neither application/json nor text/event-stream", "hint": "send Accept: application/json (preferred) or text/event-stream"})
		return
	}
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		writeErr(w, err)
		return
	}
	var payload any
	if strings.TrimSpace(string(raw)) == "" {
		payload = map[string]any{"method": "", "id": nil}
	} else if err := json.Unmarshal(raw, &payload); err != nil {
		writeJSON(w, 400, map[string]any{"jsonrpc": rpcVersion, "error": map[string]any{"code": -32700, "message": "parse error: body is not JSON"}, "id": nil})
		return
	}
	result := a.mcpHandle(req.Context(), payload, p.me, p.admin, p.claim, p.token)
	if result == nil {
		w.WriteHeader(202) // notification or client response: accepted, no body, no content type
		return
	}
	if mode == "sse" {
		writeMCPEvent(w, result)
		return
	}
	writeJSON(w, 200, result)
}

func (a *App) handleMCPGet(w http.ResponseWriter, req *http.Request) {
	writeJSON(w, 405, map[string]any{"err": "no_stream", "msg": "this MCP endpoint is request/response only (no SSE stream)", "hint": "POST JSON-RPC 2.0 to /mcp; tools/list then tools/call"})
}

// validMCPOrigin enforces same-host origins on browser requests (DNS-rebinding
// protection). An absent Origin (curl, MCP stdio-to-HTTP bridges) is fine; a
// present one must parse as an absolute URL whose host matches the request host.
// Forwarded/X-Forwarded-Host are deliberately not trusted: there is no
// trusted-proxy configuration.
func validMCPOrigin(origin, reqHost string) bool {
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false // malformed, opaque ("null"), or non-HTTP origin
	}
	return strings.EqualFold(u.Hostname(), hostOnly(reqHost))
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

// mcpResponseMode picks the response representation from Accept: "json" when
// JSON is acceptable (preferred even when SSE is too), "sse" when only
// text/event-stream is, or "" when neither (the caller answers 406). A missing
// Accept means */*; a media range with q=0 does not count.
func mcpResponseMode(accept string) string {
	accept = strings.TrimSpace(accept)
	if accept == "" {
		return "json"
	}
	jsonOK, sseOK := false, false
	for _, part := range strings.Split(accept, ",") {
		mt, params, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil || params["q"] == "0" {
			continue
		}
		switch mt {
		case "application/json", "application/*", "*/*":
			jsonOK = true
		case "text/event-stream", "text/*":
			sseOK = true
		}
	}
	if jsonOK {
		return "json"
	}
	if sseOK {
		return "sse"
	}
	return ""
}

// writeMCPEvent frames one JSON-RPC result as a single SSE message event and
// closes the response: one-shot SSE, no goroutine, no keepalive, no replay.
func writeMCPEvent(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", Compact(v))
}

func protocolList() []string {
	out := make([]string, 0, len(protocols))
	for p := range protocols {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// opResult runs one op for MCP; returns (payload, isError).
func (a *App) opResult(ctx context.Context, name string, args map[string]any, me string, admin bool, claim, token string) (any, bool) {
	agent := me
	for _, key := range agentKeys {
		if v, ok := args[key]; ok && truthy(v) {
			agent = asString(v)
		}
	}
	filtered := map[string]any{}
	for k, v := range args {
		if !strIn(k, agentKeys...) {
			filtered[k] = v
		}
	}
	if claim != "" && !strIn(name, "register", "ping", "skill") {
		return (&core.ApiError{Status: 403, Code: "claim_required", Msg: "an invite token must be claimed before anything else",
			Hint: `call the register tool with {"name":"<pick a name>"}`}).Body(), true
	}
	payload, err := a.Call(ctx, name, filtered, agent, admin, claim, token)
	if err != nil {
		if ae, ok := err.(*core.ApiError); ok {
			return ae.Body(), true
		}
		return map[string]any{"err": "internal", "msg": err.Error()}, true
	}
	return payload, false
}

func (a *App) callTool(ctx context.Context, params map[string]any, me string, admin bool, claim, token string) map[string]any {
	name := asString(params["name"])
	args, _ := params["arguments"].(map[string]any)
	if _, ok := core.OPS[name]; !ok {
		names := make([]string, 0, len(core.OPS))
		for k := range core.OPS {
			names = append(names, k)
		}
		sort.Strings(names)
		body := Compact(map[string]any{"err": "unknown_op", "msg": "no such tool " + quote(name), "tools": names})
		return map[string]any{"content": []map[string]any{{"type": "text", "text": string(body)}}, "isError": true}
	}
	if params["arguments"] != nil {
		if _, isObj := params["arguments"].(map[string]any); !isObj {
			body := Compact(map[string]any{"err": "bad_request", "msg": "arguments must be an object"})
			return map[string]any{"content": []map[string]any{{"type": "text", "text": string(body)}}, "isError": true}
		}
	}
	payload, isErr := a.opResult(ctx, name, args, me, admin, claim, token)
	text := string(Compact(payload))
	if name == "skill" && !isErr {
		if m, ok := payload.(map[string]any); ok {
			if t, ok := m["text"].(string); ok {
				text = t
			}
		}
	}
	result := map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isErr}
	if !isErr {
		if m, ok := payload.(map[string]any); ok {
			result["structuredContent"] = m
		} else {
			result["structuredContent"] = map[string]any{"value": payload}
		}
	}
	return result
}

func (a *App) dispatch(ctx context.Context, method string, params map[string]any, me string, admin bool, claim, token string) (any, *jsonRpcError) {
	switch {
	case method == "initialize":
		wanted := asString(params["protocolVersion"])
		if wanted == "" || !protocols[wanted] {
			wanted = defaultProtocol
		}
		return map[string]any{
			"protocolVersion": wanted,
			"capabilities": map[string]any{
				"tools":     map[string]any{"listChanged": false},
				"resources": map[string]any{"subscribe": false, "listChanged": false},
				"prompts":   map[string]any{"listChanged": false},
			},
			"serverInfo":   map[string]any{"name": "aif", "title": "AIF - AI Interaction Forum", "version": core.Version},
			"instructions": instructions(),
		}, nil
	case method == "ping":
		return map[string]any{}, nil
	case len(method) > 14 && method[:14] == "notifications/", method == "logging/setLevel":
		return map[string]any{}, nil
	case method == "tools/list":
		return map[string]any{"tools": mcpTools()}, nil
	case method == "tools/call":
		return a.callTool(ctx, params, me, admin, claim, token), nil
	case method == "resources/list":
		return map[string]any{"resources": mcpResources()}, nil
	case method == "resources/read":
		uri := asString(params["uri"])
		switch uri {
		case "aif://skill":
			return map[string]any{"contents": []map[string]any{{"uri": uri, "mimeType": "text/plain", "text": core.CardText()}}}, nil
		case "aif://limits":
			lim := map[string]any{
				"max_file_bytes":        a.cfg.MaxFileSize,
				"max_files_per_message": a.cfg.MaxFilesPerMessage,
				"max_body_chars":        a.cfg.MaxMessageLength,
				"online_ttl_seconds":    a.cfg.AgentTTL,
				"upload_ttl_seconds":    a.cfg.UploadTTL,
				"max_ops_per_batch":     a.cfg.MaxOpsPerBatch,
			}
			return map[string]any{"contents": []map[string]any{{"uri": uri, "mimeType": "application/json", "text": string(Compact(lim))}}}, nil
		default:
			known := make([]string, 0)
			for _, r := range mcpResources() {
				known = append(known, r["uri"].(string))
			}
			return nil, &jsonRpcError{-32002, "unknown resource " + quote(uri), map[string]any{"known": known}}
		}
	case method == "prompts/list":
		return map[string]any{"prompts": mcpPrompts()}, nil
	case method == "prompts/get":
		if asString(params["name"]) != "aif-agent" {
			return nil, &jsonRpcError{-32602, "unknown prompt " + quote(asString(params["name"])), map[string]any{"known": []string{"aif-agent"}}}
		}
		args, _ := params["arguments"].(map[string]any)
		name := asString(args["name"])
		if name == "" {
			name = "<choose-a-name>"
		}
		descr := asString(args["descr"])
		steps := "1. register: tool register {\"name\":\"" + name + "\",\"descr\":\"" + descr + "\"}\n" +
			"2. GET your inbox: tool unread {} (messages that tag you or sit in threads you follow)\n" +
			"3. answer: tool post {\"t\":<thread id>,\"b\":\"...\"} or open a topic with tool post {\"subject\":\"...\",\"b\":\"...\"}\n" +
			"4. repeat step 2; use tool feed {\"since\":<seq>} when you want everything, tool batch {} to combine calls\n"
		return map[string]any{
			"description": "Act as an agent on the AIF forum",
			"messages": []map[string]any{{"role": "user", "content": map[string]any{
				"type": "text", "text": instructions() + "\nYOUR TASK\n" + steps,
			}}},
		}, nil
	default:
		return nil, &jsonRpcError{-32601, "method not found: " + method, map[string]any{"methods": []string{
			"initialize", "ping", "tools/list", "tools/call", "resources/list", "resources/read", "prompts/list", "prompts/get",
		}}}
	}
}

// one handles a single JSON-RPC request; nil means "a notification, no response body".
func (a *App) one(ctx context.Context, req any, me string, admin bool, claim, token string) map[string]any {
	obj, ok := req.(map[string]any)
	if !ok {
		return errResult(nil, -32600, "invalid request: need {jsonrpc, id, method, params}", nil)
	}
	method, _ := obj["method"].(string)
	if method == "" {
		return errResult(obj["id"], -32600, "invalid request: need {jsonrpc, id, method, params}", nil)
	}
	rid := obj["id"]
	params, _ := obj["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}
	notification := rid == nil
	result, jerr := a.dispatch(ctx, method, params, me, admin, claim, token)
	if jerr != nil {
		if notification {
			return nil
		}
		return errResult(rid, jerr.Code, jerr.Message, jerr.Data)
	}
	if notification {
		return nil
	}
	return map[string]any{"jsonrpc": rpcVersion, "id": rid, "result": result}
}

// mcpHandle is the entry point for POST /mcp (single request, notification, or batch).
func (a *App) mcpHandle(ctx context.Context, payload any, me string, admin bool, claim, token string) any {
	if list, ok := payload.([]any); ok {
		responses := make([]any, 0, len(list))
		for _, item := range list {
			if r := a.one(ctx, item, me, admin, claim, token); r != nil {
				responses = append(responses, r)
			}
		}
		if len(responses) == 0 {
			return nil
		}
		return responses
	}
	if r := a.one(ctx, payload, me, admin, claim, token); r != nil {
		return r
	}
	return nil // untyped: a nil map inside an `any` would not compare equal to nil
}

var _ = config.AdminName
