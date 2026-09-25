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

// startOpencode starts the opencode driver on the fake harness replaying transcript (a path).
func startOpencode(t *testing.T, transcript string, l Launch) *opencodeDriver {
	t.Helper()
	d, err := New("opencode")
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
	return d.(*opencodeDriver)
}

const ocInit = `dynamic: cwd
in: {"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{}}}
out: {"jsonrpc":"2.0","id":$id,"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":true}}}
`

const ocMCP = `"mcpServers":[{"type":"http","name":"aif","url":"http://aif:18080/mcp","headers":[{"name":"Authorization","value":"Bearer aif_tok"}]}]`

func TestOpencodeRecordedTurn(t *testing.T) {
	stateDir := t.TempDir()
	d := startOpencode(t, "testdata/opencode.jsonl", Launch{StateDir: stateDir, MCPURL: "http://aif:18080/mcp", MCPToken: "aif_tok", SystemPrompt: "contract"})

	sysPath := filepath.Join(stateDir, "system.md")
	if b, _ := os.ReadFile(sysPath); string(b) != "contract" {
		t.Fatalf("system.md %q", b)
	}
	if got := opencodeConfig(sysPath); got != `{"instructions":["`+sysPath+`"]}` {
		t.Fatalf("config %s", got)
	}
	if d.SessionID() != "ses_f278c877bffem44Qo9lI5KxouO" {
		t.Fatalf("session %q", d.SessionID())
	}

	if err := d.Prompt(context.Background(), "Call the whoami tool once, then reply with the single word OK."); err != nil {
		t.Fatal(err)
	}
	want := []Event{{Kind: ToolCall, Text: "aif_whoami"}, {Kind: Text, Text: "OK"}, {Kind: TurnEnd, OK: true}}
	if got := nextEvents(t, d, TurnEnd); !slices.Equal(got, want) {
		t.Fatalf("events %+v\nwant   %+v", got, want)
	}

	if err := d.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := nextEvents(t, d, Exit); len(got) != 1 || got[0].Err == "" {
		t.Fatalf("after Stop %+v, want one Exit with the signal's code", got)
	}
	if _, err := os.Stat(sysPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("system.md not removed: %v", err)
	}
}

