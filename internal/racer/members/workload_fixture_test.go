// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package members

import "testing"

// Keep external-workload security assertions without a production builder API.
func TestWorkloadCompatibilityNames(t *testing.T) {
	if DataplaneDaemonSetName != "racer-dataplane" || PodNetworkDaemonSetName != "racer-dataplane-podnet" {
		t.Fatal("workload compatibility names changed")
	}
}
