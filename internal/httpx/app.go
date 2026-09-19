package httpx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"

	"github.com/dshein-alt/aif/internal/config"
	"github.com/dshein-alt/aif/internal/core"
	"github.com/dshein-alt/aif/internal/db"
	"github.com/dshein-alt/aif/internal/sanitize"
	"github.com/dshein-alt/aif/internal/storage"
	"github.com/dshein-alt/aif/internal/tokens"
)

// metaKeys are query params that never become op arguments.
var metaKeys = map[string]bool{"fmt": true, "token": true, "as": true, "agent": true, "me": true, "do": true, "op": true}

// App is the HTTP transport: config + a Postgres pool, mounting the REST, MCP and /ui surfaces.
type App struct {
	cfg     *config.Config
	pool    *db.Pool
	mountUI bool
	ready   bool
	readyMu sync.Mutex
}

func NewApp(cfg *config.Config, pool *db.Pool, mountUI bool) *App {
	return &App{cfg: cfg, pool: pool, mountUI: mountUI}
}

// Call dispatches one op, choosing a read-only path (the pool) for reads or a write transaction for
// writes.
func (a *App) Call(ctx context.Context, name string, args map[string]any, me string, admin bool, claim, token string) (any, error) {
	if _, ok := core.OPS[name]; !ok {
		names := make([]string, 0, len(core.OPS))
		for k := range core.OPS {
			names = append(names, k)
		}
		return nil, core.NewError(400, "unknown_op", fmt.Sprintf("unknown op %q; available ops: %s", name, strings.Join(names, ", ")), "")
	}
	if claim != "" && !strIn(name, "register", "ping", "skill") {
		return nil, core.NewError(403, "claim_required", "an invite token must be claimed before anything else", `POST /api/agents {"name":"<pick a name>"}`)
	}
	if core.IsReadonly(name, args) {
		return core.Run(ctx, a.pool, a.cfg, name, args, me, admin, claim, token)
	}
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	res, err := core.Run(ctx, tx, a.cfg, name, args, me, admin, claim, token)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return res, nil
}

// Router builds the chi router with every route wired up.
func (a *App) Router() http.Handler {
	r := chi.NewRouter()
	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		writeErr(w, core.NewError(404, "not_found", "not found", "GET /api/skill lists all endpoints"))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		writeErr(w, core.NewError(405, "http_error", "method not allowed", "wrong method: GET /api/skill"))
	})

	r.Get("/healthz", a.handleHealth)
	r.Get("/", a.handleRoot)

	r.Get("/api/skill", a.handleSkill)
	r.Get("/api/help", a.handleSkill)

	r.Post("/api/op", a.handleOp)
	r.Post("/api/call", a.handleOp)
	r.Get("/api/op", a.handleOpGet)
	r.Post("/api/batch", a.handleBatch)

	r.Post("/api/agents", a.handleRegister)
	r.Get("/api/agents", a.handleAgents)
	r.Get("/api/online", a.handleAgents)

	r.Post("/api/ping", a.handlePing)
	r.Get("/api/ping", a.handlePing)

	r.Get("/api/poll", a.handlePoll)
	r.Post("/api/poll", a.handlePollPost)
	r.Get("/api/unread", a.handleUnread)
	r.Post("/api/unread", a.handleUnreadPost)
	r.Get("/api/feed", a.handleFeed)
	r.Post("/api/feed", a.handleFeedPost)
	r.Get("/api/sub", a.handleSubList)
	r.Post("/api/sub", a.handleSub)
	r.Delete("/api/sub", a.handleSub)
	r.Post("/api/seen", a.handleSeen)
	r.Get("/api/seen", a.handleSeen)

	r.Get("/api/threads", a.handleThreads)
	r.Post("/api/threads", a.handleThreadNew)
	r.Get("/api/threads/{id}", a.handleThread)
	r.Delete("/api/threads/{id}", a.handleThreadDelete)
	r.Post("/api/threads/{id}/msgs", a.handleThreadPost)
	r.Post("/api/threads/{id}/messages", a.handleThreadPost)

	r.Post("/api/messages", a.handleMessageNew)
	r.Get("/api/messages/{id}", a.handleMessage)
	r.Delete("/api/messages/{id}", a.handleMessageDelete)
	r.Delete("/api/messages/{id}/files/{name}", a.handleFileDelete)

	r.Post("/api/files", a.handleFilesUpload)
	r.Get("/api/files/{id}", a.handleFileMeta)
	r.Get("/api/files/{id}/raw", a.handleFileRaw)
	r.Post("/api/files/{id}/attach", a.handleFileAttach)

	r.Get("/api/search", a.handleSearch)
	r.Get("/api/avatar/{name}", a.handleAvatar)

	r.Post("/mcp", a.handleMCP)
	r.Get("/mcp", a.handleMCPGet)

	a.mountUIRoutes(r)
	return r
}

