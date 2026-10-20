// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nodestart

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

var reconcileTestTime = time.Date(2026, 10, 20, 15, 37, 0, 0, time.UTC)

// reconcileHarness runs ReconcileNVIDIA against a temporary machine rootfs
// and records the in-machine commands and tasks it runs, in order.
type reconcileHarness struct {
	machineDir string
	operations []string
	setupErr   error
}

func newReconcileHarness(t *testing.T) *reconcileHarness {
	t.Helper()

	return &reconcileHarness{machineDir: t.TempDir()}
}

func (h *reconcileHarness) task(assessment goalstates.NVIDIAAssessment, required bool) *reconcileNVIDIA {
	nvidia := assessment.Host
	nvidia.Required = assessment.Usable
	nvidia.ContainerLibDir = "/usr/lib/x86_64-linux-gnu"

	state := &goalstates.NodeStart{
		MachineName: "kube1",
		MachineDir:  h.machineDir,
		Containerd:  goalstates.ResolveContainerd(goalstates.ContainerdOptions{NvidiaRequired: true}),
		Nvidia:      nvidia,
	}

	task := ReconcileNVIDIA(slog.New(slog.DiscardHandler), state, NVIDIAReconcileOptions{
		Assessment: assessment,
		Required:   required,
	}).(*reconcileNVIDIA)

	task.now = func() time.Time { return reconcileTestTime }
	task.run = func(_ context.Context, _ *slog.Logger, machine string, args ...string) (string, error) {
		if machine != "kube1" {
			return "", errors.New("unexpected machine " + machine)
		}

		h.operations = append(h.operations, strings.Join(args, " "))

		return "", nil
	}
	task.execute = func(_ context.Context, _ *slog.Logger, task phases.Task) error {
		h.operations = append(h.operations, task.Name())

		if task.Name() == "setup-nvidia" {
			return h.setupErr
		}

		return nil
	}

	return task
}

func (h *reconcileHarness) dropInPath() string {
	return filepath.Join(h.machineDir, goalstates.NvidiaRuntimeDropInPath)
}

func (h *reconcileHarness) cdiSpecPath() string {
	return filepath.Join(h.machineDir, goalstates.CDISpecFile)
}

// writeEarlierGPUConfig leaves the files an earlier successful start writes.
func (h *reconcileHarness) writeEarlierGPUConfig(t *testing.T) {
	t.Helper()

	for _, path := range []string{h.dropInPath(), h.cdiSpecPath()} {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte("earlier"), 0o644))
	}
}

func (h *reconcileHarness) status(t *testing.T) *goalstates.GPUStatus {
	t.Helper()

	status, err := goalstates.ReadGPUStatus(h.machineDir)
	require.NoError(t, err)

	return status
}

func usableAssessment() goalstates.NVIDIAAssessment {
	return goalstates.AssessNVIDIAHost(goalstates.NvidiaHost{
		GPUDevicePaths: []string{"/dev/nvidia0"},
		LibMappings:    []goalstates.NvidiaLibMapping{{HostPath: "/usr/lib/x86_64-linux-gnu/libcuda.so.1"}},
		DriverVersion:  "580.126.09",
	}, nil)
}

func incompleteAssessment() goalstates.NVIDIAAssessment {
	return goalstates.AssessNVIDIAHost(goalstates.NvidiaHost{GPUDevicePaths: []string{"/dev/nvidia0"}}, nil)
}

func TestReconcileNVIDIAOrdersServiceRestartAroundSetup(t *testing.T) {
	t.Parallel()

	h := newReconcileHarness(t)
	require.NoError(t, h.task(usableAssessment(), false).Do(context.Background()))

	require.Equal(t, []string{
		"systemctl stop kubelet.service",
		"systemctl stop containerd.service",
		"setup-nvidia",
		"start-containerd",
		"import-container-images",
		"start-kubelet",
	}, h.operations)

	require.FileExists(t, h.dropInPath(), "a working GPU enables the NVIDIA runtime before containerd starts")
	require.Equal(t, &goalstates.GPUStatus{
		Ready:   true,
		Reason:  goalstates.GPUReasonSetupSucceeded,
		Message: "NVIDIA GPU setup succeeded",
		Time:    reconcileTestTime,
	}, h.status(t))
}

func TestReconcileNVIDIASetupFailureStartsNodeWithoutGPUs(t *testing.T) {
	t.Parallel()

	h := newReconcileHarness(t)
	h.writeEarlierGPUConfig(t)
	h.setupErr = errors.New("nvidia-ctk cdi generate in kube1: exit status 1")

	require.NoError(t, h.task(usableAssessment(), false).Do(context.Background()))

	require.Equal(t, []string{
		"systemctl stop kubelet.service",
		"systemctl stop containerd.service",
		"setup-nvidia",
		"rm -rf /run/nvidia/driver",
		"find /usr/lib/x86_64-linux-gnu -maxdepth 1 -lname /run/host-nvidia/* -delete",
		"ldconfig",
		"start-containerd",
		"import-container-images",
		"start-kubelet",
	}, h.operations)

	require.NoFileExists(t, h.dropInPath(), "containerd must start without the NVIDIA runtime")
	require.NoFileExists(t, h.cdiSpecPath(), "a stale CDI spec must not outlive a failed setup")

	status := h.status(t)
	require.False(t, status.Ready)
	require.Equal(t, goalstates.GPUReasonSetupFailed, status.Reason)
	require.Equal(t, "setup-nvidia: nvidia-ctk cdi generate in kube1: exit status 1", status.Message)
}

