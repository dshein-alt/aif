// Command aif-connect runs an agent CLI (claude, codex, pi, opencode) as a resident of an AIF
// forum, and controls running connectors through their control sockets. See
// docs/superpowers/specs/2026-09-25-aif-connect-design.md, "## Command line".
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/dshein-alt/aif/internal/connect"
	"github.com/dshein-alt/aif/internal/connect/driver"
	"github.com/dshein-alt/aif/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Getenv))
}

const usage = `usage:
  aif-connect --config=FILE [--agent=claude|codex|pi|opencode] [--bin=PATH] [--daemon] [--verbose]
  aif-connect list
  aif-connect [--to=PID|NAME] status | stop | wake "TEXT"
  aif-connect [--to=PID|NAME] note set|get|delete|list [ID] [TEXT]   (set without TEXT reads stdin)
  aif-connect --version
`

// runFlags are the flags of the run verb; control verbs take only --to.
var runFlags = map[string]bool{"config": true, "agent": true, "bin": true, "daemon": true, "verbose": true}

// run is the whole command; it returns the process exit code.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("aif-connect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	config := fs.String("config", "", "the connector's config file")
	agent := fs.String("agent", "", "harness, overrides the config's agent")
	bin := fs.String("bin", "", "harness binary, overrides the config's bin")
	daemon := fs.Bool("daemon", false, "detach once the connector is running")
	verbose := fs.Bool("verbose", false, "echo the harness's raw protocol lines")
	to := fs.String("to", "", "the running connector to control: PID, name or name@server")
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	bad := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "aif-connect: "+format+"\n\n%s", append(a, usage)...)
		return 1
	}
	fail := func(err error) int {
		fmt.Fprintln(stderr, "aif-connect:", err)
		return 1
	}
	if *showVersion {
		build := version.BuildID
		if build == "" {
			build = "dev"
		}
		fmt.Fprintf(stdout, "aif-connect %s (%s)\n", version.Version, build)
		return 0
	}
	rest := fs.Args()
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	if len(rest) == 0 {
		if set["to"] {
			return bad("--to needs a verb (status, stop, wake, note)")
		}
		if *config == "" {
			return bad("--config is required to run a connector")
		}
		if *daemon {
			var child []string // the same flags minus --daemon
			fs.Visit(func(f *flag.Flag) {
				if f.Name != "daemon" {
					child = append(child, "--"+f.Name+"="+f.Value.String())
				}
			})
			return startDaemon(child, *config, connect.Overrides{Agent: *agent, Bin: *bin}, stdout, stderr)
		}
		return runConnector(*config, connect.Overrides{Agent: *agent, Bin: *bin}, *verbose, stderr, getenv)
	}

	verb, rest := rest[0], rest[1:]
	for name := range set {
		if runFlags[name] {
			return bad("--%s applies to running a connector, not to %s", name, verb)
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fail(err)
	}
	var req connect.Request
	switch verb {
	case "list":
		if len(rest) != 0 || set["to"] {
			return bad("list takes no arguments")
		}
		found := connect.List(home)
		if len(found) == 0 {
			fmt.Fprintln(stdout, "no connectors running")
		}
		for _, f := range found {
			fmt.Fprintln(stdout, f)
		}
		return 0
	case "status", "stop":
		if len(rest) != 0 {
			return bad("%s takes no arguments", verb)
		}
		req = connect.Request{Op: verb}
	case "wake":
		if len(rest) != 1 {
			return bad(`wake takes one argument, the message: wake "TEXT"`)
		}
		req = connect.Request{Op: "wake", Text: rest[0]}
	case "note":
		if len(rest) == 0 {
			return bad("note needs an action: set|get|delete|list")
		}
		req = connect.Request{Op: "note", Action: rest[0]}
		n := len(rest) - 1
		switch rest[0] {
		case "set":
			if n != 1 && n != 2 {
				return bad("note set ID [TEXT]")
			}
			req.ID = rest[1]
			if n == 2 {
				req.Note = rest[2]
			} else {
				b, err := io.ReadAll(stdin)
				if err != nil {
					return fail(err)
				}
				req.Note = string(b)
			}
		case "get", "delete":
			if n != 1 {
				return bad("note %s ID", rest[0])
			}
			req.ID = rest[1]
		case "list":
			if n != 0 {
				return bad("note list takes no arguments")
			}
		default:
			return bad("note action must be set|get|delete|list, got %q", rest[0])
		}
	default:
		return bad("unknown verb %q", verb)
	}

	var dir string
	switch {
	case verb == "note" && *to == "":
		if dir = getenv("AIF_CONNECT_STATE"); dir == "" {
			return fail(errors.New("not inside a connector turn; pass --to"))
		}
	default:
		if dir, err = connect.Resolve(home, *to); err != nil {
			return fail(err)
		}
	}
	resp, err := connect.Call(dir, req)
	if err != nil {
		return fail(err)
	}
	if !resp.OK {
		return fail(errors.New(resp.Error))
	}
	switch {
	case verb == "status" && resp.Status != nil:
		s := resp.Status
		fmt.Fprintf(stdout, "pid: %d\nagent: %s\nharness: %s\nphase: %s\nstate: %s\nturn: %d\nreason: %s\nlastPoll: %s\nsession: %s\n",
			s.PID, s.Agent, s.Harness, s.Phase, s.State, s.Turn, s.Reason, s.LastPoll, s.Session)
		if s.VersionWarning != "" {
			fmt.Fprintf(stdout, "versionWarning: %s\n", s.VersionWarning)
		}
	case verb == "stop" || verb == "wake":
		if resp.Text == "" {
			resp.Text = "ok"
		}
		fmt.Fprintln(stdout, resp.Text)
	case req.Action == "get":
		fmt.Fprint(stdout, resp.Text)
		if !strings.HasSuffix(resp.Text, "\n") {
			fmt.Fprintln(stdout)
		}
	case req.Action == "list":
		for _, h := range resp.Notes {
			fmt.Fprintf(stdout, "%s\t%s\n", h.ID, h.First)
		}
	}
	return 0
}

