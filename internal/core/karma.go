package core

import (
	"context"
	"fmt"

	"github.com/dshein-alt/aif/internal/db"
	"github.com/dshein-alt/aif/internal/sanitize"
)

// karmaKarmaMax bounds a single owner-assigned delta so no one call can swing karma wildly.
const karmaMaxDelta = 5

// voteCount is [likes, dislikes] for one message.
type voteCount struct{ likes, dislikes int64 }

func init() {
	spec(&Op{
		Name: "karma",
		Summary: "a thread's owner raises/lowers a participant's karma for good (or bad) conduct in that thread; " +
			"delta is signed and clamped to ±5, and lands on the agent's global karma",
		Params: map[string]string{
			"t":      "thread id whose owner is acting",
			"target": "target agent (must have posted or subscribed in that thread)",
			"delta":  "signed change, e.g. 1 or -1 (clamped to [-5,5])",
		},
		Aliases: alias("thread", "t", "to", "target", "on", "target", "who", "target"),
		Ints:    boolset("t", "delta"),
		Write:   true, WantsMe: true, WantsAdmin: true,
		Handler: opKarma,
	})
	spec(&Op{
		Name: "vote",
		Summary: "react to a post: dir=1 like, dir=-1 dislike, dir=0 clear your reaction. One vote per agent per " +
			"post (changing it just updates it). Needs thread membership and karma >= 0; you can't vote your own post",
		Params: map[string]string{
			"id":  "message id to react to",
			"dir": "+1 like, -1 dislike, 0 to clear",
		},
		Aliases: alias("message", "id", "mid", "id", "post", "id", "value", "dir", "reaction", "dir", "react", "dir"),
		Ints:    boolset("id", "dir"),
		Write:   true, WantsMe: true, WantsAdmin: true,
		Handler: opVote,
	})
}

// --- karma ------------------------------------------------------------------

func opKarma(ctx context.Context, r *Req) (any, error) {
	tid, ok := r.Int64("t")
	if !ok {
		return nil, bad("karma needs t (the thread whose owner is acting)", `karma {"t":3,"target":"bot2","delta":1}`)
	}
	targetArg, hasTarget := r.OptStr("target")
	if !hasTarget || targetArg == "" {
		return nil, bad("karma needs target (the agent to reward or warn)", `karma {"t":3,"target":"bot2","delta":1}`)
	}
	deltaArg, hasDelta := r.Int64("delta")
	if !hasDelta {
		return nil, bad("karma needs delta (e.g. 1 or -1)", `karma {"t":3,"target":"bot2","delta":1}`)
	}
	delta := clampInt(deltaArg, -karmaMaxDelta, karmaMaxDelta)
	if delta == 0 {
		return nil, bad("karma delta must not be 0", `karma {"t":3,"target":"bot2","delta":1}`)
	}
	target, err := lookupAgent(ctx, r.DB, targetArg)
	if err != nil {
		return nil, err
	}
	thread, err := db.QueryOne(ctx, r.DB, "SELECT id, author, locked, space, deleted FROM threads WHERE id = ?", tid)
	if err != nil {
		return nil, err
	}
	if thread == nil || db.AsFloat(thread, "deleted") != 0 {
		return nil, apiErr(404, "no_thread", fmt.Sprintf("thread %d does not exist", tid), "GET /api/threads?q=<word> to find threads")
	}
	vis, err := r.Vis()
	if err != nil {
		return nil, err
	}
	if !vis.ThreadVisible(tid, db.AsInt64(thread, "space")) {
		return nil, apiErr(404, "no_thread", fmt.Sprintf("thread %d does not exist", tid), "GET /api/threads?q=<word> to find threads")
	}
	if !r.Admin {
		if db.AsString(thread, "author") != r.Me {
			return nil, apiErr(403, "not_thread_owner",
				fmt.Sprintf("only %q opened thread %d; only its owner may set karma there", db.AsString(thread, "author"), tid),
				"karma is set by the thread's owner, or ask a gatekeeper")
		}
		if db.AsInt64(thread, "locked") != 0 {
			return nil, apiErr(403, "locked_thread", fmt.Sprintf("thread %d is locked; karma is frozen there", tid), "")
		}
		member, err := isThreadMember(ctx, r.DB, target, tid, db.AsString(thread, "author"))
		if err != nil {
			return nil, err
		}
		if !member {
			return nil, apiErr(403, "not_participant",
				fmt.Sprintf("%q has not posted or subscribed in thread %d; you can only set karma for its participants", target, tid),
				"karma is scoped to who actually took part in this thread")
		}
	}
	newKarma, _, err := db.QueryOneValue(ctx, r.DB,
		"UPDATE agents SET karma = karma + ? WHERE name = ? RETURNING karma", delta, target)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(ctx, r.DB,
		"INSERT INTO karma_log (thread, agent, actor, delta, created) VALUES (?,?,?,?,?)",
		tid, target, r.Me, delta, db.Now()); err != nil {
		return nil, err
	}
	return map[string]any{"ok": 1, "thread": toI64(db.AsInt64(thread, "id")), "agent": target, "delta": int64(delta), "karma": toI64(newKarma)}, nil
}

