package connect

import (
	"fmt"
	"strings"
)

// WakeMsg is one queued local message (§ State, the "wake" list in state.json), rendered oldest
// first as an "Operator message <id>: <text>" line.
type WakeMsg struct {
	ID   string
	Text string
}

// PromptInput carries everything the per-turn prompt (docs/superpowers/specs/2026-09-25-aif-connect-design.md,
// "### Prompt") needs to render. Reason is one of start|inbox|wake|recover|shutdown. Control is
// either a recover turn's cause or the fresh-session note after RESET; it renders as its own line
// when set, for any reason. From/MsgID describe who sent SHUTDOWN and which message carried it
// (MsgID 0 means a local wake command, rendered "from local").
type PromptInput struct {
	Turn    int
	Reason  string
	Wake    []WakeMsg
	Control string
	Goal    string
	From    string
	MsgID   int64
}

var reasonText = map[string]string{
	"start":    "first turn",
	"inbox":    "AIF reports unread messages for you",
	"wake":     "a local message was queued with wake",
	"recover":  "your previous turn did not end cleanly",
	"shutdown": "an operator sent SHUTDOWN",
}

const contractText = "Follow the resident contract: whoami, read the inbox without clearing it, handle everything that\n" +
	"woke you completely (every tagging message gets its reply, the work it asks for gets done, the\n" +
	"result is posted in the home thread), clear what you handled with seen, and end the turn when\n" +
	"nothing is left that you can do now."

// Prompt renders one turn's prompt. See PromptInput and the design doc's "### Prompt" section.
func Prompt(p PromptInput) string {
	var lines []string
	lines = append(lines, fmt.Sprintf("Turn %d, woken because %s.", p.Turn, reasonText[p.Reason]))
	for _, w := range p.Wake {
		lines = append(lines, fmt.Sprintf("Operator message %s: %s", w.ID, w.Text))
	}

	if p.Reason == "shutdown" {
		lines = append(lines, fmt.Sprintf("SHUTDOWN received from %s: reply to anything you still owe, post your goodbye in the home thread, clear the inbox with seen, then end the turn", who(p.From, p.MsgID)))
		return strings.Join(lines, "\n")
	}

	if p.Control != "" {
		lines = append(lines, p.Control)
	}
	lines = append(lines, contractText)
	lines = append(lines, fmt.Sprintf("Goal: %s", p.Goal))
	return strings.Join(lines, "\n")
}

// who renders the SHUTDOWN sender: "<name> (message <id>)" for a forum command, "local" for one
// that arrived through the local wake queue (MsgID 0).
func who(from string, msgID int64) string {
	if msgID == 0 {
		return "local"
	}
	return fmt.Sprintf("%s (message %d)", from, msgID)
}
