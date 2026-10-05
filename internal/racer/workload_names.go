// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import "github.com/Azure/unbounded/internal/racer/workload"

func managedWorkloadNames(cfg Config) []string {
	return workload.ManagedNames(cfg.DaemonSetName)
}
