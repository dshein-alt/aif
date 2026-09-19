package itest

import (
	"strings"
	"testing"

	"aif/internal/harness"
)

// pinRig is two agents plus the admin, posting on behalf of a1, for pinned-thread tests.
type pinRig struct {
	r  *harness.Rig
	a1 *harness.Client
	a2 *harness.Client
}

func newPinRig(t *testing.T) *pinRig {
	r := harness.New(t, false)
	r.Join("a1")
	r.Join("a2")
	return &pinRig{r: r, a1: r.Client(r.Tokens["a1"]), a2: r.Client(r.Tokens["a2"])}
}

func (p *pinRig) makeThread(body string, files []map[string]any) map[string]any {
	if body == "" {
		body = "the topic of this thread"
	}
	body2 := map[string]any{"subject": "the subject", "b": body}
	if files != nil {
		body2["files"] = files
	}
	return p.a1.Post("/api/threads", body2).MustOK().JSON()
}

func TestFirstMessageReturnedAsDescription(t *testing.T) {
	p := newPinRig(t)
	made := p.makeThread("", nil)
	tid := itoa(int64f(made["t"]))
	p.a2.Post("/api/threads/"+tid+"/msgs", map[string]any{"b": "first reply"}).MustOK()
	page := p.r.Admin.Get("/api/threads/" + tid).JSON()
	pin := page["pin"].(map[string]any)
	eq(t, int64f(pin["i"]), int64f(made["i"]), "pin id")
	eqStr(t, pin["b"].(string), "the topic of this thread", "pin body")
	eqStr(t, pin["a"].(string), "a1", "pin author")
	msgs := page["ms"].([]any)
	eq(t, int64f(msgs[0].(map[string]any)["i"]), int64f(made["i"]), "first msg")
	eq(t, int64f(msgs[1].(map[string]any)["i"]), int64f(made["i"])+1, "second msg")
}

func TestDescriptionSurvivesEveryWayOfPaging(t *testing.T) {
	p := newPinRig(t)
	made := p.makeThread("DESCRIPTION", nil)
	tid := int64f(made["t"])
	for i := 0; i < 5; i++ {
		p.a2.Post("/api/threads/"+itoa(tid)+"/msgs", map[string]any{"b": "reply"}).MustOK()
	}
	variants := []struct {
		label   string
		params  string
		idsOnly bool
	}{
		{"paged forward", "?since=2&limit=2", false},
		{"paged backward", "?before=6&order=desc&limit=2", false},
		{"last message only", "?order=desc&limit=1", false},
		{"metadata only", "?msgs=0", false},
		{"ids only", "?body=0", true},
	}
	for _, v := range variants {
		page := p.r.Admin.Get("/api/threads/" + itoa(tid) + v.params).JSON()
		pin := page["pin"].(map[string]any)
		eq(t, int64f(pin["i"]), int64f(made["i"]), "pin id for "+v.label)
		if v.idsOnly {
			missing(t, pin, "b", "ids only pin")
		} else {
			eqStr(t, pin["b"].(string), "DESCRIPTION", "pin body for "+v.label)
		}
	}
}

func TestPinCanBeSkipped(t *testing.T) {
	p := newPinRig(t)
	made := p.makeThread("", nil)
	tid := itoa(int64f(made["t"]))
	missing(t, p.r.Admin.Get("/api/threads/"+tid+"?pin=0").JSON(), "pin", "pin=0 query")
	missing(t, p.r.Admin.Op("thread", map[string]any{"id": made["t"], "pin": 0}).JSON(), "pin", "pin=0 op")
}

func TestPinRespectsTruncationAndMentionsFiles(t *testing.T) {
	p := newPinRig(t)
	long := strings.Repeat("x", 500)
	made := p.makeThread(long+" @a2", []map[string]any{{"n": "readme.txt", "text": "spec"}})
	tid := itoa(int64f(made["t"]))
	clipped := p.r.Admin.Get("/api/threads/" + tid + "?max_body=50").JSON()["pin"].(map[string]any)
	if int(len([]rune(clipped["b"].(string)))) > 51 {
		t.Fatalf("clipped body too long: %d", len([]rune(clipped["b"].(string))))
	}
	at := clipped["at"].([]any)
	eqStr(t, at[0].(string), "a2", "pin mention")
	eqStr(t, clipped["fl"].([]any)[0].(map[string]any)["n"].(string), "readme.txt", "pin file")
	verbose := p.r.Admin.Get("/api/threads/" + tid + "?long=1").JSON()["pin"].(map[string]any)
	has(t, verbose, "body", "long pin has body")
	missing(t, verbose, "b", "long pin drops b")
	eqStr(t, verbose["author"].(string), "a1", "long pin author")
}

func TestDeletingOpenerPassesDescriptionToNext(t *testing.T) {
	p := newPinRig(t)
	made := p.makeThread("original description", nil)
	tid := made["t"]
	second := p.a2.Post("/api/threads/"+itoa(int64f(tid))+"/msgs", map[string]any{"b": "second description"}).MustOK().JSON()
	gone := p.a1.Op("rm", map[string]any{"what": "message", "id": made["i"]}).MustOK().JSON()
	eq(t, num(gone["ok"]), 1.0, "rm ok")
	page := p.r.Admin.Get("/api/threads/" + itoa(int64f(tid))).JSON()
	pin := page["pin"].(map[string]any)
	eq(t, int64f(pin["i"]), int64f(second["i"]), "pin moved")
	eqStr(t, pin["b"].(string), "second description", "new pin body")
	p.a2.Op("rm", map[string]any{"what": "message", "id": second["i"]})
	missing(t, p.r.Admin.Get("/api/threads/"+itoa(int64f(tid))).JSON(), "pin", "no pin after deleting both")
}

func TestMCPToolSchemaDocumentsThePin(t *testing.T) {
	p := newPinRig(t)
	tools := p.r.Admin.Post("/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}).JSON()["result"].(map[string]any)["tools"].([]any)
	var props map[string]any
	for _, t := range tools {
		tm := t.(map[string]any)
		if tm["name"] == "thread" {
			props = tm["inputSchema"].(map[string]any)["properties"].(map[string]any)
		}
	}
	if props == nil {
		t.Fatal("no thread tool schema")
	}
	if _, ok := props["pin"]; !ok {
		t.Fatalf("thread schema missing pin: %v", keys(props))
	}
}
