package connect

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dshein-alt/aif/internal/connect/driver"
)

// step scripts one prompt: the events the fake harness emits for it, or Prompt's error. It may
// act on the connector through e (wake, stop) while the turn runs.
type step func(e *env) ([]driver.Event, error)

var (
	okStep   step = func(*env) ([]driver.Event, error) { return []driver.Event{{Kind: driver.TurnEnd, OK: true}}, nil }
	hangStep step = func(*env) ([]driver.Event, error) { return nil, nil }
	dieStep  step = func(*env) ([]driver.Event, error) { return []driver.Event{{Kind: driver.Exit}}, nil }
	// blockStep is a Prompt stuck writing to a harness that does not read its stdin: only Stop frees it.
	blockStep step = func(e *env) ([]driver.Event, error) {
		e.drv.mu.Lock()
		halt := e.drv.halt
		e.drv.mu.Unlock()
		<-halt
		return nil, errors.New("write |1: broken pipe")
	}
)

func errStep(msg string) step {
	return func(*env) ([]driver.Event, error) { return []driver.Event{{Kind: driver.TurnEnd, Err: msg}}, nil }
}

// then runs f, then behaves like s.
func then(f func(e *env), s step) step {
	return func(e *env) ([]driver.Event, error) { f(e); return s(e) }
}

type fakeDriver struct {
	e          *env
	mu         sync.Mutex
	ch         chan driver.Event
	halt       chan struct{} // closed by Stop
	id         string
	ids        []string // SessionID after each Start; default: resume, else "sess-<n>"
	launches   []driver.Launch
	prompts    []string
	steps      []step
	stops      int
	sysChannel bool
	preErr     error
	startErr   error
}

func (f *fakeDriver) Preflight(string) error { f.e.rec("preflight"); return f.preErr }
func (f *fakeDriver) Start(_ context.Context, l driver.Launch) error {
	f.e.rec("start")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return f.startErr
	}
	f.launches = append(f.launches, l)
	f.ch, f.halt = make(chan driver.Event, 64), make(chan struct{})
	switch {
	case len(f.ids) > 0:
		f.id, f.ids = f.ids[0], f.ids[1:]
	case l.SessionID != "":
		f.id = l.SessionID
	default:
		f.id = fmt.Sprintf("sess-%d", len(f.launches))
	}
	return nil
}
func (f *fakeDriver) Prompt(_ context.Context, text string) error {
	f.e.rec("prompt")
	f.mu.Lock()
	f.prompts = append(f.prompts, text)
	s := okStep
	if len(f.steps) > 0 {
		s, f.steps = f.steps[0], f.steps[1:]
	}
	f.mu.Unlock()
	evs, err := s(f.e)
	for _, ev := range evs {
		f.emit(ev)
	}
	return err
}
func (f *fakeDriver) emit(ev driver.Event) {
	f.mu.Lock()
	ch := f.ch
	f.mu.Unlock()
	ch <- ev
}
func (f *fakeDriver) Events() <-chan driver.Event { f.mu.Lock(); defer f.mu.Unlock(); return f.ch }
func (f *fakeDriver) SessionID() string           { f.mu.Lock(); defer f.mu.Unlock(); return f.id }
func (f *fakeDriver) Stop(context.Context) error {
	f.e.rec("stop")
	f.mu.Lock()
	f.stops++
	ch, halt := f.ch, f.halt
	f.halt = nil
	f.mu.Unlock()
	if halt != nil {
		close(halt)
	}
	for { // the contract: Stop drains the process's events, its Exit included
		select {
		case <-ch:
		default:
			return nil
		}
	}
}
func (f *fakeDriver) ToolHint() string             { return "HINT" }
func (f *fakeDriver) HasSystemPromptChannel() bool { return f.sysChannel }

type pollFn func(ctx context.Context, e *env) (int, int64, error)
type feedFn func(since int64) ([]Message, int64, error)

func ans(n int, seq int64) pollFn {
	return func(context.Context, *env) (int, int64, error) { return n, seq, nil }
}

type scriptAIF struct {
	e       *env
	v       string
	pingErr error
	as      string
	whoErr  error
	polls   []pollFn
	feeds   []feedFn
	waits   []int
	sinces  []int64
}

func (a *scriptAIF) Ping(context.Context) (string, error) { a.e.rec("ping"); return a.v, a.pingErr }
func (a *scriptAIF) WhoAmI(context.Context) (string, error) {
	a.e.rec("whoami")
	return a.as, a.whoErr
}

