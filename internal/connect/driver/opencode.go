package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// opencodeDriver runs `opencode acp`: the Agent Client Protocol (agentclientprotocol.com),
// JSON-RPC 2.0 over stdio. The aif MCP server goes in session/new (session/load) as an "http"
// entry with an Authorization header; the system prompt is a file named by `instructions` in
// OPENCODE_CONFIG_CONTENT; model and thinking are the session's "model" and "effort" config
// options (ACP's form of opencode's --model / --variant).
type opencodeDriver struct {
	*proc
	events  chan Event // shared by every proc a restart makes
	session string
	auto    bool

	mu     sync.Mutex
	next   int                       // request ids 1, 2, … per proc
	calls  map[int]chan opencodeLine // Start's requests awaiting their response
	prompt int                       // id of the session/prompt in flight; 0: idle
}

func init() {
	Register("opencode", func() Driver { return &opencodeDriver{events: make(chan Event, 64)} })
}

func (d *opencodeDriver) Preflight(bin string) error {
	if out, err := exec.Command(bin, "--version").CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s --version: %v %s", ErrPrerequisite, bin, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// opencodeConfig is OPENCODE_CONFIG_CONTENT: only the system prompt file, which opencode adds to
// its own system prompt.
func opencodeConfig(sysPath string) string {
	b, _ := json.Marshal(map[string]any{"instructions": []string{sysPath}})
	return string(b)
}

func (d *opencodeDriver) Start(ctx context.Context, l Launch) (err error) {
	cwd, err := filepath.Abs(l.Cwd) // ACP wants an absolute cwd
	if err != nil {
		return err
	}
	p := newProc(l)
	sysPath, err := p.TempFile("system.md", l.SystemPrompt)
	if err != nil {
		return err
	}
	// OPENCODE_DISABLE_PROJECT_CONFIG: no project opencode.json/AGENTS.md (pi's --no-context-files);
	// a project config could also carry another aif server under someone else's token.
	p.launch.Env = append([]string{"OPENCODE_CONFIG_CONTENT=" + opencodeConfig(sysPath), "OPENCODE_DISABLE_PROJECT_CONFIG=1"}, l.Env...)
	if err := p.start("acp"); err != nil {
		p.Cleanup()
		return err
	}
	d.mu.Lock()
	d.next, d.prompt, d.calls = 0, 0, map[int]chan opencodeLine{}
	d.mu.Unlock()
	d.auto = l.AutoApprove
	started := make(chan bool, 1) // the reader reports Exit only for a proc Start handed over
	go d.read(p, started)
	defer func() {
		if err != nil {
			started <- false
			_ = p.Stop()
			p.Cleanup()
			return
		}
		d.proc = p
		started <- true
	}()

	if _, err := d.call(ctx, p, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}); err != nil {
		return err
	}
	mcp := []any{map[string]any{"type": "http", "name": "aif", "url": l.MCPURL,
		"headers": []any{map[string]string{"name": "Authorization", "value": "Bearer " + l.MCPToken}}}}
	d.session = ""
	if l.SessionID != "" {
		// session/load replays the history as session/update before it answers; read drops
		// updates outside a turn. A rejected id starts a new session; the connector sees the change.
		_, err := d.call(ctx, p, "session/load", map[string]any{"sessionId": l.SessionID, "cwd": cwd, "mcpServers": mcp})
		if err == nil {
			d.session = l.SessionID
		} else {
			p.log(fmt.Sprintf("opencode: session %s not loaded: %v", l.SessionID, err))
		}
	}
	if d.session == "" {
		res, err := d.call(ctx, p, "session/new", map[string]any{"cwd": cwd, "mcpServers": mcp})
		if err != nil {
			return err
		}
		var r struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(res, &r) != nil || r.SessionID == "" {
			return fmt.Errorf("opencode: session/new returned no sessionId: %s", res)
		}
		d.session = r.SessionID
	}
	// model before effort: a model change resets the effort to the model's default.
	for _, o := range [][2]string{{"model", l.Model}, {"effort", l.Thinking}} {
		if o[1] == "" {
			continue
		}
		if _, err := d.call(ctx, p, "session/set_config_option", map[string]any{"sessionId": d.session, "configId": o[0], "value": o[1]}); err != nil {
			return err
		}
	}
	return nil
}

// call sends one request and waits for its response (Start only; the reader routes it here).
func (d *opencodeDriver) call(ctx context.Context, p *proc, method string, params any) (json.RawMessage, error) {
	d.mu.Lock()
	d.next++
	id := d.next
	ch := make(chan opencodeLine, 1)
	d.calls[id] = ch
	d.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err := p.Send(string(b)); err != nil {
		return nil, err
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return nil, fmt.Errorf("opencode: %s: %s", method, m.Error.Message)
		}
		return m.Result, nil
	case <-p.Exited():
		return nil, fmt.Errorf("opencode: exited with code %d during %s", p.ExitCode(), method)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (d *opencodeDriver) Prompt(ctx context.Context, text string) error {
	d.mu.Lock()
	d.next++
	id := d.next
	d.prompt = id
	d.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "session/prompt",
		"params": map[string]any{"sessionId": d.session, "prompt": []any{map[string]string{"type": "text", "text": text}}}})
	err := d.Send(string(b))
	if err != nil {
		d.mu.Lock()
		d.prompt = 0
		d.mu.Unlock()
	}
	return err
}

