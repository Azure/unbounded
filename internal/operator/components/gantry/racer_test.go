// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantry

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	racerv1alpha1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/operator/component"
	racermeta "github.com/Azure/unbounded/internal/racer"
)

func racerSite() unboundedv1alpha3.Site {
	return unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "edge"}, Spec: unboundedv1alpha3.SiteSpec{Components: unboundedv1alpha3.SiteComponents{
		Racer: &unboundedv1alpha3.RacerComponentSpec{SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: ptr.To(true)}},
	}}}
}

func racerConfig(payload string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: configName, Namespace: component.DefaultNamespace}, Data: map[string]string{"config.yaml": payload}}
}

func TestRacerPlan(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", Labels: map[string]string{racermeta.SiteLabelKey: "edge", corev1.LabelOSStable: "linux"}}}
	env := testEnv(t, node, racerConfig("content_backend: racer\nracer_cache_name: gantry\n"))

	plan, _, err := (Component{}).Plan(t.Context(), env, []unboundedv1alpha3.Site{racerSite()})
	if err != nil {
		t.Fatal(err)
	}

	var (
		ds    appsv1.DaemonSet
		cache racerv1alpha1.P2PCache
	)

	for _, op := range plan.Operations {
		if op.Kind == component.OpDelete {
			continue
		}

		switch op.Object.GetKind() {
		case "Lease", "Role", "RoleBinding":
			t.Fatalf("Racer plan must not install chair resources: %s", op.Ref())
		case "P2PCache":
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(op.Object.Object, &cache); err != nil {
				t.Fatal(err)
			}
		case "DaemonSet":
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(op.Object.Object, &ds); err != nil {
				t.Fatal(err)
			}
		}
	}

	if cache.Name != "gantry" || cache.Labels[cacheOwnerLabel] != "true" || cache.Spec.CacheGeneration != 1 || len(cache.Spec.SiteSelector.MatchLabels) != 0 || len(cache.Spec.SiteSelector.MatchExpressions) != 0 {
		t.Fatalf("dedicated cache mapping: %#v", cache)
	}

	pod := ds.Spec.Template.Spec
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken || pod.SecurityContext == nil || len(pod.SecurityContext.SupplementalGroups) != 1 || pod.SecurityContext.SupplementalGroups[0] != 65532 {
		t.Fatalf("Racer pod identity: %#v", pod)
	}

	for _, c := range pod.Containers {
		if *c.SecurityContext.RunAsUser != 65532 || *c.SecurityContext.RunAsGroup != 0 {
			t.Fatal("must preserve nonroot containerd socket access")
		}

		found := false

		for _, mount := range c.VolumeMounts {
			if mount.Name == "racer-sockets" {
				found = mount.MountPath == "/dev/racer" && mount.SubPath == "" && !mount.ReadOnly
			}
		}

		if !found {
			t.Fatal("missing restart-safe writable Racer parent mount")
		}

		for _, port := range c.Ports {
			if port.ContainerPort == 5001 || port.ContainerPort == 5002 {
				t.Fatal("direct listeners exposed in Racer mode")
			}
		}
	}

	command := strings.Join(pod.InitContainers[0].Command, " ")
	if !strings.Contains(command, "mkdir -p /dev/racer/gantry") || !strings.Contains(command, "chmod 2770 /dev/racer /dev/racer/gantry") {
		t.Fatalf("cache directory must be writable before either process starts: %s", command)
	}

	for _, volume := range pod.Volumes {
		if volume.Name == "racer-sockets" && (volume.HostPath == nil || volume.HostPath.Path != "/dev/racer" || *volume.HostPath.Type != corev1.HostPathDirectoryOrCreate) {
			t.Fatalf("unexpected socket hostPath: %#v", volume)
		}
	}
}

