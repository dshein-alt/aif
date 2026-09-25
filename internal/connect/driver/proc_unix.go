//go:build !windows

package driver

import (
	"os"
	"os/exec"
	"syscall"
)

// ownGroup makes the harness the leader of a new process group, so the group can be signalled as
// one and the terminal's Ctrl-C (sent to the foreground group) does not reach it.
func ownGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func terminate(p *os.Process) { _ = syscall.Kill(-p.Pid, syscall.SIGTERM) }
func kill(p *os.Process)      { _ = syscall.Kill(-p.Pid, syscall.SIGKILL) }

// reapGroup kills what the harness left in its group once it has exited, e.g. a tool still
// running when the harness died on its own. ESRCH (nothing left) is the usual outcome.
func reapGroup(pid int) { _ = syscall.Kill(-pid, syscall.SIGKILL) }
