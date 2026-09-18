package itest

import (
	"context"
	"testing"

	"aif/internal/config"
	"aif/internal/db"
	"aif/internal/harness"
)

const admin = "gatekeeper"

// --- the system account exists ---------------------------------------------

func TestSystemAccountSeededAndIdempotent(t *testing.T) {
	r := harness.New(t, false)
	if err := db.Init(context.Background(), r.Pool(), r.Cfg); err != nil { // init again
		t.Fatal(err)
	}
	rows, err := db.QueryRows(context.Background(), r.Pool(), "SELECT name, descr FROM agents")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 agent, got %d: %+v", len(rows), rows)
	}
	eqStr(t, db.AsString(rows[0], "name"), admin, "seeded agent name")
	contains(t, db.AsString(rows[0], "descr"), "system account", "descr")
}

func TestSystemAccountListedAsSpecialAgent(t *testing.T) {
	r := harness.New(t, false)
	r.Join("bot1")
	body := r.Admin.Get("/api/agents").JSON()
	top := body["a"].([]any)[0].(map[string]any)
	eqStr(t, top["n"].(string), admin, "top agent")
	eq(t, num(top["sys"]), 1.0, "sys flag")
	eq(t, num(top["on"]), 1.0, "admin always online")
	names := map[string]bool{}
	for _, a := range body["a"].([]any) {
		names[a.(map[string]any)["n"].(string)] = true
	}
	if !names[admin] || !names["bot1"] || len(names) != 2 {
		t.Fatalf("agent set: %v", names)
	}
	online := r.Admin.Get("/api/online").JSON()["on"].([]any)
	if !containsStr(online, admin) {
		t.Fatalf("gatekeeper not in online list: %v", online)
	}
}

func TestNoOneMayRegisterTheReservedName(t *testing.T) {
	r := harness.New(t, false)
	for _, cand := range []string{admin, "GateKeeper", "GATEKEEPER", " gatekeeper "} {
		res := r.Claim(cand, "")
		eq(t, res.Code, 403, "invite claim status for "+cand)
		eqStr(t, errCode(res), "name_reserved", "invite claim err for "+cand)
		direct := r.Admin.Post("/api/agents", map[string]any{"name": cand})
		eq(t, direct.Code, 403, "admin direct status for "+cand)
		eqStr(t, errCode(direct), "name_reserved", "admin direct err for "+cand)
	}
	eq(t, r.Claim("gate_keeper", "").Code, 200, "different name ok")
}

// --- acting as it ----------------------------------------------------------

func TestAgentTokenCannotActAsSystemAccount(t *testing.T) {
	r := harness.New(t, false)
	bot := r.Join("bot2")
	u := bot.H("x-agent", admin).Get("/api/unread")
	eq(t, u.Code, 403, "unread status")
	eqStr(t, errCode(u), "token_agent_mismatch", "unread err")
	p := bot.H("x-agent", admin).Post("/api/threads", map[string]any{"subject": "fake", "b": "x"})
	eq(t, p.Code, 403, "post status")
	eqStr(t, errCode(p), "token_agent_mismatch", "post err")

	spoof := mcpResult(t, mcp(r, r.Tokens["bot2"], "unread", map[string]any{"agent": admin}, nil))
	eq(t, spoof["isError"].(bool), true, "mcp spoof is error")
}

func TestGatekeeperTokenWithoutNameActsAsSystem(t *testing.T) {
	r := harness.New(t, false)
	made := r.Admin.Post("/api/threads", map[string]any{"subject": "pinned rules", "b": "read me"}).MustOK().JSON()
	eq(t, num(made["ok"]), 1.0, "ok")
	eqStr(t, r.Admin.Get("/api/messages/" + itoa(int64f(made["i"]))).JSON()["a"].(string), admin, "author")
}

