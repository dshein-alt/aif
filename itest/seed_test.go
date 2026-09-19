package itest

import (
	"context"
	"strings"
	"testing"

	"github.com/dshein-alt/aif/internal/config"
	"github.com/dshein-alt/aif/internal/db"
	"github.com/dshein-alt/aif/internal/harness"
	"github.com/dshein-alt/aif/internal/seed"
)

// seededRig builds a rig with the seed switch on (as a normal serve would).
func seededRig(t *testing.T) *harness.Rig {
	return harness.NewWith(t, false, func(c *config.Config) { c.Seed = true })
}

func TestSeededManualIsThreadOneAndOrderedFirst(t *testing.T) {
	r := seededRig(t)
	// READ ME FIRST is created first (id 1), CHITCHAT second (id 2).
	th := threadsBySubject(r)
	eq(t, int64f(th["READ ME FIRST"]["i"]), 1, "READ ME FIRST is thread 1")
	eq(t, int64f(th["CHITCHAT"]["i"]), 2, "CHITCHAT is thread 2")

	// A very recent noise thread must NOT interleave into the pinned top two, and must not
	// reorder them: the manual stays above CHITCHAT even though CHITCHAT is less active.
	r.Join("busy")
	busy := r.Client(r.Tokens["busy"])
	for _, n := range []string{"a", "b", "c"} {
		busy.Post("/api/threads", map[string]any{"subject": "noise " + n, "b": "x"}).MustOK()
	}
	// bump CHITCHAT's activity so an activity-ordered pinned group would float it above the manual
	busy.Post("/api/threads/2/msgs", map[string]any{"b": "latest ever"}).MustOK()

	subjects := []string{}
	for _, row := range r.Admin.Get("/api/threads?limit=100").JSON()["th"].([]any) {
		subjects = append(subjects, row.(map[string]any)["s"].(string))
	}
	eq(t, len(subjects), 5, "two pinned + three noise")
	eqStr(t, subjects[0], "READ ME FIRST", "manual is first")
	eqStr(t, subjects[1], "CHITCHAT", "chitchat is second (pinned order is stable, not by activity)")
	// the three noise threads fill the tail by active-desc: c (newest) then b then a
	for i, want := range []string{"noise c", "noise b", "noise a"} {
		eqStr(t, subjects[2+i], want, "tail ordered by latest message desc")
	}
}

func threadsBySubject(r *harness.Rig) map[string]map[string]any {
	th := r.Admin.Get("/api/threads?limit=100").JSON()["th"].([]any)
	out := map[string]map[string]any{}
	for _, t := range th {
		m := t.(map[string]any)
		out[m["s"].(string)] = m
	}
	return out
}

func TestBothThreadsExistAuthoredByTheGatekeeper(t *testing.T) {
	r := seededRig(t)
	th := threadsBySubject(r)
	if len(th) != 2 {
		t.Fatalf("expected 2 seeded threads, got %v", keys(mapAny(th)))
	}
	readme, chitchat := th["READ ME FIRST"], th["CHITCHAT"]
	if readme == nil || chitchat == nil {
		t.Fatalf("missing a seeded thread: %v", keys(mapAny(th)))
	}
	eqStr(t, readme["a"].(string), admin, "readme author")
	eq(t, num(readme["lck"]), 1.0, "readme locked")
	eqStr(t, chitchat["a"].(string), admin, "chitchat author")
	missing(t, chitchat, "lck", "chitchat not locked")
}

func TestReadmeIsALockedManualPointingAtChitchat(t *testing.T) {
	r := seededRig(t)
	readme := threadsBySubject(r)["READ ME FIRST"]
	page := r.Admin.Get("/api/threads/" + itoa(int64f(readme["i"]))).JSON()
	pin := page["pin"].(map[string]any)
	contains(t, pin["b"].(string), "CHITCHAT", "manual points at chitchat")
	contains(t, pin["b"].(string), "GET /api/skill", "manual points at skill")
	contains(t, pin["b"].(string), "pin", "manual states the pin rule")
	eqStr(t, pin["a"].(string), admin, "manual author")
}

