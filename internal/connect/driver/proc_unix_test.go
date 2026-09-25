//go:build !windows

package driver

import (
	"strconv"
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
			// the orphan is reaped by init once killed; until then signal 0 still finds it
			for deadline := time.Now().Add(5 * time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(20 * time.Millisecond) {
				if time.Now().After(deadline) {
					_ = syscall.Kill(pid, syscall.SIGKILL)
					t.Fatalf("spawned pid %d outlived the harness", pid)
				}
			}
		})
	}
}
