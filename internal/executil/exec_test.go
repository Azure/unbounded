// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package executil

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// commandAt returns a factory for the command runners, which take one rather
// than a path so they can rebuild the command on every attempt.
func commandAt(path string) func(context.Context) *exec.Cmd {
	return func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, path)
	}
}

// busyScript writes an executable and returns its path along with an open
// write handle to it.
//
// While that handle is open the kernel refuses to execute the file: execve
// returns ETXTBSY. That makes the race this package guards against reproducible
// on demand, rather than something a test has to wait for.
func busyScript(t *testing.T, body string) (string, *os.File) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "probe")

	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("write probe: %v", err)
	}

	held, err := os.OpenFile(path, os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatalf("hold probe open for writing: %v", err)
	}

	t.Cleanup(func() { _ = held.Close() })

	return path, held
}

// TestOutputCmdRetriesWhileTextFileBusy is the regression test for the failure
// this package exists to absorb.
//
// A binary the process has just written can transiently refuse to execute:
// fork() duplicates the writing descriptor into the child, and O_CLOEXEC only
// closes it at execve, so for the lifetime of somebody else's fork the inode is
// open for writing and Linux returns ETXTBSY. The agent installs kubelet,
// containerd, runc, the CNI plugins and CoreDNS and immediately runs each to
// read its version, and those installers run concurrently under
// phases.Parallel, so the window is reached in practice.
func TestOutputCmdRetriesWhileTextFileBusy(t *testing.T) {
	path, held := busyScript(t, "echo ready")

	// Release the file shortly after the first attempt, so the command can only
	// succeed if the retry happens.
	go func() {
		time.Sleep(50 * time.Millisecond)

		_ = held.Close()
	}()

	output, err := OutputCmd(t.Context(), discardLogger(), path)
	if err != nil {
		t.Fatalf("OutputCmd: %v", err)
	}

	if output != "ready" {
		t.Fatalf("output = %q, want %q", output, "ready")
	}
}

// TestRunCmdRetriesWhileTextFileBusy covers the other entry point, which takes
// a command factory rather than a path.
func TestRunCmdRetriesWhileTextFileBusy(t *testing.T) {
	path, held := busyScript(t, "exit 0")

	go func() {
		time.Sleep(50 * time.Millisecond)

		_ = held.Close()
	}()

	if err := RunCmd(t.Context(), discardLogger(), commandAt(path)); err != nil {
		t.Fatalf("RunCmd: %v", err)
	}
}

// TestRunCmdDoesNotDuplicateArgsAcrossRetries pins the reason the command is
// rebuilt on every attempt rather than reused.
//
// An exec.Cmd cannot be started twice, and RunCmdAt appends the caller's
// arguments to the factory's command, so retrying a reused command would append
// them again and run something different from what the caller asked for.
func TestRunCmdDoesNotDuplicateArgsAcrossRetries(t *testing.T) {
	// The script fails unless it receives exactly one argument.
	path, held := busyScript(t, `[ "$#" -eq 1 ] || exit 3`)

	go func() {
		time.Sleep(50 * time.Millisecond)

		_ = held.Close()
	}()

	if err := RunCmd(t.Context(), discardLogger(), commandAt(path), "only-one"); err != nil {
		t.Fatalf("RunCmd: %v; arguments were probably appended more than once", err)
	}
}

// TestRetryWhileTextFileBusyReturnsOtherErrorsImmediately confirms the retry is
// confined to ETXTBSY. Retrying anything else would re-run a command that has
// already taken effect.
func TestRetryWhileTextFileBusyReturnsOtherErrorsImmediately(t *testing.T) {
	wanted := errors.New("the command genuinely failed")

	attempts := 0

	start := time.Now()

	err := RetryWhileTextFileBusy(t.Context(), discardLogger(), func() error {
		attempts++

		return wanted
	})

	if !errors.Is(err, wanted) {
		t.Fatalf("err = %v, want the original error", err)
	}

	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1: only ETXTBSY may be retried", attempts)
	}

	if elapsed := time.Since(start); elapsed > defaultTextBusyPolicy.budget {
		t.Fatalf("took %s, want an immediate return", elapsed)
	}
}

// TestRetryWhileTextFileBusyGivesUp bounds the retry, so a file something else
// holds open forever cannot hang a pass indefinitely.
func TestRetryWhileTextFileBusyGivesUp(t *testing.T) {
	path, _ := busyScript(t, "echo never reached")

	start := time.Now()

	_, err := OutputCmd(t.Context(), discardLogger(), path)
	if err == nil {
		t.Fatal("a file held open for writing must eventually fail rather than retry forever")
	}

	if !IsTextFileBusy(err) {
		t.Fatalf("err = %v, want it to carry the underlying ETXTBSY", err)
	}

	// Sleeps are clamped to what is left of the budget, so none can run past
	// the deadline and buy a further attempt on the far side of it. The slack
	// covers the attempts themselves, which are fast because execve rejects
	// the file immediately, and a loaded machine.
	const slack = 500 * time.Millisecond

	if elapsed := time.Since(start); elapsed > defaultTextBusyPolicy.budget+slack {
		t.Fatalf("gave up after %s, want no more than %s", elapsed, defaultTextBusyPolicy.budget+slack)
	}

	// The binary is still named, even though the helper no longer takes a path.
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("err = %v, want it to name %s", err, path)
	}
}

