package driver

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// logSink collects Launch.Log lines.
type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (s *logSink) log(l string) { s.mu.Lock(); s.lines = append(s.lines, l); s.mu.Unlock() }
func (s *logSink) has(l string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Contains(s.lines, l)
}

// startFake writes transcript, points the fake at it and starts a proc on it.
func startFake(t *testing.T, transcript string, l Launch) (*proc, *logSink) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "transcript")
	if err := os.WriteFile(path, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	sink := &logSink{}
	l.Bin, l.Log = fakeHarness(t, path), sink.log
	p := newProc(l)
	if err := p.start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })
	return p, sink
}

// collect reads lines until the channel closes, then waits for Exited.
func collect(t *testing.T, p *proc) []string {
	t.Helper()
	var got []string
	timeout := time.After(10 * time.Second)
	for {
		select {
		case l, ok := <-p.Lines():
			if !ok {
				select {
				case <-p.Exited():
					return got
				case <-timeout:
					t.Fatal("lines closed but Exited never fired")
				}
			}
			got = append(got, l)
		case <-timeout:
			t.Fatalf("no exit; lines so far %q", got)
		}
	}
}

func TestProcCwdEnvVerbose(t *testing.T) {
	cwd := t.TempDir()
	p, sink := startFake(t, "env: PWD\nenv: FAKE_X\nout: {\"a\":1}\nexit: 0\n",
		Launch{Cwd: cwd, Env: []string{"FAKE_X=hello"}, Verbose: true})
	got := collect(t, p)
	if want := []string{cwd, "hello", `{"a":1}`}; !slices.Equal(got, want) {
		t.Fatalf("lines %q, want %q", got, want)
	}
	if p.ExitCode() != 0 {
		t.Fatalf("exit code %d", p.ExitCode())
	}
	if !sink.has(`<< {"a":1}`) {
		t.Fatalf("verbose echo missing: %q", sink.lines)
	}
}

func TestProcNotVerboseNoEcho(t *testing.T) {
	p, sink := startFake(t, "out: x\nexit: 0\n", Launch{})
	collect(t, p)
	if sink.has("<< x") {
		t.Fatal("stdout echoed without Verbose")
	}
}

func TestProcSendMatchDynamicAndID(t *testing.T) {
	p, _ := startFake(t, `dynamic: id,sessionId
in: some banner
in: {"id":1,"method":"init","params":{"sessionId":"a","nested":[{"sessionId":"q","k":2}]}}
out: {"id":$id,"result":"ok"}
exit: 3
`, Launch{})
	if err := p.Send("some banner"); err != nil {
		t.Fatal(err)
	}
	if err := p.Send(`{"method":"init","id":"t7","params":{"nested":[{"k":2,"sessionId":"zzz"}],"sessionId":"b"}}`); err != nil {
		t.Fatal(err)
	}
	if got := collect(t, p); !slices.Equal(got, []string{`{"id":"t7","result":"ok"}`}) {
		t.Fatalf("lines %q", got)
	}
	if p.ExitCode() != 3 {
		t.Fatalf("exit code %d, want 3", p.ExitCode())
	}
}

func TestProcMismatchGoesToLog(t *testing.T) {
	p, sink := startFake(t, "in: {\"method\":\"x\",\"params\":{\"n\":1}}\n", Launch{})
	if err := p.Send(`{"method":"x","params":{"n":2}}`); err != nil {
		t.Fatal(err)
	}
	collect(t, p)
	if p.ExitCode() != 99 {
		t.Fatalf("exit code %d, want 99", p.ExitCode())
	}
	if want := `harness: MISMATCH expected={"method":"x","params":{"n":1}} got={"method":"x","params":{"n":2}}`; !sink.has(want) {
		t.Fatalf("log %q lacks %q", sink.lines, want)
	}
}

func TestProcStopGraceful(t *testing.T) {
	p, _ := startFake(t, "out: ready\n", Launch{})
	if l := <-p.Lines(); l != "ready" {
		t.Fatalf("got %q", l)
	}
	begin := time.Now()
	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(begin); d > 5*time.Second {
		t.Fatalf("SIGTERM did not stop the fake, took %v", d)
	}
	<-p.Exited()
}

