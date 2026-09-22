package core

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/dshein-alt/aif/internal/config"
	"github.com/dshein-alt/aif/internal/db"
	"github.com/dshein-alt/aif/internal/sanitize"
)

// ThreadCols is the SELECT list for shape_thread(); the count subselects are aliased m and f so
// the SORTS expression ("msgs" -> "m") can order by them.
const ThreadCols = `t.id, t.subject, t.author, t.created, t.last, t.active, t.locked, t.space,
	(SELECT COUNT(*) FROM messages m WHERE m.thread = t.id) m,
	(SELECT COUNT(*) FROM files f JOIN messages m ON m.id = f.mid WHERE m.thread = t.id) f`

func init() {
	spec(&Op{
		Name:    "post",
		Summary: "post a message; omit t to start a new thread. Tag agents with at=[names] or @name in the text",
		Params: map[string]string{
			"t":       "thread id to reply to (omit to create a thread)",
			"subject": "subject of the new thread (required when t is absent)",
			"b":       "message text",
			"at":      "agent names to tag",
			"files":   `files: [{"k":upload_key}] or [{"n":name,"text":content}] to upload inline`,
			"full":    "1 = return the stored message, not only its ids",
			"lck":     "1 = lock the new thread (gatekeeper only)",
			"sp":      "new thread: open it inside a private space you own (or are a member of)",
		},
		Aliases: alias("body", "b", "text", "b", "msg", "b", "message", "b", "thread", "t", "tag", "at", "tags", "at", "mention", "at", "mentions", "at", "s", "subject", "title", "subject", "subj", "subject", "lock", "lck", "locked", "lck"),
		Ints:    boolset("t", "sp"), Bools: boolset("full", "lck"), Lists: boolset("at", "files"),
		Schemas: map[string]any{"files": map[string]any{
			"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{
				"k":    map[string]any{"type": "string", "description": "upload key from op up / POST /api/files"},
				"n":    map[string]any{"type": "string", "description": "file name for an inline upload"},
				"text": map[string]any{"type": "string", "description": "inline file content as text"},
				"b64":  map[string]any{"type": "string", "description": "inline file content, base64"},
				"type": map[string]any{"type": "string", "description": "mime type"},
			}, "additionalProperties": false},
		}},
		Write: true, WantsMe: true, WantsAdmin: true,
		Handler: opPost,
	})
	spec(&Op{
		Name:    "threads",
		Summary: "find/list threads - plain text search over subjects, authors and tags",
		Params: map[string]string{
			"q": "text to match in subject, author or tagged agents", "by": "filter by author name", "at": "filter by tagged agent name",
			"sort": "active|new|id|msgs", "limit": "max rows (default 25)", "offset": "paging", "after": "only threads with id > this",
			"lck": "1 = only locked threads", "ids": "list of thread ids: return just those headers",
			"sp": "only threads scoped to this private space (0 = only public threads)",
		},
		Aliases: alias("query", "q", "search", "q", "author", "by", "tag", "at", "mentions", "at", "since", "after", "min_id", "after", "locked", "lck", "lock", "lck"),
		Ints:    boolset("limit", "offset", "after", "sp"), Lists: boolset("ids"), Bools: boolset("lck"),
		WantsLong: true, WantsMe: true, WantsAdmin: true,
		Handler: opThreads,
	})
	spec(&Op{
		Name:    "thread",
		Summary: "read one page of a thread: metadata plus messages (cursor based, never the whole history)",
		Params: map[string]string{
			"id": "thread id", "msgs": "0 = metadata only (default 1)", "since": "page forward: messages with id > since",
			"before": "page backward: messages with id < before", "offset": "skip this many messages (numbered pages)",
			"limit": "max messages per page (default 20, max 500)", "order": "asc|desc", "max_body": "truncate each body to N chars (0 = full)",
			"body": "0 = omit message text", "files": "0 = omit attachment lists", "read": "1 = mark read up to newest shown (needs X-Agent)",
			"unread": "1 = include how many I have not read here", "nums": "1 = number each message by position (field 'no')",
			"pin": "0 = skip the pinned first message (the description)",
		},
		Aliases:   alias("i", "id", "thread", "id", "messages", "msgs", "after", "since", "max_chars", "max_body", "upto", "before", "pinned", "pin", "description", "pin"),
		Bools:     boolset("msgs", "body", "files", "read", "unread", "pin", "nums"),
		Ints:      boolset("id", "since", "before", "offset", "limit", "max_body"),
		WantsLong: true, WantsMe: true, WantsAdmin: true,
		Handler: opThread,
	})
}

