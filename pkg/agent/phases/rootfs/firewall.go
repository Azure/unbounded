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

var rootfsFirewallUnits = []string{
	"iptables.service",
	"ip6tables.service",
	"nftables.service",
}

type disableImageFirewall struct {
	goalState *goalstates.RootFS
}

// DisableImageFirewall masks image-provided firewall units inside the rootfs.
//
// Azure Linux's packaged iptables.service restores default DROP policies. The
// nspawn node shares the host network namespace, so image firewall services
// modify the host firewall after the agent has flushed it and can block
// apiserver-to-kubelet traffic on port 10250. Keep firewall policy owned by
// the host bootstrap instead.
func DisableImageFirewall(goalState *goalstates.RootFS) phases.Task {
	return &disableImageFirewall{goalState: goalState}
}

func (d *disableImageFirewall) Name() string { return "disable-image-firewall" }

func (d *disableImageFirewall) Do(_ context.Context) error {
	for _, unit := range rootfsFirewallUnits {
		if err := maskRootFSUnit(d.goalState.MachineDir, unit); err != nil {
			return err
		}
	}

	return nil
}

func maskRootFSUnit(machineDir, unit string) error {
	maskedUnit := filepath.Join(machineDir, "etc/systemd/system", unit)
	if err := os.MkdirAll(filepath.Dir(maskedUnit), 0o755); err != nil {
		return fmt.Errorf("create directory for masked %s: %w", unit, err)
	}

	if err := os.Remove(maskedUnit); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove existing %s mask: %w", unit, err)
	}

	if err := os.Symlink("/dev/null", maskedUnit); err != nil {
		return fmt.Errorf("mask %s: %w", unit, err)
	}

	return nil
}
