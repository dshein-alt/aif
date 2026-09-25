package driver

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// proc is the child-process plumbing every driver embeds: a line-oriented stdin writer, a
// stdout line channel, stderr re-emitted through Launch.Log, and the lifecycle. One proc runs
// one process; a restart makes a new proc.
//
// Use: p := newProc(launch); cfg, _ := p.TempFile(...) (argv may name it); p.start(args...);
// read p.Lines() until it closes (the process exited), then ExitCode(). Stop and Cleanup on the
// way out. A driver's own Stop(ctx) shadows the promoted proc.Stop; call d.proc.Stop().
//
// On Unix the harness runs in its own process group (proc_unix.go), and Stop signals the whole
// group: the turn timeout exists for a hung tool, which must die with the harness rather than
// linger as an orphan, and Ctrl-C in the connector's terminal must not reach the harness directly
// (SIGINT to the connector finishes the running turn first). Once the harness has exited, anything
// left in its group is killed too. Tools that setsid or otherwise detach escape the group.
type proc struct {
	launch Launch
	log    func(string)
	grace  time.Duration // SIGTERM → kill

	cmd      *exec.Cmd
	stdin    io.WriteCloser
	sendMu   sync.Mutex
	lines    chan string
	exited   chan struct{}
	stopping chan struct{} // closed by Stop: stdout lines nobody takes are discarded from then on
	stopOnce sync.Once
	code     int
	temps    []string
}

func newProc(l Launch) *proc {
	log := l.Log
	if log == nil {
		log = func(string) {}
	}
	return &proc{launch: l, log: log, grace: 10 * time.Second, lines: make(chan string),
		exited: make(chan struct{}), stopping: make(chan struct{})}
}

// start spawns launch.Bin with args in launch.Cwd, the connector's environment plus launch.Env.
func (p *proc) start(args ...string) error {
	cmd := exec.Command(p.launch.Bin, args...)
	cmd.Dir = p.launch.Cwd
	cmd.Env = append(cmd.Environ(), p.launch.Env...) // Environ sets PWD to Dir
	ownGroup(cmd)
	// A grandchild (a tool the harness spawned) may keep stdout open after the harness dies:
	// WaitDelay closes the pipes 1 s after the death so it cannot hold up exit detection. It does
	// not bound a stalled consumer: Wait also waits for the copy goroutine, which blocks on an
	// undrained Lines for as long as nobody reads it. Stop closes stopping to release it.
	cmd.WaitDelay = time.Second
	stdout := &lineWriter{emit: func(l string) {
		if p.launch.Verbose {
			p.log("<< " + l)
		}
		select {
		case p.lines <- l:
		case <-p.stopping:
		}
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
		reapGroup(cmd.Process.Pid)
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
// Once Stop has been called, lines nobody takes are discarded.
func (p *proc) Lines() <-chan string { return p.lines }

// Exited fires once the process has exited and Lines is closed.
func (p *proc) Exited() <-chan struct{} { return p.exited }

// ExitCode is valid after Exited fired; -1 when killed by a signal.
func (p *proc) ExitCode() int { return p.code }

// Send writes one line to the harness's stdin. It blocks while the harness is not reading stdin
// (the pipe is full); only Stop, by ending the process, unblocks it.
func (p *proc) Send(line string) error {
	p.sendMu.Lock()
	defer p.sendMu.Unlock()
	if p.stdin == nil {
		return errors.New("driver: Send before start")
	}
	_, err := io.WriteString(p.stdin, line+"\n")
	return err
}

// Stop asks the process group to exit (SIGTERM; on Windows it kills at once) and kills it if the
// harness is still alive after the grace period. It returns once the process has exited, whether
// or not anyone still reads Lines.
func (p *proc) Stop() error {
	if p.cmd == nil {
		return nil
	}
	p.stopOnce.Do(func() { close(p.stopping) })
	select {
	case <-p.exited:
		return nil
	default:
	}
	terminate(p.cmd.Process)
	select {
	case <-p.exited:
	case <-time.After(p.grace):
		kill(p.cmd.Process)
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

// maxLine caps one line: a longer run without a newline is emitted as is and the buffer reset, so
// a runaway harness cannot grow memory without bound. A var so tests can lower it.
var maxLine = 64 << 20

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
			if len(w.buf) > maxLine {
				w.flush()
			}
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
