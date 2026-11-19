// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package registryauth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// backoffState holds the mutable clock and restart counter a backoff test
// drives.
type backoffState struct {
	i        *Installer
	restarts *int
	now      time.Time
}

func (s *backoffState) count() int { return *s.restarts }

func (s *backoffState) advance(by time.Duration) {
	s.now = s.now.Add(by)
}

// backoffFixture is an installer whose kubelet never activates the managed
// flags, so every reconcile wants to restart. The Revision is set so the
// persisted budget is keyed to a stable policy. The clock is fixed so backoff
// is deterministic.
func backoffFixture(t *testing.T) *backoffState {
	t.Helper()

	i, _, restarts := installerFixture(t)
	i.Revision = "rev-1"

	s := &backoffState{i: i, restarts: restarts, now: time.Now().Add(time.Hour)}

	i.KubeletArgs = func() ([]string, error) {
		// The kubelet restarts but never picks up the flags, so the installer
		// keeps wanting to restart until the budget is spent.
		return []string{"/usr/local/bin/kubelet", "--config=/var/lib/kubelet/config.yaml"}, nil
	}

	i.now = func() time.Time { return s.now }

	return s
}

// reconcileUntilExhausted drives the installer's reconcile loop, advancing the
// clock past each backoff window, until the budget is spent. It returns the
// number of restarts issued.
func reconcileUntilExhausted(t *testing.T, s *backoffState, cfg Config) {
	t.Helper()

	for range 10 * restartBudget {
		err := s.i.Reconcile(context.Background(), cfg)
		if errors.Is(err, ErrRestartBudgetExhausted) {
			return
		}

		// Advance far past any in-progress backoff so the next loop iteration
		// is not clock-gated. The budget, not the clock, is what must stop the
		// restarts.
		s.advance(time.Hour)
	}

	t.Fatal("restart budget was never exhausted; the loop did not terminate")
}

func TestRestartBudgetStopsAtLimit(t *testing.T) {
	s := backoffFixture(t)
	cfg := testManagedConfig(t)

	reconcileUntilExhausted(t, s, cfg)

	// The total restart count must equal the budget, not grow without bound.
	require.Equal(t, restartBudget, s.count())

	// A further reconcile must not restart again and must report exhaustion.
	require.ErrorIs(t, s.i.Reconcile(context.Background(), cfg), ErrRestartBudgetExhausted)
	require.Equal(t, restartBudget, s.count(), "an exhausted budget must not restart the kubelet again")
}

func TestRestartBackoffDelaysSubsequentAttempts(t *testing.T) {
	s := backoffFixture(t)
	cfg := testManagedConfig(t)

	// The first reconcile installs and restarts once (immediate, no backoff).
	_ = s.i.Reconcile(context.Background(), cfg)
	require.Equal(t, 1, s.count())

	// The second reconcile is also allowed (the first attempt schedules a zero
	// delay), and it issues restart #2, which schedules a real backoff.
	_ = s.i.Reconcile(context.Background(), cfg)
	require.Equal(t, 2, s.count())

	// Immediately after, the third reconcile is in backoff: no restart, and the
	// error carries the wait.
	var backoffErr ErrRestartBackoff

	err := s.i.Reconcile(context.Background(), cfg)
	require.True(t, errors.As(err, &backoffErr), "third attempt must be backoff-gated, got %v", err)
	require.True(t, backoffErr.RetryAfter > 0)
	require.Equal(t, 2, s.count(), "a backoff must not restart the kubelet")

	// After the backoff elapses, the next attempt is allowed to restart.
	s.advance(restartBackoffDelay(2) + time.Second)
	_ = s.i.Reconcile(context.Background(), cfg)
	require.Equal(t, 3, s.count())
}

func TestRestartBudgetResetsOnNewRevision(t *testing.T) {
	s := backoffFixture(t)
	cfg := testManagedConfig(t)

	// Exhaust the budget on rev-1.
	reconcileUntilExhausted(t, s, cfg)
	require.Equal(t, restartBudget, s.count())

	// A new policy revision resets the budget, so a restart is allowed again.
	s.i.Revision = "rev-2"
	before := s.count()
	_ = s.i.Reconcile(context.Background(), cfg)
	require.Equal(t, before+1, s.count(), "a new revision must allow a fresh restart")
}

func TestRestartBudgetPersistsAcrossInstallerRestart(t *testing.T) {
	// Two installers share the same node root (same on-disk state) but are
	// separate Go objects, simulating a pod recreation. The budget must survive.
	s := backoffFixture(t)
	cfg := testManagedConfig(t)

	// Exhaust the budget on the first installer.
	reconcileUntilExhausted(t, s, cfg)
	require.ErrorIs(t, s.i.Reconcile(context.Background(), cfg), ErrRestartBudgetExhausted)

	// A second installer object (a recreated pod) with the same revision and
	// node root must still see the exhausted budget, not a fresh one.
	second := &Installer{
		Root:        s.i.Root,
		Executable:  s.i.Executable,
		HostCommand: s.i.HostCommand,
		KubeletArgs: s.i.KubeletArgs,
		Revision:    s.i.Revision,
		now:         s.i.now,
	}

	before := s.count()
	err := second.Reconcile(context.Background(), cfg)
	require.ErrorIs(t, err, ErrRestartBudgetExhausted)
	require.Equal(t, before, s.count(), "a recreated installer must not reset the budget")
}
