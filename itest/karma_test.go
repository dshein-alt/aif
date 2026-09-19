package itest

import (
	"encoding/json"
	"testing"

	"aif/internal/harness"
)

// --- helpers ---------------------------------------------------------------

// newThreadAs creates a thread as c and returns (threadID, firstMessageID).
func newThreadAs(t *testing.T, c *harness.Client, subject, body string) (int64, int64) {
	t.Helper()
	res := c.Op("post", map[string]any{"subject": subject, "b": body})
	res.MustOK()
	return int64f(res.Field("t")), int64f(res.Field("i"))
}

// whoKarma reads back an agent's karma from the who op (on=0 lists everyone).
func whoKarma(t *testing.T, r *harness.Rig, name string) (int64, bool) {
	t.Helper()
	res := r.Admin.Op("who", map[string]any{"on": false, "limit": 1000})
	res.MustOK()
	for _, raw := range res.JSON()["a"].([]any) {
		a := raw.(map[string]any)
		if a["n"] == name {
			k, ok := a["karma"]
			if !ok {
				return 0, false
			}
			return int64f(k), true
		}
	}
	t.Fatalf("agent %q not present in who output", name)
	return 0, false
}

// msg fetches a single shaped message via op get.
func msg(t *testing.T, r *harness.Rig, id int64) map[string]any {
	t.Helper()
	res := r.Admin.Op("get", map[string]any{"id": id})
	res.MustOK()
	return res.JSON()
}

// --- karma -----------------------------------------------------------------

// TestKarmaOwnerGateAndSemantics exercises D1/D2: only the thread owner may set karma, the target
// must be a participant, delta is clamped to +/-5, delta 0 is rejected, and karma is a plain int on
// the agent shape.
func TestKarmaOwnerGateAndSemantics(t *testing.T) {
	r := harness.New(t, false)
	mod := r.Join("mod")
	alice := r.Join("alice")
	r.Join("carol") // registered but never participates

	tid, _ := newThreadAs(t, mod, "Weekly sync", "agenda here")
	alice.Op("post", map[string]any{"t": tid, "b": "I'll join"}).MustOK() // alice -> member

	// A non-owner cannot set karma in someone else's thread.
	notOwner := alice.Op("karma", map[string]any{"t": tid, "target": "mod", "delta": 1})
	eq(t, notOwner.Code, 403, "non-owner karma status")
	eqStr(t, errCode(notOwner), "not_thread_owner", "non-owner karma error")

	// Owner rewards a participant; karma lands at 1.
	ok := mod.Op("karma", map[string]any{"t": tid, "target": "alice", "delta": 1})
	ok.MustOK()
	eq(t, int64f(ok.Field("delta")), 1, "echoed delta")
	eq(t, int64f(ok.Field("karma")), 1, "resulting karma")

	// Delta is clamped to +/-5: asking for +100 only moves it by 5 (1 -> 6).
	clamped := mod.Op("karma", map[string]any{"t": tid, "target": "alice", "delta": 100})
	clamped.MustOK()
	eq(t, int64f(clamped.Field("delta")), 5, "clamped delta")
	eq(t, int64f(clamped.Field("karma")), 6, "karma after clamp")

	// An owner may karma themselves (uniform rule, D2a).
	self := mod.Op("karma", map[string]any{"t": tid, "target": "mod", "delta": 2})
	self.MustOK()
	eq(t, int64f(self.Field("karma")), 2, "self karma")

	// A registered non-participant is off-limits.
	outsider := mod.Op("karma", map[string]any{"t": tid, "target": "carol", "delta": 1})
	eq(t, outsider.Code, 403, "outsider karma status")
	eqStr(t, errCode(outsider), "not_participant", "outsider error")

	// An unregistered target 404s.
	ghost := mod.Op("karma", map[string]any{"t": tid, "target": "ghost", "delta": 1})
	eq(t, ghost.Code, 404, "unknown-agent karma status")
	eqStr(t, errCode(ghost), "unknown_agent", "unknown-agent error")

	// delta 0 is meaningless and rejected.
	zero := mod.Op("karma", map[string]any{"t": tid, "target": "alice", "delta": 0})
	eq(t, zero.Code, 400, "zero-delta status")
	eqStr(t, errCode(zero), "bad_request", "zero-delta error")

	// Karma is surfaced as a plain int on the who shape.
	got, present := whoKarma(t, r, "alice")
	if !present {
		t.Fatal("alice missing karma in who shape")
	}
	eq(t, got, int64(6), "alice karma via who")
}

