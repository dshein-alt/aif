package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dshein-alt/aif/internal/connect"
)

// sockDir is a short temp directory: t.TempDir() embeds the test name and can push a socket path
// past the Unix limit.
func sockDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ac")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func noEnv(string) string { return "" }

func call(t *testing.T, getenv func(string) string, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(args, strings.NewReader(stdin), &out, &errb, getenv)
	return code, out.String(), errb.String()
}

// TestHelperProcess is the daemon child in the handshake tests, not a test itself.
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv("AIF_CONNECT_HELPER")
	if mode == "" {
		return
	}
	switch mode {
	case "exit2":
		os.Exit(2)
	case "exit3": // outlive a few polls of somebody else's socket, then fail as a second connector does
		time.Sleep(time.Second)
		os.Exit(3)
	case "ready": // answer starting twice, then running, then leave
		var n atomic.Int32
		done := make(chan struct{}, 1)
		_, err := connect.Serve(os.Getenv("AIF_CONNECT_HELPER_DIR"), nil, func(connect.Request) connect.Response {
			phase := "starting"
			if n.Add(1) > 2 {
				phase = "running"
				select {
				case done <- struct{}{}:
				default:
				}
			}
			return connect.Response{OK: true, Status: &connect.Status{PID: os.Getpid(), Phase: phase}}
		})
		if err != nil {
			os.Exit(9)
		}
		select {
		case <-done:
			time.Sleep(500 * time.Millisecond) // let the reply reach the parent
			os.Exit(0)
		case <-time.After(30 * time.Second):
			os.Exit(8)
		}
	}
	os.Exit(7)
}

func daemonTest(t *testing.T, mode, dir string) (int, string) {
	t.Helper()
	t.Setenv("AIF_CONNECT_HELPER", mode)
	t.Setenv("AIF_CONNECT_HELPER_DIR", dir)
	errf, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer errf.Close()
	var out bytes.Buffer
	code := daemonize(os.Args[0], []string{"-test.run=^TestHelperProcess$"}, dir, 20*time.Second, &out, errf)
	return code, out.String()
}

func TestVersion(t *testing.T) {
	code, out, _ := call(t, noEnv, "", "--version")
	if code != 0 || out != "aif-connect 0.3.0 (dev)\n" {
		t.Fatalf("code %d, out %q", code, out)
	}
}

func TestUsageErrors(t *testing.T) {
	t.Setenv("HOME", sockDir(t))
	for _, args := range [][]string{
		{"--bogus"}, {}, {"--to=x"}, {"frob"}, {"--config=x.json", "status"}, {"--daemon", "list"},
		{"list", "x"}, {"--to=x", "list"}, {"status", "x"}, {"wake"}, {"wake", "a", "b"},
		{"note"}, {"note", "get"}, {"note", "set"}, {"note", "list", "x"}, {"note", "frob", "x"},
	} {
		code, _, errs := call(t, noEnv, "", args...)
		if code != 1 || !strings.Contains(errs, "usage:") {
			t.Errorf("%q: code %d, stderr %q", args, code, errs)
		}
	}
}

func TestDaemonChildFails(t *testing.T) {
	if code, out := daemonTest(t, "exit2", sockDir(t)); code != 2 || out != "" {
		t.Fatalf("code %d, out %q", code, out)
	}
}

func TestDaemonReady(t *testing.T) {
	code, out := daemonTest(t, "ready", sockDir(t))
	if pid, err := strconv.Atoi(strings.TrimSpace(out)); code != 0 || err != nil || pid == os.Getpid() {
		t.Fatalf("code %d, out %q", code, out)
	}
}

// A connector already running for the name answers running with its own PID: the parent ignores it
// and reports the child's exit 3.
func TestDaemonIgnoresForeignSocket(t *testing.T) {
	dir := sockDir(t)
	var asked atomic.Int32
	srv, err := connect.Serve(dir, nil, func(connect.Request) connect.Response {
		asked.Add(1)
		return connect.Response{OK: true, Status: &connect.Status{PID: os.Getpid(), Phase: "running"}}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if code, out := daemonTest(t, "exit3", dir); code != 3 || out != "" {
		t.Fatalf("code %d, out %q", code, out)
	}
	if asked.Load() == 0 {
		t.Fatal("the parent never polled the socket")
	}
}

func TestNote(t *testing.T) {
	t.Setenv("HOME", sockDir(t)) // nothing running there: only AIF_CONNECT_STATE can find the connector
	dir := sockDir(t)
	notes, err := connect.LoadNotes(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := connect.Serve(dir, notes, func(connect.Request) connect.Response { return connect.Response{} })
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	env := func(k string) string {
		if k == "AIF_CONNECT_STATE" {
			return dir
		}
		return ""
	}
	for _, c := range []struct {
		stdin string
		args  []string
		code  int
		out   string
	}{
		{"", []string{"note", "set", "plan", "step one\nstep two"}, 0, ""},
		{"from stdin\n", []string{"note", "set", "log"}, 0, ""},
		{"", []string{"note", "get", "plan"}, 0, "step one\nstep two\n"},
		{"", []string{"note", "list"}, 0, "log\tfrom stdin\nplan\tstep one\n"},
		{"", []string{"note", "delete", "plan"}, 0, ""},
		{"", []string{"note", "get", "plan"}, 1, ""},
	} {
		code, out, errs := call(t, env, c.stdin, c.args...)
		if code != c.code || out != c.out {
			t.Errorf("%q: code %d, out %q, stderr %q", c.args, code, out, errs)
		}
	}
	code, _, errs := call(t, noEnv, "", "note", "list")
	if code != 1 || !strings.Contains(errs, "not inside a connector turn; pass --to") {
		t.Fatalf("no env: code %d, stderr %q", code, errs)
	}
}

func TestControlVerbs(t *testing.T) {
	home := sockDir(t)
	t.Setenv("HOME", home)
	if code, out, _ := call(t, noEnv, "", "list"); code != 0 || out != "no connectors running\n" {
		t.Fatalf("list, none: code %d, out %q", code, out)
	}
	dir := connect.StateDir(home, "bot", "http_h_80")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []connect.Request
	srv, err := connect.Serve(dir, nil, func(r connect.Request) connect.Response {
		switch r.Op {
		case "status":
			return connect.Response{OK: true, Status: &connect.Status{PID: 42, Agent: "bot", Harness: "pi", Phase: "running", State: "idle", Turn: 3}}
		case "wake":
			mu.Lock()
			got = append(got, r)
			mu.Unlock()
			return connect.Response{OK: true, Text: "w1"}
		}
		return connect.Response{OK: true}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	for _, c := range []struct {
		args []string
		code int
		out  string
	}{
		{[]string{"list"}, 0, "42  bot@http_h_80  pi  idle  3\n"},
		{[]string{"status"}, 0, "pid: 42\nagent: bot\nharness: pi\nphase: running\nstate: idle\nturn: 3\nreason: \nlastPoll: \nsession: \n"},
		{[]string{"--to=bot", "wake", "hello"}, 0, "w1\n"},
		{[]string{"--to=42", "stop"}, 0, "ok\n"},
		{[]string{"--to=other", "stop"}, 1, ""},
	} {
		code, out, errs := call(t, noEnv, "", c.args...)
		if code != c.code || out != c.out {
			t.Errorf("%q: code %d, out %q, stderr %q", c.args, code, out, errs)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].Text != "hello" {
		t.Fatalf("wake requests %+v", got)
	}
}
