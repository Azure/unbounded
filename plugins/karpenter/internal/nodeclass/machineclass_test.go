// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nodeclass

import (
	"testing"

	"github.com/awslabs/operatorpkg/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Azure/unbounded/plugins/karpenter/internal/capacity"
)

func TestReadinessRejectsAnUnobservedGeneration(t *testing.T) {
	class := &MachineClass{
		ObjectMeta: metav1.ObjectMeta{
			Generation: 2,
		},
		Status: MachineClassStatus{
			ObservedGeneration: 1,
			Conditions: []status.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					ObservedGeneration: 1,
				},
			},
		},
	}

	if class.StatusConditions().Get("Ready").IsTrue() {
		t.Fatal("stale class readiness must not enable a NodePool")
	}

	class.Status.ObservedGeneration = 2
	class.Status.Conditions[0].ObservedGeneration = 2

	if !class.StatusConditions().Get("Ready").IsTrue() {
		t.Fatal("observed readiness should enable a NodePool")
	}
}

func TestDeepCopyDoesNotAliasDiscovery(t *testing.T) {
	class := &MachineClass{
		Status: MachineClassStatus{
			Offerings: []capacity.Offering{
				{
					Labels: map[string]string{
						"shape": "original",
					},
					Price: &capacity.Price{
						PerHour: "0.192",
					},
				},
			},
		},
	}

	copy := class.DeepCopyObject().(*MachineClass)
	copy.Status.Offerings[0].Labels["shape"] = "changed"
	copy.Status.Offerings[0].Price.PerHour = "0"

	if class.Status.Offerings[0].Labels["shape"] != "original" ||
		class.Status.Offerings[0].Price.PerHour != "0.192" {
		t.Fatal("cached shared discovery was mutated through a projection copy")
	}
}
