package driver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// startClaude starts the claude driver on bin (the fake harness when empty, replaying transcript).
func startClaude(t *testing.T, transcript string, l Launch) (*claudeDriver, error) {
	t.Helper()
	d, err := New("claude")
	if err != nil {
		t.Fatal(err)
	}
	if l.Bin == "" {
		l.Bin = fakeHarness(t, transcript)
	}
	if l.StateDir == "" {
		l.StateDir = t.TempDir()
	}
	err = d.Start(context.Background(), l)
	t.Cleanup(func() { _ = d.Stop(context.Background()) })
	return d.(*claudeDriver), err
}

// claudeScript writes a /bin/sh stand-in for claude; @FAKE@ in body names the replay fake.
func claudeScript(t *testing.T, transcript, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script stand-in for claude")
	}
	bin := filepath.Join(t.TempDir(), "claude")
	body = strings.ReplaceAll(body, "@FAKE@", fakeHarness(t, transcript))
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// logged reports whether a log line contains s.
func logged(logs *logSink, s string) bool {
	logs.mu.Lock()
	defer logs.mu.Unlock()
	return slices.ContainsFunc(logs.lines, func(l string) bool { return strings.Contains(l, s) })
}

// noEvent fails if an event is waiting.
func noEvent(t *testing.T, d Driver) {
	t.Helper()
	select {
	case e := <-d.Events():
		t.Fatalf("unexpected event %+v", e)
	default:
	}
}

const claudeHandshake = `in: {"type":"control_request","request_id":"init","request":{"subtype":"initialize"}}
out: {"type":"control_response","response":{"subtype":"success","request_id":"init","response":{}}}
`

