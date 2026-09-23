package itest

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/dshein-alt/aif/internal/config"
	"github.com/dshein-alt/aif/internal/db"
	"github.com/dshein-alt/aif/internal/harness"
)

const spaceWebToken = "space-human-view"

// --- helpers ---------------------------------------------------------------

// spaceRig is a UI-mounted rig with seeding on (pins at threads 1 and 2).
func spaceRig(t *testing.T) *harness.Rig {
	return harness.NewWith(t, true, func(c *config.Config) {
		c.Seed = true
		c.WebToken = spaceWebToken
	})
}

// newSpaceAs creates a space as c and returns its id.
func newSpaceAs(t *testing.T, c *harness.Client, name string) int64 {
	t.Helper()
	res := c.Op("space", map[string]any{"new": 1, "name": name, "descr": "itest " + name})
	res.MustOK()
	sp, ok := res.Field("sp").(map[string]any)
	if !ok {
		t.Fatalf("space new reply has no sp: %s", res.Text())
	}
	return idOf(sp)
}

// idOf reads a compact ("i") or long ("id") identifier.
func idOf(m map[string]any) int64 {
	if v := int64f(m["id"]); v != 0 {
		return v
	}
	return int64f(m["i"])
}

// scopedJoin claims an invite (issued by issuer with sp) as name and returns the child's client.
func scopedJoin(t *testing.T, r *harness.Rig, issuer *harness.Client, name string, sp int64) *harness.Client {
	t.Helper()
	res := issuer.Op("issue", map[string]any{"sp": sp})
	res.MustOK()
	tok, _ := res.Field("token").(string)
	claim := r.Claim(name, tok)
	claim.MustOK()
	if int64f(claim.Field("sp")) != sp {
		t.Fatalf("claim %s: expected sp=%d in reply, got %s", name, sp, claim.Text())
	}
	return r.Client(r.Tokens[name])
}

// joinAs claims a name under `issuer`'s invite (a trust-chain child of issuer, not of gatekeeper).
func joinAs(t *testing.T, r *harness.Rig, issuer *harness.Client, name string) *harness.Client {
	t.Helper()
	res := issuer.Op("issue", nil)
	res.MustOK()
	tok, _ := res.Field("token").(string)
	r.Claim(name, tok).MustOK()
	return r.Client(r.Tokens[name])
}

func threadIDs(res *harness.Resp) []int64 {
	var out []int64
	list, _ := res.JSON()["th"].([]any)
	for _, raw := range list {
		out = append(out, int64f(raw.(map[string]any)["i"]))
	}
	return out
}

func hasID(xs []int64, want int64) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// --- the space lifecycle ----------------------------------------------------

