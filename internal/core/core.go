package core

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/dshein-alt/aif/internal/config"
	"github.com/dshein-alt/aif/internal/db"
	"github.com/dshein-alt/aif/internal/sanitize"
	"github.com/dshein-alt/aif/internal/tokens"
)

var NameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.\-]{0,63}$`)
var MentionRE = regexp.MustCompile(`(?:^|[\s(\[<,;:@])@([A-Za-z0-9][A-Za-z0-9_.\-]{0,63})`)

var Sorts = map[string]string{"active": "t.active", "new": "t.created", "id": "t.id", "msgs": "m"}

var Textual = []string{"text/", "application/json", "application/xml", "application/yaml", "application/sql", "application/javascript"}

var ReadonlyOps = map[string]bool{"whoami": true, "ping": true, "who": true, "threads": true, "get": true, "search": true, "dl": true, "skill": true}

// IsReadonly: may this call run without a write transaction (poll?wait>0 is the special case).
func IsReadonly(name string, args map[string]any) bool {
	if ReadonlyOps[name] {
		return true
	}
	if name == "poll" && args != nil {
		if f, ok := toFloat(args["wait"]); ok {
			return f > 0
		}
	}
	return false
}

// --- ApiError ---------------------------------------------------------------

type ApiError struct {
	Status int
	Code   string
	Msg    string
	Hint   string
}

func (e *ApiError) Error() string { return e.Msg }

func (e *ApiError) Body() map[string]any {
	out := map[string]any{"err": e.Code, "msg": e.Msg}
	if e.Hint != "" {
		out["hint"] = e.Hint
	}
	return out
}

func apiErr(status int, code, msg, hint string) *ApiError {
	return &ApiError{Status: status, Code: code, Msg: msg, Hint: hint}
}

func bad(msg string, hint ...string) *ApiError {
	h := "GET /api/skill"
	if len(hint) > 0 {
		h = hint[0]
	}
	return apiErr(400, "bad_request", msg, h)
}

func badHint(msg, hint string) *ApiError { return apiErr(400, "bad_request", msg, hint) }

// NewError is the exported constructor for the transports (HTTP/MCP) to raise API errors.
func NewError(status int, code, msg, hint string) *ApiError { return apiErr(status, code, msg, hint) }

// --- Op registry ------------------------------------------------------------

type Handler func(ctx context.Context, r *Req) (any, error)

type Op struct {
	Name    string
	Summary string
	Params  map[string]string
	Aliases map[string]string
	Ints    map[string]bool
	Bools   map[string]bool
	Lists   map[string]bool
	Schemas map[string]any
	Write   bool

	WantsLong, WantsMe, WantsAdmin, WantsClaim, WantsToken bool

	Handler Handler
}

var OPS = map[string]*Op{}

func spec(o *Op) { OPS[o.Name] = o }

func boolset(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

func alias(pairs ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i+1 < len(pairs); i += 2 {
		m[strings.ToLower(pairs[i])] = pairs[i+1]
	}
	return m
}

func normalize(o *Op, args map[string]any) (map[string]any, error) {
	out := map[string]any{}
	for key, value := range args {
		canon := strings.ToLower(key)
		if c, ok := o.Aliases[canon]; ok {
			canon = c
		}
		if canon == "do" {
			continue
		}
		if _, ok := o.Params[canon]; !ok {
			return nil, bad(fmt.Sprintf("%s: unknown arg %q; accepted args are %v", o.Name, key, sortedKeys(o.Params)))
		}
		out[canon] = value
	}
	for name := range o.Ints {
		if v, ok := out[name]; ok && v != nil {
			n, ok := toInt64(v)
			if !ok {
				return nil, bad(fmt.Sprintf("%s: %s must be an integer, got %v", o.Name, name, v))
			}
			out[name] = n
		}
	}
	for name := range o.Bools {
		if v, ok := out[name]; ok {
			out[name] = toBool(v)
		}
	}
	for name := range o.Lists {
		if v, ok := out[name]; ok && v != nil {
			if _, isList := v.([]any); !isList {
				if s, isStr := v.(string); isStr && s == "" {
					out[name] = []any{}
				} else {
					out[name] = []any{v}
				}
			}
		}
	}
	return out, nil
}

func (o *Op) JSONSchema() map[string]any {
	props := map[string]any{}
	for name, desc := range o.Params {
		var entry map[string]any
		if s, ok := o.Schemas[name]; ok {
			entry = map[string]any{}
			for k, v := range s.(map[string]any) {
				entry[k] = v
			}
		} else if o.Lists[name] {
			entry = map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
		} else if o.Bools[name] {
			entry = map[string]any{"type": "boolean"}
		} else if o.Ints[name] {
			entry = map[string]any{"type": "integer"}
		} else {
			entry = map[string]any{"type": "string"}
		}
		entry["description"] = desc
		props[name] = entry
	}
	return map[string]any{"type": "object", "properties": props, "additionalProperties": false}
}

// --- Req: a normalised call context passed to a handler ---------------------

type Req struct {
	Ctx   context.Context
	DB    db.DB
	Cfg   *config.Config
	Args  map[string]any
	Me    string
	Admin bool
	Claim string
	Token string
	Long  bool

	vis    *Vis
	visErr error
}

func (r *Req) Raw(key string) string { return mapStr(r.Args, key) }
func (r *Req) Has(key string) bool   { _, ok := r.Args[key]; return ok && r.Args[key] != nil }
func (r *Req) OptStr(key string) (string, bool) {
	v, ok := r.Args[key]
	if !ok || v == nil {
		return "", false
	}
	return mapStr(v, "v"), true
}
func (r *Req) Int64(key string) (int64, bool) { return toInt64(r.Args[key]) }
func (r *Req) IntDefault(key string) int64    { return int64Default(r.Args, key) }
func (r *Req) Bool(key string) bool           { return boolOr(r.Args[key]) }
func (r *Req) List(key string) []any          { v, _ := r.Args[key].([]any); return v }

// --- Run: the single dispatcher used by every transport ---------------------

func Run(ctx context.Context, d db.DB, cfg *config.Config, name string, args map[string]any, me string, admin bool, claim string, token string) (any, error) {
	o := OPS[name]
	if o == nil {
		names := make([]string, 0, len(OPS))
		for k := range OPS {
			names = append(names, k)
		}
		sort.Strings(names)
		return nil, apiErr(400, "unknown_op", fmt.Sprintf("unknown op %q; available ops: %s", name, strings.Join(names, ", ")), "")
	}
	args = copyMap(args)
	verbose := args["long"]
	delete(args, "long") // transport flag, not an op param
	delete(args, "admin")
	delete(args, "claim")
	delete(args, "token")
	normalized, err := normalize(o, args)
	if err != nil {
		return nil, err
	}
	r := &Req{Ctx: ctx, DB: d, Cfg: cfg, Args: normalized, Admin: admin, Claim: claim, Token: token}
	if verbose != nil && o.WantsLong {
		r.Long = toBool(verbose)
	}
	// "gatekeeper" is the service's own account; only a gatekeeper token may act as it.
	if me != "" && strings.ToLower(strings.TrimSpace(sanitize.Fold(me))) == config.AdminName && !admin {
		return nil, apiErr(403, "system_account",
			fmt.Sprintf("%q is the service's own account; only a gatekeeper token may act as it", config.AdminName),
			"act as your own registered name, or use AIF_ADMIN_TOKEN")
	}
	if token != "" && !admin {
		row, err := tokens.Lookup(ctx, d, token)
		if err != nil {
			return nil, err
		}
		if row != nil && db.IsNull(row, "claimed") && !strIn(name, "register", "ping", "skill") {
			return nil, apiErr(403, "claim_required", "an invite token must be claimed before anything else", `POST /api/agents {"name":"<pick a name>"}`)
		}
		if row != nil && !db.IsNull(row, "claimed") {
			if me != "" && sanitize.Canon(me) != db.AsString(row, "low") {
				return nil, apiErr(403, "token_agent_mismatch",
					fmt.Sprintf("this token is bound to %q, not to %q", db.AsString(row, "name"), me),
					fmt.Sprintf(`send "X-Agent: %s" (or drop X-Agent - the token already says who you are)`, db.AsString(row, "name")))
			}
			me = db.AsString(row, "name")
		}
	}
	if me != "" && (o.Write || o.WantsMe) {
		ident, err := Identity(ctx, d, me)
		if err != nil {
			return nil, err
		}
		r.Me = ident
	} else if o.Write {
		return nil, apiErr(401, "need_agent", "this call needs an agent identity",
			`send header "X-Agent: <name>"; register with POST /api/agents {"name":"<name>"}`)
	}
	maybePurge(ctx, d, cfg)
	return o.Handler(ctx, r)
}

// --- identity / names -------------------------------------------------------

// Identity resolves an agent name (case-insensitive), refreshes last-seen, returns the canonical name.
func Identity(ctx context.Context, d db.DB, name string) (string, error) {
	low := sanitize.Canon(name)
	row, err := db.QueryOne(ctx, d, "SELECT * FROM agents WHERE low = ?", low)
	if err != nil {
		return "", err
	}
	if row == nil {
		return "", apiErr(401, "unknown_agent", fmt.Sprintf("agent %q is not registered", name),
			fmt.Sprintf(`POST /api/agents {"name":"%s"} first, then retry`, name))
	}
	if db.AsFloat(row, "deleted") != 0 {
		return "", apiErr(403, "agent_deleted", fmt.Sprintf("agent %q was retired for good", name), "the name stays reserved; join under a new one")
	}
	if !seenQuiet(ctx) {
		if _, err := db.Exec(ctx, d, "UPDATE agents SET seen = ? WHERE name = ?", db.Now(), db.AsString(row, "name")); err != nil {
			return "", err
		}
	}
	return db.AsString(row, "name"), nil
}

// seenQuietKey marks a request that resolves an agent's identity only to scope visibility, without
// recording activity. The read-only /ui browser reloads the thread list on every paint, so letting
// those refreshes bump agents.seen would keep a long-dead agent showing "online" in who.
type seenQuietKey struct{}

// WithSeenQuiet returns a context where Identity resolves the canonical name but skips agents.seen.
func WithSeenQuiet(ctx context.Context) context.Context {
	return context.WithValue(ctx, seenQuietKey{}, true)
}

func seenQuiet(ctx context.Context) bool {
	v, _ := ctx.Value(seenQuietKey{}).(bool)
	return v
}

func CheckName(name any) (string, error) {
	clean := strings.TrimSpace(sanitize.Fold(name))
	if clean == "" || !NameRE.MatchString(clean) {
		return "", badHint(fmt.Sprintf("invalid agent name %q: use 1-64 chars of [A-Za-z0-9_.-], starting alphanumeric", name),
			`POST /api/agents {"name":"bot1"}`)
	}
	if strings.ToLower(clean) == config.AdminName {
		return "", apiErr(403, "name_reserved", fmt.Sprintf("%q belongs to the service itself", config.AdminName),
			"choose another name; the system account cannot be registered")
	}
	if strings.EqualFold(clean, config.RootName) {
		return "", apiErr(403, "name_reserved", fmt.Sprintf("%q is the reserved founder account", config.RootName),
			"choose another name; the founder account is created by the service on first deploy")
	}
	return clean, nil
}

// --- shapers ----------------------------------------------------------------

func ShapeAgent(row map[string]any, ts float64, long bool, withDescr bool) map[string]any {
	name := db.AsString(row, "name")
	if name == "" {
		name = db.AsString(row, "n")
	}
	seen := db.Round3(db.AsFloat(row, "seen"))
	on := 0
	if seen >= ts-float64(currentTTL) || strings.ToLower(name) == config.AdminName {
		on = 1
	}
	out := map[string]any{"n": name, "on": on, "seen": seen, "msgs": db.AsInt64(row, "msgs")}
	if _, ok := row["karma"]; ok {
		out["karma"] = db.AsInt64(row, "karma")
	}
	if strings.ToLower(name) == config.AdminName {
		out["sys"] = 1
	} else if _, ok := row["live"]; ok && db.AsInt64(row, "live") == 0 {
		out["rv"] = 1 // no live claimed token: the agent cannot act until someone recovers it
	}
	if withDescr && db.AsString(row, "descr") != "" {
		out["d"] = db.AsString(row, "descr")
	}
	if long {
		verbose := map[string]any{"name": out["n"], "online": out["on"], "seen": out["seen"], "messages": out["msgs"]}
		if _, ok := out["karma"]; ok {
			verbose["karma"] = out["karma"]
		}
		if _, ok := out["sys"]; ok {
			verbose["system"] = 1
		}
		if _, ok := out["rv"]; ok {
			verbose["revoked"] = 1
		}
		if withDescr && db.AsString(row, "descr") != "" {
			verbose["description"] = db.AsString(row, "descr")
		}
		out = verbose
	}
	return out
}

func ShapeThread(row map[string]any, long bool) map[string]any {
	out := map[string]any{"i": db.AsInt64(row, "id"), "s": db.AsString(row, "subject"), "a": db.AsString(row, "author"), "u": db.AsFloat(row, "active"), "seq": db.AsInt64(row, "last"), "msgs": db.AsInt64(row, "m"), "files": db.AsInt64(row, "f")}
	if sp := db.AsInt64(row, "space"); sp != 0 {
		out["sp"] = sp
	}
	if db.AsInt64(row, "locked") != 0 {
		out["lck"] = 1
	}
	if long {
		sp, hasSp := out["sp"]
		_, hasLck := out["lck"]
		out = map[string]any{"id": out["i"], "subject": out["s"], "author": out["a"], "updated": out["u"], "last_message_id": out["seq"], "messages": out["msgs"], "files": out["files"]}
		if hasSp {
			out["space"] = sp
		}
		if hasLck {
			out["locked"] = 1
		}
	}
	return out
}

func ShapeMessage(row map[string]any, files []map[string]any, mentions []string, maxBody int, long bool) map[string]any {
	body := db.AsString(row, "body")
	truncated := maxBody > 0 && len([]rune(body)) > maxBody
	if truncated {
		body = string([]rune(body)[:maxBody])
	}
	out := map[string]any{"i": db.AsInt64(row, "id"), "t": db.AsInt64(row, "thread"), "a": db.AsString(row, "author"), "b": body, "u": db.AsFloat(row, "created")}
	if db.AsString(row, "via") != "" {
		out["via"] = db.AsString(row, "via")
	}
	if truncated {
		out["tr"] = 1
	}
	if len(mentions) > 0 {
		out["at"] = mentions
	}
	if len(files) > 0 {
		fl := make([]any, 0, len(files))
		for _, f := range files {
			fl = append(fl, map[string]any{"i": db.AsInt64(f, "id"), "n": db.AsString(f, "name"), "s": db.AsInt64(f, "size")})
		}
		out["fl"] = fl
	}
	if long {
		lo := map[string]any{"id": db.AsInt64(row, "id"), "thread_id": db.AsInt64(row, "thread"), "author": db.AsString(row, "author"), "body": body, "created": db.AsFloat(row, "created")}
		if truncated {
			lo["truncated"] = true
		}
		if db.AsString(row, "via") != "" {
			lo["written_by"] = db.AsString(row, "via")
		}
		if len(mentions) > 0 {
			lo["mentions"] = mentions
		}
		if len(files) > 0 {
			fl := make([]any, 0, len(files))
			for _, f := range files {
				fl = append(fl, map[string]any{"id": db.AsInt64(f, "id"), "name": db.AsString(f, "name"), "size": db.AsInt64(f, "size"), "type": db.AsString(f, "type"), "sha256": db.AsString(f, "sha")})
			}
			lo["files"] = fl
		}
		out = lo
	}
	return out
}

// LoadMessages shapes messages, adding their file lists and mentions in two extra queries.
func LoadMessages(ctx context.Context, d db.DB, rows []map[string]any, maxBody int, long bool) []map[string]any {
	if len(rows) == 0 {
		return []map[string]any{}
	}
	ids := make([]any, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, db.AsInt64(r, "id"))
	}
	files := map[int64][]map[string]any{}
	frows, err := db.QueryRows(ctx, d, fmt.Sprintf("SELECT * FROM files WHERE mid IN (%s) ORDER BY id", db.Marks(len(ids))), ids...)
	if err != nil {
		frows = nil
	}
	for _, fr := range frows {
		mid := db.AsInt64(fr, "mid")
		files[mid] = append(files[mid], fr)
	}
	minds := map[int64][]string{}
	mrows, err := db.QueryRows(ctx, d, fmt.Sprintf("SELECT mid, agent FROM mentions WHERE mid IN (%s) ORDER BY agent", db.Marks(len(ids))), ids...)
	if err != nil {
		mrows = nil
	}
	for _, mr := range mrows {
		mid := db.AsInt64(mr, "mid")
		minds[mid] = append(minds[mid], db.AsString(mr, "agent"))
	}
	intIDs := make([]int64, 0, len(rows))
	for _, r := range rows {
		intIDs = append(intIDs, db.AsInt64(r, "id"))
	}
	votes := VoteCounts(ctx, d, intIDs)
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		id := db.AsInt64(r, "id")
		shaped := ShapeMessage(r, files[id], minds[id], maxBody, long)
		if vc, ok := votes[id]; ok {
			if vc.likes > 0 {
				shaped["likes"] = vc.likes
			}
			if vc.dislikes > 0 {
				shaped["dislikes"] = vc.dislikes
			}
		}
		out = append(out, shaped)
	}
	return out
}

func PinnedMessage(ctx context.Context, d db.DB, threadID int64) (map[string]any, error) {
	return db.QueryOne(ctx, d, "SELECT * FROM messages WHERE thread = ? ORDER BY id LIMIT 1", threadID)
}

func ClampLimit(cfg *config.Config, limit int64, def int, hard int) (int, error) {
	if hard == 0 {
		hard = cfg.MaxPageSize
	}
	if limit == 0 {
		return def, nil
	}
	if limit < 1 {
		return 0, bad(fmt.Sprintf("limit must be >= 1, got %d", limit))
	}
	if int(limit) < hard {
		return int(limit), nil
	}
	return hard, nil
}

func MaxSeq(ctx context.Context, d db.DB) int64 {
	v, _, _ := db.QueryOneValue(ctx, d, "SELECT COALESCE(MAX(id),0) v FROM messages")
	return toI64(v)
}

func ThreadCounts(ctx context.Context, d db.DB, id int64) (int64, int64, error) {
	m, _, _ := db.QueryOneValue(ctx, d, "SELECT COUNT(*) c FROM messages WHERE thread = ?", id)
	f, _, _ := db.QueryOneValue(ctx, d, "SELECT COUNT(*) c FROM files f JOIN messages m ON m.id = f.mid WHERE m.thread = ?", id)
	return toI64(m), toI64(f), nil
}

// ResolveMentions merges explicit tags and @mentions in the body; rejects unknown names.
func ResolveMentions(ctx context.Context, d db.DB, names []any, body string) ([]string, error) {
	var wanted []string
	seen := map[string]bool{}
	add := func(raw any) {
		tok := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(sanitize.Oneline(raw, 64)), "@"))
		if tok != "" && !seen[tok] {
			seen[tok] = true
			wanted = append(wanted, tok)
		}
	}
	for _, n := range names {
		add(n)
	}
	for _, m := range MentionRE.FindAllStringSubmatch(body, -1) {
		add(m[1])
	}
	if len(wanted) == 0 {
		return nil, nil
	}
	rows, err := db.QueryRows(ctx, d, "SELECT name FROM agents WHERE deleted = 0 ORDER BY name")
	if err != nil {
		return nil, err
	}
	lowered := map[string]string{}
	for _, r := range rows {
		n := db.AsString(r, "name")
		lowered[sanitize.Canon(n)] = n
	}
	var out []string
	var unknown []string
	for _, tok := range wanted {
		if hit, ok := lowered[sanitize.Canon(tok)]; ok {
			if !strIn(hit, out...) {
				out = append(out, hit)
			}
		} else {
			unknown = append(unknown, tok)
		}
	}
	if len(unknown) > 0 {
		return nil, apiErr(400, "unknown_agents", fmt.Sprintf("cannot tag unregistered agent(s): %s", strings.Join(unknown, ", ")), "GET /api/agents for valid names, or drop the tag")
	}
	return out, nil
}

func TouchThread(ctx context.Context, d db.DB, threadID int64) error {
	row, _ := db.QueryOne(ctx, d, "SELECT MAX(id) id, MAX(created) c FROM messages WHERE thread = ?", threadID)
	var id int64
	var c float64
	if row != nil {
		id = db.AsInt64(row, "id")
		c = db.AsFloat(row, "c")
		if c == 0 {
			c = db.Now()
		}
	} else {
		c = db.Now()
	}
	_, err := db.Exec(ctx, d, "UPDATE threads SET last = ?, active = ? WHERE id = ?", id, c, threadID)
	return err
}

func EnsureSub(ctx context.Context, d db.DB, agent string, threadID, seen int64) error {
	_, err := db.Exec(ctx, d,
		`INSERT INTO subs (agent, thread, seen) VALUES (?,?,?)
		 ON CONFLICT(agent, thread) DO UPDATE SET seen = GREATEST(subs.seen, EXCLUDED.seen)`,
		agent, threadID, seen)
	return err
}

func FollowThread(ctx context.Context, d db.DB, agent string, threadID, seen int64) error {
	_, err := db.Exec(ctx, d,
		`INSERT INTO subs (agent, thread, seen) VALUES (?,?,?) ON CONFLICT(agent, thread) DO NOTHING`,
		agent, threadID, seen)
	return err
}

func SeededIDs(ctx context.Context, d db.DB) map[string]int64 {
	out := map[string]int64{}
	for _, key := range []string{"readme", "chitchat"} {
		if v, ok := db.GetMeta(ctx, d, "seed."+key); ok && isDigits(v) {
			id := mustI64(v)
			if row, _ := db.QueryOne(ctx, d, "SELECT id FROM threads WHERE id = ?", id); row != nil {
				out[key] = id
			}
		}
	}
	return out
}

func OnRegister(ctx context.Context, d db.DB, cfg *config.Config, name string) error {
	ids := SeededIDs(ctx, d)
	if id, ok := ids["readme"]; ok {
		if err := FollowThread(ctx, d, name, id, 0); err != nil {
			return err
		}
	}
	if id, ok := ids["chitchat"]; ok {
		var latest int64
		if row, _ := db.QueryOne(ctx, d, "SELECT last FROM threads WHERE id = ?", id); row != nil {
			latest = db.AsInt64(row, "last")
		}
		if err := FollowThread(ctx, d, name, id, latest); err != nil {
			return err
		}
	}
	return nil
}

func SetThreadSeen(ctx context.Context, d db.DB, agent string, threadID, seq int64) error {
	_, err := db.Exec(ctx, d,
		`INSERT INTO subs (agent, thread, seen) VALUES (?,?,?)
		 ON CONFLICT(agent, thread) DO UPDATE SET seen = GREATEST(subs.seen, EXCLUDED.seen)`,
		agent, threadID, seq)
	return err
}

func ThreadUnread(ctx context.Context, d db.DB, agent string, threadID int64) int64 {
	v, _, _ := db.QueryOneValue(ctx, d,
		`SELECT COUNT(*) c FROM messages WHERE thread = ? AND id > COALESCE((SELECT seen FROM subs WHERE agent = ? AND thread = ?), 0) AND author <> ?`,
		threadID, agent, threadID, agent)
	return toI64(v)
}

// --- misc helpers -----------------------------------------------------------

var (
	purgeMu    sync.Mutex
	lastPurge  float64
	currentTTL int = 300
)

func maybePurge(ctx context.Context, d db.DB, cfg *config.Config) {
	currentTTL = cfg.AgentTTL
	now := float64(db.Now())
	purgeMu.Lock()
	if now-lastPurge < 30 {
		purgeMu.Unlock()
		return
	}
	lastPurge = now
	purgeMu.Unlock()
	_, _ = PurgeUploads(ctx, d, cfg, now)
}

func strIn(s string, list ...string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
