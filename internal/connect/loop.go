package connect

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/dshein-alt/aif/internal/connect/driver"
)

const (
	failBudget = 3  // consecutive failed turns: exit 4
	idleBudget = 3  // harness deaths while idle within one interval: exit 4
	scanBudget = 3  // consecutive failed scans hold new turns
	pollCap    = 60 // the server's long-poll cap, seconds
)

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
	text                strings.Builder // model text not logged yet (logEvent); the idle watcher's while it runs
}

func (l *loop) logf(format string, a ...any) { l.d.Log(fmt.Sprintf(format, a...)) }

// save persists the state; the caller holds its lock. A failed write is logged, not fatal: the
// in-memory state stays authoritative and the next save retries it.
func (l *loop) save() {
	if err := l.st.Save(); err != nil {
		l.logf("state: %v", err)
	}
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
	l.logf("stopped")
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
		skipped, exit := l.idle(func(ctx context.Context) { n, seq, err = l.d.Client.Poll(ctx, min(l.cfg.Interval, pollCap)) })
		if exit != nil {
			if code := l.died(exit); code != 0 {
				return "", code
			}
			continue
		}
		if skipped { // no poll ran: the checks above act on the wake or stop
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
			if _, exit := l.idle(func(ctx context.Context) { l.d.Sleep(ctx, rest) }); exit != nil {
				if code := l.died(exit); code != 0 {
					return "", code
				}
			}
		}
	}
}

// idle runs f (a poll or a sleep) under a context the control handler cancels on wake or stop,
// while a watcher reads the harness's events; exit is the harness's Exit if it died meanwhile. It
// skips f when a stop, an actionable wake or a local command (held or not) is already there.
func (l *loop) idle(f func(context.Context)) (skipped bool, exit *driver.Event) {
	ctx, cancel := context.WithCancel(l.ctx)
	defer cancel()
	l.st.Lock()
	queued, local := len(l.st.Wake) > 0, len(promptWake(l.st.Wake)) < len(l.st.Wake)
	if l.stopReq || local || queued && !l.held() {
		l.st.Unlock()
		return true, nil
	}
	l.cancelWait = cancel
	l.st.Unlock()
	dead := make(chan *driver.Event, 1)
	go func() { dead <- l.watch(ctx, cancel) }()
	f(ctx)
	cancel()
	l.st.Lock()
	l.cancelWait = nil
	l.st.Unlock()
	return false, <-dead
}

// watch logs the harness's events until ctx ends; it returns the Exit of a harness that died.
func (l *loop) watch(ctx context.Context, cancel context.CancelFunc) *driver.Event {
	defer l.flushText()
	events := l.d.Driver.Events()
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, open := <-events:
			if !open || ev.Kind == driver.Exit {
				cancel()
				return &ev
			}
			l.logEvent(ev)
		}
	}
}

// died handles a harness death while idle: restart at once, fatal at the third within one interval.
func (l *loop) died(exit *driver.Event) int {
	l.alive = false
	if l.stopRequested() {
		return 0
	}
	now := l.d.Now()
	window := time.Duration(l.cfg.Interval) * time.Second
	l.deaths = append(slices.DeleteFunc(l.deaths, func(t time.Time) bool { return now.Sub(t) >= window }), now)
	if len(l.deaths) >= idleBudget {
		l.logf("%s", exitCause(fmt.Sprintf("harness keeps dying: %d exits while idle within %s", len(l.deaths), window), exit.Err))
		return 4
	}
	if err := l.start(); err != nil {
		l.logf("harness keeps dying: restart failed: %v", err)
		return 4
	}
	l.logf("%s; restarted", exitCause("harness exited while idle", exit.Err))
	return 0
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