// TestSpaceLifecycle covers create, scoping threads, listing, membership, ancestor access,
// nested ban, and deletion (soft threads, hard scoped children, everyone else untouched).
func TestSpaceLifecycle(t *testing.T) {
	r := spaceRig(t)
	owner := r.Join("sowner")
	stranger := r.Join("sstranger")

	pubT, pubMsg := newThreadAs(t, stranger, "public topic", "hello all")

	spID := newSpaceAs(t, owner, "lab")

	// `spaces` shows the owner their space, with the owner role.
	lst := owner.Op("spaces", nil).MustOK()
	sps, _ := lst.Field("sp").([]any)
	if len(sps) != 1 {
		t.Fatalf("owner should see exactly 1 space: %s", lst.Text())
	}
	first := sps[0].(map[string]any)
	eq(t, idOf(first), spID, "space id in list")
	eqStr(t, fmt.Sprint(first["role"]), "owner", "owner role")

	// A thread opened with sp carries the space id and the sc directory.
	post := owner.Op("post", map[string]any{"subject": "lab notes", "b": "hush", "sp": spID}).MustOK()
	tid := int64f(post.Field("t"))
	eq(t, int64f(post.Field("sp")), spID, "post reply echoes the space")

	tr := owner.Op("thread", map[string]any{"id": tid}).MustOK()
	eq(t, int64f(tr.Field("sp")), spID, "thread carries sp")
	if sc, ok := tr.Field("sc").(map[string]any); !ok || sc[itoa(spID)] == nil {
		t.Fatalf("thread reply missing sc dir entry: %s", tr.Text())
	}

	// A stranger cannot see, open or search the space thread.
	eq(t, stranger.Op("thread", map[string]any{"id": tid}).Code, 404, "stranger thread")
	eq(t, stranger.Op("get", map[string]any{"id": int64f(post.Field("i"))}).Code, 404, "stranger get msg")
	if hasID(threadIDs(stranger.Op("threads", map[string]any{"q": "lab"}).MustOK()), tid) {
		t.Fatal("space thread must not surface in a stranger's search")
	}
	// ... and the public thread never carries sp.
	pub := owner.Op("thread", map[string]any{"id": pubT}).MustOK()
	missing(t, pub.JSON(), "sp", "public thread has no sp")

	// Only the owner may scope invites into the space or manage it.
	eqStr(t, errCode(stranger.Op("issue", map[string]any{"sp": spID})), "not_space_owner", "stranger scoped issue")
	eqStr(t, errCode(stranger.Op("space", map[string]any{"id": spID, "del": 1})), "not_space_owner", "stranger del")

	// A scoped child sees its space + the two seeded pins and nothing else - even public threads.
	bot := scopedJoin(t, r, owner, "sbot", spID)
	seen := threadIDs(bot.Op("threads", map[string]any{"limit": 50}).MustOK())
	if !hasID(seen, tid) || !hasID(seen, 1) || !hasID(seen, 2) {
		t.Fatalf("scoped child must see space + pins: %v", seen)
	}
	if hasID(seen, pubT) {
		t.Fatalf("scoped child must not see public threads: %v", seen)
	}

	// Scoped children write inside their space, but cannot nest spaces or write publicly.
	bot.Op("post", map[string]any{"t": tid, "b": "scoped reply"}).MustOK()
	for _, dir := range []int{1, -1, 0} {
		bot.Op("vote", map[string]any{"id": int64f(post.Field("i")), "dir": dir}).MustOK()
	}
	eq(t, bot.Op("vote", map[string]any{"id": pubMsg, "dir": 1}).Code, 404, "scoped public vote")
	childThread := bot.Op("post", map[string]any{"subject": "child work", "b": "inside", "sp": spID}).MustOK()
	eq(t, int64f(childThread.Field("sp")), spID, "child thread stays in scope")
	eq(t, bot.Op("post", map[string]any{"t": pubT, "b": "leak"}).Code, 404, "scoped public reply")
	for _, pin := range []int64{1, 2} {
		pinMsg, _, err := db.QueryOneValue(context.Background(), r.Pool(), "SELECT id FROM messages WHERE thread = ? ORDER BY id LIMIT 1", pin)
		if err != nil {
			t.Fatal(err)
		}
		eqStr(t, errCode(bot.Op("vote", map[string]any{"id": pinMsg, "dir": 1})), "scoped_readonly", "scoped pin vote")
		eqStr(t, errCode(bot.Op("post", map[string]any{"t": pin, "b": "leak"})), "scoped_readonly", "scoped pin reply")
	}
	eqStr(t, errCode(bot.Op("post", map[string]any{"subject": "leak", "b": "!"})), "scoped_readonly", "scoped public post")
	eqStr(t, errCode(bot.Op("space", map[string]any{"new": 1, "name": "subspace"})), "nested_space", "nested space")

	// Their own invites stay inside the scope.
	sub := scopedJoin(t, r, bot, "sbotling", spID)
	seen = threadIDs(sub.Op("threads", map[string]any{"limit": 50}).MustOK())
	if !hasID(seen, tid) || hasID(seen, pubT) {
		t.Fatalf("inherited scope wrong: %v", seen)
	}
	// A scoped child cannot be invited into another space.
	sp2 := newSpaceAs(t, owner, "lab2")
	eq(t, bot.Op("post", map[string]any{"subject": "leak", "b": "!", "sp": sp2}).Code, 404, "scoped other-space creation")
	sub.Op("post", map[string]any{"t": tid, "b": "inherited scoped reply"}).MustOK()
	eqStr(t, errCode(owner.Op("space", map[string]any{"id": sp2, "add": "sbot"})), "bound_agent", "invite a bound child")

	// Members get read+write; they cannot manage the space.
	owner.Op("space", map[string]any{"id": spID, "add": "sstranger"}).MustOK()
	if !hasID(threadIDs(stranger.Op("threads", map[string]any{"limit": 50}).MustOK()), tid) {
		t.Fatal("member must see the space thread")
	}
	stranger.Op("post", map[string]any{"t": tid, "b": "member reply"}).MustOK()
	eqStr(t, errCode(stranger.Op("space", map[string]any{"id": spID, "del": 1})), "not_space_owner", "member del")
	// An invited member appears in the roster.
	info := owner.Op("space", map[string]any{"id": spID}).MustOK()
	if ats, ok := info.Field("sp").(map[string]any)["at"].([]any); !ok || len(ats) != 4 {
		t.Fatalf("roster should list owner+member+2 scoped: %v", info.Field("sp"))
	}
	// After removal the member loses the thread again.
	owner.Op("space", map[string]any{"id": spID, "rm": "sstranger"}).MustOK()
	eq(t, stranger.Op("thread", map[string]any{"id": tid}).Code, 404, "removed member thread")

	// Ancestors: a child of `owner` opens a space; the owner reads but may not write it.
	carat := joinAs(t, r, owner, "scarol")
	sp3 := newSpaceAs(t, carat, "beta")
	carat.Op("post", map[string]any{"subject": "beta note", "b": "hi", "sp": sp3}).MustOK()
	if !hasSpace(t, owner, sp3) {
		t.Fatal("ancestor should see child's space")
	}
	ancestorPost := owner.Op("post", map[string]any{"subject": "into beta", "b": "!", "sp": sp3})
	eqStr(t, errCode(ancestorPost), "space_readonly", "ancestor write")

	// Deleting a space soft-deletes its threads and hard-removes ONLY scoped children.
	del := owner.Op("space", map[string]any{"id": spID, "del": 1}).MustOK()
	eq(t, int64f(del.Field("threads_deleted")), 2, "threads soft-deleted")
	// The space and its threads are gone everywhere...
	eq(t, owner.Op("thread", map[string]any{"id": tid}).Code, 404, "deleted thread")
	eq(t, owner.Op("thread", map[string]any{"id": childThread.Field("t")}).Code, 404, "deleted child thread")
	if hasID(threadIDs(owner.Op("threads", map[string]any{"limit": 50}).MustOK()), tid) {
		t.Fatal("deleted space thread still listed")
	}
	// ... but scoped children were removed, and members/ancestors survive.
	if present(t, r, "sbot") || present(t, r, "sbotling") {
		t.Fatal("scoped children should be gone after deletion")
	}
	if !present(t, r, "sstranger") {
		t.Fatal("member must survive space deletion")
	}
	if !present(t, r, "scarol") {
		t.Fatal("ancestor agent must survive deletion of another space")
	}
	// The deleted space is out of the live list but visible with dead=1.
	if hasSpaceID(t, owner, spID, false) {
		t.Fatal("deleted space in live list")
	}
	if !hasSpaceID(t, owner, spID, true) {
		t.Fatal("deleted space missing with dead=1")
	}
}

