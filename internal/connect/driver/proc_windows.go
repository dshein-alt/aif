package driver

import (
	"os"
	"os/exec"
)

// ponytail: on Windows only the harness itself is killed, never the tools it started; a Job
// object (kill-on-job-close) would be the equivalent of the Unix process group.
func ownGroup(*exec.Cmd)      {}
func terminate(p *os.Process) { _ = p.Kill() }
func kill(p *os.Process)      { _ = p.Kill() }
func reapGroup(int)           {}
