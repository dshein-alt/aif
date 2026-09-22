package itest

import (
	"context"
	"testing"

	"github.com/dshein-alt/aif/internal/db"
	"github.com/dshein-alt/aif/internal/harness"
)

func tokenNamedView(t *testing.T, r *harness.Rig, name string, dead bool) map[string]any {
	t.Helper()
	args := map[string]any{"name": name}
	if dead {
		args["dead"] = 1
	}
	tl := r.Admin.Op("tokens", args).MustOK().JSON()
	for _, raw := range tl["tk"].([]any) {
		if v := raw.(map[string]any); v["name"] == name {
			return v
		}
	}
	return nil
}

func TestIssueNamedInviteWindowAndBinding(t *testing.T) {
	r := harness.New(t, false)

	res := r.Admin.Op("issue", map[string]any{"name": "bot1", "days": 30, "descr": "hire"}).MustOK().JSON()
	if res["name"] != "bot1" {
		t.Fatalf("issued name = %v", res["name"])
	}
	if _, ok := res["invite"]; ok {
		t.Error("a named invite must not be flagged as an un-named invite")
	}
	if e, ok := res["exp"]; !ok || e.(float64) == 0 {
		t.Errorf("named invite should carry an expiry, got %v", res["exp"])
	}

	view := tokenNamedView(t, r, "bot1", false)
	if view == nil {
		t.Fatal("bot1 missing from the token tree")
	}
	if num(view["claimed"]) != 0 {
		t.Error("a freshly issued named invite is unclaimed")
	}
	if num(view["left"]) <= 0 {
		t.Errorf("a live expiring token should report seconds left, got %v", view["left"])
	}
	if view["root"] == nil {
		t.Error("tokenView should always report root")
	}

	// Re-issuing a live, unclaimed named invite collides.
	if errCode(r.Admin.Op("issue", map[string]any{"name": "bot1"})) != "name_bound" {
		t.Error("re-issue of a live named invite must be name_bound")
	}
	// days without a name is rejected.
	if errCode(r.Admin.Op("issue", map[string]any{"days": 5})) != "bad_request" {
		t.Error("days without a name must be rejected")
	}

	// Claiming bot1 registers it. Only the gatekeeper may then re-bind a registered name.
	invite := res["token"].(string)
	r.Client(invite).Post("/api/agents", map[string]any{"name": "bot1"}).MustOK()
	if errCode(r.Admin.Op("issue", map[string]any{"name": "bot1"})) != "" {
		t.Error("gatekeeper recovery re-binding a registered name should be allowed")
	}
	boss := r.Join("boss")
	if errCode(boss.Op("issue", map[string]any{"name": "bot1"})) != "name_registered" {
		t.Error("a non-admin may not bind a token to a registered name")
	}
}

func TestNamedInviteExpiredIsReissuable(t *testing.T) {
	r := harness.New(t, false)
	tok := r.Issue("temp", map[string]any{"days": 1})
	// Force the invite to be expired directly (no clock hook in the rig).
	if _, err := r.Pool().Exec(context.Background(), "UPDATE tokens SET exp = 1 WHERE self_token = $1", tok); err != nil {
		t.Fatalf("expire invite: %v", err)
	}
	// It no longer counts as "live", so the same name can be bound again.
	if err := errCode(r.Admin.Op("issue", map[string]any{"name": "temp"})); err != "" {
		t.Fatalf("re-issue after expiry should succeed, got %v", err)
	}
}

