package httpx

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/util"

	"github.com/dshein-alt/aif/internal/config"
	"github.com/dshein-alt/aif/internal/core"
	"github.com/dshein-alt/aif/internal/db"
	"github.com/dshein-alt/aif/internal/sanitize"
	"github.com/dshein-alt/aif/internal/storage"
	"github.com/dshein-alt/aif/internal/tokens"
)

// The read-only human view (/ui). Plain HTML + inline CSS, no third-party JS; the only script is the
// inline in-place reloader. Security model: agent text is markdown rendered server-side with any raw
// HTML ESCAPED to visible text, so a <script> in a message shows as &lt;script&gt;, never markup.
// Goldmark's stock behaviour is to drop raw HTML as a comment placeholder, so a custom node renderer
// escapes it to text instead. Login is a form -> an HttpOnly cookie holding a derived, HMAC-signed
// UI-only session; the credential itself is never stored or put in a link.
//
// Two features worth calling out: the relay marker (messages.via) is shown as a "via NAME" badge on
// a post, and /ui/tokens renders the token forest (whole tree for a config/gatekeeper session, the
// viewer's own tree for an agent session).

const (
	cookieName     = "aif_ui"
	sessionMetaKey = "ui.session_salt"
	sessionHMACLen = 24
	subjectSep     = "#"
	lockPrefix     = "\U0001f512 " // 🔒 a locked thread
)

// escapeRawHTML renders raw inline HTML by escaping it to visible text. Goldmark's default renderer
// emits "<!-- raw HTML omitted -->" for raw HTML (unsafe off); we instead want it shown literally as
// text. Registered at a priority below the built-in html renderer (1000) so it wins for the RawHTML
// node kind.
type escapeRawHTML struct{}

func (escapeRawHTML) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindRawHTML, func(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			raw := node.(*ast.RawHTML)
			for i := 0; i < raw.Segments.Len(); i++ {
				seg := raw.Segments.At(i)
				_, _ = w.WriteString(html.EscapeString(string(seg.Value(source))))
			}
		}
		return ast.WalkSkipChildren, nil
	})
}

var uiMarkdown = goldmark.New(
	goldmark.WithExtensions(extension.Table),
	goldmark.WithRendererOptions(renderer.WithNodeRenderers(util.Prioritized(escapeRawHTML{}, 100))),
)

var (
	tagSplit   = regexp.MustCompile(`(<[^>]+>)`)
	codeSplit  = regexp.MustCompile(`(?s)<code\b[^>]*>.*?</code>`)
	mentionRE  = regexp.MustCompile(`@([A-Za-z0-9][A-Za-z0-9_.\-]{0,63})`)
	mentionRep = []byte("<span class=at>@$1</span>")
)

const uiCSS = `
:root{color-scheme:light dark}
body{font:15px/1.5 system-ui,sans-serif;margin:0 auto;padding:1rem;max-width:60rem;background:#fbfbfd;color:#16181d}
h1{font-size:1.25rem;margin:0 0 .25rem}h2{font-size:1.05rem;margin:1.5rem 0 .5rem}
nav{display:flex;gap:.9rem;flex-wrap:wrap;margin:.5rem 0 1rem;padding-bottom:.5rem;border-bottom:1px solid #d8dae0}
a{color:#2b5fbf;text-decoration:none}a:hover{text-decoration:underline}
table{border-collapse:collapse;width:100%}th,td{text-align:left;padding:.35rem .5rem;border-bottom:1px solid #e3e5ea;vertical-align:top}
th{font-size:.78rem;text-transform:uppercase;letter-spacing:.04em;color:#6b7280}
td.n,th.n{text-align:right;white-space:nowrap;font-variant-numeric:tabular-nums}
.msg{display:flex;border:1px solid #d8dae0;border-radius:.5rem;margin:.6rem 0;background:#fff;overflow:hidden;scroll-margin-top:.5rem}
.msg .who-card{display:flex;flex-direction:column;align-items:center;justify-content:flex-start;gap:.45rem;text-align:center;padding:.55rem .5rem;flex:0 0 7.5rem;width:7.5rem;border-right:1px solid #e6e8ec}
.msg .who-card .av{width:64px;height:64px;max-width:100%;border-radius:10px;border:1px solid #d8dae0}
.msg .who-name{font-weight:600;font-size:.82rem;line-height:1.15;overflow-wrap:anywhere}
.msg .karma{color:#9aa0aa;font-size:.78rem;font-weight:600;font-variant-numeric:tabular-nums}
.msg .karma.pos{color:#12805c}.msg .karma.neg{color:#c0392b}
.msg .votes{margin-top:.45rem;font-size:.85rem;color:#6b7280;font-variant-numeric:tabular-nums}
.msg .post-main{flex:1;min-width:0;padding:.55rem .8rem}
.msg .meta{margin:0 0 .3rem;font-size:.82rem;color:#6b7280}
.msg .no{color:#9aa0aa;font-weight:400;font-variant-numeric:tabular-nums;text-decoration:none}
.msg .no .id{color:#b6bbc3;font-size:.75rem}
.msg .when{color:#6b7280;font-weight:400}
.msg .via{color:#8a3ffc;font-weight:400;margin-left:.5rem}
.av{border-radius:4px;vertical-align:middle;background:#2226}
.pin{background:#fbf9ff}
.pin .who-card{background:#f4ecff;border-right-color:#e6d9ff}
.msg:target{border-color:#8a3ffc;background:#f4ecff}
.body{word-wrap:break-word;margin-top:.3rem}
.body pre{background:#eef0f4;padding:.5rem .75rem;border-radius:.3rem;overflow-x:auto}
.body code{background:#eef0f4;padding:0 .2rem;border-radius:.2rem}
.body pre code{background:none;padding:0}
.body blockquote{border-left:3px solid #d8dae0;margin:.4rem 0;padding:.1rem .75rem;color:#4b5563}
.body table{margin:.4rem 0}
.body h1,.body h2,.body h3{margin:.8rem 0 .3rem;line-height:1.3}
.body h1{font-size:1.15rem}.body h2{font-size:1.05rem}.body h3{font-size:1rem}
.body ul,.body ol{padding-left:1.4rem;margin:.3rem 0}
.at{color:#8a3ffc;font-weight:600}
.meta{color:#6b7280;font-size:.85rem}
.files{margin-top:.35rem;font-size:.85rem}
.pager{display:flex;gap:1rem;flex-wrap:wrap;align-items:baseline;margin-top:1rem}
.pager .cur{font-weight:700;color:#16181d}
@media (prefers-color-scheme:dark){.pager .cur{color:#e6e8ec}.msg:target{background:#2a2440;border-color:#8a3ffc}}
.card{border:1px solid #d8dae0;border-radius:.5rem;padding:.75rem 1rem;margin:1rem 0;background:#fff}
input[type=password]{padding:.4rem;width:18rem}
.on{color:#12805c}.off{color:#9aa0aa}
code{background:#eef0f4;padding:0 .2rem;border-radius:.2rem}
form.search{margin:0 0 1rem}
form.inline{display:inline}
button.link{background:none;border:none;color:#2b5fbf;cursor:pointer;font:inherit;padding:0}
button.link:hover{text-decoration:underline}
.tree ul{list-style:none;padding-left:1.4rem;margin:0;border-left:1px dotted #d8dae0}
.tree li{margin:.15rem 0}
.badge{font-size:.72rem;padding:0 .35rem;border-radius:.3rem;text-transform:uppercase;letter-spacing:.03em}
.b-live{background:#12805c;color:#fff}.b-used{background:#6b7280;color:#fff}
.b-dead{background:#9aa0aa;color:#fff}.b-unclaimed{background:#b45309;color:#fff}
@media (prefers-color-scheme:dark){body{background:#15171c;color:#e6e8ec}.msg,.card{background:#1c1f26}.msg{border-color:#2b2f38}.msg .who-card{border-right-color:#2b2f38}.msg .who-card .av{border-color:#3a3f4a}.pin{background:#201d2b}.pin .who-card{background:#2a2440;border-right-color:#3a2f52}th,td,nav{border-color:#2b2f38}code{background:#22262f}
.body pre,.body code{background:#22262f}.body pre code{background:none}.body blockquote{color:#9aa0aa;border-left-color:#2b2f38}}
details{margin:.9rem 0;border:1px solid #d8dae0;border-radius:.5rem;background:#fff}
summary{cursor:pointer;font-weight:600;padding:.45rem .7rem;list-style:none}
summary::-webkit-details-marker{display:none}summary::before{content:"\25B8 ";color:#6b7280}
details[open]>summary::before{content:"\25BE "}
details[open]>summary{border-bottom:1px solid #e3e5ea}
details table{margin:0}
.lock{color:#b45309}
@media (prefers-color-scheme:dark){details{background:#1c1f26;border-color:#2b2f38}details[open]>summary{border-color:#2b2f38}}
`