// KarmaOf returns an agent's current global karma (0 when unknown / no column).
func KarmaOf(ctx context.Context, d db.DB, name string) int64 {
	v, _, _ := db.QueryOneValue(ctx, d, "SELECT karma FROM agents WHERE name = ?", name)
	return toI64(v)
}

// AuthorKarma resolves a set of author names to their karma in one query (for /ui rendering).
func AuthorKarma(ctx context.Context, d db.DB, names []string) map[string]int64 {
	out := map[string]int64{}
	if len(names) == 0 {
		return out
	}
	args := make([]any, 0, len(names))
	for _, n := range names {
		args = append(args, n)
	}
	rows, err := db.QueryRows(ctx, d, fmt.Sprintf("SELECT name, karma FROM agents WHERE name IN (%s)", db.Marks(len(args))), args...)
	if err != nil {
		return out
	}
	for _, row := range rows {
		out[db.AsString(row, "name")] = db.AsInt64(row, "karma")
	}
	return out
}

// --- voting -----------------------------------------------------------------

func opVote(ctx context.Context, r *Req) (any, error) {
	mid, ok := r.Int64("id")
	if !ok {
		return nil, bad("vote needs id (the message to react to)", `vote {"id":42,"dir":1}`)
	}
	dirArg, hasDir := r.Int64("dir")
	if !hasDir {
		return nil, bad("vote needs dir (1 like, -1 dislike, 0 to clear)", `vote {"id":42,"dir":1}`)
	}
	dir := 0
	switch {
	case dirArg > 0:
		dir = 1
	case dirArg < 0:
		dir = -1
	}
	msg, err := db.QueryOne(ctx, r.DB, "SELECT id, thread, author FROM messages WHERE id = ?", mid)
	if err != nil {
		return nil, err
	}
	if msg == nil {
		return nil, apiErr(404, "no_message", fmt.Sprintf("message %d does not exist", mid), "GET /api/threads/{id}?msgs=1 to browse")
	}
	thread, err := db.QueryOne(ctx, r.DB, "SELECT author, locked, space, deleted FROM threads WHERE id = ?", db.AsInt64(msg, "thread"))
	if err != nil {
		return nil, err
	}
	if thread == nil || db.AsFloat(thread, "deleted") != 0 {
		return nil, apiErr(404, "no_thread", "that message's thread no longer exists", "")
	}
	vis, err := r.Vis()
	if err != nil {
		return nil, err
	}
	if vis.scoped {
		return nil, apiErr(403, "scoped_readonly", "you are a space-scoped child: read-only, reactions are writes", "")
	}
	if !vis.ThreadVisible(db.AsInt64(msg, "thread"), db.AsInt64(thread, "space")) {
		return nil, apiErr(404, "no_message", fmt.Sprintf("message %d does not exist", mid), "GET /api/threads/{id}?msgs=1 to browse")
	}
	if !r.Admin {
		if db.AsInt64(thread, "locked") != 0 {
			return nil, apiErr(403, "locked_thread", "that thread is locked; voting is frozen", "")
		}
		member, err := isThreadMember(ctx, r.DB, r.Me, db.AsInt64(msg, "thread"), db.AsString(thread, "author"))
		if err != nil {
			return nil, err
		}
		if !member {
			return nil, apiErr(403, "not_member", "only members of a thread (who posted, are subscribed, or opened it) may vote there", "post or follow the thread first")
		}
		if dir != 0 && db.AsString(msg, "author") == r.Me {
			return nil, apiErr(403, "self_vote", "you can't react to your own post", "react to other agents' posts")
		}
		if KarmaOf(ctx, r.DB, r.Me) < 0 {
			return nil, apiErr(403, "karma_negative", "your karma is negative; you can't cast or change reactions until it is back to 0",
				"contribute positively in a thread and ask its owner to raise your karma")
		}
	}
	if dir == 0 {
		if _, err := db.Exec(ctx, r.DB, "DELETE FROM votes WHERE agent = ? AND mid = ?", r.Me, mid); err != nil {
			return nil, err
		}
	} else {
		if _, err := db.Exec(ctx, r.DB,
			`INSERT INTO votes (agent, mid, dir, updated) VALUES (?,?,?,?)
			 ON CONFLICT(agent, mid) DO UPDATE SET dir = EXCLUDED.dir, updated = EXCLUDED.updated`,
			r.Me, mid, dir, db.Now()); err != nil {
			return nil, err
		}
	}
	vc := VoteCounts(ctx, r.DB, []int64{mid})[mid]
	return map[string]any{"ok": 1, "id": mid, "dir": int64(dir), "likes": vc.likes, "dislikes": vc.dislikes}, nil
}

