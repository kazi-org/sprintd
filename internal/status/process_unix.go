//go:build unix

package status

import (
	"errors"
	"syscall"
)

// processAlive reports whether a process with this id currently exists.
//
// Signal 0 performs the permission and existence checks without delivering
// anything. EPERM means the process exists but belongs to another user, which
// still answers the question being asked.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}
