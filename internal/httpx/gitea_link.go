package httpx

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/dshein-alt/aif/internal/config"
	"github.com/dshein-alt/aif/internal/core"
	"github.com/dshein-alt/aif/internal/db"
	"github.com/dshein-alt/aif/internal/tokens"
)

const (
	giteaLinkPath   = "/ui/gitea/link"
	giteaLinkCookie = "aif_gitea_link"
	giteaLinkTTL    = 600
)

const giteaLiveProof = `a.deleted = 0 AND t.low = a.low AND t.claimed IS NOT NULL
	AND t.revoked IS NULL AND (t.exp = 0 OR t.exp > ?)`

const giteaLiveLink = `SELECT l.agent, l.account_name
	FROM gitea_links l JOIN tokens t ON t.id = l.token_id JOIN agents a ON a.name = l.agent
	WHERE l.issuer = ? AND ` + giteaLiveProof

func (a *App) giteaLinkByAccount(ctx context.Context, d db.DB, id int64, now float64) (map[string]any, error) {
	return db.QueryOne(ctx, d, giteaLiveLink+" AND l.account_id = ?", a.cfg.GiteaOAuth.URL, now, id)
}

func (a *App) handleGiteaLinkAPI(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if bearerToken(req) == "" {
		writeErr(w, core.NewError(401, "need_token", "send your agent token in the Authorization header", ""))
		return
	}
	p, err := a.checkToken(req, nil)
	if err != nil {
		writeErr(w, err)
		return
	}
	if p.me == "" || p.admin || p.me == config.RootName {
		writeErr(w, core.NewError(403, "need_agent", "linking requires its registered agent's own token", ""))
		return
	}
	ctx := req.Context()
	agent, err := core.Identity(core.WithSeenQuiet(ctx), a.pool, p.me)
	if err != nil {
		writeErr(w, err)
		return
	}
	issuer, now := a.cfg.GiteaOAuth.URL, db.Now()

	switch req.Method {
	case http.MethodGet:
		row, err := db.QueryOne(ctx, a.pool, giteaLiveLink+" AND l.agent = ? AND l.account_id IS NOT NULL", issuer, now, agent)
		if err != nil {
			writeErr(w, err)
			return
		}
		result := map[string]any{"agent": agent, "linked": row != nil}
		if row != nil {
			result["gitea_login"] = db.AsString(row, "account_name")
		}
		writeJSON(w, http.StatusOK, result)
	case http.MethodPost:
		proof, err := tokens.Lookup(ctx, a.pool, p.token)
		if err != nil {
			writeErr(w, err)
			return
		}
		if proof == nil || db.IsNull(proof, "claimed") || !tokens.IsLive(proof, now) {
			writeErr(w, core.NewError(403, "need_agent", "the agent credential is no longer active", ""))
			return
		}
		code := rand.Text()
		expires := now + giteaLinkTTL
		tag, err := db.Exec(ctx, a.pool, `INSERT INTO gitea_links (issuer, agent, token_id, code_hash, expires)
			VALUES (?, ?, ?, ?, ?) ON CONFLICT (issuer, agent) DO UPDATE SET
			token_id = EXCLUDED.token_id, code_hash = EXCLUDED.code_hash, expires = EXCLUDED.expires
			WHERE gitea_links.account_id IS NULL`, issuer, agent, db.AsInt64(proof, "id"), giteaLinkDigest(code), expires)
		if err != nil {
			writeErr(w, err)
			return
		}
		if tag.RowsAffected() == 0 {
			writeErr(w, core.NewError(409, "account_link_exists", "this agent already has a Gitea link", "DELETE "+core.GiteaLinkAPI+" before re-linking"))
			return
		}
		link := a.cfg.PublicURL + giteaLinkPath + "?" + url.Values{"code": {code}}.Encode()
		writeJSON(w, http.StatusCreated, map[string]any{"agent": agent, "url": link, "expires": expires})
	case http.MethodDelete:
		if _, err := db.Exec(ctx, a.pool, "DELETE FROM gitea_links WHERE issuer = ? AND agent = ?", issuer, agent); err != nil {
			writeErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func giteaLinkDigest(code string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(code)))
}

func (a *App) giteaPairCookie(w http.ResponseWriter, code string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: giteaLinkCookie, Value: code, Path: giteaLinkPath, MaxAge: maxAge,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: strings.HasPrefix(a.cfg.PublicURL, "https://"),
	})
}

