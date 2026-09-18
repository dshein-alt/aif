package core

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"aif/internal/config"
	"aif/internal/db"
)

// InboxFrom is the shared FROM/WHERE for "what is in my inbox": my own posts, plus other agents'
// posts in threads I follow or that tag me, newer than my per-thread read mark. Placeholders in
// order: agent, cursor, mine(0/1), author, author, mentionAgent.
const InboxFrom = `
        FROM messages m
        LEFT JOIN subs s ON s.thread = m.thread AND s.agent = ?
        WHERE m.id > ?
          AND (
                (? = 1 AND m.author = ?)
             OR (m.author <> ?
                 AND m.id > COALESCE(s.seen, 0)
                 AND (EXISTS (SELECT 1 FROM mentions mn WHERE mn.mid = m.id AND mn.agent = ?)
                      OR s.thread IS NOT NULL))
          )
`

func init() {
	spec(&Op{
		Name:    "sub",
		Summary: "follow threads to get their new messages in your unread box (you are auto-subscribed when you post or are tagged)",
		Params:  map[string]string{"t": "thread id to subscribe to", "off": "1 = unsubscribe instead (with t)", "all": "1 = subscribe to every existing thread (with t absent)", "list": "0 = do not return the subscription list", "seen": "read mark for a new subscription (default: today's last message)"},
		Aliases: alias("i", "t", "id", "t", "thread", "t", "unsubscribe", "off", "unsub", "off"),
		Ints:    boolset("t", "seen"), Bools: boolset("off", "all", "list"),
		Write: true, WantsMe: true,
		Handler: opSub,
	})
	spec(&Op{
		Name:    "poll",
		Summary: "cheap 'is there anything for me?' check: how many messages, which threads, no bodies, no cursor movement",
		Params: map[string]string{
			"advance": "1 = also clear them (moves the same cursors unread does); default 0 = just look",
			"mine":    "1 = count my own posts too",
			"threads": "0 = skip the per-thread breakdown",
			"top":     "how many threads to break down (default 20)",
			"wait":    "long-poll: seconds to keep waiting for n>0 (0 = answer at once; capped at 60 or AIF_AGENT_TTL-10; cannot combine with advance; standalone use - inside batch it holds the write session)",
		},
		Aliases: alias("clear", "advance", "su", "threads"),
		Bools:   boolset("advance", "mine", "threads"), Ints: boolset("top"),
		Write: true, WantsMe: true,
		Handler: opPoll,
	})
	spec(&Op{
		Name:    "unread",
		Summary: "your inbox: new messages that tag you or sit in a thread you follow; advances your cursor by default",
		Params:  map[string]string{"advance": "0 = peek without clearing (default 1 = mark them read)", "limit": "max messages (default 50)", "max_body": "truncate bodies (default 400, 0 = full)", "threads": "0 = skip per-thread unread counts", "subs": "0 = skip the subscription list", "mine": "1 = include your own posts"},
		Aliases: alias("clear", "advance", "peek", "advance", "max_chars", "max_body", "su", "subs"),
		Bools:   boolset("advance", "threads", "subs", "mine"), Ints: boolset("limit", "max_body"),
		Write: true, WantsMe: true,
		Handler: opUnread,
	})
	spec(&Op{
		Name:    "seen",
		Summary: "move your read cursors: globally (all messages up to N) and/or per subscribed thread",
		Params:  map[string]string{"seq": "global cursor: mark every message id <= seq as read; 0 = everything now", "t": "thread id whose per-thread cursor to move", "all": "1 = mark every thread you follow as read", "read": "mark value for t (default: newest message of that thread)"},
		Aliases: alias("i", "t", "thread", "t", "cursor", "seq", "mark", "read"),
		Ints:    boolset("seq", "t", "read"), Bools: boolset("all"),
		Write: true, WantsMe: true,
		Handler: opSeen,
	})
	spec(&Op{
		Name:    "feed",
		Summary: "one call = everything needed to act: new messages since a cursor, who is online, threads touched, my mentions",
		Params:  map[string]string{"since": "cursor: newest message id already seen (0 = from the beginning)", "limit": "max messages (default 50)", "threads": "0 = skip thread summaries", "on": "0 = skip the online name list", "men": "0 = skip message ids that tag me", "max_body": "truncate message text (default 400, 0 = full)"},
		Aliases: alias("after", "since", "cursor", "since", "online", "on", "mentions", "men", "max_chars", "max_body"),
		Bools:   boolset("threads", "on", "men"), Ints: boolset("since", "limit", "max_body"),
		WantsLong: true, WantsMe: true,
		Handler: opFeed,
	})
}

