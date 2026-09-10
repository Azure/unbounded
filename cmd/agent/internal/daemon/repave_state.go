// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"

	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/bootstrap"
	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/installstate"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestop"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
)

// A transition explains the two-config state deliberately produced by repave.
// Source configuration and full target intent survive controller changes and
// unavailable original configuration versions. This file contains credentials
// and is root-readable only, just like applied configuration.
type repaveState struct {
	Version      int                            `json:"version"`
	Source       string                         `json:"source"`
	Target       string                         `json:"target"`
	Phase        string                         `json:"phase"`
	SourceConfig provision.AgentConfig          `json:"sourceConfig"`
	TargetConfig provision.UnboundedAgentConfig `json:"targetConfig"`
	Downloads    *goalstates.DownloadOverrides  `json:"downloads,omitempty"`
	// Installation identity binds this transition to its host owner.
	InstallID string `json:"installID,omitempty"`
}

func repaveStatePath(dir string) string { return filepath.Join(dir, "repave-state.json") }

func readRepaveState(dir string) (*repaveState, error) {
	data, err := os.ReadFile(repaveStatePath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	var s repaveState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}

	validSlot := func(slot string) bool { return slot == "kube1" || slot == "kube2" }
	if s.Version != 1 || !validSlot(s.Source) || !validSlot(s.Target) || s.Source == s.Target ||
		s.SourceConfig.MachineName == "" || s.SourceConfig.MachineName != s.TargetConfig.MachineName ||
		s.SourceConfig.NodeName != s.TargetConfig.NodeName || s.SourceConfig.HostPrefix != s.TargetConfig.HostPrefix {
		return nil, fmt.Errorf("invalid repave transition identity")
	}

	if err := config.ValidateHostPrefix(s.SourceConfig.HostPrefix); err != nil {
		return nil, err
	}

	switch s.Phase {
	case "preparing", "switching", "starting", "verifying", "cleaning":
	default:
		return nil, fmt.Errorf("invalid repave phase %q", s.Phase)
	}

	return &s, nil
}

func saveRepaveState(s *repaveState) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}

	if err := writeFile(repaveStatePath(goalstates.AgentConfigDir), data, 0o600); err != nil {
		return err
	}

	return bootstrap.SyncFilesystems(goalstates.AgentConfigDir)
}

// ResumePendingRepave is run before daemon discovery and before drift checks.
// Node deletion events are not replayed after process restart, so recovery must
// not depend on observing another deletion.
func (nspawnNodeOperator) ResumePendingRepave(ctx context.Context, log *slog.Logger) error {
	s, err := readRepaveState(goalstates.AgentConfigDir)
	if err != nil || s == nil {
		return err
	}

	return withInstallLock(log, &installStateTask{
		name: "resume-repave", log: log,
		run: func(ctx context.Context, log *slog.Logger) error {
			// Reread after acquisition; reset may have removed the transition.
			s, err := readRepaveState(goalstates.AgentConfigDir)
			if err != nil || s == nil {
				return err
			}

			return driveRepave(ctx, log, s)
		},
	}).Do(ctx)
}

func driveRepave(ctx context.Context, log *slog.Logger, s *repaveState) error {
	record, err := installstate.DefaultStore().Load()
	if err != nil && (s.InstallID != "" || !errors.Is(err, installstate.ErrNotFound)) {
		return err
	}

	if err == nil {
		if err := validateRepaveOwner(s, record); err != nil {
			return err
		}
	}

	return runRepaveSteps(ctx, s, func(ctx context.Context, s *repaveState) error {
		return advanceRepave(ctx, log, s)
	}, saveRepaveState)
}

// Each phase is retryable until its outputs and next phase are durable.
// A failed checkpoint write must leave recovery at the previous phase.
func runRepaveSteps(ctx context.Context, s *repaveState, advance func(context.Context, *repaveState) error, save func(*repaveState) error) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		phase := s.Phase
		if err := advance(ctx, s); err != nil {
			return err
		}

		if phase == "cleaning" {
			return nil
		}

		if err := save(s); err != nil {
			return err
		}
	}
}

