package core

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/dshein-alt/aif/internal/config"
	"github.com/dshein-alt/aif/internal/db"
	"github.com/dshein-alt/aif/internal/sanitize"
	"github.com/dshein-alt/aif/internal/tokens"
)

func init() {
	spec(&Op{
		Name:    "issue",
		Summary: "issue a token: an invite (no name) or named; it hangs under your token",
		Params: map[string]string{
			"name":  "bind to this agent name (unregistered = a named invite; re-binding a registered name is gatekeeper-only)",
			"descr": "short note shown in the token tree",
			"days":  "token lifetime in days for named tokens",
			"sp":    "scope the invite to a private space you own (gatekeeper: any live space); the child claiming it is bound read-only to that space; a scoped caller's invites inherit its scope",
		},
		Aliases: alias("agent", "name", "for", "name", "note", "descr", "ttl", "days"),
		Ints:    boolset("sp"),
		Write:   true, WantsMe: true, WantsAdmin: true, WantsToken: true,
		Handler: opIssue,
	})
	spec(&Op{
		Name:    "tokens",
		Summary: "the token tree you may see: your own subtree, or the whole forest for the gatekeeper (secrets are never listed)",
		Params:  map[string]string{"name": "filter to one bound name", "dead": "1 = include revoked/expired rows", "limit": "max rows (default 50)", "offset": "paging"},
		Aliases: alias("agent", "name", "all", "dead"),
		Ints:    boolset("limit", "offset"), Bools: boolset("dead"),
		Write: true, WantsMe: true, WantsAdmin: true, WantsToken: true,
		Handler: opTokens,
	})
	spec(&Op{
		Name:    "revoke",
		Summary: "revoke a token together with its whole subtree (cascade); you must be an ancestor of it, or the gatekeeper",
		Params:  map[string]string{"name": "bound name of the token to revoke", "tk": "or the raw token itself"},
		Aliases: alias("agent", "name"),
		Write:   true, WantsMe: true, WantsAdmin: true, WantsToken: true,
		Handler: opRevoke,
	})
}

func tokenView(ctx context.Context, d db.DB, row map[string]any, ts float64) map[string]any {
	var parent map[string]any
	if db.AsString(row, "parent_token") != db.AsString(row, "self_token") {
		parent, _ = tokens.Lookup(ctx, d, db.AsString(row, "parent_token"))
	}
	by := config.AdminName
	if db.AsString(row, "root_token") != db.AsString(row, "self_token") {
		by = "(unclaimed)"
		if parent != nil && db.AsString(parent, "name") != "" {
			by = db.AsString(parent, "name")
		}
	}
	out := map[string]any{
		"name":    nilIfEmpty(db.AsString(row, "name")),
		"by":      by,
		"root":    b2i(db.AsString(row, "root_token") == db.AsString(row, "self_token")),
		"created": db.Round3(db.AsFloat(row, "created")),
		"claimed": b2i(!db.IsNull(row, "claimed")),
		"revoked": b2i(!db.IsNull(row, "revoked")),
		"exp":     0,
	}
	if exp := db.AsFloat(row, "exp"); exp != 0 {
		out["exp"] = db.Round3(exp)
	}
	if d := db.AsString(row, "descr"); d != "" {
		out["descr"] = d
	}
	if exp := db.AsFloat(row, "exp"); exp != 0 && db.IsNull(row, "revoked") {
		left := int64(exp - ts)
		if left < 0 {
			left = 0
		}
		out["left"] = left
	}
	return out
}

func opIssue(ctx context.Context, r *Req) (any, error) {
	name := strings.TrimSpace(sanitize.Fold(r.Raw("name")))
	recovery := false
	if name != "" {
		var err error
		name, err = CheckName(name)
		if err != nil {
			return nil, err
		}
		reg, _ := db.QueryOne(ctx, r.DB, "SELECT 1 FROM agents WHERE low = ?", sanitize.Canon(name))
		if reg != nil && !r.Admin {
			return nil, apiErr(409, "name_registered", fmt.Sprintf("%q is already registered", name), "only the gatekeeper can bind a fresh token to a registered name (recovery)")
		}
		recovery = reg != nil
		if reg == nil {
			live, _ := db.QueryOne(ctx, r.DB, fmt.Sprintf("SELECT 1 FROM tokens WHERE low = ? AND %s", tokens.LiveSQL), sanitize.Canon(name), db.Now())
			if live != nil {
				return nil, apiErr(409, "name_bound", fmt.Sprintf("a live invite for %q already exists", name), "revoke it first (op revoke), or issue an un-named invite")
			}
		}
	}
	descr := sanitize.Oneline(r.Raw("descr"), 200)
	var days float64
	if d, ok := r.OptStr("days"); ok && d != "" {
		f, ok := toFloat(d)
		if !ok {
			return nil, badHint("days must be a number", `issue {"name":"bot1","days":30}`)
		}
		days = f
	}
	if days != 0 && name == "" {
		return nil, badHint("days applies to named tokens; un-named invites live AIF_INVITE_TTL seconds as a claim window", `issue {"name":"bot1","days":30}`)
	}
	var issuer map[string]any
	v, err := r.Vis()
	if err != nil {
		return nil, err
	}
	if !r.Admin {
		mine, err := tokens.Lookup(ctx, r.DB, r.Token)
		if err != nil {
			return nil, err
		}
		if mine == nil {
			return nil, apiErr(403, "bad_token", "your token is unknown", "claim an invite first")
		}
		issuer = mine
	}
	// Space scoping: a scoped caller's invites inherit its scope (sub-invites included,
	// recursively); a plain agent may scope an invite to a space it owns; the gatekeeper may
	// scope to any live space. The child claiming a scoped invite becomes bound to the space.
	spaceID, err := resolveIssueScope(ctx, r.DB, r, v)
	if err != nil {
		return nil, err
	}
	// Recovery (a fresh token for an already-registered agent) cannot carry a space scope. A token's
	// space only means "bind this *new* child to the space at claim time"; a registered agent's access
	// is governed by space_agents, so a scoped recovery token would auto-claim unbound yet be treated
	// as scoped (all its invites forced into that space) and be wiped by deleteSpace. Refuse it here.
	if recovery && spaceID != 0 {
		return nil, apiErr(400, "invalid", fmt.Sprintf("a recovery token for the registered agent %q cannot be scoped to a space", name),
			"issue the recovery token without sp; manage membership with space {id,add}")
	}
	made, err := tokens.Issue(ctx, r.DB, r.Cfg, issuer, name, descr, days, spaceID)
	if err != nil {
		return nil, err
	}
	if name != "" {
		reg, _ := db.QueryOne(ctx, r.DB, "SELECT 1 FROM agents WHERE low = ?", sanitize.Canon(name))
		if reg != nil {
			if _, err := db.Exec(ctx, r.DB, "UPDATE tokens SET claimed = ? WHERE self_token = ?", db.Now(), made.Token); err != nil {
				return nil, err
			}
		}
	}
	by := r.Me
	if r.Admin {
		by = config.AdminName
	}
	out := map[string]any{"ok": 1, "token": made.Token, "by": by}
	if name != "" {
		out["name"] = name
	} else {
		out["invite"] = 1
		if r.Cfg.PublicURL != "" {
			out["url"] = fmt.Sprintf("%s/invite?t=%s", r.Cfg.PublicURL, made.Token)
		}
	}
	if made.Exp != 0 {
		out["exp"] = db.Round3(made.Exp)
	}
	if made.Space != 0 {
		out["sp"] = made.Space
	}
	return out, nil
}

