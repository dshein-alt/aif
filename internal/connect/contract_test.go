package connect

import (
	"regexp"
	"strings"
	"testing"
)

// Ported from plugin/pi/test/contract.test.mjs against the new, connector-owned wording:
// no SHUTDOWN/RESET steps, no resident_inbox/resident_memory, no DONE/BLOCKED, a fixed home
// thread, and generic tool names with a per-harness ToolHint inserted after the tool list.

func contract(overrides ...func(*contractArgs)) string {
	a := contractArgs{
		name:      "Tess",
		thread:    3,
		operators: []string{"TheRoot", "gatekeeper"},
		role:      "ROLE TEXT",
		toolHint:  "This harness exposes them as mcp__aif__<tool>.",
	}
	for _, o := range overrides {
		o(&a)
	}
	return Contract(a.name, a.thread, a.operators, a.role, a.toolHint)
}

type contractArgs struct {
	name      string
	thread    int64
	operators []string
	role      string
	toolHint  string
}

// steps extracts the numbered turn-loop lines, the same way the JS test does: split on the
// "Every turn, in this order:" marker, then pull out lines starting "<N>. ".
func steps(t *testing.T, text string) map[int]string {
	t.Helper()
	parts := strings.SplitN(text, "Every turn, in this order:\n", 2)
	if len(parts) != 2 {
		t.Fatalf("contract must contain a numbered turn loop")
	}
	out := map[int]string{}
	re := regexp.MustCompile(`^(\d+)\. `)
	for _, line := range strings.Split(parts[1], "\n") {
		m := re.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		n := 0
		for _, c := range m[1] {
			n = n*10 + int(c-'0')
		}
		out[n] = line
	}
	return out
}

func TestTurnLoopNumberedContiguously(t *testing.T) {
	numbered := steps(t, contract())
	var keys []int
	for k := range numbered {
		keys = append(keys, k)
	}
	if len(keys) != 4 {
		t.Fatalf("expected 4 turn steps (whoami, read, reply, clear), got %d: %v", len(keys), numbered)
	}
	for n := 1; n <= 4; n++ {
		if _, ok := numbered[n]; !ok {
			t.Fatalf("missing contiguous step %d, got %v", n, numbered)
		}
	}
}

func TestIdentityConfirmedFirst(t *testing.T) {
	numbered := steps(t, contract())
	if !regexp.MustCompile(`^1\. Call whoami first\.`).MatchString(numbered[1]) {
		t.Fatalf("step 1 must open with 'Call whoami first.', got %q", numbered[1])
	}
	if strings.Contains(numbered[1], "claim_required") {
		t.Fatalf("claim_required branch must be gone from step 1 (startup whoami covers it): %q", numbered[1])
	}
}

func TestInboxReadWithoutAdvancing(t *testing.T) {
	numbered := steps(t, contract())
	read := numbered[2]
	if !strings.Contains(read, "`advance: 0`") {
		t.Fatalf("read step must use advance: 0, got %q", read)
	}
	if strings.Contains(read, "`seen`") || strings.Contains(read, "seq") {
		t.Fatalf("the read step must not also clear the inbox: %q", read)
	}
	if !strings.Contains(read, "gone forever") {
		t.Fatalf("read step must warn that clearing is irreversible: %q", read)
	}
	if !strings.Contains(read, "highest message id") {
		t.Fatalf("read step must ask to note the highest message id: %q", read)
	}
}

func TestClearingOnlyAfterReplies(t *testing.T) {
	numbered := steps(t, contract())
	read, reply, clear := numbered[2], numbered[3], numbered[4]
	if strings.Contains(read, "`seen`") || strings.Contains(reply, "`seen`") {
		t.Fatalf("no step before the clearing step may move the read cursor: read=%q reply=%q", read, reply)
	}
	if !strings.Contains(clear, "`seen`") {
		t.Fatalf("step 4 must be the clearing step: %q", clear)
	}
	if !strings.Contains(reply, "If a reply cannot be posted, stop before step 4") {
		t.Fatalf("reply step must say to stop before clearing on a failed post: %q", reply)
	}
	if !strings.Contains(reply, "before this turn ends") {
		t.Fatalf("reply step must require one reply per tagging message before the turn ends: %q", reply)
	}
	if !strings.Contains(clear, "If a reply is still owed, clear only up to the id just below the oldest owed message") {
		t.Fatalf("clear step must bound itself below any owed reply: %q", clear)
	}
}

func TestClearingIsBoundedNoLiteralSeq(t *testing.T) {
	text := contract()
	if !strings.Contains(text, "Never pass `seq 0` (it clears the whole forum)") {
		t.Fatalf("contract must ban seq 0: %q", text)
	}
	seenCalls := 0
	re := regexp.MustCompile("`seq=([^`]*)`")
	for _, line := range steps(t, text) {
		for _, m := range re.FindAllStringSubmatch(line, -1) {
			seenCalls++
			if !strings.HasPrefix(m[1], "<") {
				t.Fatalf("a clearing step must name a bounded id, got %q", m[1])
			}
		}
	}
	if seenCalls != 1 {
		t.Fatalf("expected exactly one seen call in the turn loop (SHUTDOWN/RESET clears are gone), got %d", seenCalls)
	}
}

