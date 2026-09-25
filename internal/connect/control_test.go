package connect

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// sockDir is a short temp directory: t.TempDir() embeds the test name and can push a socket path
// past maxSockPath.
func sockDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ac")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// staleSocket leaves a socket file nobody listens on, as a crashed connector does.
func staleSocket(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(dir, "control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
}

func TestControlRoundTrip(t *testing.T) {
	dir := sockDir(t)
	staleSocket(t, dir) // Serve replaces it
	var mu sync.Mutex
	var got []Request
	st := &Status{PID: 42, Agent: "mybot", Harness: "claude", Phase: "running", Turn: 7,
		Reason: "tag", State: "turn", LastPoll: "2026-09-25T10:00:00Z", Session: "s1", VersionWarning: "old"}
	srv, err := Serve(dir, nil, func(r Request) Response {
		mu.Lock()
		got = append(got, r)
		mu.Unlock()
		if r.Op == "status" {
			return Response{OK: true, Status: st}
		}
		return Response{OK: true, Text: "queued"}
	})
	if err != nil {
		t.Fatal(err)
	}

	r, err := Call(dir, Request{Op: "status"})
	if err != nil || !r.OK || !reflect.DeepEqual(r.Status, st) {
		t.Fatalf("status = %+v %+v, %v", r, r.Status, err)
	}
	r, err = Call(dir, Request{Op: "wake", Text: "hello\nthere"})
	if err != nil || !r.OK || r.Text != "queued" {
		t.Fatalf("wake = %+v, %v", r, err)
	}
	mu.Lock()
	if want := []Request{{Op: "status"}, {Op: "wake", Text: "hello\nthere"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("handler saw %+v", got)
	}
	mu.Unlock()
	if fi, err := os.Stat(filepath.Join(dir, "control.sock")); err != nil {
		t.Fatal(err)
	} else if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v, want 0600", fi.Mode())
	}

	srv.Close()
	if _, err := os.Stat(filepath.Join(dir, "control.sock")); !os.IsNotExist(err) {
		t.Fatalf("socket left after Close: %v", err)
	}
	if _, err := Call(dir, Request{Op: "status"}); err == nil {
		t.Fatal("Call after Close succeeded")
	}
}

func TestControlBadRequest(t *testing.T) {
	dir := sockDir(t)
	srv, err := Serve(dir, nil, func(Request) Response { t.Error("handler called"); return Response{} })
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	c, err := net.Dial("unix", filepath.Join(dir, "control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write([]byte("not json\n"))
	b := make([]byte, 512)
	k, err := c.Read(b)
	if err != nil || !strings.HasPrefix(string(b[:k]), `{"ok":false,"error":`) {
		t.Fatalf("reply %q, %v", b[:k], err)
	}
}

func TestControlEmptyError(t *testing.T) {
	dir := sockDir(t)
	srv, err := Serve(dir, nil, func(Request) Response { return Response{} })
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if r, err := Call(dir, Request{Op: "stop"}); err != nil || r.OK || r.Error != "request failed" {
		t.Fatalf("reply %+v, %v", r, err)
	}
}

func TestControlPathTooLong(t *testing.T) {
	dir := filepath.Join(sockDir(t), strings.Repeat("x", maxSockPath))
	_, err := Serve(dir, nil, func(Request) Response { return Response{} })
	if err == nil || !strings.Contains(err.Error(), "control socket path too long") {
		t.Fatalf("err = %v", err)
	}
}

// TestControlConcurrent runs 20 calls at once, notes and status mixed (go test -race).
func TestControlConcurrent(t *testing.T) {
	dir := sockDir(t)
	notes, err := LoadNotes(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := Serve(dir, notes, func(Request) Response { return Response{OK: true, Status: &Status{PID: 7}} })
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			req := Request{Op: "status"}
			if i%2 == 0 {
				req = Request{Op: "note", Action: "set", ID: fmt.Sprint("n", i), Note: "x"}
			}
			if r, err := Call(dir, req); err != nil || !r.OK {
				t.Errorf("call %d: %+v, %v", i, r, err)
			}
		})
	}
	wg.Wait()
	if got := len(notes.List()); got != 10 {
		t.Fatalf("%d notes, want 10", got)
	}
}

func TestControlCloseWaits(t *testing.T) {
	dir := sockDir(t)
	entered, release := make(chan struct{}), make(chan struct{})
	srv, err := Serve(dir, nil, func(Request) Response {
		close(entered)
		<-release
		return Response{OK: true, Text: "late"}
	})
	if err != nil {
		t.Fatal(err)
	}
	reply := make(chan Response, 1)
	go func() {
		r, err := Call(dir, Request{Op: "wake"})
		if err != nil {
			t.Error(err)
		}
		reply <- r
	}()
	<-entered
	closed := make(chan struct{})
	go func() { srv.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("Close returned with a request in flight")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	<-closed
	if r := <-reply; !r.OK || r.Text != "late" {
		t.Fatalf("in-flight reply %+v", r)
	}
}

func TestControlNotes(t *testing.T) {
	dir := sockDir(t)
	notes, err := LoadNotes(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := Serve(dir, notes, func(Request) Response { t.Error("handler called for a note"); return Response{} })
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	note := func(action, id, text string) Response {
		t.Helper()
		r, err := Call(dir, Request{Op: "note", Action: action, ID: id, Note: text})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := note("set", "plan", "step 1\nstep 2"); !r.OK {
		t.Fatalf("set: %+v", r)
	}
	if r := note("get", "plan", ""); !r.OK || r.Text != "step 1\nstep 2" {
		t.Fatalf("get: %+v", r)
	}
	if r := note("list", "", ""); !r.OK || !reflect.DeepEqual(r.Notes, []NoteHead{{"plan", "step 1"}}) {
		t.Fatalf("list: %+v", r)
	}
	if r := note("delete", "plan", ""); !r.OK {
		t.Fatalf("delete: %+v", r)
	}
	if r := note("get", "plan", ""); r.OK || r.Error == "" {
		t.Fatalf("get of a deleted note: %+v", r)
	}
	if r := note("set", "bad id", "x"); r.OK || r.Error == "" {
		t.Fatalf("bad id: %+v", r)
	}
	if r := note("frob", "", ""); r.OK {
		t.Fatalf("unknown action: %+v", r)
	}
}

// fakeConnector serves status for home/.aif-connect/<name> with the given pid.
func fakeConnector(t *testing.T, home, name string, pid int) string {
	t.Helper()
	dir := filepath.Join(home, ".aif-connect", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	srv, err := Serve(dir, nil, func(Request) Response {
		return Response{OK: true, Status: &Status{PID: pid, Agent: strings.Split(name, "@")[0], State: "idle"}}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	return dir
}

func TestListAndResolve(t *testing.T) {
	home := sockDir(t)
	if _, err := Resolve(home, ""); err == nil {
		t.Fatal("empty to with nothing running resolved")
	}
	mybot := fakeConnector(t, home, "mybot@h1", 101)
	staleSocket(t, filepath.Join(home, ".aif-connect", "ghost@h1"))

	if dir, err := Resolve(home, ""); err != nil || dir != mybot {
		t.Fatalf("empty to, one running: %q %v", dir, err)
	}
	other := fakeConnector(t, home, "other@h2", 102)

	found := List(home)
	if len(found) != 2 || found[0].Dir != mybot || found[0].Name != "mybot@h1" || found[0].Status.PID != 101 ||
		found[1].Dir != other || found[1].Status.PID != 102 {
		t.Fatalf("list = %+v", found)
	}

	for to, want := range map[string]string{"101": mybot, "102": other, "mybot": mybot, "other": other, "other@h2": other} {
		if dir, err := Resolve(home, to); err != nil || dir != want {
			t.Fatalf("Resolve(%q) = %q %v, want %q", to, dir, err, want)
		}
	}
	for _, to := range []string{"", "ghost", "ghost@h1", "999", "mybot@h2"} {
		_, err := Resolve(home, to)
		if err == nil {
			t.Fatalf("Resolve(%q) succeeded", to)
		}
		if !strings.Contains(err.Error(), "mybot@h1") || !strings.Contains(err.Error(), "other@h2") {
			t.Fatalf("Resolve(%q) error does not list the running ones: %v", to, err)
		}
	}

	fakeConnector(t, home, "mybot@h3", 103)
	if _, err := Resolve(home, "mybot"); err == nil || !strings.Contains(err.Error(), "mybot@h3") {
		t.Fatalf("ambiguous bare name: %v", err)
	}

	// An agent named 102 (PID 104): "102" is other's PID, and a PID match wins.
	digits := fakeConnector(t, home, "102@h4", 104)
	if dir, err := Resolve(home, "102"); err != nil || dir != other {
		t.Fatalf("Resolve(102) = %q %v, want the PID match %q", dir, err, other)
	}
	if dir, err := Resolve(home, "102@h4"); err != nil || dir != digits {
		t.Fatalf("Resolve(102@h4) = %q %v", dir, err)
	}
}
