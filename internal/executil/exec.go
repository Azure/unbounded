// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package executil

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// A binary this process has just written can be transiently unexecutable.
//
// fork() duplicates the writing file descriptor into the child, and O_CLOEXEC
// only closes it at execve, so between the fork and the exec the child holds
// the inode open for writing. Linux then refuses to execute it: execve returns
// ETXTBSY, "text file busy". Any concurrent fork anywhere in the process is
// enough, so installing a binary and running it is unsafe whenever anything
// else in the process might be starting a command. See golang.org/issue/22315.
//
// The agent does exactly this in several places: it downloads kubelet,
// containerd, runc, the CNI plugins and CoreDNS and immediately runs each one
// to check its version, and those installers run concurrently with each other
// under phases.Parallel.
//
// Retrying is safe because the error comes from execve, before the process
// exists: nothing has run and nothing has taken effect. The window is the
// lifetime of somebody else's fork, so it closes in well under a millisecond;
// the budget below is generous against a loaded machine rather than sized to
// any real expectation.
//
// The budget bounds how long this package keeps retrying, not the total wall
// time of the call. Sleeps are clamped so none runs past the deadline, but the
// attempt that follows the last sleep is a command execution and is not
// bounded here; a caller that needs a hard ceiling should impose it on ctx.
var defaultTextBusyPolicy = retryPolicy{
	budget:       time.Second,
	initialDelay: 10 * time.Millisecond,
	maxDelay:     100 * time.Millisecond,
}

// retryPolicy is a parameter so tests can pin the clamping behavior. With the
// production values the difference clamping makes is at most one maxDelay,
// which is too small to distinguish from scheduling noise on a loaded machine;
// a test policy whose delay dwarfs its budget makes it unmistakable.
type retryPolicy struct {
	budget       time.Duration
	initialDelay time.Duration
	maxDelay     time.Duration
}

// IsTextFileBusy reports whether an error is the transient ETXTBSY described
// above, so callers doing their own exec can make the same allowance.
func IsTextFileBusy(err error) bool {
	return errors.Is(err, syscall.ETXTBSY)
}

// RetryWhileTextFileBusy runs attempt until it does not fail with ETXTBSY.
//
// attempt must build everything it needs each time it is called, including the
// exec.Cmd: an exec.Cmd cannot be reused once started, and any pipes it created
// must be released before the next attempt. Nothing outside attempt constructs
// a command, so a caller-supplied command factory is invoked exactly once per
// attempt and never merely to inspect it.
//
// The path of the binary is deliberately not a parameter. Every error this can
// return already names it, because a failed start is wrapped with the command
// path and the underlying os.PathError carries it as well.
func RetryWhileTextFileBusy(ctx context.Context, logger *slog.Logger, attempt func() error) error {
	return retryWhileTextFileBusy(ctx, logger, defaultTextBusyPolicy, attempt)
}

func retryWhileTextFileBusy(ctx context.Context, logger *slog.Logger, policy retryPolicy, attempt func() error) error {
	deadline := time.Now().Add(policy.budget)
	delay := policy.initialDelay

	for {
		err := attempt()
		if !IsTextFileBusy(err) {
			return err
		}

		// Clamped to what is left, so a sleep cannot run past the deadline and
		// buy an extra attempt on the far side of it.
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("still busy after %s: %w", policy.budget, err)
		}

		if delay > remaining {
			delay = remaining
		}

		if logger != nil {
			logger.Debug("executable is still open for writing; retrying",
				"delay", delay, "error", err)
		}

		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), err)
		case <-time.After(delay):
		}

		if delay *= 2; delay > policy.maxDelay {
			delay = policy.maxDelay
		}
	}
}

// RunCmd creates a command from the given factory, appends args, streams stdout
// at Debug and stderr at Info, and waits for it to finish.
func RunCmd(ctx context.Context, logger *slog.Logger, newCmd func(context.Context) *exec.Cmd, args ...string) error {
	return RunCmdAt(ctx, logger, slog.LevelInfo, newCmd, args...)
}

// RunCmdAt is like RunCmd but streams stderr at stderrLevel instead of Info.
// Use a lower level (e.g. Debug) when stderr output is known to be benign or
// when a failure is expected and already handled by the caller.
func RunCmdAt(ctx context.Context, logger *slog.Logger, stderrLevel slog.Level, newCmd func(context.Context) *exec.Cmd, args ...string) error {
	// newCmd is called inside the attempt and nowhere else, so it runs exactly
	// once per attempt. An exec.Cmd is single use, and appending args to a
	// reused one would duplicate them.
	return RetryWhileTextFileBusy(ctx, logger, func() error {
		return runCmdOnce(ctx, logger, stderrLevel, newCmd, args...)
	})
}

