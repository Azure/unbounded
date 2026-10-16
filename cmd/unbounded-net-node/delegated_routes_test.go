// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

func TestUnmanagedLocalPoolsDoNotAttractThirdPartyTraffic(t *testing.T) {
	state := &wireGuardState{sitePodCIDRPools: []string{"10.244.0.0/16"}}
	for _, managed := range []bool{false, true} {
		supernets := collectSupernets(state, nil, nil, nil, managed)
		if supernets["10.244.0.0/16"] != managed {
			t.Fatalf("local pool routing with management=%v: %v", managed, supernets)
		}
	}
}
