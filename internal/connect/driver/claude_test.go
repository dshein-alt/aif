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
	"sync"
	"testing"
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

const claudeHandshake = `in: {"type":"control_request","request_id":"init","request":{"subtype":"initialize"}}
out: {"type":"control_response","response":{"subtype":"success","request_id":"init","response":{}}}
`

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestClaudeRecordedTurn(t *testing.T) {
	stateDir := t.TempDir()
	d, err := startClaude(t, "testdata/claude.jsonl", Launch{StateDir: stateDir, MCPURL: "http://aif:18080/mcp", MCPToken: "aif_tok", SystemPrompt: "contract"})
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

	// Stop closes stdin; claude (and the fake) exit 0 on EOF.
	if err := d.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := nextEvents(t, d, Exit); !slices.Equal(got, []Event{{Kind: Exit}}) {
		t.Fatalf("after Stop %+v", got)
	}
	for _, f := range []string{"mcp.json", "system.md"} {
		if _, err := os.Stat(filepath.Join(stateDir, f)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s not removed: %v", f, err)
		}
	}
}

// An error result ends the turn with its text; a request from claude is refused, not left hanging.
func TestClaudeErrorResult(t *testing.T) {
	d, err := startClaude(t, writeTranscript(t, claudeHandshake+`in: {"type":"user","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}
out: {"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{}}}
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

// claude rejects an unknown --resume id at startup (live 2.1.280: an error result, "No
// conversation found with session ID: …", exit 1); the driver starts a new session instead.
func TestClaudeRejectedResume(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script wrapper")
	}
	fake := fakeHarness(t, writeTranscript(t, claudeHandshake))
	bin := filepath.Join(t.TempDir(), "claude")
	script := `#!/bin/sh
for a in "$@"; do
  if [ "$a" = --resume ]; then
    echo '{"type":"result","subtype":"error_during_execution","is_error":true,"num_turns":0,"session_id":"old","errors":["No conversation found with session ID: old"]}'
    exit 1
  fi
done
exec "` + fake + `" "$@"
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var logs []string
	d, err := startClaude(t, "", Launch{Bin: bin, SessionID: "old", Log: func(s string) { mu.Lock(); logs = append(logs, s); mu.Unlock() }})
	if err != nil {
		t.Fatal(err)
	}
	if !uuidRE.MatchString(d.SessionID()) {
		t.Fatalf("session %q, want a new uuid", d.SessionID())
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.ContainsFunc(logs, func(s string) bool {
		return strings.HasPrefix(s, "claude: resume old failed (claude exited with code 1 at startup: No conversation found")
	}) {
		t.Fatalf("logs %q", logs)
	}
}

func TestClaudeCmd(t *testing.T) {
	args, env := claudeCmd(Launch{Model: "haiku", Thinking: "low", AutoApprove: true}, "sid", false, "/s/system.md", "/s/mcp.json", 1000)
	want := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--session-id", "sid", "--append-system-prompt-file", "/s/system.md", "--model", "haiku", "--effort", "low",
		"--mcp-config", "/s/mcp.json", "--strict-mcp-config", "--allowedTools", "mcp__aif", "--permission-prompts", "none",
		"--dangerously-skip-permissions"}
	if !slices.Equal(args, want) || env != nil {
		t.Fatalf("args %q env %q", args, env)
	}
	args, env = claudeCmd(Launch{Thinking: "off", AutoApprove: true}, "old", true, "s", "m", 0)
	want = []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--resume", "old", "--append-system-prompt-file", "s",
		"--mcp-config", "m", "--strict-mcp-config", "--allowedTools", "mcp__aif", "--permission-prompts", "none",
		"--dangerously-skip-permissions"}
	if !slices.Equal(args, want) || !slices.Equal(env, []string{"MAX_THINKING_TOKENS=0", "IS_SANDBOX=1"}) {
		t.Fatalf("args %q env %q", args, env)
	}
	args, env = claudeCmd(Launch{}, "sid", false, "s", "m", 0)
	if slices.Contains(args, "--dangerously-skip-permissions") || env != nil {
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
