// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package goalstates

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
)

// EnvRequiredHardware carries AgentConfig.RequiredHardware, comma-separated,
// to the nspawn lifecycle hooks. It is set in the machine's service drop-in
// because the post-start hook runs before the applied config is persisted on a
// node's first boot.
const EnvRequiredHardware = "UNBOUNDED_AGENT_REQUIRED_HARDWARE"

// ErrRequiredHardwareUnavailable indicates that hardware listed in
// AgentConfig.RequiredHardware is missing or cannot be set up.
var ErrRequiredHardwareUnavailable = errors.New("required hardware is unavailable")

// Reasons recorded in a GPUStatus. The agent daemon reports them as the reason
// of the Node's GPUReady condition.
const (
	// GPUReasonSetupSucceeded means the GPUs were set up for workloads.
	GPUReasonSetupSucceeded = "SetupSucceeded"
	// GPUReasonNotFound means GPUs are required but none were found.
	GPUReasonNotFound = "NotFound"
	// GPUReasonDiscoveryFailed means host GPU discovery returned an error.
	GPUReasonDiscoveryFailed = "DiscoveryFailed"
	// GPUReasonDriverStateIncomplete means GPU device files exist but the
	// driver libraries or driver version could not be found.
	GPUReasonDriverStateIncomplete = "DriverStateIncomplete"
	// GPUReasonSetupFailed means GPU setup inside the machine failed.
	GPUReasonSetupFailed = "SetupFailed"
)

// NVIDIAAssessment classifies NVIDIA host discovery for one machine start.
type NVIDIAAssessment struct {
	// Host is the discovered host state. It is set only when Usable is true.
	Host NvidiaHost

	// Present reports whether NVIDIA GPUs were found, whether or not they can
	// be used.
	Present bool

	// Usable reports whether discovery found everything needed to set up the
	// GPUs inside the machine.
	Usable bool

	// Reason and Message explain why GPUs that are present cannot be used.
	Reason  string
	Message string
}

// AssessNVIDIAHost classifies the result of ResolveNvidiaHost.
//
// A discovery error counts as GPUs that are present but unusable: on supported
// architectures ResolveNvidiaHost only fails while initializing NVIDIA devices
// it has already found on the PCI bus.
func AssessNVIDIAHost(host NvidiaHost, err error) NVIDIAAssessment {
	switch {
	case err != nil:
		return NVIDIAAssessment{
			Present: true,
			Reason:  GPUReasonDiscoveryFailed,
			Message: err.Error(),
		}
	case len(host.GPUDevicePaths) == 0:
		return NVIDIAAssessment{}
	case !NVIDIAStateAvailable(host):
		return NVIDIAAssessment{
			Present: true,
			Reason:  GPUReasonDriverStateIncomplete,
			Message: incompleteNVIDIAStateMessage(host),
		}
	default:
		return NVIDIAAssessment{Host: host, Present: true, Usable: true}
	}
}

func incompleteNVIDIAStateMessage(host NvidiaHost) string {
	var missing []string
	if len(host.LibMappings) == 0 {
		missing = append(missing, "driver libraries")
	}

	if host.DriverVersion == "" {
		missing = append(missing, "driver version")
	}

	return fmt.Sprintf("NVIDIA device files exist but the %s could not be found", strings.Join(missing, " and "))
}

// Require returns an error wrapping ErrRequiredHardwareUnavailable when NVIDIA
// GPUs are required but cannot be set up. Incomplete driver state also wraps
// ErrNVIDIAStateUnavailable. It returns nil when the GPUs are not required or
// are usable.
func (a NVIDIAAssessment) Require(required bool) error {
	switch {
	case !required || a.Usable:
		return nil
	case !a.Present:
		return fmt.Errorf("%w: %s: no NVIDIA GPUs were found", ErrRequiredHardwareUnavailable, config.HardwareNvidiaGPU)
	case a.Reason == GPUReasonDriverStateIncomplete:
		return fmt.Errorf("%w: %s: %s: %w", ErrRequiredHardwareUnavailable, config.HardwareNvidiaGPU, a.Message, ErrNVIDIAStateUnavailable)
	default:
		return fmt.Errorf("%w: %s: %s", ErrRequiredHardwareUnavailable, config.HardwareNvidiaGPU, a.Message)
	}
}

// UnavailableStatus returns the GPU status to record when the assessed GPUs
// will not be set up.
func (a NVIDIAAssessment) UnavailableStatus(now time.Time) GPUStatus {
	if !a.Present {
		return GPUStatus{Reason: GPUReasonNotFound, Message: "no NVIDIA GPUs were found", Time: now}
	}

	return GPUStatus{Reason: a.Reason, Message: a.Message, Time: now}
}

// GPUStatus records the outcome of GPU setup at a machine start. The nspawn
// post-start hook writes it inside the machine's root filesystem, so deleting
// the machine deletes it, and the agent daemon reports it as the Node's
// GPUReady condition.
type GPUStatus struct {
	// Ready reports whether the GPUs were set up for workloads.
	Ready bool `json:"ready"`

	// Reason is a stable CamelCase reason, one of the GPUReason constants.
	Reason string `json:"reason"`

	// Message explains Reason.
	Message string `json:"message,omitempty"`

	// Time is when the machine start checked the GPUs.
	Time time.Time `json:"time"`
}

// gpuStatusFile is the GPU status record's path inside a machine.
const gpuStatusFile = "/var/lib/unbounded/gpu-status.json"

// GPUStatusPath returns the host path of the GPU status record for the machine
// whose root filesystem is machineDir.
func GPUStatusPath(machineDir string) string {
	return filepath.Join(machineDir, gpuStatusFile)
}

// WriteGPUStatus records status for the machine whose root filesystem is
// machineDir.
func WriteGPUStatus(machineDir string, status GPUStatus) error {
	data, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("encode GPU status: %w", err)
	}

	if err := utilio.WriteFile(GPUStatusPath(machineDir), append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write GPU status: %w", err)
	}

	return nil
}

// ReadGPUStatus returns the GPU status recorded for the machine whose root
// filesystem is machineDir, or nil when none is recorded.
func ReadGPUStatus(machineDir string) (*GPUStatus, error) {
	path := GPUStatusPath(machineDir)

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("read GPU status: %w", err)
	}

	var status GPUStatus
	if err := json.Unmarshal(data, &status); err != nil {
		return nil, fmt.Errorf("decode GPU status %s: %w", path, err)
	}

	return &status, nil
}

// RemoveGPUStatus deletes the GPU status recorded for the machine whose root
// filesystem is machineDir. It is not an error when none is recorded.
func RemoveGPUStatus(machineDir string) error {
	if err := os.Remove(GPUStatusPath(machineDir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove GPU status: %w", err)
	}

	return nil
}

// LatestGPUStatus returns the most recently written GPU status across the
// nspawn machine slots under machinesDir, or nil when none is recorded. The
// post-start hook writes a record at every start of a machine with GPUs, so
// the newest record belongs to the machine that started last, which is the
// active one.
func LatestGPUStatus(machinesDir string) (*GPUStatus, error) {
	var latest *GPUStatus

	for _, machine := range []string{NSpawnMachineKube1, NSpawnMachineKube2} {
		status, err := ReadGPUStatus(filepath.Join(machinesDir, machine))
		if err != nil {
			return nil, err
		}

		if status != nil && (latest == nil || status.Time.After(latest.Time)) {
			latest = status
		}
	}

	return latest, nil
}
