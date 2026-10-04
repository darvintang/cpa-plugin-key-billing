// Created by Darvin.
package plugin

import (
	"os"
	"syscall"
)

// A zero-timeout wait observes termination without blocking a host call.
func workerExited(process *os.Process) bool {
	handle, err := syscall.OpenProcess(syscall.SYNCHRONIZE, false, uint32(process.Pid))
	if err != nil {
		// Win32 ERROR_INVALID_PARAMETER means the process no longer exists.
		return err == syscall.Errno(87)
	}
	defer syscall.CloseHandle(handle)
	status, err := syscall.WaitForSingleObject(handle, 0)
	return err == nil && status == syscall.WAIT_OBJECT_0
}