func TestGatekeeperTokenMayActAsAnyAgent(t *testing.T) {
	r := harness.New(t, false)
	worker := r.Join("worker")
	tid := int64f(worker.Post("/api/threads", map[string]any{"subject": "work", "b": "yours"}).JSON()["t"])
	made := r.Admin.H("x-agent", "worker").Post("/api/threads/"+itoa(tid)+"/msgs", map[string]any{"b": "posted on your behalf"})
	eq(t, made.Code, 200, "relay status")
	eqStr(t, r.Admin.Get("/api/messages/" + itoa(int64f(made.JSON()["i"]))).JSON()["a"].(string), "worker", "relay author")
	ghost := r.Admin.H("x-agent", "ghost").Post("/api/threads", map[string]any{"subject": "as nobody", "b": "x"})
	eq(t, ghost.Code, 401, "ghost author status")
}

func TestSystemAccountReadableByAnyone(t *testing.T) {
	r := harness.New(t, false)
	reader := r.Join("reader")
	r.Admin.H("x-agent", admin).Post("/api/threads", map[string]any{"subject": "notice", "b": "from the server"})
	th := reader.Get("/api/threads?q=notice").JSON()["th"].([]any)[0].(map[string]any)
	tid := itoa(int64f(th["i"]))
	page := reader.Get("/api/threads/" + tid).JSON()
	eqStr(t, page["a"].(string), admin, "thread author")
	eqStr(t, page["ms"].([]any)[0].(map[string]any)["a"].(string), admin, "msg author")
}

// --- privileges ------------------------------------------------------------

func TestRelayedPostSaysWhoActuallyWroteIt(t *testing.T) {
	r := harness.New(t, false)
	gupta := r.Join("gupta")
	own := gupta.Post("/api/threads", map[string]any{"subject": "mine", "b": "my own work"}).MustOK().JSON()
	relay := r.Admin.H("x-agent", "gupta").Post("/api/threads", map[string]any{"subject": "relayed", "b": "sent for them"}).MustOK().JSON()

	mine := r.Admin.Get("/api/messages/" + itoa(int64f(own["i"]))).JSON()
	theirs := r.Admin.Get("/api/messages/" + itoa(int64f(relay["i"]))).JSON()
	eqStr(t, mine["a"].(string), "gupta", "mine author")
	eqStr(t, theirs["a"].(string), "gupta", "relay author")
	missing(t, mine, "via", "own post has no via")
	eqStr(t, theirs["via"].(string), admin, "relay via")
	long := r.Admin.Get("/api/messages/" + itoa(int64f(relay["i"])) + "?long=1").JSON()
	eqStr(t, long["written_by"].(string), admin, "long written_by")

	plain := r.Admin.Post("/api/threads", map[string]any{"subject": "service", "b": "notice"}).MustOK().JSON()
	missing(t, r.Admin.Get("/api/messages/"+itoa(int64f(plain["i"]))).JSON(), "via", "admin-as-self has no via")
}

func TestGatekeeperTokenMayDeleteAnything(t *testing.T) {
	r := harness.New(t, false)
	owner := r.Join("owner")
	r.Join("nosey")
	mid := int64f(owner.Post("/api/threads", map[string]any{"subject": "spam", "b": "x"}).JSON()["i"])
	tid := int64f(owner.Get("/api/messages/" + itoa(mid)).JSON()["t"])
	blocked := r.Client(r.Tokens["nosey"]).Post("/api/op", map[string]any{"do": "rm", "what": "thread", "id": tid})
	eq(t, blocked.Code, 403, "nosey rm status")
	eqStr(t, errCode(blocked), "not_yours", "nosey rm err")
	gone := r.Admin.H("x-agent", admin).Post("/api/op", map[string]any{"do": "rm", "what": "thread", "id": tid}).MustOK().JSON()
	eqStr(t, gone["gone"].(string), "thread:"+itoa(tid), "gone")
	eq(t, owner.Get("/api/threads/"+itoa(tid)).Code, 404, "thread gone")
}

func TestGatekeeperRegistersOnBehalfOfOthers(t *testing.T) {
	r := harness.New(t, false)
	one := r.Admin.Post("/api/agents", map[string]any{"name": "issued-one"}).MustOK().JSON()
	missing(t, one, "by", "admin-as-self register has no by")
	missing(t, one, "token", "admin-as-self register has no token")
	onBehalf := r.Admin.H("x-agent", "issued-one").Post("/api/agents", map[string]any{"name": "issued-two"}).MustOK().JSON()
	eqStr(t, onBehalf["by"].(string), "issued-one", "by")
	eqStr(t, errCode(r.Claim("issued-two", "")), "name_taken", "reclaim")
}

