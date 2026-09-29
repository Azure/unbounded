// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// DesiredDaemonSets builds steady-state placement, not a safe migration plan.
// The operator must additionally exclude occupied destination nodes until every
// source Pod, including terminating Pods, has disappeared.
func DesiredDaemonSets(c WorkloadConfig) ([]*appsv1.DaemonSet, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}

	nodes := slices.Clone(c.PodNetworkNodes)
	slices.Sort(nodes)

	c.PodNetworkNodes = nil

	host, err := DesiredDaemonSet(c)
	if err != nil {
		return nil, err
	}

	if len(nodes) == 0 {
		return []*appsv1.DaemonSet{host}, nil
	}

	c.HostNetwork = false
	c.DaemonSetName = PodNetworkDaemonSetName

	pod, err := DesiredDaemonSet(c)
	if err != nil {
		return nil, err
	}

	// The existing selector is immutable. Use a distinct app value, not an
	// additional label that would still match the original workload selector.
	pod.Labels["app.kubernetes.io/name"] = PodNetworkDaemonSetName
	pod.Spec.Selector.MatchLabels["app.kubernetes.io/name"] = PodNetworkDaemonSetName
	pod.Spec.Template.Labels["app.kubernetes.io/name"] = PodNetworkDaemonSetName
	hostSelector := host.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	podSelector := pod.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	base := podSelector.NodeSelectorTerms[0].DeepCopy()
	podSelector.NodeSelectorTerms = nil
	// Field selectors accept one value per requirement. Host exclusions are
	// ANDed; each pod-network node gets an OR term retaining the base constraints.
	for _, node := range nodes {
		hostSelector.NodeSelectorTerms[0].MatchFields = append(hostSelector.NodeSelectorTerms[0].MatchFields, corev1.NodeSelectorRequirement{
			Key: "metadata.name", Operator: corev1.NodeSelectorOpNotIn, Values: []string{node},
		})
		term := base.DeepCopy()
		term.MatchFields = []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{node}}}
		podSelector.NodeSelectorTerms = append(podSelector.NodeSelectorTerms, *term)
	}

	return []*appsv1.DaemonSet{host, pod}, nil
}