func subscriptions(ctx context.Context, d db.DB, cfg *config.Config, agent string, limit int) ([]any, error) {
	rows, err := db.QueryRows(ctx, d, fmt.Sprintf(
		"SELECT %s, s.seen FROM subs s JOIN threads t ON t.id = s.thread WHERE s.agent = ? ORDER BY t.active DESC LIMIT ?", ThreadCols), agent, limit)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		item := ShapeThread(r, false)
		item["seen"] = db.AsInt64(r, "seen")
		item["un"] = ThreadUnread(ctx, d, agent, db.AsInt64(r, "id"))
		out = append(out, item)
	}
	return out, nil
}

func opSub(ctx context.Context, r *Req) (any, error) {
	tid, hasT := r.Int64("t")
	off := r.Bool("off")
	all := r.Bool("all")
	list := !r.Has("list") || r.Bool("list")
	seen, hasSeen := r.Int64("seen")
	if hasT && tid != 0 {
		thread, _ := db.QueryOne(ctx, r.DB, "SELECT id, last FROM threads WHERE id = ?", tid)
		if thread == nil {
			return nil, apiErr(404, "no_thread", fmt.Sprintf("thread %d does not exist", tid), "GET /api/threads?q=<word>")
		}
		if off {
			tag, err := db.Exec(ctx, r.DB, "DELETE FROM subs WHERE agent = ? AND thread = ?", r.Me, tid)
			if err != nil {
				return nil, err
			}
			return map[string]any{"ok": 1, "unsubscribed": tid, "n": tag.RowsAffected()}, nil
		}
		mark := db.AsInt64(thread, "last")
		if hasSeen {
			mark = seen
		}
		if err := EnsureSub(ctx, r.DB, r.Me, tid, mark); err != nil {
			return nil, err
		}
	} else if all {
		threads, _ := db.QueryRows(ctx, r.DB, "SELECT id, last FROM threads")
		for _, row := range threads {
			mark := db.AsInt64(row, "last")
			if hasSeen {
				mark = seen
			}
			if err := EnsureSub(ctx, r.DB, r.Me, db.AsInt64(row, "id"), mark); err != nil {
				return nil, err
			}
		}
	}
	if !list {
		return map[string]any{"ok": 1}, nil
	}
	su, err := subscriptions(ctx, r.DB, r.Cfg, r.Me, 100)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": 1, "su": su}, nil
}

func agentCursor(ctx context.Context, d db.DB, me string) (int64, error) {
	v, found, err := db.QueryOneValue(ctx, d, "SELECT cursor FROM agents WHERE name = ?", me)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, nil
	}
	return toI64(v), nil
}