func TestRevokeCascadeAncestryAndGatekeeper(t *testing.T) {
	r := harness.New(t, false)

	parent := r.Join("parent")
	kidInvite := parent.Op("issue", map[string]any{"name": "kid", "days": 30}).MustOK().Field("token").(string)
	kidToken := r.Client(kidInvite).Post("/api/agents", map[string]any{"name": "kid"}).MustOK().Field("token").(string)
	kid := r.Client(kidToken)

	// An unrelated agent may not revoke outside its subtree.
	other := r.Join("other")
	if errCode(other.Op("revoke", map[string]any{"name": "kid"})) != "cannot_revoke" {
		t.Error("a non-ancestor must not be able to revoke")
	}

	// The ancestor can, and reports the cascade.
	res := parent.Op("revoke", map[string]any{"name": "kid"}).MustOK().JSON()
	if num(res["revoked"]) < 1 {
		t.Fatalf("revoke reported %v affected", res["revoked"])
	}
	if errCode(kid.Op("ping", nil)) != "token_revoked" {
		t.Error("a revoked token must stop authenticating")
	}

	// revoke needs a name or tk.
	if errCode(parent.Op("revoke", map[string]any{})) != "bad_request" {
		t.Error("revoke with neither name nor tk should be rejected")
	}
	// An unknown token is a 404 no_token.
	if errCode(r.Admin.Op("revoke", map[string]any{"tk": "aif_does_not_exist"})) != "no_token" {
		t.Error("revoking an unknown tk should be no_token")
	}

	// The gatekeeper can revoke anything.
	if err := errCode(r.Admin.Op("revoke", map[string]any{"name": "parent"})); err != "" {
		t.Fatalf("gatekeeper revoke failed: %v", err)
	}
}

func TestTokensVisibilityPagingAndDead(t *testing.T) {
	r := harness.New(t, false)

	r.Admin.Op("issue", map[string]any{"name": "secret1", "days": 30}).MustOK()
	if tokenNamedView(t, r, "secret1", true) == nil {
		t.Fatal("gatekeeper should see its own invite")
	}

	// A different agent's tree must not contain the gatekeeper's unrelated invite.
	a := r.Join("a")
	mine := a.Op("tokens", map[string]any{"dead": 1}).MustOK().JSON()
	for _, raw := range mine["tk"].([]any) {
		if v := raw.(map[string]any); v["name"] == "secret1" {
			t.Fatal("agent a must not see the gatekeeper's invite 'secret1'")
		}
	}

	// Paging over the whole forest (extra un-named invites, distinct rows).
	for i := 0; i < 3; i++ {
		r.Issue("")
	}
	pg := r.Admin.Op("tokens", map[string]any{"limit": 2}).MustOK().JSON()
	if num(pg["n"]) != 2 {
		t.Errorf("page size = %v, want 2", pg["n"])
	}
	if num(pg["total"]) < 3 {
		t.Errorf("total = %v, want >=3", pg["total"])
	}
	if _, ok := pg["next_offset"]; !ok {
		t.Error("a truncated page should report next_offset")
	}
	off := r.Admin.Op("tokens", map[string]any{"limit": 2, "offset": 2}).MustOK().JSON()
	if num(off["offset"]) != 2 {
		t.Errorf("offset echo = %v", off["offset"])
	}

	// Revoked rows are hidden unless dead=1.
	r.Admin.Op("revoke", map[string]any{"name": "secret1"}).MustOK()
	if len(r.Admin.Op("tokens", map[string]any{"name": "secret1"}).MustOK().JSON()["tk"].([]any)) != 0 {
		t.Error("a revoked token should be hidden without dead=1")
	}
	rows := r.Admin.Op("tokens", map[string]any{"name": "secret1", "dead": 1}).MustOK().JSON()["tk"].([]any)
	if len(rows) == 0 || num(rows[0].(map[string]any)["revoked"]) != 1 {
		t.Errorf("dead=1 should surface the revoked row, got %v", rows)
	}
}

// canonLow reads the stored canonical column for a token row, straight from Postgres.
func canonLow(t *testing.T, r *harness.Rig, selfToken string) string {
	t.Helper()
	row, err := db.QueryOne(context.Background(), r.Pool(), "SELECT low, name FROM tokens WHERE self_token = ?", selfToken)
	if err != nil || row == nil {
		t.Fatalf("no token row for %q (err %v)", selfToken, err)
	}
	return db.AsString(row, "low")
}

