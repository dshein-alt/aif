package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/dshein-alt/aif/internal/version"
)

// codexMCPName names the connector's MCP server inside codex. Not "aif": codex deep-merges the
// thread's MCP config into the user's config.toml, so a user's own [mcp_servers.aif] would lend
// its http_headers (another agent's Authorization or X-Agent) to ours.
const codexMCPName = "aif_connect"

// codexDriver runs `codex app-server` (JSON-RPC over stdio, experimental in codex itself; codex
// 0.155): initialize, then thread/start or thread/resume, then one turn/start per prompt.
type codexDriver struct {
	*proc
	events chan Event // shared by every proc a restart makes
	thread string
	auto   bool

	mu      sync.Mutex
	next    int
	pending map[int]func(json.RawMessage, string) // request id → reply handler (result, error)
}

func init() { Register("codex", func() Driver { return &codexDriver{events: make(chan Event, 64)} }) }

func (d *codexDriver) Preflight(bin string) error {
	if out, err := exec.Command(bin, "--version").CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s --version: %v %s", ErrPrerequisite, bin, err, out)
	}
	return nil
}

func (d *codexDriver) Start(ctx context.Context, l Launch) error {
	l.Env = append(append([]string{}, l.Env...), "AIF_CONNECT_TOKEN="+l.MCPToken) // child only; last wins
	p := newProc(l)
	d.mu.Lock()
	d.pending = map[int]func(json.RawMessage, string){}
	d.mu.Unlock()
	d.auto = l.AutoApprove
	if err := p.start(codexArgs(l)...); err != nil {
		return err
	}
	d.proc = p
	go d.read(p)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute) // a live but silent codex must not hang Start
	defer cancel()
	if err := d.handshake(ctx, l); err != nil {
		_ = p.Stop()
		for e := range d.events { // the reader's Exit for this proc; nothing else precedes a turn
			if e.Kind == Exit {
				break
			}
		}
		p.Cleanup()
		return fmt.Errorf("codex app-server: %w", err)
	}
	return nil
}

// codexArgs is the argv after the binary. The aif MCP server is not here: a -c mcp_servers table
// is dropped once thread/start carries its own config.mcp_servers, which it must (handshake).
func codexArgs(l Launch) []string {
	args := []string{"app-server"}
	if l.Model != "" {
		args = append(args, "-c", "model="+tomlString(l.Model))
	}
	if l.Thinking != "" {
		args = append(args, "-c", "model_reasoning_effort="+tomlString(l.Thinking))
	}
	if l.AutoApprove { // what `codex exec --dangerously-bypass-approvals-and-sandbox` sets
		args = append(args, "-c", `approval_policy="never"`, "-c", `sandbox_mode="danger-full-access"`)
	}
	return args
}

// tomlString quotes s as a TOML basic string (JSON string escapes are valid TOML); unquoted, codex
// would parse a value like `true` or `5` as TOML rather than as the literal string.
func tomlString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func (d *codexDriver) handshake(ctx context.Context, l Launch) error {
	if _, err := d.call(ctx, "initialize", map[string]any{"clientInfo": map[string]string{"name": "aif-connect", "version": version.Version}}); err != nil {
		return err
	}
	if err := d.send(map[string]any{"method": "initialized"}); err != nil {
		return err
	}
	// Exclusive, as for pi: every MCP server of the user's config is disabled for this thread, so
	// the model reaches the forum only through ours, under our token.
	cwd := l.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	res, err := d.call(ctx, "config/read", map[string]any{"cwd": cwd})
	if err != nil {
		return err
	}
	var cfg struct {
		Config struct {
			MCPServers map[string]json.RawMessage `json:"mcp_servers"`
		} `json:"config"`
	}
	_ = json.Unmarshal(res, &cfg)
	servers := map[string]any{}
	for name := range cfg.Config.MCPServers {
		servers[name] = map[string]any{"enabled": false}
	}
	// ponytail: a user server that is itself named aif_connect would merge into ours; rename then.
	servers[codexMCPName] = map[string]any{"url": l.MCPURL, "bearer_token_env_var": "AIF_CONNECT_TOKEN", "default_tools_approval_mode": "approve"}
	params := map[string]any{"config": map[string]any{"mcp_servers": servers}}
	if l.SystemPrompt != "" {
		params["developerInstructions"] = l.SystemPrompt
	}
	if l.SessionID != "" {
		params["threadId"], params["excludeTurns"] = l.SessionID, true
		res, err = d.call(ctx, "thread/resume", params)
		if err == nil {
			return d.setThread(res)
		}
		d.proc.log(fmt.Sprintf("codex: resume %s: %v; starting a new thread", l.SessionID, err))
		delete(params, "threadId")
		delete(params, "excludeTurns")
	}
	if res, err = d.call(ctx, "thread/start", params); err != nil {
		return err
	}
	return d.setThread(res)
}

func (d *codexDriver) setThread(res json.RawMessage) error {
	var r struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if json.Unmarshal(res, &r) != nil || r.Thread.ID == "" {
		return fmt.Errorf("no thread id in %s", res)
	}
	d.thread = r.Thread.ID
	return nil
}

// request sends a JSON-RPC request; the reader hands its reply to on.
func (d *codexDriver) request(method string, params any, on func(json.RawMessage, string)) error {
	d.mu.Lock()
	d.next++
	id := d.next
	d.pending[id] = on
	d.mu.Unlock()
	return d.send(map[string]any{"id": id, "method": method, "params": params})
}