func advanceRepave(ctx context.Context, log *slog.Logger, s *repaveState) error {
	log.Info("resuming repave transition", "source", s.Source, "target", s.Target, "phase", s.Phase)

	switch s.Phase {
	case "preparing":
		downloads, archives, err := provision.ResolveDownloadOverridesWithOfflineArtifacts(ctx, &s.TargetConfig)
		if err != nil {
			return err
		}

		gs, err := goalstates.ResolveMachine(log, &s.TargetConfig.AgentConfig, s.Target, downloads)
		if err != nil {
			return err
		}

		if err := phases.Serial(log, rootfs.DownloadContainerImageArchives(log, archives), rootfs.Provision(log, gs.RootFS, rootfs.RebuildOwned)).Do(ctx); err != nil {
			return err
		}

		if err := bootstrap.SyncFilesystems(gs.RootFS.MachineDir, gs.RootFS.HostPaths.Prefix, goalstates.SystemdSystemDir, goalstates.SystemdNSpawnDir); err != nil {
			return err
		}

		// Preserve resolved artifact metadata for startup without reopening
		// the original offline bundle after preparation has completed.
		s.Downloads = downloads
		s.Phase = "switching"
	case "switching":
		if err := phases.Serial(log, nodestop.StopNode(log, s.Source), reset.CleanupNetwork(log)).Do(ctx); err != nil {
			return err
		}

		if err := bootstrap.SyncFilesystems(goalstates.SystemdSystemDir); err != nil {
			return err
		}
		// Record before target startup so a retry never repeats network
		// cleanup beneath a target that may already be running.
		s.Phase = "starting"
	case "starting":
		// No artifact acquisition after preparation: target outputs are durable.
		gs, err := goalstates.ResolveMachine(log, &s.TargetConfig.AgentConfig, s.Target, s.Downloads)
		if err != nil {
			return err
		}

		if err := phases.Serial(log, nodestart.StartNode(log, gs.NodeStart), PersistAppliedConfig(log, s.Target, &s.TargetConfig.AgentConfig)).Do(ctx); err != nil {
			return err
		}

		if err := bootstrap.SyncFilesystems(gs.RootFS.MachineDir, goalstates.AgentConfigDir, goalstates.SystemdSystemDir); err != nil {
			return err
		}

		s.Phase = "verifying"
	case "verifying":
		gs, err := goalstates.ResolveMachine(log, &s.TargetConfig.AgentConfig, s.Target, s.Downloads)
		if err != nil {
			return err
		}

		if err := phases.Serial(log, nodestart.StartNode(log, gs.NodeStart), nodestart.WaitForKubelet(log, s.Target)).Do(ctx); err != nil {
			return err
		}
		// This durable phase change commits target ownership before source removal.
		s.Phase = "cleaning"
	case "cleaning":
		if err := reset.CleanupMachine(log, s.Source).Do(ctx); err != nil {
			return err
		}

		for _, path := range []string{goalstates.AppliedConfigPath(s.Source), goalstates.AppliedConfigChecksumPath(s.Source)} {
			if err := removeOwnedFile(path); err != nil {
				return err
			}
		}

		if err := bootstrap.SyncFilesystems("/var/lib/machines", goalstates.AgentConfigDir, goalstates.SystemdSystemDir, goalstates.SystemdNSpawnDir); err != nil {
			return err
		}

		if err := os.Remove(repaveStatePath(goalstates.AgentConfigDir)); err != nil {
			return err
		}

		return bootstrap.SyncFilesystems(goalstates.AgentConfigDir)
	default:
		return fmt.Errorf("invalid repave phase %q", s.Phase)
	}

	return nil
}

func validateRepaveOwner(s *repaveState, record installstate.Record) error {
	if record.InstallID != s.InstallID || record.MachineName != s.SourceConfig.MachineName ||
		record.HostPrefix != goalstates.ResolveHostPaths(s.SourceConfig.HostPrefix).Prefix ||
		(record.Checkpoint != installstate.CheckpointComplete && record.Checkpoint != installstate.CheckpointRepairingDaemon) {
		return fmt.Errorf("repave transition does not belong to the current completed installation")
	}

	return nil
}

func beginRepave(ctx context.Context, log *slog.Logger, active *ActiveMachine, cfg *provision.UnboundedAgentConfig) error {
	lock, err := installstate.AcquireLock()
	if err != nil {
		return err
	}
	defer func() {
		if err := lock.Release(); err != nil {
			log.Warn("release repave lock", "error", err)
		}
	}()

	pending, err := readRepaveState(goalstates.AgentConfigDir)
	if err != nil {
		return err
	}

	if pending != nil {
		return driveRepave(ctx, log, pending)
	}

	current, err := findActiveMachine(log, goalstates.AgentConfigDir)
	if err != nil {
		return err
	}

	if current.Name != active.Name || !reflect.DeepEqual(current.Config, active.Config) {
		return fmt.Errorf("active node changed before repave acquired installation lock")
	}

	s := &repaveState{Version: 1, Source: active.Name, Target: goalstates.AlternateMachine(active.Name), Phase: "preparing", SourceConfig: *active.Config, TargetConfig: *cfg}

	if record, err := installstate.DefaultStore().Load(); err == nil {
		if record.Checkpoint != installstate.CheckpointComplete {
			return fmt.Errorf("repave requires completed bootstrap")
		}

		s.InstallID = record.InstallID
		if err := validateRepaveOwner(s, record); err != nil {
			return err
		}
	} else if !errors.Is(err, installstate.ErrNotFound) {
		return err
	}

	if err := saveRepaveState(s); err != nil {
		return err
	}

	return driveRepave(ctx, log, s)
}
