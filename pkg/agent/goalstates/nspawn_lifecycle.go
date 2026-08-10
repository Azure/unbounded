// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/Azure/unbounded/pkg/agent/config"
)

const NSpawnLifecycleStateVersion = 1

// ErrNVIDIAStateUnavailable indicates that a provisioned GPU machine cannot
// currently resolve the complete host state needed for safe startup.
var ErrNVIDIAStateUnavailable = errors.New("required NVIDIA host state is unavailable")

// NSpawnLifecycleState is the durable handoff between nspawn pre-start
// discovery and post-start NVIDIA setup. NVIDIA contains both the provisioned
// capability and the exact host state used to render the current nspawn mounts.
type NSpawnLifecycleState struct {
	Version           int               `json:"version"`
	MachineName       string            `json:"machineName"`
	NVIDIA            NvidiaHost        `json:"nvidia"`
	NSpawnConfigInput NSpawnConfigInput `json:"nspawnConfigInput"`
}

// NSpawnConfigInput is the durable configuration needed to rediscover host
// mounts and devices before the applied config is persisted.
type NSpawnConfigInput struct {
	AdditionalHostDevices []string                     `json:"additionalHostDevices,omitempty"`
	AdditionalHostMounts  []config.AdditionalHostMount `json:"additionalHostMounts,omitempty"`
}

func (i NSpawnConfigInput) AgentConfig() *config.AgentConfig {
	return &config.AgentConfig{
		AdditionalHostDevices: i.AdditionalHostDevices,
		AdditionalHostMounts:  i.AdditionalHostMounts,
	}
}

func NSpawnLifecycleStatePath(machineName string) string {
	return filepath.Join(AgentConfigDir, machineName+"-nspawn-lifecycle.json")
}

// LoadNSpawnLifecycleState loads and validates a persisted lifecycle handoff.
func LoadNSpawnLifecycleState(path, machineName string) (*NSpawnLifecycleState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read nspawn lifecycle state %s: %w", path, err)
	}

	var state NSpawnLifecycleState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("decode nspawn lifecycle state %s: %w", path, err)
	}

	if err := state.Validate(machineName); err != nil {
		return nil, fmt.Errorf("validate nspawn lifecycle state %s: %w", path, err)
	}

	return &state, nil
}

// LoadOrInferNVIDIACapability loads an existing lifecycle state. Only when the
// state file is absent does it infer a legacy machine's capability from the
// managed NVIDIA containerd drop-in. Corrupt or mismatched state is never
// replaced by inference.
func LoadOrInferNVIDIACapability(statePath, legacyNVIDIADropInPath, machineName string) (bool, error) {
	state, err := LoadNSpawnLifecycleState(statePath, machineName)
	if err == nil {
		return state.NVIDIA.Required, nil
	}

	if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}

	if _, err := os.Stat(legacyNVIDIADropInPath); err == nil {
		return true, nil
	} else if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else {
		return false, fmt.Errorf("inspect legacy NVIDIA capability %s: %w", legacyNVIDIADropInPath, err)
	}
}

func (s *NSpawnLifecycleState) Validate(machineName string) error {
	if s.Version != NSpawnLifecycleStateVersion {
		return fmt.Errorf("unsupported nspawn lifecycle state version %d", s.Version)
	}

	if s.MachineName != machineName {
		return fmt.Errorf("nspawn lifecycle state is for machine %q, not %q", s.MachineName, machineName)
	}

	if s.NVIDIA.Required && !NVIDIAStateAvailable(s.NVIDIA) {
		return fmt.Errorf("NVIDIA is required but persisted host state is incomplete")
	}

	if !s.NVIDIA.Required && !reflect.DeepEqual(s.NVIDIA, NvidiaHost{}) {
		return fmt.Errorf("CPU-provisioned machine contains NVIDIA lifecycle state")
	}

	return nil
}

// NVIDIAStateAvailable reports whether discovery produced all host state needed
// to render mounts and perform in-machine setup.
func NVIDIAStateAvailable(nvidia NvidiaHost) bool {
	return len(nvidia.GPUDevicePaths) > 0 && len(nvidia.LibMappings) > 0 && nvidia.DriverVersion != ""
}