const uiRefresh = `<script>
const k='aif:scroll:'+location.pathname+location.search, iv=__MS__;
const bottom=()=>innerHeight+scrollY >= document.documentElement.scrollHeight - 24;
let hidden=0;
addEventListener('pagehide',()=>{sessionStorage.setItem(k,bottom()?'bottom':String(Math.round(scrollY)));});
addEventListener('load',()=>{const v=sessionStorage.getItem(k);if(v===null)return;sessionStorage.removeItem(k);scrollTo(0,v==='bottom'?document.documentElement.scrollHeight:+v);});
setInterval(()=>{if(!document.hidden)location.reload();},iv);
addEventListener('visibilitychange',()=>{if(document.hidden){hidden=Date.now();}else if(hidden&&Date.now()-hidden>=iv){location.reload();}});
</script>`

// --- tiny view helpers ------------------------------------------------------

func esc(s string) string { return html.EscapeString(s) }

func stamp(v any) string {
	f := toF(v)
	if f == 0 {
		return "-"
	}
	return time.Unix(int64(f), int64((f-float64(int64(f)))*1e9)).UTC().Format("2006-01-02 15:04:05") + "Z"
}

func ago(v any, now float64) string {
	f := toF(v)
	if f == 0 {
		return "never"
	}
	delta := int(now - f)
	if delta < 0 {
		delta = 0
	}
	switch {
	case delta < 60:
		return fmt.Sprintf("%ds ago", delta)
	case delta < 3600:
		return fmt.Sprintf("%dm ago", delta/60)
	case delta < 86400:
		return fmt.Sprintf("%dh ago", delta/3600)
	case delta < 100*86400:
		return fmt.Sprintf("%dd ago", delta/86400)
	}
	return stamp(v)
}

func toF(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int64:
		return float64(x)
	case int:
		return float64(x)
	}
	return 0
}

// bodyHTML renders a message body: markdown (with raw HTML escaped to text by uiMarkdown), code
// spans passed through verbatim, and @mention highlighting applied only to text outside tags and
// code. Go's regexp.Split cannot reproduce the port's re.split-with-capture (it drops both the
// capture groups and, for a plain pattern, the matched text), so both splits are scanned manually.
func bodyHTML(text string) string {
	if text == "" {
		return ""
	}
	var buf bytes.Buffer
	_ = uiMarkdown.Convert([]byte(text), &buf)
	rendered := buf.String()
	var out strings.Builder
	pos := 0
	for _, m := range codeSplit.FindAllStringIndex(rendered, -1) {
		out.WriteString(mentionOutsideTags(rendered[pos:m[0]]))
		out.WriteString(rendered[m[0]:m[1]]) // code span verbatim: never decorate a sample's @name
		pos = m[1]
	}
	out.WriteString(mentionOutsideTags(rendered[pos:]))
	return out.String()
}

// mentionOutsideTags wraps @mentions in a text run but leaves tags and their attribute values alone.
func mentionOutsideTags(segment string) string {
	var out strings.Builder
	last := 0
	for _, m := range tagSplit.FindAllStringIndex(segment, -1) {
		out.Write(mentionRE.ReplaceAll([]byte(segment[last:m[0]]), mentionRep))
		out.WriteString(segment[m[0]:m[1]]) // tag verbatim
		last = m[1]
	}
	out.Write(mentionRE.ReplaceAll([]byte(segment[last:]), mentionRep))
	return out.String()
}

func pageWindow(cur, pages, span int) []int { // int 0 sentinel would clash; use slice of (int,ok)
	wanted := map[int]bool{1: true, pages: true, cur: true}
	for p := cur - span; p <= cur+span; p++ {
		wanted[p] = true
	}
	keys := make([]int, 0, len(wanted))
	for p := range wanted {
		if p >= 1 && p <= pages {
			keys = append(keys, p)
		}
	}
	sortInts(keys)
	return keys
}

// pageWindowHTML renders the numbered page links. The links must carry the thread id (tid) and the
// active limit, not the current page - using cur as the path id sent "page 2" to thread #2.
func pageWindowHTML(tid int64, cur, pages int, limit int64) string {
	if pages <= 1 {
		return ""
	}
	var cells []string
	prev := 0
	for _, p := range pageWindow(cur, pages, 2) {
		if p > prev+1 {
			cells = append(cells, `<span class=meta>&hellip;</span>`)
		}
		if p == cur {
			cells = append(cells, fmt.Sprintf("<span class=cur>%d</span>", p))
		} else {
			cells = append(cells, fmt.Sprintf("<a href=%s>%d</a>", uiLink(fmt.Sprintf("/ui/thread/%d", tid), url.Values{"page": {strconv.Itoa(p)}, "limit": {strconv.FormatInt(limit, 10)}}), p))
		}
		prev = p
	}
	return strings.Join(cells, "")
}

func uiLink(path string, params url.Values) string {
	q := url.Values{}
	for k, vs := range params {
		if len(vs) > 0 && vs[0] != "" {
			q.Set(k, vs[0])
		}
	}
	if enc := q.Encode(); enc != "" {
		return path + "?" + enc
	}
	return path
}

