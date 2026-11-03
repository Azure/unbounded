// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/Azure/unbounded/cmd/agent/internal/containerops"
	"github.com/Azure/unbounded/cmd/agent/internal/hostnode"
	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	"github.com/Azure/unbounded/cmd/agent/internal/noneops"
	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/internal/fsutil"
	"github.com/Azure/unbounded/internal/hostroot"
	"github.com/Azure/unbounded/pkg/agent/appliedconfig"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
)

// ResetAgent removes the unbounded-agent and all associated resources, stopping
// the daemon first. The daemon's own operation path stops it last instead, so
// that ordering stays with the caller.
func ResetAgent(log *slog.Logger) phases.Task {
	mode := detectResetMode(log)

	return ownedReset(log, installstate.DefaultStore(), mode, phases.Serial(log, StopDaemon(log), resetResourcesFor(log, mode)))
}

// ResetAgentForReprovision is ResetAgent for the reprovision handoff: it leaves
// the handoff's unit, binary, configuration and journal in place.
func ResetAgentForReprovision(log *slog.Logger) phases.Task {
	mode := detectResetMode(log)

	return ownedReset(log, installstate.DefaultStore(), mode, phases.Serial(log, StopDaemon(log), resetNodeResourcesFor(log, mode)))
}

func ownedReset(log *slog.Logger, store *installstate.Store, mode isolation.Mode, inner phases.Task) phases.Task {
	// The composed name keeps the underlying cleanup sequence visible to callers
	// and to the reset ordering test.
	return hostnode.Func("owned-reset("+inner.Name()+")", func(ctx context.Context) error {
		lock, err := store.AcquireLock()
		if err != nil {
			return err
		}

		defer func() {
			if err := lock.Release(); err != nil {
				log.Error("release reset lock", "error", err)
			}
		}()

		return resetUnderLock(ctx, log, store, mode, inner)
	})
}

// recordForTeardown returns the record reset should mark as resetting.
//
// Any record it cannot read is replaced rather than obeyed. Reset is about to
// delete it, so refusing to proceed protects nothing and costs everything:
// decide rejects the same unreadable record, so start is refused too, and the
// host is left with no way out through either path. The guide tells operators
// to keep this file intact, so it must not be the thing that strands them.
func recordForTeardown(log *slog.Logger, store *installstate.Store) (installstate.Record, error) {
	r, err := store.Load()
	if err == nil {
		return r, nil
	}

	if !errors.Is(err, installstate.ErrNotFound) {
		log.Warn("installation record is unreadable; replacing it for teardown", "error", err)
	}

	return installstate.NewRecord("legacy-reset", "legacy-reset")
}

func resetUnderLock(ctx context.Context, log *slog.Logger, store *installstate.Store, mode isolation.Mode, inner phases.Task) error {
	r, err := recordForTeardown(log, store)
	if err != nil {
		return err
	}

	r.Phase = installstate.Resetting
	if err := store.Save(r); err != nil {
		return err
	}
	// Cancel recovery waiting on ownership before removing its executable.
	if err := stopRecoveryUnit(ctx, log); err != nil {
		return err
	}

	paths := teardownSyncPaths(store.Root())
	// The node's own directory may be on another filesystem than /etc.
	switch mode {
	case isolation.ModeNone:
		paths = append(paths, none.DefaultBase)
	case isolation.ModeContainer:
		paths = append(paths, container.DefaultBase)
	case resetUnknown:
		paths = append(paths, none.DefaultBase, container.DefaultBase)
	}

	return durableReset(ctx, store, inner, paths, unix.Syncfs)
}

// teardownSyncPaths returns the directories whose filesystems a teardown has to
// persist: those holding the agent's files under either root, and the one the
// host root link is in. They are resolved now, while the host root still leads
// to the files; durableReset opens the nearest existing ancestor of each.
func teardownSyncPaths(storeRoot string) []string {
	return []string{
		"/etc",
		"/var/lib/machines",
		filepath.Dir(hostroot.Path),
		hostroot.Resolve(),
		hostroot.LegacyPath,
		storeRoot,
	}
}