// Poll answers from the script; past its end it blocks until ctx is done and tells the test the
// connector is idle.
func (a *scriptAIF) Poll(ctx context.Context, wait int) (int, int64, error) {
	a.e.rec("poll")
	a.e.mu.Lock()
	a.waits = append(a.waits, wait)
	var p pollFn
	if len(a.polls) > 0 {
		p, a.polls = a.polls[0], a.polls[1:]
	}
	a.e.mu.Unlock()
	if p != nil {
		return p(ctx, a.e)
	}
	select {
	case a.e.idle <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return 0, 0, ctx.Err()
}

// Feed answers from the script; past its end it is an empty batch at since.
func (a *scriptAIF) Feed(_ context.Context, since int64) ([]Message, int64, error) {
	a.e.rec("feed")
	a.e.mu.Lock()
	a.sinces = append(a.sinces, since)
	var f feedFn
	if len(a.feeds) > 0 {
		f, a.feeds = a.feeds[0], a.feeds[1:]
	}
	a.e.mu.Unlock()
	if f == nil {
		return nil, since, nil
	}
	return f(since)
}

type stubServer struct{ e *env }

func (s stubServer) Close() { s.e.rec("close") }

type env struct {
	t    *testing.T
	cfg  Config
	home string
	aif  *scriptAIF
	drv  *fakeDriver
	idle chan struct{}

	mu      sync.Mutex
	calls   []string
	logs    []string
	h       Handler
	clock   time.Time
	sleeps  []time.Duration
	onSleep func(ctx context.Context) // runs in Sleep; nil returns at once
}

func newEnv(t *testing.T) *env {
	e := &env{t: t, home: t.TempDir(), idle: make(chan struct{}, 1), clock: time.Unix(1_000_000, 0)}
	e.cfg = Config{Agent: "pi", Bin: "/bin/pi", Goal: "be useful", AifURL: "http://aif.test:80", AgentName: "mybot",
		AgentToken: "tok", Thread: 3, Operators: []string{"root"}, Interval: 60, TurnTimeout: time.Minute, Cwd: t.TempDir()}
	e.aif = &scriptAIF{e: e, v: "1.0", as: "mybot"}
	e.drv = &fakeDriver{e: e}
	return e
}

func (e *env) rec(call string) { e.mu.Lock(); e.calls = append(e.calls, call); e.mu.Unlock() }

func (e *env) dir() string {
	_, key, _ := OriginKey(e.cfg.AifURL)
	return StateDir(e.home, e.cfg.AgentName, key)
}

// seed writes state.json before the run; Origin is filled in.
func (e *env) seed(st *State) {
	e.t.Helper()
	if err := EnsureDir(e.dir()); err != nil {
		e.t.Fatal(err)
	}
	st.dir, st.Origin = e.dir(), "http://aif.test:80"
	if err := st.Save(); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) state() *State {
	e.t.Helper()
	st, _, err := LoadState(e.dir(), "http://aif.test:80")
	if err != nil {
		e.t.Fatal(err)
	}
	return st
}

func (e *env) call(r Request) Response {
	e.mu.Lock()
	h := e.h
	e.mu.Unlock()
	return h(r)
}

func (e *env) deps() Deps {
	return Deps{
		Client: e.aif, Driver: e.drv, Home: e.home, Version: "1.0",
		Control: func(dir string, notes *Notes, h Handler) (ControlServer, error) {
			e.rec("serve")
			e.mu.Lock()
			e.h = h
			e.mu.Unlock()
			return stubServer{e}, nil
		},
		Log: func(s string) { e.mu.Lock(); e.logs = append(e.logs, s); e.mu.Unlock() },
		Now: func() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.clock },
		Sleep: func(ctx context.Context, d time.Duration) {
			e.mu.Lock()
			e.sleeps = append(e.sleeps, d)
			e.clock = e.clock.Add(d)
			f := e.onSleep
			e.mu.Unlock()
			if f != nil {
				f(ctx)
			}
		},
	}
}

func (e *env) advance(d time.Duration) { e.mu.Lock(); e.clock = e.clock.Add(d); e.mu.Unlock() }

// run runs the connector to its exit code; ctx may be nil.
func (e *env) run(ctx context.Context) int {
	e.t.Helper()
	if ctx == nil {
		ctx = context.Background()
	}
	done := make(chan int, 1)
	go func() { done <- Run(ctx, e.cfg, e.deps()) }()
	select {
	case code := <-done:
		return code
	case <-time.After(10 * time.Second):
		e.t.Fatalf("Run did not return; log:\n%s", strings.Join(e.logLines(), "\n"))
		return -1
	}
}

// runUntilIdle runs the connector, stops it once it blocks in an idle poll, and returns its code.
func (e *env) runUntilIdle() int {
	e.t.Helper()
	go func() {
		<-e.idle
		e.call(Request{Op: "stop"})
	}()
	return e.run(nil)
}

func (e *env) logLines() []string { e.mu.Lock(); defer e.mu.Unlock(); return slices.Clone(e.logs) }

func (e *env) logged(sub string) bool {
	return slices.ContainsFunc(e.logLines(), func(l string) bool { return strings.Contains(l, sub) })
}

func (e *env) wantLog(sub string) {
	e.t.Helper()
	if !e.logged(sub) {
		e.t.Errorf("no log line containing %q; log:\n%s", sub, strings.Join(e.logLines(), "\n"))
	}
}

func (e *env) prompts() []string {
	e.drv.mu.Lock()
	defer e.drv.mu.Unlock()
	return slices.Clone(e.drv.prompts)
}

func (e *env) wantCode(got, want int) {
	e.t.Helper()
	if got != want {
		e.t.Fatalf("exit code %d, want %d; log:\n%s", got, want, strings.Join(e.logLines(), "\n"))
	}
}

func stopStep(s step) step { return then(func(e *env) { e.call(Request{Op: "stop"}) }, s) }

// ---- startup ----

