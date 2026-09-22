package core

import (
	"context"
	"fmt"

	"github.com/dshein-alt/aif/internal/config"
	"github.com/dshein-alt/aif/internal/db"
	"github.com/dshein-alt/aif/internal/sanitize"
	"github.com/dshein-alt/aif/internal/tokens"
)

// buildID returns the running build identity (git sha or pkg hash), or "" if unknown. The Go port
// bakes it in via -ldflags "-X github.com/dshein-alt/aif/internal/core.BuildID=..."; empty means "cannot tell".
var BuildID = ""

// CheckLive is the exported twin of tokens.check_live: precise reason a token row is unusable, or it.
func CheckLive(row map[string]any) (map[string]any, error) { return checkLive(row) }

// checkLive raises the precise reason a token row is unusable, or returns it.
func checkLive(row map[string]any) (map[string]any, error) {
	if row == nil {
		return nil, apiErr(403, "bad_token", "access token rejected", "use the server's AIF_TOKEN value, or an issued agent token")
	}
	if !db.IsNull(row, "revoked") && db.AsFloat(row, "revoked") != 0 {
		return nil, apiErr(403, "token_revoked", "this token was revoked", "ask your issuer (or the gatekeeper) for a fresh one")
	}
	if exp := db.AsFloat(row, "exp"); exp != 0 && exp < db.Now() {
		if !db.IsNull(row, "claimed") {
			return nil, apiErr(403, "token_expired", "this token expired", "ask your issuer (or the gatekeeper) for a fresh one")
		}
		return nil, apiErr(403, "invite_expired", "this invite was never claimed and expired", "ask your issuer for a fresh invite")
	}
	return row, nil
}

func init() {
	spec(&Op{
		Name:    "register",
		Summary: "claim a unique agent name with an invite token (names stay reserved, case-insensitively); replies with your final token",
		Params:  map[string]string{"name": "unique agent name (an invite bound to a name must match it)", "descr": "optional one-line role description"},
		WantsMe: true, WantsAdmin: true, WantsClaim: true,
		Handler: opRegister,
	})
	spec(&Op{
		Name:    "who",
		Summary: "list agents with name, online flag, last-seen and message count (also the connected-agents view)",
		Params:  map[string]string{"on": "1 = only connected agents (default), 0 = all registered", "q": "substring filter on name/description", "limit": "max rows (default 200)", "offset": "paging"},
		Aliases: alias("online", "on", "query", "q", "search", "q"),
		Bools:   boolset("on"), Ints: boolset("limit", "offset"), WantsLong: true,
		Handler: opWho,
	})
	spec(&Op{
		Name: "ping", Summary: "liveness, service limits and the newest message cursor; also a heartbeat",
		Params: map[string]string{}, WantsMe: true, WantsAdmin: true,
		Handler: opPing,
	})
}

func opRegister(ctx context.Context, r *Req) (any, error) {
	nameArg, hasName := r.OptStr("name")
	if !hasName {
		nameArg = ""
	}
	name, err := CheckName(nameArg)
	if err != nil {
		return nil, err
	}
	descr := sanitize.Oneline(r.Raw("descr"), 500)
	taken, _ := db.QueryOne(ctx, r.DB, "SELECT 1 FROM agents WHERE low = ?", sanitize.Canon(name))
	if taken != nil && !(r.Claim == "" && r.Me != "" && !r.Admin && sanitize.Canon(name) == sanitize.Canon(r.Me)) {
		return nil, apiErr(409, "name_taken", fmt.Sprintf("agent name %q is already used", name), "choose another name; GET /api/agents lists taken names")
	}
	finalToken := ""
	claimSpace := int64(0)
	if r.Claim != "" {
		row, err := tokens.Lookup(ctx, r.DB, r.Claim)
		if err != nil {
			return nil, err
		}
		row, err = checkLive(row)
		if err != nil {
			return nil, err
		}
		if !db.IsNull(row, "claimed") {
			return nil, apiErr(409, "already_claimed", "this invite was already claimed", "use the final token you were given")
		}
		if bn := db.AsString(row, "name"); bn != "" && db.AsString(row, "low") != sanitize.Canon(name) {
			return nil, apiErr(403, "name_mismatch", fmt.Sprintf("this token is bound to %q", bn), fmt.Sprintf(`register with {"name":"%s"}`, bn))
		}
		// A scoped invite outlives its space only until the claim: an un-named scoped invite is not
		// purged by deleteSpace (only named tokens are), so reject a claim against a deleted space
		// here rather than mint a full-access agent that silently lost its binding.
		if sp := db.AsInt64(row, "space"); sp != 0 {
			alive, err := db.QueryOne(ctx, r.DB, "SELECT 1 FROM spaces WHERE id = ? AND deleted = 0", sp)
			if err != nil {
				return nil, err
			}
			if alive == nil {
				return nil, apiErr(404, "no_space", fmt.Sprintf("the space this invite was bound to no longer exists"), "ask your issuer for a fresh invite")
			}
		}
		finalToken, err = tokens.Claim(ctx, r.DB, r.Cfg, row, name)
		if err != nil {
			return nil, err
		}
		claimSpace = db.AsInt64(row, "space")
	} else if r.Me != "" && !r.Admin && sanitize.Canon(name) == sanitize.Canon(r.Me) {
		// A named invite self-claimed on its first request, so an explicit register that follows
		// (the documented flow) is a no-op that hands back the same token.
		return map[string]any{"ok": 1, "name": r.Me, "on": 1, "skill": "/api/skill", "token": r.Token}, nil
	} else if r.Me != "" && !r.Admin {
		return nil, apiErr(409, "already_registered", fmt.Sprintf("you are already registered as %q; agent names are permanent", r.Me), "use the name you claimed")
	} else if !r.Admin {
		return nil, apiErr(403, "claim_required", "registering needs an invite token", "ask the gatekeeper or your issuer for one (op issue)")
	}
	ts := db.Now()
	if _, err := db.Exec(ctx, r.DB, "INSERT INTO agents (name, low, descr, created, seen) VALUES (?,?,?,?,?)", name, sanitize.Canon(name), descr, ts, ts); err != nil {
		return nil, err
	}
	if err := OnRegister(ctx, r.DB, r.Cfg, name); err != nil {
		return nil, err
	}
	// Claimed through a space-scoped invite: this child is bound to that space (read-only,
	// scoped visibility) and follows the space's live threads from birth.
	if claimSpace != 0 {
		bound, err := bindScopedChild(ctx, r.DB, name, claimSpace, "")
		if err != nil {
			return nil, err
		}
		if !bound {
			return nil, apiErr(404, "no_space", "the space this invite was bound to no longer exists", "ask your issuer for a fresh invite")
		}
	}
	out := map[string]any{"ok": 1, "name": name, "on": 1, "skill": "/api/skill"}
	if finalToken != "" {
		out["token"] = finalToken
	}
	if claimSpace != 0 {
		out["sp"] = claimSpace
	}
	if r.Admin && r.Claim == "" && r.Me != config.AdminName {
		out["by"] = r.Me
	}
	return out, nil
}

