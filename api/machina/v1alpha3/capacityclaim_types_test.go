// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"encoding/json"
	"testing"
)

func TestCapacityClaimGB200PolicyRoundTrip(t *testing.T) {
	claim := CapacityClaim{
		Spec: CapacityClaimSpec{
			ImagePolicy: CapacityImagePolicy{RequireNVIDIA: true},
			GPU: &CapacityGPUPolicy{
				Vendor:        "NVIDIA",
				Model:         "GB200",
				Count:         4,
				RequireFabric: true,
			},
		},
	}

	data, err := json.Marshal(claim)
	if err != nil {
		t.Fatalf("marshal CapacityClaim: %v", err)
	}

	var decoded CapacityClaim
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal CapacityClaim: %v", err)
	}

	if decoded.Spec.GPU == nil || decoded.Spec.GPU.Count != 4 {
		t.Fatalf("decoded GPU policy = %#v, want four GPUs", decoded.Spec.GPU)
	}

	if !decoded.Spec.ImagePolicy.RequireNVIDIA {
		t.Fatal("decoded image policy does not require NVIDIA")
	}
}

func TestMachineAllocationInternalIPIsStatusOnly(t *testing.T) {
	machine := Machine{
		Status: MachineStatus{
			Allocation: &MachineAllocationStatus{
				Node: &MachineNodeObservation{
					Name:       "gb2002159d2426",
					InternalIP: "192.0.2.10",
				},
			},
		},
	}

	data, err := json.Marshal(machine)
	if err != nil {
		t.Fatalf("marshal Machine: %v", err)
	}

	var decoded Machine
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal Machine: %v", err)
	}

	decoded.Status.Allocation.Node.InternalIP = "192.0.2.11"
	if decoded.Spec.SSH != nil {
		t.Fatal("mutable DHCP observation unexpectedly created spec identity")
	}
}
