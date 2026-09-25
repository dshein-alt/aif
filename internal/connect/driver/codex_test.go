package driver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
)

// startCodex starts the codex driver on the fake harness replaying transcript (a path).
func startCodex(t *testing.T, transcript string, l Launch) *codexDriver {
	t.Helper()
	d, err := New("codex")
	if err != nil {
		t.Fatal(err)
	}
	l.Bin = fakeHarness(t, transcript)
	if l.StateDir == "" {
		l.StateDir = t.TempDir()
	}
	if err := d.Start(context.Background(), l); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Stop(context.Background()) })
	return d.(*codexDriver)
}

// codexHello is the handshake up to thread/start's request, for a user config with no MCP servers.
const codexHello = `dynamic: version,cwd
in: {"id":1,"method":"initialize","params":{"clientInfo":{"name":"aif-connect","version":"*"}}}
out: {"id":$id,"result":{"userAgent":"aif-connect/0.155.0","codexHome":"/home/u/.codex","platformFamily":"unix","platformOs":"linux"}}
in: {"method":"initialized"}
in: {"id":2,"method":"config/read","params":{"cwd":"*"}}
out: {"id":$id,"result":{"config":{"model":"gpt-6-astra"},"origins":{}}}
`

const codexMCP = `"config":{"mcp_servers":{"aif_connect":{"url":"","bearer_token_env_var":"AIF_CONNECT_TOKEN","default_tools_approval_mode":"approve"}}}`

func TestCodexRecordedTurn(t *testing.T) {
	d := startCodex(t, "testdata/codex.jsonl", Launch{MCPURL: "http://aif:18080/mcp", MCPToken: "aif_tok", SystemPrompt: "contract"})
	if d.SessionID() != "01a0d871-4254-7931-8590-f775898d34dc" {
		t.Fatalf("thread id %q", d.SessionID())
	}
	if err := d.Prompt(context.Background(), "Call the whoami tool of the aif MCP server, then reply with the exact function name you invoked and nothing else."); err != nil {
		t.Fatal(err)
	}
	var want []Event
	for _, s := range []string{"I", "’ll", " find", " and", ".", "I", " couldn", "’t", " invoke", "."} {
		want = append(want, Event{Kind: Text, Text: s})
	}
	want = append(want, Event{Kind: TurnEnd, OK: true})
	if got := nextEvents(t, d, TurnEnd); !slices.Equal(got, want) {
		t.Fatalf("events %+v\nwant   %+v", got, want)
	}
	if err := d.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := nextEvents(t, d, Exit); len(got) != 1 || got[0].Err == "" {
		t.Fatalf("after Stop %+v, want one Exit with the signal's code", got)
	}
}

func TestCodexResume(t *testing.T) {
	d := startCodex(t, writeTranscript(t, codexHello+`in: {"id":3,"method":"thread/resume","params":{"threadId":"saved","excludeTurns":true,`+codexMCP+`}}
out: {"id":$id,"result":{"thread":{"id":"saved","turns":[]},"model":"gpt-6-astra"}}
`), Launch{SessionID: "saved"})
	if d.SessionID() != "saved" {
		t.Fatalf("thread %q", d.SessionID())
	}
}

