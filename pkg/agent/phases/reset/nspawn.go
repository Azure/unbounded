// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package reset

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation/nspawn"
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
	return nspawn.RemoveConfig(t.log, nspawnConfigPaths(t.machineName))
}

func nspawnConfigPaths(machineName string) nspawn.ConfigPaths {
	return nspawn.ConfigPaths{
		NSpawnFile:             fmt.Sprintf("%s/%s.nspawn", goalstates.SystemdNSpawnDir, machineName),
		OverrideDir:            fmt.Sprintf("%s/systemd-nspawn@%s.service.d", goalstates.SystemdSystemDir, machineName),
		ConfigRegenerationUnit: fmt.Sprintf("%s/%s", goalstates.SystemdSystemDir, goalstates.ConfigRegenerationUnit(machineName)),
		BPFFSMountPath:         goalstates.BPFFSMountPath(machineName),
	}
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
	return nspawn.RemoveBPFFSMount(ctx, t.log, goalstates.BPFFSMountPath(t.machineName))
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
