package itest

import (
	"strconv"
	"strings"
	"testing"

	"github.com/dshein-alt/aif/internal/config"
	"github.com/dshein-alt/aif/internal/harness"
)

// TestRESTSurface exercises the thin HTTP handlers that map routes onto ops, plus the transport
// behaviours (auth, formats, long=1, batch, discovery, error mapping) that Op()/the /ui path miss.
func TestRESTSurface(t *testing.T) {
	r := harness.New(t, true)
	admin := r.Admin
	alice := r.Join("alice")

	// --- discovery / no auth ---
	if h := r.Client("").Get("/healthz"); h.Code != 200 || h.Field("ok") == nil || h.Str("v") == "" {
		t.Errorf("healthz wrong: %d %s", h.Code, h.Text())
	}
	// Root redirects humans (Accept: text/html) to /ui but returns a JSON directory otherwise.
	root := r.NoRedirect().H("Accept", "text/html").Get("/")
	if root.Code != 303 || root.Header.Get("Location") != "/ui" {
		t.Errorf("root html redirect = %d loc %q, want 303 /ui", root.Code, root.Header.Get("Location"))
	}
	if dir := r.Client("").Get("/"); dir.Code != 200 || dir.Str("service") == "" {
		t.Errorf("root json = %d %s", dir.Code, dir.Text())
	}

	// --- auth error mapping ---
	if p := r.Client("").Get("/api/ping"); p.Code != 401 || p.Str("err") != "need_token" {
		t.Errorf("no-token ping = %d %s, want 401 need_token", p.Code, p.Text())
	}
	r2 := harness.NewWith(t, false, func(c *config.Config) { c.WebToken = "webpw" })
	if w := r2.Client("webpw").Get("/api/skill"); w.Code != 403 || w.Str("err") != "web_token" {
		t.Errorf("web-token REST = %d %s, want 403 web_token", w.Code, w.Text())
	}

	// --- skill card: text (default) and json ---
	if s := admin.Get("/api/skill"); s.Code != 200 || !strings.Contains(s.Header.Get("Content-Type"), "text/plain") || !strings.Contains(s.Text(), "POST /api/op") {
		t.Errorf("skill text wrong: %d %q", s.Code, s.Header.Get("Content-Type"))
	}
	if sj := admin.Get("/api/skill?fmt=json"); sj.Code != 200 || !strings.Contains(sj.Header.Get("Content-Type"), "application/json") || len(sj.JSON()) == 0 {
		t.Error("skill json should return a JSON object")
	}

	// --- generic call: POST /api/op, GET /api/op, unknown op, bad json, missing do ---
	if ok := admin.Post("/api/op", map[string]any{"do": "threads"}).MustOK(); ok.Field("th") == nil {
		t.Error("POST /api/op threads should return a th list")
	}
	if g := admin.Get("/api/op?do=threads&limit=5"); g.Code != 200 || g.Field("th") == nil {
		t.Errorf("GET /api/op?do=threads = %d %s", g.Code, g.Text())
	}
	if u := admin.Post("/api/op", map[string]any{"do": "no_such_op"}); u.Code != 400 || u.Str("err") != "unknown_op" {
		t.Errorf("unknown op = %d %s, want 400 unknown_op", u.Code, u.Text())
	}
	bad := admin.Do("POST", "/api/op", "not-an-object")
	if bad.Code != 400 || bad.Str("err") != "bad_json" {
		t.Errorf("bad json = %d %s, want 400 bad_json", bad.Code, bad.Text())
	}
	if nd := admin.Post("/api/op", map[string]any{"nothing": 1}); nd.Code != 400 || nd.Str("err") != "bad_request" {
		t.Errorf("missing do = %d %s, want 400 bad_request", nd.Code, nd.Text())
	}

	// --- response formats on a list op (Render wiring over HTTP) ---
	if tsv := admin.Get("/api/threads?fmt=tsv"); tsv.Code != 200 || !strings.Contains(tsv.Header.Get("Content-Type"), "text/tab-separated-values") {
		t.Errorf("fmt=tsv content-type = %q", tsv.Header.Get("Content-Type"))
	} else if !strings.Contains(tsv.Text(), "\t") {
		t.Error("fmt=tsv body should be tab separated")
	}
	if jl := admin.Get("/api/threads?fmt=jsonl"); jl.Code != 200 || !strings.Contains(jl.Header.Get("Content-Type"), "application/x-ndjson") {
		t.Errorf("fmt=jsonl content-type = %q", jl.Header.Get("Content-Type"))
	}

	// --- long=1 is a transport flag accepted on a read op ---
	if lg := admin.Get("/api/op?do=threads&long=1"); lg.Code != 200 {
		t.Errorf("long=1 = %d %s, want 200", lg.Code, lg.Text())
	}

	// --- batch: all-good runs every step; an error does not roll back the others ---
	// (batch steps run non-admin, so this must be a normal agent, not the gatekeeper)
	b := alice.Post("/api/batch", map[string]any{"ops": []any{
		map[string]any{"do": "who"}, map[string]any{"do": "ping"},
	}}).MustOK()
	if b.Field("ok").(float64) != 1 || len(b.Field("r").([]any)) != 2 {
		t.Errorf("all-good batch wrong: %s", b.Text())
	}
	bm := alice.Post("/api/batch", map[string]any{"ops": []any{
		map[string]any{"do": "who"}, map[string]any{"do": "no_such"},
	}})
	if bm.Field("ok").(float64) != 0 || bm.Field("err") == nil || len(bm.Field("r").([]any)) != 2 {
		t.Errorf("error-step batch should run both + flag err: %s", bm.Text())
	}

	// --- search over subjects and names ---
	alice.Op("post", map[string]any{"subject": "Quarterly budget review", "b": "numbers"}).MustOK()
	sr := alice.Get("/api/search?q=budget").MustOK()
	if sr.Str("q") != "budget" || sr.Field("th") == nil || sr.Field("a") == nil {
		t.Errorf("search shape wrong: %s", sr.Text())
	}
	if sr := alice.Get("/api/search"); sr.Code != 400 || sr.Str("err") != "bad_request" {
		t.Errorf("search without q = %d %s, want 400 bad_request", sr.Code, sr.Text())
	}

	// --- threads CRUD + messages via REST paths ---
	nt := alice.Post("/api/threads", map[string]any{"subject": "REST CRUD thread", "b": "first post"}).MustOK()
	tid := int64f(nt.Field("t"))
	firstMid := int64f(nt.Field("i"))
	if tid == 0 || firstMid == 0 {
		t.Fatal("POST /api/threads did not return a thread and message id")
	}
	if list := alice.Get("/api/threads?q=REST%20CRUD"); list.Field("th") == nil || list.Field("n").(float64) < 1 {
		t.Errorf("threads list should find the new thread: %s", list.Text())
	}
	th := alice.Get("/api/threads/" + strconv.FormatInt(tid, 10)).MustOK()
	if int64f(th.Field("i")) != tid || th.Field("ms") == nil {
		t.Errorf("GET /api/threads/{id} wrong: %s", th.Text())
	}
	replyMid := int64f(alice.Post("/api/threads/"+strconv.FormatInt(tid, 10)+"/msgs", map[string]any{"b": "a reply"}).MustOK().Field("i"))
	if g := alice.Get("/api/messages/" + strconv.FormatInt(firstMid, 10)).MustOK(); int64f(g.Field("i")) != firstMid {
		t.Errorf("GET /api/messages/{id} wrong: %s", g.Text())
	}
	alice.Delete("/api/messages/" + strconv.FormatInt(replyMid, 10)).MustOK()
	if gone := alice.Get("/api/messages/" + strconv.FormatInt(replyMid, 10)); gone.Code != 404 {
		t.Errorf("deleted message = %d, want 404", gone.Code)
	}
	alice.Delete("/api/threads/" + strconv.FormatInt(tid, 10)).MustOK()
	if gone := alice.Get("/api/threads/" + strconv.FormatInt(tid, 10)); gone.Code != 404 {
		t.Errorf("deleted thread = %d, want 404", gone.Code)
	}

	// --- inbox: sub / seen / feed / poll / unread over REST ---
	nt2 := alice.Post("/api/threads", map[string]any{"subject": "Inbox thread", "b": "hello"}).MustOK()
	tid2 := int64f(nt2.Field("t"))
	if sub := alice.Post("/api/sub", map[string]any{"t": tid2}).MustOK(); sub.Field("su") == nil {
		t.Errorf("POST /api/sub should return the subscription list: %s", sub.Text())
	}
	if sl := alice.Get("/api/sub").MustOK(); sl.Field("su") == nil {
		t.Error("GET /api/sub should list subscriptions")
	}
	if del := alice.Do("DELETE", "/api/sub", map[string]any{"t": tid2}).MustOK(); del.Field("unsubscribed") == nil {
		t.Errorf("DELETE /api/sub should unsubscribe: %s", del.Text())
	}
	if seen := alice.Post("/api/seen", map[string]any{"seq": 0}).MustOK(); seen.Field("ok") == nil {
		t.Errorf("POST /api/seen wrong: %s", seen.Text())
	}
	if fd := alice.Get("/api/feed").MustOK(); fd.Field("seq") == nil || fd.Field("ms") == nil {
		t.Errorf("GET /api/feed wrong: %s", fd.Text())
	}
	if po := alice.Get("/api/poll").MustOK(); po.JSON() == nil {
		t.Error("GET /api/poll should return an object")
	}
	if un := alice.Get("/api/unread").MustOK(); un.JSON() == nil {
		t.Error("GET /api/unread should return an object")
	}

	// --- transport-level errors ---
	if nf := admin.Get("/api/does_not_exist"); nf.Code != 404 || nf.Str("err") != "not_found" {
		t.Errorf("unknown route = %d %s, want 404 not_found", nf.Code, nf.Text())
	}
	if ma := admin.Do("DELETE", "/api/skill", nil); ma.Code != 405 || ma.Str("err") != "http_error" {
		t.Errorf("wrong method = %d %s, want 405 http_error", ma.Code, ma.Text())
	}
	if pb := admin.Get("/api/threads/not-a-number"); pb.Code != 400 {
		t.Errorf("bad path id = %d, want 400", pb.Code)
	}
}
