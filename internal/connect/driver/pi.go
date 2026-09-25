package driver

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sync/atomic"
	"time"
)

// piDriver runs `pi --mode rpc` (pi-coding-agent docs/rpc.md): prompts go in as
// {"type":"prompt"} commands, the agent event stream comes back on stdout.
type piDriver struct {
	*proc
	events  chan Event    // shared by every proc a restart makes
	done    chan struct{} // closed by the current proc's reader once its Exit is on events
	stopped chan struct{} // nil until Stop; closed once the current proc is gone and its temp files removed
	pending atomic.Bool   // a Prompt awaits its TurnEnd: set by Prompt, cleared by the reader
	session string
	n       int // prompt ids t1, t2, …; Prompt is called from one goroutine
}

func init() { Register("pi", func() Driver { return &piDriver{events: make(chan Event, 64)} }) }

// Preflight: the aif MCP server reaches pi only through the pi-mcp-adapter package's
// --mcp-config flag, which pi lists under "Extension CLI Flags" once the adapter is installed.
func (d *piDriver) Preflight(bin string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--help").CombinedOutput()
	if bytes.Contains(out, []byte("--mcp-config")) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: %s --help: %v", ErrPrerequisite, bin, err)
	}
	return fmt.Errorf("%w: pi has no --mcp-config: install pi-mcp-adapter (pi install pi-mcp-adapter)", ErrPrerequisite)
}

// piTokenEnv hands the MCP bearer token to the adapter (its bearerTokenEnv): only the child's
// environment holds it, never a file.
const piTokenEnv = "AIF_CONNECT_TOKEN"

func (d *piDriver) Start(ctx context.Context, l Launch) error {
	if d.done != nil {
		select {
		case <-d.done:
		default:
			return errors.New("pi: Start while the harness is running; Stop it first")
		}
	}
	// Exclusive: the adapter reads only our --mcp-config, never the user's global or project MCP
	// files, which could carry another aif server under someone else's token. The token goes last
	// so Launch.Env cannot override it.
	env := append([]string{"PI_MCP_CONFIG_MODE=exclusive"}, l.Env...)
	l.Env = append(env, piTokenEnv+"="+l.MCPToken)
	p := newProc(l)
	mcp, _ := json.Marshal(map[string]any{
		"settings": map[string]any{"scriptMode": false},
		"mcpServers": map[string]any{"aif": map[string]any{
			"url": l.MCPURL, "auth": "bearer", "bearerTokenEnv": piTokenEnv, "lifecycle": "eager",
		}},
	})
	mcpPath, err := p.TempFile("mcp.json", string(mcp))
	if err != nil {
		return err
	}
	sysPath, err := p.TempFile("system.md", l.SystemPrompt)
	if err != nil {
		p.Cleanup()
		return err
	}
	d.session = l.SessionID
	if d.session == "" {
		d.session = newUUID()
	}
	if err := p.start(piArgs(l, d.session, sysPath, mcpPath)...); err != nil {
		p.Cleanup()
		return err
	}
	d.proc, d.done, d.stopped = p, make(chan struct{}), nil
	d.pending.Store(false)
	go d.read(p, d.done)
	return nil
}

// piArgs is the argv after the binary. No --no-extensions: it also disables the installed
// pi-mcp-adapter package, and with it --mcp-config.
func piArgs(l Launch, session, sysPath, mcpPath string) []string {
	args := []string{"--mode", "rpc", "--session-id", session, "--append-system-prompt", sysPath}
	if l.Model != "" {
		args = append(args, "--model", l.Model)
	}
	if l.Thinking != "" {
		args = append(args, "--thinking", l.Thinking)
	}
	args = append(args, "--mcp-config", mcpPath, "--no-context-files", "--no-skills", "--no-prompt-templates")
	if l.AutoApprove {
		return append(args, "--approve")
	}
	return append(args, "--no-approve")
}

func (d *piDriver) Prompt(ctx context.Context, text string) error {
	d.n++
	b, _ := json.Marshal(map[string]string{"id": fmt.Sprintf("t%d", d.n), "type": "prompt", "message": text})
	d.pending.Store(true)
	if err := d.Send(string(b)); err != nil {
		d.pending.Store(false)
		return err
	}
	return nil
}

// piLine is the union of the stdout lines the driver reads. Message stays raw: a message_end
// carries an object (piMessage), a confirm dialog request a string.
type piLine struct {
	Type     string          `json:"type"`
	ID       string          `json:"id"`
	Command  string          `json:"command"`
	Success  bool            `json:"success"`
	Error    string          `json:"error"`
	Method   string          `json:"method"`
	ToolName string          `json:"toolName"`
	Message  json.RawMessage `json:"message"`
	Args     struct {
		Tool string `json:"tool"`
	} `json:"args"`
	Delta struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
	} `json:"assistantMessageEvent"`
}