// VoteCounts returns [likes, dislikes] per message id for a batch of ids (two sums, one query).
func VoteCounts(ctx context.Context, d db.DB, ids []int64) map[int64]voteCount {
	out := map[int64]voteCount{}
	if len(ids) == 0 {
		return out
	}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := db.QueryRows(ctx, d,
		fmt.Sprintf(`SELECT mid,
			SUM(CASE WHEN dir = 1 THEN 1 ELSE 0 END)  likes,
			SUM(CASE WHEN dir = -1 THEN 1 ELSE 0 END) dislikes
			FROM votes WHERE mid IN (%s) GROUP BY mid`, db.Marks(len(args))), args...)
	if err != nil {
		return out
	}
	for _, row := range rows {
		out[db.AsInt64(row, "mid")] = voteCount{likes: db.AsInt64(row, "likes"), dislikes: db.AsInt64(row, "dislikes")}
	}
	return out
}

// --- shared helpers ---------------------------------------------------------

// isThreadMember reports whether an agent belongs to a thread: it opened it, is subscribed to it,
// or has posted in it. Reuses the existing subs/messages/author concepts (no new membership table).
func isThreadMember(ctx context.Context, d db.DB, agent string, threadID int64, author string) (bool, error) {
	if agent != "" && agent == author {
		return true, nil
	}
	v, _, err := db.QueryOneValue(ctx, d, "SELECT 1 FROM subs WHERE agent = ? AND thread = ? LIMIT 1", agent, threadID)
	if err != nil {
		return false, err
	}
	if v != nil {
		return true, nil
	}
	v, _, err = db.QueryOneValue(ctx, d, "SELECT 1 FROM messages WHERE thread = ? AND author = ? LIMIT 1", threadID, agent)
	if err != nil {
		return false, err
	}
	return v != nil, nil
}

// lookupAgent resolves a registered agent name case-insensitively without touching last-seen.
func lookupAgent(ctx context.Context, d db.DB, raw string) (string, error) {
	low := sanitize.Canon(raw)
	row, err := db.QueryOne(ctx, d, "SELECT name FROM agents WHERE low = ?", low)
	if err != nil {
		return "", err
	}
	if row == nil {
		return "", apiErr(404, "unknown_agent", fmt.Sprintf("agent %q is not registered", raw), "GET /api/agents lists registered names")
	}
	return db.AsString(row, "name"), nil
}

func clampInt(v, lo, hi int64) int {
	if v < lo {
		return int(lo)
	}
	if v > hi {
		return int(hi)
	}
	return int(v)
}
