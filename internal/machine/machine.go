// Package machine runs shell commands on the local host or over ssh.
//
// Deadlines are enforced in-process rather than by wrapping commands in the
// timeout(1) utility: timeout is absent from a stock macOS, and one of
// sprintd's three target machines is a Mac. Cancelling the context kills the
// child's whole process group, so an agent that has spawned builds or test
// runners does not survive its lane.
package machine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// LocalHost is the Host value meaning "run here, without ssh".
const LocalHost = "local"

// Command is one shell command to run on one machine.
type Command struct {
	// Host is LocalHost or an ssh destination such as user@10.0.0.2.
	Host string
	// Dir is the working directory the script runs in.
	Dir string
	// Env holds variables exported for the script, on top of the ambient
	// environment.
	Env map[string]string
	// Script is a shell command line, executed by bash -lc. A login shell is
	// used deliberately: claude and node are commonly installed by a version
	// manager that only a login shell puts on PATH, and a non-interactive ssh
	// session otherwise fails with "command not found".
	Script string
}

// Result reports how a command finished.
type Result struct {
	// ExitCode is the child's exit status, or -1 if it never produced one.
	ExitCode int
	// Killed is true when the command was terminated because its context was
	// cancelled, rather than exiting on its own.
	Killed bool
}

// OK reports whether the command exited cleanly.
func (r Result) OK() bool { return !r.Killed && r.ExitCode == 0 }

// Executor runs commands through os/exec.
type Executor struct {
	// SSHOpts are passed to ssh before the destination. BatchMode keeps a
	// missing key from hanging the run on a password prompt.
	SSHOpts []string
}

// NewExecutor returns an Executor with non-interactive ssh defaults.
func NewExecutor() *Executor {
	return &Executor{SSHOpts: []string{
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ConnectTimeout=10",
	}}
}

// Run executes cmd, streaming its combined stdout and stderr to out.
//
// A non-zero exit status is reported in Result, not as an error: the caller
// decides what a failing command means. An error is returned only when the
// command could not be run at all.
func (e *Executor) Run(ctx context.Context, cmd Command, out io.Writer) (Result, error) {
	if strings.TrimSpace(cmd.Script) == "" {
		return Result{ExitCode: -1}, errors.New("running command: empty script")
	}
	c, err := e.build(ctx, cmd)
	if err != nil {
		return Result{ExitCode: -1}, err
	}
	c.Stdout = out
	c.Stderr = out
	setProcessGroup(c)
	c.Cancel = func() error { return killGroup(c) }
	c.WaitDelay = killGrace

	if err := c.Start(); err != nil {
		return Result{ExitCode: -1}, fmt.Errorf("starting command on %s: %w", cmd.Host, err)
	}
	waitErr := c.Wait()
	res := Result{ExitCode: c.ProcessState.ExitCode(), Killed: ctx.Err() != nil}
	if waitErr == nil {
		return res, nil
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) || res.Killed {
		return res, nil
	}
	return res, fmt.Errorf("running command on %s: %w", cmd.Host, waitErr)
}

func (e *Executor) build(ctx context.Context, cmd Command) (*exec.Cmd, error) {
	if cmd.Host == LocalHost || cmd.Host == "" {
		c := exec.CommandContext(ctx, "bash", "-lc", cmd.Script)
		c.Dir = cmd.Dir
		c.Env = append(os.Environ(), envPairs(cmd.Env)...)
		return c, nil
	}
	args := append(append([]string{}, e.SSHOpts...), cmd.Host, "bash", "-lc", Quote(RemoteScript(cmd)))
	return exec.CommandContext(ctx, "ssh", args...), nil
}

// RemoteScript renders a Command as a single self-contained shell script, with
// the working directory and environment applied inside the remote shell.
func RemoteScript(cmd Command) string {
	var b strings.Builder
	if cmd.Dir != "" {
		fmt.Fprintf(&b, "cd %s && ", Quote(cmd.Dir))
	}
	for _, pair := range envPairs(cmd.Env) {
		name, value, _ := strings.Cut(pair, "=")
		fmt.Fprintf(&b, "export %s=%s && ", name, Quote(value))
	}
	b.WriteString(cmd.Script)
	return b.String()
}

func envPairs(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// Quote renders s as a single POSIX shell word.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