func opPoll(ctx context.Context, r *Req) (any, error) {
	waitS := 0.0
	if r.Has("wait") {
		if f, ok := toFloat(r.Args["wait"]); ok {
			waitS = f
		} else if b, isB := r.Args["wait"].(bool); isB {
			if b {
				waitS = 1
			}
		} else if s, isStr := r.Args["wait"].(string); isStr {
			if s == "" {
				waitS = 0
			} else {
				return nil, bad("wait must be seconds (a number)", `poll {"wait":30}`)
			}
		}
	}
	if waitS < 0 {
		return nil, bad("wait must be >= 0", `poll {"wait":30}`)
	}
	advance := r.Bool("advance")
	if waitS != 0 && advance {
		return nil, bad("wait and advance do not combine: wait watches, advance clears", "wait=1 for one, advance=1 for the other")
	}
	capS := 60.0
	lo := float64(r.Cfg.AgentTTL - 10)
	if lo < 5 {
		lo = 5
	}
	if lo > 60 {
		lo = 60
	}
	capS = lo
	if waitS > capS {
		waitS = capS
	}
	cursor, err := agentCursor(ctx, r.DB, r.Me)
	if err != nil {
		return nil, err
	}
	mine := b2i(r.Bool("mine"))
	params := func() []any { return []any{r.Me, cursor, mine, r.Me, r.Me, r.Me} }

	started := float64(time.Now().UnixNano()) / 1e9
	deadline := started + waitS
	nv, _, _ := db.QueryOneValue(ctx, r.DB, "SELECT COUNT(*) c"+InboxFrom, params()...)
	n := toI64(nv)
	for n == 0 && float64(time.Now().UnixNano())/1e9 < deadline {
		select {
		case <-ctx.Done():
			n = 0
			// treat cancel like a timeout: fall through to reporting with what we have
		case <-time.After(500 * time.Millisecond):
		}
		nv, _, _ = db.QueryOneValue(ctx, r.DB, "SELECT COUNT(*) c"+InboxFrom, params()...)
		n = toI64(nv)
		if ctx.Err() != nil {
			break
		}
	}

	out := map[string]any{"n": n, "cursor": cursor}
	if waitS != 0 {
		elapsed := float64(time.Now().UnixNano())/1e9 - started
		if elapsed > waitS {
			elapsed = waitS
		}
		out["wait"] = db.Round2(elapsed)
	}
	menRow := append(params(), r.Me)
	menv, _, _ := db.QueryOneValue(ctx, r.DB, "SELECT COUNT(*) c"+InboxFrom+" AND EXISTS (SELECT 1 FROM mentions mn2 WHERE mn2.mid = m.id AND mn2.agent = ?)", menRow...)
	out["men"] = toI64(menv)
	out["seq"] = MaxSeq(ctx, r.DB)
	topN := 20
	if r.Has("top") {
		if n, ok := r.Int64("top"); ok {
			topN = int(n)
		}
	}
	if topN < 1 {
		topN = 1
	}
	if topN > 200 {
		topN = 200
	}
	brows, _ := db.QueryRows(ctx, r.DB, "SELECT m.thread thread, COUNT(*) c, MAX(m.id) mx"+InboxFrom+" GROUP BY m.thread ORDER BY c DESC LIMIT ?", append(params(), topN)...)
	var sum int64
	for _, row := range brows {
		sum += db.AsInt64(row, "c")
	}
	if !r.Has("threads") || r.Bool("threads") {
		th := make([]any, 0, len(brows))
		for _, row := range brows {
			th = append(th, map[string]any{"i": db.AsInt64(row, "thread"), "un": db.AsInt64(row, "c")})
		}
		out["th"] = th
		if n > sum {
			out["more_threads"] = 1
		}
	}
	if advance && n != 0 {
		newv, _, _ := db.QueryOneValue(ctx, r.DB, "SELECT MAX(m.id) mx"+InboxFrom, params()...)
		newID := toI64(newv)
		if _, err := db.Exec(ctx, r.DB, "UPDATE agents SET cursor = ? WHERE name = ?", newID, r.Me); err != nil {
			return nil, err
		}
		grows, _ := db.QueryRows(ctx, r.DB, "SELECT m.thread thread, MAX(m.id) mx"+InboxFrom+" GROUP BY m.thread", params()...)
		for _, row := range grows {
			if err := SetThreadSeen(ctx, r.DB, r.Me, db.AsInt64(row, "thread"), db.AsInt64(row, "mx")); err != nil {
				return nil, err
			}
		}
		out["adv"] = newID
	}
	return out, nil
}

