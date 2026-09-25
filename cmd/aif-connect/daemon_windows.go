package main

import (
	"os"
	"syscall"
)

// stopSignals end a foreground connector: Windows delivers only Ctrl-C (os.Interrupt).
var stopSignals = []os.Signal{os.Interrupt}

// detached starts the daemon child without a console, in a process group of its own.
func detached() *syscall.SysProcAttr {
	const detachedProcess = 0x00000008 // DETACHED_PROCESS; package syscall does not name it
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | detachedProcess}
}
