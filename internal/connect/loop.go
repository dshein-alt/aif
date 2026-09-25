package connect

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/dshein-alt/aif/internal/connect/driver"
)

// AIF is the part of the AIF client the loop uses; *Client implements it.
type AIF interface {
	Ping(ctx context.Context) (string, error)
	WhoAmI(ctx context.Context) (string, error)
	Poll(ctx context.Context, wait int) (n int, seq int64, err error)
	Feed(ctx context.Context, since int64) ([]Message, int64, error)
}

// ControlServer is a bound control socket; *Server implements it.
type ControlServer interface{ Close() }

// Deps are Run's collaborators, injected so the tests use fakes. A nil Control, Log, Now or Sleep
// means Serve, discard, time.Now and a plain timer.
type Deps struct {
	Client  AIF
	Driver  driver.Driver
	Control func(dir string, notes *Notes, h Handler) (ControlServer, error)
	Log     func(string)
	Now     func() time.Time
	Sleep   func(ctx context.Context, d time.Duration) // returns early when ctx is done
	Home    string                                     // the state directory lives under it (StateDir)
	Version string                                     // compared with ping's v
	Env     []string                                   // harness environment beyond AIF_CONNECT_NAME/STATE (the CLI's PATH)
	Verbose bool                                       // drivers echo raw harness stdout
}

const (
	failBudget = 3               // consecutive failed turns: exit 4
	idleBudget = 3               // harness deaths while idle within one interval: exit 4
	scanBudget = 3               // consecutive failed scans hold new turns
	pollCap    = 60              // the server's long-poll cap, seconds
	drainWait  = 2 * time.Second // how long a stopped harness gets to report its Exit
)