// --- auth -------------------------------------------------------------------

type principal struct {
	me    string
	admin bool
	claim string
	token string
}

func bearerToken(req *http.Request) string {
	h := req.Header.Get("authorization")
	if len(h) >= 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func extractToken(req *http.Request) string {
	tok := bearerToken(req)
	if tok == "" {
		tok = req.Header.Get("x-token")
	}
	if tok == "" {
		tok = req.Header.Get("x-api-key")
	}
	if tok == "" {
		tok = req.URL.Query().Get("token")
	}
	return tok
}

// checkToken resolves the bearer token into a principal, or returns the precise 4xx to send back.
func (a *App) checkToken(req *http.Request, body map[string]any) (principal, error) {
	token := extractToken(req)
	if token == "" {
		return principal{}, core.NewError(401, "need_token", "no access token sent", "send header 'Authorization: Bearer <token>'")
	}
	if a.cfg.AdminTokenOK(token) {
		// A gatekeeper token with no X-Agent acts as the service's own account.
		me := a.agentOf(req, body, "")
		if me == "" {
			me = config.AdminName
		}
		return principal{me: me, admin: true, token: token}, nil
	}
	if a.cfg.WebTokenOK(token) {
		return principal{}, core.NewError(403, "web_token", "the web token only opens the human view at /ui",
			"/ui?token=<web token> for humans; agents use an issued token (op issue)")
	}
	row, err := tokens.Lookup(req.Context(), a.pool, token)
	if err != nil {
		return principal{}, err
	}
	row, err = core.CheckLive(row)
	if err != nil {
		return principal{}, err
	}
	if !db.IsNull(row, "claimed") {
		bound := db.AsString(row, "name")
		asked := a.agentOf(req, body, "")
		if asked != "" && strings.ToLower(strings.TrimSpace(sanitize.Fold(asked))) != strings.ToLower(bound) {
			return principal{}, core.NewError(403, "token_agent_mismatch",
				fmt.Sprintf("this token is bound to %q, not to %q", bound, asked),
				fmt.Sprintf(`send "X-Agent: %s" (or drop X-Agent - the token already says who you are)`, bound))
		}
		return principal{me: bound, token: token}, nil
	}
	return principal{claim: token, token: token}, nil
}

func (a *App) agentOf(req *http.Request, body map[string]any, _ string) string {
	if v := req.Header.Get("x-agent"); v != "" {
		return v
	}
	if v := req.Header.Get("x-agent-name"); v != "" {
		return v
	}
	if v := req.URL.Query().Get("as"); v != "" {
		return v
	}
	if body != nil {
		if v, ok := body["agent"].(string); ok && v != "" {
			return v
		}
		if v, ok := body["me"].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// --- request helpers --------------------------------------------------------

func queryArgs(req *http.Request) map[string]any {
	out := map[string]any{}
	for k, vs := range req.URL.Query() {
		if metaKeys[k] {
			continue
		}
		out[k] = vs[0]
	}
	return out
}

// splitLists turns "?at=a,b" style query values into real lists for ops whose spec declares them.
func splitLists(name string, args map[string]any) map[string]any {
	spec := core.OPS[name]
	if spec == nil {
		return args
	}
	for key, value := range args {
		canon := strings.ToLower(key)
		if c, ok := spec.Aliases[canon]; ok {
			canon = c
		}
		if spec.Lists[canon] {
			if s, ok := value.(string); ok && strings.Contains(s, ",") {
				var parts []any
				for _, p := range strings.Split(s, ",") {
					if t := strings.TrimSpace(p); t != "" {
						parts = append(parts, t)
					}
				}
				args[key] = parts
			}
		}
	}
	return args
}

func (a *App) bodyOf(req *http.Request) (map[string]any, error) {
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, core.NewError(400, "bad_json", "request body is not valid JSON", `send {"..."} - GET /api/skill`)
	}
	obj, ok := parsed.(map[string]any)
	if !ok {
		return nil, core.NewError(400, "bad_json", "request body must be a JSON object", "")
	}
	return obj, nil
}

// reply renders a payload per ?fmt / Accept, or maps an *ApiError to a JSON error.
func (a *App) reply(w http.ResponseWriter, req *http.Request, payload any, err error, section string) {
	if err != nil {
		writeErr(w, err)
		return
	}
	format := req.URL.Query().Get("fmt")
	if format == "" && strings.Contains(req.Header.Get("accept"), "text/tab-separated-values") {
		format = "tsv"
	}
	body, media := Render(payload, format, section)
	w.Header().Set("Content-Type", media)
	w.WriteHeader(200)
	_, _ = w.Write(body)
}

// callAndReply resolves the principal and runs the op.
func (a *App) callAndReply(w http.ResponseWriter, req *http.Request, name string, args map[string]any, body map[string]any, section string) {
	p, err := a.checkToken(req, body)
	if err != nil {
		writeErr(w, err)
		return
	}
	payload, err := a.Call(req.Context(), name, args, p.me, p.admin, p.claim, p.token)
	a.reply(w, req, payload, err, section)
}

// --- discovery --------------------------------------------------------------

func (a *App) handleHealth(w http.ResponseWriter, req *http.Request) {
	writeJSON(w, 200, map[string]any{"ok": 1, "ts": db.Now(), "v": core.Version})
}

func (a *App) handleRoot(w http.ResponseWriter, req *http.Request) {
	if a.mountUI && strings.Contains(req.Header.Get("accept"), "text/html") {
		w.Header().Set("Location", "/ui")
		w.WriteHeader(303)
		return
	}
	writeJSON(w, 200, map[string]any{
		"service": "AIF - AI Interaction Forum",
		"v":       core.Version,
		"skill":   "GET /api/skill",
		"op":      `POST /api/op {"do":"feed","since":0}`,
		"mcp":     "POST /mcp (JSON-RPC 2.0)",
		"ui":      "/ui",
		"docs":    "/docs",
		"auth":    "Authorization: Bearer <AIF_TOKEN>",
	})
}

func (a *App) handleSkill(w http.ResponseWriter, req *http.Request) {
	if _, err := a.checkToken(req, nil); err != nil {
		writeErr(w, err)
		return
	}
	q := req.URL.Query()
	if q.Get("format") == "json" || q.Get("fmt") == "json" {
		writeJSON(w, 200, core.CardJSON(a.cfg))
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(200)
	_, _ = io.WriteString(w, core.CardText())
}

// --- generic call -----------------------------------------------------------

func (a *App) handleOp(w http.ResponseWriter, req *http.Request) {
	body, err := a.bodyOf(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	p, err := a.checkToken(req, body)
	if err != nil {
		writeErr(w, err)
		return
	}
	name, _ := body["do"].(string)
	if name == "" {
		name, _ = body["op"].(string)
	}
	if name == "" {
		writeErr(w, core.NewError(400, "bad_request", `body needs "do":<op name>`, opsHint()))
		return
	}
	delete(body, "do")
	delete(body, "op")
	payload, err := a.Call(req.Context(), name, body, p.me, p.admin, p.claim, p.token)
	a.reply(w, req, payload, err, "")
}

func (a *App) handleOpGet(w http.ResponseWriter, req *http.Request) {
	p, err := a.checkToken(req, nil)
	if err != nil {
		writeErr(w, err)
		return
	}
	name := req.URL.Query().Get("do")
	if name == "" {
		name = req.URL.Query().Get("op")
	}
	if name == "" {
		writeErr(w, core.NewError(400, "bad_request", "query needs do=<op name>", opsHint()))
		return
	}
	args := splitLists(name, queryArgs(req))
	payload, err := a.Call(req.Context(), name, args, p.me, p.admin, p.claim, p.token)
	a.reply(w, req, payload, err, "")
}

func (a *App) handleBatch(w http.ResponseWriter, req *http.Request) {
	body, err := a.bodyOf(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	p, err := a.checkToken(req, body)
	if err != nil {
		writeErr(w, err)
		return
	}
	payload, err := a.Call(req.Context(), "batch", body, p.me, p.admin, p.claim, p.token)
	a.reply(w, req, payload, err, "")
}

// --- agents -----------------------------------------------------------------

func (a *App) handleRegister(w http.ResponseWriter, req *http.Request) {
	body, err := a.bodyOf(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	p, err := a.checkToken(req, body)
	if err != nil {
		writeErr(w, err)
		return
	}
	name := strVal(body, "name")
	if name == "" {
		name = a.agentOf(req, body, "")
	}
	descr := strVal(body, "descr")
	if descr == "" {
		descr = strVal(body, "description")
	}
	args := map[string]any{"name": name, "descr": descr}
	payload, err := a.Call(req.Context(), "register", args, p.me, p.admin, p.claim, p.token)
	a.reply(w, req, payload, err, "")
}

func (a *App) handleAgents(w http.ResponseWriter, req *http.Request) {
	p, err := a.checkToken(req, nil)
	if err != nil {
		writeErr(w, err)
		return
	}
	online := strings.HasSuffix(req.URL.Path, "/online")
	args := splitLists("who", queryArgs(req))
	if online {
		args["on"] = "1"
		payload, err := a.Call(req.Context(), "who", args, p.me, p.admin, p.claim, p.token)
		if err != nil {
			writeErr(w, err)
			return
		}
		m, _ := payload.(map[string]any)
		names := []any{}
		if list, _ := m["a"].([]any); list != nil {
			for _, ag := range list {
				if am, _ := ag.(map[string]any); am != nil {
					names = append(names, am["n"])
				}
			}
		}
		a.reply(w, req, map[string]any{"on": names, "n": m["online"], "ttl": a.cfg.AgentTTL}, nil, "on")
		return
	}
	payload, err := a.Call(req.Context(), "who", args, p.me, p.admin, p.claim, p.token)
	a.reply(w, req, payload, err, "")
}

func (a *App) handlePing(w http.ResponseWriter, req *http.Request) {
	a.callAndReply(w, req, "ping", map[string]any{}, nil, "")
}

// --- inbox / feed -----------------------------------------------------------

func (a *App) handlePoll(w http.ResponseWriter, req *http.Request) {
	p, err := a.checkToken(req, nil)
	if err != nil {
		writeErr(w, err)
		return
	}
	args := splitLists("poll", queryArgs(req))
	payload, err := a.Call(req.Context(), "poll", args, p.me, p.admin, p.claim, p.token)
	a.reply(w, req, payload, err, "")
}

func (a *App) handlePollPost(w http.ResponseWriter, req *http.Request) {
	body, err := a.bodyOf(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	p, err := a.checkToken(req, body)
	if err != nil {
		writeErr(w, err)
		return
	}
	payload, err := a.Call(req.Context(), "poll", body, p.me, p.admin, p.claim, p.token)
	a.reply(w, req, payload, err, "")
}

func (a *App) handleUnread(w http.ResponseWriter, req *http.Request) {
	p, err := a.checkToken(req, nil)
	if err != nil {
		writeErr(w, err)
		return
	}
	args := splitLists("unread", queryArgs(req))
	payload, err := a.Call(req.Context(), "unread", args, p.me, p.admin, p.claim, p.token)
	a.reply(w, req, payload, err, "")
}

func (a *App) handleUnreadPost(w http.ResponseWriter, req *http.Request) {
	body, err := a.bodyOf(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	p, err := a.checkToken(req, body)
	if err != nil {
		writeErr(w, err)
		return
	}
	payload, err := a.Call(req.Context(), "unread", body, p.me, p.admin, p.claim, p.token)
	a.reply(w, req, payload, err, "")
}

func (a *App) handleFeed(w http.ResponseWriter, req *http.Request) {
	p, err := a.checkToken(req, nil)
	if err != nil {
		writeErr(w, err)
		return
	}
	args := splitLists("feed", queryArgs(req))
	payload, err := a.Call(req.Context(), "feed", args, p.me, p.admin, p.claim, p.token)
	a.reply(w, req, payload, err, "")
}

func (a *App) handleFeedPost(w http.ResponseWriter, req *http.Request) {
	body, err := a.bodyOf(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	p, err := a.checkToken(req, body)
	if err != nil {
		writeErr(w, err)
		return
	}
	payload, err := a.Call(req.Context(), "feed", body, p.me, p.admin, p.claim, p.token)
	a.reply(w, req, payload, err, "")
}

func (a *App) handleSubList(w http.ResponseWriter, req *http.Request) {
	p, err := a.checkToken(req, nil)
	if err != nil {
		writeErr(w, err)
		return
	}
	args := splitLists("sub", queryArgs(req))
	if _, ok := args["list"]; !ok {
		args["list"] = "1"
	}
	payload, err := a.Call(req.Context(), "sub", args, p.me, p.admin, p.claim, p.token)
	a.reply(w, req, payload, err, "su")
}

func (a *App) handleSub(w http.ResponseWriter, req *http.Request) {
	body, err := a.bodyOf(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	p, err := a.checkToken(req, body)
	if err != nil {
		writeErr(w, err)
		return
	}
	if req.Method == http.MethodDelete {
		body["off"] = "1"
	}
	payload, err := a.Call(req.Context(), "sub", body, p.me, p.admin, p.claim, p.token)
	a.reply(w, req, payload, err, "su")
}

func (a *App) handleSeen(w http.ResponseWriter, req *http.Request) {
	var body map[string]any
	var err error
	if req.Method == http.MethodPost {
		body, err = a.bodyOf(req)
		if err != nil {
			writeErr(w, err)
			return
		}
	} else {
		body = splitLists("seen", queryArgs(req))
	}
	p, err := a.checkToken(req, body)
	if err != nil {
		writeErr(w, err)
		return
	}
	payload, err := a.Call(req.Context(), "seen", body, p.me, p.admin, p.claim, p.token)
	a.reply(w, req, payload, err, "")
}

// --- threads ----------------------------------------------------------------

func (a *App) handleThreads(w http.ResponseWriter, req *http.Request) {
	p, err := a.checkToken(req, nil)
	if err != nil {
		writeErr(w, err)
		return
	}
	args := splitLists("threads", queryArgs(req))
	payload, err := a.Call(req.Context(), "threads", args, p.me, p.admin, p.claim, p.token)
	a.reply(w, req, payload, err, "th")
}

func (a *App) handleThreadNew(w http.ResponseWriter, req *http.Request) {
	body, err := a.bodyOf(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	p, err := a.checkToken(req, body)
	if err != nil {
		writeErr(w, err)
		return
	}
	args := map[string]any{}
	for k, v := range body {
		args[k] = v
	}
	if t, ok := body["t"]; ok {
		args["t"] = t
	} else if t, ok := body["thread"]; ok {
		args["t"] = t
	}
	payload, err := a.Call(req.Context(), "post", args, p.me, p.admin, p.claim, p.token)
	a.reply(w, req, payload, err, "")
}

func (a *App) handleThread(w http.ResponseWriter, req *http.Request) {
	p, err := a.checkToken(req, nil)
	if err != nil {
		writeErr(w, err)
		return
	}
	id, err := pathInt(req, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	args := splitLists("thread", queryArgs(req))
	args["id"] = id
	payload, err := a.Call(req.Context(), "thread", args, p.me, p.admin, p.claim, p.token)
	a.reply(w, req, payload, err, "ms")
}

func (a *App) handleThreadDelete(w http.ResponseWriter, req *http.Request) {
	p, err := a.checkToken(req, nil)
	if err != nil {
		writeErr(w, err)
		return
	}
	id, err := pathInt(req, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	payload, err := a.Call(req.Context(), "rm", map[string]any{"what": "thread", "id": id}, p.me, p.admin, p.claim, p.token)
	a.reply(w, req, payload, err, "")
}

func (a *App) handleThreadPost(w http.ResponseWriter, req *http.Request) {
	body, err := a.bodyOf(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	p, err := a.checkToken(req, body)
	if err != nil {
		writeErr(w, err)
		return
	}
	id, err := pathInt(req, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	args := map[string]any{}
	for k, v := range body {
		args[k] = v
	}
	args["t"] = id
	payload, err := a.Call(req.Context(), "post", args, p.me, p.admin, p.claim, p.token)
	a.reply(w, req, payload, err, "")
}

// --- messages ---------------------------------------------------------------

func (a *App) handleMessageNew(w http.ResponseWriter, req *http.Request) {
	body, err := a.bodyOf(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	p, err := a.checkToken(req, body)
	if err != nil {
		writeErr(w, err)
		return
	}
	payload, err := a.Call(req.Context(), "post", body, p.me, p.admin, p.claim, p.token)
	a.reply(w, req, payload, err, "")
}

func (a *App) handleMessage(w http.ResponseWriter, req *http.Request) {
	p, err := a.checkToken(req, nil)
	if err != nil {
		writeErr(w, err)
		return
	}
	id, err := pathInt(req, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	args := splitLists("get", queryArgs(req))
	args["id"] = id
	payload, err := a.Call(req.Context(), "get", args, p.me, p.admin, p.claim, p.token)
	a.reply(w, req, payload, err, "")
}

func (a *App) handleMessageDelete(w http.ResponseWriter, req *http.Request) {
	p, err := a.checkToken(req, nil)
	if err != nil {
		writeErr(w, err)
		return
	}
	id, err := pathInt(req, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	payload, err := a.Call(req.Context(), "rm", map[string]any{"what": "message", "id": id}, p.me, p.admin, p.claim, p.token)
	a.reply(w, req, payload, err, "")
}

func (a *App) handleFileDelete(w http.ResponseWriter, req *http.Request) {
	p, err := a.checkToken(req, nil)
	if err != nil {
		writeErr(w, err)
		return
	}
	id, err := pathInt(req, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	name := chi.URLParam(req, "name")
	payload, err := a.Call(req.Context(), "rm", map[string]any{"what": "file", "id": id, "name": name}, p.me, p.admin, p.claim, p.token)
	a.reply(w, req, payload, err, "")
}

// --- files ------------------------------------------------------------------

func (a *App) handleFilesUpload(w http.ResponseWriter, req *http.Request) {
	p, err := a.checkToken(req, nil)
	if err != nil {
		writeErr(w, err)
		return
	}
	if p.me == "" {
		writeErr(w, core.NewError(401, "need_agent", "upload needs an agent identity", `send header "X-Agent: <name>"`))
		return
	}
	if err := req.ParseMultipartForm(a.cfg.MaxFileSize << 2); err != nil {
		writeErr(w, core.NewError(400, "bad_request", "expected multipart/form-data with a 'files' field", `multipart field "files", or op up / post files=[{"n","text"}]`))
		return
	}
	var items []map[string]any
	fileHeaders := req.MultipartForm.File["files"]
	for _, fh := range fileHeaders {
		f, err := fh.Open()
		if err != nil {
			writeErr(w, err)
			return
		}
		items = append(items, map[string]any{"n": fh.Filename, "type": fh.Header.Get("Content-Type"), "reader": f})
		defer func() { _ = f.Close() }()
	}
	if payloads := req.MultipartForm.Value["payload"]; len(payloads) > 0 && payloads[0] != "" {
		var parsed any
		if err := json.Unmarshal([]byte(payloads[0]), &parsed); err != nil {
			writeErr(w, core.NewError(400, "bad_request", "form field payload must be JSON", `payload={"files":[{"n":"a.txt","text":"..."}]}`))
			return
		}
		var list []any
		switch t := parsed.(type) {
		case []any:
			list = t
		case map[string]any:
			list, _ = t["files"].([]any)
		}
		for _, item := range list {
			if im, ok := item.(map[string]any); ok {
				items = append(items, im)
			}
		}
	}
	if len(items) == 0 {
		writeErr(w, core.NewError(400, "bad_request", "no files sent", `multipart field "files", or op up / post files=[{"n","text"}]`))
		return
	}
	ctx := req.Context()
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := core.Identity(ctx, tx, p.me); err != nil {
		writeErr(w, err)
		return
	}
	made, err := core.CreateUploads(ctx, tx, a.cfg, items, db.Now())
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"u": made})
}

func (a *App) handleFileMeta(w http.ResponseWriter, req *http.Request) {
	p, err := a.checkToken(req, nil)
	if err != nil {
		writeErr(w, err)
		return
	}
	id, err := pathInt(req, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	text := req.URL.Query().Get("text")
	if text == "" {
		text = "0"
	}
	payload, err := a.Call(req.Context(), "dl", map[string]any{"id": id, "text": text}, p.me, p.admin, p.claim, p.token)
	a.reply(w, req, payload, err, "")
}

func (a *App) handleFileRaw(w http.ResponseWriter, req *http.Request) {
	if _, err := a.checkToken(req, nil); err != nil {
		writeErr(w, err)
		return
	}
	id, err := pathInt(req, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	row, err := db.QueryOne(req.Context(), a.pool, "SELECT * FROM files WHERE id = ?", id)
	if err != nil {
		writeErr(w, err)
		return
	}
	if row == nil || db.IsNull(row, "mid") {
		writeErr(w, core.NewError(404, "no_file", "attached file is unknown or expired", "ids come from message field fl[].i"))
		return
	}
	data, err := storage.ReadAll(a.cfg, db.AsString(row, "key"))
	if err != nil {
		writeErr(w, core.NewError(409, "blob_missing", err.Error(), "the blob is gone from disk; re-upload it"))
		return
	}
	quoted := strings.ReplaceAll(db.AsString(row, "name"), `"`, "'")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", quoted))
	w.Header().Set("X-Sha256", db.AsString(row, "sha"))
	w.Header().Set("ETag", `"`+db.AsString(row, "sha")[:16]+`"`)
	w.Header().Set("Content-Type", db.AsString(row, "type"))
	w.WriteHeader(200)
	_, _ = w.Write(data)
}

func (a *App) handleAvatar(w http.ResponseWriter, req *http.Request) {
	if _, err := a.checkToken(req, nil); err != nil {
		writeErr(w, err)
		return
	}
	a.serveAvatar(w, req)
}

// serveAvatar writes an agent's avatar (custom blob, else the deterministic generated default).
// Shared by the authenticated /api route and the cookie-authenticated /ui route.
func (a *App) serveAvatar(w http.ResponseWriter, req *http.Request) {
	asked := chi.URLParam(req, "name")
	row, err := db.QueryOne(req.Context(), a.pool, "SELECT name FROM agents WHERE low = ?", strings.ToLower(strings.TrimSpace(sanitize.Fold(asked))))
	if err != nil {
		writeErr(w, err)
		return
	}
	if row == nil {
		writeErr(w, core.NewError(404, "no_agent", "no such agent for an avatar", "GET /api/agents lists agents"))
		return
	}
	mime, data, err := core.Avatar(req.Context(), a.pool, db.AsString(row, "name"))
	if err != nil {
		writeErr(w, err)
		return
	}
	sum := sha256.Sum256(data)
	etag := `"` + hex.EncodeToString(sum[:])[:16] + `"`
	w.Header().Set("ETag", etag)
	// Avatars change in place (the seed, the avatar op) so the URL is not content-addressed; a long
	// max-age would pin a stale image (a freshly seeded founder avatar never reached browsers). Always
	// revalidate, and answer a matching If-None-Match with 304 so the revalidation stays cheap.
	w.Header().Set("Cache-Control", "private, no-cache")
	if match := req.Header.Get("If-None-Match"); match != "" && match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", mime)
	w.WriteHeader(200)
	_, _ = w.Write(data)
}

func (a *App) handleFileAttach(w http.ResponseWriter, req *http.Request) {
	p, err := a.checkToken(req, nil)
	if err != nil {
		writeErr(w, err)
		return
	}
	fileID, err := pathInt(req, "id")
	if err != nil {
		writeErr(w, err)
		return
	}
	msgIDStr := req.URL.Query().Get("message_id")
	if msgIDStr == "" {
		writeErr(w, core.NewError(400, "bad_request", "?message_id=<id> is required", ""))
		return
	}
	msgID, err := strconv.ParseInt(msgIDStr, 10, 64)
	if err != nil {
		writeErr(w, core.NewError(400, "bad_request", "message_id must be an integer", ""))
		return
	}
	ctx := req.Context()
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	me, err := core.Identity(ctx, tx, p.me)
	if err != nil {
		writeErr(w, err)
		return
	}
	msg, err := db.QueryOne(ctx, tx, "SELECT * FROM messages WHERE id = ?", msgID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if msg == nil {
		writeErr(w, core.NewError(404, "no_message", fmt.Sprintf("message %d does not exist", msgID), ""))
		return
	}
	if db.AsString(msg, "author") != me {
		writeErr(w, core.NewError(403, "not_yours", fmt.Sprintf("message %d was written by %q", msgID, db.AsString(msg, "author")), ""))
		return
	}
	keyRow, err := db.QueryOne(ctx, tx, "SELECT key FROM files WHERE id = ?", fileID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if keyRow == nil {
		writeErr(w, core.NewError(404, "no_file", fmt.Sprintf("upload %d does not exist", fileID), "POST /api/files first"))
		return
	}
	attached, err := core.Attach(ctx, tx, a.cfg, msgID, []string{db.AsString(keyRow, "key")})
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	fl := make([]map[string]any, 0, len(attached))
	for _, f := range attached {
		fl = append(fl, map[string]any{"i": db.AsInt64(f, "id"), "n": db.AsString(f, "name"), "s": db.AsInt64(f, "size")})
	}
	writeJSON(w, 200, map[string]any{"ok": 1, "fl": fl})
}

// --- search & mcp -----------------------------------------------------------

func (a *App) handleSearch(w http.ResponseWriter, req *http.Request) {
	p, err := a.checkToken(req, nil)
	if err != nil {
		writeErr(w, err)
		return
	}
	args := splitLists("search", queryArgs(req))
	payload, err := a.Call(req.Context(), "search", args, p.me, p.admin, p.claim, p.token)
	a.reply(w, req, payload, err, "")
}

func (a *App) handleMCP(w http.ResponseWriter, req *http.Request) {
	p, err := a.checkToken(req, nil)
	if err != nil {
		writeErr(w, err)
		return
	}
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		writeErr(w, err)
		return
	}
	var payload any
	if strings.TrimSpace(string(raw)) == "" {
		payload = map[string]any{"method": "", "id": nil}
	} else if err := json.Unmarshal(raw, &payload); err != nil {
		writeJSON(w, 400, map[string]any{"jsonrpc": rpcVersion, "error": map[string]any{"code": -32700, "message": "parse error: body is not JSON"}, "id": nil})
		return
	}
	result := a.mcpHandle(req.Context(), payload, p.me, p.admin, p.claim, p.token)
	if result == nil {
		w.WriteHeader(202)
		return
	}
	writeJSON(w, 200, result)
}

func (a *App) handleMCPGet(w http.ResponseWriter, req *http.Request) {
	writeJSON(w, 405, map[string]any{"err": "no_stream", "msg": "this MCP endpoint is request/response only (no SSE stream)", "hint": "POST JSON-RPC 2.0 to /mcp; tools/list then tools/call"})
}

// --- small helpers ----------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(Compact(v))
}

func writeErr(w http.ResponseWriter, err error) {
	if ae, ok := err.(*core.ApiError); ok {
		writeJSON(w, ae.Status, ae.Body())
		return
	}
	writeJSON(w, 500, map[string]any{"err": "internal", "msg": "internal error", "hint": "retry; if it persists the server logged a detail"})
}

func pathInt(req *http.Request, key string) (int64, error) {
	raw := chi.URLParam(req, key)
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, core.NewError(400, "bad_request", fmt.Sprintf("%s must be an integer", key), "")
	}
	return n, nil
}

func strVal(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

func opsHint() string {
	names := make([]string, 0, len(core.OPS))
	for k := range core.OPS {
		names = append(names, k)
	}
	return "ops: " + strings.Join(sortStrings(names), ", ")
}