// session/load replays the history before it answers (shapes from a live load): none of it
// becomes an event. Model and thinking go in as session config options.
func TestOpencodeResume(t *testing.T) {
	d := startOpencode(t, writeTranscript(t, ocInit+`in: {"jsonrpc":"2.0","id":2,"method":"session/load","params":{"sessionId":"ses_saved","cwd":"/work",`+ocMCP+`}}
out: {"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_saved","update":{"sessionUpdate":"user_message_chunk","messageId":"msg_1","content":{"type":"text","text":"earlier"}}}}
out: {"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_saved","update":{"sessionUpdate":"agent_message_chunk","messageId":"msg_2","content":{"type":"text","text":"old reply"}}}}
out: {"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_saved","update":{"sessionUpdate":"tool_call","toolCallId":"call_1","title":"aif_whoami","kind":"other","status":"pending","locations":[],"rawInput":{}}}}
out: {"jsonrpc":"2.0","id":$id,"result":{"configOptions":[]}}
in: {"jsonrpc":"2.0","id":3,"method":"session/set_config_option","params":{"sessionId":"ses_saved","configId":"model","value":"llamacpp/Qwen3.6-27B-Q4_K_M"}}
out: {"jsonrpc":"2.0","id":$id,"result":{"configOptions":[]}}
in: {"jsonrpc":"2.0","id":4,"method":"session/set_config_option","params":{"sessionId":"ses_saved","configId":"effort","value":"high"}}
out: {"jsonrpc":"2.0","id":$id,"result":{"configOptions":[]}}
in: {"jsonrpc":"2.0","id":5,"method":"session/prompt","params":{"sessionId":"ses_saved","prompt":[{"type":"text","text":"hi"}]}}
out: {"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_saved","update":{"sessionUpdate":"agent_message_chunk","messageId":"msg_3","content":{"type":"text","text":"hello"}}}}
out: {"jsonrpc":"2.0","id":$id,"result":{"stopReason":"end_turn"}}
`), Launch{SessionID: "ses_saved", MCPURL: "http://aif:18080/mcp", MCPToken: "aif_tok", Model: "llamacpp/Qwen3.6-27B-Q4_K_M", Thinking: "high"})
	if d.SessionID() != "ses_saved" {
		t.Fatalf("session %q", d.SessionID())
	}
	if err := d.Prompt(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	want := []Event{{Kind: Text, Text: "hello"}, {Kind: TurnEnd, OK: true}}
	if got := nextEvents(t, d, TurnEnd); !slices.Equal(got, want) {
		t.Fatalf("events %+v", got)
	}
}

// An unknown session id (the live error) starts a new session.
func TestOpencodeLoadRejected(t *testing.T) {
	var log []string
	d := startOpencode(t, writeTranscript(t, ocInit+`in: {"jsonrpc":"2.0","id":2,"method":"session/load","params":{"sessionId":"ses_gone","cwd":"/work",`+ocMCP+`}}
out: {"jsonrpc":"2.0","id":$id,"error":{"code":-32603,"message":"Internal error: OpenCode service failure","data":{"service":"session"}}}
in: {"jsonrpc":"2.0","id":3,"method":"session/new","params":{"cwd":"/work",`+ocMCP+`}}
out: {"jsonrpc":"2.0","id":$id,"result":{"sessionId":"ses_new","configOptions":[]}}
`), Launch{SessionID: "ses_gone", MCPURL: "http://aif:18080/mcp", MCPToken: "aif_tok", Log: func(s string) { log = append(log, s) }})
	if d.SessionID() != "ses_new" {
		t.Fatalf("session %q", d.SessionID())
	}
	if len(log) != 1 || log[0] != "opencode: session ses_gone not loaded: opencode: session/load: Internal error: OpenCode service failure" {
		t.Fatalf("log %q", log)
	}
}

// A permission request mid-turn (the live shape) is answered at once: allow under AutoApprove,
// reject otherwise. Any other request from the agent gets a method-not-found error.
func TestOpencodePermission(t *testing.T) {
	for _, tc := range []struct {
		auto   bool
		option string
	}{{true, "once"}, {false, "reject"}} {
		d := startOpencode(t, writeTranscript(t, ocInit+`in: {"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/work",`+ocMCP+`}}
out: {"jsonrpc":"2.0","id":$id,"result":{"sessionId":"ses_p"}}
in: {"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"ses_p","prompt":[{"type":"text","text":"read /etc/hostname"}]}}
out: {"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_p","update":{"sessionUpdate":"tool_call","toolCallId":"call_function_iyfzhxkfa3wc_1","title":"read","kind":"read","status":"pending","locations":[],"rawInput":{}}}}
out: {"jsonrpc":"2.0","id":0,"method":"session/request_permission","params":{"sessionId":"ses_p","toolCall":{"toolCallId":"call_function_iyfzhxkfa3wc_1","title":"/etc","kind":"other","status":"pending","locations":[{"path":"/etc/hostname"},{"path":"/etc"}],"rawInput":{"filepath":"/etc/hostname","parentDir":"/etc"}},"options":[{"optionId":"once","kind":"allow_once","name":"Allow once"},{"optionId":"always","kind":"allow_always","name":"Always allow"},{"optionId":"reject","kind":"reject_once","name":"Reject"}]}}
in: {"jsonrpc":"2.0","id":0,"result":{"outcome":{"outcome":"selected","optionId":"`+tc.option+`"}}}
out: {"jsonrpc":"2.0","id":1,"method":"fs/read_text_file","params":{"sessionId":"ses_p","path":"/etc/hostname"}}
in: {"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found: fs/read_text_file"}}
out: {"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_p","update":{"sessionUpdate":"agent_message_chunk","messageId":"msg_1","content":{"type":"text","text":"done"}}}}
out: {"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}
`), Launch{MCPURL: "http://aif:18080/mcp", MCPToken: "aif_tok", AutoApprove: tc.auto})
		if err := d.Prompt(context.Background(), "read /etc/hostname"); err != nil {
			t.Fatal(err)
		}
		want := []Event{{Kind: ToolCall, Text: "read"}, {Kind: Text, Text: "done"}, {Kind: TurnEnd, OK: true}}
		if got := nextEvents(t, d, TurnEnd); !slices.Equal(got, want) {
			t.Fatalf("auto=%v events %+v", tc.auto, got)
		}
	}
}

// An error response or a stop reason other than end_turn fails the turn; the exit code reaches Exit.
func TestOpencodeFailedTurns(t *testing.T) {
	d := startOpencode(t, writeTranscript(t, ocInit+`in: {"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/work",`+ocMCP+`}}
out: {"jsonrpc":"2.0","id":$id,"result":{"sessionId":"ses_e"}}
in: {"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"ses_e","prompt":[{"type":"text","text":"a"}]}}
out: {"jsonrpc":"2.0","id":$id,"error":{"code":-32603,"message":"Internal error: Connection error.","data":{"service":"session"}}}
in: {"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":"ses_e","prompt":[{"type":"text","text":"b"}]}}
out: {"jsonrpc":"2.0","id":$id,"result":{"stopReason":"cancelled"}}
exit: 3
`), Launch{MCPURL: "http://aif:18080/mcp", MCPToken: "aif_tok"})
	var got []Event
	for _, p := range []string{"a", "b"} {
		if err := d.Prompt(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		got = append(got, nextEvents(t, d, TurnEnd)...)
	}
	got = append(got, nextEvents(t, d, Exit)...)
	want := []Event{{Kind: TurnEnd, Err: "Internal error: Connection error."}, {Kind: TurnEnd, Err: "stop reason: cancelled"}, {Kind: Exit, Err: "opencode exited with code 3"}}
	if !slices.Equal(got, want) {
		t.Fatalf("events %+v", got)
	}
}

func TestOpencodeStartFails(t *testing.T) {
	d, _ := New("opencode")
	err := d.Start(context.Background(), Launch{Bin: fakeHarness(t, writeTranscript(t, ocInit+`in: {"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/work",`+ocMCP+`}}
out: {"jsonrpc":"2.0","id":$id,"error":{"code":-32603,"message":"Internal error: OpenCode service failure"}}
`)), StateDir: t.TempDir(), MCPURL: "http://aif:18080/mcp", MCPToken: "aif_tok"})
	if err == nil || err.Error() != "opencode: session/new: Internal error: OpenCode service failure" {
		t.Fatalf("err %v", err)
	}
	select {
	case e := <-d.Events():
		t.Fatalf("event after a failed Start: %+v", e)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestOpencodePreflight(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script stand-in for opencode")
	}
	path := filepath.Join(t.TempDir(), "opencode")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n[ \"$1\" = --version ] && echo 1.18.32\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d, _ := New("opencode")
	if err := d.Preflight(path); err != nil {
		t.Fatal(err)
	}
	if err := d.Preflight(filepath.Join(t.TempDir(), "nosuch")); !errors.Is(err, ErrPrerequisite) || !strings.Contains(err.Error(), "--version") {
		t.Fatalf("missing binary: %v", err)
	}
}