func TestStartupOrderFirstRun(t *testing.T) {
	e := newEnv(t)
	e.aif.polls = []pollFn{ans(0, 42)} // the first-run poll
	e.drv.steps = []step{stopStep(okStep)}
	e.wantCode(e.run(nil), 0)
	want := []string{"preflight", "ping", "whoami", "serve", "poll", "feed", "start", "prompt", "stop", "close"}
	if !slices.Equal(e.calls, want) {
		t.Errorf("calls %v, want %v", e.calls, want)
	}
	if e.aif.sinces[0] != 42 || e.aif.waits[0] != 0 {
		t.Errorf("first-run poll wait %d, scan since %d; want 0 and 42", e.aif.waits[0], e.aif.sinces[0])
	}
	st := e.state()
	if st.LastControl != 42 || st.Turn != 1 || st.Session != "sess-1" || st.Agent != "pi" {
		t.Errorf("state %+v", st)
	}
	p := e.prompts()[0]
	if !strings.HasPrefix(p, "RESIDENT CONTRACT") || !strings.Contains(p, "Turn 1, woken because first turn.") {
		t.Errorf("first prompt of a fresh session without a system-prompt channel lacks the contract:\n%s", p)
	}
	l := e.drv.launches[0]
	if l.MCPURL != "http://aif.test:80/mcp" || l.MCPToken != "tok" || l.StateDir != e.dir() || !l.AutoApprove ||
		!slices.Contains(l.Env, "AIF_CONNECT_NAME=mybot") || !strings.Contains(l.SystemPrompt, "HINT") {
		t.Errorf("launch %+v", l)
	}
	e.wantLog("turn=1 start reason=start")
	e.wantLog("turn=1 end reason=start outcome=ok")
	e.wantLog("exit code=0")
}

func TestStartupVersionWarningAndStatus(t *testing.T) {
	e := newEnv(t)
	e.seed(&State{})
	e.aif.v = "9.9"
	var st *Status
	e.drv.steps = []step{stopStep(then(func(e *env) { st = e.call(Request{Op: "status"}).Status }, okStep))}
	e.wantCode(e.run(nil), 0)
	e.wantLog("warning: server version 9.9 differs from connector version 1.0")
	if st == nil || st.VersionWarning == "" || st.Phase != "running" || st.State != "stopping" || st.Turn != 1 ||
		st.Reason != "start" || st.Session != "sess-1" || st.Agent != "mybot" || st.Harness != "pi" || st.PID != os.Getpid() {
		t.Errorf("status %+v", st)
	}
	if r := e.call(Request{Op: "bogus"}); r.OK || r.Error != "unknown op" {
		t.Errorf("unknown op: %+v", r)
	}
}

func TestStartupExitCodes(t *testing.T) {
	cases := []struct {
		name string
		set  func(e *env)
		code int
		log  string
	}{
		{"prerequisite", func(e *env) { e.drv.preErr = fmt.Errorf("%w: pi has no --mcp-config", driver.ErrPrerequisite) }, 1, "pi has no --mcp-config"},
		{"start prerequisite", func(e *env) { e.drv.startErr = driver.ErrPrerequisite }, 1, "harness:"},
		{"wrong name", func(e *env) { e.aif.as = "other" }, 2, `token belongs to "other", config says "mybot"`},
		{"unclaimed", func(e *env) { e.aif.whoErr = &APIError{Status: 403, Code: "claim_required"} }, 2,
			`token is not claimed for "mybot" (server says: claim_required)`},
		{"locked", func(e *env) {
			EnsureDir(e.dir())
			unlock, err := LockDir(e.dir())
			if err != nil {
				e.t.Fatal(err)
			}
			e.t.Cleanup(func() { unlock() })
		}, 3, "is already running"},
		{"start fails", func(e *env) { e.drv.startErr = errors.New("exec: no such file") }, 4, "no such file"},
		{"ping unreachable", func(e *env) { e.aif.pingErr = ErrUnreachable }, 5, "unreachable"},
		{"whoami unreachable", func(e *env) { e.aif.whoErr = ErrUnreachable }, 5, "unreachable"},
		{"origin mismatch", func(e *env) {
			EnsureDir(e.dir())
			os.WriteFile(filepath.Join(e.dir(), "state.json"), []byte(`{"origin":"https://elsewhere:443"}`), 0o600)
		}, 1, "holds origin"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.seed(&State{})
			c.set(e)
			e.wantCode(e.run(nil), c.code)
			e.wantLog(c.log)
			if len(e.drv.prompts) != 0 {
				t.Errorf("a turn ran")
			}
		})
	}
}

// ---- the poll gate ----

func TestNext(t *testing.T) {
	cases := []struct {
		g    gate
		want Reason
	}{
		{gate{}, ""},
		{gate{held: true, recover: true, queued: true, polled: true, n: 1, seq: 5}, ""},
		{gate{recover: true, queued: true}, ReasonRecover},
		{gate{queued: true, polled: true, n: 1, seq: 5}, ReasonWake},
		{gate{polled: true, n: 1, seq: 5, lastInbox: 4}, ReasonInbox},
		{gate{polled: true, n: 1, seq: 5, lastInbox: 5}, ""},
		{gate{polled: true, n: 0, seq: 9}, ""},
		{gate{n: 1, seq: 5}, ""}, // not polled yet
	}
	for _, c := range cases {
		if got := next(c.g); got != c.want {
			t.Errorf("next(%+v) = %q, want %q", c.g, got, c.want)
		}
	}
}

