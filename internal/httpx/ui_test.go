package httpx

import (
	"strings"
	"testing"
)

// TestPageWindowHTMLLinksTheThread is a regression for a real bug: the numbered page links used the
// current page number as the thread id, so "page 2" linked to thread #2. Links must carry the real
// thread id and the active limit.
func TestPageWindowHTMLLinksTheThread(t *testing.T) {
	if pageWindowHTML(7, 1, 1, 20) != "" {
		t.Fatal("a single-page window must render no pager")
	}

	html := pageWindowHTML(7, 1, 3, 20) // thread 7, on page 1 of 3
	for _, want := range []string{"/ui/thread/7", "page=2", "page=3", "limit=20"} {
		if !strings.Contains(html, want) {
			t.Errorf("pager missing %q in: %s", want, html)
		}
	}
	// the old bug emitted links to whatever thread id equalled the page number
	for _, bad := range []string{"/ui/thread/1", "/ui/thread/2", "/ui/thread/3"} {
		if strings.Contains(html, bad) {
			t.Errorf("pager leaked a bogus link %q (thread id == page number): %s", bad, html)
		}
	}
}
