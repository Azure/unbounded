// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantry

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/racer/workload"
)

const (
	// RacerCacheName is the ClusterCache that switches an enabled Gantry
	// installation to its Racer backend. The Gantry agent always serves the
	// cache with this name (internal/gantry/racer.CacheName), so no other
	// cache name can select it.
	RacerCacheName = "gantry"

	racerEnabledEnv    = "GANTRY_RACER_ENABLED"
	racerClientVolume  = "racer-client"
	racerOriginVolume  = "racer-origin"
	reasonRacerPending = "RacerPending"

	// reasonRacerOriginConflict reports a ClusterCache/gantry that sets a
	// managed origin. Racer would then run its own origin on the Gantry origin
	// socket, and the SDK allows only one owner per socket.
	reasonRacerOriginConflict = "RacerOriginConflict"

	racerPendingRequeue = 30 * time.Second
)

// racerSocketDir is the host directory holding the Gantry cache socket for an
// SDK role. The Racer dataplane creates both directories; Gantry connects to
// the client socket and listens on the origin socket.
func racerSocketDir(role string) string {
	return "/run/racer/" + RacerCacheName + "/" + role
}

// racerMode reports whether the Gantry Racer backend is requested and, when
// it is, the node placement of the live racer DaemonSet. Racer mode
// is requested while ClusterCache/gantry exists and is not being deleted. A
// nil placement with requested=true means the dataplane DaemonSet does not
// exist yet, so no safe placement is known and the caller must wait.
// conflict=true means the cache sets spec.origin; Racer then runs its own
// origin on the Gantry origin socket, so Gantry stays in default mode.
func racerMode(ctx context.Context, env *component.Env) (requested, conflict bool, placement *workload.NodePlacement, err error) {
	cache := &racerv1.ClusterCache{}

	err = env.LiveReader().Get(ctx, client.ObjectKey{Name: RacerCacheName}, cache)
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return false, false, nil, nil
	}

	if err != nil {
		return false, false, nil, fmt.Errorf("get ClusterCache/%s: %w", RacerCacheName, err)
	}

	if cache.DeletionTimestamp != nil {
		return false, false, nil, nil
	}

	if cache.Spec.Origin != nil {
		return false, true, nil, nil
	}

	ds := &appsv1.DaemonSet{}

	err = env.LiveReader().Get(ctx, client.ObjectKey{Namespace: env.Namespace, Name: workload.DataplaneDaemonSetName}, ds)
	if apierrors.IsNotFound(err) {
		return true, false, nil, nil
	}

	if err != nil {
		return false, false, nil, fmt.Errorf("get DaemonSet/%s placement: %w", workload.DataplaneDaemonSetName, err)
	}

	got := workload.PlacementOf(&ds.Spec.Template.Spec)

	return true, false, &got, nil
}

// racerPending is the result while Gantry waits for Racer to deploy its
// dataplane before switching to the Racer backend.
func racerPending() component.Result {
	return component.NotReadyAfter(reasonRacerPending,
		fmt.Sprintf("ClusterCache/%s selects the Gantry Racer backend; waiting for DaemonSet/%s", RacerCacheName, workload.DataplaneDaemonSetName),
		racerPendingRequeue)
}

// racerOriginConflict is the result while ClusterCache/gantry sets a managed
// origin. Gantry stays in default mode because it is the origin for that cache.
func racerOriginConflict() component.Result {
	return component.NotReady(reasonRacerOriginConflict,
		fmt.Sprintf("ClusterCache/%s must not set spec.origin because Gantry is its origin; Gantry stays in default mode", RacerCacheName))
}

