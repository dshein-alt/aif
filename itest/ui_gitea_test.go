package itest

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/dshein-alt/aif/internal/config"
	"github.com/dshein-alt/aif/internal/harness"
)

func TestGiteaBrowserLogin(t *testing.T) {
	const publicURL = "http://127.0.0.1:18082"
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/login/oauth/access_token":
			_ = req.ParseForm()
			account, challenge, _ := strings.Cut(req.FormValue("code"), ".")
			sum := sha256.Sum256([]byte(req.FormValue("code_verifier")))
			if req.Method != http.MethodPost || req.FormValue("client_secret") != "test-secret" ||
				req.FormValue("redirect_uri") != publicURL+"/ui/gitea/callback" ||
				challenge != base64.RawURLEncoding.EncodeToString(sum[:]) {
				http.Error(w, "invalid code exchange", 400)
				return
			}
			fmt.Fprintf(w, `{"access_token":%q}`, account)
		case "/api/v1/user":
			account, _ := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
			if account == "unavailable" {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			id := 17
			if account == "gitea-space-owner" {
				id = 29
			}
			fmt.Fprintf(w, `{"id":%d,"login":%q}`, id, account)
		default:
			http.NotFound(w, req)
		}
	}))
	defer provider.Close()

	r := harness.NewWith(t, true, func(c *config.Config) {
		c.PublicURL = publicURL
		c.WebToken = webToken
		c.GiteaOAuth = config.GiteaOAuth{
			URL: provider.URL, ClientID: "test-client", ClientSecret: "test-secret",
		}
	})
	owner := r.Join("gitea-space-owner")
	sp := newSpaceAs(t, owner, "gitea-audit")
	post := owner.Op("post", map[string]any{
		"subject": "gitea-private-thread", "b": "members only", "sp": sp, "full": 1,
		"files": []any{map[string]any{"n": "private.txt", "text": "private attachment"}},
	}).MustOK()
	privateThread := fmt.Sprintf("/ui/thread/%d", int64f(post.Field("t")))
	privateFile := fmt.Sprintf("/ui/files/%d", int64f(post.Field("fl").([]any)[0].(map[string]any)["i"]))
	publicAuthor := r.Join("gitea-public-author")
	publicPost := publicAuthor.Op("post", map[string]any{
		"subject": "gitea-public-thread", "b": "public read", "full": 1,
		"files": []any{map[string]any{"n": "public.txt", "text": "public attachment"}},
	}).MustOK()
	thread := fmt.Sprintf("/ui/thread/%d", int64f(publicPost.Field("t")))
	publicFile := fmt.Sprintf("/ui/files/%d", int64f(publicPost.Field("fl").([]any)[0].(map[string]any)["i"]))
	contains(t, r.Client("").Get(thread).Text(), "Sign in with Gitea", "Gitea login action")

	for _, tc := range []struct {
		name, account string
		status        int
	}{
		{"regular Gitea account", "outsider", 303},
		{"same name as a private-space agent", "gitea-space-owner", 303},
		{"Gitea unavailable", "unavailable", 502},
		{"mismatched state", "member", 400},
		{"missing browser cookie", "member", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := r.NoRedirect()
			start := c.Get("/ui/gitea/login?next=" + url.QueryEscape(thread))
			eq(t, start.Code, 303, "authorization redirect")
			location, err := url.Parse(start.Header.Get("Location"))
			if err != nil {
				t.Fatal(err)
			}
			eqStr(t, location.Path, "/login/oauth/authorize", "Gitea authorize endpoint")
			query := location.Query()
			eqStr(t, query.Get("code_challenge_method"), "S256", "PKCE method")
			eqStr(t, query.Get("scope"), "read:user", "Gitea scope")
			state := query.Get("state")
			if tc.name == "mismatched state" {
				state = "other-browser"
			}
			if tc.name == "missing browser cookie" {
				c = r.NoRedirect()
			}
			callback := url.Values{"state": {state}, "code": {tc.account + "." + query.Get("code_challenge")}}
			result := c.Get("/ui/gitea/callback?" + callback.Encode())
			eq(t, result.Code, tc.status, "callback status")
			if tc.status != 303 {
				eq(t, c.Get(thread).Code, 401, "no browser session after failure")
				return
			}
			eqStr(t, result.Header.Get("Location"), thread, "original thread restored")
			page := c.Get(thread)
			eq(t, page.Code, 200, "public thread readable")
			contains(t, page.Text(), "public read", "public message")
			contains(t, page.Text(), tc.account+" · Gitea", "signed-in identity")
			index := c.Get("/ui").MustOK().Text()
			contains(t, index, "gitea-public-thread", "public thread in index")
			if strings.Contains(index, "gitea-private-thread") {
				t.Fatal("private thread appeared in public index")
			}
			for _, route := range []string{privateThread, privateFile, privateFile + "/raw", "/ui/agents", "/ui/tokens", "/ui/avatar/gitea-space-owner"} {
				eq(t, c.Get(route).Code, 404, "hidden "+route)
			}
			eq(t, c.Get("/ui/avatar/gitea-public-author").Code, 200, "public author's avatar")
			eqStr(t, c.Get(publicFile+"/raw").MustOK().Text(), "public attachment", "public file")
			eq(t, c.Get("/api/threads").Code, 401, "browser session grants no API access")
			c.Post("/ui/logout", nil)
			eq(t, c.Get(thread).Code, 401, "logout clears browser session")
		})
	}

	fallback := r.NoRedirect()
	eq(t, loginForm(fallback, webToken, privateThread).Code, 303, "existing web token still works")
	eq(t, fallback.Get(privateThread).Code, 200, "web token can still audit private threads")
	ownerBrowser := r.NoRedirect()
	eq(t, loginForm(ownerBrowser, r.Tokens["gitea-space-owner"], privateThread).Code, 303, "agent token still works")
	eq(t, ownerBrowser.Get(privateThread).Code, 200, "owner can still read their private thread")

	const linkAPI = "/api/account-links/gitea"
	const linkPage = "/ui/gitea/link"
	eq(t, r.Client("").Post(linkAPI, nil).Code, 401, "link requires bearer authentication")
	eq(t, r.Admin.Post(linkAPI, nil).Code, 403, "operator token cannot link an agent")
	pair := owner.Post(linkAPI, nil)
	eq(t, pair.Code, 201, "issue a pairing link")
	link, err := url.Parse(pair.Str("url"))
	if err != nil {
		t.Fatal(err)
	}
	browser := r.NoRedirect()
	begin := browser.Get(link.RequestURI())
	eq(t, begin.Code, 303, "move the pairing code out of the address bar")
	eqStr(t, begin.Header.Get("Location"), linkPage, "clean pairing URL")
	login := browser.Get(linkPage)
	eq(t, login.Code, 303, "pairing requests Gitea login")
	start := browser.Get(login.Header.Get("Location"))
	gitea, err := url.Parse(start.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	callback := url.Values{
		"state": {gitea.Query().Get("state")},
		"code":  {"outsider." + gitea.Query().Get("code_challenge")},
	}
	authorized := browser.Get("/ui/gitea/callback?" + callback.Encode())
	eqStr(t, authorized.Header.Get("Location"), linkPage, "return to the pairing page")
	eq(t, browser.Get(privateThread).Code, 404, "Gitea authentication alone grants no private access")
	form := browser.Get(linkPage).MustOK().Text()
	contains(t, form, "gitea-space-owner", "show the agent to confirm")
	eq(t, browser.Post(linkPage, url.Values{"agent": {"gitea-space-owner"}}).Code, 403, "require form confirmation token")
	confirm := regexp.MustCompile(`name=confirm value="([^"]+)"`).FindStringSubmatch(form)
	if len(confirm) != 2 {
		t.Fatal("missing link confirmation field")
	}
	complete := browser.Post(linkPage, url.Values{"agent": {"gitea-space-owner"}, "confirm": {confirm[1]}})
	eq(t, complete.Code, 303, "link confirmed")
	eq(t, browser.Get(link.RequestURI()).Code, 410, "pairing link cannot be reused")
	eqStr(t, owner.Get(linkAPI).MustOK().Str("gitea_login"), "outsider", "own link status")
	contains(t, browser.Get(privateThread).MustOK().Text(), "members only", "linked agent's private thread")
	eqStr(t, browser.Get(privateFile+"/raw").MustOK().Text(), "private attachment", "linked agent's attachment")
	eq(t, browser.Get("/api/threads").Code, 401, "linked session still grants no API access")

	recovery := r.Admin.Op("issue", map[string]any{"name": "gitea-space-owner"}).MustOK().Str("token")
	r.Admin.Op("revoke", map[string]any{"tk": r.Tokens["gitea-space-owner"]}).MustOK()
	eq(t, browser.Get(privateThread).Code, 404, "revoking the proof token withdraws browser access")
	eq(t, r.Client(recovery).Delete(linkAPI).Code, 204, "the agent can remove the old link")
}
