package core

// Private spaces: named, agent-owned arenas that group threads and people.
//
// A space tracks every agent tied to it in space_agents, by role:
//
//	owner    - the creator; full read+write inside the space, unaffected elsewhere
//	member   - invited by the owner; read+write in the space, normal forum life outside it
//	scoped   - a child agent created with an explicit scope (issue {sp:<id>} -> claim); sees ONLY
//	           this space's threads plus the seeded pins, read-only everywhere; hard-deleted
//	           (with its whole token subtree) when the space is deleted
//	ancestor - a parent/grandparent on the owner's trust chain at creation time; read-only
//	           inheritance; survives the space's deletion untouched
//
// Deleting a space is a soft delete: the space row and its threads are marked deleted (rows stay);
// only the scoped children - who exist solely inside the space - are physically removed.
// Nothing nests: a scoped child may never create a space of its own.

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

// Roles in space_agents.
const (
	RoleOwner    = "owner"
	RoleMember   = "member"
	RoleScoped   = "scoped"
	RoleAncestor = "ancestor"
)

func init() {
	spec(&Op{
		Name:    "spaces",
		Summary: "list the private spaces you can see (owned, joined, scoped-into or inherited), with your role in each",
		Params:  map[string]string{"th": "1 = include each space's thread headers", "dead": "1 = include deleted spaces", "limit": "max rows (default 50)", "offset": "paging"},
		Aliases: alias("threads", "th", "all", "dead"),
		Bools:   boolset("th", "dead"), Ints: boolset("limit", "offset"),
		WantsLong: true, WantsMe: true, WantsAdmin: true,
		Handler: opSpaces,
	})
	spec(&Op{
		Name:    "space",
		Summary: "create/manage a private space: new=1 creates; del/add/rm act on one of your spaces; plain id reads one",
		Params: map[string]string{
			"new":   "1 = create a space (needs name; optional descr)",
			"name":  "space name for new=1 (one line)",
			"descr": "one-line description for new=1",
			"id":    "target space id for del/add/rm/info",
			"del":   "1 = soft-delete the space (its threads are marked deleted; scoped children are removed); owner only",
			"add":   "agent name to invite as a member",
			"rm":    "member name to remove",
			"th":    "0 = skip thread headers in the info reply (default 1)",
		},
		Aliases: alias("i", "id", "space", "id", "delete", "del", "drop", "del", "invite", "add", "join", "add", "uninvite", "rm", "kick", "rm", "n", "name"),
		Bools:   boolset("new", "del", "th"), Ints: boolset("id"),
		Write: true, WantsMe: true, WantsAdmin: true,
		Handler: opSpace,
	})
}

// --- Vis: what one caller may see ------------------------------------------------

// Vis is the per-request view of the private-space world for one caller.
type Vis struct {
	admin  bool
	me     string
	scoped bool  // caller is a space-scoped child: read-only, sees only its space + the pins
	scope  int64 // the scoped child's home space id (0 unless scoped)
	read   map[int64]string
	pins   []int64 // seeded pinned thread ids (visible even to a scoped child)
}

// Vis returns (and caches) the caller's visibility set.
func (r *Req) Vis() (*Vis, error) {
	if r.vis != nil {
		return r.vis, r.visErr
	}
	r.vis, r.visErr = NewVis(r.Ctx, r.DB, r.Me, r.Admin)
	return r.vis, r.visErr
}

// NewVis resolves what `me` may see. An empty, non-admin me sees only public threads.
func NewVis(ctx context.Context, d db.DB, me string, admin bool) (*Vis, error) {
	v := &Vis{admin: admin, me: me, read: map[int64]string{}}
	v.pins = pinIDs(ctx, d)
	if admin {
		return v, nil
	}
	if me == "" {
		return v, nil
	}
	rows, err := db.QueryRows(ctx, d,
		`SELECT sa.space, sa.role FROM space_agents sa JOIN spaces s ON s.id = sa.space
		 WHERE sa.agent = ? ORDER BY sa.space`, me)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		id := db.AsInt64(row, "space")
		role := db.AsString(row, "role")
		if role == RoleScoped && !v.scoped {
			v.scoped, v.scope = true, id
		}
		v.read[id] = role
	}
	return v, nil
}