func TestPollGate(t *testing.T) {
	e := newEnv(t)
	e.seed(&State{LastControl: 100})
	e.aif.polls = []pollFn{
		ans(1, 100), // the start turn read the inbox up to the startup scan's seq: no turn
		ans(1, 101), // news: inbox turn
		ans(1, 101), // same seq: no turn, waits out the interval
		func(_ context.Context, e *env) (int, int64, error) {
			e.advance(20 * time.Second)
			return 0, 0, errors.New("boom")
		},
		func(_ context.Context, e *env) (int, int64, error) {
			e.advance(20 * time.Second)
			return 0, 0, errors.New("boom")
		},
		ans(1, 102), // moved: inbox turn
	}
	e.drv.steps = []step{okStep, okStep, stopStep(okStep)}
	e.wantCode(e.run(nil), 0)
	ps := e.prompts()
	if len(ps) != 3 || !strings.Contains(ps[1], "woken because "+reasonText[ReasonInbox]) {
		t.Fatalf("prompts %q", ps)
	}
	if want := []time.Duration{time.Minute, time.Minute, 40 * time.Second, 40 * time.Second}; !slices.Equal(e.sleeps, want) {
		t.Errorf("sleeps %v, want %v", e.sleeps, want)
	}
	if n := strings.Count(strings.Join(e.logLines(), "\n"), "poll error: boom"); n != 1 {
		t.Errorf("poll error logged %d times, want once", n)
	}
	for _, w := range e.aif.waits {
		if w != 60 {
			t.Errorf("poll wait %d, want 60", w)
		}
	}
}

func TestPollScansOnlyWhenBehind(t *testing.T) {
	e := newEnv(t)
	e.seed(&State{LastControl: 10})
	e.aif.feeds = []feedFn{nil, nil, func(int64) ([]Message, int64, error) { return nil, 12, nil }}
	e.aif.polls = []pollFn{ans(0, 10), ans(0, 12), ans(0, 12)}
	e.wantCode(e.runUntilIdle(), 0)
	// startup scan, after-turn scan, then only the poll whose seq passed lastControl
	if want := []int64{10, 10, 10}; !slices.Equal(e.aif.sinces, want) {
		t.Errorf("feed sinces %v, want %v", e.aif.sinces, want)
	}
	if st := e.state(); st.LastControl != 12 {
		t.Errorf("lastControl %d, want 12", st.LastControl)
	}
}

// ---- wake ----

func TestWakeDuringTurn(t *testing.T) {
	e := newEnv(t)
	e.seed(&State{})
	var id string
	e.drv.steps = []step{
		then(func(e *env) { id = e.call(Request{Op: "wake", Text: "do the thing"}).Text }, okStep),
		errStep("overloaded"), // the wake turn fails: the message stays queued
		stopStep(okStep),      // the recover turn delivers it
	}
	e.wantCode(e.run(nil), 0)
	ps := e.prompts()
	line := "Operator message " + id + ": do the thing"
	if len(ps) != 3 || strings.Contains(ps[0], line) || !strings.Contains(ps[1], "woken because "+reasonText[ReasonWake]) ||
		!strings.Contains(ps[1], line) || !strings.Contains(ps[2], line) || !strings.Contains(ps[2], RecoverFailed("overloaded")) {
		t.Fatalf("prompts %q", ps)
	}
	if st := e.state(); len(st.Wake) != 0 {
		t.Errorf("wake queue %+v, want empty after the ok turn", st.Wake)
	}
	if r := e.call(Request{Op: "wake"}); r.OK {
		t.Errorf("empty wake accepted")
	}
}

func TestWakeWhileIdle(t *testing.T) {
	e := newEnv(t)
	e.seed(&State{})
	e.drv.steps = []step{okStep, stopStep(okStep)}
	go func() {
		<-e.idle
		e.call(Request{Op: "wake", Text: "hi"})
	}()
	e.wantCode(e.run(nil), 0)
	if ps := e.prompts(); len(ps) != 2 || !strings.Contains(ps[1], ": hi") {
		t.Fatalf("prompts %q", ps)
	}
}

// ---- outcomes ----

func TestOutcomesAndFailureBudget(t *testing.T) {
	e := newEnv(t)
	e.seed(&State{})
	wake := func(e *env) { e.call(Request{Op: "wake", Text: "again"}) }
	e.drv.steps = []step{
		errStep("model overloaded"), // failed: recover with its cause
		dieStep,                     // the harness died: restarted, recover with RecoverCrash
		then(wake, okStep),          // ok resets the count
		errStep("e1"), errStep("e2"), errStep("e3"),
	}
	e.wantCode(e.run(nil), 4)
	ps := e.prompts()
	if len(ps) != 6 {
		t.Fatalf("%d prompts, want 6", len(ps))
	}
	if !strings.Contains(ps[1], RecoverFailed("model overloaded")) || !strings.Contains(ps[2], RecoverCrash) ||
		!strings.Contains(ps[3], "woken because "+reasonText[ReasonWake]) || !strings.Contains(ps[5], RecoverFailed("e2")) {
		t.Errorf("prompts %q", ps)
	}
	if len(e.drv.launches) != 2 || e.drv.launches[1].SessionID != "sess-1" {
		t.Errorf("launches %+v: want one restart resuming sess-1", e.drv.launches)
	}
	if e.drv.stops != 1 {
		t.Errorf("harness stopped %d times: a fatal exit must stop the live harness once", e.drv.stops)
	}
	e.wantLog("turn keeps failing: e3")
	e.wantLog(`outcome=failed duration=0s cause="model overloaded"`)
}

