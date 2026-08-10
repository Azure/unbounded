// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodestart

import (
	"context"
	"fmt"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type ensureNSpawnLifecycleUnits struct {
	goalState *goalstates.NodeStart
}

// EnsureNSpawnLifecycleUnits updates only the in-machine systemd units needed
// for lifecycle readiness. It writes through the host-visible rootfs and does
// not reload or restart the currently running in-machine services.
func EnsureNSpawnLifecycleUnits(goalState *goalstates.NodeStart) phases.Task {
	return &ensureNSpawnLifecycleUnits{goalState: goalState}
}

func (e *ensureNSpawnLifecycleUnits) Name() string { return "ensure-nspawn-lifecycle-units" }

func (e *ensureNSpawnLifecycleUnits) Do(_ context.Context) error {
	containerd := &configureContainerd{goalState: e.goalState}
	if err := containerd.ensureContainerdServiceUnit(); err != nil {
		return fmt.Errorf("ensure containerd lifecycle unit: %w", err)
	}

	if err := containerd.ensureNVIDIAReadyServiceUnit(); err != nil {
		return fmt.Errorf("ensure NVIDIA ready service unit: %w", err)
	}

	if err := containerd.ensureGPUDropInConfigs(); err != nil {
		return fmt.Errorf("ensure NVIDIA containerd runtime config: %w", err)
	}

	kubelet := &configureKubelet{goalState: e.goalState}
	if err := kubelet.ensureKubeletServiceUnit(); err != nil {
		return fmt.Errorf("ensure kubelet lifecycle unit: %w", err)
	}

	return nil
}