// A named invite is the only shape a long-lived MCP client can use: its token must be the same
// string before and after the claim, whatever case the claimer types, or the client 403s on its
// very next call with nothing in the error to explain it.
func TestNamedInviteKeepsItsTokenAcrossClaim(t *testing.T) {
	r := harness.New(t, false)

	invite := r.Issue("mixedbot")
	eqStr(t, canonLow(t, r, invite), "mixedbot", "issue must store the canonical name in tokens.low")

	// Claim with a different case; the binding check accepts it, so the token must not move.
	res := r.Client(invite).Post("/api/agents", map[string]any{"name": "MixedBot"})
	res.MustOK()
	final, _ := res.Field("token").(string)
	eqStr(t, final, invite, "a named invite must hand back the token the client already holds")

	// The original bearer still authenticates, which is the whole point.
	r.Client(invite).Op("ping", nil).MustOK()
	eqStr(t, canonLow(t, r, invite), "mixedbot", "claim must keep tokens.low canonical")

	// An un-named invite keeps the opposite contract: claiming it mints a new token.
	open := r.Issue("")
	res2 := r.Client(open).Post("/api/agents", map[string]any{"name": "OtherBot"})
	res2.MustOK()
	fresh, _ := res2.Field("token").(string)
	if fresh == "" || fresh == open {
		t.Errorf("an un-named invite must mint a new token, got %q (invite %q)", fresh, open)
	}
	eqStr(t, canonLow(t, r, fresh), "otherbot", "claiming an un-named invite must store the canonical name")
}

// tokens.low is the canonical column, so every lookup through it has to be case-blind.
func TestTokenLookupsAreCaseInsensitive(t *testing.T) {
	r := harness.New(t, false)
	r.Issue("CaseBot")

	// The duplicate-invite guard reads tokens.low; a differently-cased name is the same name.
	if got := errCode(r.Admin.Op("issue", map[string]any{"name": "casebot"})); got != "name_bound" {
		t.Errorf("re-issue of a live invite under another case = %q, want name_bound", got)
	}
	// tokens {name:...} filters on the same column.
	if tokenNamedView(t, r, "CaseBot", false) == nil {
		t.Error("the issued token is missing from its own token tree")
	}
	// revoke by name queried the canonical column with the raw spelling, so this used to 404.
	r.Admin.Op("revoke", map[string]any{"name": "casebot"}).MustOK()
	if got := errCode(r.Admin.Op("revoke", map[string]any{"name": "CASEBOT"})); got != "no_token" {
		t.Errorf("revoking an already-revoked token = %q, want no_token", got)
	}
}

// A named invite is meant to be dropped into a client config and just work: its first request
// self-registers the name, ping already answers "as" it, and a later explicit register is a no-op
// that returns the same token.
func TestNamedInviteSelfClaimsOnFirstUse(t *testing.T) {
	r := harness.New(t, false)
	invite := r.Issue("dropin", map[string]any{"descr": "resident"})
	bot := r.Client(invite)

	ping := bot.Op("ping", nil).MustOK().JSON()
	eqStr(t, ping["as"].(string), "dropin", "first ping must already answer as the bound name")
	bot.Op("post", map[string]any{"subject": "hello", "b": "first"}).MustOK()

	who := r.Admin.Op("who", map[string]any{"on": 0, "q": "dropin"}).MustOK().JSON()
	if len(who["a"].([]any)) != 1 {
		t.Fatalf("self-claim must register the agent, who = %v", who)
	}

	again := bot.Post("/api/agents", map[string]any{"name": "DropIn"})
	again.MustOK()
	tok, _ := again.Field("token").(string)
	eqStr(t, tok, invite, "register after self-claim must hand back the same token")
	eqStr(t, errCode(bot.Post("/api/agents", map[string]any{"name": "other"})), "already_registered", "a different name is still refused")
}
