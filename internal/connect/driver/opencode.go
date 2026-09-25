package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// opencodeDriver runs `opencode acp`: the Agent Client Protocol (agentclientprotocol.com),
// JSON-RPC 2.0 over stdio. The aif MCP server goes in session/new (session/load) as an "http"
// entry with an Authorization header; the system prompt is a file named by `instructions` in
// OPENCODE_CONFIG_CONTENT; model and thinking are the session's "model" and "effort" config
// options (ACP's form of opencode's --model / --variant).
//
// Isolation (opencodeEnv) keeps out the project's config and AGENTS.md, Claude Code's
// CLAUDE.md and skills, external skills and external plugins. Its limit: the user's global
// opencode config still loads, since it also holds their providers, so its `mcp` servers (one
// could carry another aif server under someone else's token) and ~/.config/opencode/AGENTS.md
// reach the resident too; keep them out of the global config of an account that runs one.
type opencodeDriver struct {
	*proc
	run     *opencodeRun  // the current proc's reader state
	events  chan Event    // shared by every proc a restart makes
	stopped chan struct{} // nil until Stop; closed once the current proc is gone and its temp files removed
	session string
}

// opencodeRun is one proc's protocol state, owned by that proc's reader: a restart makes a new
// one, so a stale reader never touches the next process's requests or turn.
type opencodeRun struct {
	p    *proc
	auto bool
	done chan struct{} // closed by the reader once its Exit is on events

	mu     sync.Mutex
	next   int                           // request ids 1, 2, …
	calls  map[int]chan opencodeResponse // Start's requests awaiting their response
	prompt int                           // id of the session/prompt in flight; 0: idle
}

// opencodeStartTimeout bounds each handshake request (call): a live but silent opencode must not
// hang Start. A var so tests can lower it.
var opencodeStartTimeout = 2 * time.Minute

// errOpencodeSlow is the cause of a handshake request's deadline.
var errOpencodeSlow = errors.New("opencode handshake timeout")

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
// its own system prompt. The path must be absolute: a relative one is looked up elsewhere.
func opencodeConfig(sysPath string) string {
	b, _ := json.Marshal(map[string]any{"instructions": []string{sysPath}})
	return string(b)
}

// opencodeEnv is the child's isolation (pi's --no-context-files --no-skills): no project
// opencode.json/AGENTS.md, no ~/.claude/CLAUDE.md or Claude Code skills, no external skills, no
// external plugins (OPENCODE_PURE; opencode's built-in plugins, provider auth among them, stay).
// Launch.Env comes after it and may override it.
func opencodeEnv(sysPath string) []string {
	return []string{
		"OPENCODE_CONFIG_CONTENT=" + opencodeConfig(sysPath),
		"OPENCODE_DISABLE_PROJECT_CONFIG=1",
		"OPENCODE_DISABLE_CLAUDE_CODE=1",
		"OPENCODE_DISABLE_EXTERNAL_SKILLS=1",
		"OPENCODE_PURE=1",
	}
}

func (d *opencodeDriver) Start(ctx context.Context, l Launch) error {
	if d.run != nil {
		select {
		case <-d.run.done:
		default:
			return errors.New("opencode: Start while the harness is running; Stop it first")
		}
	}
	cwd, err := filepath.Abs(l.Cwd) // ACP wants an absolute cwd
	if err != nil {
		return err
	}
	p := newProc(l)
	sysPath, err := p.TempFile("system.md", l.SystemPrompt)
	if err == nil {
		sysPath, err = filepath.Abs(sysPath)
	}
	if err != nil {
		p.Cleanup()
		return err
	}
	p.launch.Env = append(opencodeEnv(sysPath), l.Env...)
	if err := p.start("acp"); err != nil {
		p.Cleanup()
		return err
	}
	r := &opencodeRun{p: p, auto: l.AutoApprove, done: make(chan struct{}), calls: map[int]chan opencodeResponse{}}
	go d.read(r)

	session, err := r.handshake(ctx, l, cwd)
	if err != nil {
		// Nothing but this proc's Exit can be on events before a turn: discard it. p.Stop marks
		// the stop first, so that Exit carries no Err either.
		_ = p.Stop()
		for exited := false; !exited; {
			select {
			case <-d.events:
			case <-r.done:
				exited = true
			}
		}
		for drained := false; !drained; {
			select {
			case <-d.events:
			default:
				drained = true
			}
		}
		p.Cleanup()
		return err
	}
	d.proc, d.run, d.stopped, d.session = p, r, nil, session
	return nil
}

