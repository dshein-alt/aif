package httpx

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	giteaLoginPath    = "/ui/gitea/login"
	giteaCallbackPath = "/ui/gitea/callback"
	giteaStateCookie  = "aif_gitea_state"
	giteaStateKind    = "gitea-state"
	giteaSessionKind  = "gitea"
	giteaStateTTL     = 300
)

var giteaLoginName = regexp.MustCompile("^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$")

type giteaAccount struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

var giteaHTTPClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func (a *App) giteaConfigDigest() string {
	g := a.cfg.GiteaOAuth
	return tokenDigest(strings.Join([]string{g.URL, g.ClientID, g.ClientSecret, a.cfg.PublicURL}, "\x00"))
}

func (a *App) giteaUICookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: giteaStateCookie, Value: value, MaxAge: maxAge, Path: giteaCallbackPath,
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: strings.HasPrefix(a.cfg.PublicURL, "https://"),
	})
}

func (a *App) handleGiteaLogin(w http.ResponseWriter, req *http.Request) {
	g := a.cfg.GiteaOAuth
	state := rand.Text()
	var entropy [32]byte
	_, _ = rand.Read(entropy[:])
	verifier := base64.RawURLEncoding.EncodeToString(entropy[:])
	challenge := sha256.Sum256([]byte(verifier))
	next := cleanNext(req.URL.Query().Get("next"))
	if len(next) > 1024 {
		next = "/ui"
	}
	subject := state + "." + verifier + "." + base64.RawURLEncoding.EncodeToString([]byte(next))
	expires := time.Now().Unix() + giteaStateTTL
	a.giteaUICookie(w, sessionValue(g.ClientSecret, giteaStateKind, subject, expires), giteaStateTTL)

	params := url.Values{
		"client_id":             {g.ClientID},
		"redirect_uri":          {a.cfg.PublicURL + giteaCallbackPath},
		"response_type":         {"code"},
		"scope":                 {"read:user"},
		"state":                 {state},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, req, g.URL+"/login/oauth/authorize?"+params.Encode(), http.StatusSeeOther)
}

func (a *App) handleGiteaCallback(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	a.giteaUICookie(w, "", -1)

	g := a.cfg.GiteaOAuth
	query := req.URL.Query()
	now := float64(time.Now().Unix())
	var pending *uiSession
	if cookie, err := req.Cookie(giteaStateCookie); err == nil {
		pending = signedSession(cookie.Value, g.ClientSecret, now)
	}
	if pending == nil || pending.Kind != giteaStateKind {
		a.loginPage(w, "/ui", "The Gitea sign-in expired. Please try again.", http.StatusBadRequest)
		return
	}
	parts := strings.SplitN(pending.Subject, ".", 3)
	if len(parts) != 3 || !hmac.Equal([]byte(query.Get("state")), []byte(parts[0])) {
		a.loginPage(w, "/ui", "The Gitea sign-in could not be verified. Please try again.", http.StatusBadRequest)
		return
	}
	rawNext, _ := base64.RawURLEncoding.DecodeString(parts[2])
	next := cleanNext(string(rawNext))
	code := query.Get("code")
	if query.Get("error") != "" || code == "" {
		a.loginPage(w, next, "Gitea did not authorize this sign-in.", http.StatusForbidden)
		return
	}

	ctx, cancel := context.WithTimeout(req.Context(), 8*time.Second)
	defer cancel()
	account := a.giteaUser(ctx, code, parts[1])
	if account.ID == 0 {
		a.loginPage(w, next, "Gitea could not confirm your sign-in. Try again or use an AIF token.", http.StatusBadGateway)
		return
	}

	salt := a.sessionSalt(req.Context(), a.pool, true)
	if salt == "" {
		a.loginPage(w, next, "AIF could not start your browser session. Please try again.", http.StatusServiceUnavailable)
		return
	}
	now = float64(time.Now().Unix())
	expires := int64(now) + int64(a.cfg.UISessionTTL)
	subject := a.giteaConfigDigest() + "#" + strconv.FormatInt(account.ID, 10) + "#" + account.Login
	value := sessionValue(salt, giteaSessionKind, subject, expires)
	a.setUICookie(w, value, expires, now, strings.HasPrefix(a.cfg.PublicURL, "https://"))
	http.Redirect(w, req, next, http.StatusSeeOther)
}

func (a *App) giteaUser(ctx context.Context, code, verifier string) giteaAccount {
	g := a.cfg.GiteaOAuth
	form := url.Values{
		"client_id":     {g.ClientID},
		"client_secret": {g.ClientSecret},
		"code":          {code},
		"code_verifier": {verifier},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {a.cfg.PublicURL + giteaCallbackPath},
	}
	var exchange struct {
		AccessToken string `json:"access_token"`
	}
	if giteaRequest(ctx, g.URL+"/login/oauth/access_token", form, "", &exchange) != http.StatusOK || exchange.AccessToken == "" {
		return giteaAccount{}
	}

	var user giteaAccount
	if giteaRequest(ctx, g.URL+"/api/v1/user", nil, exchange.AccessToken, &user) != http.StatusOK ||
		user.ID <= 0 || !giteaLoginName.MatchString(user.Login) {
		return giteaAccount{}
	}
	return user
}

func giteaRequest(ctx context.Context, endpoint string, form url.Values, token string, result any) int {
	method := http.MethodGet
	var body io.Reader
	if form != nil {
		method = http.MethodPost
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return http.StatusBadGateway
	}
	req.Header.Set("Accept", "application/json")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := giteaHTTPClient.Do(req)
	if err != nil {
		return http.StatusBadGateway
	}
	defer res.Body.Close()
	if result != nil && res.StatusCode == http.StatusOK {
		if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(result); err != nil {
			return http.StatusBadGateway
		}
	}
	return res.StatusCode
}