func present(t *testing.T, r *harness.Rig, name string) bool {
	t.Helper()
	res := r.Admin.Op("who", map[string]any{"on": false, "q": name, "limit": 10}).MustOK()
	for _, raw := range res.JSON()["a"].([]any) {
		if raw.(map[string]any)["n"] == name {
			return true
		}
	}
	return false
}

// hasSpace reports whether the client's live `spaces` list contains the id.
func hasSpace(t *testing.T, c *harness.Client, id int64) bool { return hasSpaceID(t, c, id, false) }

func hasSpaceID(t *testing.T, c *harness.Client, id int64, dead bool) bool {
	t.Helper()
	args := map[string]any{"limit": 100}
	if dead {
		args["dead"] = 1
	}
	res := c.Op("spaces", args).MustOK()
	for _, raw := range res.JSON()["sp"].([]any) {
		if idOf(raw.(map[string]any)) == id {
			return true
		}
	}
	return false
}

// --- inbox and feed follow the wall -----------------------------------------

func TestSpaceScopedInbox(t *testing.T) {
	r := spaceRig(t)
	owner := r.Join("iowner")
	spID := newSpaceAs(t, owner, "inbox-lab")
	tid, _ := newThreadAs(t, owner, "lab inbox", "start") // a PUBLIC thread
	res := owner.Op("post", map[string]any{"subject": "lab inbox", "b": "private start", "sp": spID}).MustOK()
	spid := int64f(res.Field("t"))

	bot := scopedJoin(t, r, owner, "ibot", spID)
	n0 := int64f(bot.Op("poll", map[string]any{}).MustOK().Field("n")) // seeded pins may already be unread

	owner.Op("post", map[string]any{"t": spid, "b": "reply in space"}).MustOK()
	p2 := bot.Op("poll", map[string]any{}).MustOK()
	if int64f(p2.Field("n")) != n0+1 {
		t.Fatalf("space reply must reach the scoped child (want %d, got %s)", n0+1, p2.Text())
	}

	// A public-thread reply never reaches the scoped child (its inbox is the same wall).
	owner.Op("post", map[string]any{"t": tid, "b": "public reply"}).MustOK()
	p3 := bot.Op("poll", map[string]any{}).MustOK()
	if int64f(p3.Field("n")) != n0+1 {
		t.Fatalf("public reply must not enter a scoped inbox (want %d, got %s)", n0+1, p3.Text())
	}

	// And feed respects the wall too: since 0 it must not contain the public thread's messages.
	feed := bot.Op("feed", map[string]any{"since": 0, "limit": 50}).MustOK()
	for _, raw := range feed.JSON()["ms"].([]any) {
		if int64f(raw.(map[string]any)["t"]) == tid {
			t.Fatalf("feed leaked a public message to a scoped child: %v", raw)
		}
	}

	// A regular (non-scoped) child of the same owner sees public threads as always.
	plain := joinAs(t, r, owner, "iplain")
	if !hasID(threadIDs(plain.Op("threads", map[string]any{"limit": 50}).MustOK()), tid) {
		t.Fatal("non-scoped child must see public threads")
	}
	eq(t, plain.Op("thread", map[string]any{"id": spid}).Code, 404, "non-scoped child must not see space threads")
}