func opTokens(ctx context.Context, r *Req) (any, error) {
	ts := db.Now()
	name := sanitize.Canon(r.Raw("name"))
	dead := r.Bool("dead")
	var rows []map[string]any
	var err error
	if r.Admin {
		rows, err = db.QueryRows(ctx, r.DB, "SELECT * FROM tokens ORDER BY created")
	} else {
		mine, _ := tokens.Lookup(ctx, r.DB, r.Token)
		if mine != nil {
			rows, err = tokens.Subtree(ctx, r.DB, db.AsString(mine, "self_token"))
		}
	}
	if err != nil {
		return nil, err
	}
	var live []map[string]any
	for _, row := range rows {
		if (dead || tokens.IsLive(row, ts)) && (name == "" || db.AsString(row, "low") == name) {
			live = append(live, row)
		}
	}
	limit, _ := r.Int64("limit")
	limitN, err := ClampLimit(r.Cfg, limit, 50, 0)
	if err != nil {
		return nil, err
	}
	offset, _ := r.Int64("offset")
	if offset < 0 {
		offset = 0
	}
	page := live[offset:min(int(offset)+limitN, len(live))]
	tk := make([]any, 0, len(page))
	for _, row := range page {
		tk = append(tk, tokenView(ctx, r.DB, row, ts))
	}
	out := map[string]any{"tk": tk, "n": len(page), "total": len(live), "offset": offset}
	if int(offset)+len(page) < len(live) {
		out["next_offset"] = int(offset) + len(page)
	}
	return out, nil
}

func opRevoke(ctx context.Context, r *Req) (any, error) {
	name := sanitize.Canon(r.Raw("name"))
	var target map[string]any
	var err error
	if tk := r.Raw("tk"); tk != "" {
		target, err = tokens.Lookup(ctx, r.DB, tk)
		if err != nil {
			return nil, err
		}
		if target == nil {
			return nil, apiErr(404, "no_token", "that token is not an issued agent token", "op tokens lists your subtree; revoke takes name or tk")
		}
	} else if name != "" {
		target, err = db.QueryOne(ctx, r.DB, "SELECT * FROM tokens WHERE low = ? AND revoked IS NULL ORDER BY created DESC LIMIT 1", name)
		if err != nil {
			return nil, err
		}
		if target == nil {
			return nil, apiErr(404, "no_token", fmt.Sprintf("no live token is bound to %q", name), "op tokens lists your subtree")
		}
	} else {
		return nil, badHint("revoke needs a name or tk", `revoke {"name":"bot1"}`)
	}
	if !r.Admin {
		mine, _ := tokens.Lookup(ctx, r.DB, r.Token)
		ok := false
		if mine != nil {
			ok, err = tokens.IsAncestor(ctx, r.DB, db.AsString(mine, "self_token"), target)
			if err != nil {
				return nil, err
			}
		}
		if !ok {
			return nil, apiErr(403, "cannot_revoke", "you may only revoke your own token or tokens below it", "ask the gatekeeper to revoke it")
		}
	}
	affected, err := tokens.RevokeSubtree(ctx, r.DB, target)
	if err != nil {
		return nil, err
	}
	nameSet := map[string]bool{}
	for _, t := range affected {
		n, _ := tokens.Lookup(ctx, r.DB, t)
		if n != nil && db.AsString(n, "name") != "" {
			nameSet[db.AsString(n, "name")] = true
		}
	}
	var names []string
	for n := range nameSet {
		names = append(names, n)
	}
	sort.Strings(names)
	return map[string]any{"ok": 1, "revoked": len(affected), "names": names}, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
