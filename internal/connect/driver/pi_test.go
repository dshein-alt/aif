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
	"testing"
	"time"
)

// startPi starts the pi driver on the fake harness replaying transcript (a path).
func startPi(t *testing.T, transcript string, l Launch) *piDriver {
	t.Helper()
	d, err := New("pi")
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
	return d.(*piDriver)
}

func writeTranscript(t *testing.T, s string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pi.jsonl")
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// nextEvents reads events up to and including the first one of kind until.
func nextEvents(t *testing.T, d Driver, until Kind) []Event {
	t.Helper()
	var got []Event
	timeout := time.After(10 * time.Second)
	for {
		select {
		case e := <-d.Events():
			got = append(got, e)
			if e.Kind == until {
				return got
			}
		case <-timeout:
			t.Fatalf("no %s; events so far %+v", until, got)
		}
	}
}

const piPrompt = `Call the tool mcp__aif with {"tool":"whoami"}, then reply with the single word OK.`

func TestPiRecordedTurn(t *testing.T) {
	stateDir := t.TempDir()
	d := startPi(t, "testdata/pi.jsonl", Launch{StateDir: stateDir, MCPURL: "http://aif:18080/mcp", MCPToken: "aif_tok", SystemPrompt: "contract"})

	var mcp any
	b, _ := os.ReadFile(filepath.Join(stateDir, "mcp.json"))
	if err := json.Unmarshal(b, &mcp); err != nil {
		t.Fatal(err)
	}
	var want any
	_ = json.Unmarshal([]byte(`{"settings":{"scriptMode":false},"mcpServers":{"aif":{"url":"http://aif:18080/mcp","auth":"bearer","bearerTokenEnv":"AIF_CONNECT_TOKEN","lifecycle":"eager"}}}`), &want)
	if !reflect.DeepEqual(mcp, want) {
		t.Fatalf("mcp.json %s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(stateDir, "system.md")); string(b) != "contract" {
		t.Fatalf("system.md %q", b)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(d.SessionID()) {
		t.Fatalf("generated session id %q", d.SessionID())
	}

	if err := d.Prompt(context.Background(), piPrompt); err != nil {
		t.Fatal(err)
	}
	want2 := []Event{
		{Kind: Text, Text: "\n\n"},
		{Kind: ToolCall, Text: "mcp__aif whoami"},
		{Kind: Text, Text: "\n\n"},
		{Kind: Text, Text: "OK"},
		{Kind: TurnEnd, OK: true},
	}
	if got := nextEvents(t, d, TurnEnd); !slices.Equal(got, want2) {
		t.Fatalf("events %+v\nwant   %+v", got, want2)
	}

	if err := d.Start(context.Background(), Launch{Bin: d.launch.Bin, StateDir: stateDir}); err == nil {
		t.Fatal("Start on a running driver succeeded")
	}
	if err := d.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(d.Events()); n != 0 {
		t.Fatalf("%d events left after Stop", n)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "mcp.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mcp.json not removed: %v", err)
	}
}

// Also: the token reaches the child only through its environment (the mcp.json names the var).
func TestPiRejectedPrompt(t *testing.T) {
	logs := &logSink{}
	d := startPi(t, writeTranscript(t, `env: AIF_CONNECT_TOKEN
in: {"id":"t1","type":"prompt","message":"hi"}
out: {"id":$id,"type":"response","command":"prompt","success":false,"error":"Agent is already streaming"}
exit: 0
`), Launch{SessionID: "saved-id", MCPToken: "aif_tok", Env: []string{"AIF_CONNECT_TOKEN=spoof"}, Verbose: true, Log: logs.log})
	if d.SessionID() != "saved-id" {
		t.Fatalf("session %q", d.SessionID())
	}
	if err := d.Prompt(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	want := []Event{{Kind: TurnEnd, Err: "pi rejected the prompt: Agent is already streaming"}, {Kind: Exit}}
	if got := nextEvents(t, d, Exit); !slices.Equal(got, want) {
		t.Fatalf("events %+v", got)
	}
	if !logs.has("<< aif_tok") {
		t.Fatalf("child env lacks the token: %q", logs.lines)
	}
}

// A model call that fails through pi's auto-retry (shapes from a live run with the model server
// down): one TurnEnd at agent_settled, not one per agent_end; a dialog is cancelled at once (the
// fake waits at its in: line for the cancel, so without it no TurnEnd ever comes).
func TestPiModelErrorAfterRetries(t *testing.T) {
	d := startPi(t, writeTranscript(t, `in: {"id":"t1","type":"prompt","message":"hi"}
out: {"id":$id,"type":"response","command":"prompt","success":true}
out: {"type":"extension_ui_request","id":"u1","method":"confirm","title":"Trust this project?","message":"It runs its own extensions."}
in: {"type":"extension_ui_response","id":"u1","cancelled":true}
out: {"type":"agent_start"}
out: {"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"Connection error."}}
out: {"type":"agent_end","messages":[],"willRetry":true}
out: {"type":"auto_retry_start","attempt":1,"maxAttempts":2,"delayMs":2000,"errorMessage":"Connection error."}
out: {"type":"agent_start"}
out: {"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"Connection error."}}
out: {"type":"agent_end","messages":[],"willRetry":false}
out: {"type":"auto_retry_end","success":false,"attempt":2,"finalError":"Connection error."}
out: {"type":"agent_settled"}
exit: 3
`), Launch{})
	if err := d.Prompt(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	want := []Event{{Kind: TurnEnd, Err: "Connection error."}, {Kind: Exit, Err: "pi exited with code 3"}}
	if got := nextEvents(t, d, Exit); !slices.Equal(got, want) {
		t.Fatalf("events %+v", got)
	}
}

// Stop discards the old process's events, queued or not, so a restart starts clean.
func TestPiRestart(t *testing.T) {
	ctx := context.Background()
	d := startPi(t, writeTranscript(t, `in: {"id":"t1","type":"prompt","message":"hi"}
out: {"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"old"}}
out: {"type":"agent_settled"}
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
	bin := fakeHarness(t, writeTranscript(t, `in: {"id":"t2","type":"prompt","message":"again"}
out: {"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"new"}}
out: {"type":"agent_settled"}
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
func TestPiStopCtxEnds(t *testing.T) {
	d := startPi(t, writeTranscript(t, "ignore-sigterm:\n"), Launch{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d.Stop(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stop: %v", err)
	}
	if got := nextEvents(t, d, Exit); !slices.Equal(got, []Event{{Kind: Exit}}) {
		t.Fatalf("events %+v", got)
	}
}

func TestPiKilledBySignal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no signals")
	}
	d := startPi(t, writeTranscript(t, ""), Launch{})
	_ = d.cmd.Process.Kill()
	if got := nextEvents(t, d, Exit); !slices.Equal(got, []Event{{Kind: Exit, Err: "pi killed by signal"}}) {
		t.Fatalf("events %+v", got)
	}
}

// A settle nobody prompted for (no prompt pending) is logged, not a TurnEnd.
func TestPiSettledWithoutPrompt(t *testing.T) {
	logs := &logSink{}
	d := startPi(t, writeTranscript(t, `out: {"type":"agent_settled"}
exit: 0
`), Launch{Log: logs.log})
	if got := nextEvents(t, d, Exit); !slices.Equal(got, []Event{{Kind: Exit}}) {
		t.Fatalf("events %+v", got)
	}
	if !logs.has("pi: agent_settled with no prompt pending, ignored") {
		t.Fatalf("log %q", logs.lines)
	}
}

func TestPiArgs(t *testing.T) {
	got := piArgs(Launch{Model: "local/m", Thinking: "low", AutoApprove: true}, "sid", "/s/system.md", "/s/mcp.json")
	want := []string{"--mode", "rpc", "--session-id", "sid", "--append-system-prompt", "/s/system.md",
		"--model", "local/m", "--thinking", "low", "--mcp-config", "/s/mcp.json",
		"--no-context-files", "--no-skills", "--no-prompt-templates", "--approve"}
	if !slices.Equal(got, want) {
		t.Fatalf("args %q", got)
	}
	got = piArgs(Launch{}, "sid", "s", "m")
	want = []string{"--mode", "rpc", "--session-id", "sid", "--append-system-prompt", "s", "--mcp-config", "m",
		"--no-context-files", "--no-skills", "--no-prompt-templates", "--no-approve"}
	if !slices.Equal(got, want) {
		t.Fatalf("args %q", got)
	}
}

func TestPiPreflight(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script stand-in for pi")
	}
	fake := func(help string) string {
		path := filepath.Join(t.TempDir(), "pi")
		if err := os.WriteFile(path, []byte("#!/bin/sh\necho '"+help+"'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	d, _ := New("pi")
	if err := d.Preflight(fake("Extension CLI Flags:\n  --mcp-config <value>  Path to MCP config file")); err != nil {
		t.Fatal(err)
	}
	err := d.Preflight(fake("Options:\n  --model <pattern>"))
	if !errors.Is(err, ErrPrerequisite) || err.Error() != "harness prerequisite missing: pi has no --mcp-config: install pi-mcp-adapter (pi install pi-mcp-adapter)" {
		t.Fatalf("err %v", err)
	}
	if err := d.Preflight(filepath.Join(t.TempDir(), "nosuch")); !errors.Is(err, ErrPrerequisite) {
		t.Fatalf("missing binary: %v", err)
	}
}
