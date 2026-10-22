// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package workload

import (
	"maps"

	corev1 "k8s.io/api/core/v1"
)

// NodePlacement holds the node-level scheduling constraints of a dataplane
// Pod. Workloads that must run on every node that serves a cache, such as
// cache origins, copy it so they follow the dataplane, including user
// overrides applied to the live DaemonSet.
type NodePlacement struct {
	// Affinity carries only node affinity. Pod affinity and anti-affinity
	// are relative to dataplane Pods, so a rule such as hostname
	// anti-affinity against racer-dataplane would otherwise exclude the
	// copying workload from every dataplane node.
	Affinity     *corev1.Affinity
	NodeSelector map[string]string
	Tolerations  []corev1.Toleration
}

// PlacementOf returns a deep copy of the node-level scheduling constraints of
// a Pod spec.
func PlacementOf(pod *corev1.PodSpec) NodePlacement {
	var placement NodePlacement

	if pod.Affinity != nil && pod.Affinity.NodeAffinity != nil {
		placement.Affinity = &corev1.Affinity{NodeAffinity: pod.Affinity.NodeAffinity.DeepCopy()}
	}

	placement.NodeSelector = maps.Clone(pod.NodeSelector)

	if pod.Tolerations != nil {
		placement.Tolerations = make([]corev1.Toleration, len(pod.Tolerations))
		for i := range pod.Tolerations {
			pod.Tolerations[i].DeepCopyInto(&placement.Tolerations[i])
		}
	}

	return placement
}