func TestChitchatOpensWithAWelcome(t *testing.T) {
	r := seededRig(t)
	chitchat := threadsBySubject(r)["CHITCHAT"]
	page := r.Admin.Get("/api/threads/" + itoa(int64f(chitchat["i"]))).JSON()
	pin := page["pin"].(map[string]any)
	eqStr(t, pin["a"].(string), admin, "welcome author")
	contains(t, strings.ToLower(pin["b"].(string)), "broadcast", "welcome says broadcast")
	contains(t, pin["b"].(string), "subscribed", "welcome says subscribed")
}

func TestSeedingIsIdempotent(t *testing.T) {
	r := seededRig(t)
	before := num(r.Admin.Get("/api/threads?limit=100").JSON()["n"])
	if _, err := seed.Run(context.Background(), r.Pool(), r.Cfg); err != nil { // a second run
		t.Fatal(err)
	}
	after := num(r.Admin.Get("/api/threads?limit=100").JSON()["n"])
	eq(t, after, before, "thread count unchanged")
	eq(t, after, 2.0, "exactly two seeded threads")
	c, _, _ := db.QueryOneValue(context.Background(), r.Pool(), "SELECT COUNT(*) c FROM messages")
	eq(t, int64f(c), 2, "exactly two seeded messages")
}

func TestRegistrationFollowsYouIntoBothThreads(t *testing.T) {
	r := seededRig(t)
	r.Join("newbie")
	nb := r.Client(r.Tokens["newbie"])
	th := threadsBySubject(r)
	unread := nb.Get("/api/unread").JSON()["ms"].([]any)
	eq(t, len(unread), 1, "one join-time duty (the manual)")
	eq(t, int64f(unread[0].(map[string]any)["i"]), int64f(th["READ ME FIRST"]["seq"]), "duty is the manual")
	eq(t, num(nb.Get("/api/unread").JSON()["n"]), 0.0, "then nothing")
	subs := map[string]bool{}
	for _, s := range nb.Get("/api/sub").JSON()["su"].([]any) {
		subs[s.(map[string]any)["s"].(string)] = true
	}
	if !subs["READ ME FIRST"] || !subs["CHITCHAT"] || len(subs) != 2 {
		t.Fatalf("subscriptions: %v", subs)
	}
}

func TestChitchatReachesEveryAgent(t *testing.T) {
	r := seededRig(t)
	for _, name := range []string{"one", "two"} {
		r.Join(name)
		r.Client(r.Tokens[name]).Get("/api/unread") // clear the manual duty
	}
	th := threadsBySubject(r)
	one := r.Client(r.Tokens["one"])
	one.Post("/api/threads/"+itoa(int64f(th["CHITCHAT"]["i"]))+"/msgs", map[string]any{"b": "service notice"}).MustOK()
	poll := r.Client(r.Tokens["two"]).Get("/api/poll").JSON()
	eq(t, num(poll["n"]), 1.0, "one new for two")
	tharr := poll["th"].([]any)
	eq(t, int64f(tharr[0].(map[string]any)["i"]), int64f(th["CHITCHAT"]["i"]), "poll thread")
	eq(t, num(tharr[0].(map[string]any)["un"]), 1.0, "one unread in chitchat")
	ms := r.Client(r.Tokens["two"]).Get("/api/unread").JSON()["ms"].([]any)
	eqStr(t, ms[0].(map[string]any)["b"].(string), "service notice", "the notice")
	eq(t, num(one.Get("/api/poll").JSON()["n"]), 0.0, "your own shout is not news")
}

func TestAgentsRegisteredBeforeSeedingGetBackfilled(t *testing.T) {
	r := harness.New(t, false) // seed off
	r.Join("old-timer")
	eq(t, num(r.Admin.Get("/api/threads").JSON()["n"]), 0.0, "no threads before seeding")
	r.Cfg.Seed = true                                                          // seed regardless of the original switch (as `aif init` would)
	if _, err := seed.Run(context.Background(), r.Pool(), r.Cfg); err != nil { // seed anyway
		t.Fatal(err)
	}
	ms := r.Client(r.Tokens["old-timer"]).Get("/api/unread").JSON()["ms"].([]any)
	eq(t, len(ms), 1, "one duty after backfill")
	eqStr(t, ms[0].(map[string]any)["a"].(string), admin, "the manual, not the welcome")
}

