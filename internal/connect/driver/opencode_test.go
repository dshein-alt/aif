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
	if l.MCPURL == "" {
		l.MCPURL, l.MCPToken = "http://aif:18080/mcp", "aif_tok"
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

// ocNew is request 2, a session/new answered with session id s.
func ocNew(s string) string {
	return `in: {"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/work",` + ocMCP + `}}
out: {"jsonrpc":"2.0","id":$id,"result":{"sessionId":"` + s + `"}}
`
}

// ocPrompt is request id, a session/prompt of text in session s.
func ocPrompt(id, s, text string) string {
	return `in: {"jsonrpc":"2.0","id":` + id + `,"method":"session/prompt","params":{"sessionId":"` + s + `","prompt":[{"type":"text","text":"` + text + `"}]}}
`
}

// noEvent fails if an event arrives within 200 ms.
func noEvent(t *testing.T, d Driver, after string) {
	t.Helper()
	select {
	case e := <-d.Events():
		t.Fatalf("event after %s: %+v", after, e)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestOpencodeRecordedTurn(t *testing.T) {
	stateDir := t.TempDir()
	d := startOpencode(t, "testdata/opencode.jsonl", Launch{StateDir: stateDir, SystemPrompt: "contract"})

	sysPath := filepath.Join(stateDir, "system.md")
	if b, _ := os.ReadFile(sysPath); string(b) != "contract" {
		t.Fatalf("system.md %q", b)
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
	noEvent(t, d, "Stop") // the Driver contract: Stop discards the Exit too
	if _, err := os.Stat(sysPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("system.md not removed: %v", err)
	}
}

// The isolation env reaches the child, and the system prompt path in it is absolute even when
// StateDir is not. A protocolVersion other than 1 is logged.
func TestOpencodeEnv(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.Mkdir("state", 0o700); err != nil {
		t.Fatal(err)
	}
	logs := &logSink{}
	d := startOpencode(t, writeTranscript(t, `dynamic: cwd
in: {"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{}}}
out: {"jsonrpc":"2.0","id":$id,"result":{"protocolVersion":2}}
`+ocNew("ses_env")+`env: OPENCODE_DISABLE_PROJECT_CONFIG
env: OPENCODE_DISABLE_CLAUDE_CODE
env: OPENCODE_DISABLE_EXTERNAL_SKILLS
env: OPENCODE_PURE
env: OPENCODE_CONFIG_CONTENT
exit: 0
`), Launch{StateDir: "state", Verbose: true, Log: logs.log})
	if got := nextEvents(t, d, Exit); !slices.Equal(got, []Event{{Kind: Exit}}) {
		t.Fatalf("events %+v", got)
	}
	abs, _ := filepath.Abs(filepath.Join("state", "system.md"))
	want := []string{"1", "1", "1", "1", opencodeConfig(abs)}
	var got []string
	logs.mu.Lock()
	for _, l := range logs.lines {
		if v, ok := strings.CutPrefix(l, "<< "); ok && !strings.HasPrefix(v, `{"jsonrpc"`) {
			got = append(got, v)
		}
	}
	logs.mu.Unlock()
	if !slices.Equal(got, want) {
		t.Fatalf("env %q\nwant %q", got, want)
	}
	if !logs.has("opencode: protocolVersion 2, this driver speaks 1") {
		t.Fatalf("log %q", logs.lines)
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
`+ocPrompt("5", "ses_saved", "hi")+`out: {"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_saved","update":{"sessionUpdate":"agent_message_chunk","messageId":"msg_3","content":{"type":"text","text":"hello"}}}}
out: {"jsonrpc":"2.0","id":$id,"result":{"stopReason":"end_turn"}}
`), Launch{SessionID: "ses_saved", Model: "llamacpp/Qwen3.6-27B-Q4_K_M", Thinking: "high"})
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
`), Launch{SessionID: "ses_gone", Log: func(s string) { log = append(log, s) }})
	if d.SessionID() != "ses_new" {
		t.Fatalf("session %q", d.SessionID())
	}
	if len(log) != 1 || log[0] != `opencode: session ses_gone not loaded: opencode: session/load: Internal error: OpenCode service failure {"service":"session"}` {
		t.Fatalf("log %q", log)
	}
}

// A rejected model or thinking level names the values the session accepts, from the config
// options of the last answer (a model change brings the new model's effort levels).
func TestOpencodeConfigRejected(t *testing.T) {
	const opts = `"configOptions":[{"id":"model","currentValue":"opencode/big-pickle","options":[{"value":"opencode/big-pickle"},{"group":"llamacpp","options":[{"value":"llamacpp/q"}]}]}]`
	for _, tc := range []struct {
		model, thinking, transcript, err string
	}{{"nope", "", `in: {"jsonrpc":"2.0","id":3,"method":"session/set_config_option","params":{"sessionId":"ses_c","configId":"model","value":"nope"}}
out: {"jsonrpc":"2.0","id":$id,"error":{"code":-32602,"message":"Invalid params"}}
`, `opencode: model "nope" rejected (accepts: opencode/big-pickle, llamacpp/q): Invalid params`},
		{"llamacpp/q", "xhigh", `in: {"jsonrpc":"2.0","id":3,"method":"session/set_config_option","params":{"sessionId":"ses_c","configId":"model","value":"llamacpp/q"}}
out: {"jsonrpc":"2.0","id":$id,"result":{"configOptions":[{"id":"model","currentValue":"llamacpp/q"},{"id":"effort","currentValue":"default","options":[{"value":"low"},{"value":"high"},{"value":"default"}]}]}}
in: {"jsonrpc":"2.0","id":4,"method":"session/set_config_option","params":{"sessionId":"ses_c","configId":"effort","value":"xhigh"}}
out: {"jsonrpc":"2.0","id":$id,"error":{"code":-32602,"message":"Invalid params","data":{"effort":"xhigh"}}}
`, `opencode: thinking "xhigh" rejected (model llamacpp/q accepts: low, high, default): Invalid params {"effort":"xhigh"}`},
	} {
		d, _ := New("opencode")
		err := d.Start(context.Background(), Launch{Bin: fakeHarness(t, writeTranscript(t, ocInit+`in: {"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/work",`+ocMCP+`}}
out: {"jsonrpc":"2.0","id":$id,"result":{"sessionId":"ses_c",`+opts+`}}
`+tc.transcript)), StateDir: t.TempDir(), MCPURL: "http://aif:18080/mcp", MCPToken: "aif_tok", Model: tc.model, Thinking: tc.thinking})
		if err == nil || err.Error() != tc.err {
			t.Fatalf("err %v\nwant %s", err, tc.err)
		}
	}
}

// A permission request mid-turn (the live shape) is answered at once: allow_once (never
// allow_always) under AutoApprove, a reject option otherwise, cancelled when the wanted option is
// not offered. Any other request from the agent gets a method-not-found error.
func TestOpencodePermission(t *testing.T) {
	const all = `[{"optionId":"always","kind":"allow_always","name":"Always allow"},{"optionId":"once","kind":"allow_once","name":"Allow once"},{"optionId":"reject","kind":"reject_once","name":"Reject"}]`
	for _, tc := range []struct {
		auto             bool
		options, outcome string
	}{
		{true, all, `{"outcome":"selected","optionId":"once"}`},
		{false, all, `{"outcome":"selected","optionId":"reject"}`},
		{true, `[{"optionId":"always","kind":"allow_always","name":"Always allow"}]`, `{"outcome":"cancelled"}`},
		{false, `[{"optionId":"once","kind":"allow_once","name":"Allow once"}]`, `{"outcome":"cancelled"}`},
	} {
		d := startOpencode(t, writeTranscript(t, ocInit+ocNew("ses_p")+ocPrompt("3", "ses_p", "read /etc/hostname")+`out: {"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_p","update":{"sessionUpdate":"tool_call","toolCallId":"call_function_iyfzhxkfa3wc_1","title":"read","kind":"read","status":"pending","locations":[],"rawInput":{}}}}
out: {"jsonrpc":"2.0","id":0,"method":"session/request_permission","params":{"sessionId":"ses_p","toolCall":{"toolCallId":"call_function_iyfzhxkfa3wc_1","title":"/etc","kind":"other","status":"pending","locations":[{"path":"/etc/hostname"},{"path":"/etc"}],"rawInput":{"filepath":"/etc/hostname","parentDir":"/etc"}},"options":`+tc.options+`}}
in: {"jsonrpc":"2.0","id":0,"result":{"outcome":`+tc.outcome+`}}
out: {"jsonrpc":"2.0","id":1,"method":"fs/read_text_file","params":{"sessionId":"ses_p","path":"/etc/hostname"}}
in: {"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found: fs/read_text_file"}}
out: {"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_p","update":{"sessionUpdate":"agent_message_chunk","messageId":"msg_1","content":{"type":"text","text":"done"}}}}
out: {"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}
`), Launch{AutoApprove: tc.auto})
		if err := d.Prompt(context.Background(), "read /etc/hostname"); err != nil {
			t.Fatal(err)
		}
		want := []Event{{Kind: ToolCall, Text: "read"}, {Kind: Text, Text: "done"}, {Kind: TurnEnd, OK: true}}
		if got := nextEvents(t, d, TurnEnd); !slices.Equal(got, want) {
			t.Fatalf("auto=%v options %s: events %+v", tc.auto, tc.options, got)
		}
	}
}

// A field of an unexpected type loses neither the line nor the answer: a tool_call carrying
// content (an array in ACP) still makes a ToolCall, and a malformed request is still answered.
func TestOpencodeDecodingLenient(t *testing.T) {
	d := startOpencode(t, writeTranscript(t, ocInit+ocNew("ses_d")+ocPrompt("3", "ses_d", "x")+`out: {"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_d","update":{"sessionUpdate":"tool_call","toolCallId":"c1","title":"aif_whoami","status":"completed","content":[{"type":"content","content":{"type":"text","text":"hi"}}]}}}
out: {"jsonrpc":"2.0","id":7,"method":"session/request_permission","params":{"sessionId":"ses_d","toolCall":"weird","options":{"not":"an array"}}}
in: {"jsonrpc":"2.0","id":7,"result":{"outcome":{"outcome":"cancelled"}}}
out: {"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_d","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"ok"}}}}
out: {"jsonrpc":"2.0","id":3,"result":{"stopReason":"end_turn"}}
`), Launch{AutoApprove: true})
	if err := d.Prompt(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	want := []Event{{Kind: ToolCall, Text: "aif_whoami"}, {Kind: Text, Text: "ok"}, {Kind: TurnEnd, OK: true}}
	if got := nextEvents(t, d, TurnEnd); !slices.Equal(got, want) {
		t.Fatalf("events %+v", got)
	}
}

// An error response, a stop reason other than end_turn or none at all fails the turn; a
// response nobody asked for is logged; the exit code reaches Exit.
func TestOpencodeFailedTurns(t *testing.T) {
	logs := &logSink{}
	d := startOpencode(t, writeTranscript(t, ocInit+ocNew("ses_e")+ocPrompt("3", "ses_e", "a")+`out: {"jsonrpc":"2.0","id":$id,"error":{"code":-32603,"message":"Internal error: Connection error.","data":{"service":"session"}}}
`+ocPrompt("4", "ses_e", "b")+`out: {"jsonrpc":"2.0","id":$id,"result":{"stopReason":"cancelled"}}
`+ocPrompt("5", "ses_e", "c")+`out: {"jsonrpc":"2.0","id":42,"result":{}}
out: {"jsonrpc":"2.0","id":$id,"result":{}}
exit: 3
`), Launch{Log: logs.log})
	var got []Event
	for _, p := range []string{"a", "b", "c"} {
		if err := d.Prompt(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		got = append(got, nextEvents(t, d, TurnEnd)...)
	}
	got = append(got, nextEvents(t, d, Exit)...)
	want := []Event{{Kind: TurnEnd, Err: "Internal error: Connection error."}, {Kind: TurnEnd, Err: "stop reason: cancelled"},
		{Kind: TurnEnd, Err: "no stop reason"}, {Kind: Exit, Err: "opencode exited with code 3"}}
	if !slices.Equal(got, want) {
		t.Fatalf("events %+v", got)
	}
	if !logs.has("opencode: response to unknown id 42, ignored") {
		t.Fatalf("log %q", logs.lines)
	}
}

// A second Prompt before the first one's TurnEnd is refused without reaching the harness.
func TestOpencodePromptInFlight(t *testing.T) {
	d := startOpencode(t, writeTranscript(t, ocInit+ocNew("ses_f")+ocPrompt("3", "ses_f", "a")), Launch{})
	if err := d.Prompt(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if err := d.Prompt(context.Background(), "b"); err == nil || err.Error() != "opencode: Prompt while a turn is in flight" {
		t.Fatalf("second Prompt: %v", err)
	}
}

// Start after an unexpected Exit starts a fresh process on the same driver.
func TestOpencodeStartAfterExit(t *testing.T) {
	ctx := context.Background()
	d := startOpencode(t, writeTranscript(t, ocInit+ocNew("ses_a")+"exit: 0\n"), Launch{})
	if got := nextEvents(t, d, Exit); !slices.Equal(got, []Event{{Kind: Exit}}) {
		t.Fatalf("events %+v", got)
	}
	bin := fakeHarness(t, writeTranscript(t, ocInit+ocNew("ses_b")+ocPrompt("3", "ses_b", "again")+`out: {"jsonrpc":"2.0","id":$id,"result":{"stopReason":"end_turn"}}
`))
	if err := d.Start(ctx, Launch{Bin: bin, StateDir: t.TempDir(), MCPURL: "http://aif:18080/mcp", MCPToken: "aif_tok"}); err != nil {
		t.Fatal(err)
	}
	if d.SessionID() != "ses_b" {
		t.Fatalf("session %q", d.SessionID())
	}
	if err := d.Prompt(ctx, "again"); err != nil {
		t.Fatal(err)
	}
	if got := nextEvents(t, d, TurnEnd); !slices.Equal(got, []Event{{Kind: TurnEnd, OK: true}}) {
		t.Fatalf("events %+v", got)
	}
}

// Start while running is refused; Stop discards the old process's events, queued or not, so a
// restart starts clean and its request ids start over.
func TestOpencodeRestart(t *testing.T) {
	ctx := context.Background()
	d := startOpencode(t, writeTranscript(t, ocInit+ocNew("ses_r")+ocPrompt("3", "ses_r", "hi")+`out: {"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_r","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"old"}}}}
out: {"jsonrpc":"2.0","id":$id,"result":{"stopReason":"end_turn"}}
`), Launch{})
	if err := d.Prompt(ctx, "hi"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); len(d.Events()) < 2; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the old turn's events never queued")
		}
	}
	launch := Launch{StateDir: t.TempDir(), MCPURL: "http://aif:18080/mcp", MCPToken: "aif_tok"}
	if err := d.Start(ctx, launch); err == nil {
		t.Fatal("Start while running succeeded")
	}
	if err := d.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	launch.Bin = fakeHarness(t, writeTranscript(t, ocInit+ocNew("ses_r2")+ocPrompt("3", "ses_r2", "again")+`out: {"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"ses_r2","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"new"}}}}
out: {"jsonrpc":"2.0","id":$id,"result":{"stopReason":"end_turn"}}
`))
	if err := d.Start(ctx, launch); err != nil {
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

func TestOpencodeKilledBySignal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no signals")
	}
	d := startOpencode(t, writeTranscript(t, ocInit+ocNew("ses_k")), Launch{})
	_ = d.cmd.Process.Kill()
	if got := nextEvents(t, d, Exit); !slices.Equal(got, []Event{{Kind: Exit, Err: "opencode killed by signal"}}) {
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
	noEvent(t, d, "a failed Start")
}

// A live opencode that never answers fails Start after opencodeStartTimeout, naming the request.
func TestOpencodeHandshakeTimeout(t *testing.T) {
	defer func(t0 time.Duration) { opencodeStartTimeout = t0 }(opencodeStartTimeout)
	opencodeStartTimeout = 200 * time.Millisecond
	d, _ := New("opencode")
	err := d.Start(context.Background(), Launch{Bin: fakeHarness(t, writeTranscript(t, ocInit+`in: {"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/work",`+ocMCP+`}}
`)), StateDir: t.TempDir(), MCPURL: "http://aif:18080/mcp", MCPToken: "aif_tok"})
	if err == nil || err.Error() != "opencode did not answer session/new within 200ms" {
		t.Fatalf("err %v", err)
	}
	noEvent(t, d, "a timed-out Start")
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
