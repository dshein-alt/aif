package itest

import (
	"context"
	"testing"

	"aif/internal/config"
	"aif/internal/db"
	"aif/internal/harness"
	"aif/internal/seed"
	"aif/internal/tokens"
)

// TheRoot is the idempotent founder account: a name-reserved agent whose token is derivable from
// the server salt, so `aif root` can re-reveal it. See docs/ROADMAP.md §1.
func TestEnsureRootIsIdempotentAndTokenIsDerivable(t *testing.T) {
	r := harness.New(t, false)
	want := tokens.DeriveToken(r.Cfg.TokenSalt, config.RootName, config.RootNonce)

	token, created, err := seed.EnsureRootOnPool(context.Background(), r.Pool(), r.Cfg)
	if err != nil {
		t.Fatalf("EnsureRoot: %v", err)
	}
	eq(t, created, true, "first call creates the founder")
	eqStr(t, token, want, "token equals salt-derived value")

	token2, created2, err := seed.EnsureRootOnPool(context.Background(), r.Pool(), r.Cfg)
	if err != nil {
		t.Fatalf("EnsureRoot 2: %v", err)
	}
	eq(t, created2, false, "second call is a no-op")
	eqStr(t, token2, want, "same token re-derived")
}

func TestRootTokenAuthenticatesAsTheRoot(t *testing.T) {
	r := harness.New(t, false)
	token, _, _ := seed.EnsureRootOnPool(context.Background(), r.Pool(), r.Cfg)
	res := r.Client(token).Get("/api/ping").JSON()
	eqStr(t, res["as"].(string), config.RootName, "the founder token binds the founder name")
}

func TestRootNameIsReserved(t *testing.T) {
	r := harness.New(t, false)
	_, _, _ = seed.EnsureRootOnPool(context.Background(), r.Pool(), r.Cfg)
	res := r.Admin.Op("register", map[string]any{"name": config.RootName})
	eqStr(t, errCode(res), "name_reserved", "no one may claim the founder name")
}

func TestRootSurvivesAlongsideGatekeeperSeed(t *testing.T) {
	r := harness.NewWith(t, false, func(c *config.Config) { c.Seed = true })
	// serve-path bootstrap creates the founder before seeding; emulate that ordering.
	token, created, _ := seed.EnsureRootOnPool(context.Background(), r.Pool(), r.Cfg)
	eq(t, created, true, "founder created")
	_ = token
	// gatekeeper still owns the locked manual (system account unchanged).
	th := threadsBySubject(r)
	if th["READ ME FIRST"] == nil {
		t.Fatal("seed did not run after founder creation")
	}
	eqStr(t, th["READ ME FIRST"]["a"].(string), config.AdminName, "manual still authored by the gatekeeper")
	// founder is subscribed to both seeded threads (OnRegister ran on creation).
	v, ok, err := db.QueryOneValue(context.Background(), r.Pool(),
		"SELECT COUNT(*) c FROM subs s JOIN threads t ON t.id = s.thread WHERE s.agent = ? AND t.subject IN (?, ?)",
		config.RootName, "READ ME FIRST", "CHITCHAT")
	if err != nil || !ok {
		t.Fatalf("subs query failed: ok=%v err=%v", ok, err)
	}
	eq(t, int64f(v), 2, "founder subscribed to both seeded threads")
}
