// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import "github.com/Azure/unbounded/internal/racer/members"

func managedWorkloadNames(cfg Config) []string {
	return members.ManagedNames(cfg.DaemonSetName)
}