func TestRacerRejectsMisconfiguration(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		mutate  func(*unboundedv1alpha3.Site, *corev1.Node)
		cache   *racerv1alpha1.P2PCache
	}{
		{name: "unknown backend", payload: "content_backend: other"},
		{name: "invalid cache name", payload: "content_backend: racer\nracer_cache_name: ../bad"},
		{name: "unknown config field", payload: "content_backnd: racer"},
		{name: "Racer disabled", mutate: func(s *unboundedv1alpha3.Site, _ *corev1.Node) { s.Spec.Components.Racer = nil }},
		{name: "Gantry disabled", mutate: func(s *unboundedv1alpha3.Site, _ *corev1.Node) {
			s.Spec.Components.Gantry = &unboundedv1alpha3.GantryComponentSpec{SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: ptr.To(false)}}
		}},
		{name: "unassigned node", mutate: func(_ *unboundedv1alpha3.Site, n *corev1.Node) { delete(n.Labels, racermeta.SiteLabelKey) }},
		{name: "excluded node", mutate: func(_ *unboundedv1alpha3.Site, n *corev1.Node) { n.Labels[racermeta.ExcludeLabelKey] = "true" }},
		{name: "canonical empty overrides legacy", mutate: func(_ *unboundedv1alpha3.Site, n *corev1.Node) {
			n.Labels[racermeta.SiteLabelKey] = ""
			n.Labels[racermeta.DeprecatedSiteLabelKey] = "edge"
		}},
		{name: "non Linux", mutate: func(_ *unboundedv1alpha3.Site, n *corev1.Node) { n.Labels[corev1.LabelOSStable] = "windows" }},
		{name: "untolerated taint", mutate: func(_ *unboundedv1alpha3.Site, n *corev1.Node) {
			n.Spec.Taints = []corev1.Taint{{Key: "dedicated", Effect: corev1.TaintEffectNoSchedule}}
		}},
		{name: "foreign cache", cache: &racerv1alpha1.P2PCache{ObjectMeta: metav1.ObjectMeta{Name: "gantry"}}},
		{name: "selector excludes origin", cache: &racerv1alpha1.P2PCache{ObjectMeta: metav1.ObjectMeta{Name: "gantry", Labels: map[string]string{cacheOwnerLabel: "true"}}, Spec: racerv1alpha1.P2PCacheSpec{SiteSelector: metav1.LabelSelector{MatchLabels: map[string]string{"other": "true"}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			site := racerSite()

			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", Labels: map[string]string{racermeta.SiteLabelKey: "edge", corev1.LabelOSStable: "linux"}}}
			if tc.mutate != nil {
				tc.mutate(&site, node)
			}

			payload := tc.payload
			if payload == "" {
				payload = "content_backend: racer"
			}

			objects := []client.Object{node, racerConfig(payload)}
			if tc.cache != nil {
				objects = append(objects, tc.cache)
			}

			plan, _, err := (Component{}).Plan(t.Context(), testEnv(t, objects...), []unboundedv1alpha3.Site{site})
			if err == nil || plan != nil {
				t.Fatalf("misconfiguration produced plan=%v err=%v", plan, err)
			}
		})
	}
}

func TestRacerPreservesCacheGenerationAndSupportsLegacySite(t *testing.T) {
	cache := &racerv1alpha1.P2PCache{ObjectMeta: metav1.ObjectMeta{Name: "custom", Labels: map[string]string{cacheOwnerLabel: "true"}}, Spec: racerv1alpha1.P2PCacheSpec{CacheGeneration: 9, MaxCandidateAttempts: 5}}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", Labels: map[string]string{racermeta.DeprecatedSiteLabelKey: "edge", corev1.LabelOSStable: "linux"}}}
	env := testEnv(t, cache, node, racerConfig("content_backend: racer\nracer_cache_name: custom"))

	plan, _, err := (Component{}).Plan(t.Context(), env, []unboundedv1alpha3.Site{racerSite()})
	if err != nil {
		t.Fatal(err)
	}

	for _, op := range plan.Operations {
		if op.Object.GetKind() == "P2PCache" {
			t.Fatal("existing cache generation/policy must not be overwritten")
		}
	}
}

func TestReturnToDirectRetainsCacheAndRestoresChairs(t *testing.T) {
	cache := &racerv1alpha1.P2PCache{ObjectMeta: metav1.ObjectMeta{Name: "gantry", Labels: map[string]string{cacheOwnerLabel: "true"}}}
	env := testEnv(t, cache, racerConfig("content_backend: direct"))

	plan, _, err := (Component{}).Plan(t.Context(), env, []unboundedv1alpha3.Site{*siteWithGantry("edge", nil)})
	if err != nil {
		t.Fatal(err)
	}

	chairs := 0

	for _, op := range plan.Operations {
		if op.Object.GetKind() == "P2PCache" {
			t.Fatal("returning to direct must retain the cache")
		}

		if op.Object.GetKind() == "Lease" && op.Kind == component.OpCreateIfAbsent {
			chairs++
		}

		if op.Object.GetKind() == "DaemonSet" && op.Object.GetName() == "gantry" {
			var ds appsv1.DaemonSet
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(op.Object.Object, &ds); err != nil {
				t.Fatal(err)
			}

			for _, volume := range ds.Spec.Template.Spec.Volumes {
				if volume.Name == "racer-sockets" {
					t.Fatal("direct mode must not mount Racer sockets")
				}
			}

			if len(ds.Spec.Template.Spec.Containers[0].Ports) != 5 {
				t.Fatal("direct ports must be restored")
			}
		}
	}

	if chairs != 64 {
		t.Fatalf("chair count=%d", chairs)
	}
}
