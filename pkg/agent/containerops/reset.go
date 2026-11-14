// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package containerops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/hostnode"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
	"github.com/Azure/unbounded/pkg/agent/noneops"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// Teardown removes a container node. Its tasks slot into the same positions in
// the reset order as the nspawn machine tasks they replace: stop, then config,
// then the node itself, with the host-global cleanup the caller owns in between
// and after.
type Teardown struct {
	Log *slog.Logger
	// Layout, SystemdDir, RuntimePath and Run are as in Params.
	Layout      container.Layout
	SystemdDir  string
	RuntimePath string
	Run         container.Runner

	// Root prefixes every host path the teardown removes outside the layout and
	// systemd directory. Empty is the real host; tests use a temporary tree.
	Root string
	// MountInfo is the mount table. Empty means /proc/self/mountinfo.
	MountInfo string
	// GoneTimeout and PollInterval are as in Params.
	GoneTimeout  time.Duration
	PollInterval time.Duration
}

func (t *Teardown) logger() *slog.Logger { return hostnode.Logger(t.Log) }

func (t *Teardown) path(p string) string { return filepath.Join(t.Root, p) }

func (t *Teardown) unitDir() string { return hostnode.OrDefault(t.SystemdDir, container.SystemdDir) }

func (t *Teardown) node(slot string) *Node {
	return New(Params{
		Log: t.logger(), Slot: slot,
		Overrides: Overrides{
			Layout: t.Layout, SystemdDir: t.SystemdDir, RuntimePath: t.RuntimePath, Run: t.Run, MountInfoPath: t.MountInfo,
			GoneTimeout: t.GoneTimeout, PollInterval: t.PollInterval,
		},
		Hooks: hostnode.Hooks{HostRoot: t.Root},
	})
}

func (t *Teardown) runtimeDir() string {
	rt := t.RuntimePath
	if rt == "" {
		rt = container.DefaultRuntimePath
	}

	return filepath.Dir(rt)
}

// slots are both blue/green slots; reset removes whichever exist.
var slots = []string{goalstates.NSpawnMachineKube1, goalstates.NSpawnMachineKube2}

// Present reports whether the host has any trace of a container node: the
// layout directory or an agent-owned unit. Reset uses it to choose the
// container teardown even after the applied config is gone.
func (t *Teardown) Present() bool {
	if _, err := os.Lstat(t.Layout.RootfsDir("")); err == nil {
		return true
	}

	for _, slot := range slots {
		if _, err := os.Lstat(filepath.Join(t.unitDir(), container.UnitName(slot))); err == nil {
			return true
		}
	}

	return false
}

// Present is the package level form of Teardown.Present for the real host.
func Present() bool { return (&Teardown{}).Present() }

// Stop returns the task that stops both slots' containers. A slot that is not
// there is skipped; the node stack inside is stopped first when the container
// runs.
func (t *Teardown) Stop() phases.Task {
	return hostnode.Func("stop-container-node", func(ctx context.Context) error {
		var errs []error

		for _, slot := range slots {
			if !t.slotPresent(slot) {
				continue
			}

			if err := t.node(slot).Stop(ctx); err != nil {
				errs = append(errs, fmt.Errorf("stop %s: %w", slot, err))
			}
		}

		return errors.Join(errs...)
	})
}

func (t *Teardown) slotPresent(slot string) bool { return t.node(slot).Backend().SlotExists(slot) }

// RemoveConfig returns the task that removes the configuration the agent wrote
// outside the slots: the host kernel modules list (written by bootstrap).
func (t *Teardown) RemoveConfig() phases.Task {
	return hostnode.Func("remove-container-config", func(context.Context) error {
		if err := os.Remove(t.path(noneops.ModulesLoadPath)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}

		return nil
	})
}

// Destroy returns the task that removes the nodes: for each slot the unit,
// bundle and rootfs through the backend, then the shared data the node left on
// the host (pod mounts, kubelet state and CNI binaries), the layout directory
// and the host runtime. CNI configuration, IPAM state and containerd data are
// private to each slot and disappear with its rootfs; their host copies are
// not owned by this teardown.
//
// The shared data goes only once every slot is destroyed, which the backend
// reports only after the runtime confirmed its container gone: a container
// still running holds those paths, and its pod volumes are mounted below
// /var/lib/kubelet. The mounts there are unmounted after the slots are gone,
// so none of them is recreated behind the unmount, and the kubelet directory
// is kept if one stays, since RemoveAll would delete a volume's data through
// it.
func (t *Teardown) Destroy() phases.Task {
	return hostnode.Func("remove-container-node", func(ctx context.Context) error {
		var errs []error

		for _, slot := range slots {
			if !t.slotPresent(slot) {
				continue
			}

			if err := t.node(slot).Destroy(ctx); err != nil {
				errs = append(errs, fmt.Errorf("destroy %s: %w", slot, err))
			}
		}

		if len(errs) > 0 {
			t.logger().Warn("keeping the shared kubelet, CNI and image directories while a container slot remains")

			return errors.Join(errs...)
		}

		keepKubelet := false

		if err := t.unmountShared(ctx); err != nil {
			errs = append(errs, err)
			keepKubelet = true
		}

		for _, d := range []string{hostnode.KubeletDir, goalstates.CNIBinDir, container.ImageArchiveDir} {
			if keepKubelet && d == hostnode.KubeletDir {
				t.logger().Warn("keeping the kubelet directory because mounts below it could not be unmounted", "dir", d)

				continue
			}

			if err := os.RemoveAll(t.path(d)); err != nil {
				errs = append(errs, err)
			}
		}

		// Last, and only when everything else went, so a failed destroy keeps
		// the directory that explains what is left.
		if len(errs) == 0 {
			errs = append(errs, t.removeLayout()...)
		}

		return errors.Join(errs...)
	})
}

// removeLayout removes the layout directory (slots and bundles) and the host
// runtime the agent installed. The runtime directory is removed only when it is
// empty afterwards, since it is shared by both slots and nothing else is
// expected there.
func (t *Teardown) removeLayout() []error {
	var errs []error

	if err := os.RemoveAll(t.Layout.RootfsDir("")); err != nil {
		errs = append(errs, err)
	}

	rt := t.RuntimePath
	if rt == "" {
		rt = container.DefaultRuntimePath
	}

	if err := os.Remove(rt); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}

	if err := os.Remove(t.runtimeDir()); err != nil && !errors.Is(err, os.ErrNotExist) && !noneops.IsNotEmpty(err) {
		errs = append(errs, err)
	}

	return errs
}

func (t *Teardown) unmountShared(ctx context.Context) error {
	return hostnode.UnmountBelow(ctx, t.logger(), t.Run, t.MountInfo, hostnode.KubeletDir)
}
