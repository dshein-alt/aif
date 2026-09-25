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
)

// claudeDriver runs Claude Code as `claude -p --input-format stream-json --output-format
// stream-json --verbose`: user turns go in as {"type":"user"} lines, whole assistant messages and
// one result per turn come back.
type claudeDriver struct {
	*proc
	events  chan Event    // shared by every proc a restart makes
	done    chan struct{} // closed by the current proc's reader once its Exit is on events
	stopped chan struct{} // nil until Stop; closed once the current proc is gone and its temp files removed
	pending atomic.Bool   // a Prompt awaits its TurnEnd: set by Prompt, cleared by the reader

	mu      sync.Mutex // session is also written by the read goroutine
	session string
}

func init() { Register("claude", func() Driver { return &claudeDriver{events: make(chan Event, 64)} }) }

var (
	// claudeStartTimeout bounds the initialize handshake.
	claudeStartTimeout = 2 * time.Minute
	// claudeStopWait bounds Stop's interrupt and stdin EOF before it falls back to proc.Stop.
	claudeStopWait = 5 * time.Second
)

// claudeInterrupt ends the turn in flight, if any. Stop sends it: a turn cut short by a signal is
// left unfinished, and claude resumes an unfinished turn unprompted on the next --resume.
const claudeInterrupt = `{"type":"control_request","request_id":"stop","request":{"subtype":"interrupt"}}`

// claudeSettings keeps hooks out: the user's and the project's hooks (SessionStart and the like)
// belong to their interactive sessions, not to a resident. --settings takes precedence over the
// user, project and local settings files. Not --bare: it also disables OAuth login and most tools.
const claudeSettings = `{"disableAllHooks":true}`

func (d *claudeDriver) Preflight(bin string) error {
	if out, err := exec.Command(bin, "--version").CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s --version: %v %s", ErrPrerequisite, bin, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// claudeRejected: claude printed an error result and exited at startup, which is how it rejects a
// --resume (unknown id, pruned store, changed format).
type claudeRejected struct{ msg string }

func (e *claudeRejected) Error() string { return e.msg }

// Start spawns claude and waits for its answer to an initialize control request, which comes only
// after a --resume has loaded the session. A resume claude rejects is retried once as a new
// session; the connector sees the changed SessionID. Any other failure (a timeout, a spawn error)
// keeps the saved session: it may well be resumable next time.
func (d *claudeDriver) Start(ctx context.Context, l Launch) error {
	if d.done != nil {
		select {
		case <-d.done:
		default:
			return errors.New("claude: Start while the harness is running; Stop it first")
		}
	}
	err := d.start(ctx, l)
	var rej *claudeRejected
	if errors.As(err, &rej) && l.SessionID != "" {
		if l.Log != nil {
			l.Log(fmt.Sprintf("claude: resume %s failed (%v); starting a new session", l.SessionID, err))
		}
		l.SessionID = ""
		err = d.start(ctx, l)
	}
	return err
}

func (d *claudeDriver) start(ctx context.Context, l Launch) error {
	// The last stderr line explains a startup exit with no result (e.g. a bad --effort). Written by
	// the stderr copier, read only once the process has exited.
	lastStderr, log := "", l.Log
	l.Log = func(s string) {
		if rest, ok := strings.CutPrefix(s, "harness: "); ok {
			lastStderr = rest
		}
		if log != nil {
			log(s)
		}
	}
	p := newProc(l)
	mcp, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"aif": map[string]any{
		"type": "http", "url": l.MCPURL, "headers": map[string]string{"Authorization": "Bearer " + l.MCPToken},
	}}})
	var sysPath, setPath string
	mcpPath, err := p.TempFile("mcp.json", string(mcp))
	if err == nil {
		sysPath, err = p.TempFile("system.md", l.SystemPrompt)
	}
	if err == nil {
		setPath, err = p.TempFile("settings.json", claudeSettings)
	}
	if err != nil {
		p.Cleanup()
		return err
	}
	session, resume := l.SessionID, l.SessionID != ""
	if !resume {
		session = newUUID()
	}
	args, env := claudeCmd(l, session, resume, sysPath, mcpPath, setPath, os.Geteuid())
	p.launch.Env = append(env, l.Env...)
	if err := p.start(args...); err != nil {
		p.Cleanup()
		return err
	}
	fail := func(err error) error {
		_ = p.Stop()
		p.Cleanup()
		return err
	}
	// A dead process fails the write; the loop below still learns why from its output.
	_ = p.Send(`{"type":"control_request","request_id":"init","request":{"subtype":"initialize"}}`)
	timeout := time.NewTimer(claudeStartTimeout)
	defer timeout.Stop()
	rejected := ""
	for {
		select {
		case line, ok := <-p.Lines():
			if !ok {
				<-p.Exited()
				p.Cleanup()
				msg := fmt.Sprintf("claude exited with code %d at startup: ", p.ExitCode())
				switch {
				case rejected != "":
					return &claudeRejected{msg + rejected}
				case lastStderr != "":
					return errors.New(msg + lastStderr)
				}
				return errors.New(msg + "no error message")
			}
			var m claudeLine
			_ = json.Unmarshal([]byte(line), &m) // best effort, see read
			switch {
			case m.Type == "control_response" && m.Response.RequestID == "init":
				if m.Response.Subtype != "success" {
					why := m.Response.Error
					if why == "" {
						why = m.Response.Subtype
					}
					return fail(fmt.Errorf("claude rejected initialize: %s", why))
				}
				d.setSession(session)
				d.proc, d.done, d.stopped = p, make(chan struct{}), nil
				d.pending.Store(false)
				go d.read(p, d.done)
				return nil
			case m.Type == "result" && m.IsError:
				rejected = m.errText()
			case m.Type == "control_request":
				d.refuse(p, m.RequestID)
			}
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-timeout.C:
			return fail(fmt.Errorf("claude did not answer initialize within %s", claudeStartTimeout))
		}
	}
}