func TestProcStopKillsAfterGrace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no SIGTERM: Stop kills at once")
	}
	p, _ := startFake(t, "ignore-sigterm:\nout: ready\n", Launch{})
	p.grace = 200 * time.Millisecond
	if l := <-p.Lines(); l != "ready" {
		t.Fatalf("got %q", l)
	}
	begin := time.Now()
	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(begin); d < 200*time.Millisecond {
		t.Fatalf("killed after %v, before the grace period", d)
	}
	select {
	case <-p.Exited():
	default:
		t.Fatal("Stop returned before the process exited")
	}
	if p.ExitCode() != -1 {
		t.Fatalf("exit code %d, want -1 (killed)", p.ExitCode())
	}
	if err := p.Send("late"); err == nil {
		t.Fatal("Send to a dead process succeeded")
	}
}

func TestProcTempFileCleanup(t *testing.T) {
	dir := t.TempDir()
	p := newProc(Launch{StateDir: dir})
	path, err := p.TempFile("mcp.json", `{"x":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, "mcp.json") {
		t.Fatalf("path %s", path)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
	if b, _ := os.ReadFile(path); string(b) != `{"x":1}` {
		t.Fatalf("content %q", b)
	}
	p.Cleanup()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("not removed: %v", err)
	}
}

func TestRegistry(t *testing.T) {
	_, err := New("nosuch")
	if !errors.Is(err, ErrUnknownHarness) || !strings.Contains(err.Error(), `"nosuch"`) {
		t.Fatalf("err %v", err)
	}
	Register("zz-test", func() Driver { return nil })
	Register("aa-test", func() Driver { return nil })
	defer delete(registry, "zz-test")
	defer delete(registry, "aa-test")
	if _, err := New("zz-test"); err != nil {
		t.Fatal(err)
	}
	if n := Names(); !slices.IsSorted(n) || !slices.Contains(n, "aa-test") {
		t.Fatalf("names %q", n)
	}
}

func TestProcStopUndrained(t *testing.T) {
	p, sink := startFake(t, "out: a\nout: b\nout: c\n", Launch{Verbose: true})
	p.grace = 2 * time.Second
	for !sink.has("<< a") { // the copy goroutine is now blocked handing "a" to nobody
		time.Sleep(10 * time.Millisecond)
	}
	done := make(chan struct{})
	go func() { _ = p.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(p.grace):
		t.Fatal("Stop hung on an undrained Lines")
	}
}

func TestProcSendBeforeStart(t *testing.T) {
	if err := newProc(Launch{}).Send("x"); err == nil {
		t.Fatal("Send before start succeeded")
	}
}

func TestFakeIDTokenAndDynamicAfterComment(t *testing.T) {
	p, _ := startFake(t, "# a comment\n\ndynamic: id\nin: {\"id\":5}\nout: {\"id\":$id,\"x\":\"$idle\"}\nexit: 0\n", Launch{})
	if err := p.Send(`{"id":7}`); err != nil {
		t.Fatal(err)
	}
	if got := collect(t, p); !slices.Equal(got, []string{`{"id":7,"x":"$idle"}`}) || p.ExitCode() != 0 {
		t.Fatalf("lines %q, exit %d", got, p.ExitCode())
	}
	p, sink := startFake(t, "out: {\"id\":$id}\n", Launch{})
	if got := collect(t, p); len(got) != 0 || p.ExitCode() != 98 {
		t.Fatalf("$id with no id: lines %q, exit %d, log %q", got, p.ExitCode(), sink.lines)
	}
}

func TestLineWriterCap(t *testing.T) {
	defer func(n int) { maxLine = n }(maxLine)
	maxLine = 8
	var got []string
	w := &lineWriter{emit: func(l string) { got = append(got, l) }}
	_, _ = w.Write([]byte("0123456789"))
	_, _ = w.Write([]byte("ab\n"))
	if !slices.Equal(got, []string{"0123456789", "ab"}) {
		t.Fatalf("lines %q", got)
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	Register("dup-test", func() Driver { return nil })
	defer delete(registry, "dup-test")
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate Register did not panic")
		}
	}()
	Register("dup-test", func() Driver { return nil })
}
