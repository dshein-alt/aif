package connect

import (
	"strings"
	"testing"
)

// Ports the "### Prompt" section of docs/superpowers/specs/2026-09-25-aif-connect-design.md into
// executable tests: every reason's rendering, the queued-message lines, the control-text line,
// and the shutdown variant's different shape (no contract/goal lines).

const contractLine = "Follow the resident contract: whoami, read the inbox without clearing it, handle everything that\nwoke you completely (every tagging message gets its reply, the work it asks for gets done, the\nresult is posted in the home thread), clear what you handled with seen, and end the turn when\nnothing is left that you can do now."

func TestPromptReasonStart(t *testing.T) {
	got := Prompt(PromptInput{Turn: 1, Reason: "start", Goal: "Help out."})
	want := "Turn 1, woken because first turn.\n" +
		contractLine + "\n" +
		"Goal: Help out."
	if got != want {
		t.Fatalf("start prompt mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestPromptReasonInbox(t *testing.T) {
	got := Prompt(PromptInput{Turn: 5, Reason: "inbox", Goal: "Help out."})
	want := "Turn 5, woken because AIF reports unread messages for you.\n" +
		contractLine + "\n" +
		"Goal: Help out."
	if got != want {
		t.Fatalf("inbox prompt mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestPromptReasonWake(t *testing.T) {
	got := Prompt(PromptInput{
		Turn:   2,
		Reason: "wake",
		Wake:   []WakeMsg{{ID: "a1", Text: "check the deploy"}, {ID: "a2", Text: "then ping me"}},
		Goal:   "Help out.",
	})
	want := "Turn 2, woken because a local message was queued with wake.\n" +
		"Operator message a1: check the deploy\n" +
		"Operator message a2: then ping me\n" +
		contractLine + "\n" +
		"Goal: Help out."
	if got != want {
		t.Fatalf("wake prompt mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestPromptReasonRecover(t *testing.T) {
	got := Prompt(PromptInput{
		Turn:    9,
		Reason:  "recover",
		Control: "You've been killed due to turn timeout; the previous turn's work may be incomplete. Check the forum before repeating anything.",
		Goal:    "Help out.",
	})
	want := "Turn 9, woken because your previous turn did not end cleanly.\n" +
		"You've been killed due to turn timeout; the previous turn's work may be incomplete. Check the forum before repeating anything.\n" +
		contractLine + "\n" +
		"Goal: Help out."
	if got != want {
		t.Fatalf("recover prompt mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestPromptReasonRecoverFreshSessionNote(t *testing.T) {
	got := Prompt(PromptInput{
		Turn:    1,
		Reason:  "start",
		Control: "Your session was reset by TheRoot (message 4712). Read `aif-connect note list` first; the inbox still holds everything unanswered.",
		Goal:    "Help out.",
	})
	want := "Turn 1, woken because first turn.\n" +
		"Your session was reset by TheRoot (message 4712). Read `aif-connect note list` first; the inbox still holds everything unanswered.\n" +
		contractLine + "\n" +
		"Goal: Help out."
	if got != want {
		t.Fatalf("fresh-session prompt mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestPromptReasonShutdownFromForumMessage(t *testing.T) {
	got := Prompt(PromptInput{
		Turn:   4,
		Reason: "shutdown",
		From:   "TheRoot",
		MsgID:  4712,
		Goal:   "Help out.",
	})
	want := "Turn 4, woken because an operator sent SHUTDOWN.\n" +
		"SHUTDOWN received from TheRoot (message 4712): reply to anything you still owe, post your goodbye in the home thread, clear the inbox with seen, then end the turn"
	if got != want {
		t.Fatalf("shutdown prompt mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestPromptReasonShutdownLocal(t *testing.T) {
	got := Prompt(PromptInput{
		Turn:   4,
		Reason: "shutdown",
		From:   "TheRoot",
		Goal:   "Help out.",
	})
	want := "Turn 4, woken because an operator sent SHUTDOWN.\n" +
		"SHUTDOWN received from local: reply to anything you still owe, post your goodbye in the home thread, clear the inbox with seen, then end the turn"
	if got != want {
		t.Fatalf("shutdown local prompt mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestPromptReasonShutdownWithQueuedMessages(t *testing.T) {
	got := Prompt(PromptInput{
		Turn:   4,
		Reason: "shutdown",
		From:   "TheRoot",
		MsgID:  4712,
		Wake:   []WakeMsg{{ID: "w1", Text: "one more thing"}},
		Goal:   "Help out.",
	})
	want := "Turn 4, woken because an operator sent SHUTDOWN.\n" +
		"Operator message w1: one more thing\n" +
		"SHUTDOWN received from TheRoot (message 4712): reply to anything you still owe, post your goodbye in the home thread, clear the inbox with seen, then end the turn"
	if got != want {
		t.Fatalf("shutdown+queued prompt mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestPromptShutdownHasNoContractOrGoalLines(t *testing.T) {
	got := Prompt(PromptInput{Turn: 1, Reason: "shutdown", From: "TheRoot", MsgID: 1, Goal: "Help out."})
	for _, forbidden := range []string{"Follow the resident contract", "Goal:"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("shutdown prompt must not contain %q: %q", forbidden, got)
		}
	}
}