// Space membership auto-follows a thread when it is created (or when the agent joins later), but
// replies must not recreate a subscription an agent explicitly removed.
func TestSpaceUnsubscribeSurvivesReplies(t *testing.T) {
	r := spaceRig(t)
	ancestor := r.Join("mute-ancestor")
	owner := joinAs(t, r, ancestor, "mute-owner")
	member := r.Join("mute-member")
	spID := newSpaceAs(t, owner, "mute-lab")
	owner.Op("space", map[string]any{"id": spID, "add": "mute-member"}).MustOK()
	post := owner.Op("post", map[string]any{"subject": "mute me", "b": "start", "sp": spID}).MustOK()
	tid := int64f(post.Field("t"))

	for name, c := range map[string]*harness.Client{"member": member, "ancestor": ancestor} {
		c.Op("sub", map[string]any{"t": tid, "off": 1}).MustOK()
		if subscribed(t, c, tid) {
			t.Fatalf("%s remained subscribed immediately after sub off", name)
		}
	}

	owner.Op("post", map[string]any{"t": tid, "b": "reply after mute"}).MustOK()
	for name, c := range map[string]*harness.Client{"member": member, "ancestor": ancestor} {
		if subscribed(t, c, tid) {
			t.Fatalf("%s was silently re-subscribed by a space reply", name)
		}
	}
}

func subscribed(t *testing.T, c *harness.Client, tid int64) bool {
	t.Helper()
	res := c.Op("sub", map[string]any{"list": 1}).MustOK()
	for _, raw := range res.JSON()["su"].([]any) {
		if idOf(raw.(map[string]any)) == tid {
			return true
		}
	}
	return false
}

// --- files respect the wall --------------------------------------------------

func TestSpaceFiles(t *testing.T) {
	r := spaceRig(t)
	owner := r.Join("fowner")
	curious := r.Join("fcurious")
	spID := newSpaceAs(t, owner, "vault")
	res := owner.Op("post", map[string]any{
		"subject": "secret plans", "b": "see file", "sp": spID, "full": 1,
		"files": []any{map[string]any{"n": "keys.txt", "text": "secret"}},
	}).MustOK()
	fid := int64f(res.JSON()["fl"].([]any)[0].(map[string]any)["i"])

	eq(t, curious.Op("dl", map[string]any{"id": fid}).Code, 404, "stranger dl")
	eq(t, curious.Get(fmt.Sprintf("/api/files/%d", fid)).Code, 404, "stranger meta")
	eq(t, curious.Get(fmt.Sprintf("/api/files/%d/raw", fid)).Code, 404, "stranger raw")
	web := uiAs(t, r, spaceWebToken)
	web.Get(fmt.Sprintf("/ui/files/%d", fid)).MustOK()
	eqStr(t, web.Get(fmt.Sprintf("/ui/files/%d/raw", fid)).MustOK().Text(), "secret", "web-token private attachment")

	owner.Op("space", map[string]any{"id": spID, "add": "fcurious"}).MustOK()
	curious.Op("dl", map[string]any{"id": fid, "text": 1}).MustOK()
	curious.Get(fmt.Sprintf("/api/files/%d/raw", fid)).MustOK()

	// Raw API/UI file routes construct visibility directly instead of going through an op. A
	// recovery token may preserve different display-case from the registered agent, so those routes
	// must canonicalise identity just as the dl op does.
	recTok, _ := r.Admin.Op("issue", map[string]any{"name": "FOWNER"}).MustOK().Field("token").(string)
	recovered := r.Client(recTok)
	recovered.Get(fmt.Sprintf("/api/files/%d", fid)).MustOK()
	recovered.Get(fmt.Sprintf("/api/files/%d/raw", fid)).MustOK()
	recoveredUI := uiAs(t, r, recTok)
	recoveredUI.Get(fmt.Sprintf("/ui/files/%d", fid)).MustOK()
	recoveredUI.Get(fmt.Sprintf("/ui/files/%d/raw", fid)).MustOK()
}

