package connect

import (
	"strings"
	"testing"
)

// Ports the "### Prompt" section of docs/superpowers/specs/2026-09-25-aif-connect-design.md into
// executable tests: every reason's rendering, the queued-message lines, the recover and
// fresh-session lines, and the shutdown variant's different shape (no contract/goal lines).

const contractLine = "Follow the resident contract: whoami, read the inbox without clearing it, handle everything that woke you completely (every tagging message gets its reply, the work it asks for gets done, the result is posted in the home thread), clear what you handled with seen, and end the turn when nothing is left that you can do now."

const shutdownTail = ": reply to anything you still owe, post your goodbye in the home thread, clear what you handled with seen, then end the turn."

func TestPrompt(t *testing.T) {
	tail := contractLine + "\nGoal: Help out."
	cases := []struct {
		name string
		in   PromptInput
		want string
	}{
		{"start", PromptInput{Turn: 1, Reason: ReasonStart},
			"Turn 1, woken because first turn.\n" + tail},
		{"inbox", PromptInput{Turn: 5, Reason: ReasonInbox},
			"Turn 5, woken because AIF reports unread messages for you.\n" + tail},
		{"wake", PromptInput{Turn: 2, Reason: ReasonWake, Wake: []Wake{{ID: "a1", Text: "check the deploy"}, {ID: "a2", Text: "then ping me"}}},
			"Turn 2, woken because a local message was queued with wake.\n" +
				"Operator message a1: check the deploy\n" +
				"Operator message a2: then ping me\n" + tail},
		{"recover timeout", PromptInput{Turn: 9, Reason: ReasonRecover, Control: RecoverTimeout},
			"Turn 9, woken because your previous turn did not end cleanly.\n" +
				"You've been killed due to turn timeout; the previous turn's work may be incomplete. Check the forum before repeating anything.\n" + tail},
		{"recover crash", PromptInput{Turn: 9, Reason: ReasonRecover, Control: RecoverCrash},
			"Turn 9, woken because your previous turn did not end cleanly.\n" +
				"The harness died during your previous turn; its work may be incomplete. Check the forum before repeating anything.\n" + tail},
		{"recover failed", PromptInput{Turn: 9, Reason: ReasonRecover, Control: RecoverFailed("exit status 1")},
			"Turn 9, woken because your previous turn did not end cleanly.\n" +
				"Your previous turn failed: exit status 1. Check the forum before repeating anything.\n" + tail},
		{"fresh session", PromptInput{Turn: 1, Reason: ReasonStart, FreshSession: &Ref{From: "TheRoot", Msg: 4712}},
			"Turn 1, woken because first turn.\n" +
				"Your session was reset by TheRoot (message 4712). Read `aif-connect note list` right after whoami; the inbox still holds everything unanswered.\n" + tail},
		{"fresh session local", PromptInput{Turn: 1, Reason: ReasonStart, FreshSession: &Ref{From: "TheRoot"}},
			"Turn 1, woken because first turn.\n" +
				"Your session was reset by a local operator. Read `aif-connect note list` right after whoami; the inbox still holds everything unanswered.\n" + tail},
		{"shutdown forum", PromptInput{Turn: 4, Reason: ReasonShutdown, Shutdown: &Ref{From: "TheRoot", Msg: 4712}},
			"Turn 4, woken because an operator sent SHUTDOWN.\n" +
				"SHUTDOWN received from TheRoot (message 4712)" + shutdownTail},
		{"shutdown local", PromptInput{Turn: 4, Reason: ReasonShutdown, Shutdown: &Ref{From: "TheRoot"}},
			"Turn 4, woken because an operator sent SHUTDOWN.\n" +
				"SHUTDOWN received from a local operator" + shutdownTail},
		{"shutdown queued", PromptInput{Turn: 4, Reason: ReasonShutdown, Shutdown: &Ref{From: "TheRoot", Msg: 4712}, Wake: []Wake{{ID: "w1", Text: "one more thing"}}},
			"Turn 4, woken because an operator sent SHUTDOWN.\n" +
				"Operator message w1: one more thing\n" +
				"SHUTDOWN received from TheRoot (message 4712)" + shutdownTail},
		{"shutdown ignores Control and FreshSession", PromptInput{Turn: 4, Reason: ReasonShutdown, Shutdown: &Ref{From: "TheRoot", Msg: 1}, Control: RecoverTimeout, FreshSession: &Ref{From: "TheRoot", Msg: 2}},
			"Turn 4, woken because an operator sent SHUTDOWN.\n" +
				"SHUTDOWN received from TheRoot (message 1)" + shutdownTail},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.in.Goal = "Help out."
			got, err := Prompt(c.in)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("prompt mismatch:\ngot:  %q\nwant: %q", got, c.want)
			}
		})
	}
}

func TestPromptShutdownHasNoContractOrGoalLines(t *testing.T) {
	got, err := Prompt(PromptInput{Turn: 1, Reason: ReasonShutdown, Shutdown: &Ref{From: "TheRoot", Msg: 1}, Goal: "Help out."})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"Follow the resident contract", "Goal:"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("shutdown prompt must not contain %q: %q", forbidden, got)
		}
	}
}

func TestPromptErrors(t *testing.T) {
	for _, in := range []PromptInput{
		{Turn: 1, Reason: "timer"},
		{Turn: 1},
		{Turn: 1, Reason: ReasonShutdown},
	} {
		if got, err := Prompt(in); err == nil {
			t.Fatalf("Prompt(%+v) must fail, got %q", in, got)
		}
	}
}