// claudeCmd is the argv after the binary and the env the driver adds. MCP tools need approval
// outside bypass mode, so the aif server's tools are always allowed; anything else that would
// prompt is denied at once (--permission-prompts none), so no turn waits on an approval.
func claudeCmd(l Launch, session string, resume bool, sysPath, mcpPath, settingsPath string, euid int) (args, env []string) {
	args = []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose"}
	if resume {
		args = append(args, "--resume", session)
	} else {
		args = append(args, "--session-id", session)
	}
	args = append(args, "--append-system-prompt-file", sysPath, "--settings", settingsPath)
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
	if d.proc == nil {
		return errors.New("claude: Prompt before Start")
	}
	b, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{
		"role": "user", "content": []map[string]string{{"type": "text", "text": text}},
	}})
	d.pending.Store(true)
	if err := d.Send(string(b)); err != nil {
		d.pending.Store(false)
		return err
	}
	return nil
}

// claudeLine is the union of the stdout lines the driver reads.
type claudeLine struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	RequestID string `json:"request_id"`
	Response  struct {
		Subtype   string `json:"subtype"`
		RequestID string `json:"request_id"`
		Error     string `json:"error"`
	} `json:"response"`
	Message struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Name string `json:"name"`
		} `json:"content"`
	} `json:"message"`
	IsError    bool     `json:"is_error"`
	Result     string   `json:"result"`
	Errors     []string `json:"errors"`
	MCPServers []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"mcp_servers"`
	MCPServerErrors []struct { // --mcp-config entries that failed validation; absent from mcp_servers
		Name    string `json:"name"`
		Message string `json:"message"`
	} `json:"mcp_server_errors"`
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

// aifProblem describes the aif MCP server on a system/init line, "" when it is connected: a
// resident without its tools must not look healthy.
func (m claudeLine) aifProblem() string {
	status, why := "missing", ""
	for _, s := range m.MCPServers {
		if s.Name == "aif" {
			status = s.Status
		}
	}
	for _, e := range m.MCPServerErrors {
		if e.Name == "aif" {
			why = e.Message
		}
	}
	switch {
	case why != "":
		return "claude: aif MCP server " + status + ": " + why
	case status != "connected":
		return "claude: aif MCP server " + status
	}
	return ""
}

// read maps one proc's stdout to Events (text and tool_use blocks of whole assistant messages,
// a TurnEnd for the result that ends a prompted turn), then emits its Exit and closes done.
//
// Decoding is best effort: a field whose type changes fails only that field, not the line, so a
// result is never lost to it (a lost result hangs the turn). A line with no type is skipped.
func (d *claudeDriver) read(p *proc, done chan struct{}) {
	defer close(done)
	for line := range p.Lines() {
		var m claudeLine
		_ = json.Unmarshal([]byte(line), &m)
		switch m.Type {
		case "system":
			if m.Subtype == "init" {
				if m.SessionID != "" {
					d.setSession(m.SessionID)
				}
				if s := m.aifProblem(); s != "" {
					p.log(s)
				}
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
			// No prompt pending: a background task's completion, or a resumed unfinished turn.
			if !d.pending.CompareAndSwap(true, false) {
				p.log("claude: result with no prompt pending, ignored")
			} else if m.IsError {
				d.events <- Event{Kind: TurnEnd, Err: m.errText()}
			} else {
				d.events <- Event{Kind: TurnEnd, OK: true}
			}
		case "control_request":
			d.refuse(p, m.RequestID)
		}
	}
	e := Event{Kind: Exit}
	select {
	case <-p.stopping: // a deliberate Stop is not a failure
	default:
		switch c := p.ExitCode(); c {
		case 0:
		case -1:
			e.Err = "claude killed by signal"
		default:
			e.Err = fmt.Sprintf("claude exited with code %d", c)
		}
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

// Stop ends the process and discards its events up to and including its Exit (the Driver
// contract). See claudeShutdown for how the process is ended.
func (d *claudeDriver) Stop(ctx context.Context) error {
	if d.proc == nil {
		return nil
	}
	p, done := d.proc, d.done
	if d.stopped == nil { // the first Stop of this proc
		stopped := make(chan struct{})
		d.stopped = stopped
		// Mark the stop before anything can end the process, so the reader's Exit sees it even
		// when the ctx kill below wins the race.
		p.stopOnce.Do(func() { close(p.stopping) })
		go func() {
			claudeShutdown(p)
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

// claudeShutdown ends p: an interrupt, so a turn in flight ends rather than being left unfinished;
// then stdin EOF, on which claude exits; then, claudeStopWait after the start, proc.Stop (SIGTERM,
// then kill).
func claudeShutdown(p *proc) {
	wait, cancel := context.WithTimeout(context.Background(), claudeStopWait)
	defer cancel()
	sent := make(chan struct{})
	go func() { _ = p.Send(claudeInterrupt); close(sent) }()
	select {
	case <-sent:
	case <-wait.Done():
	}
	// Not under sendMu: a refuse() may be blocked in Send, and the close unblocks it.
	_ = p.stdin.Close()
	select {
	case <-p.Exited():
	case <-wait.Done():
	}
	_ = p.Stop()
}

func (d *claudeDriver) ToolHint() string {
	return "This harness exposes them as `mcp__aif__<tool>`, e.g. `mcp__aif__whoami`."
}

func (d *claudeDriver) HasSystemPromptChannel() bool { return true }
