package driver

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os/exec"
)

// piDriver runs `pi --mode rpc` (pi-coding-agent docs/rpc.md): prompts go in as
// {"type":"prompt"} commands, the agent event stream comes back on stdout.
type piDriver struct {
	*proc
	events  chan Event // shared by every proc a restart makes
	session string
	n       int // prompt ids t1, t2, …; Prompt is called from one goroutine
}

func init() { Register("pi", func() Driver { return &piDriver{events: make(chan Event, 64)} }) }

// Preflight: the aif MCP server reaches pi only through the pi-mcp-adapter package's
// --mcp-config flag, which pi lists under "Extension CLI Flags" once the adapter is installed.
func (d *piDriver) Preflight(bin string) error {
	out, err := exec.Command(bin, "--help").CombinedOutput()
	if bytes.Contains(out, []byte("--mcp-config")) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: %s --help: %v", ErrPrerequisite, bin, err)
	}
	return fmt.Errorf("%w: pi has no --mcp-config: install pi-mcp-adapter (pi install pi-mcp-adapter)", ErrPrerequisite)
}

func (d *piDriver) Start(ctx context.Context, l Launch) error {
	// Exclusive: the adapter reads only our --mcp-config, never the user's global or project MCP
	// files, which could carry another aif server under someone else's token.
	l.Env = append([]string{"PI_MCP_CONFIG_MODE=exclusive"}, l.Env...)
	p := newProc(l)
	mcp, _ := json.Marshal(map[string]any{
		"settings": map[string]any{"scriptMode": false},
		"mcpServers": map[string]any{"aif": map[string]any{
			"url": l.MCPURL, "auth": "bearer", "bearerToken": l.MCPToken, "lifecycle": "eager",
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
	d.proc = p
	go d.read(p)
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
	return d.Send(string(b))
}

// piLine is the union of the stdout lines the driver reads.
type piLine struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	Command  string `json:"command"`
	Success  bool   `json:"success"`
	Error    string `json:"error"`
	Method   string `json:"method"`
	ToolName string `json:"toolName"`
	Message  struct {
		Role         string `json:"role"`
		StopReason   string `json:"stopReason"`
		ErrorMessage string `json:"errorMessage"`
	} `json:"message"`
	Delta struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
	} `json:"assistantMessageEvent"`
}

// read maps one proc's stdout to Events. A turn ends at agent_settled, not agent_end: pi emits
// agent_end once per attempt (willRetry) when it retries a failed model call. The turn's outcome
// is that of the last assistant message: stopReason "error"/"aborted" carries errorMessage.
func (d *piDriver) read(p *proc) {
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
			d.events <- Event{Kind: ToolCall, Text: m.ToolName}
		case "message_end":
			if m.Message.Role == "assistant" {
				lastErr = ""
				if r := m.Message.StopReason; r == "error" || r == "aborted" {
					lastErr = m.Message.ErrorMessage
					if lastErr == "" {
						lastErr = r
					}
				}
			}
		case "agent_settled":
			d.events <- Event{Kind: TurnEnd, OK: lastErr == "", Err: lastErr}
			lastErr = ""
		case "response":
			if m.Command == "prompt" && !m.Success {
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
	if c := p.ExitCode(); c != 0 {
		e.Err = fmt.Sprintf("pi exited with code %d", c)
	}
	d.events <- e
}

func (d *piDriver) Events() <-chan Event { return d.events }
func (d *piDriver) SessionID() string    { return d.session }

func (d *piDriver) Stop(ctx context.Context) error {
	if d.proc == nil {
		return nil
	}
	err := d.proc.Stop()
	d.proc.Cleanup()
	return err
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
