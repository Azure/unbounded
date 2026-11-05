// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nodestart

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type setupNVIDIAOnHost struct {
	log       *slog.Logger
	goalState *goalstates.NodeStart
	stat      func(string) (os.FileInfo, error)
}

// NVIDIAToolkitPaths are the host paths the host-native node stack needs from
// the NVIDIA Container Toolkit: containerd's nvidia runtime handler runs the
// runtime, and the CDI spec is generated with the control tool. They are the
// paths the nspawn rootfs images ship the same tools at, unless the machine's
// configuration says the host installs them elsewhere.
func NVIDIAToolkitPaths(toolkit goalstates.NvidiaToolkit) []string {
	return []string{toolkit.CTK(), toolkit.ContainerRuntime()}
}

// SetupNVIDIAOnHost returns a task that prepares a host-native node stack
// (isolation mode none) to run GPU workloads. The kubelet and containerd see
// the host directly, so nothing is bind-mounted and no driver root is
// assembled. What the nspawn flow does inside its machine reduces to:
//
//  1. Checking that the NVIDIA Container Toolkit is installed on the host. It
//     is not shipped by the agent in this mode, so its absence is reported
//     rather than worked around.
//  2. Selecting CDI runtime mode, so environment-variable requests from the
//     device plugin resolve against the generated CDI spec.
//  3. Generating the CDI spec at /etc/cdi/nvidia.yaml from the host driver,
//     with the same hook selection as the nspawn flow (create-symlinks and
//     update-ldcache only).
//
// It must run before containerd and kubelet start, on every start, because a
// host reboot or driver change alters the devices and libraries the spec
// names. The task is a no-op when the machine has no NVIDIA GPUs.
//
// It changes two host-global files: the toolkit's runtime config and the CDI
// spec. Both are regenerated on every start. A reset removes the CDI spec, which
// the agent generated, and leaves the toolkit's runtime config, which belongs
// to the toolkit installation.
func SetupNVIDIAOnHost(log *slog.Logger, goalState *goalstates.NodeStart) phases.Task {
	return &setupNVIDIAOnHost{log: log, goalState: goalState, stat: os.Stat}
}

func (s *setupNVIDIAOnHost) Name() string { return "setup-nvidia-on-host" }

func (s *setupNVIDIAOnHost) Do(ctx context.Context) error {
	gs := s.goalState

	if !gs.Nvidia.Required {
		s.log.Info("NVIDIA was not provisioned for this machine, skipping")

		return nil
	}

	if !gs.Containerd.NvidiaRuntime.Enabled || !goalstates.NVIDIAStateAvailable(gs.Nvidia) {
		return fmt.Errorf("NVIDIA is required but resolved setup state is incomplete")
	}

	for _, p := range NVIDIAToolkitPaths(gs.NvidiaToolkit) {
		if _, err := s.stat(gs.Root().Path(p)); err != nil {
			return fmt.Errorf("NVIDIA GPUs were found but %s is not available on the host (%w); install the NVIDIA Container Toolkit, which isolation mode none requires on GPU hosts", p, err)
		}
	}

	if _, err := gs.Exec(ctx, s.log,
		gs.NvidiaToolkit.CTK(), "config",
		"--set", "nvidia-container-runtime.mode=cdi",
		"--in-place",
	); err != nil {
		return fmt.Errorf("configure NVIDIA container runtime CDI mode: %w", err)
	}

	cdiDir := gs.Root().Path(goalstates.CDISpecDir)
	if err := os.MkdirAll(cdiDir, 0o755); err != nil {
		return fmt.Errorf("create CDI spec directory %s: %w", cdiDir, err)
	}

	s.log.Info("generating CDI spec from the host NVIDIA driver", slog.String("output", goalstates.CDISpecFile))

	if _, err := gs.Exec(ctx, s.log,
		gs.NvidiaToolkit.CTK(), "cdi", "generate",
		"--driver-root", "/",
		"--dev-root", "/",
		"--output", goalstates.CDISpecFile,
		"--disable-hook", "all",
		"--enable-hook", "create-symlinks",
		"--enable-hook", "update-ldcache",
	); err != nil {
		return fmt.Errorf("nvidia-ctk cdi generate: %w", err)
	}

	return nil
}