// applyRacerMode configures the Gantry agent DaemonSet for the Racer backend:
// it enables Racer mode, mounts the cache socket directories, and replaces the
// node placement with the dataplane's so Gantry serves as the cache origin on
// every node that runs the dataplane, and only there. The socket directories
// are mounted with hostPath type Directory, so a Pod scheduled before Racer
// creates them waits instead of creating root-owned directories.
func applyRacerMode(obj *unstructured.Unstructured, placement *workload.NodePlacement) error {
	containers, _, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
	if err != nil {
		return fmt.Errorf("get gantry containers: %w", err)
	}

	found := false

	for i := range containers {
		container, ok := containers[i].(map[string]any)
		if !ok || container["name"] != agentContainerName {
			continue
		}

		found = true

		if err := upsertNamed(container, "env", &corev1.EnvVar{Name: racerEnabledEnv, Value: "true"}); err != nil {
			return err
		}

		for _, mount := range []corev1.VolumeMount{
			{Name: racerClientVolume, MountPath: racerSocketDir("client"), ReadOnly: true},
			{Name: racerOriginVolume, MountPath: racerSocketDir("origin")},
		} {
			if err := upsertNamed(container, "volumeMounts", &mount); err != nil {
				return err
			}
		}

		containers[i] = container
	}

	if !found {
		return fmt.Errorf("gantry DaemonSet has no %q container", agentContainerName)
	}

	podSpec, _, err := unstructured.NestedMap(obj.Object, "spec", "template", "spec")
	if err != nil {
		return fmt.Errorf("get gantry pod spec: %w", err)
	}

	podSpec["containers"] = containers

	for _, volume := range []corev1.Volume{
		{Name: racerClientVolume, VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: racerSocketDir("client"), Type: ptr.To(corev1.HostPathDirectory)}}},
		{Name: racerOriginVolume, VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: racerSocketDir("origin"), Type: ptr.To(corev1.HostPathDirectory)}}},
	} {
		if err := upsertNamed(podSpec, "volumes", &volume); err != nil {
			return err
		}
	}

	if err := setPlacement(podSpec, placement); err != nil {
		return err
	}

	return unstructured.SetNestedMap(obj.Object, podSpec, "spec", "template", "spec")
}

// setPlacement replaces the node-level scheduling constraints of a Pod spec.
// Pod affinity rules in the manifest are kept; Gantry declares none today.
func setPlacement(podSpec map[string]any, placement *workload.NodePlacement) error {
	affinity, ok := podSpec["affinity"].(map[string]any)
	if ok {
		delete(affinity, "nodeAffinity")
	}

	if placement.Affinity != nil && placement.Affinity.NodeAffinity != nil {
		nodeAffinity, err := runtime.DefaultUnstructuredConverter.ToUnstructured(placement.Affinity.NodeAffinity)
		if err != nil {
			return fmt.Errorf("convert racer node affinity: %w", err)
		}

		if affinity == nil {
			affinity = map[string]any{}
		}

		affinity["nodeAffinity"] = nodeAffinity
	}

	if len(affinity) == 0 {
		delete(podSpec, "affinity")
	} else {
		podSpec["affinity"] = affinity
	}

	delete(podSpec, "nodeSelector")

	if len(placement.NodeSelector) != 0 {
		selector := make(map[string]any, len(placement.NodeSelector))
		for key, value := range placement.NodeSelector {
			selector[key] = value
		}

		podSpec["nodeSelector"] = selector
	}

	delete(podSpec, "tolerations")

	if len(placement.Tolerations) != 0 {
		tolerations := make([]any, 0, len(placement.Tolerations))

		for i := range placement.Tolerations {
			toleration, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&placement.Tolerations[i])
			if err != nil {
				return fmt.Errorf("convert racer toleration: %w", err)
			}

			tolerations = append(tolerations, toleration)
		}

		podSpec["tolerations"] = tolerations
	}

	return nil
}

// upsertNamed replaces the entry with the same name in the list field of obj,
// or appends it. value must be a pointer to a struct.
func upsertNamed(obj map[string]any, field string, value any) error {
	entry, err := runtime.DefaultUnstructuredConverter.ToUnstructured(value)
	if err != nil {
		return fmt.Errorf("convert %s entry: %w", field, err)
	}

	list, _, err := unstructured.NestedSlice(obj, field)
	if err != nil {
		return fmt.Errorf("get %s: %w", field, err)
	}

	for i := range list {
		existing, ok := list[i].(map[string]any)
		if ok && existing["name"] == entry["name"] {
			list[i] = entry

			return unstructured.SetNestedSlice(obj, list, field)
		}
	}

	return unstructured.SetNestedSlice(obj, append(list, entry), field)
}

// racerCachePredicate reconciles Gantry when ClusterCache/gantry is created,
// deleted, starts deleting, or changes its spec (generation), such as adding
// or removing spec.origin. Status-only writes do not change the Gantry mode.
func racerCachePredicate() predicate.Predicate {
	match := func(obj client.Object) bool { return obj.GetName() == RacerCacheName }

	return predicate.Funcs{
		CreateFunc: func(ev event.CreateEvent) bool { return match(ev.Object) },
		DeleteFunc: func(ev event.DeleteEvent) bool { return match(ev.Object) },
		UpdateFunc: func(ev event.UpdateEvent) bool {
			return match(ev.ObjectNew) &&
				((ev.ObjectOld.GetDeletionTimestamp() == nil) != (ev.ObjectNew.GetDeletionTimestamp() == nil) ||
					ev.ObjectOld.GetGeneration() != ev.ObjectNew.GetGeneration())
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}
