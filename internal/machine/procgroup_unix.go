//go:build unix

package machine

import (
	"os/exec"
	"syscall"
	"time"
)

// killGrace is how long a killed process group has to flush output before the
// wait is abandoned.
const killGrace = 5 * time.Second

// setProcessGroup puts the child in its own process group so that cancelling
// it also reaps whatever it spawned: an agent lane routinely forks compilers,
// test runners and package managers, and killing only the shell would leave
// those holding the repo.
func setProcessGroup(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killGroup(c *exec.Cmd) error {
	if c.Process == nil {
		return nil
	}
	if err := syscall.Kill(-c.Process.Pid, syscall.SIGKILL); err != nil {
		// The group may already be gone; fall back to the process itself.
		return c.Process.Kill()
	}
	return nil
}
