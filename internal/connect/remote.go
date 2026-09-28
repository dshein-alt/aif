package connect

import (
	"context"
	"fmt"
	"time"
)

// This optional interface keeps existing local connector clients and test harnesses compatible.
type remoteClient interface {
	NextCommand(context.Context, int64) (*RemoteCommand, error)
	AckCommand(context.Context, int64, string, string, int64) (string, error)
	PostNotice(context.Context, int64, string) error
}

// watchRemote runs independently of model turns. A command is handed to the loop goroutine,
// which alone mutates lifecycle state and drives the harness. Polling is short so a KILL is
// useful even while the ordinary forum poll is waiting.
func (l *loop) watchRemote(ctx context.Context) {
	client := l.d.Client.(remoteClient)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	lastError := ""
	for {
		cmd, err := client.NextCommand(ctx, l.cfg.CommandExpires)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if err.Error() != lastError {
				l.logf("command poll: %v", err)
				lastError = err.Error()
			}
		} else {
			lastError = ""
			if cmd != nil {
				select {
				case l.remoteCh <- *cmd:
				case <-ctx.Done():
					return
				}
				l.st.Lock()
				l.interrupt()
				l.st.Unlock()
				select {
				case <-l.remoteDone:
				case <-ctx.Done():
					return
				}
				continue // clear a backlog without a two-second delay per command
			}
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

// handleRemote validates locally, asks the server to record its decision (and recheck expiry
// atomically), then posts the receipt before changing the harness lifecycle.
func (l *loop) handleRemote(cmd RemoteCommand) Reason {
	defer func() { l.remoteDone <- struct{}{} }()
	client := l.d.Client.(remoteClient)
	status, reason := "accepted", ""
	switch {
	case cmd.ServerNow >= cmd.ExpiresAt:
		status, reason = "rejected", "command_expired"
	case !l.cfg.IsOperator(cmd.Issuer):
		status, reason = "rejected", "unauthorized_sender"
	case cmd.Name != "SHUTDOWN" && cmd.Name != "RESET" && cmd.Name != "KILL":
		status, reason = "rejected", "unknown_command"
	}
	if status == "accepted" {
		l.st.Lock()
		l.st.Pending = &Pending{Action: cmd.Name, From: cmd.Issuer, Remote: cmd.ID}
		if cmd.Name == "SHUTDOWN" {
			l.st.Pending.Deadline = time.Now().Add(l.cfg.TurnTimeout).Unix()
		}
		err := l.st.Save()
		l.st.Unlock()
		if err != nil {
			l.logf("state: %v", err)
			return ""
		}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(l.ctx), 5*time.Second)
	actual, err := client.AckCommand(ctx, cmd.ID, status, reason, l.cfg.CommandExpires)
	cancel()
	if err != nil {
		l.logf("command ack %d: %v", cmd.ID, err)
		l.clearRemotePending(cmd.ID)
		return ""
	}
	if actual != "accepted" {
		l.clearRemotePending(cmd.ID)
		l.logf("command %d rejected: %s", cmd.ID, reason)
		return ""
	}
	p := l.st.Pending
	l.st.Lock()
	p.Acknowledged = true
	if err := l.st.Save(); err != nil {
		l.logf("state: %v", err)
	}
	l.st.Unlock()
	l.execute(p)
	switch cmd.Name {
	case "KILL":
		return ReasonKill
	case "SHUTDOWN":
		l.kill() // stop the interrupted turn only after its receipt was posted
		return ReasonShutdown
	case "RESET":
		return ReasonStart
	}
	return ""
}

func (l *loop) clearRemotePending(id int64) {
	l.st.Lock()
	defer l.st.Unlock()
	if l.st.Pending != nil && l.st.Pending.Remote == id {
		l.st.Pending = nil
		if err := l.st.Save(); err != nil {
			l.logf("state: %v", err)
		}
	}
}

// announce is deliberately bounded: KILL must still act if the forum cannot take a post.
// A crash after the post but before Noticed is saved may repeat the notice on restart.
func (l *loop) announce(p *Pending) {
	if p.Remote == 0 || p.Noticed {
		return
	}
	client, ok := l.d.Client.(remoteClient)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(l.ctx), 5*time.Second)
	defer cancel()
	body := fmt.Sprintf("Agent %s received command %s from %s.", l.cfg.AgentName, p.Action, p.From)
	if err := client.PostNotice(ctx, l.cfg.Thread, body); err != nil {
		l.logf("command %d notice: %v", p.Remote, err)
		return
	}
	l.st.Lock()
	p.Noticed = true
	if err := l.st.Save(); err != nil {
		l.logf("state: %v", err)
	}
	l.st.Unlock()
}

// confirmPending closes the crash window between saving a command and acknowledging it.
func (l *loop) confirmPending(p *Pending) bool {
	if p.Remote == 0 || p.Acknowledged {
		return true
	}
	client, ok := l.d.Client.(remoteClient)
	if !ok {
		return false
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(l.ctx), 5*time.Second)
	status, err := client.AckCommand(ctx, p.Remote, "accepted", "", l.cfg.CommandExpires)
	cancel()
	if err != nil {
		l.logf("command ack %d: %v", p.Remote, err)
		return false
	}
	if status != "accepted" {
		l.clearRemotePending(p.Remote)
		return false
	}
	l.st.Lock()
	p.Acknowledged = true
	if err := l.st.Save(); err != nil {
		l.logf("state: %v", err)
	}
	l.st.Unlock()
	return true
}
