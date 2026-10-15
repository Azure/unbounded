// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package members

import "github.com/Azure/unbounded/internal/racer/workload"

type Config = workload.Config

const (
	DataplaneDaemonSetName  = workload.DataplaneDaemonSetName
	PodNetworkDaemonSetName = workload.PodNetworkDaemonSetName
)

var (
	ConfigFromLookup  = workload.ConfigFromLookup
	DesiredDaemonSet  = workload.DesiredDaemonSet
	DesiredDaemonSets = workload.DesiredDaemonSets
	ManagedNames      = workload.ManagedNames
)
