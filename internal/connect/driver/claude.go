package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// claudeDriver runs Claude Code as `claude -p --input-format stream-json --output-format
// stream-json --verbose`: user turns go in as {"type":"user"} lines, whole assistant messages and
// one result per turn come back. Closing stdin ends the process once the turn in flight is done.
type claudeDriver struct {
	*proc
	events chan Event // shared by every proc a restart makes

	mu      sync.Mutex // session is also written by the read goroutine
	session string
}

func init() { Register("claude", func() Driver { return &claudeDriver{events: make(chan Event, 64)} }) }

// claudeStartTimeout bounds the initialize handshake (SessionStart hooks run before it answers).
var claudeStartTimeout = 2 * time.Minute

func (d *claudeDriver) Preflight(bin string) error {
	if out, err := exec.Command(bin, "--version").CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s --version: %v %s", ErrPrerequisite, bin, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Start spawns claude and waits for its answer to an initialize control request, which comes only
// after a --resume has loaded the session. A resume claude rejects (it prints an error result and
// exits: unknown id, pruned store, changed format) is retried once as a new session; the
// connector sees the changed SessionID.
func (d *claudeDriver) Start(ctx context.Context, l Launch) error {
	err := d.start(ctx, l)
	if err != nil && l.SessionID != "" && ctx.Err() == nil {
		if l.Log != nil {
			l.Log(fmt.Sprintf("claude: resume %s failed (%v); starting a new session", l.SessionID, err))
		}
		l.SessionID = ""
		err = d.start(ctx, l)
	}
	return err
}

func (d *claudeDriver) start(ctx context.Context, l Launch) error {
	p := newProc(l)
	mcp, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"aif": map[string]any{
		"type": "http", "url": l.MCPURL, "headers": map[string]string{"Authorization": "Bearer " + l.MCPToken},
	}}})
	mcpPath, err := p.TempFile("mcp.json", string(mcp))
	if err != nil {
		return err
	}
	sysPath, err := p.TempFile("system.md", l.SystemPrompt)
	if err != nil {
		p.Cleanup()
		return err
	}
	session, resume := l.SessionID, l.SessionID != ""
	if !resume {
		session = newUUID()
	}
	args, env := claudeCmd(l, session, resume, sysPath, mcpPath, os.Geteuid())
	p.launch.Env = append(env, l.Env...)
	if err := p.start(args...); err != nil {
		p.Cleanup()
		return err
	}
	// A dead process fails the write; the loop below still learns why from its output.
	_ = p.Send(`{"type":"control_request","request_id":"init","request":{"subtype":"initialize"}}`)
	timeout := time.NewTimer(claudeStartTimeout)
	defer timeout.Stop()
	why := ""
	for {
		select {
		case line, ok := <-p.Lines():
			if !ok {
				<-p.Exited()
				p.Cleanup()
				if why == "" {
					why = "no error message"
				}
				return fmt.Errorf("claude exited with code %d at startup: %s", p.ExitCode(), why)
			}
			var m claudeLine
			if json.Unmarshal([]byte(line), &m) != nil {
				continue
			}
			switch {
			case m.Type == "control_response" && m.Response.RequestID == "init":
				d.setSession(session)
				d.proc = p
				go d.read(p)
				return nil
			case m.Type == "result" && m.IsError:
				why = m.errText()
			case m.Type == "control_request":
				d.refuse(p, m.RequestID)
			}
		case <-ctx.Done():
			_ = p.Stop()
			p.Cleanup()
			return ctx.Err()
		case <-timeout.C:
			_ = p.Stop()
			p.Cleanup()
			return fmt.Errorf("claude did not answer initialize within %s", claudeStartTimeout)
		}
	}
}