func TestPromptErrorRestarts(t *testing.T) {
	e := newEnv(t)
	e.seed(&State{})
	e.drv.steps = []step{func(*env) ([]driver.Event, error) { return nil, errors.New("broken pipe") }, stopStep(okStep)}
	e.wantCode(e.run(nil), 0)
	if ps := e.prompts(); !strings.Contains(ps[1], RecoverFailed("broken pipe")) {
		t.Errorf("recover prompt %q", ps[1])
	}
	if e.drv.stops != 2 || len(e.drv.launches) != 2 {
		t.Errorf("stops %d launches %d: want the harness killed and restarted", e.drv.stops, len(e.drv.launches))
	}
}

func TestTurnTimeout(t *testing.T) {
	e := newEnv(t)
	e.seed(&State{Session: "s1", Agent: "pi"})
	e.cfg.TurnTimeout = 20 * time.Millisecond
	e.drv.steps = []step{hangStep, hangStep, hangStep}
	e.wantCode(e.run(nil), 4)
	ps := e.prompts()
	if len(ps) != 3 || !strings.Contains(ps[1], RecoverTimeout) {
		t.Fatalf("prompts %q", ps)
	}
	e.wantLog("turn=1 timeout after 20ms")
	e.wantLog("turn keeps failing: timeout after 20ms")
	if e.drv.stops != 4 || len(e.drv.launches) != 3 { // the fatal exit stops the dead harness too: its cleanup
		t.Errorf("stops %d launches %d: want each timed-out harness killed and restarted", e.drv.stops, len(e.drv.launches))
	}
	for i, l := range e.drv.launches {
		if l.SessionID != "s1" {
			t.Errorf("launch %d resumes %q, want s1", i, l.SessionID)
		}
	}
}

func TestCancelledWhenStopping(t *testing.T) {
	e := newEnv(t)
	e.seed(&State{})
	e.drv.steps = []step{stopStep(errStep("boom"))}
	e.wantCode(e.run(nil), 0)
	if len(e.prompts()) != 1 {
		t.Errorf("a recover turn ran while stopping")
	}
	e.wantLog("outcome=cancelled")
}

// ---- stop ----

func TestStopDuringTurn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := newEnv(t)
	e.seed(&State{Session: "s1", Agent: "pi", FreshSession: &Ref{From: "root", Msg: 3}})
	e.drv.steps = []step{then(func(*env) { cancel() }, okStep)} // SIGTERM mid-turn
	e.wantCode(e.run(ctx), 0)
	st := e.state()
	if st.Session != "s1" || st.FreshSession != nil {
		t.Errorf("state %+v: want the session kept and the ok turn recorded", st)
	}
	if e.drv.stops != 1 {
		t.Errorf("harness stopped %d times", e.drv.stops)
	}
}

func TestStopWhileIdle(t *testing.T) {
	e := newEnv(t)
	e.seed(&State{Session: "s1", Agent: "pi"})
	e.wantCode(e.runUntilIdle(), 0)
	if len(e.prompts()) != 1 || e.drv.stops != 1 || e.state().Session != "s1" {
		t.Errorf("prompts %d stops %d session %q", len(e.prompts()), e.drv.stops, e.state().Session)
	}
	e.wantLog("stop requested")
}

// ---- commands ----

func msg(id int64, author, body string) Message {
	return Message{ID: id, Thread: 3, Author: author, Body: body}
}

func batch(seq int64, ms ...Message) feedFn {
	return func(int64) ([]Message, int64, error) { return ms, seq, nil }
}

func TestResetAfterTurn(t *testing.T) {
	e := newEnv(t)
	e.seed(&State{Session: "s-old", Agent: "pi", LastControl: 5})
	e.aif.feeds = []feedFn{nil, batch(7, msg(6, "david", "#CMD[RESET]#"), msg(7, "Root", "@mybot #CMD[RESET]#"))}
	e.drv.steps = []step{okStep, stopStep(okStep)}
	e.wantCode(e.run(nil), 0)
	ps := e.prompts()
	if len(ps) != 2 || strings.HasPrefix(ps[0], "RESIDENT CONTRACT") {
		t.Fatalf("prompts %q", ps)
	}
	if !strings.HasPrefix(ps[1], "RESIDENT CONTRACT") || !strings.Contains(ps[1], "woken because first turn") ||
		!strings.Contains(ps[1], "reset by Root (message 7)") {
		t.Errorf("turn after RESET:\n%s", ps[1])
	}
	if len(e.drv.launches) != 2 || e.drv.launches[1].SessionID != "" {
		t.Errorf("launches %+v: want a restart on a fresh session", e.drv.launches)
	}
	st := e.state()
	if st.LastControl != 7 || st.Session != "sess-2" || st.FreshSession != nil || st.Pending != nil {
		t.Errorf("state %+v", st)
	}
	e.wantLog("control=RESET from=Root msg=7")
}