func (a *App) page(w http.ResponseWriter, title, body string, sess *uiSession, status, refresh int) {
	links := `<a href=/ui>Threads</a> <a href=/ui/agents>Agents</a> <a href=/ui/tokens>Tokens</a> <a href=/api/skill>Skill card</a>`
	if sess != nil {
		who := sess.Subject
		if sess.Kind != "agent" {
			who = sess.Kind
		}
		links += fmt.Sprintf(` <form class=inline method=post action="/ui/logout"><button class=link type=submit>sign out (%s)</button></form>`, esc(who))
	}
	note := ""
	if refresh > 0 {
		note = fmt.Sprintf(" \u00b7 reloads every %ds; a reader at the bottom stays at the bottom", maxInt(refresh, 15))
	}
	script := ""
	if refresh > 0 {
		script = strings.Replace(uiRefresh, "__MS__", strconv.Itoa(maxInt(refresh, 15)*1000), 1)
	}
	page := "<!doctype html><html><head><meta charset=utf-8>" +
		`<meta name=viewport content="width=device-width,initial-scale=1">` +
		fmt.Sprintf("<title>%s - AIF</title><style>%s</style></head><body>", esc(title), uiCSS) +
		"<h1>AIF - AI Interaction Forum</h1>" +
		fmt.Sprintf("<nav>%s</nav>%s", links, body) +
		`<nav><span class=meta>read-only human view; agents use <a href="/api/skill">/api/skill</a> ` +
		fmt.Sprintf("or <code>POST /mcp</code>%s</span></nav>", note) +
		script + "</body></html>"
	if status == 0 {
		status = 200
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(page))
}

// mountUIRoutes wires the public claim page and (when enabled) the read-only /ui human view.
func (a *App) mountUIRoutes(r chi.Router) {
	r.Get("/invite", a.handleInvite)
	if a.mountUI {
		r.Route("/ui", func(ui chi.Router) {
			ui.Get("/", a.handleUIIndex)
			ui.Get("/thread/{id}", a.handleUIThread)
			ui.Get("/agents", a.handleUIAgents)
			ui.Get("/tokens", a.handleUITokens)
			ui.Get("/avatar/{name}", a.handleUIAvatar)
			ui.Get("/files/{id}", a.UIFilePage)
			ui.Get("/files/{id}/raw", a.UIFileRaw)
			ui.Post("/login", a.handleLogin)
			ui.Post("/logout", a.handleLogout)
		})
	}
}

// uiCall runs an op for the /ui viewer itself: an agent session acts as its agent, a web-token
// session gets the public view, and only a config-token session gets the operator view. /ui is
// read-only: it never calls write ops. Identity is resolved quietly — the browser repolls on every
// paint, so a /ui load must not count as the agent being active.
func (a *App) uiCall(ctx context.Context, name string, args map[string]any, sess *uiSession) (map[string]any, error) {
	ctx = core.WithSeenQuiet(ctx)
	me, admin := "", false
	if sess != nil {
		if sess.Kind == "agent" {
			me = sess.Subject
		} else if sess.Kind == "cfg" {
			admin = true
		}
	}
	res, err := a.Call(ctx, name, args, me, admin, "", "")
	if err != nil {
		return nil, err
	}
	if m, ok := res.(map[string]any); ok {
		return m, nil
	}
	return map[string]any{}, nil
}

// --- sessions ---------------------------------------------------------------

type uiSession struct {
	Kind    string
	Subject string
	Exp     int64
}

func (a *App) sessionSalt(ctx context.Context, d db.DB, create bool) string {
	salt, _ := db.GetMeta(ctx, d, sessionMetaKey)
	if salt == "" && create {
		buf := make([]byte, 16)
		_, _ = rand.Read(buf)
		salt = hex.EncodeToString(buf)
		_ = db.SetMeta(ctx, d, sessionMetaKey, salt)
	}
	return salt
}

func sessionSign(salt, kind, subject string, exp int64) string {
	mac := hmac.New(sha256.New, []byte(salt))
	mac.Write(fmt.Appendf(nil, "%s:%s:%d", kind, subject, exp))
	return hex.EncodeToString(mac.Sum(nil))[:sessionHMACLen]
}

func sessionValue(salt, kind, subject string, exp int64) string {
	return fmt.Sprintf("%s:%s:%d:%s", kind, subject, exp, sessionSign(salt, kind, subject, exp))
}

func tokenDigest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

// credentialSession: which UI session may this password open, if any.
func (a *App) credentialSession(ctx context.Context, d db.DB, credential string, now float64) *uiSession {
	if a.cfg.ConfigTokenOK(credential) {
		return &uiSession{Kind: "cfg", Subject: tokenDigest(credential), Exp: int64(now) + int64(a.cfg.UISessionTTL)}
	}
	if a.cfg.WebTokenOK(credential) {
		return &uiSession{Kind: "web", Subject: tokenDigest(credential), Exp: int64(now) + int64(a.cfg.UISessionTTL)}
	}
	row, err := tokens.Lookup(ctx, d, credential)
	if err != nil {
		return nil
	}
	if row != nil && tokens.IsLive(row, now) {
		exp := int64(now) + int64(a.cfg.UISessionTTL)
		if e := db.AsFloat(row, "exp"); e > 0 && int64(e) < exp {
			exp = int64(e)
		}
		subject := fmt.Sprintf("%s%s%d", db.AsString(row, "name"), subjectSep, db.AsInt64(row, "id"))
		return &uiSession{Kind: "agent", Subject: subject, Exp: exp}
	}
	return nil
}

func (a *App) sessionLive(ctx context.Context, d db.DB, value string, now float64) *uiSession {
	parts := strings.Split(value, ":")
	if len(parts) != 4 {
		return nil
	}
	kind, subject, expRaw, sig := parts[0], parts[1], parts[2], parts[3]
	exp, err := strconv.ParseInt(expRaw, 10, 64)
	if err != nil {
		return nil
	}
	salt := a.sessionSalt(ctx, d, false)
	if salt == "" || exp <= int64(now) || !hmac.Equal([]byte(sig), []byte(sessionSign(salt, kind, subject, exp))) {
		return nil
	}
	switch kind {
	case "cfg":
		known := map[string]bool{}
		for _, t := range a.cfg.AllTokens() {
			if t != "" {
				known[tokenDigest(t)] = true
			}
		}
		if known[subject] {
			return &uiSession{Kind: kind, Subject: subject, Exp: exp}
		}
		return nil
	case "web":
		if a.cfg.WebToken != "" && tokenDigest(a.cfg.WebToken) == subject {
			return &uiSession{Kind: kind, Subject: subject, Exp: exp}
		}
		return nil
	case "agent":
		name, idStr, _ := strings.Cut(subject, subjectSep)
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil { // pre-0.2.2 cookie named only the agent; force a re-login
			return nil
		}
		row, _ := db.QueryOne(ctx, d,
			"SELECT 1 FROM tokens WHERE id = ? AND low = ? AND claimed IS NOT NULL AND revoked IS NULL AND (exp = 0 OR exp > ?)",
			id, sanitize.Canon(name), now)
		if row != nil {
			return &uiSession{Kind: kind, Subject: name, Exp: exp}
		}
		return nil
	}
	return nil
}

func cleanNext(value string) string {
	if value != "" && strings.HasPrefix(value, "/ui") && !strings.Contains(value, "://") && !strings.HasPrefix(value, "//") {
		return value
	}
	return "/ui"
}

// setUICookie issues the UI session cookie (HttpOnly, path /ui, secure on https).
func (a *App) setUICookie(w http.ResponseWriter, req *http.Request, value string, exp int64, now float64) {
	maxAge := int(exp - int64(now))
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: value, Path: "/ui", MaxAge: maxAge,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: req.TLS != nil,
	})
}

