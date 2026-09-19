// Package harness is the Go test rig: it spins the real HTTP app (chi router + core ops + Postgres)
// on an httptest server against a throwaway per-test database, and exposes admin/agent clients plus
// the issue->claim join flow. It drives the stack end-to-end: real requests, no mocks.
//
// Requirements: a reachable Postgres for tests, via AIF_PG_TEST_URL (a URL whose role can CREATE
// DATABASE, e.g. postgres://aif:testpw@127.0.0.1:55432/aif_test?sslmode=disable). When unset, tests
// skip rather than fail, so a plain `go test ./...` without a database stays green.
package harness

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dshein-alt/aif/internal/config"
	"github.com/dshein-alt/aif/internal/db"
	"github.com/dshein-alt/aif/internal/httpx"
	"github.com/dshein-alt/aif/internal/seed"
)

// AdminToken and Salt are the fixed rig credentials so expected token derivations line up.
const (
	AdminToken = "admin-secret"
	Salt       = "unit-test-salt-0001"
)

var dbSeq int64

func testURL(t *testing.T) string {
	u := strings.TrimSpace(getenv("AIF_PG_TEST_URL"))
	if u == "" {
		t.Skip("AIF_PG_TEST_URL not set; skipping Postgres-backed test")
	}
	return u
}

// Rig is one server plus a registry of claimed agent tokens.
type Rig struct {
	t      *testing.T
	Cfg    *config.Config
	App    *httpx.App
	Server *httptest.Server
	pool   *db.Pool
	admin  *Client
	Admin  *Client
	Tokens map[string]string
}

// New builds a rig with a fresh database and the given feature flags. UI toggles the /ui surface.
func New(t *testing.T, ui bool) *Rig {
	return NewWith(t, ui, nil)
}

// NewWith lets a test tweak the config before the app is built (seed, web token, caps, ...).
func NewWith(t *testing.T, ui bool, tweak func(*config.Config)) *Rig {
	t.Helper()
	base := testURL(t)
	ctx := context.Background()

	maint, err := db.Open(ctx, base)
	if err != nil {
		t.Fatalf("connect maintenance db: %v", err)
	}
	t.Cleanup(maint.Close)

	name := fmt.Sprintf("aif_t_%d_%s", atomic.AddInt64(&dbSeq, 1), randHex(4))
	if _, err := maint.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create test db: %v", err)
	}
	drop := func() { // terminate stragglers, then drop
		_, _ = maint.Exec(context.Background(), "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1", name)
		_, _ = maint.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name)
	}
	t.Cleanup(drop)

	perDB := swapDatabase(base, name)
	pool, err := db.Open(ctx, perDB)
	if err != nil {
		t.Fatalf("connect test db: %v", err)
	}
	t.Cleanup(pool.Close) // runs before drop (LIFO)

	dir := t.TempDir()
	cfg := &config.Config{
		Tokens: []string{AdminToken}, AdminTokens: []string{AdminToken}, TokenSalt: Salt,
		InviteTTL: 86400, DataDir: dir, AttachmentsDir: dir + "/attachments",
		MaxFileSize: 5 * 1024 * 1024, MaxFilesPerMessage: 8, MaxMessageLength: 20000,
		MaxSubjectLength: 200, MaxPageSize: 100, FeedDefaultLimit: 50,
		AgentTTL: 300, UploadTTL: 3600, MaxOpsPerBatch: 20, AvatarMaxSize: 512 * 1024,
		UISessionTTL: 43200, UIRefresh: 120,
		Seed: false, UI: ui, PGURL: perDB,
	}
	if tweak != nil {
		tweak(cfg)
	}

	if err := db.Init(ctx, pool, cfg); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	if cfg.Seed {
		if _, err := seed.Run(ctx, pool, cfg); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	app := httpx.NewApp(cfg, pool, cfg.UI)
	srv := httptest.NewServer(app.Router())
	t.Cleanup(srv.Close)

	r := &Rig{t: t, Cfg: cfg, App: app, Server: srv, pool: pool, Tokens: map[string]string{}}
	r.Admin = r.Client(AdminToken)
	r.admin = r.Admin
	return r
}

// Pool exposes the rig's pool for tests that assert on rows directly.
func (r *Rig) Pool() *db.Pool { return r.pool }

func (r *Rig) URL(path string) string { return r.Server.URL + path }

// --- clients ----------------------------------------------------------------

// Client is an HTTP client with an optional default bearer and a cookie jar (for /ui sessions).
type Client struct {
	t       *testing.T
	r       *Rig
	bearer  string
	headers map[string]string
	hc      *http.Client
}

func (r *Rig) Client(bearer string) *Client {
	jar, _ := cookiejar.New(nil)
	return &Client{t: r.t, r: r, bearer: bearer, hc: &http.Client{Jar: jar}}
}