// handshake initializes the agent and opens the session; it returns the session id.
func (r *opencodeRun) handshake(ctx context.Context, l Launch, cwd string) (string, error) {
	res, err := r.call(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}})
	if err != nil {
		return "", err
	}
	var ini struct {
		ProtocolVersion int `json:"protocolVersion"`
	}
	_ = json.Unmarshal(res, &ini)
	if ini.ProtocolVersion != 1 {
		r.p.log(fmt.Sprintf("opencode: protocolVersion %d, this driver speaks 1", ini.ProtocolVersion))
	}
	mcp := []any{map[string]any{"type": "http", "name": "aif", "url": l.MCPURL,
		"headers": []any{map[string]string{"name": "Authorization", "value": "Bearer " + l.MCPToken}}}}
	session := ""
	if l.SessionID != "" {
		// session/load replays the history as session/update before it answers; read drops
		// updates outside a turn. Any error answer starts a new session (the connector sees the
		// new id): opencode reports an unknown id as a generic -32603, indistinguishable from a
		// transient failure, so such a failure forks the conversation. Accepted. No answer
		// (timeout, exit) fails Start.
		res, err = r.call(ctx, "session/load", map[string]any{"sessionId": l.SessionID, "cwd": cwd, "mcpServers": mcp})
		var re *opencodeError
		switch {
		case err == nil:
			session = l.SessionID
		case errors.As(err, &re):
			r.p.log(fmt.Sprintf("opencode: session %s not loaded: %v", l.SessionID, err))
		default:
			return "", err
		}
	}
	if session == "" {
		if res, err = r.call(ctx, "session/new", map[string]any{"cwd": cwd, "mcpServers": mcp}); err != nil {
			return "", err
		}
		var s struct {
			SessionID string `json:"sessionId"`
		}
		_ = unmarshalLenient(res, &s)
		if s.SessionID == "" {
			return "", fmt.Errorf("opencode: session/new returned no sessionId: %s", res)
		}
		session = s.SessionID
	}
	// model before effort: a model change resets the effort to the model's default. Each answer
	// carries the session's config options, which name the accepted values when one is rejected.
	for _, o := range [][2]string{{"model", l.Model}, {"effort", l.Thinking}} {
		if o[1] == "" {
			continue
		}
		opts := opencodeOptions(res)
		if res, err = r.call(ctx, "session/set_config_option", map[string]any{"sessionId": session, "configId": o[0], "value": o[1]}); err != nil {
			var re *opencodeError
			if !errors.As(err, &re) {
				return "", err
			}
			if o[0] == "model" {
				return "", fmt.Errorf("opencode: model %q rejected (accepts: %s): %s", o[1], opts.accepts("model"), re)
			}
			return "", fmt.Errorf("opencode: thinking %q rejected (model %s accepts: %s): %s", o[1], opts.current("model"), opts.accepts("effort"), re)
		}
	}
	return session, nil
}

// opencodeConfigOptions is the configOptions of a session/new, session/load or
// session/set_config_option answer.
type opencodeConfigOptions []struct {
	ID           string `json:"id"`
	CurrentValue string `json:"currentValue"`
	Options      []struct {
		Value   string     `json:"value"`
		Options []struct { // a group of options
			Value string `json:"value"`
		} `json:"options"`
	} `json:"options"`
}

func opencodeOptions(res json.RawMessage) opencodeConfigOptions {
	var r struct {
		ConfigOptions opencodeConfigOptions `json:"configOptions"`
	}
	_ = json.Unmarshal(res, &r) // best effort: a field of another type is skipped
	return r.ConfigOptions
}

func (o opencodeConfigOptions) current(id string) string {
	for _, c := range o {
		if c.ID == id && c.CurrentValue != "" {
			return c.CurrentValue
		}
	}
	return "?"
}