func TestOrdinaryAgentCannotRegisterSecondName(t *testing.T) {
	r := harness.New(t, false)
	onlyme := r.Join("onlyme")
	second := onlyme.Post("/api/agents", map[string]any{"name": "secondone"})
	eq(t, second.Code, 409, "second status")
	eqStr(t, errCode(second), "already_registered", "second err")
	anon := r.Client("").Post("/api/agents", map[string]any{"name": "thirdone"})
	eq(t, anon.Code, 401, "anon register status")
	eqStr(t, errCode(onlyme.H("x-agent", "").Post("/api/agents", map[string]any{"name": "thirdone"})), "already_registered", "empty x-agent")
}

func TestPingReportsWhoTheTokenThinksYouAre(t *testing.T) {
	r := harness.New(t, false)
	shown := r.Admin.Get("/api/ping").JSON()
	eqStr(t, shown["as"].(string), admin, "admin ping as")
	eq(t, num(shown["admin"]), 1.0, "admin flag")
	bot := r.Join("bot9")
	named := bot.Get("/api/ping").JSON()
	eqStr(t, named["as"].(string), "bot9", "bot ping as")
	missing(t, named, "admin", "agent ping has no admin key")
}

func TestMCPRespectsTheSamePrivileges(t *testing.T) {
	r := harness.New(t, false)
	sys := structured(t, mcpResult(t, mcp(r, harness.AdminToken, "post", map[string]any{"subject": "mcp notice", "b": "hi"}, nil)))
	eq(t, num(sys["ok"]), 1.0, "mcp admin post ok")
	r.Join("mcpbot")
	own := structured(t, mcpResult(t, mcp(r, r.Tokens["mcpbot"], "post", map[string]any{"subject": "mcp own", "b": "hi"}, nil)))
	eq(t, num(own["ok"]), 1.0, "mcp agent post ok")
	if int64f(own["i"]) == int64f(sys["i"]) {
		t.Fatalf("mcp posts reused message id")
	}
}

// --- config (pure) ---------------------------------------------------------

func TestAdminTokenDefaultsToServiceToken(t *testing.T) {
	cfg := &config.Config{Tokens: []string{"shared"}, TokenSalt: "s"}
	if !cfg.AdminTokenOK("shared") || !cfg.ConfigTokenOK("shared") {
		t.Fatal("admin token should alias the service token when no admin list is set")
	}
	multi := &config.Config{Tokens: []string{"a", "b"}, TokenSalt: "s"}
	if !multi.AdminTokenOK("a") || !multi.AdminTokenOK("b") {
		t.Fatal("all service tokens should be admin tokens")
	}
	if multi.ConfigTokenOK("nope") || multi.AdminTokenOK("nope") {
		t.Fatal("unknown token accepted")
	}
}

func TestInsecureDefaultsAreRefused(t *testing.T) {
	bad := &config.Config{Tokens: []string{config.DefaultToken}, TokenSalt: "real", DataDir: "/tmp/x"}
	if !hasError(bad.Validate()) {
		t.Fatal("insecure AIF_TOKEN must be refused")
	}
	empty := &config.Config{Tokens: []string{"real"}, TokenSalt: "", DataDir: "/tmp/x"}
	if !hasError(empty.Validate()) {
		t.Fatal("empty salt must be refused")
	}
	defSalt := &config.Config{Tokens: []string{"real"}, TokenSalt: config.DefaultSalt, DataDir: "/tmp/x"}
	if !hasError(defSalt.Validate()) {
		t.Fatal("insecure AIF_TOKEN_SALT must be refused")
	}
	ok := &config.Config{Tokens: []string{"real"}, TokenSalt: "real", DataDir: "/tmp/x"}
	if hasError(ok.Validate()) {
		t.Fatalf("valid config rejected: %v", ok.Validate())
	}
}
