// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"fmt"
	"path/filepath"
	"reflect"
)

const NSpawnLifecycleStateVersion = 1

// NSpawnLifecycleState is the durable handoff between nspawn pre-start
// discovery and post-start NVIDIA setup. NVIDIARequired records the capability
// selected when the machine was provisioned; NVIDIA is the exact host state
// used to render the current nspawn mounts.
type NSpawnLifecycleState struct {
	Version        int        `json:"version"`
	MachineName    string     `json:"machineName"`
	NVIDIARequired bool       `json:"nvidiaRequired"`
	NVIDIA         NvidiaHost `json:"nvidia"`
}

func NSpawnLifecycleStatePath(machineName string) string {
	return filepath.Join(AgentConfigDir, machineName+"-nspawn-lifecycle.json")
}

func (s *NSpawnLifecycleState) Validate(machineName string) error {
	if s.Version != NSpawnLifecycleStateVersion {
		return fmt.Errorf("unsupported nspawn lifecycle state version %d", s.Version)
	}

	if s.MachineName != machineName {
		return fmt.Errorf("nspawn lifecycle state is for machine %q, not %q", s.MachineName, machineName)
	}

	if s.NVIDIARequired && !NVIDIAStateAvailable(s.NVIDIA) {
		return fmt.Errorf("NVIDIA is required but resolved host state is incomplete")
	}

	if !s.NVIDIARequired && !reflect.DeepEqual(s.NVIDIA, NvidiaHost{}) {
		return fmt.Errorf("CPU-provisioned machine contains NVIDIA lifecycle state")
	}

	return nil
}

// NVIDIAStateAvailable reports whether discovery produced all host state needed
// to render mounts and perform in-machine setup.
func NVIDIAStateAvailable(nvidia NvidiaHost) bool {
	return len(nvidia.GPUDevicePaths) > 0 && len(nvidia.LibMappings) > 0 && nvidia.DriverVersion != ""
}
