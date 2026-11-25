package itest

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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
			fmt.Fprintf(w, `{"login":%q}`, account)
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
}
