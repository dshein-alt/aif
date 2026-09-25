package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/dshein-alt/aif/internal/connect"
)

// childArgs are the daemon child's arguments: every flag given on the command line but --daemon,
// each as one --name=value argument.
func childArgs(fs *flag.FlagSet) []string {
	var args []string
	fs.Visit(func(f *flag.Flag) {
		if f.Name != "daemon" {
			args = append(args, "--"+f.Name+"="+f.Value.String())
		}
	})
	return args
}

// startDaemon is run --daemon: it re-executes this binary detached with args (the flags minus
// --daemon) and waits for the child's control socket to report phase running.
func startDaemon(args []string, path string, o connect.Overrides, stdout io.Writer, stderr *os.File) int {
	cfg, err := connect.LoadConfig(path, o) // the child reports ignored keys; errors show here once
	if err != nil {
		return failure(stderr, err)
	}
	_, key, _ := connect.OriginKey(cfg.AifURL) // LoadConfig validated it
	home, err := os.UserHomeDir()
	if err != nil {
		return failure(stderr, err)
	}
	exe, err := os.Executable()
	if err != nil {
		return failure(stderr, err)
	}
	return daemonize(exe, args, connect.StateDir(home, cfg.AgentName, key), 5*time.Minute, stdout, stderr)
}

// daemonize starts exe args detached with stdin and stdout on the null device and stderr
// inherited (a file, not a pipe: the child outlives us), then probes dir's control socket every
// 200 ms until the child itself (the reply's pid is the child's) reports phase running: it prints
// the PID and returns 0. The child exiting first returns its exit code. Nothing ready within limit,
// or a stop signal meanwhile, terminates the child (it takes its harness down with it), waits for
// it and returns 1.
func daemonize(exe string, args []string, dir string, limit time.Duration, stdout io.Writer, stderr *os.File) int {
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return failure(stderr, err)
	}
	defer null.Close()
	cmd := exec.Command(exe, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = null, null, stderr
	cmd.SysProcAttr = detached()
	if err := cmd.Start(); err != nil {
		return failure(stderr, err)
	}
	pid := cmd.Process.Pid
	exited := make(chan int, 1)
	go func() {
		cmd.Wait()
		exited <- cmd.ProcessState.ExitCode()
	}()
	ctx, stop := stopContext(func(s string) { fmt.Fprintln(stderr, "aif-connect:", s) })
	defer stop()
	// ponytail: the wait after terminate is unbounded; the connector's own shutdown is bounded and
	// a second Ctrl-C ends this process, so add a kill deadline only if a wedged child shows up.
	abandon := func(why string) int {
		stop() // a second Ctrl-C while the child shuts down ends this process at once
		terminate(cmd.Process)
		<-exited
		fmt.Fprintf(stderr, "aif-connect: connector %d %s; stopped it; see its stderr above\n", pid, why)
		return 1
	}
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(limit)
	for {
		select {
		case code := <-exited:
			switch code {
			case 0:
				fmt.Fprintln(stderr, "aif-connect: connector exited 0 before it was ready (stopped, or a pending SHUTDOWN was executed)")
			case -1: // killed by a signal
				fmt.Fprintf(stderr, "aif-connect: connector %d was killed\n", pid)
				code = 1
			}
			return code
		case <-deadline:
			d := limit.String()
			if strings.HasSuffix(d, "m0s") {
				d = strings.TrimSuffix(d, "0s") // 5m0s reads 5m
			}
			return abandon("did not become ready within " + d)
		case <-ctx.Done():
			return abandon("was interrupted before it was ready")
		case <-tick.C:
			if s, err := connect.Probe(dir); err == nil && s.PID == pid && s.Phase == "running" {
				fmt.Fprintln(stdout, pid)
				return 0
			}
		}
	}
}
