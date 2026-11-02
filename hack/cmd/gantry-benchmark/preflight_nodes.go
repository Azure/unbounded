// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (b *benchmark) validateStreamingDaemonSetNodes(ctx context.Context, namespace, name string, targetNodes []string) error {
	output, err := b.commands.Run(ctx, nil, "kubectl", "-n", namespace, "get", "daemonset", name, "-o", "json")
	if err != nil {
		return err
	}

	var daemonSet appsv1.DaemonSet
	if err := json.Unmarshal(output, &daemonSet); err != nil {
		return fmt.Errorf("decode daemonset %s: %w", name, err)
	}

	if len(targetNodes) == 0 || daemonSet.UID == "" || daemonSet.Spec.Selector == nil {
		return fmt.Errorf("daemonset %s coverage requires target nodes, a UID, and a selector", name)
	}

	if int(daemonSet.Status.DesiredNumberScheduled) != len(targetNodes) || int(daemonSet.Status.NumberReady) != len(targetNodes) {
		return fmt.Errorf("daemonset %s is ready on %d/%d target nodes", name, daemonSet.Status.NumberReady, len(targetNodes))
	}

	selector, err := metav1.LabelSelectorAsSelector(daemonSet.Spec.Selector)
	if err != nil || selector.Empty() {
		return fmt.Errorf("daemonset %s has an invalid or empty pod selector", name)
	}

	output, err = b.commands.Run(ctx, nil, "kubectl", "-n", namespace, "get", "pods", "-l", selector.String(), "-o", "json")
	if err != nil {
		return err
	}

	var pods corev1.PodList
	if err := json.Unmarshal(output, &pods); err != nil {
		return fmt.Errorf("decode daemonset %s pods: %w", name, err)
	}

	readyNodes := make(map[string]bool, len(targetNodes))

	for _, pod := range pods.Items {
		owner := metav1.GetControllerOf(&pod)
		if owner == nil || owner.Kind != "DaemonSet" || owner.UID != daemonSet.UID || owner.Name != name ||
			pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
			continue
		}

		ready := false

		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				ready = true
				break
			}
		}

		if !ready {
			continue
		}

		if pod.Spec.NodeName == "" || readyNodes[pod.Spec.NodeName] {
			return fmt.Errorf("daemonset %s has unscheduled or duplicate ready pods on node %q", name, pod.Spec.NodeName)
		}

		readyNodes[pod.Spec.NodeName] = true
	}

	var missing []string

	for _, node := range targetNodes {
		if !readyNodes[node] {
			missing = append(missing, node)
		}

		delete(readyNodes, node)
	}

	var unexpected []string
	for node := range readyNodes {
		unexpected = append(unexpected, node)
	}

	if len(missing) != 0 || len(unexpected) != 0 {
		sort.Strings(missing)
		sort.Strings(unexpected)

		return fmt.Errorf("daemonset %s ready-node coverage differs from target nodes: missing=%v unexpected=%v", name, missing, unexpected)
	}

	return nil
}
