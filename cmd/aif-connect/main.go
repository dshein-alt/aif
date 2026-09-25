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
  aif-connect [--to=PID|NAME] note set ID TEXT | get ID | delete ID | list
  aif-connect [--to=PID|NAME] note set ID -    (TEXT from stdin: up to 16 KiB, one trailing newline dropped)
  aif-connect --version

A relative --bin resolves against the current directory; a relative bin in the config file, like
cwd and systemPromptFile, against the config file's directory. A bare name is looked up in PATH.
`

// runFlags are the flags of the run verb; control verbs take only --to.
var runFlags = map[string]bool{"config": true, "agent": true, "bin": true, "daemon": true, "verbose": true}

// usageError reports a command-line mistake followed by the usage text; it returns exit code 1.
func usageError(stderr io.Writer, format string, a ...any) int {
	fmt.Fprintf(stderr, "aif-connect: "+format+"\n\n%s", append(a, usage)...)
	return 1
}

// failure reports err; it returns exit code 1.
func failure(stderr io.Writer, err error) int {
	fmt.Fprintln(stderr, "aif-connect:", err)
	return 1
}

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
			return usageError(stderr, "--to needs a verb (status, stop, wake, note)")
		}
		if *config == "" {
			return usageError(stderr, "--config is required to run a connector")
		}
		o := connect.Overrides{Agent: *agent, Bin: *bin}
		if *daemon {
			abs, err := filepath.Abs(*config) // childArgs reads it back through the flag
			if err != nil {
				return failure(stderr, err)
			}
			*config = abs
			// The child inherits the real stderr: a pipe in its place would break once we exit.
			return startDaemon(childArgs(fs), *config, o, stdout, os.Stderr)
		}
		return runConnector(*config, o, *verbose, stderr, getenv)
	}

	verb := rest[0]
	for name := range set {
		if runFlags[name] {
			return usageError(stderr, "--%s applies to running a connector, not to %s", name, verb)
		}
	}
	return control(verb, rest[1:], *to, stdin, stdout, stderr, getenv)
}

// runConnector is the run verb in the foreground (design doc, "## Startup" steps 1-2, then the
// loop).
func runConnector(path string, o connect.Overrides, verbose bool, stderr io.Writer, getenv func(string) string) int {
	cfg, err := connect.LoadConfig(path, o)
	if err != nil {
		return failure(stderr, err)
	}
	for _, k := range cfg.Ignored {
		fmt.Fprintf(stderr, "aif-connect: warning: config key %q is not a connector key; ignored\n", k)
	}
	drv, err := driver.New(cfg.Agent)
	if err != nil {
		return failure(stderr, err)
	}
	want := cfg.Bin
	if want == "" {
		want = cfg.Agent
	}
	// A relative bin path from the config file resolves against the config's directory, as cwd and
	// systemPromptFile do; a relative --bin against the current directory (LookPath does that); a
	// bare name is looked up in PATH.
	if o.Bin == "" && strings.ContainsAny(want, `/`+string(os.PathSeparator)) && !filepath.IsAbs(want) {
		want = filepath.Join(filepath.Dir(path), want)
	}
	bin, err := exec.LookPath(want)
	if err != nil {
		return failure(stderr, fmt.Errorf("harness binary: %w (set bin in the config or pass --bin)", err))
	}
	// The harness runs in cfg.Cwd, where a relative bin would resolve differently.
	if cfg.Bin, err = filepath.Abs(bin); err != nil {
		return failure(stderr, err)
	}
	if err := drv.Preflight(cfg.Bin); err != nil {
		return failure(stderr, err)
	}
	client, err := connect.NewClient(cfg.AifURL, cfg.AgentToken)
	if err != nil {
		return failure(stderr, err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return failure(stderr, err)
	}
	// The harness finds this very binary first, so its `aif-connect note` is the matching build.
	pathEnv := getenv("PATH")
	if exe, err := os.Executable(); err == nil {
		pathEnv = prependPath(filepath.Dir(exe), pathEnv)
	}
	logger := log.New(stderr, "", log.LstdFlags|log.Lmicroseconds) // write errors are ignored
	ctx, stop := stopContext(func(s string) { logger.Print(s) })
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

// prependPath puts dir in front of the PATH list. An empty list gets no separator: a trailing
// empty entry would mean the current directory on Unix.
func prependPath(dir, list string) string {
	if list == "" {
		return dir
	}
	return dir + string(os.PathListSeparator) + list
}

// stopContext is canceled by the first stop signal, which also restores the default handling, so
// a second Ctrl-C ends the process at once.
func stopContext(logf func(string)) (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), stopSignals...)
	go func() {
		<-ctx.Done()
		stop()                                      // first: once the line shows, the next signal forces
		if context.Cause(ctx) != context.Canceled { // a signal (its cause Is Canceled too), not our stop
			logf("stopping; Ctrl-C again to force")
		}
	}()
	return ctx, stop
}
