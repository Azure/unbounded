// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package migrate

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestPlan(t *testing.T) {
	t.Parallel()

	n, c, no := isolation.ModeNSpawn, isolation.ModeContainer, isolation.ModeNone

	tests := []struct {
		name     string
		from, to isolation.Mode
		want     Strategy
		wantErr  string
	}{
		{name: "nspawn to container is in place", from: n, to: c, want: InPlace},
		{name: "container to nspawn is in place", from: c, to: n, want: InPlace},
		{name: "nspawn to none is a reprovision", from: n, to: no, want: Reprovision},
		{name: "none to nspawn is a reprovision", from: no, to: n, want: Reprovision},
		{name: "none to container is a reprovision", from: no, to: c, want: Reprovision},
		{name: "container to none is a reprovision", from: c, to: no, want: Reprovision},
		{name: "same mode rejected", from: n, to: n, wantErr: "already in isolation mode"},
		{name: "container to container rejected", from: c, to: c, wantErr: "already in isolation mode"},
		{name: "none to none rejected", from: no, to: no, wantErr: "already in isolation mode"},
		{name: "unknown target rejected", from: n, to: "microvm", wantErr: "unsupported isolation mode"},
		{name: "unknown source rejected", from: "microvm", to: n, wantErr: "unsupported isolation mode"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := Plan(tt.from, tt.to)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

// harness wires fake backends and records the order of every hook and backend
// call in one log.
type harness struct {
	source, target *isolation.FakeBackend
	log            *[]string
	fail           map[string]error
	spec           InPlaceSpec
}

func newHarness() *harness {
	var log []string

	h := &harness{
		source: &isolation.FakeBackend{FakeMode: isolation.ModeNSpawn},
		target: &isolation.FakeBackend{FakeMode: isolation.ModeContainer, Dir: "/var/lib/machines/kube2"},
		log:    &log,
		fail:   map[string]error{},
	}

	hook := func(name string) func(context.Context) error {
		return func(context.Context) error {
			log = append(log, name)
			return h.fail[name]
		}
	}

	h.spec = InPlaceSpec{
		Source:     h.source,
		SourceRoot: isolation.SlotName("kube1"),
		Target:     h.target,
		TargetSpec: isolation.NodeSpec{Slot: "kube2"},
		Preflight: func(context.Context, isolation.Backend) error {
			log = append(log, "preflight")
			return h.fail["preflight"]
		},
		Download:       hook("download"),
		CleanupNetwork: hook("cleanup-network"),
		PersistTarget: func(context.Context, isolation.SlotRef) error {
			log = append(log, "persist")
			return h.fail["persist"]
		},
		RestoreSource:      hook("restore-source"),
		WaitReady:          hook("wait-ready"),
		RemoveSourceConfig: hook("remove-source-config"),
	}

	return h
}

// hooks returns the migration hooks that ran, in order. Backend calls are
// read from the fakes.
func (h *harness) hooks() []string { return *h.log }

func TestRunInPlaceSuccess(t *testing.T) {
	t.Parallel()

	h := newHarness()

	res, err := RunInPlace(context.Background(), h.spec)
	require.NoError(t, err)
	require.False(t, res.RolledBack)
	require.Equal(t, "kube2", res.Target.Slot())

	require.Equal(t, []string{"preflight", "download", "cleanup-network", "persist", "wait-ready", "remove-source-config"}, h.hooks())
	require.Equal(t, []string{"prepare kube2", "start kube2"}, h.target.Calls())
	require.Equal(t, []string{"stop kube1", "destroy kube1"}, h.source.Calls())
}

func TestRunInPlacePreflightFailureMutatesNothing(t *testing.T) {
	t.Parallel()

	h := newHarness()
	h.fail["preflight"] = errors.New("no runtime")

	_, err := RunInPlace(context.Background(), h.spec)
	require.ErrorContains(t, err, "no runtime")
	require.Empty(t, h.source.Calls())
	require.Empty(t, h.target.Calls())
	require.Equal(t, []string{"preflight"}, h.hooks())
}

func TestRunInPlaceDownloadFailureMutatesNothing(t *testing.T) {
	t.Parallel()

	h := newHarness()
	h.fail["download"] = errors.New("offline")

	_, err := RunInPlace(context.Background(), h.spec)
	require.ErrorContains(t, err, "offline")
	require.Empty(t, h.source.Calls())
	require.Empty(t, h.target.Calls())
}

func TestRunInPlacePrepareFailureLeavesSourceRunning(t *testing.T) {
	t.Parallel()

	h := newHarness()
	h.target.PrepareErr = errors.New("unpack failed")

	_, err := RunInPlace(context.Background(), h.spec)
	require.ErrorContains(t, err, "unpack failed")
	require.Empty(t, h.source.Calls())
	require.NotContains(t, h.hooks(), "cleanup-network")
}

func TestRunInPlaceCleanupNetworkFailureRestartsSource(t *testing.T) {
	t.Parallel()

	h := newHarness()
	h.fail["cleanup-network"] = errors.New("link busy")

	res, err := RunInPlace(context.Background(), h.spec)
	require.ErrorContains(t, err, "link busy")
	require.True(t, res.RolledBack)
	require.Equal(t, []string{"stop kube1", "start kube1"}, h.source.Calls())
	require.Equal(t, []string{"prepare kube2", "destroy kube2"}, h.target.Calls())
}

func TestRunInPlaceTargetStartFailureRollsBack(t *testing.T) {
	t.Parallel()

	h := newHarness()
	h.target.StartErr = errors.New("container did not start")

	res, err := RunInPlace(context.Background(), h.spec)
	require.ErrorContains(t, err, "container did not start")
	require.True(t, res.RolledBack)
	require.Nil(t, res.Target)
	require.Equal(t, []string{"stop kube1", "start kube1"}, h.source.Calls(), "source must be restarted and not destroyed")
	require.Equal(t, []string{"prepare kube2", "start kube2", "stop kube2", "destroy kube2", "status kube2"}, h.target.Calls())
	require.NotContains(t, h.hooks(), "persist")
	require.NotContains(t, h.hooks(), "restore-source")
	require.NotContains(t, h.hooks(), "remove-source-config")
}

func TestRunInPlaceNotReadyRestoresSourceConfig(t *testing.T) {
	t.Parallel()

	h := newHarness()
	h.fail["wait-ready"] = errors.New("timed out")

	res, err := RunInPlace(context.Background(), h.spec)
	require.ErrorContains(t, err, "timed out")
	require.True(t, res.RolledBack)
	require.Contains(t, h.hooks(), "persist")
	require.Contains(t, h.hooks(), "restore-source")
	require.NotContains(t, h.hooks(), "remove-source-config")
	require.Equal(t, []string{"stop kube1", "start kube1"}, h.source.Calls())
}

func TestRunInPlaceSourceStopFailureStillRestartsSource(t *testing.T) {
	t.Parallel()

	h := newHarness()
	h.source.StopErr = errors.New("stuck")

	res, err := RunInPlace(context.Background(), h.spec)
	require.ErrorContains(t, err, "stuck")
	require.True(t, res.RolledBack)
	require.Equal(t, []string{"stop kube1", "start kube1"}, h.source.Calls())
}

func TestRunInPlaceRollbackFailureIsReported(t *testing.T) {
	t.Parallel()

	h := newHarness()
	h.target.StartErr = errors.New("start failed")
	h.source.StartErr = errors.New("source will not come back")

	res, err := RunInPlace(context.Background(), h.spec)
	require.ErrorIs(t, err, ErrRollbackFailed)
	require.ErrorContains(t, err, "start failed")
	require.ErrorContains(t, err, "source will not come back")
	require.False(t, res.RolledBack)
}

// The source must never start while the target may still be up: two node
// stacks would share the kubelet's identity and the host's networking.
func TestRunInPlaceRollbackLeavesSourceStoppedWhileTargetMayRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		breakIt func(h *harness)
		want    string
	}{
		{
			name:    "target stop fails",
			breakIt: func(h *harness) { h.target.StopErr = errors.New("stop timed out") },
			want:    "stop timed out",
		},
		{
			name:    "target destroy fails",
			breakIt: func(h *harness) { h.target.DestroyErr = errors.New("busy") },
			want:    "busy",
		},
		{
			name:    "target still reported running",
			breakIt: func(h *harness) { h.target.FakeStatus = isolation.Status{Running: true} },
			want:    "still running",
		},
		{
			name:    "target status unknown",
			breakIt: func(h *harness) { h.target.StatusErr = errors.New("probe failed") },
			want:    "probe failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness()
			h.fail["wait-ready"] = errors.New("not ready")
			tt.breakIt(h)

			res, err := RunInPlace(context.Background(), h.spec)
			require.ErrorIs(t, err, ErrRollbackFailed)
			require.ErrorContains(t, err, tt.want)
			require.ErrorContains(t, err, "was not restarted")
			require.False(t, res.RolledBack)
			require.Equal(t, []string{"stop kube1"}, h.source.Calls(), "the source must stay stopped")
		})
	}
}

