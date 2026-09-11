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
	"time"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
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
	rootfsoci "github.com/Azure/unbounded/pkg/agent/phases/rootfs/oci"
)

// A transition explains the two-config state deliberately produced by repave.
// Source configuration and full target intent survive controller changes and
// unavailable original configuration versions. This file contains credentials
// and is root-readable only, just like applied configuration.
type repaveState struct {
	TransitionID      string                                  `json:"transitionID,omitempty"`
	TargetRef         *v1alpha3.MachineConfigurationRefStatus `json:"targetRef,omitempty"`
	TargetBootID      string                                  `json:"targetBootID,omitempty"`
	TargetNodeUID     string                                  `json:"targetNodeUID,omitempty"`
	VerifiedAt        *time.Time                              `json:"verifiedAt,omitempty"`
	RecoveryOperation string                                  `json:"recoveryOperation,omitempty"`
	RecoveryAction    string                                  `json:"recoveryAction,omitempty"`
	ReselectedConfig  *provision.UnboundedAgentConfig         `json:"reselectedConfig,omitempty"`
	ReselectedRef     *v1alpha3.MachineConfigurationRefStatus `json:"reselectedRef,omitempty"`
	Version           int                                     `json:"version"`
	Source            string                                  `json:"source"`
	Target            string                                  `json:"target"`
	Phase             string                                  `json:"phase"`
	SourceConfig      provision.AgentConfig                   `json:"sourceConfig"`
	TargetConfig      provision.UnboundedAgentConfig          `json:"targetConfig"`
	Downloads         *goalstates.DownloadOverrides           `json:"downloads,omitempty"`
	// Installation identity binds this transition to its host owner.
	InstallID string `json:"installID,omitempty"`
}

func repaveStatePath(dir string) string { return filepath.Join(dir, "repave-state.json") }

type repaveStore struct{ dir string }

func defaultRepaveStore() repaveStore { return repaveStore{dir: goalstates.AgentConfigDir} }

func (store repaveStore) Load() (*repaveState, error) { return readRepaveState(store.dir) }

func (store repaveStore) Remove() error {
	if err := os.Remove(repaveStatePath(store.dir)); err != nil {
		return err
	}

	return bootstrap.SyncFilesystems(store.dir)
}

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
	if (s.Version != 1 && s.Version != 2) || !validSlot(s.Source) || !validSlot(s.Target) || s.Source == s.Target ||
		s.SourceConfig.MachineName == "" || s.SourceConfig.MachineName != s.TargetConfig.MachineName ||
		s.SourceConfig.NodeName != s.TargetConfig.NodeName || s.SourceConfig.HostPrefix != s.TargetConfig.HostPrefix {
		return nil, fmt.Errorf("invalid repave transition identity")
	}

	if err := config.ValidateHostPrefix(s.SourceConfig.HostPrefix); err != nil {
		return nil, err
	}

	if s.Version == 2 && s.TransitionID == "" {
		return nil, fmt.Errorf("repave transition ID is required")
	}

	if s.Version == 2 && (s.Phase == "committed" || s.Phase == "cleaning" || s.Phase == "reporting") &&
		(s.TargetBootID == "" || s.TargetNodeUID == "" || s.VerifiedAt == nil) {
		return nil, fmt.Errorf("committed repave is missing target readiness evidence")
	}

	switch s.Phase {
	case "preparing", "switching", "starting", "verifying", "committed", "cleaning", "reporting", "canceling", "canceled":
	default:
		return nil, fmt.Errorf("invalid repave phase %q", s.Phase)
	}

	return &s, nil
}

func saveRepaveState(s *repaveState) error {
	return defaultRepaveStore().Save(s)
}

func (store repaveStore) Save(s *repaveState) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}

	if err := writeFile(repaveStatePath(store.dir), data, 0o600); err != nil {
		return err
	}

	return bootstrap.SyncFilesystems(store.dir)
}

type appliedRepave struct {
	TransitionID    string                                  `json:"transitionID"`
	InstallID       string                                  `json:"installID"`
	Slot            string                                  `json:"slot"`
	Config          provision.AgentConfig                   `json:"config"`
	Configuration   *v1alpha3.MachineConfigurationRefStatus `json:"configuration,omitempty"`
	ProvenanceKnown bool                                    `json:"provenanceKnown"`
}

func (store repaveStore) SaveApplied(s *repaveState) error {
	data, err := json.Marshal(appliedRepave{
		TransitionID: s.TransitionID, InstallID: s.InstallID,
		Slot: s.Target, Config: s.TargetConfig.AgentConfig, Configuration: s.TargetRef, ProvenanceKnown: s.TargetRef != nil,
	})
	if err != nil {
		return err
	}

	if err := writeFile(filepath.Join(store.dir, "repave-applied.json"), data, 0o600); err != nil {
		return err
	}

	return bootstrap.SyncFilesystems(store.dir)
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

		if phase == "cleaning" || phase == "reporting" {
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
		if s.TargetConfig.OCIImage != "" {
			probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			err := rootfsoci.CheckImageReachable(probeCtx, s.TargetConfig.OCIImage)

			cancel()

			if err != nil {
				return fmt.Errorf("target rootfs unavailable: %w", err)
			}
		}

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
		return fmt.Errorf("target verification requires the management readiness gate")
	case "committed":
		if s.TargetBootID == "" || s.TargetNodeUID == "" || s.VerifiedAt == nil {
			return fmt.Errorf("source cleanup requires committed target readiness evidence")
		}

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

		s.Phase = "reporting"
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
	return beginRepaveWithRef(ctx, log, active, cfg, nil)
}

func beginRepaveWithRef(ctx context.Context, log *slog.Logger, active *ActiveMachine, cfg *provision.UnboundedAgentConfig, ref *v1alpha3.MachineConfigurationRefStatus) error {
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
		return fmt.Errorf("repave transition %s is already pending", pending.TransitionID)
	}

	current, err := findActiveMachine(log, goalstates.AgentConfigDir)
	if err != nil {
		return err
	}

	if current.Name != active.Name || !reflect.DeepEqual(current.Config, active.Config) {
		return fmt.Errorf("active node changed before repave acquired installation lock")
	}

	id, err := installstate.NewInstallID()
	if err != nil {
		return err
	}

	s := &repaveState{Version: 2, TransitionID: id, TargetRef: ref, Source: active.Name, Target: goalstates.AlternateMachine(active.Name), Phase: "preparing", SourceConfig: *active.Config, TargetConfig: *cfg}

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

	return nil
}
