package connect

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dshein-alt/aif/internal/connect/driver"
)

type remoteAIF struct {
	*scriptAIF
	ready                chan struct{}
	once                 sync.Once
	cmd                  RemoteCommand
	ackStatus, ackReason string
	ackResult            string
	notice               string
}

func (a *remoteAIF) NextCommand(ctx context.Context, _ int64) (*RemoteCommand, error) {
	first := false
	a.once.Do(func() { first = true })
	if !first {
		return nil, nil
	}
	select {
	case <-a.ready:
		return &a.cmd, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (a *remoteAIF) AckCommand(_ context.Context, _ int64, status, reason string, _ int64) (string, error) {
	a.ackStatus, a.ackReason = status, reason
	if a.ackResult != "" {
		return a.ackResult, nil
	}
	return status, nil
}
func (a *remoteAIF) PostNotice(_ context.Context, _ int64, body string) error {
	a.notice = body
	a.scriptAIF.e.rec("notice")
	return nil
}

func TestRemoteKillDuringTurnPostsBeforeStop(t *testing.T) {
	e := newEnv(t)
	e.cfg.CommandExpires = 1800
	e.aif.polls = []pollFn{ans(0, 0)}
	ready := make(chan struct{})
	e.drv.steps = []step{func(*env) ([]driver.Event, error) { close(ready); return nil, nil }}
	a := &remoteAIF{scriptAIF: e.aif, ready: ready, cmd: RemoteCommand{ID: 4, Issuer: "root", Name: "KILL", CreatedAt: time.Now().Unix(), ExpiresAt: time.Now().Unix() + 1800, ServerNow: time.Now().Unix()}}
	d := e.deps()
	d.Client = a
	done := make(chan int, 1)
	go func() { done <- Run(context.Background(), e.cfg, d) }()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit code %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("KILL did not interrupt turn")
	}
	if a.ackStatus != "accepted" || a.notice != "Agent mybot received command KILL from root." {
		t.Fatalf("ack=%s notice=%q", a.ackStatus, a.notice)
	}
	e.mu.Lock()
	calls := strings.Join(e.calls, ",")
	e.mu.Unlock()
	if !strings.Contains(calls, "notice") || strings.Index(calls, "notice") > strings.LastIndex(calls, "stop") {
		t.Fatalf("notice must precede harness stop: %s", calls)
	}
}

func TestRemoteRejectsWithoutNotice(t *testing.T) {
	for _, tc := range []struct {
		name, issuer, command string
		expires, now          int64
		reason                string
	}{
		{"expired", "root", "KILL", 99, 100, "command_expired"},
		{"unauthorized", "other", "KILL", 101, 100, "unauthorized_sender"},
		{"unknown", "root", "NOPE", 101, 100, "unknown_command"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.cfg.CommandExpires = 1800
			st := &State{dir: e.dir(), Origin: e.cfg.AifURL}
			if err := EnsureDir(e.dir()); err != nil {
				t.Fatal(err)
			}
			a := &remoteAIF{scriptAIF: e.aif}
			d := e.deps()
			d.Client = a
			l := &loop{ctx: context.Background(), cfg: e.cfg, d: d, st: st, remoteDone: make(chan struct{}, 1)}
			if r := l.handleRemote(RemoteCommand{ID: 1, Issuer: tc.issuer, Name: tc.command, ExpiresAt: tc.expires, ServerNow: tc.now}); r != "" {
				t.Fatalf("reason %q", r)
			}
			if a.ackStatus != "rejected" || a.ackReason != tc.reason || a.notice != "" {
				t.Fatalf("ack=%q reason=%q notice=%q", a.ackStatus, a.ackReason, a.notice)
			}
		})
	}
}

func TestRemoteResetAndShutdownDuringTurn(t *testing.T) {
	for _, action := range []string{"RESET", "SHUTDOWN"} {
		t.Run(action, func(t *testing.T) {
			e := newEnv(t)
			e.cfg.CommandExpires = 1800
			e.aif.polls = []pollFn{ans(0, 0)}
			ready := make(chan struct{})
			second := okStep
			if action == "RESET" {
				second = stopStep(okStep)
			}
			e.drv.steps = []step{func(*env) ([]driver.Event, error) { close(ready); return nil, nil }, second}
			now := time.Now().Unix()
			a := &remoteAIF{scriptAIF: e.aif, ready: ready, cmd: RemoteCommand{ID: 8, Issuer: "root", Name: action, CreatedAt: now, ExpiresAt: now + 1800, ServerNow: now}}
			d := e.deps()
			d.Client = a
			done := make(chan int, 1)
			go func() { done <- Run(context.Background(), e.cfg, d) }()
			select {
			case code := <-done:
				if code != 0 {
					t.Fatalf("exit %d", code)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("command did not interrupt turn")
			}
			ps := e.prompts()
			if len(ps) != 2 {
				t.Fatalf("prompts %d", len(ps))
			}
			if action == "RESET" && !strings.Contains(ps[1], "session was reset by root (command 8)") {
				t.Fatalf("reset prompt: %s", ps[1])
			}
			if action == "SHUTDOWN" && !strings.Contains(ps[1], "SHUTDOWN received from root (command 8)") {
				t.Fatalf("shutdown prompt: %s", ps[1])
			}
			if a.notice != "Agent mybot received command "+action+" from root." {
				t.Fatalf("notice %q", a.notice)
			}
		})
	}
}

func TestRecoveredExpiredRemoteCommandDoesNotExecute(t *testing.T) {
	e := newEnv(t)
	e.cfg.CommandExpires = 1800
	st := &State{dir: e.dir(), Origin: e.cfg.AifURL, Pending: &Pending{Action: "RESET", From: "root", Remote: 9}}
	if err := EnsureDir(e.dir()); err != nil {
		t.Fatal(err)
	}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	a := &remoteAIF{scriptAIF: e.aif, ackResult: "rejected"}
	d := e.deps()
	d.Client = a
	l := &loop{ctx: context.Background(), cfg: e.cfg, d: d, st: st}
	if r := l.commands(false); r != "" {
		t.Fatalf("reason %q", r)
	}
	if st.Pending != nil || a.notice != "" || a.ackStatus != "accepted" {
		t.Fatalf("pending=%+v notice=%q ack=%q", st.Pending, a.notice, a.ackStatus)
	}
}