const claudeHi = `in: {"type":"user","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}
`

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestClaudeRecordedTurn(t *testing.T) {
	stateDir := t.TempDir()
	logs := &logSink{}
	d, err := startClaude(t, "testdata/claude.jsonl", Launch{StateDir: stateDir, MCPURL: "http://aif:18080/mcp", MCPToken: "aif_tok", SystemPrompt: "contract", Log: logs.log})
	if err != nil {
		t.Fatal(err)
	}

	var mcp, want any
	b, _ := os.ReadFile(filepath.Join(stateDir, "mcp.json"))
	if err := json.Unmarshal(b, &mcp); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal([]byte(`{"mcpServers":{"aif":{"type":"http","url":"http://aif:18080/mcp","headers":{"Authorization":"Bearer aif_tok"}}}}`), &want)
	if !reflect.DeepEqual(mcp, want) {
		t.Fatalf("mcp.json %s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(stateDir, "system.md")); string(b) != "contract" {
		t.Fatalf("system.md %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(stateDir, "settings.json")); string(b) != `{"disableAllHooks":true}` {
		t.Fatalf("settings.json %q", b)
	}
	if !uuidRE.MatchString(d.SessionID()) {
		t.Fatalf("generated session id %q", d.SessionID())
	}

	if err := d.Prompt(context.Background(), "Call the whoami tool once, then reply with the single word OK."); err != nil {
		t.Fatal(err)
	}
	wantEv := []Event{
		{Kind: ToolCall, Text: "ToolSearch"}, // MCP tools are deferred: the model loads the schema first
		{Kind: ToolCall, Text: "mcp__aif__whoami"},
		{Kind: Text, Text: "OK"},
		{Kind: TurnEnd, OK: true},
	}
	if got := nextEvents(t, d, TurnEnd); !slices.Equal(got, wantEv) {
		t.Fatalf("events %+v\nwant   %+v", got, wantEv)
	}
	if got := d.SessionID(); got != "0d9d1c1e-7b3a-4f5e-9a51-2b6c3d4e5f60" {
		t.Fatalf("session id not captured from init/result: %q", got)
	}

	// Stop sends the interrupt, closes stdin (the transcript checks both) and drains the Exit.
	if err := d.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	noEvent(t, d)
	if logged(logs, "MISMATCH") || logged(logs, "aif MCP server") {
		t.Fatalf("log %q", logs.lines)
	}
	for _, f := range []string{"mcp.json", "system.md", "settings.json"} {
		if _, err := os.Stat(filepath.Join(stateDir, f)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s not removed: %v", f, err)
		}
	}
}

// Stop mid-turn (shapes from a live 2.1.280 run): the interrupt goes in before stdin closes, and
// everything the process says after it, its TurnEnd and Exit included, is discarded.
func TestClaudeStopMidTurn(t *testing.T) {
	logs := &logSink{}
	d, err := startClaude(t, writeTranscript(t, claudeHandshake+claudeHi+`out: {"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash"}]}}
in: {"type":"control_request","request_id":"stop","request":{"subtype":"interrupt"}}
out: {"type":"control_response","response":{"subtype":"success","request_id":"stop","response":{"still_queued":[]}}}
out: {"type":"user","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"}]}}
out: {"type":"result","subtype":"error_during_execution","is_error":true,"errors":["[ede_diagnostic] result_type=user last_content_type=n/a stop_reason=tool_use"],"terminal_reason":"aborted_streaming"}
in: <EOF>
out: {"type":"system","subtype":"fake_saw_eof"}
exit: 1
`), Launch{Verbose: true, Log: logs.log})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Prompt(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if got := nextEvents(t, d, ToolCall); !slices.Equal(got, []Event{{Kind: ToolCall, Text: "Bash"}}) {
		t.Fatalf("events %+v", got)
	}
	if err := d.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	noEvent(t, d)
	if !logs.has(`<< {"type":"system","subtype":"fake_saw_eof"}`) || logged(logs, "MISMATCH") {
		t.Fatalf("log %q", logs.lines)
	}
}

// An error result ends the turn with its text; a request from claude is refused, not left hanging.
func TestClaudeErrorResult(t *testing.T) {
	d, err := startClaude(t, writeTranscript(t, claudeHandshake+claudeHi+`out: {"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{}}}
in: {"type":"control_response","response":{"subtype":"error","request_id":"r1","error":"aif-connect answers no requests"}}
out: {"type":"result","subtype":"success","is_error":true,"result":"API Error: 529 overloaded","session_id":"s1"}
in: {"type":"user","message":{"role":"user","content":[{"type":"text","text":"again"}]}}
out: {"type":"result","subtype":"error_max_turns","is_error":true,"session_id":"s1"}
exit: 1
`), Launch{SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	var got []Event
	for _, p := range []string{"hi", "again"} {
		if err := d.Prompt(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		got = append(got, nextEvents(t, d, TurnEnd)...)
	}
	got = append(got, nextEvents(t, d, Exit)...)
	want := []Event{
		{Kind: TurnEnd, Err: "API Error: 529 overloaded"},
		{Kind: TurnEnd, Err: "error_max_turns"},
		{Kind: Exit, Err: "claude exited with code 1"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("events %+v", got)
	}
}

// Every block of one assistant message comes out, in order; a field of an unexpected type fails
// only that field, never the result.
func TestClaudeMessageShapes(t *testing.T) {
	d, err := startClaude(t, writeTranscript(t, claudeHandshake+claudeHi+`out: {"type":"assistant","message":{"content":[{"type":"text","text":"a"},{"type":"tool_use","name":"mcp__aif__whoami"},{"type":"thinking","thinking":""},{"type":"text","text":"b"}]}}
out: {"type":"result","subtype":"success","is_error":false,"session_id":42,"result":"b"}
`), Launch{})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Prompt(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	want := []Event{{Kind: Text, Text: "a"}, {Kind: ToolCall, Text: "mcp__aif__whoami"}, {Kind: Text, Text: "b"}, {Kind: TurnEnd, OK: true}}
	if got := nextEvents(t, d, TurnEnd); !slices.Equal(got, want) {
		t.Fatalf("events %+v", got)
	}
}

// A result with no prompt pending (a background task, a resumed unfinished turn) is logged, not
// a TurnEnd.
func TestClaudeResultWithoutPrompt(t *testing.T) {
	logs := &logSink{}
	d, err := startClaude(t, writeTranscript(t, claudeHandshake+claudeHi+`out: {"type":"result","subtype":"success","is_error":false,"result":"OK"}
out: {"type":"result","subtype":"success","is_error":false,"result":"background task done"}
exit: 0
`), Launch{Log: logs.log})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Prompt(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if got := nextEvents(t, d, Exit); !slices.Equal(got, []Event{{Kind: TurnEnd, OK: true}, {Kind: Exit}}) {
		t.Fatalf("events %+v", got)
	}
	if !logs.has("claude: result with no prompt pending, ignored") {
		t.Fatalf("log %q", logs.lines)
	}
}

// Death mid-turn: the Exit ends the turn, no TurnEnd.
func TestClaudeDiesMidTurn(t *testing.T) {
	d, err := startClaude(t, writeTranscript(t, claudeHandshake+claudeHi+`out: {"type":"assistant","message":{"content":[{"type":"text","text":"partial"}]}}
exit: 3
`), Launch{})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Prompt(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	want := []Event{{Kind: Text, Text: "partial"}, {Kind: Exit, Err: "claude exited with code 3"}}
	if got := nextEvents(t, d, Exit); !slices.Equal(got, want) {
		t.Fatalf("events %+v", got)
	}
}

func TestClaudeKilledBySignal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no signals")
	}
	d, err := startClaude(t, writeTranscript(t, claudeHandshake), Launch{})
	if err != nil {
		t.Fatal(err)
	}
	_ = d.cmd.Process.Kill()
	if got := nextEvents(t, d, Exit); !slices.Equal(got, []Event{{Kind: Exit, Err: "claude killed by signal"}}) {
		t.Fatalf("events %+v", got)
	}
}

// A Stop whose ctx ends first kills the harness and returns ctx.Err(); the Exit still comes, with
// no Err: a deliberate stop is not a failure.
func TestClaudeStopCtxEnds(t *testing.T) {
	d, err := startClaude(t, writeTranscript(t, claudeHandshake+"ignore-sigterm:\n"), Launch{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d.Stop(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stop: %v", err)
	}
	if got := nextEvents(t, d, Exit); !slices.Equal(got, []Event{{Kind: Exit}}) {
		t.Fatalf("events %+v", got)
	}
}

func TestClaudePromptBeforeStart(t *testing.T) {
	d, _ := New("claude")
	if err := d.Prompt(context.Background(), "hi"); err == nil {
		t.Fatal("Prompt before Start succeeded")
	}
}

// A failed start (shape from a live run: an error result, then exit 1) is Start's error.
func TestClaudeStartFails(t *testing.T) {
	stateDir := t.TempDir()
	_, err := startClaude(t, writeTranscript(t, `out: {"type":"result","subtype":"error_during_execution","is_error":true,"num_turns":0,"session_id":"x","errors":["boom"]}
exit: 1
`), Launch{StateDir: stateDir})
	if err == nil || err.Error() != "claude exited with code 1 at startup: boom" {
		t.Fatalf("err %v", err)
	}
	if left, _ := os.ReadDir(stateDir); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

func TestClaudeInitRejected(t *testing.T) {
	stateDir := t.TempDir()
	_, err := startClaude(t, writeTranscript(t, `in: {"type":"control_request","request_id":"init","request":{"subtype":"initialize"}}
out: {"type":"control_response","response":{"subtype":"error","request_id":"init","error":"bad init"}}
`), Launch{StateDir: stateDir})
	if err == nil || err.Error() != "claude rejected initialize: bad init" {
		t.Fatalf("err %v", err)
	}
	if left, _ := os.ReadDir(stateDir); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

// A handshake timeout is not a rejected resume: no retry, the saved session stands.
func TestClaudeHandshakeTimeout(t *testing.T) {
	defer func(d time.Duration) { claudeStartTimeout = d }(claudeStartTimeout)
	claudeStartTimeout = 100 * time.Millisecond
	logs := &logSink{}
	d, err := startClaude(t, writeTranscript(t, ""), Launch{SessionID: "old", Log: logs.log})
	if err == nil || err.Error() != "claude did not answer initialize within 100ms" {
		t.Fatalf("err %v", err)
	}
	if d.SessionID() != "" || logged(logs, "claude: resume") {
		t.Fatalf("session %q, log %q", d.SessionID(), logs.lines)
	}
}

func TestClaudeHandshakeCanceled(t *testing.T) {
	d, _ := New("claude")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(100*time.Millisecond, cancel)
	err := d.Start(ctx, Launch{Bin: fakeHarness(t, writeTranscript(t, "")), StateDir: t.TempDir(), SessionID: "old"})
	if !errors.Is(err, context.Canceled) || d.SessionID() != "" {
		t.Fatalf("err %v session %q", err, d.SessionID())
	}
}

// claude rejects an unknown --resume id at startup (live 2.1.280: an error result, "No
// conversation found with session ID: …", exit 1); the driver starts a new session instead.
const claudeRejectResume = `echo "$@" >> "$ARGS"
for a in "$@"; do
  if [ "$a" = --resume ]; then
    echo '{"type":"result","subtype":"error_during_execution","is_error":true,"num_turns":0,"session_id":"old","errors":["No conversation found with session ID: old"]}'
    exit 1
  fi
done
`

func TestClaudeRejectedResume(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("ARGS", argsFile)
	bin := claudeScript(t, writeTranscript(t, claudeHandshake), claudeRejectResume+`exec "@FAKE@" "$@"`+"\n")
	logs := &logSink{}
	d, err := startClaude(t, "", Launch{Bin: bin, SessionID: "old", Log: logs.log})
	if err != nil {
		t.Fatal(err)
	}
	if !uuidRE.MatchString(d.SessionID()) {
		t.Fatalf("session %q, want a new uuid", d.SessionID())
	}
	if !logged(logs, "claude: resume old failed (claude exited with code 1 at startup: No conversation found") {
		t.Fatalf("logs %q", logs.lines)
	}
	b, _ := os.ReadFile(argsFile)
	runs := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(runs) != 2 || !strings.Contains(runs[0], "--resume old") ||
		!strings.Contains(runs[1], "--session-id "+d.SessionID()) || strings.Contains(runs[1], "--resume") {
		t.Fatalf("runs %q", runs)
	}
}

// A retry that fails too is Start's error (with claude's last stderr line), and the saved session
// is not replaced.
func TestClaudeRejectedResumeRetryFails(t *testing.T) {
	t.Setenv("ARGS", filepath.Join(t.TempDir(), "args"))
	bin := claudeScript(t, writeTranscript(t, ""), claudeRejectResume+`echo "Error: unknown option '--bogus-flag'" >&2
exit 1
`)
	d, err := startClaude(t, "", Launch{Bin: bin, SessionID: "old"})
	if err == nil || err.Error() != "claude exited with code 1 at startup: Error: unknown option '--bogus-flag'" {
		t.Fatalf("err %v", err)
	}
	if d.SessionID() != "" {
		t.Fatalf("session %q", d.SessionID())
	}
}

func TestClaudeAifProblem(t *testing.T) {
	for in, want := range map[string]string{
		`{"mcp_servers":[{"name":"aif","status":"connected"}]}`:                                                 "",
		`{"mcp_servers":[{"name":"aif","status":"failed","source":"dynamic"}]}`:                                 "claude: aif MCP server failed",
		`{"mcp_servers":[],"mcp_server_errors":[{"name":"aif","type":"invalid","message":"url with no type"}]}`: "claude: aif MCP server missing: url with no type",
		`{"mcp_servers":[{"name":"other","status":"connected"}]}`:                                               "claude: aif MCP server missing",
	} {
		var m claudeLine
		_ = json.Unmarshal([]byte(in), &m)
		if got := m.aifProblem(); got != want {
			t.Errorf("%s: %q, want %q", in, got, want)
		}
	}
}

func TestClaudeCmd(t *testing.T) {
	args, env := claudeCmd(Launch{Model: "haiku", Thinking: "low", AutoApprove: true}, "sid", false, "/s/system.md", "/s/mcp.json", "/s/settings.json", 1000)
	want := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--session-id", "sid", "--append-system-prompt-file", "/s/system.md", "--settings", "/s/settings.json",
		"--model", "haiku", "--effort", "low",
		"--mcp-config", "/s/mcp.json", "--strict-mcp-config", "--allowedTools", "mcp__aif", "--permission-prompts", "none",
		"--dangerously-skip-permissions"}
	if !slices.Equal(args, want) || env != nil {
		t.Fatalf("args %q env %q", args, env)
	}
	args, env = claudeCmd(Launch{Thinking: "off", AutoApprove: true}, "old", true, "s", "m", "c", 0)
	want = []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--resume", "old", "--append-system-prompt-file", "s", "--settings", "c",
		"--mcp-config", "m", "--strict-mcp-config", "--allowedTools", "mcp__aif", "--permission-prompts", "none",
		"--dangerously-skip-permissions"}
	if !slices.Equal(args, want) || !slices.Equal(env, []string{"MAX_THINKING_TOKENS=0", "IS_SANDBOX=1"}) {
		t.Fatalf("args %q env %q", args, env)
	}
	args, env = claudeCmd(Launch{}, "sid", false, "s", "m", "c", 0)
	if slices.Contains(args, "--dangerously-skip-permissions") || slices.Contains(args, "--bare") || env != nil {
		t.Fatalf("no auto-approve: args %q env %q", args, env)
	}
}

func TestClaudePreflight(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script stand-in for claude")
	}
	fake := func(code string) string {
		path := filepath.Join(t.TempDir(), "claude")
		if err := os.WriteFile(path, []byte("#!/bin/sh\necho '2.1.280 (Claude Code)'\nexit "+code+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	d, _ := New("claude")
	if err := d.Preflight(fake("0")); err != nil {
		t.Fatal(err)
	}
	if err := d.Preflight(fake("1")); !errors.Is(err, ErrPrerequisite) {
		t.Fatalf("failing --version: %v", err)
	}
	if err := d.Preflight(filepath.Join(t.TempDir(), "nosuch")); !errors.Is(err, ErrPrerequisite) {
		t.Fatalf("missing binary: %v", err)
	}
}
