package itest

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"aif/internal/config"
	"aif/internal/db"
	"aif/internal/harness"
)

const webToken = "hum4n-web-token"

// webRig is a UI-mounted app whose web token is `webToken`.
func webRig(t *testing.T) *harness.Rig {
	return harness.NewWith(t, true, func(c *config.Config) { c.WebToken = webToken })
}

func loginForm(c *harness.Client, password, next string) *harness.Resp {
	if next == "" {
		next = "/ui"
	}
	return c.Post("/ui/login", url.Values{"password": {password}, "next": {next}})
}

func cookieValue(res *harness.Resp) string {
	sc := res.Header.Get("Set-Cookie")
	i := strings.Index(sc, "aif_ui=")
	if i < 0 {
		return ""
	}
	return strings.SplitN(sc[i+len("aif_ui="):], ";", 2)[0]
}

func splitCookie(value string) (kind, subject, exp, sig string) {
	p := strings.Split(value, ":")
	return p[0], p[1], p[2], p[3]
}

func saltOf(t *testing.T, r *harness.Rig) string {
	t.Helper()
	v, ok, err := db.QueryOneValue(context.Background(), r.Pool(), "SELECT value FROM meta WHERE key = 'ui.session_salt'")
	if err != nil || !ok {
		t.Fatalf("no session salt: ok=%v err=%v", ok, err)
	}
	s, _ := v.(string)
	return s
}

func TestLoginPageIsAPostForm(t *testing.T) {
	r := webRig(t)
	res := r.Client("").Get("/ui")
	eq(t, res.Code, 401, "unauth /ui")
	contains(t, res.Text(), `method=post`, "form method")
	contains(t, res.Text(), `action="/ui/login"`, "form action")
	contains(t, res.Text(), `type=password`, "password input")
	eqStr(t, res.Header.Get("Referrer-Policy"), "no-referrer", "referrer policy")
}

func TestWrongPasswordIsA403(t *testing.T) {
	r := webRig(t)
	res := loginForm(r.NoRedirect(), "wrong", "")
	eq(t, res.Code, 403, "wrong password status")
	contains(t, res.Text(), "password rejected", "error text")
	if strings.Contains(strings.ToLower(res.Header.Get("Set-Cookie")), "aif_ui=") {
		t.Fatalf("cookie must not be set on failure: %q", res.Header.Get("Set-Cookie"))
	}
}

func TestLoginSetsADerivedHttponlyCookie(t *testing.T) {
	r := webRig(t)
	res := loginForm(r.NoRedirect(), webToken, "")
	eq(t, res.Code, 303, "login status")
	eqStr(t, res.Header.Get("Location"), "/ui", "redirect target")
	sc := res.Header.Get("Set-Cookie")
	contains(t, sc, "HttpOnly", "httponly")
	contains(t, strings.ToLower(sc), "samesite=lax", "samesite")
	contains(t, sc, "Path=/ui", "path")
	lower := strings.ToLower(strings.ReplaceAll(sc, "samesite", ""))
	if strings.Contains(lower, "secure") {
		t.Fatalf("must not be Secure over http: %q", sc)
	}
	value := cookieValue(res)
	kind, subject, exp, sig := splitCookie(value)
	eqStr(t, kind, "cfg", "kind")
	eq(t, len(subject), 12, "subject length")
	if strings.Contains(subject, webToken) || strings.Contains(value, webToken) {
		t.Fatalf("web token leaked into cookie: %q", value)
	}
	if exp == "0" || exp[0] <= '0' {
		t.Fatalf("exp not positive: %q", exp)
	}
	eq(t, len(sig), 24, "sig length")
}

func TestCookieNeverAuthenticatesTheAPI(t *testing.T) {
	r := webRig(t)
	c := r.NoRedirect()
	loginForm(c, webToken, "")
	eq(t, c.Get("/ui").Code, 200, "ui readable with cookie")
	res := c.Get("/api/threads")
	eq(t, res.Code, 401, "cookie does not authenticate api")
	eqStr(t, errCode(res), "need_token", "api err")
}

func TestLoginNextRestrictedToUIPaths(t *testing.T) {
	r := webRig(t)
	for _, tc := range []struct{ in, want string }{
		{"https://evil.example/phish", "/ui"},
		{"//evil.example", "/ui"},
		{"/ui/agents", "/ui/agents"},
	} {
		res := loginForm(r.NoRedirect(), webToken, tc.in)
		eqStr(t, res.Header.Get("Location"), tc.want, "next "+tc.in)
	}
}

