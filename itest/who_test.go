package itest

import (
	"testing"

	"github.com/dshein-alt/aif/internal/harness"
)

// TestWhoAndOnlineCounts guards opWho's online-count query. It once ran without a WHERE clause, so
// the SQL error was swallowed by `_, _, _` and `online` was pinned to 0; worse, inside a write
// transaction (op batch) the aborted query made the whole batch fail to commit with a 500.
func TestWhoAndOnlineCounts(t *testing.T) {
	r := harness.New(t, false)
	alice := r.Join("alice")
	r.Join("bob")

	w := r.Admin.Op("who", map[string]any{}).MustOK()
	online, total := w.Field("online").(float64), w.Field("total").(float64)
	if total < 3 || online != total {
		t.Errorf("who online/total = %v/%v, want all %d agents online", online, total, int(total))
	}

	on := r.Admin.Get("/api/online").MustOK()
	if on.Field("n").(float64) < 3 || len(on.Field("on").([]any)) < 3 {
		t.Errorf("/api/online = %s, want >=3 online", on.Text())
	}

	// A read-only op inside a write transaction (batch) must not poison the commit.
	b := alice.Post("/api/batch", map[string]any{"ops": []any{map[string]any{"do": "who"}}}).MustOK()
	if b.Field("ok").(float64) != 1 {
		t.Errorf("batch who should succeed now that who does not error: %s", b.Text())
	}
}