type piMessage struct {
	Role         string `json:"role"`
	StopReason   string `json:"stopReason"`
	ErrorMessage string `json:"errorMessage"`
}

// read maps one proc's stdout to Events, then emits its Exit and closes done.
//
// A turn ends at agent_settled, not agent_end: pi emits agent_end once per attempt (willRetry)
// when it retries a failed model call. The turn's outcome is that of the last assistant message:
// stopReason "error"/"aborted" fails it with errorMessage; "stop", "toolUse" and "length" pass
// ("length" hit the token limit: the reply is truncated, not failed, and its tools did run). No
// assistant message_end before agent_settled also passes: no model call was made (e.g. an
// extension consumed the prompt), while a failed model call always ends in an assistant message
// with stopReason "error".
func (d *piDriver) read(p *proc, done chan struct{}) {
	defer close(done)
	lastErr := ""
	for line := range p.Lines() {
		var m piLine
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		switch m.Type {
		case "message_update":
			if m.Delta.Type == "text_delta" && m.Delta.Delta != "" {
				d.events <- Event{Kind: Text, Text: m.Delta.Delta}
			}
		case "tool_execution_start":
			name := m.ToolName
			if name == "mcp__aif" && m.Args.Tool != "" {
				name += " " + m.Args.Tool // which AIF op ran
			}
			d.events <- Event{Kind: ToolCall, Text: name}
		case "message_end":
			var msg piMessage
			if json.Unmarshal(m.Message, &msg) == nil && msg.Role == "assistant" {
				lastErr = ""
				if r := msg.StopReason; r == "error" || r == "aborted" {
					lastErr = msg.ErrorMessage
					if lastErr == "" {
						lastErr = r
					}
				}
			}
		case "agent_settled":
			if d.pending.CompareAndSwap(true, false) {
				d.events <- Event{Kind: TurnEnd, OK: lastErr == "", Err: lastErr}
			} else {
				p.log("pi: agent_settled with no prompt pending, ignored")
			}
			lastErr = ""
		case "response":
			if m.Command == "prompt" && !m.Success {
				d.pending.Store(false)
				d.events <- Event{Kind: TurnEnd, Err: "pi rejected the prompt: " + m.Error}
			}
		case "extension_ui_request":
			// Nobody is at a dialog: cancel it at once rather than let the turn hang on it.
			switch m.Method {
			case "select", "confirm", "input", "editor":
				b, _ := json.Marshal(map[string]any{"type": "extension_ui_response", "id": m.ID, "cancelled": true})
				_ = p.Send(string(b))
			}
		}
	}
	e := Event{Kind: Exit}
	select {
	case <-p.stopping: // a deliberate Stop is not a failure
	default:
		switch c := p.ExitCode(); c {
		case 0:
		case -1:
			e.Err = "pi killed by signal"
		default:
			e.Err = fmt.Sprintf("pi exited with code %d", c)
		}
	}
	d.events <- e
}

func (d *piDriver) Events() <-chan Event { return d.events }
func (d *piDriver) SessionID() string    { return d.session }

// Stop ends the process and discards its events up to and including its Exit (the Driver
// contract). No stdin close before SIGTERM: pi runs the same shutdown on SIGTERM as on stdin EOF.
func (d *piDriver) Stop(ctx context.Context) error {
	if d.proc == nil {
		return nil
	}
	p, done := d.proc, d.done
	if d.stopped == nil { // the first Stop of this proc
		stopped := make(chan struct{})
		d.stopped = stopped
		// Mark the stop before anything can end the process, so the reader's Exit sees it even
		// when the ctx kill below wins the race with p.Stop.
		p.stopOnce.Do(func() { close(p.stopping) })
		go func() {
			_ = p.Stop()
			p.Cleanup()
			close(stopped)
		}()
	}
	stopped := d.stopped
	for {
		select {
		case <-d.events:
		case <-done:
			for { // what the reader queued before closing done
				select {
				case <-d.events:
				default:
					<-stopped // exited already: only the temp files are left to remove
					return nil
				}
			}
		case <-ctx.Done():
			kill(p.cmd.Process)
			return ctx.Err()
		}
	}
}

func (d *piDriver) ToolHint() string {
	return "This harness exposes them as one tool, `mcp__aif`, with a `tool` argument, e.g. `mcp__aif({\"tool\":\"whoami\"})`."
}

func (d *piDriver) HasSystemPromptChannel() bool { return true }

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
