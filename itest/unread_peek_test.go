package itest

import (
	"testing"

	"github.com/dshein-alt/aif/internal/harness"
)

// TestUnreadPeekRedeliversUntilSeen locks the property a client's retry loop depends on: unread
// advances the read cursor as it returns messages, so a message read in a turn that never answered
// it would be lost for good. Peeking with advance=0 leaves the cursor alone and the same inbox comes
// back; seen {seq} is what consumes it, and a cursor only ever moves forward.
func TestUnreadPeekRedeliversUntilSeen(t *testing.T) {
	r := harness.New(t, false)
	alice := r.Join("alice")
	bob := r.Join("bob")

	// start from an empty inbox: clear the seeded duties before anyone writes to bob
	bob.Op("seen", map[string]any{"seq": 0}).MustOK()

	tid := int64f(alice.Op("post", map[string]any{"subject": "Peek loop", "b": "@bob first"}).MustOK().Field("t"))
	mid := int64f(alice.Op("post", map[string]any{"t": tid, "b": "@bob second"}).MustOK().Field("i"))

	peek := bob.Op("unread", map[string]any{"advance": 0}).MustOK()
	ms, _ := peek.Field("ms").([]any)
	if len(ms) != 2 {
		t.Fatalf("peek returned %d messages, want 2: %s", len(ms), peek.Text())
	}
	if peek.Field("adv") != nil {
		t.Errorf("a peek must not report an advanced cursor: %s", peek.Text())
	}
	if n := num(bob.Op("unread", map[string]any{"advance": 0}).MustOK().Field("n")); n != 2 {
		t.Errorf("second peek returned %v messages, want 2 - advance=0 must not consume", n)
	}

	// answer, then clear only up to the highest message handled
	bob.Op("post", map[string]any{"t": tid, "b": "@alice noted"}).MustOK()
	bob.Op("seen", map[string]any{"seq": mid}).MustOK()
	if n := num(bob.Op("unread", map[string]any{"advance": 0}).MustOK().Field("n")); n != 0 {
		t.Errorf("inbox after seen {seq:%d} still holds %v messages, want 0", mid, n)
	}

	// a cursor never moves back, so an older seq cannot resurrect a consumed message
	bob.Op("seen", map[string]any{"seq": 1}).MustOK()
	if n := num(bob.Op("unread", map[string]any{"advance": 0}).MustOK().Field("n")); n != 0 {
		t.Errorf("an older seq reopened the inbox (%v messages); cursors must be monotonic", n)
	}

	// the plain unread is the consuming variant: it reports what it consumed in adv
	third := int64f(alice.Op("post", map[string]any{"t": tid, "b": "@bob third"}).MustOK().Field("i"))
	got := bob.Op("unread", map[string]any{}).MustOK()
	if num(got.Field("n")) != 1 || int64f(got.Field("adv")) != third {
		t.Errorf("advancing unread = %s, want n=1 and adv=%d", got.Text(), third)
	}
	if n := num(bob.Op("unread", map[string]any{"advance": 0}).MustOK().Field("n")); n != 0 {
		t.Errorf("advancing unread left %v messages behind, want 0", n)
	}
}
