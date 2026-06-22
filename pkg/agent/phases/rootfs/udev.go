// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type disableUdev struct {
	goalState *goalstates.RootFS
}

// DisableUdev returns a task that masks systemd-udevd inside the container.
//
// The container sees the host-managed /dev tree and read-only /run/udev
// metadata. Running a second udev daemon inside the rootfs cannot manage those
// host devices correctly and can race with the host's device manager, so the
// container-side udev units are masked before boot.
func DisableUdev(goalState *goalstates.RootFS) phases.Task {
	return &disableUdev{goalState: goalState}
}

func (d *disableUdev) Name() string { return "disable-udev" }

func (d *disableUdev) Do(_ context.Context) error {
	for _, unit := range []string{
		"systemd-udevd.service",
		"systemd-udevd-control.socket",
		"systemd-udevd-kernel.socket",
	} {
		if err := d.maskUnit(unit); err != nil {
			return err
		}
	}

	return nil
}

func (d *disableUdev) maskUnit(unit string) error {
	maskedUnit := filepath.Join(d.goalState.MachineDir, "etc/systemd/system", unit)
	if err := os.MkdirAll(filepath.Dir(maskedUnit), 0o755); err != nil {
		return fmt.Errorf("create directory for masked unit %s: %w", unit, err)
	}

	// Remove any existing file/symlink before creating the mask.
	os.Remove(maskedUnit) //nolint:errcheck // best-effort removal

	if err := os.Symlink("/dev/null", maskedUnit); err != nil {
		return fmt.Errorf("mask %s: %w", unit, err)
	}

	return nil
}
