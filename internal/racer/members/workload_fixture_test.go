// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package members

import "github.com/Azure/unbounded/internal/racer/testutil"

// Keep external-workload security assertions without a production builder API.
type Config = testutil.Config

const (
	DataplaneDaemonSetName  = testutil.DataplaneDaemonSetName
	PodNetworkDaemonSetName = testutil.PodNetworkDaemonSetName
)

var (
	ConfigFromLookup  = testutil.ConfigFromLookup
	DesiredDaemonSet  = testutil.DesiredDaemonSet
	DesiredDaemonSets = testutil.DesiredDaemonSets
	ManagedNames      = testutil.ManagedNames
)
