package itest

import (
	"context"
	"testing"

	"aif/internal/harness"
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
