// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodestart

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

// silentLogger returns a logger that drops all output. Tests use it so the
// recovery path can log freely without polluting test output.
func silentLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// fakeRunner is a scriptable machinectlRunner for exercising the
// startWithRecovery state machine without touching real binaries.
type fakeRunner struct {
	mu sync.Mutex

	// startResults are returned by successive calls to Start.
	// If the slice is exhausted, the test fails.
	startResults []error

	// existsAfterStart is the value Exists returns immediately after a
	// failed Start, simulating a stale machinectl registration.
	existsAfterStart bool

	// terminateErr is returned by Terminate.
	terminateErr error

	// resetFailedErr is returned by ResetFailed.
	resetFailedErr error

	// terminateClears, when true, makes Exists return false on calls
	// that occur after Terminate has been invoked. This lets us model
	// "terminate succeeded" vs "terminate did not actually clear".
	terminateClears bool

	enableCalls       int
	startCalls        int
	terminateCalls    int
	existsCalls       int
	resetFailedCalls  int
	terminatedAlready bool
}

func (f *fakeRunner) Enable(_ context.Context, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.enableCalls++

	return nil
}

func (f *fakeRunner) Start(_ context.Context, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.startCalls >= len(f.startResults) {
		panic("fakeRunner: Start called more times than scripted")
	}

	err := f.startResults[f.startCalls]
	f.startCalls++

	return err
}

func (f *fakeRunner) Terminate(_ context.Context, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.terminateCalls++
	f.terminatedAlready = true

	return f.terminateErr
}

func (f *fakeRunner) Exists(_ context.Context, _ string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.existsCalls++

	if f.terminatedAlready && f.terminateClears {
		return false
	}

	return f.existsAfterStart
}

func (f *fakeRunner) ResetFailed(_ context.Context, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.resetFailedCalls++

	return f.resetFailedErr
}

// runStart constructs a startNSpawnMachine wired to the fake runner and
// invokes startWithRecovery. We avoid Do() so the test does not need to
// fake the systemd-run wait loop.
func runStart(t *testing.T, runner *fakeRunner) error {
	t.Helper()

	s := &startNSpawnMachine{
		log:       silentLogger(),
		goalState: &goalstates.NodeStart{MachineName: "kube1"},
		runner:    runner,
	}

	return s.startWithRecovery(context.Background(), "kube1")
}

// TestStartWithRecovery_HappyPath: clean start, no recovery needed.
func TestStartWithRecovery_HappyPath(t *testing.T) {
	t.Parallel()

	r := &fakeRunner{startResults: []error{nil}}

	require.NoError(t, runStart(t, r))
	require.Equal(t, 1, r.startCalls)
	require.Equal(t, 0, r.terminateCalls)
	require.Equal(t, 0, r.resetFailedCalls)
}

// TestStartWithRecovery_StaleRegistration_ErrorMessage: first start fails
// with a message matching EEXIST; we should reset-failed, terminate, and
// retry start successfully.
func TestStartWithRecovery_StaleRegistration_ErrorMessage(t *testing.T) {
	t.Parallel()

	r := &fakeRunner{
		startResults:    []error{errors.New(`Failed to register machine: Machine "kube1" already exists`), nil},
		terminateClears: true,
	}

	require.NoError(t, runStart(t, r))
	require.Equal(t, 2, r.startCalls)
	require.Equal(t, 1, r.terminateCalls)
	require.Equal(t, 1, r.resetFailedCalls)
}

// TestStartWithRecovery_StaleRegistration_ExistsCheck: first start fails
// with a generic error, but `machinectl show` reports the machine still
// exists. Treat as recoverable.
func TestStartWithRecovery_StaleRegistration_ExistsCheck(t *testing.T) {
	t.Parallel()

	r := &fakeRunner{
		startResults:     []error{errors.New("some unrelated text"), nil},
		existsAfterStart: true,
		terminateClears:  true,
	}

	require.NoError(t, runStart(t, r))
	require.Equal(t, 2, r.startCalls)
	require.Equal(t, 1, r.terminateCalls)
}

// TestStartWithRecovery_UnrelatedError_NoRecovery: start fails with a
// non-EEXIST error and the machine is not registered. Bubble up without
// terminate.
func TestStartWithRecovery_UnrelatedError_NoRecovery(t *testing.T) {
	t.Parallel()

	r := &fakeRunner{
		startResults:     []error{errors.New("permission denied")},
		existsAfterStart: false,
	}

	err := runStart(t, r)
	require.Error(t, err)
	require.Contains(t, err.Error(), "permission denied")
	require.Equal(t, 1, r.startCalls)
	require.Equal(t, 0, r.terminateCalls)
}

// TestStartWithRecovery_TerminateDoesNotClear: start fails with EEXIST,
// terminate is attempted, but the registration never disappears. We must
// surface the failure rather than retry start blindly.
func TestStartWithRecovery_TerminateDoesNotClear(t *testing.T) {
	t.Parallel()

	r := &fakeRunner{
		startResults:     []error{errors.New("File exists")},
		existsAfterStart: true,
		terminateClears:  false,
		// Even if Terminate returns nil, Exists keeps returning true.
	}

	err := runStart(t, r)
	require.Error(t, err)
	require.Contains(t, err.Error(), "stale registration did not clear")
	require.Equal(t, 1, r.startCalls, "must not retry start when the machine is still registered")
	require.GreaterOrEqual(t, r.terminateCalls, 1)
}

// TestStartWithRecovery_RetryStartFails: recovery proceeded (terminate
// cleared the registration) but the second start still fails. Surface
// the second error.
func TestStartWithRecovery_RetryStartFails(t *testing.T) {
	t.Parallel()

	r := &fakeRunner{
		startResults: []error{
			errors.New("already exists"),
			errors.New("kernel module missing"),
		},
		terminateClears: true,
	}

	err := runStart(t, r)
	require.Error(t, err)
	require.Contains(t, err.Error(), "after recovery")
	require.Contains(t, err.Error(), "kernel module missing")
}

func TestIsAlreadyExistsErr(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"already exists", errors.New(`Machine "kube1" already exists`), true},
		{"File exists", errors.New("Failed: File exists"), true},
		{"case insensitive", errors.New("ALREADY EXISTS"), true},
		{"unrelated", errors.New("permission denied"), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, isAlreadyExistsErr(tc.err))
		})
	}
}
