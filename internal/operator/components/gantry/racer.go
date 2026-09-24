// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantry

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	racerv1alpha1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/operator/component"
	racermeta "github.com/Azure/unbounded/internal/racer"
)

const (
	backingAnnotation  = "unbounded-cloud.io/gantry-backing"
	cacheUIDAnnotation = "unbounded-cloud.io/gantry-cache-uid"
)

// selectBackingCache distinguishes invalid desired state (a direct plan and a
// visible diagnostic) from failed API reads (no plan, so the current pod stays).
// ConfigMap contents and the old gantry-cache label are not selection inputs.
// Gantry still parses its preserved YAML strictly before applying generated flags.
func selectBackingCache(ctx context.Context, env *component.Env, sites []unboundedv1alpha3.Site) (*racerv1alpha1.ClusterCache, component.Result, error) {
	invalid := func(message string) (*racerv1alpha1.ClusterCache, component.Result, error) {
		return nil, component.NotReady("InvalidGantryBacking", message+"; using direct backend"), nil
	}

	caches := &racerv1alpha1.ClusterCacheList{}
	if err := env.Client.List(ctx, caches); err != nil {
		return nil, component.Result{}, fmt.Errorf("list Gantry backing caches: %w", err)
	}

	slices.SortFunc(caches.Items, func(a, b racerv1alpha1.ClusterCache) int { return strings.Compare(a.Name, b.Name) })

	var selected *racerv1alpha1.ClusterCache

	for i := range caches.Items {
		cache := &caches.Items[i]
		if !cache.DeletionTimestamp.IsZero() {
			continue
		}

		value, present := cache.Annotations[backingAnnotation]
		if !present || value == "false" {
			continue
		}

		if value != "true" {
			return invalid(fmt.Sprintf("ClusterCache %q annotation %s must be true or false, got %q", cache.Name, backingAnnotation, value))
		}

		if selected != nil {
			return invalid(fmt.Sprintf("multiple Gantry backing caches: %q and %q", selected.Name, cache.Name))
		}

		selected = cache
	}

	if selected == nil {
		return nil, component.Reconciled(), nil
	}

	if len(selected.Spec.SiteSelector.MatchLabels) != 0 || len(selected.Spec.SiteSelector.MatchExpressions) != 0 {
		return invalid(fmt.Sprintf("Gantry backing ClusterCache %q requires an empty siteSelector", selected.Name))
	}

	if _, _, err := racermeta.CacheSockets(racermeta.SocketRoot, string(selected.UID)); err != nil {
		return invalid(fmt.Sprintf("Gantry backing ClusterCache %q: %v", selected.Name, err))
	}

	eligible := map[string]bool{}
	gantryEnabled := false

	for i := range sites {
		site := &sites[i]

		if site.DeletionTimestamp.IsZero() {
			eligible[site.Name] = true
			gantryEnabled = gantryEnabled || EnabledFor(site)
		}
	}

	if !gantryEnabled {
		return invalid("Gantry Racer backend requires at least one live Site enabling Gantry")
	}
	// Gantry tolerates every taint. Fall back rather than narrowing its coverage.
	nodes := &corev1.NodeList{}
	if err := env.Client.List(ctx, nodes); err != nil {
		return nil, component.Result{}, fmt.Errorf("validate Gantry Racer node coverage: %w", err)
	}

	for i := range nodes.Items {
		node := &nodes.Items[i]
		if !node.DeletionTimestamp.IsZero() {
			continue
		}

		if !eligible[racermeta.NodeSite(node)] || !racermeta.NodeEligible(node) || node.Labels[corev1.LabelOSStable] != "linux" {
			return invalid(fmt.Sprintf("Gantry serving Node %q must be Linux and belong to a live Site without Racer exclusion", node.Name))
		}

		for _, taint := range node.Spec.Taints {
			if !daemonSetTolerates(taint) {
				return invalid(fmt.Sprintf("Gantry serving Node %q has taint %q not tolerated by the managed Racer dataplane", node.Name, taint.Key))
			}
		}
	}
	// Do not wait for Racer readiness: Gantry must start its origin first.
	// The cache remains entirely user-owned, including generation and policy.
	return selected, component.Reconciled(), nil
}

func daemonSetTolerates(taint corev1.Taint) bool {
	if taint.Effect == corev1.TaintEffectNoExecute {
		return taint.Key == "node.kubernetes.io/not-ready" || taint.Key == "node.kubernetes.io/unreachable"
	}

	if taint.Effect == corev1.TaintEffectNoSchedule {
		return slices.Contains([]string{"node.kubernetes.io/disk-pressure", "node.kubernetes.io/memory-pressure", "node.kubernetes.io/pid-pressure", "node.kubernetes.io/unschedulable"}, taint.Key)
	}

	return true
}