// A resume codex rejects (shape from a live probe with an unknown id) starts a new thread.
func TestCodexResumeRejected(t *testing.T) {
	var logged []string
	var mu sync.Mutex
	d := startCodex(t, writeTranscript(t, codexHello+`in: {"id":3,"method":"thread/resume","params":{"threadId":"gone","excludeTurns":true,"developerInstructions":"c",`+codexMCP+`}}
out: {"error":{"code":-32600,"message":"no rollout found for thread id gone"},"id":$id}
in: {"id":4,"method":"thread/start","params":{"developerInstructions":"c",`+codexMCP+`}}
out: {"id":$id,"result":{"thread":{"id":"fresh"}}}
`), Launch{SessionID: "gone", SystemPrompt: "c", Log: func(s string) { mu.Lock(); logged = append(logged, s); mu.Unlock() }})
	if d.SessionID() != "fresh" {
		t.Fatalf("thread %q", d.SessionID())
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(logged, "codex: resume gone: thread/resume: no rollout found for thread id gone; starting a new thread") {
		t.Fatalf("log %q", logged)
	}
}

// Server requests are answered at once (schema-derived shapes); tool items become ToolCall; the
// three ways a turn fails.
func TestCodexApprovalsAndFailures(t *testing.T) {
	d := startCodex(t, writeTranscript(t, codexHello+`in: {"id":3,"method":"thread/start","params":{`+codexMCP+`}}
out: {"id":$id,"result":{"thread":{"id":"th"}}}
in: {"id":4,"method":"turn/start","params":{"threadId":"th","input":[{"type":"text","text":"go"}]}}
out: {"id":$id,"result":{"turn":{"id":"tu","items":[],"status":"inProgress","error":null}}}
out: {"method":"item/started","params":{"item":{"type":"commandExecution","id":"c1","command":"ls -la","commandActions":[],"cwd":"/w","status":"inProgress"},"threadId":"th","turnId":"tu","startedAtMs":1}}
out: {"id":0,"method":"item/commandExecution/requestApproval","params":{"threadId":"th","turnId":"tu","itemId":"c1","startedAtMs":1,"command":"ls -la"}}
in: {"id":0,"result":{"decision":"decline"}}
out: {"id":1,"method":"mcpServer/elicitation/request","params":{"threadId":"th","turnId":"tu","serverName":"aif_connect"}}
in: {"id":1,"result":{"action":"decline","content":null}}
out: {"id":"x","method":"item/tool/call","params":{"threadId":"th","turnId":"tu"}}
in: {"id":"x","error":{"code":-32601,"message":"aif-connect does not handle item/tool/call"}}
out: {"method":"item/started","params":{"item":{"type":"mcpToolCall","id":"m1","server":"aif_connect","tool":"whoami","arguments":{},"status":"inProgress"},"threadId":"th","turnId":"tu","startedAtMs":2}}
out: {"method":"turn/completed","params":{"threadId":"th","turn":{"id":"tu","items":[],"status":"failed","error":{"message":"Quota exceeded.","codexErrorInfo":"usageLimitExceeded"}}}}
in: {"id":5,"method":"turn/start","params":{"threadId":"th","input":[{"type":"text","text":"again"}]}}
out: {"id":$id,"result":{"turn":{"id":"tu2","items":[],"status":"inProgress","error":null}}}
out: {"method":"error","params":{"error":{"message":"stream disconnected"},"threadId":"th","turnId":"tu2","willRetry":true}}
out: {"method":"turn/completed","params":{"threadId":"th","turn":{"id":"tu2","items":[],"status":"interrupted","error":null}}}
in: {"id":6,"method":"turn/start","params":{"threadId":"th","input":[{"type":"text","text":"third"}]}}
out: {"error":{"code":-32600,"message":"thread is busy"},"id":$id}
exit: 3
`), Launch{})
	ctx := context.Background()
	if err := d.Prompt(ctx, "go"); err != nil {
		t.Fatal(err)
	}
	want := []Event{{Kind: ToolCall, Text: "ls -la"}, {Kind: ToolCall, Text: "mcp__aif_connect__whoami"}, {Kind: TurnEnd, Err: "Quota exceeded."}}
	if got := nextEvents(t, d, TurnEnd); !slices.Equal(got, want) {
		t.Fatalf("events %+v", got)
	}
	_ = d.Prompt(ctx, "again")
	if got := nextEvents(t, d, TurnEnd); !slices.Equal(got, []Event{{Kind: TurnEnd, Err: "stream disconnected"}}) {
		t.Fatalf("events %+v", got)
	}
	_ = d.Prompt(ctx, "third")
	want = []Event{{Kind: TurnEnd, Err: "codex rejected the turn: thread is busy"}, {Kind: Exit, Err: "codex exited with code 3"}}
	if got := nextEvents(t, d, Exit); !slices.Equal(got, want) {
		t.Fatalf("events %+v", got)
	}
}

func TestCodexAutoApprove(t *testing.T) {
	d := startCodex(t, writeTranscript(t, codexHello+`in: {"id":3,"method":"thread/start","params":{`+codexMCP+`}}
out: {"id":$id,"result":{"thread":{"id":"th"}}}
in: {"id":4,"method":"turn/start","params":{"threadId":"th","input":[{"type":"text","text":"go"}]}}
out: {"id":7,"method":"item/fileChange/requestApproval","params":{"threadId":"th","turnId":"tu","itemId":"f1","startedAtMs":1}}
in: {"id":7,"result":{"decision":"accept"}}
out: {"id":8,"method":"execCommandApproval","params":{}}
in: {"id":8,"result":{"decision":"approved"}}
out: {"id":9,"method":"item/permissions/requestApproval","params":{"threadId":"th","turnId":"tu","itemId":"p1","cwd":"/w","startedAtMs":1,"permissions":{"network":{"enabled":true}}}}
in: {"id":9,"result":{"permissions":{"network":{"enabled":true}}}}
out: {"method":"turn/completed","params":{"threadId":"th","turn":{"id":"tu","items":[],"status":"completed","error":null}}}
`), Launch{AutoApprove: true})
	if err := d.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if got := nextEvents(t, d, TurnEnd); !slices.Equal(got, []Event{{Kind: TurnEnd, OK: true}}) {
		t.Fatalf("events %+v", got)
	}
}

// The bearer token reaches the child's environment (the fake prints it; Verbose echoes stdout).
func TestCodexTokenEnv(t *testing.T) {
	var mu sync.Mutex
	var logged []string
	startCodex(t, writeTranscript(t, strings.Replace(codexHello, "\n", "\nenv: AIF_CONNECT_TOKEN\n", 1)+`in: {"id":3,"method":"thread/start","params":{`+codexMCP+`}}
out: {"id":$id,"result":{"thread":{"id":"th"}}}
`), Launch{MCPToken: "aif_tok", Env: []string{"AIF_CONNECT_TOKEN=wrong"}, Verbose: true,
		Log: func(s string) { mu.Lock(); logged = append(logged, s); mu.Unlock() }})
	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(logged, "<< aif_tok") || slices.Contains(logged, "<< wrong") {
		t.Fatalf("log %q", logged)
	}
}

// A codex that dies during the handshake fails Start and leaves no Exit behind.
func TestCodexHandshakeFails(t *testing.T) {
	d, _ := New("codex")
	l := Launch{Bin: fakeHarness(t, writeTranscript(t, `dynamic: version
in: {"id":1,"method":"initialize","params":{"clientInfo":{"name":"aif-connect","version":"*"}}}
exit: 2
`)), StateDir: t.TempDir()}
	err := d.Start(context.Background(), l)
	if err == nil || err.Error() != "codex app-server: initialize: codex exited with code 2" {
		t.Fatalf("err %v", err)
	}
	select {
	case e := <-d.Events():
		t.Fatalf("stray event %+v", e)
	default:
	}
}

func TestCodexArgs(t *testing.T) {
	got := codexArgs(Launch{Model: "gpt-5.5", Thinking: "high", AutoApprove: true})
	want := []string{"app-server", "-c", `model="gpt-5.5"`, "-c", `model_reasoning_effort="high"`,
		"-c", `approval_policy="never"`, "-c", `sandbox_mode="danger-full-access"`}
	if !slices.Equal(got, want) {
		t.Fatalf("args %q", got)
	}
	if got := codexArgs(Launch{}); !slices.Equal(got, []string{"app-server"}) {
		t.Fatalf("args %q", got)
	}
}

func TestCodexPreflight(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script stand-in for codex")
	}
	path := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho codex-cli 0.155.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d, _ := New("codex")
	if err := d.Preflight(path); err != nil {
		t.Fatal(err)
	}
	if err := d.Preflight(filepath.Join(t.TempDir(), "nosuch")); !errors.Is(err, ErrPrerequisite) {
		t.Fatalf("missing binary: %v", err)
	}
}
