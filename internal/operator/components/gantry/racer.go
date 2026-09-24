// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantry

import (
	"context"
	"fmt"
	"reflect"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	racerv1alpha1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/operator/component"
	racermeta "github.com/Azure/unbounded/internal/racer"
)

const (
	backingCacheName   = "gantry"
	cacheUIDAnnotation = "unbounded-cloud.io/gantry-cache-uid"
)

// selectBackingCache distinguishes invalid desired state (a direct plan and a
// visible diagnostic) from failed API reads (no plan, so the current pod stays).
// Only ClusterCache/gantry selects Racer. ConfigMap contents and legacy cache
// annotations and labels are not selection inputs.
// Gantry still parses its preserved YAML strictly before applying generated flags.
func selectBackingCache(ctx context.Context, env *component.Env, sites []unboundedv1alpha3.Site) (*racerv1alpha1.ClusterCache, component.Result, error) {
	invalid := func(message string) (*racerv1alpha1.ClusterCache, component.Result, error) {
		return nil, component.NotReady("InvalidGantryBacking", message+"; using direct backend"), nil
	}

	selected := &racerv1alpha1.ClusterCache{}
	if err := env.Client.Get(ctx, client.ObjectKey{Name: backingCacheName}, selected); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, component.Reconciled(), nil
		}

		return nil, component.Result{}, fmt.Errorf("get Gantry backing ClusterCache %q: %w", backingCacheName, err)
	}

	if !selected.DeletionTimestamp.IsZero() {
		return nil, component.Reconciled(), nil
	}

	if len(selected.Spec.SiteSelector.MatchLabels) != 0 || len(selected.Spec.SiteSelector.MatchExpressions) != 0 {
		return invalid(fmt.Sprintf("Gantry backing ClusterCache %q requires an empty siteSelector", selected.Name))
	}

	if _, _, err := racermeta.CacheSockets(racermeta.SocketRoot, selected.Name); err != nil {
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
		if _, _, err := racermeta.CacheSockets(racermeta.SocketRoot, cache.Name); err != nil {
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
				c.Args = append(c.Args, "--content-backend=racer", "--racer-cache-name="+cache.Name)
			}
		}
	}

	if cache != nil {
		if ds.Spec.Template.Annotations == nil {
			ds.Spec.Template.Annotations = map[string]string{}
		}

		ds.Spec.Template.Annotations[cacheUIDAnnotation] = string(cache.UID)
		configureRacerPod(pod, cache.Name)
	}

	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(ds)
	if err != nil {
		return err
	}

	obj.Object["spec"] = u["spec"]
	unstructured.RemoveNestedField(obj.Object, "spec", "template", "metadata", "creationTimestamp")

	return nil
}

func configureRacerPod(pod *corev1.PodSpec, cacheName string) {
	pod.AutomountServiceAccountToken = ptr.To(false)
	pod.SecurityContext = &corev1.PodSecurityContext{SupplementalGroups: []int64{65532}}
	clientDirectory := racermeta.SocketRoot + "/" + cacheName + "/client"
	originDirectory := racermeta.SocketRoot + "/" + cacheName + "/origin"

	mounts := []corev1.VolumeMount{
		{Name: "racer-client-sockets", MountPath: clientDirectory},
		{Name: "racer-origin-sockets", MountPath: originDirectory},
	}
	for _, mount := range mounts {
		pod.Volumes = append(pod.Volumes, corev1.Volume{Name: mount.Name, VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: mount.MountPath, Type: ptr.To(corev1.HostPathDirectoryOrCreate)}}})
	}

	for i := range pod.InitContainers {
		init := &pod.InitContainers[i]
		if init.Name == "chown-hostpaths" {
			init.VolumeMounts = append(init.VolumeMounts, mounts...)
			directories := clientDirectory + " " + originDirectory
			// Kubelet creates these directories. Limit permission changes to
			// these mounts so other caches and the socket root remain isolated.
			init.Command[len(init.Command)-1] += "\nchgrp 65532 " + directories + "\nchmod 2770 " + directories + "\n"
		}
	}

	for i := range pod.Containers {
		c := &pod.Containers[i]
		if c.Name != agentContainerName {
			continue
		}

		c.VolumeMounts = append(c.VolumeMounts, mounts...)

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
	match := func(obj client.Object) bool {
		cache, ok := obj.(*racerv1alpha1.ClusterCache)

		return ok && cache != nil && cache.Name == backingCacheName
	}

	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return match(e.Object) },
		DeleteFunc: func(e event.DeleteEvent) bool { return match(e.Object) },
		UpdateFunc: func(e event.UpdateEvent) bool {
			old, oldOK := e.ObjectOld.(*racerv1alpha1.ClusterCache)

			next, nextOK := e.ObjectNew.(*racerv1alpha1.ClusterCache)
			if !oldOK || !nextOK || !match(old) || !match(next) {
				return false
			}

			// A relist can report same-name recreation as an Update even when
			// intent is identical. The new UID must reach the pod rollout stamp.
			return old.UID != next.UID || !reflect.DeepEqual(old.Spec, next.Spec) || !old.DeletionTimestamp.Equal(next.DeletionTimestamp)
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
