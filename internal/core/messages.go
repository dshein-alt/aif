package core

import (
	"context"
	"fmt"
	"strings"

	"aif/internal/db"
	"aif/internal/sanitize"
)

func init() {
	spec(&Op{
		Name: "get",
		Summary: "read one message by id",
		Params:  map[string]string{"id": "message id", "max_body": "truncate text to N chars"},
		Aliases: alias("i", "id", "message", "id"),
		Ints:    boolset("id", "max_body"),
		WantsLong: true,
		Handler:   opGet,
	})
	spec(&Op{
		Name: "rm",
		Summary: "delete own message or thread, or remove attachments from own message by file name",
		Params:  map[string]string{"what": "message|thread|file (default message)", "id": "message id for message/file, thread id for thread", "name": "attachment name for what=file; '*' removes all"},
		Aliases: alias("kind", "what", "message", "id", "thread", "id", "file", "name", "filename", "name"),
		Ints:    boolset("id"),
		Write:   true, WantsMe: true, WantsAdmin: true,
		Handler: opRm,
	})
	spec(&Op{
		Name: "search",
		Summary: "text search over thread subjects and agent names in one call",
		Params:  map[string]string{"q": "text to look for", "limit": "max rows per section"},
		Aliases: alias("query", "q"),
		Ints:    boolset("limit"),
		Handler: opSearch,
	})
}

func opGet(ctx context.Context, r *Req) (any, error) {
	id, ok := r.Int64("id")
	if !ok {
		return nil, bad("get needs id", `get {"id":42}`)
	}
	row, err := db.QueryOne(ctx, r.DB, "SELECT * FROM messages WHERE id = ?", id)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, apiErr(404, "no_message", fmt.Sprintf("message %d does not exist", id), "GET /api/threads/{id}?msgs=1 to browse")
	}
	return LoadMessages(ctx, r.DB, []map[string]any{row}, int(r.IntDefault("max_body")), r.Long)[0], nil
}

func opRm(ctx context.Context, r *Req) (any, error) {
	what := strings.ToLower(strings.TrimSuffix(strings.ToLower(sanitize.Oneline(r.Raw("what"), 20)), "s"))
	if what == "" {
		what = "message"
	}
	switch what {
	case "msg", "messages":
		what = "message"
	case "threads":
		what = "thread"
	case "files":
		what = "file"
	}
	id, ok := r.Int64("id")
	if !ok {
		return nil, bad("rm needs id", `rm {"what":"file","id":42,"name":"report.txt"}`)
	}
	switch what {
	case "thread":
		row, err := db.QueryOne(ctx, r.DB, "SELECT * FROM threads WHERE id = ?", id)
		if err != nil {
			return nil, err
		}
		if row == nil {
			return nil, apiErr(404, "no_thread", fmt.Sprintf("thread %d does not exist", id), "")
		}
		if db.AsString(row, "author") != r.Me && !r.Admin {
			return nil, apiErr(403, "not_yours", fmt.Sprintf("thread %d was created by %q; only its author may delete it", id, db.AsString(row, "author")), "")
		}
		blobs := []string{}
		brows, _ := db.QueryRows(ctx, r.DB, "SELECT f.key FROM files f JOIN messages m ON m.id = f.mid WHERE m.thread = ?", id)
		for _, b := range brows {
			blobs = append(blobs, db.AsString(b, "key"))
		}
		if _, err := db.Exec(ctx, r.DB, "DELETE FROM threads WHERE id = ?", id); err != nil {
			return nil, err
		}
		return map[string]any{"ok": 1, "gone": fmt.Sprintf("thread:%d", id), "files": PurgeBlobs(r.Cfg, blobs)}, nil
	}
	msg, err := db.QueryOne(ctx, r.DB, "SELECT * FROM messages WHERE id = ?", id)
	if err != nil {
		return nil, err
	}
	if msg == nil {
		return nil, apiErr(404, "no_message", fmt.Sprintf("message %d does not exist", id), "")
	}
	if db.AsString(msg, "author") != r.Me && !r.Admin {
		return nil, apiErr(403, "not_yours", fmt.Sprintf("message %d was written by %q; only its author may delete it or its files", id, db.AsString(msg, "author")), "")
	}
	switch what {
	case "message":
		blobs := []string{}
		brows, _ := db.QueryRows(ctx, r.DB, "SELECT key FROM files WHERE mid = ?", id)
		for _, b := range brows {
			blobs = append(blobs, db.AsString(b, "key"))
		}
		if _, err := db.Exec(ctx, r.DB, "DELETE FROM messages WHERE id = ?", id); err != nil {
			return nil, err
		}
		if err := TouchThread(ctx, r.DB, db.AsInt64(msg, "thread")); err != nil {
			return nil, err
		}
		return map[string]any{"ok": 1, "gone": fmt.Sprintf("message:%d", id), "files": PurgeBlobs(r.Cfg, blobs)}, nil
	case "file":
		name := r.Raw("name")
		if name == "" {
			return nil, bad("rm what=file needs a file name", `rm {"what":"file","id":42,"name":"report.txt"}`)
		}
		rows, err := db.QueryRows(ctx, r.DB, "SELECT * FROM files WHERE mid = ? AND (name = ? OR ? = '*')", id, name, name)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			return nil, apiErr(404, "no_file", fmt.Sprintf("message %d has no attachment named %q", id, name), fmt.Sprintf("GET /api/messages/%d lists its files", id))
		}
		ids := make([]any, 0, len(rows))
		names := make([]any, 0, len(rows))
		blobs := make([]string, 0, len(rows))
		for _, fr := range rows {
			ids = append(ids, db.AsInt64(fr, "id"))
			names = append(names, db.AsString(fr, "name"))
			blobs = append(blobs, db.AsString(fr, "key"))
		}
		if _, err := db.Exec(ctx, r.DB, fmt.Sprintf("DELETE FROM files WHERE id IN (%s)", db.Marks(len(ids))), ids...); err != nil {
			return nil, err
		}
		return map[string]any{"ok": 1, "gone": names, "count": PurgeBlobs(r.Cfg, blobs)}, nil
	}
	return nil, bad(fmt.Sprintf("rm: unknown what %q; use message|thread|file", what), "")
}

func opSearch(ctx context.Context, r *Req) (any, error) {
	q := sanitize.Oneline(r.Raw("q"), 200)
	if q == "" {
		return nil, bad("search needs q", `search {"q":"budget"}`)
	}
	limitArg, _ := r.Int64("limit")
	limitN, err := ClampLimit(r.Cfg, limitArg, 25, 0)
	if err != nil {
		return nil, err
	}
	threadsRes, err := opThreads(ctx, &Req{Ctx: ctx, DB: r.DB, Cfg: r.Cfg, Args: map[string]any{"q": q, "limit": int64(limitN)}})
	if err != nil {
		return nil, err
	}
	whoRes, err := opWho(ctx, &Req{Ctx: ctx, DB: r.DB, Cfg: r.Cfg, Args: map[string]any{"on": false, "q": q, "limit": int64(limitN)}})
	if err != nil {
		return nil, err
	}
	tm := threadsRes.(map[string]any)
	wm := whoRes.(map[string]any)
	return map[string]any{"q": q, "th": tm["th"], "a": wm["a"], "n": tm["n"].(int) + wm["n"].(int)}, nil
}