// --- the /ui renders the wall too -------------------------------------------

func uiAs(t *testing.T, r *harness.Rig, password string) *harness.Client {
	t.Helper()
	login := r.NoRedirect().Post("/ui/login", url.Values{"password": {password}, "next": {"/ui"}})
	login.MustStatus(303)
	return r.Client("").H("cookie", "aif_ui="+cookieValue(login))
}

func TestSpaceUIGroups(t *testing.T) {
	r := spaceRig(t)
	owner := r.Join("uowner")
	_ = r.Join("ustranger")
	spID := newSpaceAs(t, owner, "ui-lab")
	res := owner.Op("post", map[string]any{"subject": "ui secret", "b": "x", "sp": spID}).MustOK()
	tid := int64f(res.Field("t"))
	scopedJoin(t, r, owner, "ubot", spID)

	// The gatekeeper's operator view shows the space as its own folded group, thread inside.
	admin := uiAs(t, r, harness.AdminToken)
	page := admin.Get("/ui").MustOK().Text()
	contains(t, page, "ui-lab", "admin index shows the space group")
	contains(t, page, "ui secret", "admin index shows the space thread")

	// The scoped child's index shows its space (and the pins) - and its thread pages render.
	bot := uiAs(t, r, r.Tokens["ubot"])
	page2 := bot.Get("/ui").MustOK().Text()
	contains(t, page2, "ui-lab", "scoped index shows own space group")
	bot.Get(fmt.Sprintf("/ui/thread/%d", tid)).MustOK()

	// A stranger's index has no space group, and the thread page is a 404.
	out := uiAs(t, r, r.Tokens["ustranger"])
	idx := out.Get("/ui").MustOK().Text()
	if strings.Contains(idx, "ui secret") {
		t.Fatal("stranger index must not show space threads")
	}
	if c := out.Get(fmt.Sprintf("/ui/thread/%d", tid)).Code; c != 404 {
		t.Fatalf("stranger thread page: got %d, want 404", c)
	}

	// The human-view token audits all spaces through the read-only UI, never the API.
	web := uiAs(t, r, spaceWebToken)
	webPage := web.Get("/ui").MustOK().Text()
	contains(t, webPage, "ui secret", "web-token private thread in index")
	contains(t, webPage, "ui-lab", "web-token private space in index")
	web.Get(fmt.Sprintf("/ui/thread/%d", tid)).MustOK()
	eqStr(t, errCode(r.Client(spaceWebToken).Op("post", map[string]any{"t": tid, "b": "forbidden"})), "web_token", "web token cannot write through API")
}

// --- regression: scoped children can actually read the pins (review #1) ------
//
// Cond listed the seeded pins for a scoped child, but ThreadVisible(0) returned false, so
// opening a pin by id (thread/get/dl/sub/seen and /ui/thread/{id}) answered 404 even though the
// pin showed up in the child's listing. Fixed by passing the thread id into ThreadVisible.
func TestSpaceScopedPinsReadable(t *testing.T) {
	r := spaceRig(t)
	owner := r.Join("powner")
	spID := newSpaceAs(t, owner, "pinlab")
	bot := scopedJoin(t, r, owner, "pbot", spID)

	// The seeded pins are threads 1 and 2; the child must open each, not just list them.
	for _, pid := range []int64{1, 2} {
		tr := bot.Op("thread", map[string]any{"id": pid, "msgs": 1, "limit": 5})
		if tr.Code != 200 {
			t.Fatalf("scoped child opening pin %d: got %d, want 200 (%s)", pid, tr.Code, tr.Text())
		}
		// and read an individual message inside it
		ms, _ := tr.JSON()["ms"].([]any)
		if len(ms) == 0 {
			t.Fatalf("pin %d returned no messages to a scoped child", pid)
		}
		mid := int64f(ms[0].(map[string]any)["i"])
		bot.Op("get", map[string]any{"id": mid}).MustOK()
		// following a pin must work too (sub with t subscribes by default)
		bot.Op("sub", map[string]any{"t": pid}).MustOK()
	}

	// /ui thread pages for the pins render for the scoped child.
	uiAs(t, r, r.Tokens["pbot"]).Get("/ui/thread/1").MustOK()

	// Sanity: the same scoped child still cannot open a public thread it is not part of.
	pubT, _ := newThreadAs(t, owner, "public no", "x")
	eq(t, bot.Op("thread", map[string]any{"id": pubT}).Code, 404, "scoped child must not read public thread")
}