func stopRecoveryUnit(ctx context.Context, log *slog.Logger) error {
	if err := executil.RunCmd(ctx, log, executil.Systemctl(), "stop", goalstates.DaemonRecoveryUnit); err != nil {
		out, inspectErr := executil.OutputCmd(ctx, log, "systemctl", "show", goalstates.DaemonRecoveryUnit, "--property=LoadState", "--value")
		if inspectErr != nil || strings.TrimSpace(out) != "not-found" {
			return fmt.Errorf("stop daemon recovery: %w", err)
		}
	}

	return nil
}

func durableReset(ctx context.Context, store *installstate.Store, inner phases.Task, paths []string, syncfs func(int) error) error {
	var handles []*os.File

	defer func() {
		for _, f := range handles {
			_ = f.Close() //nolint:errcheck // Read-only directory descriptor; teardown sync errors are returned below.
		}
	}()

	for _, path := range paths {
		for {
			if _, err := os.Stat(path); err == nil {
				break
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}

			parent := filepath.Dir(path)
			if parent == path {
				return fmt.Errorf("no filesystem ancestor for %s", path)
			}

			path = parent
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}

		handles = append(handles, f)
	}

	if err := inner.Do(ctx); err != nil {
		return err
	}

	if err := fsutil.SyncOpenFilesystems(handles, syncfs); err != nil {
		return err
	}

	return store.Remove()
}

// readAppliedMode is the isolation mode the applied config records. It is a
// variable so tests can stand in for the host.
var readAppliedMode = func() (isolation.Mode, error) {
	applied, err := appliedconfig.Read()
	if err != nil {
		return "", err
	}

	return applied.Mode, nil
}

// hostIsNone and hostIsContainer report whether the host has traces of a
// host-native node or of a node in an OCI container: the node's own units and
// directory. They are what detectResetMode falls back to when the applied
// config cannot be read. PROVISIONAL: container mode is experimental.
var (
	hostIsNone      = noneops.Present
	hostIsContainer = containerops.Present
)

// resetUnknown is not an isolation mode. detectResetMode returns it when the
// applied config exists but cannot be read: the host could be in any mode, and
// the traces of one mode are no evidence that it is the one, because an nspawn
// host can have a stray none directory. Every mode's node teardown then runs.
const resetUnknown isolation.Mode = "unknown"

// detectResetMode decides which teardown a reset runs. When the applied config
// is readable its mode is trusted, so an nspawn host that has stray none or
// container leftovers still gets the nspawn teardown. When it is gone, as when
// a reset is retried, the node's traces decide. When it exists but cannot be
// read, which reset must survive on a damaged host, the answer is resetUnknown
// and every mode is torn down; the teardowns skip what is not there. It is
// called once per reset.
func detectResetMode(log *slog.Logger) isolation.Mode {
	mode, err := readAppliedMode()
	if err == nil {
		return mode
	}

	if !errors.Is(err, appliedconfig.ErrNotFound) {
		log.Warn("applied config is unreadable; tearing the host down as every isolation mode", "error", err)

		return resetUnknown
	}

	switch {
	case hostIsNone():
		mode = isolation.ModeNone
	case hostIsContainer():
		mode = isolation.ModeContainer
	default:
		mode = isolation.ModeNSpawn
	}

	return mode
}

// resetResources builds the reset sequence. The order is the same for every
// mode: daemon unit, stop, WireGuard keys, config, root, bpffs, network and
// routes, bootstrap unit, artifacts, systemd reload. Only the node steps differ.
// For a host-native node the stop, config and root steps are the none
// teardown's, and there is no per-machine bpffs mount to clean.
func resetResources(log *slog.Logger) phases.Task {
	return resetResourcesFor(log, detectResetMode(log))
}

func resetResourcesFor(log *slog.Logger, mode isolation.Mode) phases.Task {
	return phases.Serial(log, resetNodeResourcesFor(log, mode), RemoveReprovisionHandoff(log))
}

// resetNodeResources is the reset without the cleanup of a reprovision
// handoff. The handoff's own reset uses it, since the handoff runs from the
// directory the cleanup removes.
func resetNodeResources(log *slog.Logger) phases.Task {
	return resetNodeResourcesFor(log, detectResetMode(log))
}

