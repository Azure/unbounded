// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/racer/members"
)

// Scheduling state and live occupants are the durable migration checkpoint.
// Never delete a workload or Pod to advance this state machine.
func migrationPlan(ctx context.Context, env *component.Env, cfg members.Config) ([]*appsv1.DaemonSet, component.Result, error) {
	sets, err := members.DesiredDaemonSets(cfg)
	if err != nil {
		return nil, component.Result{}, err
	}

	live := map[string]*appsv1.DaemonSet{}

	for _, name := range []string{dataplaneName, members.PodNetworkDaemonSetName} {
		ds := &appsv1.DaemonSet{}
		if err := env.LiveReader().Get(ctx, objectKey(env, name), ds); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}

			return nil, component.Result{}, err
		}

		if ds.DeletionTimestamp != nil {
			return nil, component.Result{}, fmt.Errorf("dataplane workload %s is deleting", name)
		}

		live[name] = ds
	}
	// Retain an empty, unschedulable second workload on the return path. Its UID
	// remains stable and its terminating Pods continue to block the destination.
	if len(sets) == 1 && live[members.PodNetworkDaemonSetName] != nil {
		emptyConfig := cfg
		emptyConfig.HostNetwork = true
		emptyConfig.PodNetworkNodes = []string{"racer-migration-blocked"}

		emptySets, err := members.DesiredDaemonSets(emptyConfig)
		if err != nil {
			return nil, component.Result{}, err
		}

		pod := emptySets[1]
		pod.Spec.Template.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: blockedTerms()}}}
		sets = append(sets, pod)
	}

	pods := &corev1.PodList{}
	if err := env.LiveReader().List(ctx, pods, client.InNamespace(env.Namespace)); err != nil {
		return nil, component.Result{}, err
	}

	result := component.ReconciledAfter("Racer installation reconciled", time.Hour)

	for _, desired := range sets {
		sourceName := members.PodNetworkDaemonSetName
		if desired.Name == sourceName {
			sourceName = dataplaneName
		}

		source := live[sourceName]
		blocked := map[string]bool{}
		blockAll := false
		// A source controller must observe the drain before a destination can be
		// admitted, even if the Pod list is temporarily empty.
		if source != nil {
			if source.Status.ObservedGeneration < source.Generation {
				// Observation lag must prevent new admission, not evict unrelated
				// existing destinations and make both controllers drain each other.
				if destination := live[desired.Name]; destination == nil {
					blockAll = true
				} else {
					retainPlacement(desired, destination)
				}

				result = component.NotReadyAfter("Migrating", "waiting for source controller observation", 5*time.Second)
			}

			if desired.Name == members.PodNetworkDaemonSetName {
				for _, node := range cfg.PodNetworkNodes {
					if permitsNode(source, node) {
						blocked[node] = true
					}
				}
			} else {
				selector := source.Spec.Template.Spec.Affinity
				if selector == nil || selector.NodeAffinity == nil || selector.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
					blockAll = true
				} else {
					for _, term := range selector.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
						bounded := false

						for _, field := range term.MatchFields {
							if field.Key == "metadata.name" && field.Operator == corev1.NodeSelectorOpIn {
								for _, node := range field.Values {
									if permitsNode(source, node) && permitsNode(desired, node) {
										blocked[node] = true
									}
								}

								bounded = true
							}
						}

						if !bounded {
							blockAll = true
						}
					}
				}
			}
		}

		for i := range pods.Items {
			pod := &pods.Items[i]
			if !migrationPod(pod) {
				continue
			}

			owner := metav1.GetControllerOf(pod)

			current := (*appsv1.DaemonSet)(nil)
			if owner != nil {
				current = live[owner.Name]
			}

			owned := current != nil && current.UID != "" && owner.APIVersion == "apps/v1" && owner.Kind == "DaemonSet" && owner.UID == current.UID
			if owned && owner.Name == desired.Name {
				continue
			}

			if !owned {
				result = component.NotReadyAfter("MigrationBlocked", "unexpected or stale-UID dataplane occupant; manual inspection required", 5*time.Second)
			}

			node := pod.Spec.NodeName
			if node == "" && owned {
				node = pendingDaemonSetTarget(pod)
			}

			if node == "" {
				blockAll = true
			} else if permitsNode(desired, node) {
				blocked[node] = true
			}
		}

		if blockAll || len(blocked) != 0 {
			if result.Ready {
				result = component.NotReadyAfter("Migrating", "waiting for source scheduling drain and all source Pods to disappear", 5*time.Second)
			}

			selector := desired.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
			if blockAll {
				selector.NodeSelectorTerms = blockedTerms()
			} else {
				nodes := make([]string, 0, len(blocked))
				for node := range blocked {
					nodes = append(nodes, node)
				}

				slices.Sort(nodes)

				for i := range selector.NodeSelectorTerms {
					for _, node := range nodes {
						selector.NodeSelectorTerms[i].MatchFields = append(selector.NodeSelectorTerms[i].MatchFields, corev1.NodeSelectorRequirement{Key: "metadata.name", Operator: corev1.NodeSelectorOpNotIn, Values: []string{node}})
					}
				}
			}
		}
	}

	return sets, result, nil
}