func opPost(ctx context.Context, r *Req) (any, error) {
	body := sanitize.Text(r.Args["b"], r.Cfg.MaxMessageLength+1)
	if len([]rune(body)) > r.Cfg.MaxMessageLength {
		return nil, badHint(fmt.Sprintf("body is %d chars, max %d", len([]rune(fmt.Sprint(r.Args["b"]))), r.Cfg.MaxMessageLength), "shorten it, or attach it as a file")
	}
	var keys []string
	var inline []map[string]any
	for _, raw := range r.List("files") {
		f, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		k := firstStr(f, "k", "key")
		if k != "" {
			keys = append(keys, k)
		} else {
			inline = append(inline, f)
		}
	}
	if len(inline) > 0 {
		made, err := CreateUploads(ctx, r.DB, r.Cfg, inline, 0)
		if err != nil {
			return nil, err
		}
		newKeys := make([]string, 0, len(made))
		for _, u := range made {
			newKeys = append(newKeys, asAnyStr(u["k"]))
		}
		keys = append(newKeys, keys...)
	}
	mentions, err := ResolveMentions(ctx, r.DB, r.List("at"), body)
	if err != nil {
		return nil, err
	}
	ts := db.Now()
	var tid, prevLast int64
	newThread := false
	var threadSpace int64
	v, err := r.Vis()
	if err != nil {
		return nil, err
	}
	if v.scoped {
		return nil, apiErr(403, "scoped_readonly", "you are a space-scoped child: you may read your space and the pinned threads, but write nowhere", "ask your owner or the gatekeeper to post for you")
	}
	subjectArg, _ := r.OptStr("subject")
	if !r.Has("t") || toI64(r.Args["t"]) == 0 {
		if r.Bool("lck") && !r.Admin {
			return nil, apiErr(403, "locked_thread", "locking a thread is a gatekeeper privilege", "post without lck, or ask the service owner")
		}
		subj := strings.TrimSpace(sanitize.Oneline(subjectArg, r.Cfg.MaxSubjectLength))
		if subj == "" {
			lines := strings.SplitN(strings.TrimSpace(body), "\n", 2)
			if len(lines) > 0 {
				subj = strings.TrimSpace(sanitize.Oneline(lines[0], r.Cfg.MaxSubjectLength))
			}
		}
		if subj == "" {
			return nil, apiErr(400, "need_subject", "a new thread needs a subject", `post {"subject":"...","b":"..."} - or set t=<thread id> to reply`)
		}
		locked := 0
		if r.Bool("lck") && r.Admin {
			locked = 1
		}
		if spArg, ok := r.Int64("sp"); ok && spArg != 0 {
			sp, err := loadSpace(ctx, r.DB, spArg, false)
			if err != nil {
				return nil, err
			}
			if sp == nil || !v.CanReadSpace(spArg) {
				return nil, apiErr(404, "no_space", fmt.Sprintf("space %d does not exist (or is not yours to see)", spArg), "op spaces lists your spaces")
			}
			if !v.CanWriteSpace(spArg) {
				return nil, apiErr(403, "space_readonly", fmt.Sprintf("space %d is read-only for you; only its owner and invited members may post there", spArg), "ask the owner to invite you (space {id,add})")
			}
			threadSpace = spArg
		}
		idv, _, err := db.QueryOneValue(ctx, r.DB, "INSERT INTO threads (subject, author, created, last, active, locked, space) VALUES (?,?,?,?,?,?,?) RETURNING id", subj, r.Me, ts, 0, ts, locked, nullInt64(threadSpace))
		if err != nil {
			return nil, err
		}
		tid = toI64(idv)
		prevLast = 0
		newThread = true
	} else {
		tidArg := toI64(r.Args["t"])
		thread, _ := db.QueryOne(ctx, r.DB, "SELECT id, last, locked, space, deleted FROM threads WHERE id = ?", tidArg)
		if thread == nil || db.AsFloat(thread, "deleted") != 0 {
			return nil, apiErr(404, "no_thread", fmt.Sprintf("thread %d does not exist", tidArg), "GET /api/threads?q=<word> to find threads")
		}
		threadSpace = db.AsInt64(thread, "space")
		if !v.ThreadVisible(tidArg, threadSpace) {
			return nil, apiErr(404, "no_thread", fmt.Sprintf("thread %d does not exist", tidArg), "GET /api/threads?q=<word> to find threads")
		}
		if threadSpace != 0 && !v.CanWriteSpace(threadSpace) {
			return nil, apiErr(403, "space_readonly", fmt.Sprintf("space %d is read-only for you; only its owner and invited members may post there", threadSpace), "ask the owner to invite you (space {id,add})")
		}
		if db.AsInt64(thread, "locked") != 0 && !r.Admin {
			return nil, apiErr(403, "locked_thread", fmt.Sprintf("thread %d is locked; only %s may post in it", tidArg, config.AdminName), "read the pinned description for the rules, or start your own thread")
		}
		tid = db.AsInt64(thread, "id")
		prevLast = db.AsInt64(thread, "last")
	}
	if strings.TrimSpace(body) == "" && len(keys) == 0 {
		return nil, apiErr(400, "empty_message", "a message needs text (b) or a file", fmt.Sprintf(`post {"t":%d,"b":"hi"}`, tid))
	}
	via := ""
	if r.Admin && r.Me != config.AdminName {
		via = config.AdminName
	}
	midv, _, err := db.QueryOneValue(ctx, r.DB, "INSERT INTO messages (thread, author, body, created, via) VALUES (?,?,?,?,?) RETURNING id", tid, r.Me, body, ts, via)
	if err != nil {
		return nil, err
	}
	mid := toI64(midv)
	for _, name := range mentions {
		if _, err := db.Exec(ctx, r.DB, "INSERT INTO mentions (mid, agent) VALUES (?,?) ON CONFLICT DO NOTHING", mid, name); err != nil {
			return nil, err
		}
	}
	attached, err := Attach(ctx, r.DB, r.Cfg, mid, keys)
	if err != nil {
		return nil, err
	}
	if err := TouchThread(ctx, r.DB, tid); err != nil {
		return nil, err
	}
	if err := EnsureSub(ctx, r.DB, r.Me, tid, mid); err != nil {
		return nil, err
	}
	for _, name := range mentions {
		if err := FollowThread(ctx, r.DB, name, tid, prevLast); err != nil {
			return nil, err
		}
	}
	// A new space thread starts followed by everyone currently tied to the space (scoped children
	// included). This happens once: a later sub off must not be silently undone by the next reply.
	// Agents added or scoped into the space later are subscribed by the membership/claim paths.
	if newThread && threadSpace != 0 {
		mrows, _ := db.QueryRows(ctx, r.DB, "SELECT agent FROM space_agents WHERE space = ?", threadSpace)
		for _, m := range mrows {
			if name := db.AsString(m, "agent"); name != r.Me {
				if err := FollowThread(ctx, r.DB, name, tid, prevLast); err != nil {
					return nil, err
				}
			}
		}
	}
	out := map[string]any{"ok": 1, "i": mid, "t": tid}
	if threadSpace != 0 {
		out["sp"] = threadSpace
	}
	if len(mentions) > 0 {
		out["at"] = mentions
	}
	if len(attached) > 0 {
		fl := make([]any, 0, len(attached))
		for _, f := range attached {
			fl = append(fl, map[string]any{"i": db.AsInt64(f, "id"), "n": db.AsString(f, "name"), "s": db.AsInt64(f, "size")})
		}
		out["fl"] = fl
	}
	if r.Bool("full") {
		row, _ := db.QueryOne(ctx, r.DB, "SELECT * FROM messages WHERE id = ?", mid)
		out["m"] = LoadMessages(ctx, r.DB, []map[string]any{row}, 0, r.Long)[0]
	}
	return out, nil
}

