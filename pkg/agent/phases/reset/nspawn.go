// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package reset

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type removeNSpawnConfig struct {
	log         *slog.Logger
	machineName string
}

// RemoveNSpawnConfig returns a task that removes the nspawn configuration file
// and the systemd service override directory for the named machine.
func RemoveNSpawnConfig(log *slog.Logger, machineName string) phases.Task {
	return &removeNSpawnConfig{log: log, machineName: machineName}
}

func (t *removeNSpawnConfig) Name() string { return "remove-nspawn-config" }

func (t *removeNSpawnConfig) Do(_ context.Context) error {
	nspawnFile := fmt.Sprintf("%s/%s.nspawn", goalstates.SystemdNSpawnDir, t.machineName)
	overrideDir := fmt.Sprintf("%s/systemd-nspawn@%s.service.d", goalstates.SystemdSystemDir, t.machineName)
	configRegenerationUnit := fmt.Sprintf("%s/%s", goalstates.SystemdSystemDir, goalstates.ConfigRegenerationUnit(t.machineName))
	t.log.Info("removing nspawn configuration", "nspawn_file", nspawnFile, "override_dir", overrideDir, "config_regeneration_unit", configRegenerationUnit)

	return errors.Join(removeFileIfExists(t.log, nspawnFile), removeAllIfExists(t.log, overrideDir), removeFileIfExists(t.log, configRegenerationUnit))
}

type removeBPFFSMount struct {
	log         *slog.Logger
	machineName string
}

// RemoveBPFFSMount returns a task that unmounts and removes the private bpffs
// mount used by the named nspawn machine.
func RemoveBPFFSMount(log *slog.Logger, machineName string) phases.Task {
	return &removeBPFFSMount{log: log, machineName: machineName}
}

func (t *removeBPFFSMount) Name() string { return "remove-bpffs-mount" }

func (t *removeBPFFSMount) Do(ctx context.Context) error {
	mountPath := goalstates.BPFFSMountPath(t.machineName)
	if _, err := os.Stat(mountPath); errors.Is(err, os.ErrNotExist) {
		t.log.Info("bpffs mount path not present, nothing to remove", "path", mountPath)
		return nil
	} else if err != nil {
		return fmt.Errorf("stat bpffs mount path %s: %w", mountPath, err)
	}

	// util-linux reserves exit 32 for an existing path that is not a mount
	// point. Other failures must not authorize recursive removal.
	if err := executil.RunCmdAt(ctx, t.log, slog.LevelDebug, executil.Mountpoint(), "-q", mountPath); err == nil {
		if err := executil.RunCmd(ctx, t.log, executil.Umount(), mountPath); err != nil {
			return fmt.Errorf("unmount bpffs %s: %w", mountPath, err)
		}
	} else {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 32 {
			return fmt.Errorf("inspect bpffs mount %s: %w", mountPath, err)
		}
	}

	return removeAllIfExists(t.log, mountPath)
}

// CleanupMachine returns a composite task that removes all artifacts of an
// nspawn machine: its nspawn configuration and rootfs.
func CleanupMachine(log *slog.Logger, machineName string) phases.Task {
	return phases.Serial(
		log,
		RemoveNSpawnConfig(log, machineName),
		RemoveMachine(log, machineName),
		RemoveBPFFSMount(log, machineName),
	)
}
