package connect

import (
	"context"
	"regexp"
	"slices"
	"strings"
)

// cmdRe is the one command marker: #CMD[NAME args]#. NAME is case-sensitive: an upper-case
// letter, then upper-case letters, digits or underscores; args run lazily to the first "]#".
var cmdRe = regexp.MustCompile(`#CMD\[([A-Z][A-Z0-9_]*)(?:\s+(.*?))?\]#`)

// Command is one #CMD[...]# marker found in a message body.
type Command struct {
	Name string
	Args string
}

// Hit is the command a scan decided to execute.
type Hit struct {
	ID     int64
	From   string
	Action string // "SHUTDOWN" or "RESET"
}

// Extract returns every command marker in body, in order.
func Extract(body string) []Command {
	var out []Command
	for _, m := range cmdRe.FindAllStringSubmatch(body, -1) {
		out = append(out, Command{Name: m[1], Args: m[2]})
	}
	return out
}

// Scan picks the command to execute from a feed batch: only operators' messages that sit in the
// home thread or tag agentName count. SHUTDOWN beats RESET wherever they are in the batch; among
// several of the winning action the latest (highest id) supplies From. Hit.ID is the highest id
// of any known command that counted, whatever its action, so a cursor advanced to it consumes the
// batch whole. isOperator must match names case-insensitively (Config.IsOperator does).
// Unknown names are returned for logging and otherwise ignored.
func Scan(msgs []Message, thread int64, agentName string, isOperator func(string) bool) (hit *Hit, unknown []string) {
	rank := map[string]int{"RESET": 1, "SHUTDOWN": 2}
	var last int64
	for _, m := range msgs {
		if !isOperator(m.Author) {
			continue
		}
		if m.Thread != thread && !slices.ContainsFunc(m.At, func(a string) bool { return strings.EqualFold(a, agentName) }) {
			continue
		}
		for _, c := range Extract(m.Body) {
			r, known := rank[c.Name]
			if !known {
				unknown = append(unknown, c.Name)
				continue
			}
			last = max(last, m.ID)
			if hit == nil || r > rank[hit.Action] || (r == rank[hit.Action] && m.ID > hit.ID) {
				hit = &Hit{ID: m.ID, From: m.Author, Action: c.Name}
			}
		}
	}
	if hit != nil {
		hit.ID = last
	}
	return hit, unknown
}

// localCommands are the only local wake texts that are commands: the whole text, exactly.
var localCommands = map[string]string{"#CMD[SHUTDOWN]#": "SHUTDOWN", "#CMD[RESET]#": "RESET"}

// promptWake is the wake queue as a prompt renders it: local commands are never operator lines.
func promptWake(ws []Wake) []Wake {
	return slices.DeleteFunc(slices.Clone(ws), func(w Wake) bool { return localCommands[w.Text] != "" })
}

// LocalScan finds the oldest queued local command and records it as st.Pending, the first of the
// two writes (design doc, "### Commands"). Nil when the queue holds none.
func LocalScan(st *State) (*Pending, error) {
	st.Lock()
	defer st.Unlock()
	for _, w := range st.Wake {
		if a := localCommands[w.Text]; a != "" {
			st.Pending = &Pending{Action: a, From: "local", Local: w.ID}
			return st.Pending, st.Save()
		}
	}
	return nil, nil
}

// RunScan reads every message above st.LastControl (feed pages internally and returns the batch
// whole or an error) and runs Scan over it. A hit is recorded as st.Pending, the first write, and
// returned; the caller executes it and calls Commit. A clean scan without a hit advances
// LastControl to the reply's seq. A feed error changes nothing and returns (nil, nil, err); a
// failed Save is returned with whatever was found.
func RunScan(ctx context.Context, feed func(context.Context, int64) ([]Message, int64, error), st *State, cfg Config) (*Pending, []string, error) {
	st.Lock()
	since := st.LastControl
	st.Unlock()
	msgs, seq, err := feed(ctx, since)
	if err != nil {
		return nil, nil, err
	}
	hit, unknown := Scan(msgs, cfg.Thread, cfg.AgentName, cfg.IsOperator)
	st.Lock()
	defer st.Unlock()
	if hit != nil {
		st.Pending = &Pending{Action: hit.Action, From: hit.From, Msg: hit.ID}
		return st.Pending, unknown, st.Save()
	}
	if seq <= st.LastControl {
		return nil, unknown, nil
	}
	st.LastControl = seq
	return nil, unknown, st.Save()
}

// Commit is the second write: it applies st.Pending's effect and clears it in one Save. A forum
// command moves LastControl to its id, a local one leaves the wake queue; RESET clears Session and
// sets FreshSession, SHUTDOWN sets Shutdown. No pending command is a no-op.
func Commit(st *State) error {
	st.Lock()
	defer st.Unlock()
	p := st.Pending
	if p == nil {
		return nil
	}
	if p.Local != "" {
		st.Dequeue(p.Local)
	} else {
		st.LastControl = max(st.LastControl, p.Msg)
	}
	ref := &Ref{From: p.From, Msg: p.Msg}
	switch p.Action {
	case "RESET":
		st.Session, st.FreshSession = "", ref
	case "SHUTDOWN":
		st.Shutdown = ref
	}
	st.Pending = nil
	return st.Save()
}

// commands runs the command step: a command a crash left uncommitted, queued local commands, then
// (when forum) the forum scan, each hit executed, until none is left or a SHUTDOWN is committed.
// It returns the turn the commands owe: shutdown, start after a RESET, or "".
func (l *loop) commands(forum bool) Reason {
	reset := false
	for {
		l.st.Lock()
		p := l.st.Pending
		l.st.Unlock()
		var err error
		if p == nil {
			p, err = LocalScan(l.st)
		}
		if p == nil && forum {
			var unknown []string
			p, unknown, err = RunScan(l.ctx, l.d.Client.Feed, l.st, l.cfg)
			for _, u := range unknown {
				l.logf("unknown command %s ignored", u)
			}
			if p == nil {
				l.scanned(err)
				forum = false
				err = nil
			} else {
				l.scanned(nil)
			}
		}
		if err != nil { // a failed Save; the command still runs
			l.logf("state: %v", err)
		}
		if p == nil {
			break
		}
		l.execute(p)
		if p.Action == "SHUTDOWN" {
			return ReasonShutdown
		}
		reset = true
	}
	if reset {
		return ReasonStart
	}
	return ""
}

// scanned keeps the failed-scan count that holds new turns.
func (l *loop) scanned(err error) {
	switch {
	case err == nil:
		if l.held() {
			l.logf("scan ok; turns resume")
		}
		l.scanFails = 0
	case l.ctx.Err() != nil: // a stop, not a failure
	default:
		l.scanFails++
		l.logf("scan error: %v", err)
		if l.scanFails == scanBudget {
			l.logf("scan failed %d times; new turns held until a scan succeeds", scanBudget)
		}
	}
}

// execute logs and commits a recorded command; a RESET stops the harness first (the next turn
// starts it on a fresh session).
func (l *loop) execute(p *Pending) {
	if p.Local != "" {
		l.logf("control=%s from=local", p.Action)
	} else {
		l.logf("control=%s from=%s msg=%d", p.Action, p.From, p.Msg)
	}
	if p.Action == "RESET" {
		l.kill()
	}
	if err := Commit(l.st); err != nil {
		l.logf("state: %v", err)
	}
}