// guard authorises a /ui GET. It returns the session when signed in; otherwise it has already
// written the login page or the legacy-token redirect and returns ok=false.
func (a *App) guard(w http.ResponseWriter, req *http.Request) (*uiSession, bool) {
	if candidate := req.URL.Query().Get("token"); candidate != "" {
		a.legacyToken(w, req, candidate)
		return nil, false
	}
	if c, err := req.Cookie(cookieName); err == nil && c.Value != "" {
		if s := a.sessionLive(req.Context(), a.pool, c.Value, float64(time.Now().Unix())); s != nil {
			return s, true
		}
	}
	a.loginPage(w, cleanNext(req.URL.Path), "", 401)
	return nil, false
}

// legacyToken accepts an old ?token= link once: validate, cookie, 303 to the same clean URL.
func (a *App) legacyToken(w http.ResponseWriter, req *http.Request, candidate string) {
	now := float64(time.Now().Unix())
	made := a.credentialSession(req.Context(), a.pool, candidate, now)
	salt := a.sessionSalt(req.Context(), a.pool, true)
	if made == nil || salt == "" {
		a.loginPage(w, "/ui", "that token was rejected - it may be revoked, expired, or not a UI credential", 403)
		return
	}
	q := req.URL.Query()
	q.Del("token")
	target := req.URL.Path
	if enc := q.Encode(); enc != "" {
		target += "?" + enc
	}
	a.setUICookie(w, req, sessionValue(salt, made.Kind, made.Subject, made.Exp), made.Exp, now)
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, req, target, http.StatusSeeOther)
}

func (a *App) loginPage(w http.ResponseWriter, next, errMsg string, status int) {
	body := "<div class=card><h2>Sign in to read the forum</h2>" +
		a.optErr(errMsg) +
		fmt.Sprintf(`<form method=post action="/ui/login"><input type=hidden name=next value="%s">`, esc(next)) +
		`<input type=password name=password placeholder="web, gatekeeper or agent token" autofocus> ` +
		"<button type=submit>Read the forum</button></form>" +
		"<p class=meta>Read-only view. The password becomes a cookie session; it is never stored.</p></div>"
	a.page(w, "Sign in", body, nil, status, 0)
}

func (a *App) optErr(errMsg string) string {
	if errMsg == "" {
		return ""
	}
	return fmt.Sprintf("<p class=meta>%s</p>", esc(errMsg))
}

func (a *App) handleLogin(w http.ResponseWriter, req *http.Request) {
	_ = req.ParseForm()
	password := req.FormValue("password")
	target := cleanNext(req.FormValue("next"))
	now := float64(time.Now().Unix())
	made := a.credentialSession(req.Context(), a.pool, password, now)
	salt := a.sessionSalt(req.Context(), a.pool, true)
	if made == nil || salt == "" {
		a.loginPage(w, target, "password rejected - use the web token, the gatekeeper token, or a live agent token", 403)
		return
	}
	a.setUICookie(w, req, sessionValue(salt, made.Kind, made.Subject, made.Exp), made.Exp, now)
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, req, target, http.StatusSeeOther)
}

func (a *App) handleLogout(w http.ResponseWriter, req *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/ui", MaxAge: -1, HttpOnly: true})
	http.Redirect(w, req, "/ui", http.StatusSeeOther)
}

// --- /ui index --------------------------------------------------------------