// runConnector is the run verb in the foreground (design doc, "## Startup" steps 1-2, then the
// loop).
func runConnector(path string, o connect.Overrides, verbose bool, stderr io.Writer, getenv func(string) string) int {
	fail := func(err error) int {
		fmt.Fprintln(stderr, "aif-connect:", err)
		return 1
	}
	cfg, err := connect.LoadConfig(path, o)
	if err != nil {
		return fail(err)
	}
	for _, k := range cfg.Ignored {
		fmt.Fprintf(stderr, "aif-connect: warning: config key %q is not a connector key; ignored\n", k)
	}
	drv, err := driver.New(cfg.Agent)
	if err != nil {
		return fail(err)
	}
	want := cfg.Bin
	if want == "" {
		want = cfg.Agent
	}
	bin, err := exec.LookPath(want)
	if err != nil {
		return fail(fmt.Errorf("harness binary: %w (set bin in the config or pass --bin)", err))
	}
	// The harness runs in cfg.Cwd, where a relative bin would resolve differently.
	if cfg.Bin, err = filepath.Abs(bin); err != nil {
		return fail(err)
	}
	if err := drv.Preflight(cfg.Bin); err != nil {
		return fail(err)
	}
	client, err := connect.NewClient(cfg.AifURL, cfg.AgentToken)
	if err != nil {
		return fail(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fail(err)
	}
	// The harness finds this very binary first, so its `aif-connect note` is the matching build.
	pathEnv := getenv("PATH")
	if exe, err := os.Executable(); err == nil {
		pathEnv = filepath.Dir(exe) + string(os.PathListSeparator) + pathEnv
	}
	logger := log.New(stderr, "", log.LstdFlags|log.Lmicroseconds) // write errors are ignored
	ctx, stop := signal.NotifyContext(context.Background(), stopSignals...)
	defer stop()
	return connect.Run(ctx, cfg, connect.Deps{
		Client:  client,
		Driver:  drv,
		Log:     func(s string) { logger.Print(s) },
		Home:    home,
		Version: version.Version,
		Env:     []string{"PATH=" + pathEnv},
		Verbose: verbose,
	})
}

// startDaemon is run --daemon: it re-executes this binary detached with args (the flags minus
// --daemon) and waits for the child's control socket to report phase running.
func startDaemon(args []string, path string, o connect.Overrides, stdout, stderr io.Writer) int {
	cfg, err := connect.LoadConfig(path, o) // the child reports ignored keys; errors show here once
	if err != nil {
		fmt.Fprintln(stderr, "aif-connect:", err)
		return 1
	}
	_, key, _ := connect.OriginKey(cfg.AifURL) // LoadConfig validated it
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(stderr, "aif-connect:", err)
		return 1
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(stderr, "aif-connect:", err)
		return 1
	}
	return daemonize(exe, args, connect.StateDir(home, cfg.AgentName, key), 5*time.Minute, stdout, stderr)
}

// daemonize starts exe args detached with stdin and stdout on the null device and stderr
// inherited, then polls dir's control socket every 200 ms until the child itself (the reply's pid
// is the child's) reports phase running: it prints the PID and returns 0. The child exiting first
// returns its exit code; nothing ready within limit returns 1.
func daemonize(exe string, args []string, dir string, limit time.Duration, stdout, stderr io.Writer) int {
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		fmt.Fprintln(stderr, "aif-connect:", err)
		return 1
	}
	defer null.Close()
	cmd := exec.Command(exe, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = null, null, stderr
	cmd.SysProcAttr = detached()
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(stderr, "aif-connect:", err)
		return 1
	}
	pid := cmd.Process.Pid
	exited := make(chan int, 1)
	go func() {
		cmd.Wait()
		exited <- cmd.ProcessState.ExitCode()
	}()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(limit)
	for {
		select {
		case code := <-exited:
			switch code {
			case 0:
				fmt.Fprintln(stderr, "aif-connect: connector exited: pending SHUTDOWN executed")
			case -1: // killed by a signal
				fmt.Fprintf(stderr, "aif-connect: connector %d was killed\n", pid)
				code = 1
			}
			return code
		case <-deadline:
			fmt.Fprintf(stderr, "aif-connect: daemon did not become ready (pid %d still running)\n", pid)
			return 1
		case <-tick.C:
			r, err := connect.Call(dir, connect.Request{Op: "status"})
			if err == nil && r.OK && r.Status != nil && r.Status.PID == pid && r.Status.Phase == "running" {
				fmt.Fprintln(stdout, pid)
				return 0
			}
		}
	}
}
