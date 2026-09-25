package itest

import (
	"net/url"
	"strings"
	"testing"

	"github.com/dshein-alt/aif/internal/config"
	"github.com/dshein-alt/aif/internal/harness"
)

// TestUIFilePages covers the guarded /ui/files views (UIFilePage + UIFileRaw).
func TestUIFilePages(t *testing.T) {
	r := harness.NewWith(t, true, func(c *config.Config) { c.WebToken = webPW })
	alice := r.Join("alice")

	up := uploadPost(t, r.URL("/api/files"), r.Tokens["alice"], [][2]string{{"doc.txt", "hello world"}}).MustOK()
	key := up.Field("u").([]any)[0].(map[string]any)["k"].(string)
	post := alice.Op("post", map[string]any{"subject": "UI Files", "b": "attached", "files": []any{map[string]any{"k": key}}}).MustOK()
	fid := fileID(t, alice, post.Field("i").(float64))

	c := uiLogin(t, r, webPW)

	page := c.Get("/ui/files/" + itoaF(fid)).MustOK().Text()
	if !strings.Contains(page, "doc.txt") || !strings.Contains(page, "Download") {
		t.Errorf("/ui/files page missing name or download link: %s", page)
	}
	threadPath := "/ui/thread/" + itoaF(post.Field("t").(float64))
	for _, want := range []string{"from message #" + itoaF(post.Field("i").(float64)), `href=` + threadPath + `>`} {
		if !strings.Contains(page, want) {
			t.Errorf("file page missing %q: %s", want, page)
		}
	}
	c.Get(threadPath).MustOK()
	if raw := c.Get("/ui/files/" + itoaF(fid) + "/raw").MustOK(); raw.Text() != "hello world" ||
		raw.Header.Get("X-Sha256") == "" {
		t.Errorf("/ui/files raw body/headers wrong: %q", raw.Text())
	}
	// an unknown id (or a pending upload with no message) is a 404, not a 500
	if bad := c.Get("/ui/files/99999999"); bad.Code != 404 || !strings.Contains(bad.Text(), "unknown or expired") {
		t.Errorf("/ui/files unknown = %d, want 404 unknown/expired", bad.Code)
	}
	// the guard turns an anonymous file view into the login page (401)
	if anon := r.Client("").Get("/ui/files/" + itoaF(fid)); anon.Code != 401 {
		t.Errorf("anonymous /ui/files = %d, want 401", anon.Code)
	}
}

// TestInvitePage covers the public /invite claim page and its live/used/revoked/unknown states.
func TestInvitePage(t *testing.T) {
	r := harness.NewWith(t, true, func(c *config.Config) { c.WebToken = webPW })
	get := func(q string) *harness.Resp { return r.Client("").Get("/invite" + q) }

	if g := get(""); g.Code != 410 || !strings.Contains(g.Text(), "No invite token") {
		t.Errorf("/invite (no t) = %d, want 410 no-token", g.Code)
	}
	if g := get("?t=not-a-real-invite"); g.Code != 410 || !strings.Contains(g.Text(), "Unknown invite") {
		t.Errorf("/invite unknown = %d %s, want 410 unknown", g.Code, g.Text())
	}

	// a fresh, name-bound invite renders the claim instructions live
	inv := r.Issue("carol")
	if g := get("?t=" + url.QueryEscape(inv)); g.Code != 200 ||
		!strings.Contains(g.Text(), "You were invited to AIF") || !strings.Contains(g.Text(), "carol") {
		t.Errorf("live invite page wrong (code %d): %s", g.Code, g.Text())
	}

	// once claimed it reports the "used" state (200, not an error page)
	r.Claim("carol", inv)
	if g := get("?t=" + url.QueryEscape(inv)); g.Code != 200 || !strings.Contains(g.Text(), "already claimed") {
		t.Errorf("claimed invite = %d, want 200 already-claimed", g.Code)
	}

	// a revoked invite is a dead link (410)
	inv2 := r.Issue("dave")
	r.Admin.Op("revoke", map[string]any{"tk": inv2}).MustOK()
	if g := get("?t=" + url.QueryEscape(inv2)); g.Code != 410 || !strings.Contains(g.Text(), "revoked") {
		t.Errorf("revoked invite = %d %s, want 410 revoked", g.Code, g.Text())
	}
}