func TestShutdownCommand(t *testing.T) {
	e := newEnv(t)
	e.seed(&State{Session: "s1", Agent: "pi"})
	e.aif.polls = []pollFn{ans(0, 9)}
	e.aif.feeds = []feedFn{nil, nil, batch(9, msg(8, "root", "#CMD[SHUTDOWN]#"), msg(9, "root", "#CMD[RESET]# #CMD[PAUSE]#"))}
	e.wantCode(e.run(nil), 0)
	ps := e.prompts()
	if len(ps) != 2 || !strings.Contains(ps[1], "SHUTDOWN received from root (message 9)") {
		t.Fatalf("prompts %q", ps)
	}
	st := e.state()
	if st.Shutdown != nil || st.LastControl != 9 || st.Session != "s1" {
		t.Errorf("state %+v", st)
	}
	e.wantLog("control=SHUTDOWN from=root msg=9")
	e.wantLog("unknown command PAUSE ignored")
}

func TestPendingCommittedAtStartup(t *testing.T) {
	e := newEnv(t)
	e.seed(&State{Session: "s1", Agent: "pi", LastControl: 4, Pending: &Pending{Action: "RESET", From: "root", Msg: 9}})
	e.drv.steps = []step{stopStep(okStep)}
	e.wantCode(e.run(nil), 0)
	if e.aif.sinces[0] != 9 {
		t.Errorf("startup scan since %d, want 9 (the commit comes first)", e.aif.sinces[0])
	}
	if e.drv.launches[0].SessionID != "" || !strings.Contains(e.prompts()[0], "reset by root (message 9)") {
		t.Errorf("launch %+v prompt %q", e.drv.launches[0], e.prompts()[0])
	}
	if st := e.state(); st.Pending != nil || st.FreshSession != nil {
		t.Errorf("state %+v", st)
	}
}

func TestShutdownRecordAtStartup(t *testing.T) {
	e := newEnv(t)
	e.seed(&State{Session: "s1", Agent: "pi", Shutdown: &Ref{From: "root", Msg: 4}})
	e.drv.steps = []step{errStep("api down")} // a failed goodbye still exits cleanly
	e.wantCode(e.run(nil), 0)
	if slices.Contains(e.calls, "feed") {
		t.Errorf("scanned with a shutdown record: %v", e.calls)
	}
	if ps := e.prompts(); len(ps) != 1 || !strings.Contains(ps[0], "SHUTDOWN received from root (message 4)") {
		t.Errorf("prompts %q", ps)
	}
	if e.state().Shutdown != nil {
		t.Errorf("shutdown record kept")
	}
	e.wantLog("outcome=cancelled")
}

func TestScanHold(t *testing.T) {
	e := newEnv(t)
	e.seed(&State{})
	fail := func(int64) ([]Message, int64, error) { e.rec("feed-failed"); return nil, 0, ErrUnreachable }
	e.aif.feeds = []feedFn{fail, fail, fail, nil}
	e.aif.polls = []pollFn{ans(1, 5), ans(1, 5)}
	e.drv.steps = []step{okStep, stopStep(okStep)}
	e.wantCode(e.run(nil), 0)
	// the start turn goes ahead after one failure; the inbox turn waits for a scan that succeeds
	var seq []string
	for _, c := range e.calls {
		if c == "prompt" || c == "feed-failed" || c == "poll" {
			seq = append(seq, c)
		}
	}
	want := []string{"feed-failed", "prompt", "feed-failed", "poll", "feed-failed", "poll", "prompt"}
	if !slices.Equal(seq, want) {
		t.Errorf("calls %v, want %v", seq, want)
	}
	e.wantLog("new turns held")
	e.wantLog("turns resume")
}

// A hold with nothing new on the forum (seq == lastControl) still retries the scan on every poll.
func TestScanHoldRetriesWithoutNews(t *testing.T) {
	e := newEnv(t)
	e.seed(&State{LastControl: 5})
	fail := func(int64) ([]Message, int64, error) { return nil, 0, ErrUnreachable }
	e.aif.feeds = []feedFn{fail, fail, fail}
	e.aif.polls = []pollFn{ans(1, 5), ans(0, 5)} // inbox turn (not held yet), then a quiet poll while held
	e.wantCode(e.runUntilIdle(), 0)
	if len(e.prompts()) != 2 || len(e.aif.sinces) != 4 {
		t.Errorf("prompts %d feeds %d; want 2 and 4", len(e.prompts()), len(e.aif.sinces))
	}
	e.wantLog("turns resume")
}