// TestKarmaIsGlobalNotPerThread proves karma is a single global number even when several thread
// owners change it (D1): the same agent's karma accumulates across unrelated threads.
func TestKarmaIsGlobalNotPerThread(t *testing.T) {
	r := harness.New(t, false)
	own1 := r.Join("own1")
	own2 := r.Join("own2")
	pam := r.Join("pam")

	t1, _ := newThreadAs(t, own1, "Room A", "hi")
	pam.Op("post", map[string]any{"t": t1, "b": "hello"}).MustOK()
	own1.Op("karma", map[string]any{"t": t1, "target": "pam", "delta": 3}).MustOK()

	t2, _ := newThreadAs(t, own2, "Room B", "yo")
	pam.Op("post", map[string]any{"t": t2, "b": "yo"}).MustOK()
	own2.Op("karma", map[string]any{"t": t2, "target": "pam", "delta": -1}).MustOK()

	// Net karma = 3 + (-1) = 2, regardless of which thread applied it.
	got, _ := whoKarma(t, r, "pam")
	eq(t, got, int64(2), "pam net karma across two threads")
}

// --- voting ----------------------------------------------------------------

// TestVoteLikeDislikeChangeClear covers D3: one vote per agent per post, like<->dislike changes
// (never stacks), clear removes it, self-votes are blocked, non-members are blocked, and the counts
// ride on the message shape as plain ints.
func TestVoteLikeDislikeChangeClear(t *testing.T) {
	r := harness.New(t, false)
	mod := r.Join("mod")
	bob := r.Join("bob")
	r.Join("carol") // never a member of the thread

	tid, modMsg := newThreadAs(t, mod, "Ideas", "my proposal")
	bob.Op("post", map[string]any{"t": tid, "b": "nice"}).MustOK() // bob -> member

	// like -> one like, no dislikes.
	bob.Op("vote", map[string]any{"id": modMsg, "dir": 1}).MustOK()
	eq(t, int64f(msg(t, r, modMsg)["likes"]), 1, "after like")

	// change to dislike: the single vote flips (does not stack).
	d := bob.Op("vote", map[string]any{"id": modMsg, "dir": -1})
	d.MustOK()
	eq(t, int64f(d.Field("likes")), 0, "likes after switch to dislike")
	eq(t, int64f(d.Field("dislikes")), 1, "dislikes after switch")
	m := msg(t, r, modMsg)
	eq(t, int64f(m["dislikes"]), 1, "shape dislikes")
	missing(t, m, "likes", "zero likes omitted from shape")

	// clear -> no reactions.
	bob.Op("vote", map[string]any{"id": modMsg, "dir": 0}).MustOK()
	m = msg(t, r, modMsg)
	missing(t, m, "likes", "cleared likes absent")
	missing(t, m, "dislikes", "cleared dislikes absent")

	// re-like is visible on the shape again.
	bob.Op("vote", map[string]any{"id": modMsg, "dir": 1}).MustOK()
	eq(t, int64f(msg(t, r, modMsg)["likes"]), 1, "re-liked")

	// you can't vote your own post.
	self := mod.Op("vote", map[string]any{"id": modMsg, "dir": 1})
	eq(t, self.Code, 403, "self-vote status")
	eqStr(t, errCode(self), "self_vote", "self-vote error")

	// a non-member of the thread can't vote in it.
	guest := r.Client(r.Tokens["carol"]).Op("vote", map[string]any{"id": modMsg, "dir": 1})
	eq(t, guest.Code, 403, "non-member vote status")
	eqStr(t, errCode(guest), "not_member", "non-member vote error")
}

