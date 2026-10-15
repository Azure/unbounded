// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package testutil provides workload fixtures for controller tests.
package testutil

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
