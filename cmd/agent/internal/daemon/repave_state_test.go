// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/installstate"
)

func TestRepaveRetriesEveryInterruptedPhase(t *testing.T) {
	t.Parallel()

	phases := []string{"preparing", "switching", "starting", "verifying", "cleaning"}
	for _, interrupted := range phases {
		for _, boundary := range []string{"output", "checkpoint"} {
			if interrupted == "cleaning" && boundary == "checkpoint" {
				continue
			}

			t.Run(interrupted+"/"+boundary, func(t *testing.T) {
				t.Parallel()

				durable := repaveState{Phase: "preparing"}
				s := durable
				injected := errors.New("interrupted repave")
				fail := true
				targetStarted, targetHealthy, sourceRemoved := false, false, false

				var calls []string

				advance := func(_ context.Context, s *repaveState) error {
					calls = append(calls, s.Phase)
					switch s.Phase {
					case "preparing":
						require.False(t, targetStarted, "must not rebuild a running target")
					case "switching":
						require.False(t, targetStarted, "must not clean networking under a running target")
					case "starting":
						targetStarted = true
					case "verifying":
						targetHealthy = true
					case "cleaning":
						require.True(t, targetHealthy, "retain source until target health succeeds")

						sourceRemoved = true
					}

					if fail && s.Phase == interrupted && boundary == "output" {
						fail = false
						return injected
					}

					for i, phase := range phases[:len(phases)-1] {
						if s.Phase == phase {
							s.Phase = phases[i+1]
							break
						}
					}

					return nil
				}
				save := func(s *repaveState) error {
					if fail && durable.Phase == interrupted && boundary == "checkpoint" {
						fail = false
						return injected
					}

					durable = *s

					return nil
				}
				require.ErrorIs(t, runRepaveSteps(t.Context(), &s, advance, save), injected)
				require.Equal(t, interrupted, durable.Phase)
				s = durable // A fresh process reads only the last durable checkpoint.
				calls = nil

				require.NoError(t, runRepaveSteps(t.Context(), &s, advance, save))
				require.Equal(t, interrupted, calls[0])
				require.True(t, sourceRemoved)
			})
		}
	}
}

func TestDiscoveryUnderstandsOwnRepaveIntermediateStates(t *testing.T) {
	t.Parallel()

	for _, phase := range []string{"preparing", "switching", "starting", "verifying", "cleaning"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			source := baseConfig()
			target := *source
			target.Cluster.Version = "v1.33.2"
			s := repaveState{Version: 1, Source: "kube1", Target: "kube2", Phase: phase, SourceConfig: *source, TargetConfig: provision.UnboundedAgentConfig{AgentConfig: target}}
			data, err := json.Marshal(s)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(repaveStatePath(dir), data, 0o600))

			for _, slot := range []string{"kube1", "kube2"} {
				require.NoError(t, os.WriteFile(filepath.Join(dir, slot+"-applied-config.json"), []byte("{}"), 0o600))
			}

			active, err := findActiveMachine(slog.New(slog.DiscardHandler), dir)
			require.NoError(t, err)

			if phase == "cleaning" || phase == "verifying" {
				require.Equal(t, "kube2", active.Name)
				require.Equal(t, "v1.33.2", active.Config.Cluster.Version)
			} else {
				require.Equal(t, "kube1", active.Name)
			}
		})
	}
}

func TestRepaveOwnerRejectsDifferentOrUnfinishedInstallations(t *testing.T) {
	t.Parallel()

	s := &repaveState{InstallID: "owner", SourceConfig: *baseConfig()}
	valid := installstate.Record{
		InstallID: s.InstallID, MachineName: s.SourceConfig.MachineName,
		HostPrefix: goalstates.ResolveHostPaths(s.SourceConfig.HostPrefix).Prefix, Checkpoint: installstate.CheckpointComplete,
	}
	require.NoError(t, validateRepaveOwner(s, valid))

	for _, checkpoint := range []installstate.Checkpoint{installstate.CheckpointPreparingHost, installstate.CheckpointPreparingRootFS, installstate.CheckpointStartingNode, installstate.CheckpointInstallingDaemon, installstate.CheckpointResetting} {
		record := valid
		record.Checkpoint = checkpoint
		require.Error(t, validateRepaveOwner(s, record))
	}

	record := valid
	record.Checkpoint = installstate.CheckpointRepairingDaemon
	require.NoError(t, validateRepaveOwner(s, record))

	for _, mutate := range []func(*installstate.Record){
		func(r *installstate.Record) { r.InstallID = "different" },
		func(r *installstate.Record) { r.MachineName = "different" },
		func(r *installstate.Record) { r.HostPrefix = "/different" },
	} {
		record := valid
		mutate(&record)
		require.Error(t, validateRepaveOwner(s, record))
	}
}

func TestNodeRestartRespectsInstallationLock(t *testing.T) {
	original := installstate.LockPathForTest
	installstate.LockPathForTest = filepath.Join(t.TempDir(), "install.lock")
	t.Cleanup(func() { installstate.LockPathForTest = original })

	lock, err := installstate.AcquireLock()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, lock.Release()) })
	err = (nspawnNodeOperator{}).RestartNode(t.Context(), slog.New(slog.DiscardHandler), &ActiveMachine{Name: "kube1", Config: baseConfig()})
	require.ErrorIs(t, err, installstate.ErrLockHeld)
}

func TestRepaveStateRetainsResolvedOfflineMetadata(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := repaveState{
		Version: 1, Source: "kube1", Target: "kube2", Phase: "starting",
		SourceConfig: *baseConfig(), TargetConfig: provision.UnboundedAgentConfig{AgentConfig: *baseConfig()},
		Downloads: &goalstates.DownloadOverrides{CoreDNS: &goalstates.DownloadSource{Version: "1.12.3", URL: "file:///unavailable-original-bundle/coredns"}},
	}
	data, err := json.Marshal(s)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(repaveStatePath(dir), data, 0o600))
	loaded, err := readRepaveState(dir)
	require.NoError(t, err)
	require.Equal(t, s.Downloads, loaded.Downloads)
}

func TestRepaveStateRejectsUnknownOwnership(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(repaveStatePath(dir), []byte(`{"version":1,"source":"kube1","target":"kube1","phase":"cleaning"}`), 0o600))
	_, err := readRepaveState(dir)
	require.Error(t, err)
}