func configureBackendPod(obj *unstructured.Unstructured, cache *racerv1alpha1.ClusterCache) error {
	if cache != nil {
		if _, _, err := racermeta.CacheSockets(racermeta.SocketRoot, string(cache.UID)); err != nil {
			return fmt.Errorf("gantry backing ClusterCache %q: %w", cache.Name, err)
		}
	}

	ds := &appsv1.DaemonSet{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, ds); err != nil {
		return err
	}

	pod := &ds.Spec.Template.Spec
	for i := range pod.Containers {
		c := &pod.Containers[i]
		if c.Name == agentContainerName {
			if cache == nil {
				c.Args = append(c.Args, "--content-backend=direct")
			} else {
				c.Args = append(c.Args, "--content-backend=racer", "--racer-cache-uid="+string(cache.UID))
			}
		}
	}

	if cache != nil {
		if ds.Spec.Template.Annotations == nil {
			ds.Spec.Template.Annotations = map[string]string{}
		}

		ds.Spec.Template.Annotations[cacheUIDAnnotation] = string(cache.UID)
		configureRacerPod(pod, string(cache.UID))
	}

	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(ds)
	if err != nil {
		return err
	}

	obj.Object["spec"] = u["spec"]
	unstructured.RemoveNestedField(obj.Object, "spec", "template", "metadata", "creationTimestamp")

	return nil
}

func configureRacerPod(pod *corev1.PodSpec, cacheUID string) {
	pod.AutomountServiceAccountToken = ptr.To(false)
	pod.SecurityContext = &corev1.PodSecurityContext{SupplementalGroups: []int64{65532}}
	pod.Volumes = append(pod.Volumes, corev1.Volume{Name: "racer-sockets", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: racermeta.SocketRoot, Type: ptr.To(corev1.HostPathDirectoryOrCreate)}}})
	mount := corev1.VolumeMount{Name: "racer-sockets", MountPath: racermeta.SocketRoot}

	for i := range pod.InitContainers {
		init := &pod.InitContainers[i]
		if init.Name == "chown-hostpaths" {
			init.VolumeMounts = append(init.VolumeMounts, mount)
			root := racermeta.SocketRoot
			directory := root + "/" + cacheUID
			init.Command[len(init.Command)-1] += "\nmkdir -p " + directory + "\nchgrp 65532 " + root + " " + directory + "\nchmod 2770 " + root + " " + directory + "\n"
		}
	}

	for i := range pod.Containers {
		c := &pod.Containers[i]
		if c.Name != agentContainerName {
			continue
		}

		c.VolumeMounts = append(c.VolumeMounts, mount)

		ports := c.Ports[:0]
		for _, port := range c.Ports {
			if port.Name != "transfer" && port.Name != "chaircall" {
				ports = append(ports, port)
			}
		}

		c.Ports = ports
	}
}

func setupRacerWatches(b *builder.Builder, env *component.Env) {
	b.Watches(&racerv1alpha1.ClusterCache{}, env.RequestSingleton(), builder.WithPredicates(backingCachePredicate()))
	b.Watches(&unboundedv1alpha3.Site{}, env.RequestSingleton(), builder.WithPredicates(gantrySitePredicate()))
	b.Watches(&corev1.Node{}, env.RequestSingleton(), builder.WithPredicates(gantryNodePredicate()))
}

func backingCachePredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool { return true },
		DeleteFunc: func(event.DeleteEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			old, oldOK := e.ObjectOld.(*racerv1alpha1.ClusterCache)

			next, nextOK := e.ObjectNew.(*racerv1alpha1.ClusterCache)
			if !oldOK || !nextOK {
				return false
			}

			oldValue, oldPresent := old.Annotations[backingAnnotation]
			newValue, newPresent := next.Annotations[backingAnnotation]

			// A relist can report same-name recreation as an Update even when
			// intent is identical. The new UID must reach the pod rollout stamp.
			return old.UID != next.UID || oldValue != newValue || oldPresent != newPresent || !reflect.DeepEqual(old.Spec, next.Spec) || !old.DeletionTimestamp.Equal(next.DeletionTimestamp)
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

func gantrySitePredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool { return true },
		DeleteFunc: func(event.DeleteEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			old, oldOK := e.ObjectOld.(*unboundedv1alpha3.Site)
			next, nextOK := e.ObjectNew.(*unboundedv1alpha3.Site)

			return oldOK && nextOK && (EnabledFor(old) != EnabledFor(next) || !old.DeletionTimestamp.Equal(next.DeletionTimestamp))
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

func gantryNodePredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool { return true },
		DeleteFunc: func(event.DeleteEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldNode, oldOK := e.ObjectOld.(*corev1.Node)
			newNode, newOK := e.ObjectNew.(*corev1.Node)

			return oldOK && newOK && (!reflect.DeepEqual(oldNode.Labels, newNode.Labels) || !reflect.DeepEqual(oldNode.Spec.Taints, newNode.Spec.Taints) || !oldNode.DeletionTimestamp.Equal(newNode.DeletionTimestamp))
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}
