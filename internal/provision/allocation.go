// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package provision

import (
	"strings"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// AllocationUID reports whether cfg belongs to a host launched for a
// MachineAllocation, and that allocation's UID. A host is allocation-backed
// when its kubelet providerID carries v1alpha3.AllocationProviderIDPrefix. A
// backed config with an empty uid is a malformed identity. A nil cfg is not
// backed.
func AllocationUID(cfg *AgentConfig) (uid string, backed bool) {
	if cfg == nil {
		return "", false
	}

	providerID, ok := cfg.Kubelet.Configuration["providerID"].(string)
	if !ok {
		return "", false
	}

	uid, backed = strings.CutPrefix(providerID, v1alpha3.AllocationProviderIDPrefix)
	if !backed {
		return "", false
	}

	return uid, true
}
