// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package goalstates

import "github.com/Azure/unbounded/pkg/agent/isolation"

// AppliedConfigName is the name the applied config of slot is stored under
// (AppliedConfigPath of the name) for a node in mode. An nspawn node keeps the
// slot itself, kube1 or kube2, which is what every agent has always read. A
// node in any other mode uses "<mode>-<slot>". Agents that predate that mode
// read only kube1 and kube2, so they find no applied config on such a host and
// refuse to operate it, instead of reading a host-native node as an nspawn
// machine and running nspawn operations against it.
func AppliedConfigName(mode isolation.Mode, slot string) string {
	if mode == "" || mode == isolation.ModeNSpawn {
		return slot
	}

	return string(mode) + "-" + slot
}

// AppliedConfigLocation is one place an applied config can be stored: the file
// name stem and the mode a config stored there must record.
type AppliedConfigLocation struct {
	// Name is the stem passed to AppliedConfigPath.
	Name string
	// Slot is the blue/green slot the config belongs to.
	Slot string
	// Mode is the isolation mode a config stored under Name runs in.
	Mode isolation.Mode
}

// AppliedConfigLocations lists every place this agent looks for the applied
// config of the running node, in search order: the nspawn slots first, as
// older agents search them, then the slots of the other modes.
func AppliedConfigLocations() []AppliedConfigLocation {
	var out []AppliedConfigLocation

	for _, mode := range []isolation.Mode{isolation.ModeNSpawn, isolation.ModeNone} {
		slots := []string{NSpawnMachineKube1, NSpawnMachineKube2}
		if mode == isolation.ModeNone {
			// A none node has no blue/green slot; it always records kube1.
			slots = slots[:1]
		}

		for _, slot := range slots {
			out = append(out, AppliedConfigLocation{Name: AppliedConfigName(mode, slot), Slot: slot, Mode: mode})
		}
	}

	return out
}