// opencodeLine is the union of the JSON-RPC messages the driver reads: a response (id, result
// or error), a notification (method, params) or a request from the agent (id, method, params).
type opencodeLine struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		Update struct {
			Kind    string `json:"sessionUpdate"`
			Title   string `json:"title"`
			Content struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"update"`
		Options []struct {
			OptionID string `json:"optionId"`
			Kind     string `json:"kind"`
		} `json:"options"`
		ToolCall struct {
			Title string `json:"title"`
		} `json:"toolCall"`
	} `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// read maps one proc's stdout to Events. The session/prompt response ends the turn.
func (d *opencodeDriver) read(p *proc, started <-chan bool) {
	for line := range p.Lines() {
		var m opencodeLine
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		switch {
		case m.Method != "" && m.ID != nil: // a request from the agent: never leave one unanswered
			d.answer(p, m)
		case m.Method == "session/update":
			d.mu.Lock()
			inTurn := d.prompt != 0
			d.mu.Unlock()
			u := m.Params.Update
			switch {
			case !inTurn: // session/load's history replay, command lists
			case u.Kind == "agent_message_chunk" && u.Content.Type == "text" && u.Content.Text != "":
				d.events <- Event{Kind: Text, Text: u.Content.Text}
			case u.Kind == "tool_call":
				d.events <- Event{Kind: ToolCall, Text: u.Title}
			}
		case m.Method == "":
			var id int
			if json.Unmarshal(m.ID, &id) != nil {
				continue
			}
			d.mu.Lock()
			ch, isCall := d.calls[id]
			delete(d.calls, id)
			isPrompt := id == d.prompt && id != 0
			if isPrompt {
				d.prompt = 0
			}
			d.mu.Unlock()
			if isCall {
				ch <- m
			} else if isPrompt {
				d.events <- opencodeTurnEnd(m)
			}
		}
	}
	d.mu.Lock()
	d.prompt = 0
	d.mu.Unlock()
	if !<-started {
		return
	}
	e := Event{Kind: Exit}
	if c := p.ExitCode(); c != 0 {
		e.Err = fmt.Sprintf("opencode exited with code %d", c)
	}
	d.events <- e
}

func opencodeTurnEnd(m opencodeLine) Event {
	if m.Error != nil {
		return Event{Kind: TurnEnd, Err: m.Error.Message}
	}
	var r struct {
		StopReason string `json:"stopReason"`
	}
	_ = json.Unmarshal(m.Result, &r)
	if r.StopReason == "end_turn" {
		return Event{Kind: TurnEnd, OK: true}
	}
	return Event{Kind: TurnEnd, Err: "stop reason: " + r.StopReason}
}

// answer replies to a request from the agent at once. session/request_permission: the first
// allow option under AutoApprove, else the first reject option (cancelled if there is none).
func (d *opencodeDriver) answer(p *proc, m opencodeLine) {
	reply := map[string]any{"jsonrpc": "2.0", "id": m.ID}
	if m.Method != "session/request_permission" {
		reply["error"] = map[string]any{"code": -32601, "message": "method not found: " + m.Method}
	} else {
		want := "reject_"
		if d.auto {
			want = "allow_"
		}
		outcome := map[string]string{"outcome": "cancelled"}
		for _, o := range m.Params.Options {
			if strings.HasPrefix(o.Kind, want) {
				outcome = map[string]string{"outcome": "selected", "optionId": o.OptionID}
				break
			}
		}
		p.log(fmt.Sprintf("opencode: permission %q: %s %s", m.Params.ToolCall.Title, outcome["outcome"], outcome["optionId"]))
		reply["result"] = map[string]any{"outcome": outcome}
	}
	b, _ := json.Marshal(reply)
	_ = p.Send(string(b))
}

func (d *opencodeDriver) Events() <-chan Event { return d.events }
func (d *opencodeDriver) SessionID() string    { return d.session }

func (d *opencodeDriver) Stop(ctx context.Context) error {
	if d.proc == nil {
		return nil
	}
	err := d.proc.Stop()
	d.proc.Cleanup()
	return err
}

func (d *opencodeDriver) ToolHint() string {
	return "This harness exposes them as separate tools named `aif_<tool>`, e.g. `aif_whoami`."
}

func (d *opencodeDriver) HasSystemPromptChannel() bool { return true }