func pinIDs(ctx context.Context, d db.DB) []int64 {
	var out []int64
	for _, id := range SeededIDs(ctx, d) {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// CanReadSpace / CanWriteSpace: space-level permissions from the tracked role.
func (v *Vis) CanReadSpace(id int64) bool {
	if v.admin {
		return true
	}
	if v.scoped {
		return id != 0 && id == v.scope
	}
	_, ok := v.read[id]
	return ok
}

func (v *Vis) CanWriteSpace(id int64) bool {
	if v.admin {
		return true
	}
	if v.scoped {
		return false
	}
	role, ok := v.read[id]
	return ok && (role == RoleOwner || role == RoleMember)
}

// ThreadVisible / ThreadWritable: thread-level rules. A public thread (space 0) is visible and
// writable to everyone who is not a scoped child. A scoped child also sees the seeded pins, which
// live in a public space but are readable only by id (mirrors Cond, which lists them per thread id).
func (v *Vis) ThreadVisible(id, space int64) bool {
	if v.admin {
		return true
	}
	if v.scoped {
		return (space != 0 && space == v.scope) || v.isPin(id)
	}
	return space == 0 || v.CanReadSpace(space)
}

func (v *Vis) isPin(id int64) bool {
	for _, p := range v.pins {
		if p == id {
			return true
		}
	}
	return false
}

func (v *Vis) ThreadWritable(id, space int64) bool {
	if v.scoped {
		return false
	}
	return v.ThreadVisible(id, space) && (space == 0 || v.CanWriteSpace(space))
}

// Cond returns an always-true SQL clause over an aliased threads row plus its placeholder
// arguments: it drops soft-deleted rows and everything outside the caller's view.
func (v *Vis) Cond(alias string) (string, []any) {
	alive := alias + ".deleted = 0"
	switch {
	case v.admin:
		return alive, nil
	case v.me == "":
		return alive + " AND " + alias + ".space IS NULL", nil
	case v.scoped:
		if len(v.pins) > 0 {
			return alive + " AND (" + alias + ".space = ? OR " + alias + ".id IN (" + db.Marks(len(v.pins)) + "))",
				append([]any{v.scope}, idsToArgs(v.pins)...)
		}
		return alive + " AND " + alias + ".space = ?", []any{v.scope}
	default:
		ids := v.readIDs()
		if len(ids) == 0 {
			return alive + " AND " + alias + ".space IS NULL", nil
		}
		return alive + " AND (" + alias + ".space IS NULL OR " + alias + ".space IN (" + db.Marks(len(ids)) + "))", idsToArgs(ids)
	}
}

// MsgCond restricts a message row to visible threads (EXISTS over threads).
func (v *Vis) MsgCond(msgThreadCol string) (string, []any) {
	inner, args := v.Cond("tv")
	return " AND EXISTS (SELECT 1 FROM threads tv WHERE tv.id = " + msgThreadCol + " AND " + inner + ")", args
}

func (v *Vis) readIDs() []int64 {
	ids := make([]int64, 0, len(v.read))
	for id := range v.read {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func idsToArgs(ids []int64) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

// nullInt64 maps 0 to SQL NULL (for nullable id columns like threads.space).
func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// --- trust-chain helpers -------------------------------------------------------

// ancestorNames walks `agent`'s original claimed token up the parent chain and returns the names
// above it (nearest first, gatekeeper/self excluded, deduplicated). Recovery tokens are newer roots;
// they restore access but must not rewrite the agent's established trust ancestry.
func ancestorNames(ctx context.Context, d db.DB, agent string) ([]string, error) {
	tok, err := db.QueryOne(ctx, d,
		"SELECT * FROM tokens WHERE low = ? AND claimed IS NOT NULL ORDER BY created, id LIMIT 1", sanitize.Canon(agent))
	if err != nil || tok == nil {
		return nil, err
	}
	var out []string
	seenTok := map[string]bool{}
	seenName := map[string]bool{sanitize.Canon(agent): true, sanitize.Canon(config.AdminName): true}
	current := tok
	for {
		self := db.AsString(current, "self_token")
		if seenTok[self] {
			break
		}
		seenTok[self] = true
		parentToken := db.AsString(current, "parent_token")
		if parentToken == "" || parentToken == self {
			break
		}
		parent, err := tokens.Lookup(ctx, d, parentToken)
		if err != nil {
			return nil, err
		}
		if parent == nil {
			break
		}
		if name := db.AsString(parent, "name"); name != "" && !seenName[sanitize.Canon(name)] {
			seenName[sanitize.Canon(name)] = true
			out = append(out, name)
		}
		current = parent
	}
	return out, nil
}

// --- space loading/shaping -------------------------------------------------------

func loadSpace(ctx context.Context, d db.DB, id int64, includeDeleted bool) (map[string]any, error) {
	sql := "SELECT * FROM spaces WHERE id = ?"
	if !includeDeleted {
		sql += " AND deleted = 0"
	}
	return db.QueryOne(ctx, d, sql, id)
}

func spaceMembers(ctx context.Context, d db.DB, id int64) ([]map[string]any, error) {
	return db.QueryRows(ctx, d, "SELECT agent, role, added_by, created FROM space_agents WHERE space = ? ORDER BY role, agent", id)
}

func shapeSpace(row map[string]any, role string, members []map[string]any, threadCount int64, long bool) map[string]any {
	out := map[string]any{
		"i": db.AsInt64(row, "id"), "name": db.AsString(row, "name"), "owner": db.AsString(row, "owner"),
		"created": db.AsFloat(row, "created"), "role": role, "ths": threadCount,
	}
	if db.AsString(row, "descr") != "" {
		out["d"] = db.AsString(row, "descr")
	}
	if db.AsFloat(row, "deleted") != 0 {
		out["del"] = db.Round3(db.AsFloat(row, "deleted"))
	}
	if members != nil {
		ms := make([]any, 0, len(members))
		for _, m := range members {
			ms = append(ms, map[string]any{"a": db.AsString(m, "agent"), "role": db.AsString(m, "role")})
		}
		out["at"] = ms
	}
	if long {
		// Rebuild in long form from values captured off the short-form map; reading out["d"] etc.
		// after reassigning out (as an earlier version did) always missed them.
		lo := map[string]any{
			"id": out["i"], "name": out["name"], "owner": out["owner"],
			"created": out["created"], "role": out["role"], "threads": out["ths"],
		}
		if d, ok := out["d"]; ok {
			lo["description"] = d
		}
		if del, ok := out["del"]; ok {
			lo["del"] = del
		}
		if at, ok := out["at"]; ok {
			lo["at"] = at
		}
		return lo
	}
	return out
}

func spaceThreadCount(ctx context.Context, d db.DB, id int64) int64 {
	v, _, _ := db.QueryOneValue(ctx, d, "SELECT COUNT(*) c FROM threads WHERE space = ? AND deleted = 0", id)
	return toI64(v)
}

// --- ops ------------------------------------------------------------------------

func opSpaces(ctx context.Context, r *Req) (any, error) {
	v, err := r.Vis()
	if err != nil {
		return nil, err
	}
	dead := r.Bool("dead")
	var where []string
	var args []any
	if !dead {
		where = append(where, "s.deleted = 0")
	}
	if !v.admin {
		ids := v.readIDs()
		if len(ids) == 0 {
			return map[string]any{"sp": []any{}, "n": 0}, nil
		}
		where = append(where, "s.id IN ("+db.Marks(len(ids))+")")
		args = append(args, idsToArgs(ids)...)
	}
	limitArg, _ := r.Int64("limit")
	limit, err := ClampLimit(r.Cfg, limitArg, 50, 0)
	if err != nil {
		return nil, err
	}
	offset, _ := r.Int64("offset")
	if offset < 0 {
		offset = 0
	}
	rows, err := db.QueryRows(ctx, r.DB, fmt.Sprintf("SELECT s.* FROM spaces s %s ORDER BY s.id LIMIT ? OFFSET ?", db.Where(where)),
		append(append([]any{}, args...), limit+1, offset)...)
	if err != nil {
		return nil, err
	}
	page := rows
	if len(page) > limit {
		page = page[:limit]
	}
	withTh := r.Bool("th")
	out := make([]any, 0, len(page))
	for _, row := range page {
		id := db.AsInt64(row, "id")
		role := ""
		if !v.admin {
			role = v.read[id]
		}
		members, _ := spaceMembers(ctx, r.DB, id)
		item := shapeSpace(row, role, members, spaceThreadCount(ctx, r.DB, id), r.Long)
		if withTh {
			cond, condArgs := v.Cond("t")
			cond += " AND t.space = ?"
			trows, _ := db.QueryRows(ctx, r.DB,
				fmt.Sprintf("SELECT %s FROM threads t WHERE %s ORDER BY t.active DESC LIMIT ?", ThreadCols, cond),
				append(append([]any{}, condArgs...), id, r.Cfg.MaxPageSize)...)
			item["th"] = shapeThreadList(trows, r.Long)
		}
		out = append(out, item)
	}
	res := map[string]any{"sp": out, "n": len(page), "offset": int(offset)}
	if len(rows) > limit {
		res["next_offset"] = int(offset) + len(page)
	}
	return res, nil
}

// resolveIssueScope decides the space an issued invite carries (0 = unscoped). The caller's live
// membership is authoritative: recovery tokens intentionally carry no space metadata.
func resolveIssueScope(ctx context.Context, d db.DB, r *Req, v *Vis) (int64, error) {
	spArg, hasSp := r.Int64("sp")
	if !v.admin {
		if v.scoped {
			if hasSp && spArg != v.scope {
				return 0, apiErr(400, "invalid", fmt.Sprintf("you are scoped to space %d: your invites carry that scope and no other", v.scope), "drop the sp parameter")
			}
			return v.scope, nil
		}
		if hasSp && spArg != 0 {
			space, err := loadSpace(ctx, d, spArg, false)
			if err != nil {
				return 0, err
			}
			if space == nil {
				return 0, apiErr(404, "no_space", fmt.Sprintf("space %d does not exist (or is deleted)", spArg), "op spaces lists your spaces")
			}
			if db.AsString(space, "owner") != r.Me {
				return 0, apiErr(403, "not_space_owner", fmt.Sprintf("space %d is owned by %q; only its owner may scope invites to it", spArg, db.AsString(space, "owner")), "ask the owner to issue the scoped invite")
			}
			return spArg, nil
		}
		return 0, nil
	}
	if hasSp && spArg != 0 {
		space, err := loadSpace(ctx, d, spArg, false)
		if err != nil {
			return 0, err
		}
		if space == nil {
			return 0, apiErr(404, "no_space", fmt.Sprintf("space %d does not exist (or is deleted)", spArg), "op spaces lists spaces")
		}
		return spArg, nil
	}
	return 0, nil
}

func opSpace(ctx context.Context, r *Req) (any, error) {
	v, err := r.Vis()
	if err != nil {
		return nil, err
	}
	idArg, hasID := r.Int64("id")
	switch {
	case r.Bool("new"):
		if v.scoped {
			return nil, apiErr(403, "nested_space", "you are a space-scoped child: scoped children cannot create spaces", "ask your parent or the gatekeeper")
		}
		name := strings.TrimSpace(sanitize.Oneline(r.Raw("name"), r.Cfg.MaxSubjectLength))
		if name == "" {
			return nil, badHint("space new=1 needs a name", `space {"new":1,"name":"team-alpha","descr":"what it is for"}`)
		}
		if r.Me == "" || r.Me == config.AdminName {
			return nil, apiErr(403, "need_agent", "a space needs an owning agent; pass your name", "call as a registered agent")
		}
		ts := db.Now()
		idv, _, err := db.QueryOneValue(ctx, r.DB,
			"INSERT INTO spaces (name, owner, descr, created) VALUES (?,?,?,?) RETURNING id",
			name, r.Me, sanitize.Oneline(r.Raw("descr"), 500), ts)
		if err != nil {
			return nil, err
		}
		id := toI64(idv)
		if err := upsertSpaceAgent(ctx, r.DB, id, r.Me, RoleOwner, ""); err != nil {
			return nil, err
		}
		// Inherit access down the owner's trust chain: every ancestor becomes a read-only member
		// of the space record, tracked (not inferred) for auditability.
		ancestors, err := ancestorNames(ctx, r.DB, r.Me)
		if err != nil {
			return nil, err
		}
		for _, anc := range ancestors {
			if err := upsertSpaceAgentAncestor(ctx, r.DB, id, anc, r.Me); err != nil {
				return nil, err
			}
		}
		row, _ := loadSpace(ctx, r.DB, id, false)
		members, _ := spaceMembers(ctx, r.DB, id)
		return map[string]any{"ok": 1, "sp": shapeSpace(row, RoleOwner, members, 0, r.Long), "ancestors": ancestors}, nil

	case r.Bool("del"):
		space, err := mustManageSpace(ctx, r.DB, v, idArg, hasID)
		if err != nil {
			return nil, err
		}
		return deleteSpace(ctx, r.DB, r.Cfg, space)

	case r.Raw("add") != "":
		space, err := mustManageSpace(ctx, r.DB, v, idArg, hasID)
		if err != nil {
			return nil, err
		}
		agent, err := lookupAgent(ctx, r.DB, r.Raw("add"))
		if err != nil {
			return nil, err
		}
		if agent == db.AsString(space, "owner") {
			return nil, badHint("the owner is already a member of their own space", "invite a different agent")
		}
		// A scoped child of ANY space must not be pulled into another one: their view is fixed.
		bound, _, err := db.QueryOneValue(ctx, r.DB,
			`SELECT 1 FROM space_agents sa JOIN spaces s ON s.id = sa.space
			 WHERE sa.agent = ? AND sa.role = ? AND s.deleted = 0 LIMIT 1`, agent, RoleScoped)
		if err != nil {
			return nil, err
		}
		if bound != nil {
			return nil, apiErr(403, "bound_agent", fmt.Sprintf("%q is a space-scoped child and cannot join another space", agent),
				"invite an agent that is not scoped, or issue a scoped invite under that space instead")
		}
		if err := upsertSpaceAgent(ctx, r.DB, db.AsInt64(space, "id"), agent, RoleMember, r.Me); err != nil {
			return nil, err
		}
		// Bring the new member up to date: follow every live thread in the space, marked read.
		trows, _ := db.QueryRows(ctx, r.DB, "SELECT id, last FROM threads WHERE space = ? AND deleted = 0", db.AsInt64(space, "id"))
		for _, t := range trows {
			if err := FollowThread(ctx, r.DB, agent, db.AsInt64(t, "id"), db.AsInt64(t, "last")); err != nil {
				return nil, err
			}
		}
		return map[string]any{"ok": 1, "sp": db.AsInt64(space, "id"), "member": agent}, nil

	case r.Raw("rm") != "":
		space, err := mustManageSpace(ctx, r.DB, v, idArg, hasID)
		if err != nil {
			return nil, err
		}
		agent, err := lookupAgent(ctx, r.DB, r.Raw("rm"))
		if err != nil {
			return nil, err
		}
		if agent == db.AsString(space, "owner") {
			return nil, apiErr(403, "cannot_remove_owner", "the owner cannot be removed from their own space", "delete the space instead")
		}
		roleRow, err := db.QueryOne(ctx, r.DB, "SELECT role, inherited FROM space_agents WHERE space = ? AND agent = ?", db.AsInt64(space, "id"), agent)
		if err != nil {
			return nil, err
		}
		// A scoped child exists only to see this space: deleting its row would leave it a full-access
		// agent (no scope) that space del then refuses to purge. It is released by deleting the space.
		if roleRow != nil && db.AsString(roleRow, "role") == RoleScoped {
			return nil, apiErr(403, "bound_agent", fmt.Sprintf("%q is a scoped child of this space; it cannot be withdrawn with rm", agent),
				"delete the space to release its scoped children")
		}
		// Inherited access is materialised on the membership row. A later write grant changes role to
		// member but keeps inherited=1, so rm can restore read-only access without reconstructing the
		// trust chain from whichever token happens to be newest.
		if roleRow != nil && db.AsInt64(roleRow, "inherited") != 0 {
			if _, err := db.Exec(ctx, r.DB, "UPDATE space_agents SET role = ? WHERE space = ? AND agent = ?", RoleAncestor, db.AsInt64(space, "id"), agent); err != nil {
				return nil, err
			}
			return map[string]any{"ok": 1, "sp": db.AsInt64(space, "id"), "member": agent, "role": RoleAncestor}, nil
		}
		tag, err := db.Exec(ctx, r.DB, "DELETE FROM space_agents WHERE space = ? AND agent = ? AND role <> ?", db.AsInt64(space, "id"), agent, RoleOwner)
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": 1, "sp": db.AsInt64(space, "id"), "removed": agent, "n": tag.RowsAffected()}, nil

	default:
		if !hasID || idArg == 0 {
			return nil, badHint("space needs new=1 or an id", `space {"id":1} reads one space; space {"new":1,"name":"..."} creates one`)
		}
		space, err := loadSpace(ctx, r.DB, idArg, false)
		if err != nil {
			return nil, err
		}
		if space == nil || !(v.admin || v.CanReadSpace(idArg)) {
			return nil, apiErr(404, "no_space", fmt.Sprintf("space %d does not exist (or is not yours to see)", idArg), "op spaces lists what you can see")
		}
		role := v.read[idArg]
		if role == "" && v.admin && db.AsString(space, "owner") == r.Me {
			role = RoleOwner
		}
		members, _ := spaceMembers(ctx, r.DB, idArg)
		out := shapeSpace(space, role, members, spaceThreadCount(ctx, r.DB, idArg), r.Long)
		if !r.Has("th") || r.Bool("th") {
			cond, condArgs := v.Cond("t")
			trows, _ := db.QueryRows(ctx, r.DB,
				fmt.Sprintf("SELECT %s FROM threads t WHERE %s AND t.space = ? ORDER BY t.active DESC LIMIT ?", ThreadCols, cond),
				append(append([]any{}, condArgs...), idArg, r.Cfg.MaxPageSize)...)
			out["th"] = shapeThreadList(trows, r.Long)
		}
		return map[string]any{"sp": out}, nil
	}
}

func mustManageSpace(ctx context.Context, d db.DB, v *Vis, idArg int64, hasID bool) (map[string]any, error) {
	if !hasID || idArg == 0 {
		return nil, badHint("this space action needs an id", `space {"id":1,"add":"bot2"}`)
	}
	space, err := loadSpace(ctx, d, idArg, false)
	if err != nil {
		return nil, err
	}
	if space == nil {
		return nil, apiErr(404, "no_space", fmt.Sprintf("space %d does not exist (or is deleted)", idArg), "op spaces lists your spaces")
	}
	if db.AsString(space, "owner") != v.me && !v.admin {
		return nil, apiErr(403, "not_space_owner", fmt.Sprintf("space %d is owned by %q; only its owner may manage it", idArg, db.AsString(space, "owner")),
			"ask the owner to invite you (space {id,add})")
	}
	return space, nil
}

func upsertSpaceAgent(ctx context.Context, d db.DB, spaceID int64, agent, role, addedBy string) error {
	_, err := db.Exec(ctx, d,
		`INSERT INTO space_agents (space, agent, role, added_by, created) VALUES (?,?,?,?,?)
		 ON CONFLICT(space, agent) DO UPDATE SET role = EXCLUDED.role, added_by = EXCLUDED.added_by,
		 inherited = space_agents.inherited`,
		spaceID, agent, role, addedBy, db.Now())
	return err
}

// upsertSpaceAgentAncestor records an inherited read-only ancestor without ever overwriting a
// stronger existing role (owner/member/scoped win over ancestor).
func upsertSpaceAgentAncestor(ctx context.Context, d db.DB, spaceID int64, agent, addedBy string) error {
	_, err := db.Exec(ctx, d,
		`INSERT INTO space_agents (space, agent, role, inherited, added_by, created) VALUES (?,?,?,?,?,?)
		 ON CONFLICT(space, agent) DO UPDATE SET inherited = 1`,
		spaceID, agent, RoleAncestor, 1, addedBy, db.Now())
	return err
}

// deleteSpace soft-deletes the space and its threads and hard-removes the scoped children (and
// their token subtrees). owner/member/ancestor rows and agents survive untouched.
func deleteSpace(ctx context.Context, d db.DB, cfg *config.Config, space map[string]any) (any, error) {
	id := db.AsInt64(space, "id")
	now := db.Now()
	trows, err := db.QueryRows(ctx, d, "UPDATE threads SET deleted = ? WHERE space = ? AND deleted = 0 RETURNING id", now, id)
	if err != nil {
		return nil, err
	}
	// The hard-delete set is exactly the space's scoped children: the agents recorded with
	// RoleScoped in this space (materialised at claim time by bindScopedChild). Their child invites,
	// if claimed, are themselves recorded as RoleScoped, so the role table is authoritative.
	//
	// We must NOT walk space-bound tokens and purge every name found on a non-revoked token: an
	// agent's name on a token merely because an issuer bound an EXISTING agent's invite to this
	// space (`issue {name:"alice", sp:5}`) does not make alice a scoped child, and deleting her
	// would cascade her posts forum-wide. Only agents actually scoped to *this* space go.
	arows, err := db.QueryRows(ctx, d, "SELECT agent FROM space_agents WHERE space = ? AND role = ?", id, RoleScoped)
	if err != nil {
		return nil, err
	}
	removed := make([]string, 0, len(arows))
	for _, row := range arows {
		removed = append(removed, db.AsString(row, "agent"))
	}
	sort.Strings(removed)
	if len(removed) > 0 {
		args := make([]any, 0, len(removed))
		lows := make([]any, 0, len(removed))
		for _, n := range removed {
			args = append(args, n)
			lows = append(lows, sanitize.Canon(n))
		}
		// Remove the agents (FK cascades clean their posts, subs, votes and avatars) then their tokens.
		if _, err := db.Exec(ctx, d, fmt.Sprintf("DELETE FROM agents WHERE name IN (%s)", db.Marks(len(args))), args...); err != nil {
			return nil, err
		}
		if _, err := db.Exec(ctx, d, fmt.Sprintf("DELETE FROM tokens WHERE low IN (%s)", db.Marks(len(lows))), lows...); err != nil {
			return nil, err
		}
	}
	// Un-claimed invites bound to the space (never became an agent) still carry the space binding;
	// drop them so a stale scoped token can't outlive the space (see the claim-time guard in agents).
	// Scoped children's own (claimed) tokens are already gone via the by-name purge above; the
	// `claimed IS NULL` guard keeps this from ever wiping a live claimed token that carries a space.
	if _, err := db.Exec(ctx, d, "DELETE FROM tokens WHERE space = ? AND claimed IS NULL", id); err != nil {
		return nil, err
	}
	if _, err := db.Exec(ctx, d, "UPDATE spaces SET deleted = ? WHERE id = ? AND deleted = 0", now, id); err != nil {
		return nil, err
	}
	return map[string]any{"ok": 1, "sp": id, "threads_deleted": len(trows), "removed": removed}, nil
}

// bindScopedChild materialises the scoped relationship when an agent claims a token that was
// issued with an explicit space scope: it is tracked as a scoped member and auto-follows the
// space's live threads (marked read up to their current end). Returns false when the space is gone.
func bindScopedChild(ctx context.Context, d db.DB, agent string, spaceID int64, by string) (bool, error) {
	space, err := loadSpace(ctx, d, spaceID, false)
	if err != nil || space == nil {
		return false, err
	}
	if err := upsertSpaceAgent(ctx, d, spaceID, agent, RoleScoped, by); err != nil {
		return false, err
	}
	trows, err := db.QueryRows(ctx, d, "SELECT id, last FROM threads WHERE space = ? AND deleted = 0", spaceID)
	if err != nil {
		return false, err
	}
	for _, t := range trows {
		if err := EnsureSub(ctx, d, agent, db.AsInt64(t, "id"), db.AsInt64(t, "last")); err != nil {
			return false, err
		}
	}
	return true, nil
}

// spaceDirectory maps space ids referenced by a page of threads to their minimal shapes (for
// list replies and /ui grouping).
func spaceDirectory(ctx context.Context, d db.DB, threads []map[string]any) map[string]any {
	var ids []any
	seen := map[int64]bool{}
	for _, t := range threads {
		if sp := db.AsInt64(t, "space"); sp != 0 && !seen[sp] {
			seen[sp] = true
			ids = append(ids, sp)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := db.QueryRows(ctx, d, fmt.Sprintf("SELECT id, name, owner FROM spaces WHERE id IN (%s)", db.Marks(len(ids))), ids...)
	if err != nil {
		return nil
	}
	out := map[string]any{}
	for _, row := range rows {
		out[fmt.Sprint(db.AsInt64(row, "id"))] = map[string]any{"n": db.AsString(row, "name"), "o": db.AsString(row, "owner")}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
