package itest

import (
	"strings"
	"testing"

	"aif/internal/config"
	"aif/internal/harness"
)

// TestArgAndSortValidation locks the two argument-validation error contracts: an unknown op arg
// replies with the sorted list of accepted args, and a bad threads sort names the valid values.
func TestArgAndSortValidation(t *testing.T) {
	r := harness.New(t, false)
	alice := r.Join("alice")

	g := alice.Op("post", map[string]any{"subject": "x", "b": "y", "bogus": 1})
	if g.Code != 400 || g.Str("err") != "bad_request" || !strings.Contains(g.Str("msg"), "unknown arg") ||
		!strings.Contains(g.Str("msg"), "accepted args") {
		t.Errorf("unknown arg = %d %s, want 400 bad_request listing accepted args", g.Code, g.Text())
	}

	s := alice.Op("threads", map[string]any{"sort": "sideways"})
	if s.Code != 400 || !strings.Contains(s.Str("msg"), "sort must be one of") {
		t.Errorf("bad sort = %d %s, want 400 'sort must be one of'", s.Code, s.Text())
	}
}

// TestUIThreadNotFound covers the guarded UI thread page when the op behind it fails: the handler
// renders the apiMsg text with the API status (statusOf), here a 404 for a missing thread.
func TestUIThreadNotFound(t *testing.T) {
	r := harness.NewWith(t, true, func(c *config.Config) { c.WebToken = webPW })
	c := uiLogin(t, r, webPW)

	if g := c.Get("/ui/thread/999999"); g.Code != 404 || !strings.Contains(g.Text(), "Not found") {
		t.Errorf("/ui/thread/999999 = %d, want 404 Not found", g.Code)
	}
}
