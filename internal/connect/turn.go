package connect

import (
	"context"
	"strings"
	"time"

	"github.com/dshein-alt/aif/internal/connect/driver"
)

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

// kill stops the harness. Under the driver contract Stop drains its events up to its Exit, so a
// restart starts clean; it runs even when the harness already died, when Stop only removes its temp
// files (the MCP config). A Stop cut short by its timeout has killed the process: its events, the
// Exit last, must still be read before a Start.
func (l *loop) kill() {
	l.alive = false
	ctx, cancel := context.WithTimeout(context.WithoutCancel(l.ctx), 30*time.Second)
	defer cancel()
	if err := l.d.Driver.Stop(ctx); err != nil {
		l.logf("harness stop: %v", err)
		for ev := range l.d.Driver.Events() {
			if ev.Kind == driver.Exit {
				break
			}
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
// recover turn's control text. Prompt runs aside: it blocks while the harness does not read its
// stdin, and only Stop, on the timeout, unblocks it. exec returns only once Prompt has.
func (l *loop) exec(text string, turn int64) (cause, control string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(l.ctx), l.cfg.TurnTimeout)
	defer cancel()
	defer l.flushText()
	events := l.d.Driver.Events()
	sent := make(chan error, 1)
	go func() { sent <- l.d.Driver.Prompt(ctx, text) }()
	ended := false
	for !ended || sent != nil {
		select {
		case err := <-sent:
			sent = nil
			switch {
			case err == nil:
				l.needContract = false
			case ended: // the harness died first: its cause stands
			case ctx.Err() != nil:
				return l.timeout(turn, nil)
			default:
				l.kill() // a harness that cannot take a prompt is in no known state: restart it
				return "prompt: " + err.Error(), RecoverFailed(err.Error())
			}
		case ev, open := <-events:
			l.logEvent(ev)
			switch {
			case !open || ev.Kind == driver.Exit:
				l.alive = false
				cause, control = exitCause("harness died", ev.Err), RecoverCrash
			case ev.Kind == driver.TurnEnd && !ev.OK:
				if ev.Err == "" {
					ev.Err = "turn failed"
				}
				cause, control = ev.Err, RecoverFailed(ev.Err)
			case ev.Kind != driver.TurnEnd:
				continue
			}
			ended, events = true, nil
		case <-ctx.Done():
			return l.timeout(turn, sent)
		}
	}
	return cause, control
}

// timeout kills the harness of a turn that ran out of time, then waits out a Prompt still in
// flight (sent non-nil): the kill is what unblocks it.
func (l *loop) timeout(turn int64, sent chan error) (cause, control string) {
	after := short(l.cfg.TurnTimeout)
	l.flushText()
	l.logf("turn=%d timeout after %s", turn, after)
	l.kill()
	if sent != nil {
		<-sent
	}
	return "timeout after " + after, RecoverTimeout
}

// exitCause names a harness exit, with the Exit event's error when it carries one.
func exitCause(what, err string) string {
	if err == "" {
		return what
	}
	return what + ": " + err
}

// textFlush bounds the model text held back for one log line.
const textFlush = 2 << 10

// logEvent logs what the harness did. Text arrives in chunks (the driver contract): they are
// joined verbatim and logged as one line when a tool call, the turn's end, the harness's exit or
// the size bound flushes them.
func (l *loop) logEvent(ev driver.Event) {
	switch ev.Kind {
	case driver.Text:
		l.text.WriteString(ev.Text)
		if l.text.Len() > textFlush {
			l.flushText()
		}
	case driver.ToolCall:
		l.flushText()
		l.logf("tool: %s", strings.Join(strings.Fields(ev.Text), " "))
	default:
		l.flushText()
	}
}

// flushText logs the held-back model text as one line, its whitespace collapsed.
func (l *loop) flushText() {
	if l.text.Len() == 0 {
		return
	}
	l.logf("model: %s", strings.Join(strings.Fields(l.text.String()), " "))
	l.text.Reset()
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
