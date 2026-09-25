package connect

import (
	"fmt"
	"strings"
)

// Reason is why a turn started (docs/superpowers/specs/2026-09-25-aif-connect-design.md, "## The loop").
type Reason string

const (
	ReasonStart    Reason = "start"
	ReasonInbox    Reason = "inbox"
	ReasonWake     Reason = "wake"
	ReasonRecover  Reason = "recover"
	ReasonShutdown Reason = "shutdown"
)

var reasonText = map[Reason]string{
	ReasonStart:    "first turn",
	ReasonInbox:    "AIF reports unread messages for you",
	ReasonWake:     "a local message was queued with wake",
	ReasonRecover:  "your previous turn did not end cleanly",
	ReasonShutdown: "an operator sent SHUTDOWN",
}

// The recover turn's causes, passed as PromptInput.Control.
const (
	RecoverTimeout = "You've been killed due to turn timeout; the previous turn's work may be incomplete. Read the home thread with the `thread` tool before repeating anything."
	RecoverCrash   = "The harness died during your previous turn; its work may be incomplete. Read the home thread with the `thread` tool before repeating anything."
)

// RecoverFailed is the recover cause for a turn the harness reported as failed.
func RecoverFailed(cause string) string {
	return fmt.Sprintf("Your previous turn failed: %s. Read the home thread with the `thread` tool before repeating anything.", cause)
}

// PromptInput carries everything the per-turn prompt (design doc, "### Prompt") needs. Wake,
// Shutdown and FreshSession come straight from State. Control is a recover turn's cause
// (RecoverTimeout, RecoverCrash or RecoverFailed); the shutdown turn ignores it and FreshSession.
type PromptInput struct {
	Turn         int64
	Reason       Reason
	Wake         []Wake
	Shutdown     *Ref // required for ReasonShutdown
	FreshSession *Ref
	Control      string
	Goal         string
}

const contractReminder = "Follow the resident contract: whoami, read the inbox without clearing it, handle everything that woke you completely (every tagging message gets its reply, and work that outlasts the reply gets its result posted), clear what you handled with seen, and end the turn when nothing is left that you can do now."

// Prompt renders one turn's prompt; an unknown Reason, or ReasonShutdown without Shutdown, is an error.
func Prompt(p PromptInput) (string, error) {
	rt, ok := reasonText[p.Reason]
	if !ok {
		return "", fmt.Errorf("unknown turn reason %q", p.Reason)
	}
	lines := []string{fmt.Sprintf("Turn %d, woken because %s.", p.Turn, rt)}
	for _, w := range p.Wake {
		lines = append(lines, fmt.Sprintf("Operator message %s: %s", w.ID, w.Text))
	}

	if p.Reason == ReasonShutdown {
		if p.Shutdown == nil {
			return "", fmt.Errorf("shutdown turn without a Shutdown ref")
		}
		lines = append(lines, fmt.Sprintf("SHUTDOWN received from %s: read the inbox as usual, then reply to anything you still owe, do any operator messages above, post your goodbye in the home thread, clear what you handled with seen, and end the turn.", sender(*p.Shutdown)))
		return strings.Join(lines, "\n"), nil
	}

	if p.Control != "" {
		lines = append(lines, p.Control)
	}
	if p.FreshSession != nil {
		lines = append(lines, fmt.Sprintf("Your session was reset by %s. Run `aif-connect note list` with the shell tool right after whoami; the inbox still holds everything unanswered.", sender(*p.FreshSession)))
	}
	lines = append(lines, contractReminder, "Goal: "+p.Goal)
	return strings.Join(lines, "\n"), nil
}

// sender renders who ordered a command: "<name> (message <id>)" for a forum command, "a local
// operator" for one that came through the local wake queue (Msg 0).
func sender(r Ref) string {
	if r.Msg == 0 {
		return "a local operator"
	}
	return fmt.Sprintf("%s (message %d)", r.From, r.Msg)
}