// call is a request that waits for its reply.
func (d *codexDriver) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	type reply struct {
		res json.RawMessage
		err string
	}
	ch := make(chan reply, 1)
	if err := d.request(method, params, func(r json.RawMessage, e string) { ch <- reply{r, e} }); err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		if r.err != "" {
			return nil, fmt.Errorf("%s: %s", method, r.err)
		}
		return r.res, nil
	case <-d.proc.Exited():
		return nil, fmt.Errorf("%s: codex exited with code %d", method, d.proc.ExitCode())
	case <-ctx.Done():
		return nil, fmt.Errorf("%s: %w", method, ctx.Err())
	}
}

func (d *codexDriver) send(msg any) error {
	b, _ := json.Marshal(msg)
	return d.proc.Send(string(b))
}

func (d *codexDriver) Prompt(ctx context.Context, text string) error {
	if d.proc == nil {
		return errors.New("codex is not started")
	}
	input := []map[string]string{{"type": "text", "text": text}}
	return d.request("turn/start", map[string]any{"threadId": d.thread, "input": input}, func(_ json.RawMessage, e string) {
		if e != "" {
			d.events <- Event{Kind: TurnEnd, Err: "codex rejected the turn: " + e}
		}
	})
}

// codexMsg is the union of what app-server writes: a response (id, result|error), a
// notification (method, params) or a server request (id, method, params).
type codexMsg struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type codexParams struct {
	Delta string `json:"delta"`
	Item  struct {
		Type, Command, Server, Tool string
	} `json:"item"`
	Turn struct {
		Status string `json:"status"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"turn"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
	Permissions json.RawMessage `json:"permissions"`
}

// read maps one proc's stdout to Events. A turn ends at turn/completed only; an `error`
// notification (possibly one of several retries) just supplies the message if the turn fails
// without one.
func (d *codexDriver) read(p *proc) {
	lastErr := ""
	for line := range p.Lines() {
		var m codexMsg
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		if m.Method == "" { // a response to one of ours
			var id int
			if json.Unmarshal(m.ID, &id) != nil {
				continue
			}
			d.mu.Lock()
			on := d.pending[id]
			delete(d.pending, id)
			d.mu.Unlock()
			if on != nil {
				e := ""
				if m.Error != nil {
					e = m.Error.Message
					if e == "" {
						e = "error"
					}
				}
				on(m.Result, e)
			}
			continue
		}
		var pr codexParams
		_ = json.Unmarshal(m.Params, &pr)
		if m.ID != nil {
			d.answer(p, m, pr)
			continue
		}
		switch m.Method {
		case "item/agentMessage/delta":
			if pr.Delta != "" {
				d.events <- Event{Kind: Text, Text: pr.Delta}
			}
		case "item/started":
			switch it := pr.Item; it.Type {
			case "mcpToolCall":
				d.events <- Event{Kind: ToolCall, Text: "mcp__" + it.Server + "__" + it.Tool}
			case "commandExecution":
				d.events <- Event{Kind: ToolCall, Text: it.Command}
			case "dynamicToolCall":
				d.events <- Event{Kind: ToolCall, Text: it.Tool}
			case "fileChange", "webSearch":
				d.events <- Event{Kind: ToolCall, Text: it.Type}
			}
		case "error":
			lastErr = pr.Error.Message
		case "turn/completed":
			e := Event{Kind: TurnEnd, OK: pr.Turn.Status == "completed"}
			if !e.OK {
				switch {
				case pr.Turn.Error != nil && pr.Turn.Error.Message != "":
					e.Err = pr.Turn.Error.Message
				case lastErr != "":
					e.Err = lastErr
				default:
					e.Err = "turn " + pr.Turn.Status
				}
			}
			d.events <- e
			lastErr = ""
		}
	}
	e := Event{Kind: Exit}
	if c := p.ExitCode(); c != 0 {
		e.Err = fmt.Sprintf("codex exited with code %d", c)
	}
	d.events <- e
}

// answer replies to a server request at once: nobody is at a dialog, so an approval must never
// hang the turn. AutoApprove approves; otherwise everything is declined.
func (d *codexDriver) answer(p *proc, m codexMsg, pr codexParams) {
	pick := func(yes, no string) string {
		if d.auto {
			return yes
		}
		return no
	}
	var result any
	switch m.Method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		result = map[string]string{"decision": pick("accept", "decline")}
	case "execCommandApproval", "applyPatchApproval": // the v1 forms
		result = map[string]string{"decision": pick("approved", "denied")}
	case "mcpServer/elicitation/request":
		result = map[string]any{"action": pick("accept", "decline"), "content": nil}
	case "item/permissions/requestApproval":
		granted := json.RawMessage(`{}`)
		if d.auto && len(pr.Permissions) > 0 {
			granted = pr.Permissions
		}
		result = map[string]any{"permissions": granted}
	case "item/tool/requestUserInput":
		result = map[string]any{"answers": map[string]any{}}
	}
	msg := map[string]any{"id": m.ID, "result": result}
	if result == nil {
		msg = map[string]any{"id": m.ID, "error": map[string]any{"code": -32601, "message": "aif-connect does not handle " + m.Method}}
	}
	b, _ := json.Marshal(msg)
	_ = p.Send(string(b))
}

func (d *codexDriver) Events() <-chan Event { return d.events }
func (d *codexDriver) SessionID() string    { return d.thread }

func (d *codexDriver) Stop(ctx context.Context) error {
	if d.proc == nil {
		return nil
	}
	err := d.proc.Stop()
	d.proc.Cleanup()
	return err
}

func (d *codexDriver) ToolHint() string {
	return "This harness names them `mcp__" + codexMCPName + "__<tool>`, e.g. `mcp__" + codexMCPName + "__whoami`."
}

func (d *codexDriver) HasSystemPromptChannel() bool { return true }
