// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/Azure/unbounded/internal/operator/component"
)

func TestControlPlaneSpreadingAndDisruptionProtection(t *testing.T) {
	d := controlDeployment("test", component.Config{})

	constraints := d.Spec.Template.Spec.TopologySpreadConstraints
	if len(constraints) != 2 {
		t.Fatal("expected node and zone spreading")
	}

	for i, key := range []string{corev1.LabelHostname, corev1.LabelTopologyZone} {
		c := constraints[i]

		selector, err := metav1.LabelSelectorAsSelector(c.LabelSelector)
		if err != nil || !selector.Matches(labels.Set(d.Spec.Template.Labels)) || c.TopologyKey != key || c.MaxSkew != 1 || c.WhenUnsatisfiable != corev1.ScheduleAnyway || len(c.MatchLabelKeys) != 0 {
			t.Fatal("spreading must cover old and new revisions and permit a two-node surge", c)
		}
	}

	for _, object := range sharedResources("test") {
		pdb, ok := object.(*policyv1.PodDisruptionBudget)
		if !ok {
			continue
		}

		selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
		if err != nil || !selector.Matches(labels.Set(d.Spec.Template.Labels)) || pdb.Spec.MinAvailable == nil || pdb.Spec.MinAvailable.IntVal != 1 || pdb.Spec.MaxUnavailable != nil {
			t.Fatal("PDB must preserve one ready leader or warm standby", pdb)
		}

		return
	}

	t.Fatal("missing shared PDB")
}

// Exercise the Deployment rolling-update budget: scale up to desired+surge,
// then remove at most total-minAvailable-newUnavailable old replicas. This is
// the Kubernetes reconcileOldReplicaSets budget, including a failed successor.
func TestControlPlaneRolloutAvailabilityBudget(t *testing.T) {
	d := controlDeployment("test", component.Config{})
	desired := int(*d.Spec.Replicas)
	maxPods := desired + d.Spec.Strategy.RollingUpdate.MaxSurge.IntValue()

	minAvailable := desired - d.Spec.Strategy.RollingUpdate.MaxUnavailable.IntValue()
	for _, initialReady := range []int{1, desired} {
		for _, successorReady := range []bool{false, true} {
			old, updated, available := desired, 0, initialReady

			oldUnavailable := old - available
			for step := 0; step < 10 && (old > 0 || updated < desired); step++ {
				added := min(desired-updated, maxPods-old-updated)

				updated += added
				if successorReady {
					available += added
				}
				// Kubernetes first cleans up unavailable old replicas, bounded by
				// total-minAvailable-newUnavailable, then scales down available ones.
				newUnavailable := 0
				if !successorReady {
					newUnavailable = updated
				}

				cleanup := min(oldUnavailable, old+updated-minAvailable-newUnavailable)
				old -= cleanup
				oldUnavailable -= cleanup

				remove := min(old, available-minAvailable)
				old -= remove

				available -= remove
				if available < minAvailable {
					t.Fatal("rollout removed protected availability")
				}
			}

			if successorReady && (old != 0 || updated != desired) {
				t.Fatal("healthy warm standbys deadlocked rollout")
			}

			if !successorReady && old < minAvailable {
				t.Fatal("failed successor removed last working old replica")
			}
		}
	}
}