// NoRedirect returns a client that does not follow redirects, so a test can assert 303 targets and
// the Set-Cookie issued by the redirect.
func (r *Rig) NoRedirect() *Client {
	jar, _ := cookiejar.New(nil)
	return &Client{t: r.t, r: r, hc: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}}
}

// H returns a copy of the client that also sends an extra header on every request (e.g. x-agent).
func (c *Client) H(key, value string) *Client {
	h := map[string]string{}
	for k, v := range c.headers {
		h[k] = v
	}
	h[strings.ToLower(key)] = value
	cp := *c
	cp.headers = h
	return &cp
}

// Do sends a request; body is JSON-encoded when non-nil (pass url.Values for a form post).
func (c *Client) Do(method, path string, body any) *Resp {
	c.t.Helper()
	var rdr io.Reader
	ctype := ""
	switch b := body.(type) {
	case nil:
	case url.Values:
		rdr = strings.NewReader(b.Encode())
		ctype = "application/x-www-form-urlencoded"
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			c.t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(raw)
		ctype = "application/json"
	}
	req, err := http.NewRequest(method, c.r.URL(path), rdr)
	if err != nil {
		c.t.Fatalf("new request: %v", err)
	}
	if ctype != "" {
		req.Header.Set("content-type", ctype)
	}
	if c.bearer != "" {
		req.Header.Set("authorization", "Bearer "+c.bearer)
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	res, err := c.hc.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	return &Resp{Code: res.StatusCode, Header: res.Header, Body: data, Cookies: res.Cookies()}
}

func (c *Client) Get(path string) *Resp            { return c.Do("GET", path, nil) }
func (c *Client) Post(path string, body any) *Resp { return c.Do("POST", path, body) }
func (c *Client) Delete(path string) *Resp         { return c.Do("DELETE", path, nil) }

// Op posts {"do":name, ...extra} to /api/op as this client's bearer.
func (c *Client) Op(name string, extra map[string]any) *Resp {
	body := map[string]any{"do": name}
	for k, v := range extra {
		body[k] = v
	}
	return c.Post("/api/op", body)
}

// --- join flow helpers (mirror Rig.issue / Rig.claim) -----------------------

// Issue mints an invite (name=="") or a named token via the gatekeeper, returning the token string.
func (r *Rig) Issue(name string, extra ...map[string]any) string {
	r.t.Helper()
	body := map[string]any{}
	if name != "" {
		body["name"] = name
	}
	for _, m := range extra {
		for k, v := range m {
			body[k] = v
		}
	}
	res := r.admin.Op("issue", body)
	res.MustOK()
	tok, _ := res.Field("token").(string)
	if tok == "" {
		r.t.Fatalf("issue returned no token: %s", res.Text())
	}
	return tok
}

// Claim runs the full join flow (issue unless a token is given, then register) and records the token.
func (r *Rig) Claim(name, invite string) *Resp {
	r.t.Helper()
	if invite == "" {
		invite = r.Issue("")
	}
	res := r.Client(invite).Post("/api/agents", map[string]any{"name": name})
	if res.Code == 200 {
		if tok, ok := res.Field("token").(string); ok {
			r.Tokens[name] = tok
		}
	}
	return res
}

// Join is the happy path: issue + claim + return the claimed agent's bearer client.
func (r *Rig) Join(name string) *Client {
	r.t.Helper()
	res := r.Claim(name, "")
	res.MustOK()
	return r.Client(r.Tokens[name])
}

// --- response ---------------------------------------------------------------

// Resp is a buffered response with JSON helpers.
type Resp struct {
	Code    int
	Header  http.Header
	Body    []byte
	Cookies []*http.Cookie
}

func (r *Resp) Text() string { return string(r.Body) }

func (r *Resp) JSON() map[string]any {
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		panic(fmt.Sprintf("response is not a JSON object (%d): %s", r.Code, r.Text()))
	}
	return m
}

func (r *Resp) Field(k string) any { return r.JSON()[k] }

func (r *Resp) Str(k string) string {
	if s, ok := r.Field(k).(string); ok {
		return s
	}
	return ""
}

func (r *Resp) MustStatus(code int) *Resp {
	if r.Code != code {
		panic(fmt.Sprintf("expected status %d, got %d: %s", code, r.Code, r.Text()))
	}
	return r
}

func (r *Resp) MustOK() *Resp { return r.MustStatus(200) }

// --- small utilities --------------------------------------------------------

func swapDatabase(base, name string) string {
	// Replace the path segment (database name) of a postgres URL, preserving query params.
	if strings.Contains(base, "://") {
		if u, err := url.Parse(base); err == nil {
			u.Path = "/" + name
			return u.String()
		}
	}
	i := strings.LastIndex(base, "/")
	j := strings.Index(base[i+1:], "?")
	if j < 0 {
		return base[:i+1] + name
	}
	return base[:i+1] + name + base[i+1+j:]
}

func getenv(k string) string { return os.Getenv(k) }

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