func TestLocalResetViaWake(t *testing.T) {
	e := newEnv(t)
	e.seed(&State{Session: "s1", Agent: "pi", LastControl: 7})
	var hi string
	e.drv.steps = []step{
		then(func(e *env) {
			e.call(Request{Op: "wake", Text: "#CMD[RESET]#"})
			hi = e.call(Request{Op: "wake", Text: "hi"}).Text
		}, okStep),
		stopStep(okStep),
	}
	e.wantCode(e.run(nil), 0)
	ps := e.prompts()
	if len(ps) != 2 || !strings.Contains(ps[1], "reset by a local operator") || !strings.Contains(ps[1], "Operator message "+hi+": hi") ||
		strings.Count(ps[1], "Operator message ") != 1 {
		t.Fatalf("prompts %q", ps)
	}
	st := e.state()
	if st.LastControl != 7 || len(st.Wake) != 0 || st.Session != "sess-2" {
		t.Errorf("state %+v", st)
	}
	e.wantLog("control=RESET from=local")
}

// ---- sessions ----

func TestSessionNotResumable(t *testing.T) {
	for _, sys := range []bool{false, true} {
		e := newEnv(t)
		e.seed(&State{Session: "s-old", Agent: "pi"})
		e.drv.ids = []string{"s-new"}
		e.drv.sysChannel = sys
		e.drv.steps = []step{stopStep(okStep)}
		e.wantCode(e.run(nil), 0)
		e.wantLog("session s-old not resumable, started s-new")
		if e.state().Session != "s-new" {
			t.Errorf("session %q", e.state().Session)
		}
		if got := strings.HasPrefix(e.prompts()[0], "RESIDENT CONTRACT"); got == sys {
			t.Errorf("system-prompt channel %v: contract prepended %v", sys, got)
		}
	}
}

func TestSessionOfAnotherHarness(t *testing.T) {
	e := newEnv(t)
	e.seed(&State{Session: "s-claude", Agent: "claude"})
	e.drv.steps = []step{stopStep(okStep)}
	e.wantCode(e.run(nil), 0)
	e.wantLog("session s-claude was recorded by claude")
	if e.drv.launches[0].SessionID != "" || !strings.HasPrefix(e.prompts()[0], "RESIDENT CONTRACT") {
		t.Errorf("launch %+v", e.drv.launches[0])
	}
	if st := e.state(); st.Agent != "pi" || st.Session != "sess-1" {
		t.Errorf("state %+v", st)
	}
}

// ---- the harness dying while idle ----

func die(ctx context.Context, e *env) (int, int64, error) {
	e.drv.emit(driver.Event{Kind: driver.Exit})
	<-ctx.Done()
	return 0, 0, ctx.Err()
}

func TestIdleDeathRestarts(t *testing.T) {
	e := newEnv(t)
	e.seed(&State{Session: "s1", Agent: "pi"})
	e.aif.polls = []pollFn{die, func(ctx context.Context, e *env) (int, int64, error) {
		e.advance(time.Minute) // the next death is outside the window
		return die(ctx, e)
	}, die}
	e.wantCode(e.runUntilIdle(), 0)
	if len(e.drv.launches) != 4 || e.drv.launches[3].SessionID != "s1" {
		t.Errorf("launches %+v: want three restarts resuming s1", e.drv.launches)
	}
	e.wantLog("harness exited while idle; restarted")
}

func TestIdleDeathBudget(t *testing.T) {
	e := newEnv(t)
	e.seed(&State{})
	e.aif.polls = []pollFn{die, die, die}
	e.wantCode(e.run(nil), 4)
	e.wantLog("harness keeps dying")
	if e.drv.stops != 1 {
		t.Errorf("harness stopped %d times: the fatal exit must stop the dead harness once, for its cleanup", e.drv.stops)
	}
}

// ---- review follow-ups ----

// A failed state write after a clean scan is logged and never counts toward the scan hold.
func TestSaveFailureNonFatal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	e := newEnv(t)
	e.seed(&State{Session: "s1", Agent: "pi"})
	t.Cleanup(func() { os.Chmod(e.dir(), 0o700) })
	next := func(since int64) ([]Message, int64, error) { return nil, since + 1, nil } // every scan saves
	e.aif.feeds = []feedFn{next, next, next, next, next}
	e.aif.polls = []pollFn{ans(1, 100), ans(1, 200)}
	readOnly := func(e *env) { os.Chmod(e.dir(), 0o500) }
	e.drv.steps = []step{then(readOnly, okStep), okStep, stopStep(okStep)}
	e.wantCode(e.run(nil), 0)
	if n := len(e.prompts()); n != 3 {
		t.Errorf("%d prompts, want 3: the failed saves after clean scans held the turns", n)
	}
	e.wantLog("state: ")
	if e.logged("scan error") || e.logged("held") {
		t.Errorf("a failed save counted as a failed scan; log:\n%s", strings.Join(e.logLines(), "\n"))
	}
}

// A local SHUTDOWN queued while scans are held ends the idle wait at once: no rest-of-interval sleep.
func TestLocalShutdownViaWakeDuringHold(t *testing.T) {
	e := newEnv(t)
	e.seed(&State{Session: "s1", Agent: "pi", LastControl: 5})
	fail := func(int64) ([]Message, int64, error) { return nil, 0, ErrUnreachable }
	e.aif.feeds = []feedFn{fail, fail, fail}
	e.aif.polls = []pollFn{ans(1, 6)} // the third failed scan: held
	go func() {
		<-e.idle
		e.call(Request{Op: "wake", Text: "#CMD[SHUTDOWN]#"})
	}()
	e.wantCode(e.run(nil), 0)
	if ps := e.prompts(); len(ps) != 2 || !strings.Contains(ps[1], "SHUTDOWN received from a local operator") {
		t.Fatalf("prompts %q", ps)
	}
	if want := []time.Duration{time.Minute}; !slices.Equal(e.sleeps, want) {
		t.Errorf("sleeps %v, want %v: the local command must skip the rest of the interval", e.sleeps, want)
	}
	if st := e.state(); st.Shutdown != nil || len(st.Wake) != 0 {
		t.Errorf("state %+v", st)
	}
	e.wantLog("new turns held")
	e.wantLog("control=SHUTDOWN from=local")
}

