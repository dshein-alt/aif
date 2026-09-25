package driver

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"
)

// proc is the child-process plumbing every driver embeds: a line-oriented stdin writer, a
// stdout line channel, stderr re-emitted through Launch.Log, and the lifecycle. One proc runs
// one process; a restart makes a new proc.
//
// Use: p := newProc(launch); cfg, _ := p.TempFile(...) (argv may name it); p.start(args...);
// read p.Lines() until it closes (the process exited), then ExitCode(). Stop and Cleanup on the
// way out. A driver's own Stop(ctx) shadows the promoted proc.Stop; call d.proc.Stop().
type proc struct {
	launch Launch
	log    func(string)
	grace  time.Duration // SIGTERM → kill

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	sendMu sync.Mutex
	lines  chan string
	exited chan struct{}
	code   int
	temps  []string
}

func newProc(l Launch) *proc {
	log := l.Log
	if log == nil {
		log = func(string) {}
	}
	return &proc{launch: l, log: log, grace: 10 * time.Second, lines: make(chan string), exited: make(chan struct{})}
}

// start spawns launch.Bin with args in launch.Cwd, the connector's environment plus launch.Env.
func (p *proc) start(args ...string) error {
	cmd := exec.Command(p.launch.Bin, args...)
	cmd.Dir = p.launch.Cwd
	cmd.Env = append(cmd.Environ(), p.launch.Env...) // Environ sets PWD to Dir
	// A grandchild (a tool the harness spawned) may keep stdout open after the harness dies;
	// don't let it hold up exit detection. ponytail: lines not drained within 1 s of the death
	// are dropped, and grandchildren outlive a kill; add a process group if either bites.
	cmd.WaitDelay = time.Second
	stdout := &lineWriter{emit: func(l string) {
		if p.launch.Verbose {
			p.log("<< " + l)
		}
		p.lines <- l
	}}
	stderr := &lineWriter{emit: func(l string) { p.log("harness: " + l) }}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	p.cmd, p.stdin = cmd, stdin
	go func() {
		_ = cmd.Wait() // the exit code is all we need; ErrWaitDelay still has ProcessState
		stdout.flush()
		stderr.flush()
		p.code = cmd.ProcessState.ExitCode()
		close(p.lines)
		close(p.exited)
	}()
	return nil
}

// Lines delivers stdout one line at a time (without the newline); it closes when the process
// has exited, just before Exited fires. The driver must keep reading it: stdout blocks otherwise.
func (p *proc) Lines() <-chan string { return p.lines }

// Exited fires once the process has exited and Lines is closed.
func (p *proc) Exited() <-chan struct{} { return p.exited }

// ExitCode is valid after Exited fired; -1 when killed by a signal.
func (p *proc) ExitCode() int { return p.code }

// Send writes one line to the harness's stdin.
func (p *proc) Send(line string) error {
	p.sendMu.Lock()
	defer p.sendMu.Unlock()
	_, err := io.WriteString(p.stdin, line+"\n")
	return err
}

// Stop asks the process to exit (SIGTERM; on Windows it kills at once) and kills it if it is
// still alive after the grace period. It returns once the process has exited.
func (p *proc) Stop() error {
	if p.cmd == nil {
		return nil
	}
	select {
	case <-p.exited:
		return nil
	default:
	}
	if runtime.GOOS == "windows" {
		_ = p.cmd.Process.Kill()
	} else {
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
	}
	select {
	case <-p.exited:
	case <-time.After(p.grace):
		_ = p.cmd.Process.Kill()
		<-p.exited
	}
	return nil
}

// TempFile writes <StateDir>/<name> with mode 0600 and returns its path; Cleanup removes it.
func (p *proc) TempFile(name, content string) (string, error) {
	path := filepath.Join(p.launch.StateDir, name)
	_ = os.Remove(path) // WriteFile keeps an existing file's mode
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return "", err
	}
	p.temps = append(p.temps, path)
	return path, nil
}

// Cleanup removes every file TempFile created.
func (p *proc) Cleanup() {
	for _, path := range p.temps {
		_ = os.Remove(path)
	}
	p.temps = nil
}

// lineWriter splits what exec's copy goroutine writes into lines. Only that goroutine calls
// Write; flush runs after Wait, once the copying is over.
type lineWriter struct {
	buf  []byte
	emit func(string)
}

func (w *lineWriter) Write(b []byte) (int, error) {
	w.buf = append(w.buf, b...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			return len(b), nil
		}
		w.emit(string(bytes.TrimSuffix(w.buf[:i], []byte("\r"))))
		w.buf = w.buf[i+1:]
	}
}

func (w *lineWriter) flush() {
	if len(w.buf) > 0 {
		w.emit(string(w.buf))
		w.buf = nil
	}
}