func runCmdOnce(ctx context.Context, logger *slog.Logger, stderrLevel slog.Level, newCmd func(context.Context) *exec.Cmd, args ...string) error {
	cmd := newCmd(ctx)
	cmd.Args = append(cmd.Args, args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		// The pipes were created before the start failed, so they are closed
		// here rather than leaked once this attempt is retried.
		closeAll(stdout, stderr)

		return fmt.Errorf("failed to start %s: %w", cmd.Path, err)
	}

	var wg sync.WaitGroup

	wg.Add(2)

	go func() {
		defer wg.Done()

		streamLogs(ctx, logger, stdout, slog.LevelDebug)
	}()

	go func() {
		defer wg.Done()

		streamLogs(ctx, logger, stderr, stderrLevel)
	}()

	wg.Wait()

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("%s failed: %w", cmd.Path, err)
	}

	return nil
}

// OutputCmd runs the command specified by name and args, streams stdout at
// Debug and stderr at Info, and returns the captured stdout as a string.
// Unlike RunCmd it does not require a command factory - just a binary path.
func OutputCmd(ctx context.Context, logger *slog.Logger, name string, args ...string) (string, error) {
	return OutputCmdAt(ctx, logger, slog.LevelInfo, name, args...)
}

// OutputCmdAt is like OutputCmd but streams stderr at stderrLevel instead of
// Info. Use a lower level (e.g. Debug) when stderr output is known to be
// benign or an error exit is expected and already handled by the caller.
func OutputCmdAt(ctx context.Context, logger *slog.Logger, stderrLevel slog.Level, name string, args ...string) (string, error) {
	var output string

	err := RetryWhileTextFileBusy(ctx, logger, func() error {
		var attemptErr error

		output, attemptErr = outputCmdOnce(ctx, logger, stderrLevel, name, args...)

		return attemptErr
	})

	return output, err
}

func outputCmdOnce(ctx context.Context, logger *slog.Logger, stderrLevel slog.Level, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		// The pipes were created before the start failed, so they are closed
		// here rather than leaked once this attempt is retried.
		closeAll(stdout, stderr)

		return "", fmt.Errorf("failed to start %s: %w", cmd.Path, err)
	}

	var buf bytes.Buffer

	var wg sync.WaitGroup

	wg.Add(2)

	go func() {
		defer wg.Done()

		streamLogs(ctx, logger, io.TeeReader(stdout, &buf), slog.LevelDebug)
	}()

	go func() {
		defer wg.Done()

		streamLogs(ctx, logger, stderr, stderrLevel)
	}()

	wg.Wait()

	if err := cmd.Wait(); err != nil {
		return "", fmt.Errorf("%s failed: %w", cmd.Path, err)
	}

	return strings.TrimRight(buf.String(), "\n"), nil
}

// MachineRun executes a command inside the named nspawn machine using
// systemd-run --machine=<machine> --pipe --wait. It streams stdout at Debug
// and stderr at Error, and returns the captured stdout.
func MachineRun(ctx context.Context, log *slog.Logger, machine string, args ...string) (string, error) {
	runArgs := make([]string, 0, 3+len(args))
	runArgs = append(runArgs, "--machine="+machine, "--pipe", "--wait")
	runArgs = append(runArgs, args...)

	return OutputCmd(ctx, log, "systemd-run", runArgs...)
}

// streamLogs reads lines from reader and logs each one at the given level.
// closeAll releases pipes belonging to a command that never started.
func closeAll(closers ...io.Closer) {
	for _, closer := range closers {
		_ = closer.Close() //nolint:errcheck // nothing can be done about a pipe from a command that never ran
	}
}

func streamLogs(ctx context.Context, logger *slog.Logger, reader io.Reader, level slog.Level) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
			logger.Log(ctx, level, scanner.Text())
		}
	}
}

// ---------------------------------------------------------------------------
// Command factories
//
// Each factory returns a func(context.Context) *exec.Cmd that creates a fresh
// exec.Cmd for the named tool. Pass the factory to RunCmd with args, or call
// it directly for output-capture patterns (e.g. cmd.Output()).
// ---------------------------------------------------------------------------

// AptGet returns a command factory for apt-get with DEBIAN_FRONTEND=noninteractive.
func AptGet() func(context.Context) *exec.Cmd {
	return func(ctx context.Context) *exec.Cmd {
		cmd := exec.CommandContext(ctx, "apt-get")

		cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")

		return cmd
	}
}

// Tdnf returns a command factory for tdnf.
func Tdnf() func(context.Context) *exec.Cmd {
	return func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "tdnf")
	}
}

// Dnf returns a command factory for dnf.
func Dnf() func(context.Context) *exec.Cmd {
	return func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "dnf")
	}
}

// Ip returns a command factory for the ip networking utility.
func Ip() func(context.Context) *exec.Cmd {
	return func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "ip")
	}
}

// Machinectl returns a command factory for machinectl.
func Machinectl() func(context.Context) *exec.Cmd {
	return func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "machinectl")
	}
}

// Mountpoint returns a command factory for mountpoint.
func Mountpoint() func(context.Context) *exec.Cmd {
	return func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "mountpoint")
	}
}

// Sysctl returns a command factory for sysctl.
func Sysctl() func(context.Context) *exec.Cmd {
	return func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "sysctl")
	}
}

// Systemctl returns a command factory for systemctl.
func Systemctl() func(context.Context) *exec.Cmd {
	return func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "systemctl")
	}
}

// Umount returns a command factory for umount.
func Umount() func(context.Context) *exec.Cmd {
	return func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "umount")
	}
}