// pendingDaemonSetTarget recognizes the required affinity installed on a Pod by
// the DaemonSet controller, not generic workload template affinity. The caller
// must verify current DaemonSet ownership. Every OR term must enforce the same
// single target; this is a conservative occupancy claim, not proof of binding.
func pendingDaemonSetTarget(pod *corev1.Pod) string {
	a := pod.Spec.Affinity
	if a == nil || a.NodeAffinity == nil || a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return ""
	}

	target := ""

	for _, term := range a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		if len(term.MatchExpressions) != 0 || len(term.MatchFields) != 1 {
			return ""
		}

		field := term.MatchFields[0]
		if field.Key != "metadata.name" || field.Operator != corev1.NodeSelectorOpIn || len(field.Values) != 1 || field.Values[0] == "" {
			return ""
		}

		if target != "" && target != field.Values[0] {
			return ""
		}

		target = field.Values[0]
	}

	return target
}

// Retain previously applied node-name interlocks until the source controller
// observes its drain. Label constraints are reapplied by the override merger,
// not copied here, to avoid multiplying user OR terms on every reconcile.
func retainPlacement(desired, live *appsv1.DaemonSet) {
	a := live.Spec.Template.Spec.Affinity
	if a == nil || a.NodeAffinity == nil || a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return
	}

	selector := desired.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution

	var terms []corev1.NodeSelectorTerm

	for _, want := range selector.NodeSelectorTerms {
		for _, old := range a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
			if len(old.MatchExpressions) == 0 && len(old.MatchFields) == 0 {
				continue
			}

			term := want.DeepCopy()

			term.MatchFields = unionRequirements(term.MatchFields, old.MatchFields)
			if contradictoryNames(term.MatchFields) {
				continue
			}

			if !slices.ContainsFunc(terms, func(existing corev1.NodeSelectorTerm) bool { return reflect.DeepEqual(existing, *term) }) {
				terms = append(terms, *term)
			}
		}
	}

	if len(terms) == 0 {
		terms = blockedTerms()
	}

	selector.NodeSelectorTerms = terms
}

// metadata.name is single-valued. Discard impossible Cartesian combinations
// instead of multiplying them on every pass while observation is delayed.
func contradictoryNames(requirements []corev1.NodeSelectorRequirement) bool {
	for _, required := range requirements {
		if required.Key != "metadata.name" || required.Operator != corev1.NodeSelectorOpIn || len(required.Values) != 1 {
			continue
		}

		for _, other := range requirements {
			if other.Key != "metadata.name" {
				continue
			}

			if other.Operator == corev1.NodeSelectorOpIn && !slices.Contains(other.Values, required.Values[0]) || other.Operator == corev1.NodeSelectorOpNotIn && slices.Contains(other.Values, required.Values[0]) {
				return true
			}
		}
	}

	return false
}

func unionRequirements(left, right []corev1.NodeSelectorRequirement) []corev1.NodeSelectorRequirement {
	for _, requirement := range right {
		if !slices.ContainsFunc(left, func(existing corev1.NodeSelectorRequirement) bool { return reflect.DeepEqual(existing, requirement) }) {
			left = append(left, requirement)
		}
	}

	return left
}

func permitsNode(ds *appsv1.DaemonSet, node string) bool {
	a := ds.Spec.Template.Spec.Affinity
	if a == nil || a.NodeAffinity == nil || a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return true
	}

	for _, term := range a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		allowed := true

		for _, field := range term.MatchFields {
			if field.Key != "metadata.name" {
				continue
			}

			if field.Operator == corev1.NodeSelectorOpIn && !slices.Contains(field.Values, node) || field.Operator == corev1.NodeSelectorOpNotIn && slices.Contains(field.Values, node) {
				allowed = false
			}
		}

		if allowed {
			return true
		}
	}

	return false
}

func blockedTerms() []corev1.NodeSelectorTerm {
	return []corev1.NodeSelectorTerm{{MatchFields: []corev1.NodeSelectorRequirement{
		{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{"racer-migration-blocked"}},
		{Key: "metadata.name", Operator: corev1.NodeSelectorOpNotIn, Values: []string{"racer-migration-blocked"}},
	}}}
}

func migrationPod(obj client.Object) bool {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return false
	}

	owner := metav1.GetControllerOf(pod)
	if owner != nil && (owner.Name == dataplaneName || owner.Name == members.PodNetworkDaemonSetName) {
		return true
	}

	if pod.Labels["app.kubernetes.io/name"] == dataplaneName || pod.Labels["app.kubernetes.io/name"] == members.PodNetworkDaemonSetName {
		return true
	}

	// Identity and slabs are exclusive dataplane state. Socket directories under
	// /run/racer are shared with clients and origin servers, not occupancy claims.
	for _, volume := range pod.Spec.Volumes {
		if volume.HostPath != nil && (volume.HostPath.Path == "/var/lib/racer/identity" || volume.HostPath.Path == "/var/lib/racer/slabs") {
			return true
		}
	}

	return false
}