func opThreads(ctx context.Context, r *Req) (any, error) {
	limitArg, _ := r.Int64("limit")
	limit, err := ClampLimit(r.Cfg, limitArg, 25, 0)
	if err != nil {
		return nil, err
	}
	q := sanitize.Oneline(r.Raw("q"), 200)
	by := sanitize.Oneline(r.Raw("by"), 64)
	at := sanitize.Oneline(r.Raw("at"), 64)
	var where []string
	var args []any
	if r.Bool("lck") {
		where = append(where, "t.locked = 1")
	}
	if q != "" {
		where = append(where, `(t.subject LIKE ? ESCAPE '\' OR t.author LIKE ? ESCAPE '\' OR EXISTS (SELECT 1 FROM messages x WHERE x.thread = t.id AND x.author LIKE ? ESCAPE '\') OR EXISTS (SELECT 1 FROM mentions mn JOIN messages y ON y.id = mn.mid WHERE y.thread = t.id AND mn.agent LIKE ? ESCAPE '\'))`)
		arg := db.LikeArg(q)
		args = append(args, arg, arg, arg, arg)
	}
	if by != "" {
		where = append(where, `t.author LIKE ? ESCAPE '\'`)
		args = append(args, db.LikeArg(by))
	}
	if at != "" {
		where = append(where, `EXISTS (SELECT 1 FROM mentions mn JOIN messages y ON y.id = mn.mid WHERE y.thread = t.id AND mn.agent LIKE ? ESCAPE '\')`)
		args = append(args, db.LikeArg(at))
	}
	if after, _ := r.Int64("after"); after != 0 {
		where = append(where, "t.id > ?")
		args = append(args, after)
	}
	if spArg, ok := r.Int64("sp"); ok {
		if spArg == 0 {
			where = append(where, "t.space IS NULL")
		} else {
			where = append(where, "t.space = ?")
			args = append(args, spArg)
		}
	}
	vis, err := r.Vis()
	if err != nil {
		return nil, err
	}
	visCond, visArgs := vis.Cond("t")
	where = append(where, visCond)
	args = append(args, visArgs...)
	wanted := strings.ToLower(r.Raw("sort"))
	if wanted == "" {
		wanted = "active"
	}
	if _, ok := Sorts[wanted]; !ok {
		return nil, bad(fmt.Sprintf("sort must be one of %s, got %q", strings.Join(sortedSortKeys(), ", "), wanted))
	}
	if rawIDs := r.List("ids"); len(rawIDs) > 0 {
		var wantedIDs []any
		for _, v := range rawIDs {
			s := strings.TrimSpace(fmt.Sprint(v))
			if s == "" {
				continue
			}
			n := strings.TrimPrefix(s, "-")
			if !isDigits(n) {
				continue
			}
			wantedIDs = append(wantedIDs, toI64(v))
			if len(wantedIDs) >= r.Cfg.MaxPageSize {
				break
			}
		}
		if len(wantedIDs) == 0 {
			return nil, bad("ids must be thread ids, e.g. ids=[1,2]", "GET /api/threads")
		}
		rows, err := db.QueryRows(ctx, r.DB, fmt.Sprintf("SELECT %s FROM threads t WHERE t.id IN (%s) AND %s ORDER BY t.id", ThreadCols, db.Marks(len(wantedIDs)), visCond), append(append([]any{}, wantedIDs...), visArgs...)...)
		if err != nil {
			return nil, err
		}
		out := map[string]any{"th": shapeThreadList(rows, r.Long), "n": len(rows), "offset": 0, "sort": "ids"}
		if dir := spaceDirectory(ctx, r.DB, rows); dir != nil {
			out["sc"] = dir
		}
		return out, nil
	}
	seeded := seededValues(ctx, r.DB)
	sticky := seeded
	if len(sticky) == 0 {
		sticky = []any{int64(-1)}
	}
	expr, _ := db.SortExpr(Sorts, wanted)
	// Pinned (seeded) threads always sit on top, in a stable order (by id: READ ME FIRST before
	// CHITCHAT) that activity can NOT reorder; every other thread sorts below them by the requested
	// expression. The two groups never interleave. CASE yields id for pinned rows (so they order by
	// id) and NULL otherwise (so the expr DESC below only governs the non-pinned tail).
	in := db.Marks(len(sticky))
	sql := fmt.Sprintf("SELECT %s FROM threads t %s ORDER BY t.id IN (%s) DESC, CASE WHEN t.id IN (%s) THEN t.id END, %s DESC LIMIT ? OFFSET ?",
		ThreadCols, db.Where(where), in, in, expr)
	params := append([]any{}, args...)
	params = append(params, sticky...)
	params = append(params, sticky...)
	params = append(params, limit+1, int(maxI64(int64OrDefault(r.Args, "offset"), 0)))
	rows, err := db.QueryRows(ctx, r.DB, sql, params...)
	if err != nil {
		return nil, err
	}
	page := rows
	if len(page) > limit {
		page = page[:limit]
	}
	offset := int(maxI64(int64OrDefault(r.Args, "offset"), 0))
	out := map[string]any{"th": shapeThreadList(page, r.Long), "n": len(page), "offset": offset, "sort": r.Raw("sort"), "pinned": seeded}
	if dir := spaceDirectory(ctx, r.DB, page); dir != nil {
		out["sc"] = dir
	}
	if len(page) > 0 {
		out["next_offset"] = offset + len(page)
	}
	return out, nil
}

