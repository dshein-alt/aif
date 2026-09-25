package driver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
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

// codexHelloCfg is the handshake up to thread/start's request, config/read replying result.
func codexHelloCfg(result string) string {
	return `dynamic: version,cwd
in: {"id":1,"method":"initialize","params":{"clientInfo":{"name":"aif-connect","version":"*"}}}
out: {"id":$id,"result":{"userAgent":"aif-connect/0.155.0","codexHome":"/home/u/.codex","platformFamily":"unix","platformOs":"linux"}}
in: {"method":"initialized"}
in: {"id":2,"method":"config/read","params":{"cwd":"*"}}
out: {"id":$id,"result":` + result + `}
`
}

// codexHello is the handshake for a user config with no MCP servers.
var codexHello = codexHelloCfg(`{"config":{"model":"gpt-6-astra"},"origins":{}}`)

const codexMCP = `"config":{"mcp_servers":{"aif_connect":{"url":"","bearer_token_env_var":"AIF_CONNECT_TOKEN","default_tools_approval_mode":"approve"}}}`

// codexThread is codexHello plus a thread/start that yields thread "th".
var codexThread = codexHello + `in: {"id":3,"method":"thread/start","params":{` + codexMCP + `}}
out: {"id":$id,"result":{"thread":{"id":"th"}}}
`

// Also: a user MCP server codex starts anyway (the pre-fix live run's aif) is logged; Start on a
// running driver fails; Stop leaves no event behind, the Exit included.
func TestCodexRecordedTurn(t *testing.T) {
	logs := &logSink{}
	d := startCodex(t, "testdata/codex.jsonl", Launch{MCPURL: "http://aif:18080/mcp", MCPToken: "aif_tok", SystemPrompt: "contract", Log: logs.log})
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
	if !logs.has("codex: unexpected MCP server aif started — exclusivity broken") {
		t.Fatalf("log %q", logs.lines)
	}
	if err := d.Start(context.Background(), Launch{Bin: d.launch.Bin, StateDir: t.TempDir()}); err == nil {
		t.Fatal("Start on a running driver succeeded")
	}
	if err := d.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(d.Events()); n != 0 {
		t.Fatalf("%d events left after Stop", n)
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
	logs := &logSink{}
	d := startCodex(t, writeTranscript(t, codexHello+`in: {"id":3,"method":"thread/resume","params":{"threadId":"gone","excludeTurns":true,"developerInstructions":"c",`+codexMCP+`}}
out: {"error":{"code":-32600,"message":"no rollout found for thread id gone"},"id":$id}
in: {"id":4,"method":"thread/start","params":{"developerInstructions":"c",`+codexMCP+`}}
out: {"id":$id,"result":{"thread":{"id":"fresh"}}}
`), Launch{SessionID: "gone", SystemPrompt: "c", Log: logs.log})
	if d.SessionID() != "fresh" {
		t.Fatalf("thread %q", d.SessionID())
	}
	if !logs.has("codex: resume gone: thread/resume: no rollout found for thread id gone; starting a new thread") {
		t.Fatalf("log %q", logs.lines)
	}
}

// Only a "no rollout found" error reply starts a new thread: an exit, a timeout or another error
// fails Start, without the "starting a new thread" line.
func TestCodexResumeFails(t *testing.T) {
	resume := codexHello + `in: {"id":3,"method":"thread/resume","params":{"threadId":"gone","excludeTurns":true,` + codexMCP + `}}
`
	for name, tc := range map[string]struct {
		tail, err string
		timeout   time.Duration
	}{
		"exit":    {"exit: 1\n", "codex: thread/resume: codex exited with code 1", 0},
		"timeout": {"", "codex: thread/resume: context deadline exceeded", 300 * time.Millisecond},
		"other":   {`out: {"error":{"code":-32600,"message":"thread is busy"},"id":$id}` + "\n", "codex: thread/resume: thread is busy", 0},
	} {
		t.Run(name, func(t *testing.T) {
			logs := &logSink{}
			d, _ := New("codex")
			ctx := context.Background()
			if tc.timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.timeout)
				defer cancel()
			}
			err := d.Start(ctx, Launch{Bin: fakeHarness(t, writeTranscript(t, resume+tc.tail)), StateDir: t.TempDir(), SessionID: "gone", Log: logs.log})
			if err == nil || err.Error() != tc.err {
				t.Fatalf("err %v", err)
			}
			logs.mu.Lock()
			defer logs.mu.Unlock()
			for _, l := range logs.lines {
				if strings.Contains(l, "starting a new thread") {
					t.Fatalf("log %q", logs.lines)
				}
			}
			if n := len(d.Events()); n != 0 {
				t.Fatalf("%d stray events", n)
			}
		})
	}
}