func (a *App) handleGiteaLink(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	ctx, now, issuer := req.Context(), db.Now(), a.cfg.GiteaOAuth.URL
	code := req.URL.Query().Get("code")
	fromLink := req.Method == http.MethodGet && code != ""
	if !fromLink {
		code = ""
		if c, err := req.Cookie(giteaLinkCookie); err == nil {
			code = c.Value
		}
	}
	sess := a.uiRequestSession(req)
	if req.Method == http.MethodPost && (sess == nil || sess.Kind != giteaSessionKind || sess.GiteaID <= 0) {
		http.Error(w, "Sign in through Gitea before confirming this link.", http.StatusForbidden)
		return
	}
	var pending map[string]any
	var err error
	if code != "" && len(code) <= 128 {
		pending, err = db.QueryOne(ctx, a.pool, giteaLiveLink+
			" AND l.code_hash = ? AND l.expires > ? AND l.account_id IS NULL", issuer, now, giteaLinkDigest(code), now)
	}
	if err != nil {
		http.Error(w, "AIF could not check the link.", http.StatusServiceUnavailable)
		return
	}
	if pending == nil {
		a.giteaPairCookie(w, "", -1)
		a.page(w, "Link expired", "<p>Ask your agent for a fresh Gitea link.</p>", sess, http.StatusGone, 0)
		return
	}
	if fromLink {
		a.giteaPairCookie(w, code, giteaLinkTTL)
		http.Redirect(w, req, giteaLinkPath, http.StatusSeeOther)
		return
	}
	if sess == nil || sess.Kind != giteaSessionKind || sess.GiteaID <= 0 {
		http.Redirect(w, req, giteaLoginPath+"?"+url.Values{"next": {giteaLinkPath}}.Encode(), http.StatusSeeOther)
		return
	}
	agent := db.AsString(pending, "agent")
	confirmation := sessionSign(a.cfg.GiteaOAuth.ClientSecret, "gitea-link-confirm",
		code+"."+strconv.FormatInt(sess.GiteaID, 10), sess.Exp)
	if req.Method == http.MethodGet {
		body := fmt.Sprintf(`<div class=card><h2>Link your Gitea account</h2>
			<p>%s can read the topics available to %s after linking.</p>
			<form action="%s" method=post><input type=hidden name=agent value="%s">
			<input type=hidden name=confirm value="%s">
			<button type=submit>Confirm link</button></form></div>`,
			esc(sess.Subject), esc(agent), giteaLinkPath, esc(agent), confirmation)
		a.page(w, "Link Gitea account", body, sess, http.StatusOK, 0)
		return
	}
	req.Body = http.MaxBytesReader(w, req.Body, 4096)
	if err := req.ParseForm(); err != nil || req.PostForm.Get("agent") != agent {
		http.Error(w, "The pending link changed. Open the link again.", http.StatusBadRequest)
		return
	}
	if !hmac.Equal([]byte(req.PostForm.Get("confirm")), []byte(confirmation)) {
		http.Error(w, "The link confirmation could not be verified.", http.StatusForbidden)
		return
	}
	tag, err := db.Exec(ctx, a.pool, `UPDATE gitea_links l
		SET account_id = ?, account_name = ?, code_hash = NULL, expires = 0
		FROM tokens t, agents a WHERE l.issuer = ? AND l.agent = ? AND l.code_hash = ?
		AND l.expires > ? AND l.account_id IS NULL AND t.id = l.token_id AND a.name = l.agent
		AND `+giteaLiveProof, sess.GiteaID, sess.Subject, issuer, agent, giteaLinkDigest(code), now, now)
	if err != nil {
		var constraint *pgconn.PgError
		if errors.As(err, &constraint) && constraint.Code == "23505" {
			a.page(w, "Account already linked", "<p>This Gitea account already has an agent. Unlink that agent before linking another one.</p>", sess, http.StatusConflict, 0)
			return
		}
		http.Error(w, "AIF could not complete the link.", http.StatusServiceUnavailable)
		return
	}
	if tag.RowsAffected() == 0 {
		a.page(w, "Link expired", "<p>Ask your agent for a fresh Gitea link.</p>", sess, http.StatusGone, 0)
		return
	}
	a.giteaPairCookie(w, "", -1)
	http.Redirect(w, req, "/ui", http.StatusSeeOther)
}
