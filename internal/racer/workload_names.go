// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import "github.com/Azure/unbounded/internal/racer/workload"

// Keep name aliases while the membership API is extracted independently.
const (
	DataplaneDaemonSetName  = workload.DataplaneDaemonSetName
	PodNetworkDaemonSetName = workload.PodNetworkDaemonSetName
)

func managedWorkloadNames(cfg Config) []string {
	return workload.ManagedNames(cfg.DaemonSetName)
}