func TestReconcileNVIDIASetupFailureFailsWhenRequired(t *testing.T) {
	t.Parallel()

	h := newReconcileHarness(t)
	h.writeEarlierGPUConfig(t)
	h.setupErr = errors.New("nvidia-ctk cdi generate in kube1: exit status 1")

	err := h.task(usableAssessment(), true).Do(context.Background())
	require.ErrorIs(t, err, goalstates.ErrRequiredHardwareUnavailable)
	require.ErrorContains(t, err, "setup-nvidia: nvidia-ctk cdi generate")

	require.Equal(t, []string{
		"systemctl stop kubelet.service",
		"systemctl stop containerd.service",
		"setup-nvidia",
	}, h.operations, "a node that requires the GPUs must not start without them")

	require.Equal(t, goalstates.GPUReasonSetupFailed, h.status(t).Reason, "the failure is still recorded for the condition")
}

func TestReconcileNVIDIAUnusableGPUs(t *testing.T) {
	t.Parallel()

	t.Run("first start leaves containerd alone", func(t *testing.T) {
		t.Parallel()

		h := newReconcileHarness(t)
		require.NoError(t, h.task(incompleteAssessment(), false).Do(context.Background()))

		require.Empty(t, h.operations, "nothing to remove, so nothing to restart")
		require.Equal(t, goalstates.GPUReasonDriverStateIncomplete, h.status(t).Reason)
	})

	t.Run("earlier GPU config is removed and containerd restarted", func(t *testing.T) {
		t.Parallel()

		h := newReconcileHarness(t)
		h.writeEarlierGPUConfig(t)

		require.NoError(t, h.task(incompleteAssessment(), false).Do(context.Background()))

		require.Equal(t, []string{"systemctl try-restart containerd.service"}, h.operations)
		require.NoFileExists(t, h.dropInPath())
		require.NoFileExists(t, h.cdiSpecPath())
	})

	t.Run("discovery error is recorded", func(t *testing.T) {
		t.Parallel()

		h := newReconcileHarness(t)
		assessment := goalstates.AssessNVIDIAHost(goalstates.NvidiaHost{}, errors.New("nvidia-modprobe failed"))

		require.NoError(t, h.task(assessment, false).Do(context.Background()))
		require.Equal(t, &goalstates.GPUStatus{
			Reason:  goalstates.GPUReasonDiscoveryFailed,
			Message: "nvidia-modprobe failed",
			Time:    reconcileTestTime,
		}, h.status(t))
	})

	t.Run("required GPUs fail the start", func(t *testing.T) {
		t.Parallel()

		h := newReconcileHarness(t)
		h.writeEarlierGPUConfig(t)

		err := h.task(incompleteAssessment(), true).Do(context.Background())
		require.ErrorIs(t, err, goalstates.ErrRequiredHardwareUnavailable)
		require.Empty(t, h.operations)
		require.Equal(t, goalstates.GPUReasonDriverStateIncomplete, h.status(t).Reason)
	})
}

func TestReconcileNVIDIAWithoutGPUs(t *testing.T) {
	t.Parallel()

	t.Run("forgets an earlier start's GPU status", func(t *testing.T) {
		t.Parallel()

		h := newReconcileHarness(t)
		require.NoError(t, goalstates.WriteGPUStatus(h.machineDir, goalstates.GPUStatus{Ready: true}))

		require.NoError(t, h.task(goalstates.NVIDIAAssessment{}, false).Do(context.Background()))
		require.Empty(t, h.operations)
		require.Nil(t, h.status(t))
	})

	t.Run("required GPUs that are missing fail the start", func(t *testing.T) {
		t.Parallel()

		h := newReconcileHarness(t)

		err := h.task(goalstates.NVIDIAAssessment{}, true).Do(context.Background())
		require.ErrorIs(t, err, goalstates.ErrRequiredHardwareUnavailable)
		require.Equal(t, goalstates.GPUReasonNotFound, h.status(t).Reason)
	})
}

func TestNVIDIACDISpecFiles(t *testing.T) {
	t.Parallel()

	require.Equal(t, []string{
		"/etc/cdi/nvidia.yaml",
		"/etc/cdi/nvidia.coherent.yaml",
		"/etc/cdi/nvidia.noncoherent.yaml",
	}, nvidiaCDISpecFiles())
}
