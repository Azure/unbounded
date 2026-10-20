// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package goalstates

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAssessNVIDIAHost(t *testing.T) {
	t.Parallel()

	complete := completeResolvedNVIDIA()

	tests := []struct {
		name        string
		host        NvidiaHost
		err         error
		wantPresent bool
		wantUsable  bool
		wantReason  string
		wantMessage string
	}{
		{name: "no devices"},
		{
			name:        "complete",
			host:        complete,
			wantPresent: true,
			wantUsable:  true,
		},
		{
			name:        "missing libraries and version",
			host:        NvidiaHost{GPUDevicePaths: []string{"/dev/nvidia0"}},
			wantPresent: true,
			wantReason:  GPUReasonDriverStateIncomplete,
			wantMessage: "NVIDIA device files exist but the driver libraries and driver version could not be found",
		},
		{
			name: "missing version",
			host: NvidiaHost{
				GPUDevicePaths: []string{"/dev/nvidia0"},
				LibMappings:    complete.LibMappings,
			},
			wantPresent: true,
			wantReason:  GPUReasonDriverStateIncomplete,
			wantMessage: "NVIDIA device files exist but the driver version could not be found",
		},
		{
			name:        "discovery error",
			err:         errors.New("run nvidia-modprobe: exit status 1"),
			wantPresent: true,
			wantReason:  GPUReasonDiscoveryFailed,
			wantMessage: "run nvidia-modprobe: exit status 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := AssessNVIDIAHost(tt.host, tt.err)
			require.Equal(t, tt.wantPresent, got.Present)
			require.Equal(t, tt.wantUsable, got.Usable)
			require.Equal(t, tt.wantReason, got.Reason)
			require.Equal(t, tt.wantMessage, got.Message)

			if tt.wantUsable {
				require.Equal(t, tt.host, got.Host)
			} else {
				require.Empty(t, got.Host.GPUDevicePaths, "an unusable assessment must not carry host state to set up")
			}
		})
	}
}

func TestNVIDIAAssessmentRequire(t *testing.T) {
	t.Parallel()

	usable := AssessNVIDIAHost(completeResolvedNVIDIA(), nil)
	absent := AssessNVIDIAHost(NvidiaHost{}, nil)
	incomplete := AssessNVIDIAHost(NvidiaHost{GPUDevicePaths: []string{"/dev/nvidia0"}}, nil)
	discoveryFailed := AssessNVIDIAHost(NvidiaHost{}, errors.New("boom"))

	for _, assessment := range []NVIDIAAssessment{usable, absent, incomplete, discoveryFailed} {
		require.NoError(t, assessment.Require(false), "nothing is required by default")
	}

	require.NoError(t, usable.Require(true))

	err := absent.Require(true)
	require.ErrorIs(t, err, ErrRequiredHardwareUnavailable)
	require.ErrorContains(t, err, "no NVIDIA GPUs were found")

	err = incomplete.Require(true)
	require.ErrorIs(t, err, ErrRequiredHardwareUnavailable)
	require.ErrorIs(t, err, ErrNVIDIAStateUnavailable)

	err = discoveryFailed.Require(true)
	require.ErrorIs(t, err, ErrRequiredHardwareUnavailable)
	require.NotErrorIs(t, err, ErrNVIDIAStateUnavailable)
	require.ErrorContains(t, err, "boom")
}

func TestNVIDIAAssessmentUnavailableStatus(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 20, 15, 37, 0, 0, time.UTC)

	require.Equal(t, GPUStatus{
		Reason:  GPUReasonNotFound,
		Message: "no NVIDIA GPUs were found",
		Time:    now,
	}, AssessNVIDIAHost(NvidiaHost{}, nil).UnavailableStatus(now))

	require.Equal(t, GPUStatus{
		Reason:  GPUReasonDiscoveryFailed,
		Message: "boom",
		Time:    now,
	}, AssessNVIDIAHost(NvidiaHost{}, errors.New("boom")).UnavailableStatus(now))
}

func TestGPUStatusRecord(t *testing.T) {
	t.Parallel()

	machineDir := t.TempDir()

	got, err := ReadGPUStatus(machineDir)
	require.NoError(t, err)
	require.Nil(t, got, "no record is not an error")
	require.NoError(t, RemoveGPUStatus(machineDir), "removing a missing record is not an error")

	want := GPUStatus{
		Reason:  GPUReasonSetupFailed,
		Message: "nvidia-ctk cdi generate in kube1: exit status 1",
		Time:    time.Date(2026, 10, 20, 15, 37, 0, 0, time.UTC),
	}
	require.NoError(t, WriteGPUStatus(machineDir, want))
	require.FileExists(t, filepath.Join(machineDir, "var", "lib", "unbounded", "gpu-status.json"))

	got, err = ReadGPUStatus(machineDir)
	require.NoError(t, err)
	require.Equal(t, &want, got)

	require.NoError(t, RemoveGPUStatus(machineDir))

	got, err = ReadGPUStatus(machineDir)
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestReadGPUStatusRejectsCorruptRecord(t *testing.T) {
	t.Parallel()

	machineDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Dir(GPUStatusPath(machineDir)), 0o755))
	require.NoError(t, os.WriteFile(GPUStatusPath(machineDir), []byte("{"), 0o644))

	_, err := ReadGPUStatus(machineDir)
	require.ErrorContains(t, err, "decode GPU status")
}

func TestLatestGPUStatus(t *testing.T) {
	t.Parallel()

	machinesDir := t.TempDir()

	got, err := LatestGPUStatus(machinesDir)
	require.NoError(t, err)
	require.Nil(t, got)

	older := GPUStatus{Ready: true, Reason: GPUReasonSetupSucceeded, Time: time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)}
	newer := GPUStatus{Reason: GPUReasonSetupFailed, Time: time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)}

	require.NoError(t, WriteGPUStatus(filepath.Join(machinesDir, NSpawnMachineKube1), newer))
	require.NoError(t, WriteGPUStatus(filepath.Join(machinesDir, NSpawnMachineKube2), older))

	got, err = LatestGPUStatus(machinesDir)
	require.NoError(t, err)
	require.Equal(t, &newer, got, "the newest record belongs to the machine that started last")

	require.NoError(t, RemoveGPUStatus(filepath.Join(machinesDir, NSpawnMachineKube1)))

	got, err = LatestGPUStatus(machinesDir)
	require.NoError(t, err)
	require.Equal(t, &older, got)
}

func TestMachineDir(t *testing.T) {
	t.Parallel()

	require.Equal(t, "/var/lib/machines/kube1", MachineDir(NSpawnMachineKube1))
}
