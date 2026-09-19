package itest

import (
	"context"
	"testing"

	"github.com/dshein-alt/aif/internal/db"
	"github.com/dshein-alt/aif/internal/harness"
)

// pendingFileID looks up the numeric id of a just-uploaded (still detached) file by its key.
func pendingFileID(t *testing.T, r *harness.Rig, key string) float64 {
	t.Helper()
	row, err := db.QueryOne(context.Background(), r.Pool(), "SELECT id FROM files WHERE key = ?", key)
	if err != nil || row == nil {
		t.Fatalf("pending upload %q not found: %v", key, err)
	}
	switch v := row["id"].(type) {
	case int64:
		return float64(v)
	case int32:
		return float64(v)
	case float64:
		return v
	}
	t.Fatalf("unexpected id type %T", row["id"])
	return 0
}

// TestRESTExtraRoutes covers the POST aliases for poll/unread/feed, the REST message-create route,
// the file-attach route (with its guards), and the `skill` op reached through the op dispatcher.
func TestRESTExtraRoutes(t *testing.T) {
	r := harness.New(t, false)
	alice := r.Join("alice")

	// POST variants of the read-only inbox ops (the GET versions are covered elsewhere).
	if p := alice.Post("/api/poll", map[string]any{}).MustOK(); p.Field("n") == nil {
		t.Error("POST /api/poll returned no count")
	}
	if u := alice.Post("/api/unread", map[string]any{}).MustOK(); u.Field("ms") == nil {
		t.Error("POST /api/unread returned no messages")
	}
	if f := alice.Post("/api/feed", map[string]any{}).MustOK(); f.Field("ms") == nil {
		t.Error("POST /api/feed returned no messages")
	}

	// REST message creation (POST /api/messages -> op post).
	created := alice.Post("/api/messages", map[string]any{"subject": "REST create", "b": "hi"}).MustOK()
	if created.Field("t") == nil || created.Field("i") == nil {
		t.Errorf("POST /api/messages = %s", created.Text())
	}

	// The `skill` op reached via the op dispatcher (text + json).
	if card := alice.Op("skill", nil).MustOK(); card.Str("text") == "" {
		t.Error("op skill returned no card text")
	}
	if cj := alice.Op("skill", map[string]any{"format": "json"}).MustOK(); cj.JSON()["service"] == nil {
		t.Errorf("op skill json = %s", cj.Text())
	}

	// File attach route: an uploaded file becomes visible on one's own message.
	up := uploadPost(t, r.URL("/api/files"), r.Tokens["alice"], [][2]string{{"note.txt", "attach me"}}).MustOK()
	key := up.Field("u").([]any)[0].(map[string]any)["k"].(string)
	fid := pendingFileID(t, r, key)
	mid := alice.Op("post", map[string]any{"subject": "Attach target", "b": "will hold a file"}).MustOK().Field("i").(float64)

	att := alice.Post("/api/files/"+itoaF(fid)+"/attach?message_id="+itoaF(mid), map[string]any{}).MustOK()
	if att.Field("ok") != float64(1) {
		t.Errorf("attach = %s", att.Text())
	}
	if fl := alice.Get("/api/messages/" + itoaF(mid)).MustOK().Field("fl"); fl == nil {
		t.Error("attached file not listed on the message")
	}

	// guards: missing message_id (400), unknown file (404 no_file), someone else's message (403).
	if g := alice.Post("/api/files/"+itoaF(fid)+"/attach", map[string]any{}); g.Code != 400 || g.Str("err") != "bad_request" {
		t.Errorf("attach without message_id = %d %s, want 400", g.Code, g.Text())
	}
	if g := alice.Post("/api/files/99999999/attach?message_id="+itoaF(mid), map[string]any{}); g.Code != 404 || g.Str("err") != "no_file" {
		t.Errorf("attach unknown file = %d, want 404 no_file", g.Code)
	}
	bob := r.Join("bob")
	if g := bob.Post("/api/files/"+itoaF(fid)+"/attach?message_id="+itoaF(mid), map[string]any{}); g.Code != 403 || g.Str("err") != "not_yours" {
		t.Errorf("attach to another agent's message = %d %s, want 403 not_yours", g.Code, g.Text())
	}
}
