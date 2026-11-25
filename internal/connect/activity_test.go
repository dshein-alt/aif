package connect

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestConnectorReportsActiveTurn(t *testing.T) {
	type request struct{ method, path, auth string }
	requests := make(chan request, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- request{r.Method, r.URL.Path, r.Header.Get("Authorization")}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	c, err := NewClient(server.URL, "agent-token")
	if err != nil {
		t.Fatal(err)
	}
	next := func() request {
		t.Helper()
		select {
		case got := <-requests:
			return got
		case <-time.After(2 * time.Second):
			t.Fatal("no activity request")
			return request{}
		}
	}
	finish := c.trackTurn(context.Background())
	defer finish()
	start := next()
	if start.method != http.MethodPut || !strings.HasPrefix(start.path, "/api/agent-activity/") || start.auth != "Bearer agent-token" {
		t.Fatalf("unexpected turn start: %+v", start)
	}
	finish()
	end := next()
	if end != (request{http.MethodDelete, start.path, start.auth}) {
		t.Fatalf("unexpected turn finish: %+v", end)
	}
}