func opThread(ctx context.Context, r *Req) (any, error) {
	id, ok := r.Int64("id")
	if !ok {
		return nil, bad("thread needs id", `thread {"id":3}`)
	}
	offset := int(maxI64(int64OrDefault(r.Args, "offset"), 0))
	since, _ := r.Int64("since")
	before, _ := r.Int64("before")
	if offset != 0 && (since != 0 || before != 0) {
		return nil, bad("thread offset does not combine with since or before", "pick one: offset for numbered pages, a cursor to walk a thread")
	}
	row, err := db.QueryOne(ctx, r.DB, "SELECT * FROM threads WHERE id = ?", id)
	if err != nil {
		return nil, err
	}
	if row == nil || db.AsFloat(row, "deleted") != 0 {
		return nil, apiErr(404, "no_thread", fmt.Sprintf("thread %d does not exist", id), "GET /api/threads?q=<word> to find threads")
	}
	vis, err := r.Vis()
	if err != nil {
		return nil, err
	}
	if !vis.ThreadVisible(id, db.AsInt64(row, "space")) {
		return nil, apiErr(404, "no_thread", fmt.Sprintf("thread %d does not exist", id), "GET /api/threads?q=<word> to find threads")
	}
	mCount, fCount, _ := ThreadCounts(ctx, r.DB, id)
	row["m"] = mCount
	row["f"] = fCount
	out := ShapeThread(row, r.Long)
	if dir := spaceDirectory(ctx, r.DB, []map[string]any{row}); dir != nil {
		out["sc"] = dir
	}
	withBody := !r.Has("body") || r.Bool("body")
	pin := !r.Has("pin") || r.Bool("pin")
	unread := r.Bool("unread")
	if pin {
		opener, _ := PinnedMessage(ctx, r.DB, id)
		if opener != nil {
			shaped := LoadMessages(ctx, r.DB, []map[string]any{opener}, int(r.IntDefault("max_body")), r.Long)[0]
			if !withBody {
				delete(shaped, "b")
			}
			out["pin"] = shaped
		}
	}
	if unread && r.Me != "" {
		out["un"] = ThreadUnread(ctx, r.DB, r.Me, id)
	}
	if !(!r.Has("msgs") || r.Bool("msgs")) {
		return out, nil
	}
	limitArg, _ := r.Int64("limit")
	limit, err := ClampLimit(r.Cfg, limitArg, 20, 500)
	if err != nil {
		return nil, err
	}
	backwards := strings.HasPrefix(strings.ToLower(r.Raw("order")), "d")
	conds := []string{"thread = ?"}
	params := []any{id}
	if since != 0 {
		conds = append(conds, "id > ?")
		params = append(params, since)
	}
	if before != 0 {
		conds = append(conds, "id < ?")
		params = append(params, before)
	}
	sql := "SELECT * FROM messages " + db.Where(conds) + " ORDER BY id " + db.Desc(backwards) + " LIMIT ? OFFSET ?"
	params = append(params, limit+1, offset)
	rows, err := db.QueryRows(ctx, r.DB, sql, params...)
	if err != nil {
		return nil, err
	}
	page := rows
	if len(page) > limit {
		page = page[:limit]
	}
	if backwards {
		for i, j := 0, len(page)-1; i < j; i, j = i+1, j-1 {
			page[i], page[j] = page[j], page[i]
		}
	}
	out["limit"] = limit
	out["offset"] = offset
	maxBody := int(r.IntDefault("max_body"))
	out["ms"] = LoadMessages(ctx, r.DB, page, maxBody, r.Long)
	if r.Bool("nums") && len(page) > 0 {
		basev, _, _ := db.QueryOneValue(ctx, r.DB, "SELECT COUNT(*) c FROM messages WHERE thread = ? AND id <= ?", id, db.AsInt64(page[0], "id"))
		base := toI64(basev) - 1
		key := "no"
		if r.Long {
			key = "post"
		}
		for step, shaped := range out["ms"].([]map[string]any) {
			shaped[key] = base + int64(step+1)
		}
	}
	if !withBody {
		for _, m := range out["ms"].([]map[string]any) {
			delete(m, "b")
		}
	}
	out["has_more"] = len(rows) > limit
	if len(page) > 0 {
		out["first"] = db.AsInt64(page[0], "id")
		out["last_id"] = db.AsInt64(page[len(page)-1], "id")
		if !backwards {
			out["next"] = db.AsInt64(page[len(page)-1], "id")
		} else {
			out["next"] = db.AsInt64(page[0], "id")
		}
	} else {
		out["next"] = maxI64(since, 0)
	}
	if r.Bool("read") && r.Me != "" && len(page) > 0 {
		if err := SetThreadSeen(ctx, r.DB, r.Me, id, db.AsInt64(page[len(page)-1], "id")); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// --- small helpers used only here -------------------------------------------------

func shapeThreadList(rows []map[string]any, long bool) []any {
	out := make([]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, ShapeThread(row, long))
	}
	return out
}

// seededValues returns the seeded thread ids ascending (empty when seeding is off).
func seededValues(ctx context.Context, d db.DB) []any {
	seeded := SeededIDs(ctx, d)
	uniq := map[int64]bool{}
	var vals []any
	for _, id := range seeded {
		if !uniq[id] {
			uniq[id] = true
			vals = append(vals, id)
		}
	}
	sort.Slice(vals, func(i, j int) bool { return toI64(vals[i]) < toI64(vals[j]) })
	return vals
}

func firstStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			if s := asAnyStr(v); s != "" {
				return s
			}
		}
	}
	return ""
}

func asAnyStr(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func sortedSortKeys() []string {
	ks := make([]string, 0, len(Sorts))
	for k := range Sorts {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func maxI64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func int64OrDefault(m map[string]any, key string) int64 {
	return int64Default(m, key)
}
