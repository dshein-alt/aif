package itest

import (
	"fmt"
	"net/url"
	"strings"
	"testing"

	"aif/internal/config"
	"aif/internal/harness"
)

const webPW = "ui-test-web-token"

// uiRig builds a UI-enabled rig with a web token, one thread owned by alice, and a reply by bob with
// positive karma and an upvote. Returns the rig and the thread id.
func uiRig(t *testing.T) (*harness.Rig, int64) {
	t.Helper()
	r := harness.NewWith(t, true, func(c *config.Config) { c.WebToken = webPW })

	alice := r.Join("alice")
	post := alice.Op("post", map[string]any{"subject": "UI Demo", "b": "**hello** <script>alert(1)</script>"}).MustOK()
	tid := int64f(post.Field("t"))

	bob := r.Join("bob")
	bob.Op("sub", map[string]any{"t": tid}).MustOK()
	bobMid := int64f(bob.Op("post", map[string]any{"t": tid, "b": "reply from bob"}).MustOK().Field("i"))

	// Gatekeeper rewards bob (admin bypasses the owner gate); the owner then upvotes bob's post.
	r.Admin.Op("karma", map[string]any{"t": tid, "target": "bob", "delta": 3}).MustOK()
	alice.Op("vote", map[string]any{"id": bobMid, "dir": 1}).MustOK()

	return r, tid
}

func uiLogin(t *testing.T, r *harness.Rig, password string) *harness.Client {
	t.Helper()
	c := r.Client("")
	c.Post("/ui/login", url.Values{"password": {password}, "next": {"/ui"}})
	if got := c.Get("/ui").Code; got != 200 {
		t.Fatalf("login did not open /ui (status %d)", got)
	}
	return c
}

func TestUIThreadRendersAvatarsKarmaVotesAndMarkdown(t *testing.T) {
	r, tid := uiRig(t)
	page := uiLogin(t, r, webPW).Get(fmt.Sprintf("/ui/thread/%d", tid)).Text()

	for _, want := range []string{
		`<span class=who-name>bob</span>`, `src="/ui/avatar/bob"`, `<span class=who-name>alice</span>`,
		`class="karma pos" title="karma 3"`, "▲ 3", // bob positive
		`class="karma" title="karma 0"`, "• 0", // alice neutral
		`<span class=up>👍 1</span>`, `<span class=down>👎 0</span>`, // reaction counters
		"<strong>hello</strong>", "&lt;script&gt;", // markdown rendered, raw HTML escaped
	} {
		if !strings.Contains(page, want) {
			t.Errorf("thread page missing %q", want)
		}
	}
	if strings.Contains(page, "<script>alert(1)") {
		t.Error("a post body must never yield an executable <script> element")
	}
}

func TestUIOtherPagesAndGuards(t *testing.T) {
	r, tid := uiRig(t)
	c := uiLogin(t, r, webPW)

	if page := c.Get("/ui").Text(); !strings.Contains(page, "UI Demo") {
		t.Error("index should list the thread subject")
	}
	if page := c.Get("/ui/agents").Text(); !strings.Contains(page, "bob") || !strings.Contains(page, "/ui/avatar/bob") {
		t.Error("agents page should list agents with avatars")
	}
	// The token forest is visible to a config/web session and names tokens without leaking secrets.
	tok := c.Get("/ui/tokens")
	if tok.Code != 200 || !strings.Contains(tok.Text(), "Token forest") || !strings.Contains(tok.Text(), "bob") {
		t.Errorf("tokens page wrong (code %d)", tok.Code)
	}
	if av := c.Get(fmt.Sprintf("/ui/avatar/bob")); av.Code != 200 || av.Header.Get("Content-Type") == "" {
		t.Errorf("/ui/avatar status %d ct %q", av.Code, av.Header.Get("Content-Type"))
	}

	// Guard: an anonymous thread request is the login page (401), not the thread.
	anon := r.Client("").Get(fmt.Sprintf("/ui/thread/%d", tid))
	if anon.Code != 401 || !strings.Contains(anon.Text(), "Sign in") {
		t.Errorf("unauthenticated thread = %d, want 401 login page", anon.Code)
	}

	// A legacy ?token= link is accepted once: 303 to the clean URL plus a session cookie.
	lg := r.NoRedirect().Get("/ui/thread/" + fmt.Sprint(tid) + "?token=" + url.QueryEscape(webPW))
	if lg.Code != 303 {
		t.Errorf("legacy token status = %d, want 303", lg.Code)
	}
	issued := false
	for _, ck := range lg.Cookies {
		if ck.Name == "aif_ui" && ck.Value != "" {
			issued = true
		}
	}
	if !issued {
		t.Error("legacy token flow should issue the aif_ui session cookie")
	}

	// Logout clears the cookie; the session no longer opens the thread.
	c.Post("/ui/logout", nil)
	if got := c.Get(fmt.Sprintf("/ui/thread/%d", tid)).Code; got != 401 {
		t.Errorf("after logout, thread = %d, want 401", got)
	}
}

// TestUIPagination exercises the numbered pager (pageWindow / pageWindowHTML) by shrinking the page
// size so a small thread spans several pages, then walking the first and second pages.
func TestUIPagination(t *testing.T) {
	r := harness.NewWith(t, true, func(c *config.Config) { c.WebToken = webPW })
	alice := r.Join("alice")
	tid := int64f(alice.Op("post", map[string]any{"subject": "Long thread", "b": "description"}).MustOK().Field("t"))
	for i := 0; i < 4; i++ {
		alice.Op("post", map[string]any{"t": tid, "b": fmt.Sprintf("reply %d", i)}).MustOK()
	}
	c := uiLogin(t, r, webPW)

	p1 := c.Get(fmt.Sprintf("/ui/thread/%d?limit=2", tid)).MustOK().Text() // 5 posts / 2 = 3 pages
	for _, want := range []string{`class=pager`, "page 1 of 3", `<span class=cur>1</span>`, "&laquo;", "next"} {
		if !strings.Contains(p1, want) {
			t.Errorf("page 1 pager missing %q", want)
		}
	}

	p2 := c.Get(fmt.Sprintf("/ui/thread/%d?limit=2&page=2", tid)).MustOK().Text()
	for _, want := range []string{"page 2 of 3", `<span class=cur>2</span>`} {
		if !strings.Contains(p2, want) {
			t.Errorf("page 2 pager missing %q", want)
		}
	}
	// an out-of-range page clamps to the last page rather than rendering an empty 500.
	if last := c.Get(fmt.Sprintf("/ui/thread/%d?limit=2&page=99", tid)); last.Code != 200 ||
		!strings.Contains(last.Text(), "page 3 of 3") {
		t.Errorf("clamped page = %d, want 200 page 3 of 3", last.Code)
	}
}