// A target that never started needs no status check before the source comes
// back.
func TestRunInPlaceRollbackBeforeTargetStartSkipsTheStatusCheck(t *testing.T) {
	t.Parallel()

	h := newHarness()
	h.fail["cleanup-network"] = errors.New("nft failed")
	h.target.StatusErr = errors.New("must not be asked")

	res, err := RunInPlace(context.Background(), h.spec)
	require.ErrorContains(t, err, "nft failed")
	require.NotErrorIs(t, err, ErrRollbackFailed)
	require.True(t, res.RolledBack)
	require.Equal(t, []string{"stop kube1", "start kube1"}, h.source.Calls())
	require.NotContains(t, h.target.Calls(), "status kube2")
}

func TestRunInPlaceRollbackSurvivesCancellation(t *testing.T) {
	t.Parallel()

	h := newHarness()
	ctx, cancel := context.WithCancel(context.Background())

	h.spec.WaitReady = func(context.Context) error {
		cancel()
		return ctx.Err()
	}

	// The fake ignores context, so assert through a recording wrapper that the
	// rollback is given a live context.
	h.spec.RestoreSource = func(c context.Context) error {
		require.NoError(t, c.Err(), "rollback must not inherit the canceled context")
		return nil
	}

	res, err := RunInPlace(ctx, h.spec)
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, res.RolledBack)
}