func resetNodeResourcesFor(log *slog.Logger, mode isolation.Mode) phases.Task {
	switch mode {
	case isolation.ModeNone:
		return resetResourcesHostNode(log, noneTeardown(log))
	case isolation.ModeContainer:
		return resetResourcesHostNode(log, containerTeardown(log))
	case resetUnknown:
		return resetResourcesEveryMode(log)
	default:
		return resetResourcesNSpawn(log)
	}
}

// resetResourcesHostNode is the reset for a node that does not run in an nspawn
// machine (none or container): stop it, remove its configuration and the node
// itself, then the same host-global cleanup. There is no per-machine bpffs
// mount to clean: the node uses the host's.
func resetResourcesHostNode(log *slog.Logger, td hostNodeTeardown) phases.Task {
	return phases.Serial(log,
		RemoveDaemonUnit(log),
		td.Stop(),
		reset.RemoveWireGuardKeys(log),
		td.RemoveConfig(),
		td.Destroy(),
		reset.CleanupNetwork(log),
		// Before the artifacts, so a failure stops the reset while the host is
		// still recognizably installed.
		RemoveFirstBootBootstrapUnit(log),
		RemoveAgentArtifacts(log),
		reset.ReloadSystemd(log),
	)
}

// resetResourcesEveryMode is the reset of a host whose mode is not known: the
// same order as the single-mode resets, with every mode's node tasks in the
// position of the one they replace.
func resetResourcesEveryMode(log *slog.Logger) phases.Task {
	noneNode, containerNode := noneTeardown(log), containerTeardown(log)

	return phases.Serial(log,
		RemoveDaemonUnit(log),
		phases.Parallel(log,
			reset.StopMachine(log, goalstates.NSpawnMachineKube1),
			reset.StopMachine(log, goalstates.NSpawnMachineKube2),
		),
		noneNode.Stop(),
		containerNode.Stop(),
		reset.RemoveWireGuardKeys(log),
		phases.Parallel(log,
			reset.RemoveNSpawnConfig(log, goalstates.NSpawnMachineKube1),
			reset.RemoveNSpawnConfig(log, goalstates.NSpawnMachineKube2),
		),
		noneNode.RemoveConfig(),
		containerNode.RemoveConfig(),
		phases.Parallel(log,
			reset.RemoveMachine(log, goalstates.NSpawnMachineKube1),
			reset.RemoveMachine(log, goalstates.NSpawnMachineKube2),
		),
		noneNode.Destroy(),
		containerNode.Destroy(),
		phases.Parallel(log,
			reset.RemoveBPFFSMount(log, goalstates.NSpawnMachineKube1),
			reset.RemoveBPFFSMount(log, goalstates.NSpawnMachineKube2),
		),
		reset.CleanupNetwork(log),
		RemoveFirstBootBootstrapUnit(log),
		RemoveAgentArtifacts(log),
		reset.ReloadSystemd(log),
	)
}

func resetResourcesNSpawn(log *slog.Logger) phases.Task {
	return phases.Serial(log,
		RemoveDaemonUnit(log),
		phases.Parallel(log,
			reset.StopMachine(log, goalstates.NSpawnMachineKube1),
			reset.StopMachine(log, goalstates.NSpawnMachineKube2),
		),
		reset.RemoveWireGuardKeys(log),
		phases.Parallel(log,
			reset.RemoveNSpawnConfig(log, goalstates.NSpawnMachineKube1),
			reset.RemoveNSpawnConfig(log, goalstates.NSpawnMachineKube2),
		),
		phases.Parallel(log,
			reset.RemoveMachine(log, goalstates.NSpawnMachineKube1),
			reset.RemoveMachine(log, goalstates.NSpawnMachineKube2),
		),
		phases.Parallel(log,
			reset.RemoveBPFFSMount(log, goalstates.NSpawnMachineKube1),
			reset.RemoveBPFFSMount(log, goalstates.NSpawnMachineKube2),
		),
		reset.CleanupNetwork(log),
		// Before the artifacts, so a failure stops the reset while the host is
		// still recognizably installed.
		RemoveFirstBootBootstrapUnit(log),
		RemoveAgentArtifacts(log),
		reset.ReloadSystemd(log),
	)
}

func releaseInstallationLock(log *slog.Logger, lock *installstate.Lock) {
	if err := lock.Release(); err != nil {
		log.Error("release installation lock", "error", err)
	}
}