func (o opencodeConfigOptions) accepts(id string) string {
	var vals []string
	for _, c := range o {
		if c.ID != id {
			continue
		}
		for _, v := range c.Options {
			if v.Value != "" {
				vals = append(vals, v.Value)
			}
			for _, g := range v.Options {
				vals = append(vals, g.Value)
			}
		}
	}
	if len(vals) == 0 {
		return "none"
	}
	return strings.Join(vals, ", ")
}

// call sends one handshake request and waits opencodeStartTimeout at most for its response (the
// reader routes it here).
func (r *opencodeRun) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, opencodeStartTimeout, errOpencodeSlow)
	defer cancel()
	r.mu.Lock()
	r.next++
	id := r.next
	ch := make(chan opencodeResponse, 1)
	r.calls[id] = ch
	r.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err := r.p.Send(string(b)); err != nil {
		return nil, err
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return nil, fmt.Errorf("opencode: %s: %w", method, m.Error)
		}
		return m.Result, nil
	case <-r.p.Exited():
		return nil, fmt.Errorf("opencode: exited with code %d during %s", r.p.ExitCode(), method)
	case <-ctx.Done():
		if context.Cause(ctx) == errOpencodeSlow {
			return nil, fmt.Errorf("opencode did not answer %s within %v", method, opencodeStartTimeout)
		}
		return nil, fmt.Errorf("opencode: %s: %w", method, ctx.Err())
	}
}

func (d *opencodeDriver) Prompt(ctx context.Context, text string) error {
	r := d.run
	if r == nil {
		return errors.New("opencode is not started")
	}
	r.mu.Lock()
	if r.prompt != 0 {
		r.mu.Unlock()
		return errors.New("opencode: Prompt while a turn is in flight")
	}
	r.next++
	id := r.next
	r.prompt = id
	r.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "session/prompt",
		"params": map[string]any{"sessionId": d.session, "prompt": []any{map[string]string{"type": "text", "text": text}}}})
	err := r.p.Send(string(b))
	if err != nil {
		r.mu.Lock()
		if r.prompt == id {
			r.prompt = 0
		}
		r.mu.Unlock()
	}
	return err
}

// opencodeResponse is the envelope of every JSON-RPC message the driver reads: a response (id,
// result or error), a notification (method, params) or a request from the agent (id, method,
// params). Params stays raw and is decoded per method, so no shape of it loses the line.
type opencodeResponse struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *opencodeError  `json:"error"`
}

type opencodeError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (e *opencodeError) Error() string {
	if len(e.Data) > 0 && string(e.Data) != "null" {
		return e.Message + " " + string(e.Data)
	}
	return e.Message
}

// unmarshalLenient decodes b into v and fails only on malformed JSON: a field of an unexpected
// type is skipped and the rest still decodes (encoding/json's behaviour on UnmarshalTypeError).
func unmarshalLenient(b []byte, v any) error {
	var te *json.UnmarshalTypeError
	if err := json.Unmarshal(b, v); err != nil && !errors.As(err, &te) {
		return err
	}
	return nil
}

