// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantry

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	racerv1alpha1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	gantryconfig "github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/operator/component"
	racercomponent "github.com/Azure/unbounded/internal/operator/components/racer"
	racermeta "github.com/Azure/unbounded/internal/racer"
)

const cacheOwnerLabel = "unbounded-cloud.io/gantry-cache"

// The preserved main ConfigMap is the singleton authority. Never read operator
// process environment here: it is not the Gantry container's environment.
func contentConfig(ctx context.Context, env *component.Env, create *component.Operation) (*gantryconfig.Config, error) {
	cm := &corev1.ConfigMap{}
	if create != nil {
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(create.Object.Object, cm); err != nil {
			return nil, err
		}
	} else if err := env.Client.Get(ctx, client.ObjectKey{Namespace: env.Namespace, Name: configName}, cm); err != nil {
		return nil, err
	}

	cfg := gantryconfig.NewDefault()
	if err := cfg.LoadYAML(strings.NewReader(cm.Data["config.yaml"])); err != nil && err != io.EOF {
		return nil, fmt.Errorf("gantry-config config.yaml: %w", err)
	}

	if cfg.ContentBackend != "direct" && cfg.ContentBackend != "racer" {
		return nil, fmt.Errorf("gantry content_backend must be direct or racer, got %q", cfg.ContentBackend)
	}

	if _, _, err := racermeta.CacheSockets(racermeta.SocketRoot, cfg.RacerCacheName); err != nil {
		return nil, fmt.Errorf("gantry racer_cache_name: %w", err)
	}

	return cfg, nil
}

func planRacer(ctx context.Context, env *component.Env, sites []unboundedv1alpha3.Site, name string, plan *component.Plan) error {
	wanted, err := racercomponent.WantedOrRetained(ctx, env, sites)
	if err != nil {
		return err
	}

	if !wanted {
		return fmt.Errorf("gantry Racer backend requires a wanted or retained Racer installation")
	}

	eligible := map[string]bool{}

	for i := range sites {
		site := &sites[i]

		if site.DeletionTimestamp.IsZero() {
			eligible[site.Name] = true
		}
	}

	if len(eligible) == 0 {
		return fmt.Errorf("gantry Racer backend requires at least one live Site")
	}
	// Gantry is a retained cluster-wide DaemonSet tolerating every taint. Reject
	// uncovered serving nodes rather than silently narrowing it or using direct.
	nodes := &corev1.NodeList{}
	if err := env.Client.List(ctx, nodes); err != nil {
		return fmt.Errorf("validate Gantry Racer node coverage: %w", err)
	}

	for i := range nodes.Items {
		node := &nodes.Items[i]
		if !node.DeletionTimestamp.IsZero() {
			continue
		}

		if !eligible[racermeta.NodeSite(node)] || !racermeta.NodeEligible(node) || node.Labels[corev1.LabelOSStable] != "linux" {
			return fmt.Errorf("gantry serving Node %q must be Linux and belong to a live Site without Racer exclusion", node.Name)
		}

		for _, taint := range node.Spec.Taints {
			if !daemonSetTolerates(taint) {
				return fmt.Errorf("gantry serving Node %q has taint %q not tolerated by the managed Racer dataplane", node.Name, taint.Key)
			}
		}
	}
	// Empty selects all live Sites. Validation above makes that exactly
	// the Gantry participant set, including Sites added after initial installation.
	cache := &racerv1alpha1.P2PCache{
		TypeMeta:   metav1.TypeMeta{APIVersion: racerv1alpha1.GroupVersion.String(), Kind: "P2PCache"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{cacheOwnerLabel: "true"}},
		Spec:       racerv1alpha1.P2PCacheSpec{CacheGeneration: 1, MaxCandidateAttempts: 3},
	}
	existing := &racerv1alpha1.P2PCache{}

	err = env.Client.Get(ctx, client.ObjectKey{Name: name}, existing)
	if err == nil {
		if existing.Labels[cacheOwnerLabel] != "true" || len(existing.Spec.SiteSelector.MatchLabels) != 0 || len(existing.Spec.SiteSelector.MatchExpressions) != 0 || !existing.DeletionTimestamp.IsZero() {
			return fmt.Errorf("P2PCache %q must be dedicated to Gantry with an empty siteSelector and label %s=true", name, cacheOwnerLabel)
		}

		return nil
	}

	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("read Gantry P2PCache: %w", err)
	}

	plan.Add(component.Operation{Kind: component.OpCreateIfAbsent, Object: component.ToUnstructured(cache), Component: "gantry"})

	return nil
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

func configureRacerPod(obj *unstructured.Unstructured, cacheName string) error {
	ds := &appsv1.DaemonSet{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, ds); err != nil {
		return err
	}

	pod := &ds.Spec.Template.Spec
	pod.AutomountServiceAccountToken = ptr.To(false)
	pod.SecurityContext = &corev1.PodSecurityContext{SupplementalGroups: []int64{65532}}
	pod.Volumes = append(pod.Volumes, corev1.Volume{Name: "racer-sockets", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: racermeta.SocketRoot, Type: ptr.To(corev1.HostPathDirectoryOrCreate)}}})
	mount := corev1.VolumeMount{Name: "racer-sockets", MountPath: racermeta.SocketRoot}

	for i := range pod.InitContainers {
		init := &pod.InitContainers[i]
		if init.Name == "chown-hostpaths" {
			init.VolumeMounts = append(init.VolumeMounts, mount)
			init.Command[len(init.Command)-1] += "\nmkdir -p /dev/racer/" + cacheName + "\nchgrp 65532 /dev/racer /dev/racer/" + cacheName + "\nchmod 2770 /dev/racer /dev/racer/" + cacheName + "\n"
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

	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(ds)
	if err != nil {
		return err
	}

	obj.Object["spec"] = u["spec"]
	unstructured.RemoveNestedField(obj.Object, "spec", "template", "metadata", "creationTimestamp")

	return nil
}

func setupRacerWatches(b *builder.Builder, env *component.Env) {
	b.Watches(&racerv1alpha1.P2PCache{}, env.RequestSingleton(), builder.WithPredicates(predicate.Or(predicate.GenerationChangedPredicate{}, predicate.LabelChangedPredicate{})))
	b.Watches(&corev1.Node{}, env.RequestSingleton(), builder.WithPredicates(predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool { return true },
		DeleteFunc: func(event.DeleteEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldNode, oldOK := e.ObjectOld.(*corev1.Node)
			newNode, newOK := e.ObjectNew.(*corev1.Node)

			return oldOK && newOK && (!reflect.DeepEqual(oldNode.Labels, newNode.Labels) || !reflect.DeepEqual(oldNode.Spec.Taints, newNode.Spec.Taints))
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}))
}
