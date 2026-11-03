// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package containerops

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"time"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
)

// GPU support for the container node mirrors the nspawn flow. The grants put the
// host's NVIDIA devices and driver libraries into the container (devices and
// cgroup rules, read-only binds at /run/host-nvidia*), and the post-start hook
// then does inside it what nspawn-lifecycle does inside the machine: link the
// libraries, build the driver root, generate the CDI spec and select CDI mode,
// restarting containerd and kubelet around it. The image is the same NVIDIA
// rootfs the nspawn mode uses, which ships the container toolkit.
// PROVISIONAL: not run against a real GPU, runc or Azure Container Linux node.

// NVIDIAHooks are the seams of the GPU post-start flow. Zero values do the real
// work.
type NVIDIAHooks struct {
	// Resolve discovers NVIDIA state on the host. Nil is goalstates.ResolveNvidiaHost.
	Resolve func(arch string) (goalstates.NvidiaHost, error)
	// Execute runs a task. Nil is phases.ExecuteTask.
	Execute func(ctx context.Context, log *slog.Logger, task phases.Task) error
	// PollInterval and ReadyTimeout bound the wait for systemd in the container.
	PollInterval, ReadyTimeout time.Duration
}

func (h NVIDIAHooks) resolve() func(string) (goalstates.NvidiaHost, error) {
	if h.Resolve != nil {
		return h.Resolve
	}

	return goalstates.ResolveNvidiaHost
}

func (h NVIDIAHooks) execute() func(context.Context, *slog.Logger, phases.Task) error {
	if h.Execute != nil {
		return h.Execute
	}

	return phases.ExecuteTask
}

// isNvidiaPurpose reports whether a grant belongs to the NVIDIA set.
func isNvidiaPurpose(p isolation.GrantPurpose) bool {
	switch p {
	case isolation.PurposeNvidiaDevice, isolation.PurposeNvidiaLibDir, isolation.PurposeNvidiaI386LibDir, isolation.PurposeNvidiaHelperBin:
		return true
	default:
		return false
	}
}

// RefreshNVIDIAGrants returns a container.Backend.RefreshGrants that rediscovers
// the NVIDIA devices and driver libraries at pre-start, so a host reboot or
// driver change since Prepare is followed the way nspawn's pre-start hook
// follows it. Grants of every other purpose are kept as recorded and in place.
// A node recorded without NVIDIA grants is left alone: adding a GPU to a node
// provisioned with the CPU rootfs needs a new rootfs, not a refresh.
func RefreshNVIDIAGrants(h NVIDIAHooks) func(context.Context, []isolation.Grant) ([]isolation.Grant, error) {
	return func(_ context.Context, recorded []isolation.Grant) ([]isolation.Grant, error) {
		has := false

		for _, g := range recorded {
			has = has || isNvidiaPurpose(g.Purpose)
		}

		if !has {
			return recorded, nil
		}

		host, err := h.resolve()(runtime.GOARCH)
		if err != nil {
			return nil, fmt.Errorf("resolve NVIDIA host state: %w", err)
		}

		if !goalstates.NVIDIAStateAvailable(host) {
			return nil, fmt.Errorf("%w: the node was provisioned with GPUs but the host no longer reports complete NVIDIA state", goalstates.ErrNVIDIAStateUnavailable)
		}

		var out []isolation.Grant

		placed := false

		for _, g := range recorded {
			if !isNvidiaPurpose(g.Purpose) {
				out = append(out, g)

				continue
			}

			if !placed {
				out = append(out, host.Grants()...)
				placed = true
			}
		}

		return out, nil
	}
}

// ReconcileNVIDIA is the container node's post-start GPU flow. On a host with no
// NVIDIA devices it does nothing. Otherwise it waits for systemd in the
// container, then stops containerd and kubelet, rebuilds the NVIDIA state
// inside, and starts them again, as nspawn-lifecycle does for an nspawn machine.
func ReconcileNVIDIA(ctx context.Context, log *slog.Logger, b *container.Backend, slot string, h NVIDIAHooks) error {
	root, err := b.NodeRoot(slot)
	if err != nil {
		return err
	}

	nvidia, err := h.resolve()(runtime.GOARCH)
	if err != nil {
		return fmt.Errorf("resolve NVIDIA host state: %w", err)
	}

	if len(nvidia.GPUDevicePaths) == 0 {
		log.Info("no NVIDIA devices discovered; skipping post-start rewiring", "slot", slot)

		return nil
	}

	if !goalstates.NVIDIAStateAvailable(nvidia) {
		return fmt.Errorf("%w for slot %s", goalstates.ErrNVIDIAStateUnavailable, slot)
	}

	nvidia.Required = true

	if err := waitSystemd(ctx, b, root, h.PollInterval, h.ReadyTimeout); err != nil {
		return err
	}

	nodeStart := &goalstates.NodeStart{
		MachineName:  slot,
		RootOverride: root,
		InContainer:  true,
		ExecOverride: func(ctx context.Context, _ *slog.Logger, args ...string) (string, error) {
			res, err := b.Exec(ctx, root, isolation.ExecSpec{Args: args})

			return res.Stdout, err
		},
		Containerd: goalstates.ResolveContainerd(goalstates.ContainerdOptions{NvidiaRequired: true}),
		Nvidia:     nvidia,
	}

	if err := h.execute()(ctx, log, nodestart.ReconcileNVIDIA(log, nodeStart)); err != nil {
		return fmt.Errorf("reconcile NVIDIA state for %s: %w", slot, err)
	}

	return nil
}