// claudeCmd is the argv after the binary and the env the driver adds. MCP tools need approval
// outside bypass mode, so the aif server's tools are always allowed; anything else that would
// prompt is denied at once (--permission-prompts none), so no turn waits on an approval.
func claudeCmd(l Launch, session string, resume bool, sysPath, mcpPath string, euid int) (args, env []string) {
	args = []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose"}
	if resume {
		args = append(args, "--resume", session)
	} else {
		args = append(args, "--session-id", session)
	}
	args = append(args, "--append-system-prompt-file", sysPath)
	if l.Model != "" {
		args = append(args, "--model", l.Model)
	}
	// Current models think adaptively and take their budget from --effort; MAX_THINKING_TOKENS=0
	// is the documented off switch (Opus 5.5 and the Fable models cannot turn thinking off).
	switch l.Thinking {
	case "":
	case "off":
		env = append(env, "MAX_THINKING_TOKENS=0")
	default:
		args = append(args, "--effort", l.Thinking)
	}
	// --strict-mcp-config: only our aif entry, never a user-level one under someone else's token.
	args = append(args, "--mcp-config", mcpPath, "--strict-mcp-config", "--allowedTools", "mcp__aif", "--permission-prompts", "none")
	if l.AutoApprove {
		args = append(args, "--dangerously-skip-permissions")
		if euid == 0 {
			// claude refuses bypass mode as root unless told it is in a sandbox;
			// --allow-dangerously-skip-permissions does not lift that check (2.1.280).
			env = append(env, "IS_SANDBOX=1")
		}
	}
	return args, env
}

func (d *claudeDriver) Prompt(ctx context.Context, text string) error {
	b, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{
		"role": "user", "content": []map[string]string{{"type": "text", "text": text}},
	}})
	return d.Send(string(b))
}

// claudeLine is the union of the stdout lines the driver reads. A user line's content may be a
// string, which fails the decode; user lines are ignored anyway.
type claudeLine struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	RequestID string `json:"request_id"`
	Response  struct {
		RequestID string `json:"request_id"`
	} `json:"response"`
	Message struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Name string `json:"name"`
		} `json:"content"`
	} `json:"message"`
	IsError bool     `json:"is_error"`
	Result  string   `json:"result"`
	Errors  []string `json:"errors"`
}

func (m claudeLine) errText() string {
	switch {
	case m.Result != "":
		return m.Result
	case len(m.Errors) > 0:
		return strings.Join(m.Errors, "; ")
	}
	return m.Subtype
}

// read maps one proc's stdout to Events: text and tool_use blocks of whole assistant messages,
// one TurnEnd per result line.
func (d *claudeDriver) read(p *proc) {
	for line := range p.Lines() {
		var m claudeLine
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		switch m.Type {
		case "system":
			if m.Subtype == "init" && m.SessionID != "" {
				d.setSession(m.SessionID)
			}
		case "assistant":
			for _, c := range m.Message.Content {
				switch {
				case c.Type == "text" && c.Text != "":
					d.events <- Event{Kind: Text, Text: c.Text}
				case c.Type == "tool_use":
					d.events <- Event{Kind: ToolCall, Text: c.Name}
				}
			}
		case "result":
			if m.SessionID != "" {
				d.setSession(m.SessionID)
			}
			if m.IsError {
				d.events <- Event{Kind: TurnEnd, Err: m.errText()}
			} else {
				d.events <- Event{Kind: TurnEnd, OK: true}
			}
		case "control_request":
			d.refuse(p, m.RequestID)
		}
	}
	e := Event{Kind: Exit}
	if c := p.ExitCode(); c != 0 {
		e.Err = fmt.Sprintf("claude exited with code %d", c)
	}
	d.events <- e
}

// refuse answers a request from claude (a permission prompt, should one ever come despite
// --permission-prompts none) with an error, so the turn goes on without it.
func (d *claudeDriver) refuse(p *proc, id string) {
	b, _ := json.Marshal(map[string]any{"type": "control_response", "response": map[string]string{
		"subtype": "error", "request_id": id, "error": "aif-connect answers no requests",
	}})
	_ = p.Send(string(b))
}

func (d *claudeDriver) setSession(s string) {
	d.mu.Lock()
	d.session = s
	d.mu.Unlock()
}

func (d *claudeDriver) Events() <-chan Event { return d.events }

func (d *claudeDriver) SessionID() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.session
}

// Stop closes stdin, which ends claude cleanly once the turn in flight is done, then falls back
// to proc.Stop (SIGTERM, then kill).
func (d *claudeDriver) Stop(ctx context.Context) error {
	p := d.proc
	if p == nil {
		return nil
	}
	p.sendMu.Lock()
	_ = p.stdin.Close()
	p.sendMu.Unlock()
	select {
	case <-p.Exited():
	case <-time.After(5 * time.Second):
	case <-ctx.Done():
	}
	err := p.Stop()
	p.Cleanup()
	return err
}

func (d *claudeDriver) ToolHint() string {
	return "This harness exposes them as `mcp__aif__<tool>`, e.g. `mcp__aif__whoami`."
}

func (d *claudeDriver) HasSystemPromptChannel() bool { return true }