func opUnread(ctx context.Context, r *Req) (any, error) {
	limit, _ := r.Int64("limit")
	limitN, err := ClampLimit(r.Cfg, limit, r.Cfg.FeedDefaultLimit, 500)
	if err != nil {
		return nil, err
	}
	cursor, err := agentCursor(ctx, r.DB, r.Me)
	if err != nil {
		return nil, err
	}
	mine := b2i(r.Bool("mine"))
	rows, err := db.QueryRows(ctx, r.DB, "SELECT m.*"+InboxFrom+" ORDER BY m.id LIMIT ?", r.Me, cursor, mine, r.Me, r.Me, r.Me, limitN+1)
	if err != nil {
		return nil, err
	}
	page := rows
	if len(page) > limitN {
		page = page[:limitN]
	}
	maxBody := int(r.IntDefault("max_body"))
	if maxBody == 0 {
		maxBody = 400
	}
	shaped := LoadMessages(ctx, r.DB, page, maxBody, false)
	out := map[string]any{"seq": MaxSeq(ctx, r.DB), "cursor": cursor, "n": len(page), "ms": shaped, "has_more": len(rows) > limitN}
	if len(page) > 0 {
		subsByThread := map[int64]int64{}
		srows, _ := db.QueryRows(ctx, r.DB, "SELECT thread, seen FROM subs WHERE agent = ?", r.Me)
		for _, row := range srows {
			subsByThread[db.AsInt64(row, "thread")] = db.AsInt64(row, "seen")
		}
		ids := make([]any, 0, len(page))
		for _, row := range page {
			ids = append(ids, db.AsInt64(row, "id"))
		}
		tagged := map[int64]bool{}
		trows, _ := db.QueryRows(ctx, r.DB, fmt.Sprintf("SELECT mid FROM mentions WHERE agent = ? AND mid IN (%s)", db.Marks(len(ids))), append([]any{r.Me}, ids...)...)
		for _, row := range trows {
			tagged[db.AsInt64(row, "mid")] = true
		}
		for idx, msg := range page {
			var why []string
			if tagged[db.AsInt64(msg, "id")] {
				why = append(why, "at")
			}
			_, inSub := subsByThread[db.AsInt64(msg, "thread")]
			if inSub && db.AsString(msg, "author") != r.Me {
				why = append(why, "su")
			}
			whyStr := strings.Join(why, "+")
			if whyStr == "" {
				whyStr = "new"
			}
			shaped[idx]["why"] = whyStr
		}
		out["next"] = db.AsInt64(page[len(page)-1], "id")
	}
	if !r.Has("threads") || r.Bool("threads") {
		counts := map[int64]int{}
		var order []int64
		for _, row := range page {
			tid := db.AsInt64(row, "thread")
			if _, ok := counts[tid]; !ok {
				order = append(order, tid)
			}
			counts[tid]++
		}
		sort.SliceStable(order, func(i, j int) bool { return counts[order[i]] > counts[order[j]] })
		th := make([]any, 0, len(order))
		for _, tid := range order {
			th = append(th, map[string]any{"i": tid, "un": counts[tid]})
		}
		out["th"] = th
	}
	if !r.Has("advance") || r.Bool("advance") {
		newID := cursor
		for _, row := range page {
			if db.AsInt64(row, "id") > newID {
				newID = db.AsInt64(row, "id")
			}
		}
		if len(page) > 0 {
			if _, err := db.Exec(ctx, r.DB, "UPDATE agents SET cursor = ? WHERE name = ?", newID, r.Me); err != nil {
				return nil, err
			}
			maxByThread := map[int64]int64{}
			var threads []int64
			for _, row := range page {
				tid := db.AsInt64(row, "thread")
				if _, ok := maxByThread[tid]; !ok {
					threads = append(threads, tid)
				}
				if db.AsInt64(row, "id") > maxByThread[tid] {
					maxByThread[tid] = db.AsInt64(row, "id")
				}
			}
			for _, tid := range threads {
				if err := SetThreadSeen(ctx, r.DB, r.Me, tid, maxByThread[tid]); err != nil {
					return nil, err
				}
			}
		}
		out["adv"] = newID
	}
	if r.Bool("subs") {
		su, err := subscriptions(ctx, r.DB, r.Cfg, r.Me, limitN)
		if err != nil {
			return nil, err
		}
		out["su"] = su
	}
	return out, nil
}