// config/read fails closed: a reply we cannot read, or a user server that would merge into ours.
func TestCodexConfigFailsClosed(t *testing.T) {
	for name, tc := range map[string]struct{ cfg, err string }{
		"unreadable": {`{"config":{"mcp_servers":["aif"]}}`, "codex: config/read reply not understood: "},
		"named":      {`{"config":{"mcp_servers":{"aif_connect":{"url":"http://x/mcp"}}}}`, "codex: a user MCP server is already named aif_connect; rename it"},
	} {
		t.Run(name, func(t *testing.T) {
			d, _ := New("codex")
			err := d.Start(context.Background(), Launch{Bin: fakeHarness(t, writeTranscript(t, codexHelloCfg(tc.cfg))), StateDir: t.TempDir()})
			if err == nil || !strings.HasPrefix(err.Error(), tc.err) {
				t.Fatalf("err %v", err)
			}
			if n := len(d.Events()); n != 0 {
				t.Fatalf("%d stray events", n)
			}
		})
	}
}

// Notifications of another thread (a sub-agent's) are skipped, its server requests still answered;
// a turn/completed with no prompt pending is logged, not emitted; aif_connect's startup failure is
// logged; a non-JSON line is skipped, a line with a field of an unexpected type is not.
func TestCodexThreadFilter(t *testing.T) {
	logs := &logSink{}
	d := startCodex(t, writeTranscript(t, codexThread+`in: {"id":4,"method":"turn/start","params":{"threadId":"th","input":[{"type":"text","text":"go"}]}}
out: {"id":$id,"result":{"turn":{"id":"tu","items":[],"status":"inProgress","error":null}}}
out: {"method":"mcpServer/startupStatus/updated","params":{"threadId":"th","name":"aif_connect","status":"failed","error":"HTTP 403","failureReason":null}}
out: not json
out: {"method":"item/agentMessage/delta","params":{"threadId":"sub","turnId":"s1","itemId":"i","delta":"sub"}}
out: {"method":"item/started","params":{"item":{"type":"commandExecution","id":"c1","command":"rm -rf x"},"threadId":"sub","turnId":"s1","startedAtMs":1}}
out: {"id":5,"method":"item/commandExecution/requestApproval","params":{"threadId":"sub","turnId":"s1","itemId":"c1","startedAtMs":1,"command":"rm -rf x"}}
in: {"id":5,"result":{"decision":"decline"}}
out: {"method":"turn/completed","params":{"threadId":"sub","turn":{"id":"s1","items":[],"status":"completed","error":null}}}
out: {"method":"item/agentMessage/delta","params":{"threadId":"th","turnId":"tu","itemId":"i","delta":"mine"},"error":"stray"}
out: {"method":"turn/completed","params":{"threadId":"th","turn":{"id":"tu","items":[],"status":"completed","error":null}}}
out: {"method":"turn/completed","params":{"threadId":"th","turn":{"id":"tu","items":[],"status":"completed","error":null}}}
exit: 0
`), Launch{Log: logs.log})
	if err := d.Prompt(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	want := []Event{{Kind: Text, Text: "mine"}, {Kind: TurnEnd, OK: true}, {Kind: Exit}}
	if got := nextEvents(t, d, Exit); !slices.Equal(got, want) {
		t.Fatalf("events %+v", got)
	}
	for _, l := range []string{"codex: MCP server aif_connect failed to start: HTTP 403", "codex: turn/completed with no prompt pending, ignored"} {
		if !logs.has(l) {
			t.Fatalf("no %q in log %q", l, logs.lines)
		}
	}
}

// Server requests are answered at once (schema-derived shapes); tool items become ToolCall; the
// three ways a turn fails.
func TestCodexApprovalsAndFailures(t *testing.T) {
	d := startCodex(t, writeTranscript(t, codexThread+`in: {"id":4,"method":"turn/start","params":{"threadId":"th","input":[{"type":"text","text":"go"}]}}
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

// Also: a v1 approval request gets -32601 (a v2 thread never sends one).
func TestCodexAutoApprove(t *testing.T) {
	d := startCodex(t, writeTranscript(t, codexThread+`in: {"id":4,"method":"turn/start","params":{"threadId":"th","input":[{"type":"text","text":"go"}]}}
out: {"id":7,"method":"item/fileChange/requestApproval","params":{"threadId":"th","turnId":"tu","itemId":"f1","startedAtMs":1}}
in: {"id":7,"result":{"decision":"accept"}}
out: {"id":8,"method":"execCommandApproval","params":{}}
in: {"id":8,"error":{"code":-32601,"message":"aif-connect does not handle execCommandApproval"}}
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
	logs := &logSink{}
	startCodex(t, writeTranscript(t, strings.Replace(codexThread, "\n", "\nenv: AIF_CONNECT_TOKEN\n", 1)),
		Launch{MCPToken: "aif_tok", Env: []string{"AIF_CONNECT_TOKEN=wrong"}, Verbose: true, Log: logs.log})
	if !logs.has("<< aif_tok") || logs.has("<< wrong") {
		t.Fatalf("log %q", logs.lines)
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
	if err == nil || err.Error() != "codex: initialize: codex exited with code 2" {
		t.Fatalf("err %v", err)
	}
	select {
	case e := <-d.Events():
		t.Fatalf("stray event %+v", e)
	default:
	}
}

// Stop discards the old process's events, queued or not, so a restart starts clean.
func TestCodexRestart(t *testing.T) {
	ctx := context.Background()
	d := startCodex(t, writeTranscript(t, codexThread+`in: {"id":4,"method":"turn/start","params":{"threadId":"th","input":[{"type":"text","text":"hi"}]}}
out: {"method":"item/agentMessage/delta","params":{"threadId":"th","turnId":"tu","itemId":"i","delta":"old"}}
out: {"method":"turn/completed","params":{"threadId":"th","turn":{"id":"tu","items":[],"status":"completed","error":null}}}
`), Launch{})
	if err := d.Prompt(ctx, "hi"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); len(d.Events()) < 2; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the old turn's events never queued")
		}
	}
	if err := d.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	bin := fakeHarness(t, writeTranscript(t, codexHello+`in: {"id":3,"method":"thread/start","params":{`+codexMCP+`}}
out: {"id":$id,"result":{"thread":{"id":"th2"}}}
in: {"id":4,"method":"turn/start","params":{"threadId":"th2","input":[{"type":"text","text":"again"}]}}
out: {"method":"item/agentMessage/delta","params":{"threadId":"th2","turnId":"tu","itemId":"i","delta":"new"}}
out: {"method":"turn/completed","params":{"threadId":"th2","turn":{"id":"tu","items":[],"status":"completed","error":null}}}
`))
	if err := d.Start(ctx, Launch{Bin: bin, StateDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if err := d.Prompt(ctx, "again"); err != nil {
		t.Fatal(err)
	}
	want := []Event{{Kind: Text, Text: "new"}, {Kind: TurnEnd, OK: true}}
	if got := nextEvents(t, d, TurnEnd); !slices.Equal(got, want) {
		t.Fatalf("events %+v", got)
	}
}

// A Stop whose ctx ends first kills the harness and returns ctx.Err(); the Exit still comes, with
// no Err: a deliberate stop is not a failure.
func TestCodexStopCtxEnds(t *testing.T) {
	d := startCodex(t, writeTranscript(t, codexThread+"ignore-sigterm:\n"), Launch{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d.Stop(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stop: %v", err)
	}
	if got := nextEvents(t, d, Exit); !slices.Equal(got, []Event{{Kind: Exit}}) {
		t.Fatalf("events %+v", got)
	}
}

func TestCodexKilledBySignal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no signals")
	}
	d := startCodex(t, writeTranscript(t, codexThread), Launch{})
	_ = d.cmd.Process.Kill()
	if got := nextEvents(t, d, Exit); !slices.Equal(got, []Event{{Kind: Exit, Err: "codex killed by signal"}}) {
		t.Fatalf("events %+v", got)
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
