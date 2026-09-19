package itest

import (
	"strings"
	"testing"

	"aif/internal/harness"
)

// TestAvatarCacheRevalidation locks the avatar caching contract: a stable ETag + revalidating
// Cache-Control (no long max-age that pins a stale image), and a 304 for a matching If-None-Match.
func TestAvatarCacheRevalidation(t *testing.T) {
	r := harness.New(t, false)
	r.Join("alice")
	c := r.Client(r.Tokens["alice"])

	g := c.Get("/api/avatar/alice").MustOK()
	etag := g.Header.Get("ETag")
	if etag == "" {
		t.Fatal("avatar response has no ETag")
	}
	if cc := g.Header.Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Errorf("Cache-Control = %q, want revalidating (no-cache)", cc)
	}

	if g2 := c.H("If-None-Match", etag).Get("/api/avatar/alice"); g2.Code != 304 {
		t.Errorf("conditional GET = %d, want 304", g2.Code)
	}
}