// --- regression: deleteSpace must not delete unrelated agents (review #3) ----
//
// The old purge walked every token bound to the space and hard-deleted any agent named on a
// non-revoked one, so an agent whose name merely rode on a space-bound token was cascade-deleted
// with the space. Fixed twice over: the purge is now restricted to recorded RoleScoped children,
// and the recovery path that could put a non-scoped agent's name on a space-bound token
// (gatekeeper `issue {name:"alice", sp:5}`) is refused at issue time (see loop-3 finding 3).
func TestSpaceDeleteNoOverDelete(t *testing.T) {
	r := spaceRig(t)
	owner := r.Join("downer")
	victim := r.Join("victim") // an ordinary full-access agent, unrelated to the space

	// The victim has her own public content that must survive.
	vt, vm := newThreadAs(t, victim, "victim owns this", "mine")

	spID := newSpaceAs(t, owner, "doomed")
	owner.Op("post", map[string]any{"subject": "doomed note", "b": "x", "sp": spID}).MustOK()
	bot := scopedJoin(t, r, owner, "dbot", spID) // a genuine scoped child

	// The gatekeeper may not bind a scoped invite to a REGISTERED name (recovery + sp): that used to
	// auto-claim an unbound-but-scoped-looking token. It is now refused; membership goes via space add.
	eqStr(t, errCode(r.Admin.Op("issue", map[string]any{"name": "victim", "sp": spID})), "invalid",
		"a recovery token for a registered agent cannot be scoped")

	del := owner.Op("space", map[string]any{"id": spID, "del": 1}).MustOK()
	eq(t, int64f(del.Field("threads_deleted")), 1, "one space thread soft-deleted")

	// The genuine scoped child is gone...
	if present(t, r, "dbot") {
		t.Fatal("scoped child should be removed with the space")
	}
	// ... but the unrelated victim, who never became a scoped child, survives with her content.
	if !present(t, r, "victim") {
		t.Fatal("unrelated agent must NOT be deleted when a space is removed (over-delete regression)")
	}
	owner.Op("get", map[string]any{"id": vm}).MustOK() // victim's message still readable (public)
	_ = vt

	// And the victim's token still works: she is fully functional, not silently demoted.
	victim.Op("post", map[string]any{"t": vt, "b": "still here"}).MustOK()
	_ = bot
}

// --- regression: rm must not strip a scoped child's binding (review loop 2, #1) ---
//
// space {id,rm} deleted the target row whatever its role. For a scoped child that deletes the
// scope: the child silently becomes a full-access agent that can write anywhere, and space del
// no longer purges it (it only removes recorded RoleScoped rows). rm now refuses scoped rows;
// deleting the space is the correct way to release a scoped child.
func TestSpaceScopedChildNotRemovable(t *testing.T) {
	r := spaceRig(t)
	owner := r.Join("rowner")
	spID := newSpaceAs(t, owner, "guard")
	res := owner.Op("post", map[string]any{"subject": "guard note", "b": "x", "sp": spID}).MustOK()
	tid := int64f(res.Field("t"))

	bot := scopedJoin(t, r, owner, "rbot", spID)
	bot.Op("thread", map[string]any{"id": tid, "msgs": 1}).MustOK() // scoped child reads its space

	eqStr(t, errCode(owner.Op("space", map[string]any{"id": spID, "rm": "rbot"})), "bound_agent",
		"rm must refuse a scoped child, not delete its binding")
	bot.Op("thread", map[string]any{"id": tid, "msgs": 1}).MustOK() // still bound, still reads its space

	// Deleting the space is the sanctioned way to release it.
	owner.Op("space", map[string]any{"id": spID, "del": 1}).MustOK()
	if present(t, r, "rbot") {
		t.Fatal("scoped child should be released when its space is deleted")
	}
}

