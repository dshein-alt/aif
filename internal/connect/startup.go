package connect

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	Log     func(string) // must be safe for concurrent use: drivers log from their own goroutines
	Now     func() time.Time
	Sleep   func(ctx context.Context, d time.Duration) // returns early when ctx is done
	Home    string                                     // the state directory lives under it (StateDir)
	Version string                                     // compared with ping's v
	Env     []string                                   // harness environment beyond AIF_CONNECT_NAME/STATE (the CLI's PATH)
	Verbose bool                                       // drivers echo raw harness stdout
}

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
	if l.scanFails == 0 {
		// A clean scan left LastControl at its feed's seq: the start turn reads the inbox up to
		// there, so only a poll past it is news.
		l.lastInbox = st.LastControl
	}
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
	if code != 0 { // a fatal exit leaves no harness (nor its temp files) behind; finish stopped it otherwise
		l.kill()
	}
	return code
}

func (l *loop) unreachable(err error) int {
	if l.ctx.Err() != nil {
		return 0 // stopped during startup
	}
	l.logf("aif server %s unreachable: %v", l.cfg.AifURL, err)
	return 5
}
