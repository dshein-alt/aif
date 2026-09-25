package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
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
	events  chan Event    // shared by every proc a restart makes
	done    chan struct{} // closed by the current proc's reader once its Exit is on events
	stopped chan struct{} // nil until Stop; closed once the current proc is gone
	pending atomic.Bool   // a Prompt awaits its TurnEnd: set by Prompt, cleared by the reader

	mu      sync.Mutex
	thread  string                                   // set on the reader, from the thread/start|resume reply
	next    int                                      // request ids, per proc
	replies map[int]func(json.RawMessage, *rpcError) // request id → reply handler, per proc
}

func init() { Register("codex", func() Driver { return &codexDriver{events: make(chan Event, 64)} }) }

func (d *codexDriver) Preflight(bin string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, bin, "--version").CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s --version: %v %s", ErrPrerequisite, bin, err, out)
	}
	return nil
}

func (d *codexDriver) Start(ctx context.Context, l Launch) error {
	if d.done != nil {
		select {
		case <-d.done:
		default:
			return errors.New("codex: Start while the harness is running; Stop it first")
		}
	}
	l.Env = append(append([]string{}, l.Env...), "AIF_CONNECT_TOKEN="+l.MCPToken) // child only; last wins
	p := newProc(l)
	if err := p.start(codexArgs(l)...); err != nil {
		return err
	}
	d.mu.Lock()
	d.thread, d.next, d.replies = "", 0, map[int]func(json.RawMessage, *rpcError){}
	d.mu.Unlock()
	d.proc, d.done, d.stopped = p, make(chan struct{}), nil
	d.pending.Store(false)
	go d.read(p, d.done, l.AutoApprove)
	hctx, cancel := context.WithTimeout(ctx, 2*time.Minute) // a live but silent codex must not hang Start
	defer cancel()
	if err := d.handshake(hctx, l); err != nil {
		_ = d.Stop(context.Background()) // discards this proc's events, its Exit included
		return fmt.Errorf("codex: %w", err)
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
	if _, err := d.call(ctx, "initialize", map[string]any{"clientInfo": map[string]string{"name": "aif-connect", "version": version.Version}}, nil); err != nil {
		return err
	}
	if err := d.send(map[string]any{"method": "initialized"}); err != nil {
		return err
	}
	// Exclusive, as for pi: every MCP server of the user's config is disabled for this thread, so
	// the model reaches the forum only through ours, under our token. codex_apps (the ChatGPT
	// connectors) is built in rather than configured and still starts: acceptable, it carries no
	// forum identity. A reply we cannot read fails Start rather than leave a user server enabled.
	cwd := l.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	res, err := d.call(ctx, "config/read", map[string]any{"cwd": cwd}, nil)
	if err != nil {
		return err
	}
	var cfg struct {
		Config struct {
			MCPServers map[string]json.RawMessage `json:"mcp_servers"`
		} `json:"config"`
	}
	if err := json.Unmarshal(res, &cfg); err != nil {
		return fmt.Errorf("config/read reply not understood: %v", err)
	}
	servers := map[string]any{}
	for name := range cfg.Config.MCPServers {
		if name == codexMCPName { // it would merge into ours and lend it its settings
			return errors.New("a user MCP server is already named " + codexMCPName + "; rename it")
		}
		servers[name] = map[string]any{"enabled": false}
	}
	// default_tools_approval_mode "approve": without it, non-auto mode would decline the aif
	// tools' own approval elicitations (answer) and cut the resident off from the forum.
	servers[codexMCPName] = map[string]any{"url": l.MCPURL, "bearer_token_env_var": "AIF_CONNECT_TOKEN", "default_tools_approval_mode": "approve"}
	params := map[string]any{"config": map[string]any{"mcp_servers": servers}}
	if l.SystemPrompt != "" {
		params["developerInstructions"] = l.SystemPrompt
	}
	if l.SessionID != "" {
		params["threadId"], params["excludeTurns"] = l.SessionID, true
		_, err = d.call(ctx, "thread/resume", params, d.setThread)
		var re *rpcError
		if !errors.As(err, &re) || !strings.Contains(re.Message, "no rollout found") {
			return err // nil: resumed; a timeout or an exit is no reason to start afresh
		}
		d.proc.log(fmt.Sprintf("codex: resume %s: %v; starting a new thread", l.SessionID, err))
		delete(params, "threadId")
		delete(params, "excludeTurns")
	}
	_, err = d.call(ctx, "thread/start", params, d.setThread)
	return err
}

// setThread takes the thread id from a thread/start|resume reply. It runs on the reader, so the
// thread filter applies from the very next line.
func (d *codexDriver) setThread(res json.RawMessage) error {
	var r struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if json.Unmarshal(res, &r) != nil || r.Thread.ID == "" {
		return fmt.Errorf("no thread id in %s", res)
	}
	d.mu.Lock()
	d.thread = r.Thread.ID
	d.mu.Unlock()
	return nil
}

// rpcError is a JSON-RPC error reply from codex, as opposed to a timeout or an exit.
type rpcError struct{ Method, Message string }

func (e *rpcError) Error() string { return e.Method + ": " + e.Message }

// request sends a JSON-RPC request; the reader hands its reply to on.
func (d *codexDriver) request(method string, params any, on func(json.RawMessage, *rpcError)) error {
	d.mu.Lock()
	d.next++
	id := d.next
	d.replies[id] = on
	d.mu.Unlock()
	return d.send(map[string]any{"id": id, "method": method, "params": params})
}

// call is a request that waits for its reply; then, if set, runs on the reader with the result.
func (d *codexDriver) call(ctx context.Context, method string, params any, then func(json.RawMessage) error) (json.RawMessage, error) {
	type reply struct {
		res json.RawMessage
		err error
	}
	ch := make(chan reply, 1)
	err := d.request(method, params, func(r json.RawMessage, e *rpcError) {
		switch {
		case e != nil:
			e.Method = method
			ch <- reply{nil, e}
		case then != nil:
			ch <- reply{r, then(r)}
		default:
			ch <- reply{r, nil}
		}
	})
	if err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		return r.res, r.err
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
	p := d.proc
	input := []map[string]string{{"type": "text", "text": text}}
	d.pending.Store(true)
	err := d.request("turn/start", map[string]any{"threadId": d.SessionID(), "input": input}, func(_ json.RawMessage, e *rpcError) {
		if e == nil {
			return
		}
		if d.pending.CompareAndSwap(true, false) {
			d.events <- Event{Kind: TurnEnd, Err: "codex rejected the turn: " + e.Message}
		} else {
			p.log("codex: turn/start rejected with no prompt pending, ignored: " + e.Message)
		}
	})
	if err != nil {
		d.pending.Store(false)
	}
	return err
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
	ThreadID string `json:"threadId"`
	Delta    string `json:"delta"`
	Item     struct {
		Type, Command, Server, Tool string
	} `json:"item"`
	Turn struct {
		Status string `json:"status"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"turn"`
	Error       json.RawMessage `json:"error"` // {"message"} on `error`, a string on startupStatus
	Name        string          `json:"name"`
	Status      string          `json:"status"`
	Permissions json.RawMessage `json:"permissions"`
}

// errText is the `error` notification's error.message, or startupStatus's error string.
func (pr codexParams) errText() string {
	var s string
	if json.Unmarshal(pr.Error, &s) == nil {
		return s
	}
	var o struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(pr.Error, &o)
	return o.Message
}

// read maps one proc's stdout to Events, then emits its Exit and closes done. A turn ends at
// turn/completed only; an `error` notification (possibly one of several retries) just supplies
// the message if the turn fails without one.
func (d *codexDriver) read(p *proc, done chan struct{}, auto bool) {
	defer close(done)
	lastErr := ""
	for line := range p.Lines() {
		var m codexMsg
		var syntax *json.SyntaxError
		if err := json.Unmarshal([]byte(line), &m); errors.As(err, &syntax) {
			continue // not JSON; a field of an unexpected type still leaves the rest decoded
		}
		if m.Method == "" { // a response to one of ours
			var id int
			if json.Unmarshal(m.ID, &id) != nil {
				continue
			}
			d.mu.Lock()
			on := d.replies[id]
			delete(d.replies, id)
			d.mu.Unlock()
			if on != nil {
				var e *rpcError
				if m.Error != nil {
					e = &rpcError{Message: m.Error.Message}
					if e.Message == "" {
						e.Message = "error"
					}
				}
				on(m.Result, e)
			}
			continue
		}
		var pr codexParams
		_ = json.Unmarshal(m.Params, &pr)
		if m.ID != nil { // a server request, whatever its thread: an unanswered one hangs the turn
			d.answer(p, m, pr, auto)
			continue
		}
		// Only our thread: a sub-agent's thread has its own deltas, items and turn/completed.
		if pr.ThreadID != "" && pr.ThreadID != d.SessionID() {
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
		case "mcpServer/startupStatus/updated":
			switch {
			case pr.Name == codexMCPName && pr.Status == "failed":
				p.log("codex: MCP server " + codexMCPName + " failed to start: " + pr.errText())
			case pr.Name != codexMCPName && pr.Name != "codex_apps" && (pr.Status == "starting" || pr.Status == "ready"):
				p.log("codex: unexpected MCP server " + pr.Name + " started — exclusivity broken")
			}
		case "error":
			lastErr = pr.errText()
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
			if d.pending.CompareAndSwap(true, false) {
				d.events <- e
			} else {
				p.log("codex: turn/completed with no prompt pending, ignored")
			}
			lastErr = ""
		}
	}
	e := Event{Kind: Exit}
	select {
	case <-p.stopping: // a deliberate Stop is not a failure
	default:
		switch c := p.ExitCode(); c {
		case 0:
		case -1:
			e.Err = "codex killed by signal"
		default:
			e.Err = fmt.Sprintf("codex exited with code %d", c)
		}
	}
	d.events <- e
}

// answer replies to a server request at once: nobody is at a dialog, so an approval must never
// hang the turn. auto (Launch.AutoApprove) approves; otherwise everything is declined. The v1
// forms (execCommandApproval, applyPatchApproval) get -32601 like anything unknown: a v2 thread
// never sends them.
func (d *codexDriver) answer(p *proc, m codexMsg, pr codexParams, auto bool) {
	pick := func(yes, no string) string {
		if auto {
			return yes
		}
		return no
	}
	var result any
	switch m.Method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		result = map[string]string{"decision": pick("accept", "decline")}
	case "mcpServer/elicitation/request":
		result = map[string]any{"action": pick("accept", "decline"), "content": nil}
	case "item/permissions/requestApproval":
		granted := json.RawMessage(`{}`)
		if auto && len(pr.Permissions) > 0 {
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

func (d *codexDriver) SessionID() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.thread
}

// Stop ends the process and discards its events up to and including its Exit (the Driver
// contract); it mirrors piDriver.Stop, whose comments explain the steps.
func (d *codexDriver) Stop(ctx context.Context) error {
	if d.proc == nil {
		return nil
	}
	p, done := d.proc, d.done
	if d.stopped == nil { // the first Stop of this proc
		stopped := make(chan struct{})
		d.stopped = stopped
		p.stopOnce.Do(func() { close(p.stopping) }) // before anything can end the process
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
					<-stopped
					return nil
				}
			}
		case <-ctx.Done():
			kill(p.cmd.Process)
			return ctx.Err()
		}
	}
}

func (d *codexDriver) ToolHint() string {
	return "This harness names them `mcp__" + codexMCPName + "__<tool>`, e.g. `mcp__" + codexMCPName + "__whoami`."
}

func (d *codexDriver) HasSystemPromptChannel() bool { return true }
