package itest

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
)

func TestThreadGlossary(t *testing.T) {
	r, _ := uiRig(t)
	alice, bob, outsider := r.Client(r.Tokens["alice"]), r.Client(r.Tokens["bob"]), r.Join("outsider")
	sp := newSpaceAs(t, alice, "review")
	alice.Op("space", map[string]any{"id": sp, "add": "bob"}).MustOK()
	thread := int64f(alice.Op("post", map[string]any{"subject": "Review", "b": "Review both board revisions.", "sp": sp}).MustOK().Field("t"))
	path := fmt.Sprintf("/api/threads/%d", thread)
	terms := path + "/terms"
	firstRef := "[[term:驱动板#1]]"
	message := map[string]any{"b": "Compare " + firstRef}

	eq(t, outsider.Get(terms).Code, http.StatusNotFound, "private glossary is hidden")
	eq(t, bob.Post(path+"/msgs", message).Code, http.StatusUnprocessableEntity, "missing term blocks a reply")
	eq(t, bob.Post(path+"/msgs", map[string]any{"b": "[[term:驱动板]]"}).Code, http.StatusBadRequest, "ref needs its revision")

	initial := map[string]any{"term": "驱动板", "definition": "板卡版本一", "referent": "hardware/board@rev1", "revision": 0}
	eq(t, outsider.Do(http.MethodPut, terms, initial).Code, http.StatusNotFound, "outsider cannot define a private term")
	withMissingReferent := map[string]any{"term": "PCB", "definition": "board", "revision": 0}
	eq(t, alice.Do(http.MethodPut, terms, withMissingReferent).Code, http.StatusBadRequest, "concrete referent required")
	eqStr(t, alice.Do(http.MethodPut, terms, initial).MustOK().Field("ref").(string), firstRef, "stable marker")
	bob.Post(path+"/msgs", message).MustOK()
	eq(t, bob.Do(http.MethodPut, terms, initial).Code, http.StatusConflict, "stale create conflicts")
	update := map[string]any{"term": "驱动板", "definition": "板卡版本二", "referent": "hardware/board@rev2", "revision": 1}
	eqStr(t, bob.Do(http.MethodPut, terms, update).MustOK().Field("ref").(string), "[[term:驱动板#2]]", "either agent can revise")
	old := alice.Get(terms + "?" + url.Values{"term": {"驱动板"}, "revision": {"1"}}.Encode()).MustOK()
	contains(t, old.Text(), "hardware/board@rev1", "historical referent stays fixed")
	contains(t, bob.Get(terms).MustOK().Text(), "hardware/board@rev2", "latest glossary")
	bob.Op("post", map[string]any{"t": thread, "b": firstRef}).MustOK()

	other := int64f(alice.Op("post", map[string]any{"subject": "Other", "b": "Plain message"}).MustOK().Field("t"))
	eq(t, bob.Op("post", map[string]any{"t": other, "b": firstRef}).Code, http.StatusUnprocessableEntity, "no cross-thread references")
	page := uiLogin(t, r, r.Tokens["alice"]).Get(fmt.Sprintf("/ui/thread/%d", thread)).MustOK()
	contains(t, page.Text(), "Thread glossary", "glossary visible in the thread")
	contains(t, page.Text(), "hardware/board@rev2", "specific object visible in the thread")
}