func TestResetFoundWhileIdle(t *testing.T) {
	e := newEnv(t)
	e.seed(&State{Session: "s1", Agent: "pi", LastControl: 5})
	e.aif.polls = []pollFn{ans(0, 7)} // past lastControl: scan
	e.aif.feeds = []feedFn{nil, nil, batch(7, msg(7, "root", "#CMD[RESET]#"))}
	e.drv.steps = []step{okStep, stopStep(okStep)}
	e.wantCode(e.run(nil), 0)
	ps := e.prompts()
	if len(ps) != 2 || !strings.Contains(ps[1], "reset by root (message 7)") {
		t.Fatalf("prompts %q", ps)
	}
	if len(e.drv.launches) != 2 || e.drv.launches[1].SessionID != "" {
		t.Errorf("launches %+v: want a restart on a fresh session", e.drv.launches)
	}
	if st := e.state(); st.LastControl != 7 || st.Session != "sess-2" {
		t.Errorf("state %+v", st)
	}
	e.wantLog("control=RESET from=root msg=7")
}

func TestSigtermWhileSleeping(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := newEnv(t)
	e.seed(&State{Session: "s1", Agent: "pi"})
	e.aif.polls = []pollFn{ans(0, 0)}
	e.onSleep = func(ctx context.Context) { cancel(); <-ctx.Done() }
	e.wantCode(e.run(ctx), 0)
	if len(e.prompts()) != 1 || e.drv.stops != 1 || len(e.sleeps) != 1 {
		t.Errorf("prompts %d stops %d sleeps %v", len(e.prompts()), e.drv.stops, e.sleeps)
	}
	e.wantLog("stopped")
}

func TestShutdownTurnTimeout(t *testing.T) {
	e := newEnv(t)
	e.seed(&State{Session: "s1", Agent: "pi", Shutdown: &Ref{From: "root", Msg: 4}})
	e.cfg.TurnTimeout = 20 * time.Millisecond
	e.drv.steps = []step{hangStep}
	e.wantCode(e.run(nil), 0)
	e.wantLog("turn=1 timeout after 20ms")
	e.wantLog("outcome=cancelled")
	if len(e.prompts()) != 1 || len(e.drv.launches) != 1 || e.state().Shutdown != nil {
		t.Errorf("prompts %d launches %d shutdown %+v", len(e.prompts()), len(e.drv.launches), e.state().Shutdown)
	}
}

func TestStatusMidTurn(t *testing.T) {
	e := newEnv(t)
	e.seed(&State{})
	var st *Status
	e.drv.steps = []step{then(func(e *env) { st = e.call(Request{Op: "status"}).Status }, stopStep(okStep))}
	e.wantCode(e.run(nil), 0)
	if st == nil || st.State != "turn" || st.Reason != "start" || st.Turn != 1 {
		t.Errorf("status %+v", st)
	}
}

// A Prompt blocked on a harness that stopped reading its stdin still times out; the kill frees it.
func TestBlockedPrompt(t *testing.T) {
	e := newEnv(t)
	e.seed(&State{Session: "s1", Agent: "pi"})
	e.cfg.TurnTimeout = 20 * time.Millisecond
	e.drv.steps = []step{blockStep, stopStep(okStep)}
	e.wantCode(e.run(nil), 0)
	if ps := e.prompts(); len(ps) != 2 || !strings.Contains(ps[1], RecoverTimeout) {
		t.Fatalf("prompts %q", ps)
	}
	e.wantLog(`cause="timeout after 20ms"`)
	if len(e.drv.launches) != 2 {
		t.Errorf("launches %d: want the harness restarted", len(e.drv.launches))
	}
}

func TestTextCoalesced(t *testing.T) {
	e := newEnv(t)
	e.seed(&State{})
	e.drv.steps = []step{stopStep(func(*env) ([]driver.Event, error) {
		return []driver.Event{
			{Kind: driver.Text, Text: "Hel"}, {Kind: driver.Text, Text: "lo, wor"}, {Kind: driver.Text, Text: "ld!\n\n  Done"},
			{Kind: driver.ToolCall, Text: "mcp__aif post"},
			{Kind: driver.Text, Text: "bye"},
			{Kind: driver.TurnEnd, OK: true},
		}, nil
	})}
	e.wantCode(e.run(nil), 0)
	var got []string
	for _, l := range e.logLines() {
		if strings.HasPrefix(l, "model: ") || strings.HasPrefix(l, "tool: ") {
			got = append(got, l)
		}
	}
	if want := []string{"model: Hello, world! Done", "tool: mcp__aif post", "model: bye"}; !slices.Equal(got, want) {
		t.Errorf("log %q, want %q", got, want)
	}
}