func TestRunInPlaceCleanupFailureAfterSuccessDoesNotRollBack(t *testing.T) {
	t.Parallel()

	h := newHarness()
	h.source.DestroyErr = errors.New("busy")

	res, err := RunInPlace(context.Background(), h.spec)
	require.ErrorContains(t, err, "destroy source")
	require.NotNil(t, res.Target, "the node already runs on the target")
	require.False(t, res.RolledBack)
	require.Equal(t, []string{"prepare kube2", "start kube2"}, h.target.Calls())
}

// withPrepareHost adds a PrepareHost hook to the harness that records itself
// and fails with h.fail["prepare-host"].
func (h *harness) withPrepareHost() {
	h.spec.PrepareHost = func(context.Context) error {
		*h.log = append(*h.log, "prepare-host")
		return h.fail["prepare-host"]
	}
}

func TestRunInPlacePreparesTheHostOnceTheTargetIsUp(t *testing.T) {
	t.Parallel()

	h := newHarness()
	h.withPrepareHost()

	var steps []Step

	h.spec.Record = func(_ context.Context, step Step) error {
		steps = append(steps, step)
		*h.log = append(*h.log, "record "+string(step))

		return nil
	}

	_, err := RunInPlace(context.Background(), h.spec)
	require.NoError(t, err)

	hooks := h.hooks()
	require.Equal(t, []string{"wait-ready", "record ready", "prepare-host", "remove-source-config", "record complete"}, hooks[len(hooks)-5:])
	require.Equal(t, StepComplete, steps[len(steps)-1])
}

func TestRunInPlaceHostPreparationFailureDoesNotRollBack(t *testing.T) {
	t.Parallel()

	h := newHarness()
	h.withPrepareHost()
	h.fail["prepare-host"] = errors.New("nft busy")

	var steps []Step

	h.spec.Record = func(_ context.Context, step Step) error {
		steps = append(steps, step)
		return nil
	}

	res, err := RunInPlace(context.Background(), h.spec)
	require.ErrorContains(t, err, "prepare the host for container: nft busy")
	require.NotNil(t, res.Target, "the node already runs on the target")
	require.False(t, res.RolledBack)
	require.Equal(t, []string{"stop kube1", "destroy kube1"}, h.source.Calls(), "the source is still cleaned up")
	require.Equal(t, StepReady, steps[len(steps)-1], "not complete, so a resume prepares the host again")
}

func TestRunInPlaceRollbackDoesNotPrepareTheHost(t *testing.T) {
	t.Parallel()

	h := newHarness()
	h.withPrepareHost()
	h.fail["wait-ready"] = errors.New("not ready")

	_, err := RunInPlace(context.Background(), h.spec)
	require.Error(t, err)
	require.NotContains(t, h.hooks(), "prepare-host")
}

func TestResumeInPlaceAtReadyPreparesTheHost(t *testing.T) {
	t.Parallel()

	h := newHarness()
	h.withPrepareHost()
	h.spec.TargetRoot = isolation.SlotName("kube2")

	_, err := ResumeInPlace(context.Background(), h.spec, StepReady)
	require.NoError(t, err)
	require.Equal(t, []string{"prepare-host", "remove-source-config"}, h.hooks())
}

func TestRunInPlaceIncompleteSpec(t *testing.T) {
	t.Parallel()

	_, err := RunInPlace(context.Background(), InPlaceSpec{})
	require.ErrorContains(t, err, "incomplete")
}