// --- regression: rm demotes an ancestor instead of deleting its access (review loop 2, #3) ---
//
// Ancestors are read-only members materialised from the owner's trust chain. add can promote one
// to a write member; rm used to delete the row outright, so an owner could revoke the inherited
// read access the design says is tracked and survives. rm now restores the ancestor role instead.
func TestSpaceRmKeepsAncestorRead(t *testing.T) {
	r := spaceRig(t)
	mid := r.Join("mid") // created under the gatekeeper; will become an ancestor
	midTok, _ := mid.Op("issue", nil).MustOK().Field("token").(string)
	r.Claim("chain_owner", midTok).MustOK()
	owner := r.Client(r.Tokens["chain_owner"]) // owner's parent is mid

	spID := newSpaceAs(t, owner, "dyn")
	res := owner.Op("post", map[string]any{"subject": "dyn note", "b": "x", "sp": spID}).MustOK()
	tid := int64f(res.Field("t"))

	// mid is the owner's parent: tracked as a read-only ancestor.
	mid.Op("thread", map[string]any{"id": tid, "msgs": 1}).MustOK()
	eqStr(t, errCode(mid.Op("post", map[string]any{"t": tid, "b": "no"})), "space_readonly", "ancestor is read-only")

	// Owner grants mid write, then withdraws it: rm demotes mid back to ancestor, never deletes.
	owner.Op("space", map[string]any{"id": spID, "add": "mid"}).MustOK()
	mid.Op("post", map[string]any{"t": tid, "b": "yes"}).MustOK()

	// A gatekeeper recovery token is a new root credential, but must not rewrite the owner's
	// materialised ancestry or make future spaces forget the original trust chain.
	recTok, _ := r.Admin.Op("issue", map[string]any{"name": "chain_owner"}).MustOK().Field("token").(string)
	recovered := r.Client(recTok)
	rm := recovered.Op("space", map[string]any{"id": spID, "rm": "mid"}).MustOK()
	eqStr(t, fmt.Sprint(rm.JSON()["role"]), "ancestor", "rm demotes the ancestor rather than removing it")

	// The inherited read access survives; the write grant is gone.
	mid.Op("thread", map[string]any{"id": tid, "msgs": 1}).MustOK()
	eqStr(t, errCode(mid.Op("post", map[string]any{"t": tid, "b": "no again"})), "space_readonly", "demoted back to read-only")

	sp2 := newSpaceAs(t, recovered, "after-recovery")
	if !hasSpace(t, mid, sp2) {
		t.Fatal("space created after owner recovery forgot the original ancestor")
	}

	// A non-ancestor member is still genuinely removed.
	plain := r.Join("plainmem")
	owner.Op("space", map[string]any{"id": spID, "add": "plainmem"}).MustOK()
	owner.Op("space", map[string]any{"id": spID, "rm": "plainmem"}).MustOK()
	eq(t, plain.Op("thread", map[string]any{"id": tid}).Code, 404, "plain member is really removed")
}

// --- regression: /ui loads must not keep an agent "online" (review loop 2, #2) ---
//
// threads/thread became WantsMe, so uiCall (which passes the agent's name for the visibility wall)
// ran Identity() and bumped agents.seen on every browser paint — a dead agent with an open /ui tab
// looked online in who forever. /ui now resolves identity quietly; API calls still record activity.
func TestUICallDoesNotKeepAgentOnline(t *testing.T) {
	r := spaceRig(t)
	ghost := r.Join("ghost")
	ctx := context.Background()

	if _, err := db.Exec(ctx, r.Pool(), "UPDATE agents SET seen = 0 WHERE low = ?", "ghost"); err != nil {
		t.Fatalf("force seen=0: %v", err)
	}
	seen := func() float64 {
		v, _, err := db.QueryOneValue(ctx, r.Pool(), "SELECT seen FROM agents WHERE low = ?", "ghost")
		if err != nil {
			t.Fatalf("read seen: %v", err)
		}
		f, _ := v.(float64)
		return f
	}
	eq(t, seen(), float64(0), "seen forced into the past")

	// An /ui load as this agent must NOT touch seen.
	uiAs(t, r, r.Tokens["ghost"]).Get("/ui").MustOK()
	eq(t, seen(), float64(0), "/ui load must not bump agents.seen")

	// A real API call still records the agent as active.
	ghost.Op("threads", map[string]any{"limit": 1}).MustOK()
	if seen() == 0 {
		t.Fatal("a genuine API call must bump agents.seen")
	}
}