// read maps one proc's stdout to Events, then emits its Exit and closes done. The session/prompt
// response ends the turn.
func (d *opencodeDriver) read(r *opencodeRun) {
	defer close(r.done)
	p := r.p
	for line := range p.Lines() {
		var m opencodeResponse
		if unmarshalLenient([]byte(line), &m) != nil {
			continue
		}
		switch {
		case m.Method != "" && m.ID != nil: // a request from the agent: never leave one unanswered
			r.answer(m)
		case m.Method == "session/update":
			r.mu.Lock()
			inTurn := r.prompt != 0
			r.mu.Unlock()
			if !inTurn { // session/load's history replay, command lists
				continue
			}
			var u struct {
				Update struct {
					Kind    string          `json:"sessionUpdate"`
					Title   string          `json:"title"`
					Content json.RawMessage `json:"content"` // a content block; an array on tool_call
				} `json:"update"`
			}
			_ = unmarshalLenient(m.Params, &u)
			switch u.Update.Kind {
			case "agent_message_chunk":
				var c struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				if _ = unmarshalLenient(u.Update.Content, &c); c.Type == "text" && c.Text != "" {
					d.events <- Event{Kind: Text, Text: c.Text}
				}
			case "tool_call":
				d.events <- Event{Kind: ToolCall, Text: u.Update.Title}
			}
		case m.Method == "" && m.ID != nil:
			var id int
			_ = json.Unmarshal(m.ID, &id)
			r.mu.Lock()
			ch, isCall := r.calls[id]
			delete(r.calls, id)
			isPrompt := id != 0 && id == r.prompt
			if isPrompt {
				r.prompt = 0
			}
			r.mu.Unlock()
			switch {
			case isCall:
				ch <- m
			case isPrompt:
				d.events <- opencodeTurnEnd(m)
			default:
				p.log(fmt.Sprintf("opencode: response to unknown id %s, ignored", m.ID))
			}
		}
	}
	r.mu.Lock()
	r.prompt = 0 // an exit mid-turn: the Exit ends the turn
	r.mu.Unlock()
	e := Event{Kind: Exit}
	select {
	case <-p.stopping: // a deliberate Stop is not a failure
	default:
		switch c := p.ExitCode(); c {
		case 0:
		case -1:
			e.Err = "opencode killed by signal"
		default:
			e.Err = fmt.Sprintf("opencode exited with code %d", c)
		}
	}
	d.events <- e
}

func opencodeTurnEnd(m opencodeResponse) Event {
	if m.Error != nil {
		return Event{Kind: TurnEnd, Err: m.Error.Message}
	}
	var r struct {
		StopReason string `json:"stopReason"`
	}
	_ = unmarshalLenient(m.Result, &r)
	switch r.StopReason {
	case "end_turn":
		return Event{Kind: TurnEnd, OK: true}
	case "":
		return Event{Kind: TurnEnd, Err: "no stop reason"}
	}
	return Event{Kind: TurnEnd, Err: "stop reason: " + r.StopReason}
}

// answer replies to a request from the agent at once. session/request_permission: the
// allow_once option under AutoApprove (never allow_always: the approval would outlive the
// turn), else the first reject option; cancelled if the wanted one is not offered.
func (r *opencodeRun) answer(m opencodeResponse) {
	reply := map[string]any{"jsonrpc": "2.0", "id": m.ID}
	if m.Method != "session/request_permission" {
		reply["error"] = map[string]any{"code": -32601, "message": "method not found: " + m.Method}
	} else {
		var req struct {
			Options []struct {
				OptionID string `json:"optionId"`
				Kind     string `json:"kind"`
			} `json:"options"`
			ToolCall struct {
				Title string `json:"title"`
			} `json:"toolCall"`
		}
		_ = unmarshalLenient(m.Params, &req)
		outcome := map[string]string{"outcome": "cancelled"}
		for _, o := range req.Options {
			if r.auto && o.Kind == "allow_once" || !r.auto && strings.HasPrefix(o.Kind, "reject_") {
				outcome = map[string]string{"outcome": "selected", "optionId": o.OptionID}
				break
			}
		}
		r.p.log(fmt.Sprintf("opencode: permission %q: %s %s", req.ToolCall.Title, outcome["outcome"], outcome["optionId"]))
		reply["result"] = map[string]any{"outcome": outcome}
	}
	b, _ := json.Marshal(reply)
	_ = r.p.Send(string(b))
}

func (d *opencodeDriver) Events() <-chan Event { return d.events }
func (d *opencodeDriver) SessionID() string    { return d.session }

// Stop ends the process and discards its events up to and including its Exit (the Driver
// contract); after an unexpected death whose Exit was already read it only removes temp files.
func (d *opencodeDriver) Stop(ctx context.Context) error {
	if d.run == nil {
		return nil
	}
	p, done := d.proc, d.run.done
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

func (d *opencodeDriver) ToolHint() string {
	return "This harness exposes them as separate tools named `aif_<tool>`, e.g. `aif_whoami`."
}

func (d *opencodeDriver) HasSystemPromptChannel() bool { return true }
