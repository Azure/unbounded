// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package registryauth

import (
	"context"
	"errors"
	"os"
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

func TestFailedRestartConsumesBudget(t *testing.T) {
	// A failed or timed-out systemctl restart must consume the budget, so it is
	// not re-issued on every tick. The budget bounds attempts, not successes.
	i, _, restarts := installerFixture(t)
	i.Revision = "rev-1"
	i.KubeletArgs = func() ([]string, error) {
		return []string{"/usr/local/bin/kubelet", "--config=/var/lib/kubelet/config.yaml"}, nil
	}

	now := time.Now().Add(time.Hour)
	i.now = func() time.Time { return now }

	cfg := testManagedConfig(t)

	// The first reconcile issues a restart that fails. Wrap the fixture's
	// counting HostCommand so the failed attempt is still counted.
	counting := i.HostCommand
	i.HostCommand = func(_ context.Context, args ...string) error {
		if args[0] == "restart" {
			*restarts++

			return errors.New("systemctl timed out")
		}

		return counting(context.Background(), args...)
	}

	err := i.Reconcile(context.Background(), cfg)
	require.ErrorContains(t, err, "systemctl timed out")
	require.Equal(t, 1, *restarts, "the attempt is charged even though the restart failed")

	i.HostCommand = counting

	// Keep reconciling, advancing the clock past each backoff window. The total
	// number of (failing) restarts must be bounded by the budget, not grow on
	// every tick. The first attempt is immediate by design, so the backoff gates
	// attempts from the second onward; what must hold is the overall cap.
	for range 10 * restartBudget {
		err := i.Reconcile(context.Background(), cfg)
		if errors.Is(err, ErrRestartBudgetExhausted) {
			break
		}

		now = now.Add(time.Hour)
	}

	require.Equal(t, restartBudget, *restarts, "failed restarts must be bounded by the budget")
}

func TestCorruptBackoffFailsClosed(t *testing.T) {
	// An unreadable or corrupt .restart-backoff must fail closed with a
	// diagnostic, not silently reset the budget to fresh.
	i, _, restarts := installerFixture(t)
	i.Revision = "rev-1"
	i.KubeletArgs = func() ([]string, error) {
		return []string{"/usr/local/bin/kubelet", "--config=/var/lib/kubelet/config.yaml"}, nil
	}
	now := time.Now().Add(time.Hour)
	i.now = func() time.Time { return now }

	cfg := testManagedConfig(t)

	// Establish ownership first so the install dir is owned and the restart
	// block is reached before a foreign-ownership refusal.
	i.KubeletArgs = func() ([]string, error) {
		return []string{
			"/usr/local/bin/kubelet", "--config=/var/lib/kubelet/config.yaml",
			"--image-credential-provider-config=" + providerConfigPath, "--image-credential-provider-bin-dir=" + installDir + "/bin",
		}, nil
	}

	require.NoError(t, i.Reconcile(context.Background(), cfg))

	baseline := *restarts

	// Now corrupt the persisted budget. The next reconcile must fail closed with
	// a diagnostic rather than treating the corrupt state as a fresh budget.
	require.NoError(t, os.WriteFile(i.path(backoffPath), []byte("{corrupt json"), 0o600))

	// Force a restart to be needed so the restart block (and the backoff read)
	// is reached.
	i.KubeletArgs = func() ([]string, error) {
		return []string{"/usr/local/bin/kubelet", "--config=/var/lib/kubelet/config.yaml"}, nil
	}

	err := i.Reconcile(context.Background(), cfg)
	require.ErrorContains(t, err, "restart backoff state")
	require.Equal(t, baseline, *restarts, "a corrupt budget must not be treated as a fresh one")
}

func TestExhaustedBudgetAcknowledgesLaterRecovery(t *testing.T) {
	// A kubelet that recovers and activates the managed flags after the restart
	// budget is spent must be observed as healthy on the next reconciliation,
	// without another disruptive restart and without a terminal failure. The
	// budget bounds disruption, not observation.
	i, _, restarts := installerFixture(t)
	i.Revision = "rev-1"

	activated := false
	i.KubeletArgs = func() ([]string, error) {
		if activated {
			return []string{
				"/usr/local/bin/kubelet", "--config=/var/lib/kubelet/config.yaml",
				"--image-credential-provider-config=" + providerConfigPath, "--image-credential-provider-bin-dir=" + installDir + "/bin",
			}, nil
		}

		return []string{"/usr/local/bin/kubelet", "--config=/var/lib/kubelet/config.yaml"}, nil
	}

	now := time.Now().Add(time.Hour)
	i.now = func() time.Time { return now }

	cfg := testManagedConfig(t)

	// Exhaust the budget while the kubelet has not activated the flags.
	for range restartBudget {
		_ = i.Reconcile(context.Background(), cfg)
		now = now.Add(time.Hour) // clear any backoff window so the next attempt is not clock-gated
	}

	// The kubelet now activates the flags (recovered). The next reconciliation
	// must acknowledge activation without issuing another restart and without
	// returning a terminal failure.
	before := *restarts
	activated = true

	require.NoError(t, i.Reconcile(context.Background(), cfg))
	require.Equal(t, before, *restarts, "observing a recovered kubelet must not restart it again")

	// The budget is cleared, so a subsequent reconciliation is steady and
	// budget-free.
	require.NoError(t, i.Reconcile(context.Background(), cfg))
	require.Equal(t, before, *restarts)
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