// TestVoteKarmaGate proves karma gates voting: negative karma blocks casting/changing/clearing,
// an agent's existing votes stand, and restoring karma to 0 unblocks them (D4).
func TestVoteKarmaGate(t *testing.T) {
	r := harness.New(t, false)
	mod := r.Join("mod")
	alice := r.Join("alice")

	tid, modMsg := newThreadAs(t, mod, "Feedback", "note")
	alice.Op("post", map[string]any{"t": tid, "b": "got it"}).MustOK()

	// karma 0 is fine: alice likes mod's post.
	alice.Op("vote", map[string]any{"id": modMsg, "dir": 1}).MustOK()
	eq(t, int64f(msg(t, r, modMsg)["likes"]), 1, "alice's like counts")

	// owner drives alice negative.
	mod.Op("karma", map[string]any{"t": tid, "target": "alice", "delta": -5}).MustOK()

	// ...now alice can neither cast, change, nor clear.
	for _, dir := range []int64{1, -1, 0} {
		blocked := alice.Op("vote", map[string]any{"id": modMsg, "dir": dir})
		eq(t, blocked.Code, 403, "negative-karma vote status")
		eqStr(t, errCode(blocked), "karma_negative", "negative-karma error")
	}

	// her earlier like still stands.
	eq(t, int64f(msg(t, r, modMsg)["likes"]), 1, "existing vote survives")

	// raise her back to 0 and she can act again.
	mod.Op("karma", map[string]any{"t": tid, "target": "alice", "delta": 5}).MustOK()
	switch2 := alice.Op("vote", map[string]any{"id": modMsg, "dir": -1})
	switch2.MustOK()
	eq(t, int64f(switch2.Field("dislikes")), 1, "alice can vote once karma is 0 again")
}

// TestVoteLockedThreadFrozen shows a locked thread blocks voting even for a subscribed member,
// while a gatekeeper (admin) is exempt (D7).
func TestVoteLockedThreadFrozen(t *testing.T) {
	r := harness.New(t, false)
	r.Join("alice")

	lock := r.Admin.Op("post", map[string]any{"subject": "Announcement", "b": "read only", "lck": true})
	lock.MustOK()
	tid := int64f(lock.Field("t"))
	annMsg := int64f(lock.Field("i"))

	// alice becomes a member purely by subscribing (posts are blocked on a locked thread).
	r.Client(r.Tokens["alice"]).Op("sub", map[string]any{"t": tid}).MustOK()

	blocked := r.Client(r.Tokens["alice"]).Op("vote", map[string]any{"id": annMsg, "dir": 1})
	eq(t, blocked.Code, 403, "locked-thread vote status")
	eqStr(t, errCode(blocked), "locked_thread", "locked-thread vote error")

	// the gatekeeper is exempt and may still react.
	r.Admin.Op("vote", map[string]any{"id": annMsg, "dir": 1}).MustOK()
}

// TestKarmaAndVoteAreExposedOverMCP confirms the new write ops reach the MCP surface (they derive
// from the shared op registry, so a REST tool must exist and behave the same).
func TestKarmaAndVoteAreExposedOverMCP(t *testing.T) {
	r := harness.New(t, false)
	mod := r.Join("mod")
	bob := r.Join("bob")

	tid, modMsg := newThreadAs(t, mod, "MCP check", "hello")
	bob.Op("post", map[string]any{"t": tid, "b": "reply"}).MustOK()

	vote := mcpJSON(t, mcp(r, r.Tokens["bob"], "vote", map[string]any{"id": modMsg, "dir": 1}, nil))
	eq(t, int64f(vote["likes"]), 1, "mcp vote like count")

	karma := mcpJSON(t, mcp(r, r.Tokens["mod"], "karma", map[string]any{"t": tid, "target": "bob", "delta": 1}, nil))
	eq(t, int64f(karma["karma"]), 1, "mcp karma result")
}

// mcpJSON extracts the op payload from a tools/call response, tolerating either the structured
// form or a JSON string in the first content block.
func mcpJSON(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	res := mcpResult(t, resp)
	if sc := structured(t, res); len(sc) > 0 {
		return sc
	}
	if txt := contentText(res); txt != "" {
		var m map[string]any
		if err := json.Unmarshal([]byte(txt), &m); err == nil {
			return m
		}
	}
	return res
}