func TestLogoutClearsTheSession(t *testing.T) {
	r := webRig(t)
	c := r.NoRedirect()
	loginForm(c, webToken, "")
	eq(t, c.Get("/ui").Code, 200, "logged in")
	c.Post("/ui/logout", nil)
	eq(t, c.Get("/ui").Code, 401, "logged out")
}

func TestConfigSessionDiesWhenItsTokenLeavesTheConfig(t *testing.T) {
	r := webRig(t)
	c := r.NoRedirect()
	loginForm(c, webToken, "")
	eq(t, c.Get("/ui").Code, 200, "logged in")
	r.Cfg.WebToken = "rotated-web-token"
	eq(t, c.Get("/ui").Code, 401, "old session dies after rotation")
}

func TestAgentSessionDiesWithTheCredential(t *testing.T) {
	r := webRig(t)
	r.Join("bob")
	c := r.NoRedirect()
	res := loginForm(c, r.Tokens["bob"], "/ui/agents")
	eqStr(t, res.Header.Get("Location"), "/ui/agents", "agent login next")
	contains(t, c.Get("/ui").Text(), "bob", "sign out shows name")
	r.Admin.Op("revoke", map[string]any{"name": "bob"}).MustOK()
	eq(t, c.Get("/ui").Code, 401, "agent session dies on revoke")
}

func TestForgedOrExpiredCookieIsRejected(t *testing.T) {
	r := webRig(t)
	login := loginForm(r.NoRedirect(), webToken, "")
	kind, subject, exp, sig := splitCookie(cookieValue(login))
	bad := r.Client("").H("cookie", "aif_ui="+kind+":"+subject+":"+exp+":"+strings.Repeat("0", 24))
	eq(t, bad.Get("/ui").Code, 401, "bad signature")
	expired := r.Client("").H("cookie", "aif_ui="+kind+":"+subject+":1:"+sig)
	eq(t, expired.Get("/ui").Code, 401, "expired cookie")
	garbage := r.Client("").H("cookie", "aif_ui=garbage")
	eq(t, garbage.Get("/ui").Code, 401, "garbage")
}

func TestSessionSaltIsCreatedOnceAndReused(t *testing.T) {
	r := webRig(t)
	loginForm(r.NoRedirect(), webToken, "")
	first := saltOf(t, r)
	loginForm(r.NoRedirect(), webToken, "")
	if saltOf(t, r) != first {
		t.Fatal("session salt changed")
	}
	eq(t, len(first), 32, "salt length")
}

func TestLegacyTokenLinksAreCookiedAndCleaned(t *testing.T) {
	r := webRig(t)
	r.Admin.Post("/api/threads", map[string]any{"subject": "legacy check", "b": "x"}).MustOK()
	c := r.NoRedirect()
	res := c.Get("/ui?token=" + url.QueryEscape(webToken) + "&q=legacy")
	eq(t, res.Code, 303, "legacy accept status")
	eqStr(t, res.Header.Get("Location"), "/ui?q=legacy", "token stripped from url")
	if !strings.Contains(strings.ToLower(res.Header.Get("Set-Cookie")), "aif_ui=") {
		t.Fatalf("no cookie on legacy accept: %q", res.Header.Get("Set-Cookie"))
	}
	page := c.Get(res.Header.Get("Location")).Text()
	contains(t, page, "legacy check", "listing shows thread")
	if strings.Contains(page, "token=") {
		t.Fatal("token leaked into page")
	}
}

func TestRejectedLegacyTokenGetsAClearError(t *testing.T) {
	r := webRig(t)
	res := r.Client("").Get("/ui?token=nope")
	eq(t, res.Code, 403, "bad legacy token")
	contains(t, res.Text(), "token was rejected", "error text")
}

func TestGatekeeperPasswordIsDowngradedToReadonly(t *testing.T) {
	r := webRig(t)
	c := r.NoRedirect()
	login := loginForm(c, harness.AdminToken, "")
	eq(t, c.Get("/ui").Code, 200, "gatekeeper can read /ui")
	eq(t, c.Get("/api/threads").Code, 401, "gatekeeper session grants nothing to the api")
	if strings.Contains(cookieValue(login), harness.AdminToken) {
		t.Fatal("gatekeeper token leaked into cookie")
	}
}
