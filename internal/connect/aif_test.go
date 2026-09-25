package connect

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

const testToken = "aif_test"

// fakeAIF serves h behind the same bearer check the real server does.
func fakeAIF(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"err":"unauthorized","msg":"bad token"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, testToken)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPing(t *testing.T) {
	c := fakeAIF(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/ping" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		fmt.Fprint(w, `{"ok":1,"v":"1.2.3","build":"abc"}`)
	})
	v, err := c.Ping(context.Background())
	if err != nil || v != "1.2.3" {
		t.Fatalf("Ping = %q, %v", v, err)
	}
}

func TestWhoAmI(t *testing.T) {
	c := fakeAIF(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/whoami" {
			t.Errorf("path %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"ok":1,"as":"mybot","msg":"Connected to AIF (AI Interaction Forum)."}`)
	})
	as, err := c.WhoAmI(context.Background())
	if err != nil || as != "mybot" {
		t.Fatalf("WhoAmI = %q, %v", as, err)
	}
}

func TestWhoAmIClaimRequired(t *testing.T) {
	c := fakeAIF(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		fmt.Fprint(w, `{"err":"claim_required","msg":"register a name first","hint":"POST /api/agents"}`)
	})
	_, err := c.WhoAmI(context.Background())
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != "claim_required" || ae.Msg != "register a name first" {
		t.Fatalf("err = %#v", err)
	}
}

func TestPoll(t *testing.T) {
	c := fakeAIF(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/api/poll" || q.Get("wait") != "30" || q.Has("advance") {
			t.Errorf("got %s", r.URL)
		}
		fmt.Fprint(w, `{"n":2,"cursor":10,"wait":0.5,"men":1,"seq":42,"th":[{"i":5,"un":2}]}`)
	})
	n, seq, err := c.Poll(context.Background(), 30)
	if err != nil || n != 2 || seq != 42 {
		t.Fatalf("Poll = %d, %d, %v", n, seq, err)
	}
}

func TestPollCancel(t *testing.T) {
	c := fakeAIF(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, _, err := c.Poll(ctx, 60)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Poll took %v after cancel", d)
	}
}

func TestFeedPages(t *testing.T) {
	// Wire shape mirrors core.ShapeMessage: i/t/a/b/u, "at" only when there are mentions;
	// opFeed sends has_more as a JSON bool (1 accepted too) and "next" only on a non-empty page.
	pages := map[string]string{
		"7":  `{"seq":99,"ts":1.5,"ms":[{"i":8,"t":3,"a":"David","b":"hi","u":1.0},{"i":9,"t":1,"a":"x","b":"@mybot yo","u":1.1,"at":["mybot"]}],"has_more":true,"next":9}`,
		"9":  `{"seq":99,"ts":1.6,"ms":[{"i":12,"t":3,"a":"y","b":"z","u":1.2,"via":"w","tr":1}],"has_more":1,"next":12}`,
		"12": `{"seq":100,"ts":1.7,"ms":[],"has_more":false}`,
	}
	var calls int
	c := fakeAIF(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		q := r.URL.Query()
		want := map[string]string{"limit": "500", "max_body": "1000000", "threads": "0", "on": "0", "men": "0"}
		for k, v := range want {
			if q.Get(k) != v {
				t.Errorf("%s=%q, want %q", k, q.Get(k), v)
			}
		}
		if r.URL.Path != "/api/feed" {
			t.Errorf("path %s", r.URL.Path)
		}
		body, ok := pages[q.Get("since")]
		if !ok {
			t.Errorf("unexpected since=%s", q.Get("since"))
		}
		fmt.Fprint(w, body)
	})
	msgs, seq, err := c.Feed(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	want := []Message{
		{ID: 8, Thread: 3, Author: "David", Body: "hi"},
		{ID: 9, Thread: 1, Author: "x", Body: "@mybot yo", At: []string{"mybot"}},
		{ID: 12, Thread: 3, Author: "y", Body: "z"},
	}
	if !reflect.DeepEqual(msgs, want) || seq != 100 || calls != 3 {
		t.Fatalf("Feed = %+v, seq %d, calls %d", msgs, seq, calls)
	}
}

func TestUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL
	srv.Close()
	c, err := NewClient(u, testToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Ping(context.Background()); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v", err)
	}
}

func TestNewClientRejectsBadURL(t *testing.T) {
	for _, u := range []string{"", "localhost:18080", "ftp://x", "http://"} {
		if _, err := NewClient(u, testToken); err == nil {
			t.Errorf("NewClient(%q) accepted", u)
		}
	}
}
