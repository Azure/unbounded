// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package workload

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

func TestPlacementOfCopiesNodeConstraintsOnly(t *testing.T) {
	pod := &corev1.PodSpec{
		Affinity: &corev1.Affinity{
			NodeAffinity: DataplaneAffinity().NodeAffinity,
			PodAntiAffinity: &corev1.PodAntiAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{TopologyKey: "kubernetes.io/hostname"}},
			},
		},
		NodeSelector: map[string]string{"pool": "cache"},
		Tolerations:  []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpExists, TolerationSeconds: ptr.To(int64(30))}},
	}

	got := PlacementOf(pod)

	if got.Affinity == nil || got.Affinity.NodeAffinity == nil {
		t.Fatalf("affinity = %#v, want node affinity", got.Affinity)
	}

	if got.Affinity.PodAntiAffinity != nil || got.Affinity.PodAffinity != nil {
		t.Fatalf("affinity = %#v, want pod affinity rules dropped", got.Affinity)
	}

	if got.NodeSelector["pool"] != "cache" || len(got.Tolerations) != 1 || *got.Tolerations[0].TolerationSeconds != 30 {
		t.Fatalf("placement = %#v", got)
	}

	// The copy must not alias the source.
	got.NodeSelector["pool"] = "changed"
	*got.Tolerations[0].TolerationSeconds = 60
	got.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Key = "changed"

	if pod.NodeSelector["pool"] != "cache" || *pod.Tolerations[0].TolerationSeconds != 30 ||
		pod.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Key == "changed" {
		t.Fatal("PlacementOf aliased the source Pod spec")
	}
}

func TestPlacementOfEmptySpec(t *testing.T) {
	got := PlacementOf(&corev1.PodSpec{})
	if got.Affinity != nil || got.NodeSelector != nil || got.Tolerations != nil {
		t.Fatalf("placement = %#v, want empty", got)
	}
}