func opSeen(ctx context.Context, r *Req) (any, error) {
	out := map[string]any{"ok": 1}
	tid, hasT := r.Int64("t")
	if hasT && tid != 0 {
		thread, _ := db.QueryOne(ctx, r.DB, "SELECT id, last FROM threads WHERE id = ?", tid)
		if thread == nil {
			return nil, apiErr(404, "no_thread", fmt.Sprintf("thread %d does not exist", tid), "")
		}
		read, hasRead := r.Int64("read")
		mark := db.AsInt64(thread, "last")
		if hasRead {
			mark = read
		}
		if err := SetThreadSeen(ctx, r.DB, r.Me, tid, mark); err != nil {
			return nil, err
		}
		cur, _, _ := db.QueryOneValue(ctx, r.DB, "SELECT seen FROM subs WHERE agent = ? AND thread = ?", r.Me, tid)
		out["thread"] = map[string]any{"i": tid, "seen": toI64(cur)}
	}
	if r.Bool("all") {
		if _, err := db.Exec(ctx, r.DB, "UPDATE subs SET seen = (SELECT last FROM threads WHERE threads.id = subs.thread) WHERE agent = ?", r.Me); err != nil {
			return nil, err
		}
		out["all"] = 1
	}
	if r.Has("seq") {
		seq, _ := r.Int64("seq")
		target := seq
		if seq == 0 {
			target = MaxSeq(ctx, r.DB)
		}
		if _, err := db.Exec(ctx, r.DB, "UPDATE agents SET cursor = GREATEST(cursor, ?) WHERE name = ?", target, r.Me); err != nil {
			return nil, err
		}
	}
	cur, _ := agentCursor(ctx, r.DB, r.Me)
	out["cursor"] = cur
	return out, nil
}

func opFeed(ctx context.Context, r *Req) (any, error) {
	limit, _ := r.Int64("limit")
	limitN, err := ClampLimit(r.Cfg, limit, r.Cfg.FeedDefaultLimit, 500)
	if err != nil {
		return nil, err
	}
	since, _ := r.Int64("since")
	if since < 0 {
		since = 0
	}
	ts := db.Now()
	rows, err := db.QueryRows(ctx, r.DB, "SELECT * FROM messages WHERE id > ? ORDER BY id LIMIT ?", since, limitN+1)
	if err != nil {
		return nil, err
	}
	page := rows
	if len(page) > limitN {
		page = page[:limitN]
	}
	maxBody := int(r.IntDefault("max_body"))
	if maxBody == 0 {
		maxBody = 400
	}
	out := map[string]any{"seq": MaxSeq(ctx, r.DB), "ts": ts, "ms": LoadMessages(ctx, r.DB, page, maxBody, r.Long), "has_more": len(rows) > limitN}
	if len(page) > 0 {
		out["next"] = db.AsInt64(page[len(page)-1], "id")
	}
	if !r.Has("on") || r.Bool("on") {
		onRows, _ := db.QueryRows(ctx, r.DB, "SELECT name FROM agents WHERE (seen >= ? OR low = ?) ORDER BY name", ts-float64(r.Cfg.AgentTTL), config.AdminName)
		on := make([]any, 0, len(onRows))
		for _, row := range onRows {
			on = append(on, db.AsString(row, "name"))
		}
		out["on"] = on
	}
	if (!r.Has("men") || r.Bool("men")) && r.Me != "" {
		mrows, _ := db.QueryRows(ctx, r.DB, "SELECT mn.mid FROM mentions mn WHERE mn.agent = ? AND mn.mid > ? ORDER BY mn.mid", r.Me, since)
		men := make([]any, 0, len(mrows))
		for _, row := range mrows {
			men = append(men, db.AsInt64(row, "mid"))
		}
		out["men"] = men
	}
	if (!r.Has("threads") || r.Bool("threads")) && len(page) > 0 {
		set := map[int64]bool{}
		var ids []any
		for _, row := range page {
			tid := db.AsInt64(row, "thread")
			if !set[tid] {
				set[tid] = true
				ids = append(ids, tid)
			}
		}
		trows, _ := db.QueryRows(ctx, r.DB, fmt.Sprintf("SELECT %s FROM threads t WHERE t.id IN (%s) ORDER BY t.active DESC", ThreadCols, db.Marks(len(ids))), ids...)
		th := make([]any, 0, len(trows))
		for _, row := range trows {
			th = append(th, ShapeThread(row, r.Long))
		}
		out["th"] = th
	}
	return out, nil
}