// --- regression: a recovery token cannot be scoped to a space (review loop 3, #1/#3) ---
//
// Gatekeeper `issue {name:<registered>, sp:N}` auto-claimed a token carrying space=N but never
// bound the agent, so she was treated as scoped (all her invites forced into N) with no access to
// it, and deleteSpace of N wiped her live token. The recovery path now refuses sp; a plain recovery
// token leaves the agent exactly as unscoped as before.
func TestRecoveryTokenCannotBeScoped(t *testing.T) {
	r := spaceRig(t)
	owner := r.Join("recowner")
	spID := newSpaceAs(t, owner, "reclab")
	alice := r.Join("alice") // ordinary full-access agent

	// Recovery + sp is refused outright.
	eqStr(t, errCode(r.Admin.Op("issue", map[string]any{"name": "alice", "sp": spID})), "invalid",
		"gatekeeper cannot scope a recovery token")

	// A plain recovery token (no sp) still works and leaves alice unscoped: she can still own a space,
	// which a scoped child could not (that path returns nested_space).
	recTok, _ := r.Admin.Op("issue", map[string]any{"name": "alice"}).MustOK().Field("token").(string)
	fresh := r.Client(recTok).Op("space", map[string]any{"new": 1, "name": "alice-new", "descr": "d"})
	fresh.MustOK() // unscoped: space creation is allowed
	_ = alice
}

// A scoped child's recovery token carries no token-space column by design. Its recorded
// space_agents role remains authoritative, so descendants issued through the recovery credential
// must still inherit the scope instead of becoming ordinary full-access agents.
func TestScopedRecoveryStillIssuesScopedChildren(t *testing.T) {
	r := spaceRig(t)
	owner := r.Join("scopeowner")
	spID := newSpaceAs(t, owner, "scope-recovery")
	private := owner.Op("post", map[string]any{"subject": "scope private", "b": "x", "sp": spID}).MustOK()
	privateID := int64f(private.Field("t"))
	publicID, _ := newThreadAs(t, owner, "scope public", "x")
	scopedJoin(t, r, owner, "recovered-kid", spID)

	recTok, _ := r.Admin.Op("issue", map[string]any{"name": "recovered-kid"}).MustOK().Field("token").(string)
	recovered := r.Client(recTok)
	invite := recovered.Op("issue", nil).MustOK()
	eq(t, int64f(invite.Field("sp")), spID, "recovered scoped child's invite inherits membership scope")
	childTok, _ := invite.Field("token").(string)
	r.Claim("recovered-grandchild", childTok).MustOK()
	grandchild := r.Client(r.Tokens["recovered-grandchild"])
	grandchild.Op("thread", map[string]any{"id": privateID}).MustOK()
	eq(t, grandchild.Op("thread", map[string]any{"id": publicID}).Code, 404, "recovered descendant remains scoped")
}

// --- regression: long=1 thread replies must not hide the space (review loop 3, #2) ---
//
// ShapeThread reassigned `out` to the long map before reading out["sp"], so every long=1 reply from
// a private space looked public (no "space" field). Fixed by reading the short values first.
func TestThreadLongFormCarriesSpace(t *testing.T) {
	r := spaceRig(t)
	owner := r.Join("loner")
	spID := newSpaceAs(t, owner, "longlab")
	res := owner.Op("post", map[string]any{"subject": "long secret", "b": "x", "sp": spID}).MustOK()
	tid := int64f(res.Field("t"))

	// Short form already carried sp; the long form must carry "space".
	tr := owner.Op("thread", map[string]any{"id": tid, "long": 1}).MustOK()
	eq(t, int64f(tr.JSON()["space"]), spID, "long thread reply carries space")

	// And it must show up in a long listing too (long entries key off "id"/"space", not "i"/"sp").
	list := owner.Op("threads", map[string]any{"q": "long secret", "long": 1}).MustOK()
	found := false
	for _, raw := range list.JSON()["th"].([]any) {
		if e, ok := raw.(map[string]any); ok && int64f(e["space"]) == spID && int64f(e["id"]) == tid {
			found = true
		}
	}
	if !found {
		t.Fatalf("long threads listing entry missing space: %s", list.Text())
	}
}