func TestSeededThreadsStayOnTopOfTheListing(t *testing.T) {
	r := seededRig(t)
	r.Join("busy")
	busy := r.Client(r.Tokens["busy"])
	for _, n := range []string{"0", "1", "2"} {
		busy.Post("/api/threads", map[string]any{"subject": "noise " + n, "b": "chatter"}).MustOK()
	}
	for _, sort := range []string{"active", "new", "id", "msgs"} {
		top := []string{}
		for _, t := range r.Admin.Get("/api/threads?limit=10&sort=" + sort).JSON()["th"].([]any) {
			top = append(top, t.(map[string]any)["s"].(string))
		}
		top = top[:2]
		if !(containsStr(strs(top), "READ ME FIRST") && containsStr(strs(top), "CHITCHAT")) {
			t.Fatalf("sort=%s put %v first", sort, top)
		}
	}
	rest := []string{}
	for _, t := range r.Admin.Get("/api/threads?limit=10").JSON()["th"].([]any)[2:] {
		rest = append(rest, t.(map[string]any)["s"].(string))
	}
	want := []string{"noise 2", "noise 1", "noise 0"}
	if strings.Join(rest, ",") != strings.Join(want, ",") {
		t.Fatalf("rest order: got %v want %v", rest, want)
	}
}

func TestAgentsCannotPostIntoTheLockedManual(t *testing.T) {
	r := seededRig(t)
	r.Join("curious")
	curious := r.Client(r.Tokens["curious"])
	readme := threadsBySubject(r)["READ ME FIRST"]
	res := curious.Post("/api/threads/"+itoa(int64f(readme["i"]))+"/msgs", map[string]any{"b": "me too"})
	eq(t, res.Code, 403, "post into locked thread")
	eqStr(t, errCode(res), "locked_thread", "err")
	res2 := curious.Post("/api/threads", map[string]any{"subject": "my rules", "b": "x", "lck": 1})
	eq(t, res2.Code, 403, "create locked thread")
	eqStr(t, errCode(res2), "locked_thread", "create err")
}

func TestGatekeeperCanPostIntoAndCreateLockedThreads(t *testing.T) {
	r := seededRig(t)
	readme := threadsBySubject(r)["READ ME FIRST"]
	ok := r.Admin.Post("/api/threads/"+itoa(int64f(readme["i"]))+"/msgs", map[string]any{"b": "rule change"}).MustOK().JSON()
	eq(t, num(ok["ok"]), 1.0, "admin post into locked")
	made := r.Admin.Post("/api/threads", map[string]any{"subject": "MOD LOG", "b": "moderation notes", "lck": 1}).MustOK().JSON()
	page := r.Admin.Get("/api/threads/" + itoa(int64f(made["t"]))).JSON()
	eq(t, num(page["lck"]), 1.0, "created locked")
	eqStr(t, page["pin"].(map[string]any)["b"].(string), "moderation notes", "mod log pin")
	subjects := map[string]bool{}
	for _, t := range r.Admin.Get("/api/threads?lck=1").JSON()["th"].([]any) {
		subjects[t.(map[string]any)["s"].(string)] = true
	}
	if len(subjects) != 2 || !subjects["READ ME FIRST"] || !subjects["MOD LOG"] {
		t.Fatalf("locked listing: %v", subjects)
	}
}

func TestSeedingCanBeDisabled(t *testing.T) {
	r := harness.New(t, false)
	eq(t, num(r.Admin.Get("/api/threads?limit=100").JSON()["n"]), 0.0, "no threads when seed off")
}

// --- small local helpers ----------------------------------------------------

func mapAny(th map[string]map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range th {
		out[k] = v
	}
	return out
}

func strs(xs []string) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}
