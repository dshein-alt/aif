//go:build !windows

package main

import (
	"os"
	"syscall"
)

// stopSignals end a foreground connector: Ctrl-C and the service manager's TERM.
var stopSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

// detached starts the daemon child in a session of its own, away from the terminal.
func detached() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

// terminate asks the daemon child to shut down, as a service manager would.
func terminate(p *os.Process) error { return p.Signal(syscall.SIGTERM) }