func TestNoShutdownOrResetStep(t *testing.T) {
	text := contract()
	for _, line := range steps(t, text) {
		if strings.Contains(line, "SHUTDOWN") || strings.Contains(line, "RESET") {
			t.Fatalf("SHUTDOWN/RESET must not be numbered turn-loop steps (the connector executes them): %q", line)
		}
	}
	if !strings.Contains(text, "#CMD[") {
		t.Fatalf("contract must mention the #CMD[...]# marker: %q", text)
	}
	if !strings.Contains(text, "ignore") {
		t.Fatalf("contract must say the model ignores #CMD[...]# markers: %q", text)
	}
}

func TestReplyExistsOnlyAsPostWithID(t *testing.T) {
	text := contract()
	if !strings.Contains(text, "A reply exists only when you called the AIF tool `post` and got back an id") {
		t.Fatalf("missing the reply-must-return-an-id rule: %q", text)
	}
}

func TestHomeThreadIsAlwaysConfigured(t *testing.T) {
	text := contract()
	if !strings.Contains(text, "Your home thread is 3.") {
		t.Fatalf("home thread line missing or wrong: %q", text)
	}
	if strings.Contains(text, "create one with") || strings.Contains(text, "HOME_THREAD") {
		t.Fatalf("the create-your-own-thread branch must be gone: %q", text)
	}
}

func TestDoneAndBlockedAreGone(t *testing.T) {
	text := contract()
	if strings.Contains(text, "DONE") || strings.Contains(text, "BLOCKED") {
		t.Fatalf("DONE/BLOCKED files must be gone: %q", text)
	}
	if !strings.Contains(text, "idle") {
		t.Fatalf("contract must say a finished goal means the resident idles until stop/SHUTDOWN: %q", text)
	}
	if !strings.Contains(text, "tagging the operators") {
		t.Fatalf("an unanswerable question must be asked in the home thread, tagging the operators: %q", text)
	}
}

func TestNoOneBoundedStepLanguage(t *testing.T) {
	text := contract()
	if strings.Contains(text, "bounded, verifiable step") || strings.Contains(text, "one bounded step") {
		t.Fatalf("the one-bounded-step language must be gone: %q", text)
	}
	if !strings.Contains(text, "end the turn when nothing is left that you can do now") {
		t.Fatalf("contract must say to end the turn when nothing is left that can be done now: %q", text)
	}
}

func TestResidentInboxAndMemoryGoneNoteToolDescribed(t *testing.T) {
	text := contract()
	if strings.Contains(text, "resident_inbox") || strings.Contains(text, "resident_memory") {
		t.Fatalf("resident_inbox/resident_memory must be gone: %q", text)
	}
	if strings.Contains(text, "GOAL.md") || strings.Contains(text, "journal.md") {
		t.Fatalf("GOAL.md/journal.md must be gone: %q", text)
	}
	if !strings.Contains(text, "aif-connect note set|get|delete|list") {
		t.Fatalf("contract must describe the note tool by its exact invocation: %q", text)
	}
	if !strings.Contains(text, "shell tool") {
		t.Fatalf("contract must say the note tool runs through the shell tool: %q", text)
	}
	if !strings.Contains(text, "outlive the session") {
		t.Fatalf("contract must say notes are for facts that must outlive the session: %q", text)
	}
	if !strings.Contains(text, "note list") {
		t.Fatalf("contract must say a fresh session reads note list first: %q", text)
	}
}

func TestToolsNamedGenericallyWithToolHintAfterList(t *testing.T) {
	text := contract()
	if !strings.Contains(text, "`whoami`") || !strings.Contains(text, "`unread` with `advance: 0`") {
		t.Fatalf("tools must be named generically: %q", text)
	}
	lines := strings.Split(text, "\n")
	toolListIdx, hintIdx := -1, -1
	for i, l := range lines {
		if strings.Contains(l, "`whoami`") && strings.Contains(l, "`unread`") {
			toolListIdx = i
		}
		if l == "This harness exposes them as mcp__aif__<tool>." {
			hintIdx = i
		}
	}
	if toolListIdx == -1 {
		t.Fatalf("could not find the tool-list line: %q", text)
	}
	if hintIdx != toolListIdx+1 {
		t.Fatalf("toolHint must be inserted right after the tool list line, got tool list at %d, hint at %d", toolListIdx, hintIdx)
	}
}

func TestRoleSectionIsLastAndMayBeEmpty(t *testing.T) {
	text := contract()
	if !strings.HasSuffix(text, "ROLE (supplied by the operator):\nROLE TEXT") {
		t.Fatalf("ROLE section must be last: %q", text)
	}

	empty := contract(func(a *contractArgs) { a.role = "" })
	if !strings.HasSuffix(empty, "ROLE (supplied by the operator):\n") {
		t.Fatalf("an empty ROLE must still render the header with nothing after it: %q", empty)
	}
}

func TestOperatorsAppearForUnansweredQuestions(t *testing.T) {
	text := contract()
	if !strings.Contains(text, "TheRoot") || !strings.Contains(text, "gatekeeper") {
		t.Fatalf("operators must be named somewhere in the contract: %q", text)
	}
}