// TestRunCmdBuildsOneCommandPerAttempt is a regression test.
//
// The command factory is caller-supplied and may be stateful: it can allocate,
// open files or hand back a different command each time. An earlier revision
// called it an extra time purely to read the path for an error message, which
// consumed whatever that factory does and could report a path belonging to a
// command that never ran. Nothing outside the attempt builds a command now.
func TestRunCmdBuildsOneCommandPerAttempt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write probe: %v", err)
	}

	built := 0

	factory := func(ctx context.Context) *exec.Cmd {
		built++

		return exec.CommandContext(ctx, path)
	}

	if err := RunCmd(t.Context(), discardLogger(), factory); err != nil {
		t.Fatalf("RunCmd: %v", err)
	}

	if built != 1 {
		t.Fatalf("factory was called %d times for a command that succeeded first time, want 1", built)
	}
}

// runOutputCmd runs OutputCmd and fails the test if it does not return within
// the given bound, so a blocked pipe fails the test instead of hanging it.
func runOutputCmd(t *testing.T, within time.Duration, name string, args ...string) (string, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), within)
	defer cancel()

	type result struct {
		output string
		err    error
	}

	done := make(chan result, 1)

	go func() {
		output, err := OutputCmd(ctx, discardLogger(), name, args...)

		done <- result{output: output, err: err}
	}()

	select {
	case res := <-done:
		return res.output, res.err
	case <-time.After(within):
		t.Fatalf("%s did not return within %s", name, within)
		return "", nil
	}
}

// TestOutputCmdHandlesLineLongerThanScannerDefault is the regression test for
// a single output line longer than bufio.Scanner's 64 KiB default token.
//
// The scanner used to stop at that line, leave the rest of the pipe unread and
// return without an error. The caller waits for the reading goroutine before
// calling cmd.Wait, so a child still writing a line into a full pipe never
// exited and the call hung; the lines after the long one were dropped even
// when it did not.
func TestOutputCmdHandlesLineLongerThanScannerDefault(t *testing.T) {
	const (
		lineLen = 96 * 1024
		after   = "after the long line"
	)

	output, err := runOutputCmd(t, 5*time.Second,
		"sh", "-c", fmt.Sprintf(`printf '%%*s\n%s\n' %d ''`, after, lineLen))
	if err != nil {
		t.Fatalf("OutputCmd: %v", err)
	}

	want := strings.Repeat(" ", lineLen) + "\n" + after
	if output != want {
		t.Fatalf("output = %d bytes ending %q, want %d bytes", len(output), tail(output), len(want))
	}
}

// TestOutputCmdReportsOverlongLine covers the other half of the fix: a line
// past the raised buffer bound is reported rather than dropped. The child has
// to be able to finish writing for the call to return at all.
func TestOutputCmdReportsOverlongLine(t *testing.T) {
	output, err := runOutputCmd(t, 5*time.Second,
		"sh", "-c", fmt.Sprintf(`printf '%%*s\n%s\n' %d ''`, "unreachable", maxLogLine+1024))
	if err == nil {
		t.Fatal("a line longer than maxLogLine must be reported rather than silently truncated")
	}

	if !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("err = %v, want it to carry bufio.ErrTooLong", err)
	}

	if output != "" {
		t.Fatalf("output = %d bytes, want nothing alongside a read failure", len(output))
	}
}

// tail returns at most the last few bytes of s, so a failure message about a
// long line stays readable.
func tail(s string) string {
	const keep = 32

	if len(s) <= keep {
		return s
	}

	return s[len(s)-keep:]
}

// TestRetryWhileTextFileBusyHonorsContext confirms a canceled pass stops
// retrying rather than running out the budget.
func TestRetryWhileTextFileBusyHonorsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())

	attempts := 0

	err := RetryWhileTextFileBusy(ctx, discardLogger(), func() error {
		attempts++

		cancel()

		return syscall.ETXTBSY
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to report the cancellation", err)
	}

	if !IsTextFileBusy(err) {
		t.Fatalf("err = %v, want it to also carry the reason it was retrying", err)
	}

	if attempts != 1 {
		t.Fatalf("attempts = %d, want the loop to stop once the context is done", attempts)
	}
}

// TestRetryClampsSleepToRemainingBudget is the regression test for the budget
// being a bound rather than a suggestion.
//
// The deadline is checked before sleeping, so without clamping a sleep started
// just inside the budget runs its full length past it and buys a further
// attempt on the far side. With the production values that overshoot is at most
// one maxDelay, too small to separate from scheduling noise, so this uses a
// policy whose delay dwarfs its budget: unclamped it would sleep 500ms against
// a 50ms budget.
func TestRetryClampsSleepToRemainingBudget(t *testing.T) {
	policy := retryPolicy{
		budget:       50 * time.Millisecond,
		initialDelay: 500 * time.Millisecond,
		maxDelay:     time.Second,
	}

	attempts := 0

	start := time.Now()

	err := retryWhileTextFileBusy(t.Context(), discardLogger(), policy, func() error {
		attempts++

		return syscall.ETXTBSY
	})
	if err == nil {
		t.Fatal("a permanently busy file must eventually give up")
	}

	elapsed := time.Since(start)

	// Clamped, the single sleep is cut to the 50ms that remain. Unclamped it
	// would be the full 500ms.
	if elapsed > 250*time.Millisecond {
		t.Fatalf("gave up after %s, want the sleep clamped to the %s budget", elapsed, policy.budget)
	}

	// One sleep, so two attempts: the one that failed and the one after it.
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}
