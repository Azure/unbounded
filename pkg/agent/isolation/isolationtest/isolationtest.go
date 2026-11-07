// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package isolationtest is the conformance suite every isolation backend must
// pass. A backend package runs it from a test file:
//
//	func TestConformance(t *testing.T) {
//		isolationtest.Run(t, func(t *testing.T) (isolation.Backend, isolationtest.Options) {
//			// Build the backend over stubbed commands and return how to drive it.
//		})
//	}
//
// The suite calls newBackend once per subtest, so each case starts from a fresh
// backend and fresh host state. It does not run subtests in parallel, which lets
// runners use t.Setenv to stub commands on PATH.
//
// The suite covers prepare, start, stop, restart, exec, logs, destroy,
// idempotence of each, and recovery after an injected failure. It asserts only
// the contract in pkg/agent/isolation; backend specifics belong in the
// backend's own tests.
//
// A field of Options left at its zero value skips only the cases that need it.
package isolationtest

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// Op names a backend operation a runner can be asked to fail.
type Op string

// The operations that Options.Fail can target.
const (
	OpPrepare Op = "prepare"
	OpStart   Op = "start"
	OpStop    Op = "stop"
	OpRestart Op = "restart"
)

// ErrInjected is the error runners should return for an injected failure, so
// the suite can tell it from a real one.
var ErrInjected = errors.New("injected failure")

// Options tells the suite how to drive and observe the backend under test.
type Options struct {
	// Spec is passed to Prepare. Slot is required.
	Spec isolation.NodeSpec

	// ExecArgs is a command that succeeds in a running node stack, and ExecWant
	// is the stdout it produces (trailing newlines trimmed). Required for the
	// exec cases.
	ExecArgs []string
	ExecWant string

	// ExecFailArgs, when set, is a command that exits non-zero. Exec of it must
	// return an error.
	ExecFailArgs []string

	// Fail arranges for the next call of op to fail once with err and then
	// succeed again. When nil the failure recovery cases are skipped.
	Fail func(op Op, err error)

	// Grants, when set, are requests the backend must honor: Prepare with them
	// in Spec.Grants succeeds and the node starts, runs and stops. They must be
	// satisfiable on the runner's host, for example by creating the paths they
	// name in a temporary tree.
	Grants []isolation.Grant

	// RefusedGrants, when set, are requests the backend cannot honor. Prepare
	// with any one of them must return an error rather than ignore the grant,
	// and must leave the slot preparable. Backends that honor everything leave
	// it empty.
	RefusedGrants []isolation.Grant

	// SkipLogs skips the Logs case for backends that cannot stream logs under
	// test.
	SkipLogs bool

	// LogsWant, when set, must appear in the output of Logs for a started node.
	// A runner whose log command echoes its arguments uses it to check the
	// backend asked for the right journal.
	LogsWant string

	// ExecStoppedFails says Exec on a stopped node must return an error, as for
	// a machine that is not up. Backends whose node stack is the host leave it
	// false, and either outcome is accepted.
	ExecStoppedFails bool
}

// NewBackend builds a fresh backend and the Options to drive it.
type NewBackend func(t *testing.T) (isolation.Backend, Options)

// Run executes the conformance suite against the backend newBackend builds.
func Run(t *testing.T, newBackend NewBackend) {
	t.Helper()

	for _, tc := range []struct {
		name string
		run  func(t *testing.T, s *session)
	}{
		{"identity", caseIdentity},
		{"prepare", casePrepare},
		{"prepare is idempotent", casePrepareIdempotent},
		{"prepare honors grants", casePrepareGrants},
		{"prepare refuses a grant it cannot honor", casePrepareRefusedGrants},
		{"start makes the node run", caseStart},
		{"start is idempotent", caseStartIdempotent},
		{"stop stops the node", caseStop},
		{"stop is idempotent", caseStopIdempotent},
		{"restart keeps the node running", caseRestart},
		{"exec runs in the node", caseExec},
		{"exec reports command failure", caseExecFailure},
		{"exec on a stopped node fails or reports nothing running", caseExecStopped},
		{"logs returns a stream", caseLogs},
		{"destroy removes the node", caseDestroy},
		{"destroy is idempotent", caseDestroyIdempotent},
		{"full lifecycle", caseLifecycle},
		{"prepare failure is recoverable", caseFailurePrepare},
		{"start failure is recoverable", caseFailureStart},
		{"stop failure is recoverable", caseFailureStop},
		{"restart failure is recoverable", caseFailureRestart},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, opts := newBackend(t)
			require.NotNil(t, b, "newBackend returned no backend")
			require.NotEmpty(t, opts.Spec.Slot, "Options.Spec.Slot is required")

			tc.run(t, &session{b: b, opts: opts})
		})
	}
}

