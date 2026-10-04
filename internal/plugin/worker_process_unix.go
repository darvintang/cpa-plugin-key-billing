//go:build !windows

// Created by Darvin.
package plugin

import (
	"os"
	"syscall"
)

// Nonblocking wait also detects zombies without adding a goroutine to the embedded runtime.
func workerExited(process *os.Process) bool {
	var status syscall.WaitStatus
	pid, err := syscall.Wait4(process.Pid, &status, syscall.WNOHANG, nil)
	return pid == process.Pid || err == syscall.ECHILD
}