// Run is the connector: startup (design doc, "## Startup") and the turn loop ("## The loop") until
// a stop, a SHUTDOWN or a fatal error. It returns the process exit code. The CLI has resolved
// cfg.Bin; cancelling ctx is a stop.
func Run(ctx context.Context, cfg Config, d Deps) int {
	if d.Log == nil {
		d.Log = func(string) {}
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Sleep == nil {
		d.Sleep = sleep
	}
	if d.Control == nil {
		d.Control = func(dir string, n *Notes, h Handler) (ControlServer, error) { return Serve(dir, n, h) }
	}
	l := &loop{ctx: ctx, cfg: cfg, d: d, phase: "starting"}
	code := l.startup()
	d.Log(fmt.Sprintf("exit code=%d", code))
	return code
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

type loop struct {
	ctx    context.Context
	cfg    Config
	d      Deps
	st     *State
	launch driver.Launch

	// Guarded by st's mutex: the control handler reads or writes these.
	phase, reason, versionWarning string
	busy, stopReq                 bool
	lastPoll                      time.Time
	cancelWait                    context.CancelFunc

	// The loop goroutine's own.
	alive, needContract bool
	failures, scanFails int
	lastInbox           int64
	recover, cause      string // a recover turn owed: its control text and the failure's cause
	pollErr             string
	deaths              []time.Time
}

func (l *loop) logf(format string, a ...any) { l.d.Log(fmt.Sprintf(format, a...)) }

// save persists the state; the caller holds its lock. A failed write is logged, not fatal: the
// in-memory state stays authoritative and the next save retries it.
func (l *loop) save() {
	if err := l.st.Save(); err != nil {
		l.logf("state: %v", err)
	}
}

func (l *loop) startup() int {
	cfg, d := l.cfg, l.d
	if err := d.Driver.Preflight(cfg.Bin); err != nil {
		l.logf("%v", err)
		return 1
	}
	v, err := d.Client.Ping(l.ctx)
	if err != nil {
		return l.unreachable(err)
	}
	l.logf("start agent=%s harness=%s server=%s pid=%d version=%s", cfg.AgentName, cfg.Agent, cfg.AifURL, os.Getpid(), d.Version)
	if v != d.Version {
		l.versionWarning = fmt.Sprintf("server version %s differs from connector version %s", v, d.Version)
		l.logf("warning: %s", l.versionWarning)
	}
	as, err := d.Client.WhoAmI(l.ctx)
	var apiErr *APIError
	switch {
	case errors.As(err, &apiErr):
		says := apiErr.Code
		if says == "" {
			says = apiErr.Msg
		}
		l.logf("token is not claimed for %q (server says: %s)", cfg.AgentName, says)
		return 2
	case err != nil:
		return l.unreachable(err)
	case as != cfg.AgentName: // exact: the name keys the state directory
		l.logf("token belongs to %q, config says %q", as, cfg.AgentName)
		return 2
	}

	origin, key, err := OriginKey(cfg.AifURL)
	if err != nil {
		l.logf("config: aifUrl: %v", err)
		return 1
	}
	dir := StateDir(d.Home, cfg.AgentName, key)
	if err := EnsureDir(dir); err != nil {
		l.logf("state: %v", err)
		return 1
	}
	unlock, err := LockDir(dir)
	if errors.Is(err, ErrLocked) {
		l.logf("%s is already running", filepath.Base(dir))
		return 3
	}
	if err != nil {
		l.logf("state: %v", err)
		return 1
	}
	defer unlock()
	st, fresh, err := LoadState(dir, origin)
	if err != nil {
		l.logf("state: %v", err)
		return 1
	}
	l.st = st
	notes, err := LoadNotes(dir)
	if err != nil {
		l.logf("notes: %v", err)
		return 1
	}
	srv, err := d.Control(dir, notes, l.handle)
	if err != nil {
		l.logf("control socket: %v", err)
		return 1
	}
	defer srv.Close()

	if fresh { // first run: only messages posted from now on can control this connector
		_, seq, err := d.Client.Poll(l.ctx, 0)
		if err != nil {
			return l.unreachable(err)
		}
		st.Lock()
		st.LastControl = seq
		l.save()
		st.Unlock()
	}
	st.Lock()
	shutdown := st.Shutdown != nil // the goodbye is still owed: no scan
	st.Unlock()
	if !shutdown { // commits a pending command first, then scans
		shutdown = l.commands(true) == ReasonShutdown
	}

	st.Lock()
	if st.Session != "" && st.Agent != cfg.Agent {
		l.logf("session %s was recorded by %s; starting a fresh session", st.Session, st.Agent)
		st.Session = ""
	}
	st.Unlock()
	drv := d.Driver
	l.launch = driver.Launch{
		Bin: cfg.Bin, Cwd: cfg.Cwd, Model: cfg.Model, Thinking: cfg.Thinking,
		SystemPrompt: Contract(cfg.AgentName, cfg.Thread, cfg.Operators, cfg.SystemPrompt, drv.ToolHint()),
		MCPURL:       origin + "/mcp", MCPToken: cfg.AgentToken,
		Env:         append([]string{"AIF_CONNECT_NAME=" + cfg.AgentName, "AIF_CONNECT_STATE=" + dir}, d.Env...),
		AutoApprove: true, StateDir: dir, Verbose: d.Verbose, Log: d.Log,
	}
	if err := l.start(); err != nil {
		l.logf("harness: %v", err)
		if errors.Is(err, driver.ErrPrerequisite) {
			return 1
		}
		return 4
	}
	st.Lock()
	l.phase = "running"
	st.Unlock()
	first := ReasonStart
	if shutdown {
		first = ReasonShutdown
	}
	code := l.run(first)
	l.kill() // a fatal exit leaves no harness behind
	return code
}

func (l *loop) unreachable(err error) int {
	if l.ctx.Err() != nil {
		return 0 // stopped during startup
	}
	l.logf("aif server %s unreachable: %v", l.cfg.AifURL, err)
	return 5
}

// run is the turn loop proper, from turn 1 on.
func (l *loop) run(reason Reason) int {
	for {
		if l.stopRequested() {
			return l.finish(false)
		}
		if !l.ensure() {
			return 4
		}
		ok := l.turn(reason)
		if reason == ReasonShutdown || l.stopRequested() {
			return l.finish(reason == ReasonShutdown)
		}
		if !ok {
			if l.failures++; l.failures >= failBudget {
				l.logf("turn keeps failing: %s", l.cause)
				return 4
			}
			if !l.ensure() { // at once, so a death while idle is noticed from here on
				return 4
			}
		}
		if r := l.commands(true); r != "" {
			reason, l.recover = r, ""
			continue
		}
		r, code := l.wait()
		if code != 0 {
			return code
		}
		reason = r // "" only when stopping: the loop's first check ends it
	}
}

// ensure restarts a harness that is gone (after a RESET, a crash or a kill); false is fatal.
func (l *loop) ensure() bool {
	if l.alive {
		return true
	}
	if err := l.start(); err != nil {
		l.logf("harness restart failed: %v", err)
		return false
	}
	l.logf("harness restarted")
	return true
}

func (l *loop) stopRequested() bool {
	l.st.Lock()
	defer l.st.Unlock()
	return l.stopReq || l.ctx.Err() != nil
}

// finish stops the harness and ends a stopping connector; after a goodbye turn it clears the
// shutdown record. The session stays for the next run.
func (l *loop) finish(goodbye bool) int {
	l.kill()
	l.logf("stopped; harness stopped")
	if goodbye {
		l.st.Lock()
		l.st.Shutdown = nil
		l.save()
		l.st.Unlock()
	}
	return 0
}

// gate is everything the turn decision looks at.
type gate struct {
	held, recover, queued bool
	polled                bool // n and seq are a poll reply's
	n                     int
	seq, lastInbox        int64
}

// next is the turn decision: nothing while scans are held, then an owed recover turn, then the
// local queue, then the poll gate (news only when seq moved past the last inbox turn's). "" means
// no turn yet.
func next(g gate) Reason {
	switch {
	case g.held:
		return ""
	case g.recover:
		return ReasonRecover
	case g.queued:
		return ReasonWake
	case g.polled && g.n > 0 && g.seq > g.lastInbox:
		return ReasonInbox
	}
	return ""
}

func (l *loop) held() bool { return l.scanFails >= scanBudget }

// decide applies next to the live state; stop reports a stop request instead.
func (l *loop) decide(polled bool, n int, seq int64) (r Reason, stop bool) {
	l.st.Lock()
	stop = l.stopReq || l.ctx.Err() != nil
	queued := len(promptWake(l.st.Wake)) > 0
	l.st.Unlock()
	if stop {
		return "", true
	}
	return next(gate{held: l.held(), recover: l.recover != "", queued: queued, polled: polled, n: n, seq: seq, lastInbox: l.lastInbox}), false
}

// wait is runner.mjs's waitForWork: long-poll, scan when the forum moved past lastControl, turn on
// news, and wait out the rest of the interval after an early answer. A non-zero code is fatal.
func (l *loop) wait() (Reason, int) {
	interval := time.Duration(l.cfg.Interval) * time.Second
	for {
		if r := l.commands(false); r != "" {
			return r, 0
		}
		if r, stop := l.decide(false, 0, 0); stop || r != "" {
			return r, 0
		}
		began := l.d.Now()
		var n int
		var seq int64
		var err error
		if l.idle(func(ctx context.Context) { n, seq, err = l.d.Client.Poll(ctx, min(l.cfg.Interval, pollCap)) }) {
			if code := l.died(); code != 0 {
				return "", code
			}
			continue
		}
		switch {
		case err == nil:
			l.pollErr = ""
			l.st.Lock()
			l.lastPoll = l.d.Now()
			behind := seq > l.st.LastControl
			l.st.Unlock()
			if behind || l.held() {
				if r := l.commands(true); r != "" {
					return r, 0
				}
			}
			if r, stop := l.decide(true, n, seq); stop || r != "" {
				if r == ReasonInbox {
					l.lastInbox = seq
				}
				return r, 0
			}
		case errors.Is(err, context.Canceled):
		case err.Error() != l.pollErr:
			l.pollErr = err.Error()
			l.logf("poll error: %v", err)
		}
		if rest := interval - l.d.Now().Sub(began); rest > 0 {
			if l.idle(func(ctx context.Context) { l.d.Sleep(ctx, rest) }) {
				if code := l.died(); code != 0 {
					return "", code
				}
			}
		}
	}
}

// idle runs f (a poll or a sleep) under a context the control handler cancels on wake or stop,
// while a watcher reads the harness's events; it reports whether the harness died meanwhile. It
// skips f when a stop or an actionable wake is already there.
func (l *loop) idle(f func(context.Context)) (died bool) {
	ctx, cancel := context.WithCancel(l.ctx)
	defer cancel()
	l.st.Lock()
	if l.stopReq || len(l.st.Wake) > 0 && !l.held() {
		l.st.Unlock()
		return false
	}
	l.cancelWait = cancel
	l.st.Unlock()
	dead := make(chan bool, 1)
	go func() { dead <- l.watch(ctx, cancel) }()
	f(ctx)
	cancel()
	l.st.Lock()
	l.cancelWait = nil
	l.st.Unlock()
	return <-dead
}

func (l *loop) watch(ctx context.Context, cancel context.CancelFunc) bool {
	events := l.d.Driver.Events()
	for {
		select {
		case <-ctx.Done():
			return false
		case ev, open := <-events:
			if !open || ev.Kind == driver.Exit {
				cancel()
				return true
			}
			l.logEvent(ev)
		}
	}
}

// died handles a harness death while idle: restart at once, fatal at the third within one interval.
func (l *loop) died() int {
	l.alive = false
	if l.stopRequested() {
		return 0
	}
	now := l.d.Now()
	window := time.Duration(l.cfg.Interval) * time.Second
	l.deaths = append(slices.DeleteFunc(l.deaths, func(t time.Time) bool { return now.Sub(t) >= window }), now)
	if len(l.deaths) >= idleBudget {
		l.logf("harness keeps dying: %d exits while idle within %s", len(l.deaths), window)
		return 4
	}
	if err := l.start(); err != nil {
		l.logf("harness keeps dying: restart failed: %v", err)
		return 4
	}
	l.logf("harness exited while idle; restarted")
	return 0
}

// start starts the harness resuming the saved session. A session other than the saved one is a
// fresh session: recorded, and the contract goes into its first prompt when the harness has no
// system-prompt channel.
func (l *loop) start() error {
	l.st.Lock()
	saved := l.st.Session
	l.st.Unlock()
	launch := l.launch
	launch.SessionID = saved
	// Without cancel: a stop must not kill the harness mid-turn; finish stops it.
	if err := l.d.Driver.Start(context.WithoutCancel(l.ctx), launch); err != nil {
		return err
	}
	l.alive = true
	id := l.d.Driver.SessionID()
	if saved != "" && id == saved {
		return nil
	}
	if saved != "" {
		l.logf("session %s not resumable, started %s", saved, id)
	} else {
		l.logf("session %s started", id)
	}
	l.needContract = !l.d.Driver.HasSystemPromptChannel()
	l.st.Lock()
	l.st.Session, l.st.Agent = id, l.cfg.Agent
	l.save()
	l.st.Unlock()
	return nil
}

// kill stops a live harness and waits briefly for its Exit event, so a restart starts clean.
func (l *loop) kill() {
	if !l.alive {
		return
	}
	l.alive = false
	_ = l.d.Driver.Stop(context.WithoutCancel(l.ctx))
	events := l.d.Driver.Events()
	t := time.NewTimer(drainWait)
	defer t.Stop()
	for {
		select {
		case ev, open := <-events:
			if !open || ev.Kind == driver.Exit {
				return
			}
		case <-t.C:
			return
		}
	}
}

// turn runs one turn and records its outcome (design doc, "### Turn outcome"); it reports ok.
func (l *loop) turn(reason Reason) bool {
	l.st.Lock()
	l.st.Turn++
	in := PromptInput{Turn: l.st.Turn, Reason: reason, Wake: promptWake(l.st.Wake),
		Shutdown: l.st.Shutdown, FreshSession: l.st.FreshSession, Goal: l.cfg.Goal}
	if reason == ReasonRecover {
		in.Control = l.recover
	}
	l.reason, l.busy = string(reason), true
	l.save()
	l.st.Unlock()

	l.logf("turn=%d start reason=%s", in.Turn, reason)
	began := l.d.Now()
	cause, control := "", ""
	text, err := Prompt(in)
	if err != nil {
		cause, control = err.Error(), RecoverFailed(err.Error())
	} else {
		if l.needContract {
			text = l.launch.SystemPrompt + "\n\n" + text
		}
		cause, control = l.exec(text, in.Turn)
	}

	outcome := "ok"
	switch {
	case cause != "" && (reason == ReasonShutdown || l.stopRequested()):
		outcome = "cancelled"
	case cause != "":
		outcome = "failed"
	}
	l.st.Lock()
	l.busy = false
	if cause == "" {
		ids := make([]string, len(in.Wake))
		for i, w := range in.Wake {
			ids[i] = w.ID
		}
		l.st.Dequeue(ids...)
		if reason != ReasonShutdown { // the shutdown turn never carried the reset note
			l.st.FreshSession = nil
		}
		l.save()
	}
	l.st.Unlock()
	if cause == "" {
		l.failures, l.recover = 0, ""
		l.logf("turn=%d end reason=%s outcome=ok duration=%s", in.Turn, reason, l.d.Now().Sub(began).Round(time.Millisecond))
	} else {
		l.recover, l.cause = control, cause
		l.logf("turn=%d end reason=%s outcome=%s duration=%s cause=%q", in.Turn, reason, outcome, l.d.Now().Sub(began).Round(time.Millisecond), cause)
	}
	return cause == ""
}

// exec sends the prompt and reads events until the turn ends; a failure returns its cause and the
// recover turn's control text.
func (l *loop) exec(text string, turn int64) (cause, control string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(l.ctx), l.cfg.TurnTimeout)
	defer cancel()
	timeout := func() (string, string) {
		after := short(l.cfg.TurnTimeout)
		l.logf("turn=%d timeout after %s", turn, after)
		l.kill()
		return "timeout after " + after, RecoverTimeout
	}
	events := l.d.Driver.Events()
	if err := l.d.Driver.Prompt(ctx, text); err != nil {
		if ctx.Err() != nil {
			return timeout()
		}
		l.kill() // a harness that cannot take a prompt is in no known state: restart it
		return "prompt: " + err.Error(), RecoverFailed(err.Error())
	}
	l.needContract = false
	for {
		select {
		case ev, open := <-events:
			switch {
			case !open || ev.Kind == driver.Exit:
				l.alive = false
				return "harness died", RecoverCrash
			case ev.Kind == driver.TurnEnd && ev.OK:
				return "", ""
			case ev.Kind == driver.TurnEnd:
				if ev.Err == "" {
					ev.Err = "turn failed"
				}
				return ev.Err, RecoverFailed(ev.Err)
			}
			l.logEvent(ev)
		case <-ctx.Done():
			return timeout()
		}
	}
}

func (l *loop) logEvent(ev driver.Event) {
	switch ev.Kind {
	case driver.Text:
		l.logf("model: %s", strings.Join(strings.Fields(ev.Text), " "))
	case driver.ToolCall:
		l.logf("tool: %s", strings.Join(strings.Fields(ev.Text), " "))
	}
}

// short renders a duration the way an operator writes it: 30m, 1h, 90s.
func short(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}

// handle answers the control socket's status, stop and wake.
func (l *loop) handle(r Request) Response {
	l.st.Lock()
	defer l.st.Unlock()
	switch r.Op {
	case "status":
		state := "idle"
		switch {
		case l.stopReq || l.ctx.Err() != nil || l.st.Shutdown != nil:
			state = "stopping"
		case l.busy:
			state = "turn"
		}
		lastPoll := ""
		if !l.lastPoll.IsZero() {
			lastPoll = l.lastPoll.Format(time.RFC3339)
		}
		return Response{OK: true, Status: &Status{PID: os.Getpid(), Agent: l.cfg.AgentName, Harness: l.cfg.Agent,
			Phase: l.phase, Turn: l.st.Turn, Reason: l.reason, State: state, LastPoll: lastPoll,
			Session: l.st.Session, VersionWarning: l.versionWarning}}
	case "stop":
		if !l.stopReq {
			l.stopReq = true
			l.logf("stop requested")
		}
		l.interrupt()
		return Response{OK: true}
	case "wake":
		if r.Text == "" {
			return Response{Error: "wake needs a message"}
		}
		id, err := l.st.Enqueue(r.Text)
		if err != nil {
			return Response{Error: err.Error()}
		}
		if err := l.st.Save(); err != nil {
			l.st.Dequeue(id)
			return Response{Error: err.Error()}
		}
		l.logf("wake queued id=%s", id)
		l.interrupt()
		return Response{OK: true, Text: id}
	}
	return Response{Error: "unknown op"}
}

// interrupt ends the current idle poll or sleep; the caller holds st's lock.
func (l *loop) interrupt() {
	if l.cancelWait != nil {
		l.cancelWait()
	}
}
