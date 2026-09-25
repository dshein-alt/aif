//go:build !windows

package driver

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A tool the harness started dies with it: on Stop, and when the harness exits on its own.
func TestProcKillsGroup(t *testing.T) {
	for name, transcript := range map[string]string{
		"stop": "spawn: sleep 60\n",
		"exit": "spawn: sleep 60\nexit: 0\n",
	} {
		t.Run(name, func(t *testing.T) {
			p, _ := startFake(t, transcript, Launch{})
			pid, err := strconv.Atoi(<-p.Lines())
			if err != nil {
				t.Fatal(err)
			}
			if name == "stop" {
				_ = p.Stop()
			} else {
				collect(t, p)
			}
			// Container PID 1 may never reap an orphan. A zombie is dead even though
			// signal 0 still finds its PID; require termination, not init reaping it.
			for deadline := time.Now().Add(5 * time.Second); processRunning(t, pid); time.Sleep(20 * time.Millisecond) {
				if time.Now().After(deadline) {
					_ = syscall.Kill(pid, syscall.SIGKILL)
					t.Fatalf("spawned pid %d outlived the harness", pid)
				}
			}
		})
	}
}

// processRunning distinguishes a live process from a killed, unreaped Linux child.
// Other Unix hosts use the usual init reaping check.
func processRunning(t *testing.T, pid int) bool {
	t.Helper()
	if err := syscall.Kill(pid, 0); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return false
		}
		t.Fatalf("probe pid %d: %v", pid, err)
	}
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if errors.Is(err, os.ErrNotExist) {
			return false
		}
		if err != nil {
			t.Fatalf("read pid %d state: %v", pid, err)
		}
		// comm is parenthesized and can itself contain spaces and parentheses.
		end := strings.LastIndexByte(string(data), ')')
		if end < 0 {
			t.Fatalf("invalid stat for pid %d: %q", pid, data)
		}
		fields := strings.Fields(string(data[end+1:]))
		if len(fields) == 0 {
			t.Fatalf("missing state for pid %d: %q", pid, data)
		}
		return fields[0] != "Z" && fields[0] != "X"
	}
	return true
}

func TestProcessRunning(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux zombie state check")
	}
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	pid := cmd.Process.Pid
	if !processRunning(t, pid) {
		t.Fatal("live process reported dead")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	// Deliberately do not Wait yet: the child stays a zombie owned by this test,
	// reproducing a CI container whose PID 1 does not reap adopted children.
	for deadline := time.Now().Add(5 * time.Second); processRunning(t, pid); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("killed child still reported running")
		}
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("expected unreaped child: %v", err)
	}
	_ = cmd.Wait()
	if processRunning(t, pid) {
		t.Fatal("reaped process reported running")
	}
}
