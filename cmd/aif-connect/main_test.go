package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dshein-alt/aif/internal/connect"
	"github.com/dshein-alt/aif/internal/version"
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
	case "hang": // never ready; a SIGTERM (the default action) ends it early
		time.Sleep(30 * time.Second)
		os.Exit(6)
	case "sigint": // the foreground connector's signal wiring, then deaf to the context
		ctx, _ := stopContext(func(s string) { fmt.Fprintln(os.Stderr, s) })
		fmt.Println("ready")
		<-ctx.Done()
		time.Sleep(30 * time.Second)
		os.Exit(5)
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
	code, out, _ := daemonLimit(t, mode, dir, 20*time.Second)
	return code, out
}

func daemonLimit(t *testing.T, mode, dir string, limit time.Duration) (code int, stdout, stderr string) {
	t.Helper()
	t.Setenv("AIF_CONNECT_HELPER", mode)
	t.Setenv("AIF_CONNECT_HELPER_DIR", dir)
	errf, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer errf.Close()
	var out bytes.Buffer
	code = daemonize(os.Args[0], []string{"-test.run=^TestHelperProcess$"}, dir, limit, &out, errf)
	b, err := os.ReadFile(errf.Name())
	if err != nil {
		t.Fatal(err)
	}
	return code, out.String(), string(b)
}

func TestVersion(t *testing.T) {
	code, out, _ := call(t, noEnv, "", "--version")
	if code != 0 || out != "aif-connect "+version.Version+" (dev)\n" {
		t.Fatalf("code %d, out %q", code, out)
	}
}

func TestUsageErrors(t *testing.T) {
	t.Setenv("HOME", sockDir(t))
	for _, args := range [][]string{
		{"--bogus"}, {}, {"--to=x"}, {"frob"}, {"--config=x.json", "status"}, {"--daemon", "list"},
		{"list", "x"}, {"--to=x", "list"}, {"status", "x"}, {"wake"}, {"wake", "a", "b"},
		{"note"}, {"note", "get"}, {"note", "set"}, {"note", "set", "x"}, {"note", "list", "x"}, {"note", "frob", "x"},
		{"status", "--to=x"}, {"note", "get", "-to", "x"},
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
		{"from stdin\n", []string{"note", "set", "log", "-"}, 0, ""},
		{"", []string{"note", "set", "log", "-"}, 1, ""},                                   // would blank it
		{strings.Repeat("x", 16<<10) + "\n\n", []string{"note", "set", "log", "-"}, 1, ""}, // over the limit
		{"", []string{"note", "get", "log"}, 0, "from stdin\n"},
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

// note set ID - reads at most maxStdinNote bytes, so a longer input still arrives over the limit.
func TestNoteSetFromStdinLimit(t *testing.T) {
	var mu sync.Mutex
	var got string
	dir := sockDir(t)
	srv, err := connect.Serve(dir, nil, func(r connect.Request) connect.Response {
		mu.Lock()
		got = r.Note
		mu.Unlock()
		return connect.Response{OK: true}
	})
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
	full := strings.Repeat("x", 16<<10)
	for _, c := range []struct{ stdin, want string }{
		{full + "\n", full},
		{full + "xyz", full + "xy"},
	} {
		code, _, errs := call(t, env, c.stdin, "note", "set", "big", "-")
		mu.Lock()
		if code != 0 || got != c.want {
			t.Errorf("stdin of %d bytes: code %d, sent %d bytes, stderr %q", len(c.stdin), code, len(got), errs)
		}
		mu.Unlock()
	}
}

func TestChildArgs(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	fs.String("config", "", "")
	fs.String("bin", "", "")
	fs.Bool("daemon", false, "")
	fs.Bool("verbose", false, "")
	if err := fs.Parse([]string{"--daemon", "--config=/c.json", "--verbose", "--bin", "a b"}); err != nil {
		t.Fatal(err)
	}
	if got, want := childArgs(fs), []string{"--bin=a b", "--config=/c.json", "--verbose=true"}; !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestDaemonTimeoutStopsChild(t *testing.T) {
	start := time.Now()
	code, out, errs := daemonLimit(t, "hang", sockDir(t), time.Second)
	if code != 1 || out != "" || !strings.Contains(errs, "did not become ready within 1s; stopped it; see its stderr above") {
		t.Fatalf("code %d, out %q, stderr %q", code, out, errs)
	}
	if d := time.Since(start); d > 15*time.Second { // the child sleeps 30 s unless it was stopped
		t.Fatalf("took %v: the child was not stopped", d)
	}
}

// The first Ctrl-C cancels the context; the second ends the process by the default action even
// though nothing honors the context.
func TestSecondInterruptForces(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no SIGINT to send on Windows")
	}
	if signal.Ignored(os.Interrupt) { // inherited: once stopped, the child would ignore it again
		t.Skip("SIGINT is ignored here (a background job?)")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(), "AIF_CONNECT_HELPER=sigint")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	waitFor := func(r *bufio.Scanner, line string) {
		t.Helper()
		for r.Scan() {
			if strings.Contains(r.Text(), line) {
				return
			}
		}
		t.Fatalf("never saw %q", line)
	}
	waitFor(bufio.NewScanner(stdout), "ready")
	start := time.Now()
	cmd.Process.Signal(os.Interrupt)
	waitFor(bufio.NewScanner(stderr), "stopping; Ctrl-C again to force")
	cmd.Process.Signal(os.Interrupt)
	cmd.Wait()
	if code := cmd.ProcessState.ExitCode(); code != -1 || time.Since(start) > 15*time.Second {
		t.Fatalf("exit %d after %v; want killed by the second SIGINT at once", code, time.Since(start))
	}
}

// A relative bin path in the config resolves against the config's directory, --bin against the
// current one.
func TestBinResolution(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.json")
	body := `{"agent":"pi","goal":"g","aifUrl":"http://h:1","agentName":"b","agentToken":"aif_x","thread":8,"bin":"sub/nope"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, errs := call(t, noEnv, "", "--config="+path); !strings.Contains(errs, filepath.Join(dir, "sub", "nope")) {
		t.Fatalf("config bin: stderr %q", errs)
	}
	if _, _, errs := call(t, noEnv, "", "--config="+path, "--bin=sub/nope"); strings.Contains(errs, dir) || !strings.Contains(errs, "sub/nope") {
		t.Fatalf("--bin: stderr %q", errs)
	}
}
