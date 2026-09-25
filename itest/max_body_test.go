package itest

import (
	"strings"
	"testing"

	"github.com/dshein-alt/aif/internal/harness"
)

// TestMaxBodyZeroReturnsFull locks the documented meaning of max_body: the param docs promise
// "default 400, 0 = full", so an explicit max_body=0 must return the body whole while an absent
// max_body still truncates at 400 runes. Exercises both unread and feed, the two endpoints that
// shape message bodies.
func TestMaxBodyZeroReturnsFull(t *testing.T) {
	r := harness.New(t, false)
	alice := r.Join("alice")
	bob := r.Join("bob")

	long := strings.Repeat("x", 600)
	tid := int64f(alice.Op("post", map[string]any{"subject": "Long body", "b": "@bob tagging you"}).MustOK().Field("t"))
	mid := int64f(alice.Op("post", map[string]any{"t": tid, "b": long}).MustOK().Field("i"))

	// unread: max_body=0 must return the body whole
	full := bob.Get("/api/unread?advance=0&max_body=0&limit=500").MustOK().JSON()
	body, truncated := findMsgBody(t, full["ms"], mid)
	if len([]rune(body)) != 600 || truncated {
		t.Fatalf("unread max_body=0 truncated the body: len=%d truncated=%v", len([]rune(body)), truncated)
	}

	// unread: no max_body must still default to 400
	clipped := bob.Get("/api/unread?advance=0&limit=500").MustOK().JSON()
	body, truncated = findMsgBody(t, clipped["ms"], mid)
	if len([]rune(body)) != 400 || !truncated {
		t.Fatalf("unread without max_body did not truncate at 400: len=%d truncated=%v", len([]rune(body)), truncated)
	}

	// feed: max_body=0 must return the body whole
	feedFull := alice.Get("/api/feed?since=" + itoa(mid-1) + "&max_body=0").MustOK().JSON()
	body, truncated = findMsgBody(t, feedFull["ms"], mid)
	if len([]rune(body)) != 600 || truncated {
		t.Fatalf("feed max_body=0 truncated the body: len=%d truncated=%v", len([]rune(body)), truncated)
	}

	// feed: no max_body must still default to 400
	feedClipped := alice.Get("/api/feed?since=" + itoa(mid-1)).MustOK().JSON()
	body, truncated = findMsgBody(t, feedClipped["ms"], mid)
	if len([]rune(body)) != 400 || !truncated {
		t.Fatalf("feed without max_body did not truncate at 400: len=%d truncated=%v", len([]rune(body)), truncated)
	}
}

// findMsgBody locates the message with the given id in a decoded "ms" list and reports its body
// and whether the server marked it truncated ("tr":1).
func findMsgBody(t *testing.T, ms any, mid int64) (string, bool) {
	t.Helper()
	list, _ := ms.([]any)
	for _, raw := range list {
		m, _ := raw.(map[string]any)
		if int64f(m["i"]) == mid {
			b, _ := m["b"].(string)
			_, truncated := m["tr"]
			return b, truncated
		}
	}
	t.Fatalf("message %d not found in %v", mid, ms)
	return "", false
}