type session struct {
	b    isolation.Backend
	opts Options
}

func (s *session) prepare(t *testing.T) isolation.NodeRoot {
	t.Helper()

	r, err := s.b.Prepare(t.Context(), s.opts.Spec)
	require.NoError(t, err)
	require.NotNil(t, r)

	return r
}

func (s *session) started(t *testing.T) isolation.NodeRoot {
	t.Helper()

	r := s.prepare(t)
	require.NoError(t, s.b.Start(t.Context(), r))
	s.requireRunning(t, r, true)

	return r
}

func (s *session) requireRunning(t *testing.T, r isolation.NodeRoot, want bool) {
	t.Helper()

	st, err := s.b.Status(t.Context(), r)
	require.NoError(t, err)
	require.Equal(t, want, st.Running, "status detail: %s", st.Detail)
}

func (s *session) needFail(t *testing.T) {
	t.Helper()

	if s.opts.Fail == nil {
		t.Skip("runner does not support failure injection")
	}
}

func (s *session) needExec(t *testing.T) {
	t.Helper()

	if len(s.opts.ExecArgs) == 0 {
		t.Skip("runner provided no exec command")
	}
}

func caseIdentity(t *testing.T, s *session) {
	require.NotEmpty(t, s.b.Mode())

	_, err := isolation.ParseMode(string(s.b.Mode()))
	require.NoError(t, err, "Mode must be a known isolation mode")

	// Requirements must be callable before anything is prepared.
	_ = s.b.Requirements()
}

func casePrepare(t *testing.T, s *session) {
	r := s.prepare(t)
	require.Equal(t, s.opts.Spec.Slot, r.Slot())

	p := r.Path("/etc/unbounded/probe")
	require.True(t, strings.HasSuffix(p, "/etc/unbounded/probe"), "Path must keep the in-node path as its suffix, got %q", p)
	require.Equal(t, p, r.Path("/etc/unbounded/probe"), "Path must be deterministic")

	s.requireRunning(t, r, false)
}

func casePrepareIdempotent(t *testing.T, s *session) {
	first := s.prepare(t)
	second := s.prepare(t)

	require.Equal(t, first.Slot(), second.Slot())
	require.Equal(t, first.Path("/x"), second.Path("/x"))
}

func casePrepareGrants(t *testing.T, s *session) {
	if len(s.opts.Grants) == 0 {
		t.Skip("runner provided no grants")
	}

	spec := s.opts.Spec
	spec.Grants = s.opts.Grants

	r, err := s.b.Prepare(t.Context(), spec)
	require.NoError(t, err, "a backend must honor the grants it is given")
	require.Equal(t, spec.Slot, r.Slot())

	require.NoError(t, s.b.Start(t.Context(), r))
	s.requireRunning(t, r, true)

	require.NoError(t, s.b.Stop(t.Context(), r))
	s.requireRunning(t, r, false)
}

func casePrepareRefusedGrants(t *testing.T, s *session) {
	if len(s.opts.RefusedGrants) == 0 {
		t.Skip("runner provided no grant the backend refuses")
	}

	for _, g := range s.opts.RefusedGrants {
		spec := s.opts.Spec
		spec.Grants = []isolation.Grant{g}

		_, err := s.b.Prepare(t.Context(), spec)
		require.Error(t, err, "a grant the backend cannot honor must fail Prepare, not be ignored: %+v", g)
	}

	// The refusals must not have wedged the slot.
	s.started(t)
}

func caseStart(t *testing.T, s *session) {
	s.started(t)
}

func caseStartIdempotent(t *testing.T, s *session) {
	r := s.started(t)

	require.NoError(t, s.b.Start(t.Context(), r))
	s.requireRunning(t, r, true)
}

func caseStop(t *testing.T, s *session) {
	r := s.started(t)

	require.NoError(t, s.b.Stop(t.Context(), r))
	s.requireRunning(t, r, false)
}

func caseStopIdempotent(t *testing.T, s *session) {
	r := s.started(t)

	require.NoError(t, s.b.Stop(t.Context(), r))
	require.NoError(t, s.b.Stop(t.Context(), r), "stopping a stopped node must succeed")
	s.requireRunning(t, r, false)
}

