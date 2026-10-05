// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package workload

const (
	DataplaneDaemonSetName  = "racer-dataplane"
	PodNetworkDaemonSetName = "racer-dataplane-podnet"
)

// ManagedNames includes both fixed operator workloads, even during migration.
// Custom standalone installations retain their single configured workload.
func ManagedNames(daemonSetName string) []string {
	if daemonSetName == DataplaneDaemonSetName {
		return []string{DataplaneDaemonSetName, PodNetworkDaemonSetName}
	}

	return []string{daemonSetName}
}
