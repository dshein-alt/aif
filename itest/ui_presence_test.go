package itest

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

func TestUIAuthorPresence(t *testing.T) {
	r, tid := uiRig(t)
	bob := r.Client(r.Tokens["bob"])
	carol := r.Join("carol")
	browser := uiLogin(t, r, webPW)
	thread := fmt.Sprintf("/ui/thread/%d", tid)
	statusPath := thread + "/presence"
	first := "/api/agent-activity/turn-one"
	second := "/api/agent-activity/turn-two"
	expect := func(online, thinking bool) {
		t.Helper()
		state := browser.Get(statusPath).MustOK().JSON()
		got, ok := state["bob"].(map[string]any)
		if !ok || got["online"] != online || got["thinking"] != thinking {
			t.Fatalf("bob presence = %v, want online=%v thinking=%v", got, online, thinking)
		}
		if _, found := state["carol"]; found {
			t.Fatalf("presence included a non-author: %v", state)
		}
	}

	eq(t, r.Client("").Get(statusPath).Code, http.StatusUnauthorized, "browser sign-in required")
	eq(t, r.Client("").Do(http.MethodPut, first, nil).Code, http.StatusUnauthorized, "agent token required")
	eq(t, r.Admin.Do(http.MethodPut, first, nil).Code, http.StatusForbidden, "service token cannot report a turn")
	expect(true, false)
	eq(t, bob.Do(http.MethodPut, first, nil).Code, http.StatusNoContent, "start")
	eq(t, bob.Do(http.MethodPut, first, nil).Code, http.StatusNoContent, "renew")
	eq(t, bob.Do(http.MethodPut, second, nil).Code, http.StatusNoContent, "another turn")
	eq(t, carol.Do(http.MethodDelete, second, nil).Code, http.StatusNoContent, "another agent cannot clear the turn")
	eq(t, bob.Do(http.MethodDelete, first, nil).Code, http.StatusNoContent, "one turn ends")
	expect(true, true)
	page := browser.Get(thread).MustOK().Text()
	contains(t, page, `<aside class=thread-participants`, "participant panel")
	contains(t, page, `class=participant-avatar src="/ui/avatar/alice`, "first participant avatar")
	contains(t, page, `class=participant-avatar src="/ui/avatar/bob`, "second participant avatar")
	contains(t, page, "⇄", "two-way conversation marker")
	contains(t, page, ".thread-participants{position:sticky", "panel remains visible during scroll")
	contains(t, page, `title="Online · recent AIF activity"`, "online tooltip")
	contains(t, page, `title="Thinking · active model turn"></span>`, "visible thinking dot and tooltip")
	contains(t, page, "@keyframes presence-pulse", "pulse animation")
	contains(t, page, "prefers-reduced-motion", "reduced motion support")

	eq(t, bob.Do(http.MethodDelete, second, nil).Code, http.StatusNoContent, "all turns end")
	expect(true, false)
	eq(t, bob.Do(http.MethodPut, first, nil).Code, http.StatusNoContent, "start before timeout")
	if _, err := r.Pool().Exec(context.Background(), "UPDATE agent_activity SET expires = 1"); err != nil {
		t.Fatal(err)
	}
	expect(true, false)
	if _, err := r.Pool().Exec(context.Background(), "UPDATE agents SET seen = 1 WHERE name = 'bob'"); err != nil {
		t.Fatal(err)
	}
	bobBrowser := uiLogin(t, r, r.Tokens["bob"])
	bobBrowser.Get(statusPath).MustOK()
	expect(false, false)

	sp := newSpaceAs(t, bob, "presence-private")
	private := bob.Op("post", map[string]any{"subject": "Private", "b": "Secret", "sp": sp}).MustOK()
	privatePath := fmt.Sprintf("/ui/thread/%d/presence", int64f(private.Field("t")))
	eq(t, uiLogin(t, r, r.Tokens["carol"]).Get(privatePath).Code, http.StatusNotFound, "private thread is hidden")
	bobBrowser.Get(privatePath).MustOK()
}