func caseRestart(t *testing.T, s *session) {
	r := s.started(t)

	require.NoError(t, s.b.Restart(t.Context(), r))
	s.requireRunning(t, r, true)
}

func caseExec(t *testing.T, s *session) {
	s.needExec(t)

	r := s.started(t)

	res, err := s.b.Exec(t.Context(), r, isolation.ExecSpec{Args: s.opts.ExecArgs})
	require.NoError(t, err)
	require.Equal(t, s.opts.ExecWant, strings.TrimRight(res.Stdout, "\n"))
}

func caseExecFailure(t *testing.T, s *session) {
	if len(s.opts.ExecFailArgs) == 0 {
		t.Skip("runner provided no failing exec command")
	}

	r := s.started(t)

	_, err := s.b.Exec(t.Context(), r, isolation.ExecSpec{Args: s.opts.ExecFailArgs})
	require.Error(t, err)
}

// caseExecStopped pins that a stopped node never reports a running status, and
// that a command run against it either fails or, for backends whose node is the
// host, still runs. It only requires that Exec returns rather than hanging.
func caseExecStopped(t *testing.T, s *session) {
	s.needExec(t)

	r := s.started(t)
	require.NoError(t, s.b.Stop(t.Context(), r))

	_, err := s.b.Exec(t.Context(), r, isolation.ExecSpec{Args: s.opts.ExecArgs})
	if s.opts.ExecStoppedFails {
		require.Error(t, err, "exec in a stopped node must fail")
	}

	s.requireRunning(t, r, false)
}

func caseLogs(t *testing.T, s *session) {
	if s.opts.SkipLogs {
		t.Skip("runner skips logs")
	}

	r := s.started(t)

	rc, err := s.b.Logs(t.Context(), r, isolation.LogOptions{Lines: 10})
	require.NoError(t, err)
	require.NotNil(t, rc)

	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())

	if s.opts.LogsWant != "" {
		require.Contains(t, string(data), s.opts.LogsWant)
	}
}

func caseDestroy(t *testing.T, s *session) {
	r := s.started(t)

	require.NoError(t, s.b.Stop(t.Context(), r))
	require.NoError(t, s.b.Destroy(t.Context(), r))
	s.requireRunning(t, r, false)
}

func caseDestroyIdempotent(t *testing.T, s *session) {
	r := s.started(t)

	require.NoError(t, s.b.Stop(t.Context(), r))
	require.NoError(t, s.b.Destroy(t.Context(), r))
	require.NoError(t, s.b.Destroy(t.Context(), r), "destroying a destroyed node must succeed")
}

// caseLifecycle runs the whole sequence, then prepares the same slot again to
// show a destroyed node can be rebuilt.
func caseLifecycle(t *testing.T, s *session) {
	r := s.started(t)

	require.NoError(t, s.b.Restart(t.Context(), r))
	s.requireRunning(t, r, true)

	require.NoError(t, s.b.Stop(t.Context(), r))
	require.NoError(t, s.b.Destroy(t.Context(), r))

	r = s.started(t)
	s.requireRunning(t, r, true)
}

func caseFailurePrepare(t *testing.T, s *session) {
	s.needFail(t)

	s.opts.Fail(OpPrepare, ErrInjected)

	_, err := s.b.Prepare(t.Context(), s.opts.Spec)
	require.ErrorIs(t, err, ErrInjected)

	// A failed prepare must leave the slot preparable.
	s.started(t)
}

func caseFailureStart(t *testing.T, s *session) {
	s.needFail(t)

	r := s.prepare(t)

	s.opts.Fail(OpStart, ErrInjected)
	require.ErrorIs(t, s.b.Start(t.Context(), r), ErrInjected)

	// The retry must succeed, with no manual cleanup in between.
	require.NoError(t, s.b.Start(t.Context(), r))
	s.requireRunning(t, r, true)
}

func caseFailureStop(t *testing.T, s *session) {
	s.needFail(t)

	r := s.started(t)

	s.opts.Fail(OpStop, ErrInjected)
	require.ErrorIs(t, s.b.Stop(t.Context(), r), ErrInjected)

	require.NoError(t, s.b.Stop(t.Context(), r))
	s.requireRunning(t, r, false)
}

func caseFailureRestart(t *testing.T, s *session) {
	s.needFail(t)

	r := s.started(t)

	s.opts.Fail(OpRestart, ErrInjected)
	require.ErrorIs(t, s.b.Restart(t.Context(), r), ErrInjected)

	require.NoError(t, s.b.Restart(t.Context(), r))
	s.requireRunning(t, r, true)
}