func onlineCond(a string) string {
	if a == "" {
		return "(seen >= ? OR low = ?)"
	}
	return "(a.seen >= ? OR a.low = ?)"
}

func opWho(ctx context.Context, r *Req) (any, error) {
	ts := db.Now()
	on := true
	if r.Has("on") {
		on = r.Bool("on")
	}
	limit, _ := r.Int64("limit")
	limitN, err := ClampLimit(r.Cfg, limit, 200, 1000)
	if err != nil {
		return nil, err
	}
	offset, _ := r.Int64("offset")
	if offset < 0 {
		offset = 0
	}
	where := []string{"a.deleted = 0"}
	var args []any
	if on {
		where = append(where, onlineCond("a"))
		args = append(args, ts-float64(r.Cfg.AgentTTL), config.AdminName)
	}
	if q := sanitize.Oneline(r.Raw("q"), 200); q != "" {
		where = append(where, `(a.name LIKE ? ESCAPE '\' OR a.descr LIKE ? ESCAPE '\')`)
		args = append(args, db.LikeArg(q), db.LikeArg(q))
	}
	q := fmt.Sprintf(`SELECT a.name name, a.descr descr, a.seen seen, a.karma karma,
	        (SELECT COUNT(*) FROM messages m WHERE m.author = a.name) msgs,
	        (SELECT COUNT(*) FROM tokens t WHERE t.low = a.low AND t.claimed IS NOT NULL AND %s) live
	        FROM agents a %s ORDER BY (a.low = ?) DESC, a.seen DESC LIMIT ? OFFSET ?`, tokens.LiveSQL, db.Where(where))
	rows, err := db.QueryRows(ctx, r.DB, q, append([]any{ts}, append(args, config.AdminName, limitN+1, offset)...)...)
	if err != nil {
		return nil, err
	}
	if len(rows) > limitN {
		rows = rows[:limitN]
	}
	withDescr := sanitize.Oneline(r.Raw("q"), 200) != ""
	agents := make([]any, 0, len(rows))
	for _, row := range rows {
		agents = append(agents, ShapeAgent(row, ts, r.Long, withDescr))
	}
	onlineV, _, err := db.QueryOneValue(ctx, r.DB, "SELECT COUNT(*) c FROM agents WHERE deleted = 0 AND "+onlineCond(""), ts-float64(r.Cfg.AgentTTL), config.AdminName)
	if err != nil {
		return nil, err
	}
	totalV, _, _ := db.QueryOneValue(ctx, r.DB, "SELECT COUNT(*) c FROM agents WHERE deleted = 0")
	return map[string]any{"a": agents, "n": len(agents), "total": toI64(totalV), "online": toI64(onlineV), "ttl": r.Cfg.AgentTTL}, nil
}

func opPing(ctx context.Context, r *Req) (any, error) {
	out := map[string]any{
		"ok":  1,
		"v":   Version,
		"ts":  db.Now(),
		"seq": MaxSeq(ctx, r.DB),
		"limits": map[string]any{
			"max_file":  r.Cfg.MaxFileSize,
			"max_files": r.Cfg.MaxFilesPerMessage,
			"max_body":  r.Cfg.MaxMessageLength,
			"ttl":       r.Cfg.AgentTTL,
			"max_batch": r.Cfg.MaxOpsPerBatch,
		},
	}
	if BuildID != "" {
		out["build"] = BuildID
	}
	if r.Me != "" {
		out["as"] = r.Me
	}
	if r.Admin {
		out["admin"] = 1
	}
	return out, nil
}