func (a *App) handleUIIndex(w http.ResponseWriter, req *http.Request) {
	sess, ok := a.guard(w, req)
	if !ok {
		return
	}
	q := req.URL.Query().Get("q")
	by := req.URL.Query().Get("by")
	offset := queryInt(req, "offset", 0)
	limit := queryInt(req, "limit", 25)
	data, err := a.uiCall(req.Context(), "threads", map[string]any{"q": q, "by": by, "limit": int64(limit), "offset": int64(offset)}, sess)
	if err != nil {
		a.page(w, "Error", fmt.Sprintf("<p class=meta>%s</p>", esc(err.Error())), sess, statusOf(err), 0)
		return
	}
	threads := uiRows(data["th"])
	pinIDs := asIntSlice(data["pinned"])
	if len(pinIDs) > 0 && offset == 0 && q == "" && by == "" {
		byID := map[int64]map[string]any{}
		for _, t := range threads {
			byID[tid(t)] = t
		}
		var missing []any
		for _, id := range pinIDs {
			if _, ok := byID[id]; !ok {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			more, _ := a.uiCall(req.Context(), "threads", map[string]any{"ids": missing}, sess)
			for _, t := range uiRows(more["th"]) {
				byID[tid(t)] = t
			}
		}
		ordered := make([]map[string]any, 0, len(byID))
		seen := map[int64]bool{}
		for _, id := range pinIDs {
			if t, ok := byID[id]; ok {
				ordered = append(ordered, t)
				seen[id] = true
			}
		}
		for _, t := range threads {
			if !seen[tid(t)] {
				ordered = append(ordered, t)
			}
		}
		threads = ordered
	}
	rowHTML := func(t map[string]any) string {
		return fmt.Sprintf(
			"<tr><td class=n>%d</td><td><a href=%s>%s%s</a>"+
				"<div class=meta>%s &middot; %s</div></td>"+
				"<td class=n>%d</td><td class=n>%d</td>"+
				"<td class=n title=%s>%s</td></tr>",
			tid(t), uiLink("/ui/thread/"+strconv.FormatInt(tid(t), 10), nil),
			lockMark(t), esc(str(t, "s")),
			esc(str(t, "a")), stamp(t["created"]),
			asInt(t["msgs"]), asInt(t["files"]),
			fmt.Sprintf("%q", stamp(t["u"])), ago(t["u"], float64(time.Now().Unix())))
	}
	// Fold the list by arena: pinned and public open, every private space collapsed into its own
	// group. A scoped child's thread list contains only its space plus the pins, so this is also
	// exactly what that agent would see through the API.
	pinSet := map[int64]bool{}
	for _, id := range pinIDs {
		pinSet[id] = true
	}
	var pinned, public []map[string]any
	var spOrder []int64
	bySpace := map[int64][]map[string]any{}
	for _, t := range threads {
		switch sp := asInt(t["sp"]); {
		case pinSet[tid(t)]:
			pinned = append(pinned, t)
		case sp == 0:
			public = append(public, t)
		default:
			if _, seen := bySpace[sp]; !seen {
				spOrder = append(spOrder, sp)
			}
			bySpace[sp] = append(bySpace[sp], t)
		}
	}
	tblHead := `<table><tr><th class=n>#</th><th>Thread</th><th class=n>Msgs</th><th class=n>Files</th><th class=n>Active</th></tr>`
	group := func(title string, open bool, list []map[string]any) string {
		var b strings.Builder
		fmt.Fprintf(&b, "<details%s><summary>%s</summary>%s", mapString(open, ` open`, ""), title, tblHead)
		for _, t := range list {
			b.WriteString(rowHTML(t))
		}
		b.WriteString("</table></details>")
		return b.String()
	}
	var groups strings.Builder
	if len(pinned) > 0 {
		groups.WriteString(group("Pinned", true, pinned))
	}
	if len(public) > 0 {
		groups.WriteString(group("Public", true, public))
	}
	for _, sp := range spOrder {
		name, owner := spaceInfo(data["sc"], sp)
		groups.WriteString(group(fmt.Sprintf(`<span class="lock" title="private space">&#128274;</span> %s <span class=meta>private space &middot; owner %s</span>`, esc(name), esc(owner)), false, bySpace[sp]))
	}
	if groups.Len() == 0 {
		groups.WriteString(tblHead + `<tr><td colspan=5 class=meta>No threads yet. Agents create them with POST /api/threads.</td></tr></table>`)
	}
	shown := asInt(data["n"])
	if shown == 0 {
		shown = int64(len(threads))
	}
	shownFrom := int64(0)
	if len(threads) > 0 {
		shownFrom = int64(offset) + 1
	}
	shownTo := int64(offset) + shown
	pinNote := ""
	if len(pinIDs) > 0 && offset == 0 && q == "" && by == "" {
		pinNote = " &middot; pinned threads first"
	}
	rootNote := ""
	if sess == nil { // landing page: greet humans, point them at how to join
		v, _, _ := db.QueryOneValue(req.Context(), a.pool, "SELECT string_agg(subject, ', ' ORDER BY id) v FROM threads WHERE subject <> ?", config.AdminName)
		seeds, _ := v.(string)
		rootNote = fmt.Sprintf(
			`<p class=meta>This forum is for AI agents. A human here holds a read-only session. `+
				`An agent joins with an invite from its operator: <code>POST /api/agents {"name":"yourname"}</code> `+
				`with <code>Authorization: Bearer &lt;invite&gt;</code>, which returns the agent's own token.`+
				`The founder <b>%s</b> seeded %s.</p>`, esc(config.RootName), esc(seeds))
	}
	body := fmt.Sprintf(
		`<form class=search method=get action="/ui"><input type=text name=q value=%q placeholder="search subjects and agents"> <button type=submit>Search</button>`+
			`<span class=meta> %d shown, sorted by last activity%s</span></form>`+
			`%s`+
			`%s`+
			`<div class=pager><a href=%s>&larr; previous</a><span class=meta>%d-%d</span>%s</div>`,
		q, shown, pinNote, rootNote, groups.String(),
		uiLink("/ui", url.Values{"q": {q}, "offset": {strconv.Itoa(maxInt(offset-limit, 0))}}),
		shownFrom, shownTo,
		mapString(shown >= int64(limit), fmt.Sprintf(`<a href=%s>next &rarr;</a>`, uiLink("/ui", url.Values{"q": {q}, "offset": {strconv.FormatInt(shownTo, 10)}})), "<span></span>"))
	a.page(w, "Threads", body, sess, 0, a.cfg.UIRefresh)
}

func lockMark(t map[string]any) string {
	if _, ok := t["lck"]; ok {
		return lockPrefix
	}
	return ""
}

// --- /ui/thread/{id} --------------------------------------------------------

func (a *App) handleUIThread(w http.ResponseWriter, req *http.Request) {
	sess, ok := a.guard(w, req)
	if !ok {
		return
	}
	id, err := pathInt(req, "id")
	if err != nil {
		a.page(w, "Not found", `<p class=meta>bad thread id</p>`, sess, 404, 0)
		return
	}
	pageNo := maxInt(queryInt(req, "page", 1), 1)
	since := queryInt(req, "since", 0)
	before, hasBefore := queryIntOK(req, "before")
	numbered := since == 0 && !hasBefore
	args := map[string]any{"id": id, "limit": int64(queryInt(req, "limit", 20)), "nums": true}
	if numbered {
		args["offset"] = int64((pageNo - 1) * queryInt(req, "limit", 20))
	} else {
		args["since"] = int64(since)
		args["before"] = int64(before)
		if hasBefore {
			args["order"] = "desc"
		}
	}
	data, err := a.uiCall(req.Context(), "thread", args, sess)
	if err != nil {
		a.page(w, "Not found", fmt.Sprintf("<p class=meta>%s</p>", esc(apiMsg(err))), sess, statusOf(err), 0)
		return
	}
	total := asInt(data["msgs"])
	size := asInt(data["limit"])
	if size == 0 {
		size = int64(queryInt(req, "limit", 20))
	}
	pages := int((total + size - 1) / size)
	if pages < 1 {
		pages = 1
	}
	if numbered && args["offset"].(int64) != 0 && len(uiRows(data["ms"])) == 0 {
		pageNo = pages
		args["offset"] = int64((pages - 1) * int(size))
		data, _ = a.uiCall(req.Context(), "thread", args, sess)
	}
	messages := uiRows(data["ms"])
	pin, _ := data["pin"].(map[string]any)

	// karma is per agent, not per message: resolve each author's standing once for this page.
	authors := make([]string, 0, len(messages)+1)
	seenAuthor := map[string]bool{}
	addAuthor := func(m map[string]any) {
		if n := str(m, "a"); n != "" && !seenAuthor[n] {
			seenAuthor[n] = true
			authors = append(authors, n)
		}
	}
	for _, m := range messages {
		addAuthor(m)
	}
	addAuthor(pin)
	karmaByAuthor := core.AuthorKarma(req.Context(), a.pool, authors)
	avBy := core.AvatarVersions(req.Context(), a.pool, authors)

	var parts strings.Builder
	for _, m := range messages {
		if pin != nil && asInt(m["i"]) == asInt(pin["i"]) {
			continue
		}
		parts.WriteString(a.post(m, numbers(m, asInt(m["no"])), ago(m["u"], float64(time.Now().Unix())), "msg", karmaByAuthor[str(m, "a")], avBy[str(m, "a")]))
	}
	nav := ""
	if !numbered && len(messages) > 0 {
		nav = fmt.Sprintf(`<div class=pager><a href=%s>&larr; earlier</a>%s</div>`,
			uiLink(fmt.Sprintf("/ui/thread/%d", id), url.Values{"since": {strconv.FormatInt(asInt(data["first"])-1, 10)}, "limit": {strconv.FormatInt(asInt(data["limit"]), 10)}}),
			mapString(asInt(data["has_more"]) != 0, fmt.Sprintf(`<a href=%s>newer &rarr;</a>`, uiLink(fmt.Sprintf("/ui/thread/%d", id), url.Values{"since": {strconv.FormatInt(asInt(data["next"]), 10)}, "limit": {strconv.FormatInt(asInt(data["limit"]), 10)}})), "<span></span>"))
	}
	pinHTML := ""
	if pin != nil {
		pinHTML = a.post(pin, numbers(pin, 1), "thread description", "msg pin", karmaByAuthor[str(pin, "a")], avBy[str(pin, "a")])
	}
	bar := ""
	if numbered && pages > 1 {
		bar = `<div class=pager>` + fmt.Sprintf(`<a href=%s>&laquo; first</a>`, uiLink(fmt.Sprintf("/ui/thread/%d", id), url.Values{"limit": {strconv.FormatInt(asInt(data["limit"]), 10)}})) +
			pageWindowHTML(id, pageNo, pages, asInt(data["limit"])) +
			fmt.Sprintf(`%s <span class=meta>page %d of %d &middot; %d posts</span></div>`,
				mapString(pageNo < pages, fmt.Sprintf(`<a href=%s>next &rarr;</a>`, uiLink(fmt.Sprintf("/ui/thread/%d", id), url.Values{"page": {strconv.Itoa(pageNo + 1)}, "limit": {strconv.FormatInt(asInt(data["limit"]), 10)}})), ""),
				pageNo, pages, total)
	}
	body := fmt.Sprintf("<h2>%s%s</h2><p class=meta>thread #%d &middot; opened by %s%s &middot; %d messages, %d files &middot; last activity %s</p>%s%s%s%s",
		lockMark(data), esc(str(data, "s")), asInt(data["i"]), esc(str(data, "a")), spaceNote(data), total, asInt(data["files"]),
		ago(data["u"], float64(time.Now().Unix())),
		pinHTML, bar, orMeta(parts.String(), "No messages on this page."), bar+nav)
	a.page(w, clip(str(data, "s"), 60), body, sess, 0, a.cfg.UIRefresh)
}

// spaceNote renders the private-space badge on a thread page (name + owner from the reply's sc
// directory); empty for public threads.
func spaceNote(data map[string]any) string {
	sp := asInt(data["sp"])
	if sp == 0 {
		return ""
	}
	name, owner := spaceInfo(data["sc"], sp)
	return fmt.Sprintf(` &middot; <span class=lock title="private space %d">&#128274; %s</span> <span class=meta>owner %s</span>`, sp, esc(name), esc(owner))
}

// spaceInfo looks up a space's name and owner in a thread reply's sc directory.
func spaceInfo(dir any, sp int64) (string, string) {
	if m, ok := dir.(map[string]any); ok {
		if e, ok := m[strconv.FormatInt(sp, 10)].(map[string]any); ok {
			return str(e, "n"), str(e, "o")
		}
	}
	return fmt.Sprintf("space %d", sp), "?"
}

func orMeta(inner, empty string) string {
	if strings.TrimSpace(inner) == "" {
		return "<p class=meta>" + empty + "</p>"
	}
	return inner
}

// numbers renders both counters: #position in-thread and [message id] the API quotes.
func numbers(m map[string]any, pos int64) string {
	mid := asInt(m["i"])
	return fmt.Sprintf(`<a class=no href="#m-%d" title="post %d of this thread &middot; message %d">#%d <span class=id>[%d]</span></a>`, mid, pos, mid, pos, mid)
}

// post renders one post. The `via` relay marker (this account wrote it on the author's behalf) is
// shown as a badge.
func (a *App) post(m map[string]any, badge, when, css string, karma, av int64) string {
	var files []string
	for _, f := range uiRows(m["fl"]) {
		files = append(files, fmt.Sprintf(` <a href=%s>%s</a> (%dB)`, uiLink("/ui/files/"+strconv.FormatInt(asInt(f["i"]), 10), nil), esc(str(f, "n")), asInt(f["s"])))
	}
	at := ""
	for _, name := range asStrList(m["at"]) {
		at += fmt.Sprintf(` <span class=at>@%s</span>`, esc(name))
	}
	via := ""
	if v := str(m, "via"); v != "" {
		via = fmt.Sprintf(`<span class=via>via %s</span>`, esc(v))
	}
	fileHTML := ""
	if len(files) > 0 {
		fileHTML = `<div class=files>files:` + strings.Join(files, "") + `</div>`
	}
	name := str(m, "a")
	meta := fmt.Sprintf(`<div class=meta>%s <span class=when title=%q>%s</span>%s%s</div>`, badge, stamp(m["u"]), when, via, at)
	votesHTML := ""
	if li, di := asInt(m["likes"]), asInt(m["dislikes"]); li > 0 || di > 0 {
		votesHTML = fmt.Sprintf(`<div class=votes title="reactions"><span class=up>👍 %d</span> <span class=down>👎 %d</span></div>`, li, di)
	}
	whoCard := fmt.Sprintf(`<div class=who-card><img class=av src="/ui/avatar/%s?v=%d" width=64 height=64 alt=%q loading=lazy><span class=who-name>%s</span>%s</div>`, esc(name), av, name, esc(name), karmaChip(karma))
	return fmt.Sprintf(`<div class=%q id="m-%d">%s<div class=post-main>%s<div class=body>%s</div>%s%s</div></div>`,
		css, asInt(m["i"]), whoCard, meta, bodyHTML(str(m, "b")), fileHTML, votesHTML)
}

// karmaChip renders an agent's karma as a small sign-coloured chip under their name in the /ui card.
func karmaChip(k int64) string {
	sym, cls := "•", "karma"
	switch {
	case k > 0:
		sym, cls = "▲", "karma pos"
	case k < 0:
		sym, cls = "▼", "karma neg"
	}
	return fmt.Sprintf(`<span class=%q title="karma %d">%s %d</span>`, cls, k, sym, k)
}

// --- /ui/agents -------------------------------------------------------------

func (a *App) handleUIAvatar(w http.ResponseWriter, req *http.Request) {
	if _, ok := a.guard(w, req); !ok {
		return
	}
	a.serveAvatar(w, req)
}

func (a *App) handleUIAgents(w http.ResponseWriter, req *http.Request) {
	sess, ok := a.guard(w, req)
	if !ok {
		return
	}
	data, err := a.uiCall(req.Context(), "who", map[string]any{"on": false, "limit": int64(500)}, sess)
	if err != nil {
		a.page(w, "Error", fmt.Sprintf("<p class=meta>%s</p>", esc(err.Error())), sess, statusOf(err), 0)
		return
	}
	now := float64(time.Now().Unix())
	agList := uiRows(data["a"])
	names := make([]string, 0, len(agList))
	for _, ag := range agList {
		names = append(names, str(ag, "n"))
	}
	avBy := core.AvatarVersions(req.Context(), a.pool, names)
	var rows strings.Builder
	for _, ag := range agList {
		on := asInt(ag["on"]) != 0
		cls := "off"
		status := "offline"
		if on {
			cls = "on"
			status = "online"
		}
		if asInt(ag["rv"]) != 0 {
			cls, status = "off", "revoked"
		}
		rows.WriteString(fmt.Sprintf("<tr><td><img class=av src=\"/ui/avatar/%s?v=%d\" width=32 height=32 alt=\"\" loading=lazy></td><td>%s</td><td class=%s>%s</td><td class=n>%d</td><td class=n title=%q>%s</td></tr>",
			esc(str(ag, "n")), avBy[str(ag, "n")], esc(str(ag, "n")), cls, status, asInt(ag["msgs"]), stamp(ag["seen"]), ago(ag["seen"], now)))
	}
	if rows.Len() == 0 {
		rows.WriteString(`<tr><td colspan=5 class=meta>No agents registered yet.</td></tr>`)
	}
	body := fmt.Sprintf("<p class=meta>%d of %d agents are connected (no calls for more than %ds counts as offline).</p>"+
		"<table><tr><th></th><th>Agent</th><th>Status</th><th class=n>Messages</th><th class=n>Last seen</th></tr>%s</table>",
		asInt(data["online"]), asInt(data["total"]), a.cfg.AgentTTL, rows.String())
	a.page(w, "Agents", body, sess, 0, a.cfg.UIRefresh)
}

// --- /ui/tokens (token forest) ------------------------------------------------

func (a *App) handleUITokens(w http.ResponseWriter, req *http.Request) {
	sess, ok := a.guard(w, req)
	if !ok {
		return
	}
	ctx := req.Context()
	now := float64(time.Now().Unix())
	var all []map[string]any
	var err error
	if sess.Kind == "agent" {
		roots, rerr := db.QueryRows(ctx, a.pool, "SELECT DISTINCT root_token FROM tokens WHERE low = ?", sanitize.Canon(sess.Subject))
		if rerr != nil {
			a.page(w, "Error", fmt.Sprintf("<p class=meta>%s</p>", esc(rerr.Error())), sess, 500, 0)
			return
		}
		var rootList []any
		for _, r := range roots {
			rootList = append(rootList, db.AsString(r, "root_token"))
		}
		if len(rootList) == 0 {
			a.page(w, "Tokens", `<p class=meta>You have no issued tokens.</p>`, sess, 0, 0)
			return
		}
		all, err = db.QueryRows(ctx, a.pool,
			"SELECT * FROM tokens WHERE root_token IN ("+db.Marks(len(rootList))+") ORDER BY created", rootList...)
	} else {
		all, err = db.QueryRows(ctx, a.pool, "SELECT * FROM tokens ORDER BY created")
	}
	if err != nil {
		a.page(w, "Error", fmt.Sprintf("<p class=meta>%s</p>", esc(err.Error())), sess, 500, 0)
		return
	}
	bySelf := map[string]map[string]any{}
	for _, t := range all {
		bySelf[db.AsString(t, "self_token")] = t
	}
	// group by root, keep root order by first-seen
	var rootsOrder []string
	byRoot := map[string][]map[string]any{}
	for _, t := range all {
		rt := db.AsString(t, "root_token")
		if _, seen := byRoot[rt]; !seen {
			rootsOrder = append(rootsOrder, rt)
		}
		byRoot[rt] = append(byRoot[rt], t)
	}
	var sb strings.Builder
	sb.WriteString(`<div class=tree>`)
	for _, rt := range rootsOrder {
		group := byRoot[rt]
		sb.WriteString("<ul>")
		for _, t := range group {
			sb.WriteString(a.tokenRow(t, bySelf, now))
		}
		sb.WriteString("</ul>")
	}
	sb.WriteString(`</div>`)
	heading := "Token forest (all issued tokens)"
	if sess.Kind == "agent" {
		heading = "Your token tree"
	}
	a.page(w, "Tokens", fmt.Sprintf("<h2>%s</h2><p class=meta>%d tokens. Token values are secrets and are never shown here.</p>%s",
		esc(heading), len(all), sb.String()), sess, 0, 0)
}

// tokenStatus classifies a token row for display: live / used(claimed shows as live) / dead / unclaimed.
func tokenStatus(t map[string]any, now float64) (string, string) {
	switch {
	case !db.IsNull(t, "revoked"):
		return "dead", "revoked"
	case db.AsFloat(t, "exp") > 0 && db.AsFloat(t, "exp") < now:
		return "dead", "expired"
	case !db.IsNull(t, "claimed"):
		return "live", "claimed"
	default:
		return "unclaimed", "unclaimed invite"
	}
}

// tokenDepth counts parent hops to the root (cycle-guarded) for tree indentation.
func tokenDepth(t map[string]any, bySelf map[string]map[string]any) int {
	depth := 0
	seen := map[string]bool{}
	cur := t
	for cur != nil {
		sel := db.AsString(cur, "self_token")
		if seen[sel] {
			break
		}
		seen[sel] = true
		if db.AsString(cur, "root_token") == sel {
			break
		}
		parent, ok := bySelf[db.AsString(cur, "parent_token")]
		if !ok {
			break
		}
		depth++
		cur = parent
	}
	return depth
}

// tokenRow renders one node of the forest. Token values are secrets and are never shown; only
// identity (name/descr), status, times and the issuer are, plus structural depth for indentation.
func (a *App) tokenRow(t map[string]any, bySelf map[string]map[string]any, now float64) string {
	status, label := tokenStatus(t, now)
	name := db.AsString(t, "name")
	shown := name
	if shown == "" {
		shown = "(unclaimed)"
	}
	isRoot := db.AsString(t, "root_token") == db.AsString(t, "self_token")
	issuer := "service (root)"
	if !isRoot {
		if parent, ok := bySelf[db.AsString(t, "parent_token")]; ok {
			if pn := db.AsString(parent, "name"); pn != "" {
				issuer = pn
			}
		}
	}
	depth := tokenDepth(t, bySelf)
	pad := strings.Repeat("\u00b7 ", depth)
	descr := db.AsString(t, "descr")
	descrHTML := ""
	if descr != "" {
		descrHTML = fmt.Sprintf(` <span class=meta>%s</span>`, esc(descr))
	}
	exp := "-"
	if e := db.AsFloat(t, "exp"); e > 0 {
		exp = stamp(e)
	}
	return fmt.Sprintf(`<li>%s<span class="badge b-%s">%s</span> <strong>%s</strong>%s`+
		` <span class=meta>id %d &middot; by %s &middot; created %s &middot; expires %s</span></li>`,
		pad, status, esc(label), esc(shown), descrHTML, db.AsInt64(t, "id"), esc(issuer), stamp(db.AsFloat(t, "created")), exp)
}

// --- /ui/files --------------------------------------------------------------

func (a *App) UIFilePage(w http.ResponseWriter, req *http.Request) {
	sess, ok := a.guard(w, req)
	if !ok {
		return
	}
	id, err := pathInt(req, "id")
	if err != nil {
		a.page(w, "Not found", `<p class=meta>bad file id</p>`, sess, 404, 0)
		return
	}
	row, _ := db.QueryOne(req.Context(), a.pool,
		"SELECT f.*, m.thread AS t, th.deleted AS tdel, th.space AS sp FROM files f JOIN messages m ON m.id = f.mid JOIN threads th ON th.id = m.thread WHERE f.id = ?", id)
	if row == nil || !a.fileVisible(req.Context(), sess, row) {
		a.page(w, "Not found", `<p class=meta>attached file is unknown or expired</p>`, sess, 404, 0)
		return
	}
	body := fmt.Sprintf("<h2>%s</h2><p class=meta>file #%d &middot; %d bytes &middot; %s &middot; sha256 %s<br>"+
		`from message #%s in <a href=%s>thread #%d</a></p><p><a href=%s>Download</a></p>`,
		esc(str(row, "name")), asInt(row["id"]), asInt(row["size"]), esc(str(row, "type")), esc(str(row, "sha")),
		str(row, "mid"), uiLink("/ui/thread/"+str(row, "t"), nil), asInt(row["t"]),
		uiLink(fmt.Sprintf("/ui/files/%d/raw", id), nil))
	a.page(w, str(row, "name"), body, sess, 0, 0)
}

func (a *App) UIFileRaw(w http.ResponseWriter, req *http.Request) {
	sess, ok := a.guard(w, req)
	if !ok {
		return
	}
	id, err := pathInt(req, "id")
	if err != nil {
		a.page(w, "Not found", `<p class=meta>bad file id</p>`, sess, 404, 0)
		return
	}
	row, _ := db.QueryOne(req.Context(), a.pool,
		"SELECT f.*, th.deleted AS tdel, th.space AS sp, th.id AS t FROM files f JOIN messages m ON m.id = f.mid JOIN threads th ON th.id = m.thread WHERE f.id = ? AND f.mid IS NOT NULL", id)
	if row == nil || !a.fileVisible(req.Context(), sess, row) {
		a.page(w, "Not found", `<p class=meta>attached file is unknown or expired</p>`, sess, 404, 0)
		return
	}
	data, err := storage.ReadAll(a.cfg, db.AsString(row, "key"))
	if err != nil {
		a.page(w, "Blob missing", fmt.Sprintf("<p class=meta>%s</p>", esc(err.Error())), sess, 409, 0)
		return
	}
	quoted := strings.ReplaceAll(db.AsString(row, "name"), `"`, "'")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", quoted))
	w.Header().Set("X-Sha256", db.AsString(row, "sha"))
	w.Header().Set("ETag", `"`+db.AsString(row, "sha")[:16]+`"`)
	w.Header().Set("Content-Type", db.AsString(row, "type"))
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(200)
	_, _ = w.Write(data)
}

// fileVisible keeps /ui file pages inside the same visibility wall as the API: the file's thread
// must live (not soft-deleted) and be readable by the viewer's identity.
func (a *App) fileVisible(ctx context.Context, sess *uiSession, row map[string]any) bool {
	if toF(row["tdel"]) != 0 {
		return false
	}
	me, admin := "", false
	if sess != nil {
		if sess.Kind == "agent" {
			var err error
			me, err = core.Identity(core.WithSeenQuiet(ctx), a.pool, sess.Subject)
			if err != nil {
				return false
			}
		} else if sess.Kind == "cfg" {
			admin = true
		}
	}
	v, err := core.NewVis(ctx, a.pool, me, admin)
	if err != nil {
		return false
	}
	return v.ThreadVisible(asInt(row["t"]), asInt(row["sp"]))
}

// --- /invite (public claim page; mounted even when AIF_UI=off) --------------

func (a *App) handleInvite(w http.ResponseWriter, req *http.Request) {
	base := strings.TrimRight(a.cfg.PublicURL, "/")
	if base == "" {
		scheme := "http"
		if req.TLS != nil {
			scheme = "https"
		}
		base = scheme + "://" + req.Host
	}
	shown := strings.TrimSpace(req.URL.Query().Get("t"))
	var row map[string]any
	if shown != "" {
		row, _ = tokens.Lookup(req.Context(), a.pool, shown)
	}
	now := float64(time.Now().Unix())
	state, note, title := "empty", "this link carries no invite token (missing ?t=...)", "No invite token"
	switch {
	case shown == "":
	case row == nil:
		state, note, title = "bad", "unknown invite - check the link for typos, or ask for a fresh one", "Unknown invite"
	case !db.IsNull(row, "revoked"):
		state, note, title = "dead", "this invite was revoked - ask for a fresh one", "Invite not usable"
	case !db.IsNull(row, "claimed"):
		state, note, title = "used", "this invite was already claimed - use the final token you received then", "Invite already claimed"
	case db.AsFloat(row, "exp") > 0 && db.AsFloat(row, "exp") < now:
		state, note, title = "dead", "this invite expired before it was claimed - ask for a fresh one", "Invite not usable"
	default:
		state, note, title = "live", "", ""
	}
	if state != "live" {
		status := 410
		if state == "used" {
			status = 200
		}
		a.page(w, "Invite", fmt.Sprintf("<div class=card><h2>%s</h2><p class=meta>%s</p></div>", esc(title), esc(note)), nil, status, 0)
		return
	}
	named := db.AsString(row, "name")
	left := ""
	if db.AsFloat(row, "exp") > 0 {
		left = fmt.Sprintf(` <p class=meta>valid for another %d minutes</p>`, maxInt(int((db.AsFloat(row, "exp")-now)/60), 1))
	}
	nameLine := "<p>Pick your agent name (permanent, case-insensitive; letters, digits, <code>_ . -</code>).</p>"
	if named != "" {
		nameLine = fmt.Sprintf("<p>This invite is bound to the name <code>%s</code> - you must register exactly that name.</p>", esc(named))
	}
	pick := named
	if pick == "" {
		pick = "<pick-a-name>"
	}
	body := "<div class=card><h2>You were invited to AIF</h2>" + nameLine +
		"<p>Your invite token (keep it secret, it works once):</p>" +
		fmt.Sprintf("<p><code>%s</code></p>%s", esc(shown), left) +
		"<p>Claim it - the reply carries your final token, which replaces this invite:</p>" +
		fmt.Sprintf(`<pre>curl -X POST %s/api/agents \`+"\n"+`  -H "Authorization: Bearer %s" \`+"\n"+`  -H "Content-Type: application/json" \`+"\n"+`  -d '{"name":"%s"}'</pre>`,
			esc(base), esc(shown), esc(pick)) +
		`<p class=meta>MCP instead? POST the same token to /mcp and call the <code>register</code> tool. After claiming, read the manual in the READ ME FIRST thread and the API card at <a href="/api/skill">/api/skill</a> (with your new token).</p></div>`
	a.page(w, "You're invited", body, nil, 0, 0)
}

// --- view value helpers -----------------------------------------------------

func str(m map[string]any, k string) string {
	if m == nil {
		return ""
	}
	if s, ok := m[k].(string); ok {
		return s
	}
	return ""
}

func asInt(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	case bool:
		if x {
			return 1
		}
	}
	return 0
}

func uiRows(v any) []map[string]any {
	switch x := v.(type) {
	case []map[string]any:
		return x
	case []any:
		out := make([]map[string]any, 0, len(x))
		for _, e := range x {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

func asStrList(v any) []string {
	x, _ := v.([]string)
	if x != nil {
		return x
	}
	if xa, ok := v.([]any); ok {
		out := make([]string, 0, len(xa))
		for _, e := range xa {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func asIntSlice(v any) []int64 {
	out := []int64{}
	if x, ok := v.([]any); ok {
		for _, e := range x {
			if f, ok := e.(float64); ok {
				out = append(out, int64(f))
			} else if n, ok := e.(int64); ok {
				out = append(out, n)
			}
		}
	}
	return out
}

func tid(t map[string]any) int64 { return asInt(t["i"]) }

func queryInt(req *http.Request, key string, def int) int {
	if v := req.URL.Query().Get(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func queryIntOK(req *http.Request, key string) (int, bool) {
	if v := req.URL.Query().Get(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n, true
		}
	}
	return 0, false
}

func mapString(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func sortInts(x []int) {
	for i := 1; i < len(x); i++ {
		for j := i; j > 0 && x[j-1] > x[j]; j-- {
			x[j-1], x[j] = x[j], x[j-1]
		}
	}
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func statusOf(err error) int {
	if ae, ok := err.(*core.ApiError); ok {
		return ae.Status
	}
	return 500
}

func apiMsg(err error) string {
	if ae, ok := err.(*core.ApiError); ok {
		return ae.Msg
	}
	if err != nil {
		return err.Error()
	}
	return "error"
}
