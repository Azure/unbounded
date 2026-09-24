// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantry

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	racerv1alpha1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/operator/component"
	racermeta "github.com/Azure/unbounded/internal/racer"
)

func racerSite() unboundedv1alpha3.Site {
	return unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "edge"}}
}

func backingCache(name string) *racerv1alpha1.P2PCache {
	return &racerv1alpha1.P2PCache{ObjectMeta: metav1.ObjectMeta{Name: name, UID: "cache-uid", Annotations: map[string]string{backingAnnotation: "true"}}, Spec: racerv1alpha1.P2PCacheSpec{CacheGeneration: 9, MaxCandidateAttempts: 5}}
}

func racerConfig(payload string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: configName, Namespace: component.DefaultNamespace}, Data: map[string]string{"config.yaml": payload}}
}

func TestRacerPlan(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", Labels: map[string]string{racermeta.SiteLabelKey: "edge", corev1.LabelOSStable: "linux"}}}
	env := testEnv(t, node, backingCache("gantry"), racerConfig("content_backend: direct\nracer_cache_name: stale\n"))

	plan, _, err := (Component{}).Plan(t.Context(), env, []unboundedv1alpha3.Site{racerSite()})
	if err != nil {
		t.Fatal(err)
	}

	var ds appsv1.DaemonSet

	for _, op := range plan.Operations {
		if op.Kind == component.OpDelete {
			continue
		}

		switch op.Object.GetKind() {
		case "Lease", "Role", "RoleBinding":
			t.Fatalf("Racer plan must not install chair resources: %s", op.Ref())
		case "P2PCache":
			t.Fatal("operator must never write the user-managed cache")
		case "DaemonSet":
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(op.Object.Object, &ds); err != nil {
				t.Fatal(err)
			}
		}
	}

	if ds.Spec.Template.Annotations[cacheUIDAnnotation] != "cache-uid" || !slices.Contains(ds.Spec.Template.Spec.Containers[0].Args, "--content-backend=racer") || !slices.Contains(ds.Spec.Template.Spec.Containers[0].Args, "--racer-cache-name=gantry") {
		t.Fatalf("cache identity and flags missing: %#v", ds.Spec.Template)
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
		valid   bool
		direct  bool
	}{
		{name: "unknown backend", valid: true, payload: "content_backend: other"},
		{name: "invalid cache name", valid: true, payload: "content_backend: racer\nracer_cache_name: ../bad"},
		// Parsing remains the binary's responsibility. Invalid YAML cannot
		// freeze a former Racer pod configuration after cache removal.
		{name: "unknown config field", valid: true, payload: "content_backnd: racer"},
		{name: "malformed YAML", valid: true, payload: "[not: yaml"},
		{name: "Racer disabled", valid: true, direct: true, cache: &racerv1alpha1.P2PCache{ObjectMeta: metav1.ObjectMeta{Name: "gantry", Annotations: map[string]string{backingAnnotation: "false"}}}},
		{name: "Racer omitted", valid: true},
		{name: "Gantry disabled", mutate: func(s *unboundedv1alpha3.Site, _ *corev1.Node) {
			s.Spec.Components.Gantry = &unboundedv1alpha3.GantryComponentSpec{SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: ptr.To(false)}}
		}},
		{name: "unassigned node", mutate: func(_ *unboundedv1alpha3.Site, n *corev1.Node) { delete(n.Labels, racermeta.SiteLabelKey) }},
		{name: "deprecated-only node", mutate: func(_ *unboundedv1alpha3.Site, n *corev1.Node) {
			delete(n.Labels, racermeta.SiteLabelKey)
			n.Labels[racermeta.DeprecatedSiteLabelKey] = "edge"
		}},
		{name: "excluded node", mutate: func(_ *unboundedv1alpha3.Site, n *corev1.Node) { n.Labels[racermeta.ExcludeLabelKey] = "true" }},
		{name: "canonical empty overrides legacy", mutate: func(_ *unboundedv1alpha3.Site, n *corev1.Node) {
			n.Labels[racermeta.SiteLabelKey] = ""
			n.Labels[racermeta.DeprecatedSiteLabelKey] = "edge"
		}},
		{name: "non Linux", mutate: func(_ *unboundedv1alpha3.Site, n *corev1.Node) { n.Labels[corev1.LabelOSStable] = "windows" }},
		{name: "untolerated taint", mutate: func(_ *unboundedv1alpha3.Site, n *corev1.Node) {
			n.Spec.Taints = []corev1.Taint{{Key: "dedicated", Effect: corev1.TaintEffectNoSchedule}}
		}},
		{name: "terminating Site", mutate: func(s *unboundedv1alpha3.Site, _ *corev1.Node) {
			s.DeletionTimestamp = ptr.To(metav1.Now())
		}},
		{name: "terminating uncovered node ignored", valid: true, mutate: func(_ *unboundedv1alpha3.Site, n *corev1.Node) {
			n.Labels = nil
			n.DeletionTimestamp = ptr.To(metav1.Now())
			n.Finalizers = []string{"test"}
		}},
		{name: "tolerated taint", valid: true, mutate: func(_ *unboundedv1alpha3.Site, n *corev1.Node) {
			n.Spec.Taints = []corev1.Taint{{Key: "node.kubernetes.io/not-ready", Effect: corev1.TaintEffectNoExecute}}
		}},
		{name: "foreign cache", valid: true, direct: true, cache: &racerv1alpha1.P2PCache{ObjectMeta: metav1.ObjectMeta{Name: "gantry"}}},
		{name: "selector excludes origin", cache: &racerv1alpha1.P2PCache{ObjectMeta: metav1.ObjectMeta{Name: "gantry", Annotations: map[string]string{backingAnnotation: "true"}}, Spec: racerv1alpha1.P2PCacheSpec{SiteSelector: metav1.LabelSelector{MatchLabels: map[string]string{"other": "true"}}}}},
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
			} else {
				objects = append(objects, backingCache("gantry"))
			}

			plan, result, err := (Component{}).Plan(t.Context(), testEnv(t, objects...), []unboundedv1alpha3.Site{site})
			if err != nil || plan == nil {
				t.Fatalf("selection must produce an executable plan: %v", err)
			}

			if result.Ready != tc.valid || (!tc.valid && (result.Reason != "InvalidGantryBacking" || result.Message == "")) {
				t.Fatalf("unexpected selection diagnostic: %#v", result)
			}

			backend := "racer"
			if !tc.valid || tc.direct {
				backend = "direct"
			}

			if !slices.Contains(plannedDaemonSet(t, plan).Spec.Template.Spec.Containers[0].Args, "--content-backend="+backend) {
				t.Fatalf("expected %s backend", backend)
			}
		})
	}
}

func TestRacerPreservesCacheGenerationAndSupportsCanonicalSite(t *testing.T) {
	cache := backingCache("custom")
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", Labels: map[string]string{racermeta.SiteLabelKey: "edge", corev1.LabelOSStable: "linux"}}}
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
	cache := &racerv1alpha1.P2PCache{ObjectMeta: metav1.ObjectMeta{Name: "gantry", Labels: map[string]string{"unbounded-cloud.io/gantry-cache": "true"}}}
	env := testEnv(t, cache, racerConfig("content_backend: racer"))

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

func plannedDaemonSet(t *testing.T, plan *component.Plan) *appsv1.DaemonSet {
	t.Helper()

	for _, op := range plan.Operations {
		if op.Kind != component.OpDelete && op.Object.GetKind() == "DaemonSet" && op.Object.GetName() == daemonSetName {
			ds := &appsv1.DaemonSet{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(op.Object.Object, ds); err != nil {
				t.Fatal(err)
			}

			return ds
		}
	}

	t.Fatal("missing Gantry DaemonSet")

	return nil
}

func TestBackingCacheSelectionMatrix(t *testing.T) {
	now := metav1.Now()

	for _, tc := range []struct {
		name    string
		mutate  func(*racerv1alpha1.P2PCache)
		extra   bool
		racer   bool
		invalid bool
	}{
		{name: "true", racer: true},
		{name: "false", mutate: func(c *racerv1alpha1.P2PCache) { c.Annotations[backingAnnotation] = "false" }},
		{name: "removed", mutate: func(c *racerv1alpha1.P2PCache) { delete(c.Annotations, backingAnnotation) }},
		{name: "label ignored", mutate: func(c *racerv1alpha1.P2PCache) {
			c.Annotations = nil
			c.Labels = map[string]string{"unbounded-cloud.io/gantry-cache": "true"}
		}},
		{name: "empty invalid", invalid: true, mutate: func(c *racerv1alpha1.P2PCache) { c.Annotations[backingAnnotation] = "" }},
		{name: "case invalid", invalid: true, mutate: func(c *racerv1alpha1.P2PCache) { c.Annotations[backingAnnotation] = "True" }},
		{name: "whitespace invalid", invalid: true, mutate: func(c *racerv1alpha1.P2PCache) { c.Annotations[backingAnnotation] = " true" }},
		{name: "duplicate", extra: true, invalid: true},
		{name: "terminating", mutate: func(c *racerv1alpha1.P2PCache) { c.DeletionTimestamp = &now; c.Finalizers = []string{"test"} }},
		{name: "terminating duplicate ignored", extra: true, racer: true, mutate: func(c *racerv1alpha1.P2PCache) { c.DeletionTimestamp = &now; c.Finalizers = []string{"test"} }},
		{name: "expression selector", invalid: true, mutate: func(c *racerv1alpha1.P2PCache) {
			c.Spec.SiteSelector.MatchExpressions = []metav1.LabelSelectorRequirement{{Key: "region", Operator: metav1.LabelSelectorOpExists}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := backingCache("gantry")
			if tc.mutate != nil {
				tc.mutate(cache)
			}

			objects := []client.Object{cache}
			if tc.extra {
				objects = append(objects, backingCache("other"))
			}

			plan, result, err := (Component{}).Plan(t.Context(), testEnv(t, objects...), []unboundedv1alpha3.Site{racerSite()})
			if err != nil || plan == nil || result.Ready == tc.invalid {
				t.Fatalf("plan=%v result=%#v err=%v", plan, result, err)
			}

			ds := plannedDaemonSet(t, plan)
			if slices.Contains(ds.Spec.Template.Spec.Containers[0].Args, "--content-backend=racer") != tc.racer {
				t.Fatalf("wrong args: %v", ds.Spec.Template.Spec.Containers[0].Args)
			}
		})
	}
}

func TestBackingCacheTransitionsAndUserData(t *testing.T) {
	cache := backingCache("gantry")
	cache.Labels = map[string]string{"user": "preserve"}
	cache.Annotations["user"] = "preserve"
	cm := racerConfig("content_backend: racer\nracer_cache_name: ../stale\nupstream_registries:\n  - name: private.example\n    endpoint: https://private.example\n")
	cm.Data["extra"] = "user data"
	cm.Labels = map[string]string{"user": "preserve"}
	env := testEnv(t, cache, cm)
	sites := []unboundedv1alpha3.Site{racerSite()}
	planPod := func(racer bool) *appsv1.DaemonSet {
		t.Helper()

		plan, result, err := (Component{}).Plan(t.Context(), env, sites)
		if err != nil || !result.Ready {
			t.Fatalf("result=%#v err=%v", result, err)
		}

		for _, op := range plan.Operations {
			if op.Object.GetKind() == "P2PCache" || (op.Object.GetKind() == "ConfigMap" && op.Object.GetName() == configName) {
				t.Fatalf("must preserve user-owned object: %s", op.Ref())
			}
		}

		ds := plannedDaemonSet(t, plan)
		if slices.Contains(ds.Spec.Template.Spec.Containers[0].Args, "--content-backend=racer") != racer {
			t.Fatal(ds.Spec.Template.Spec.Containers[0].Args)
		}

		return ds
	}
	first := planPod(true)

	var stored racerv1alpha1.P2PCache
	if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(cache), &stored); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(stored.Spec, cache.Spec) || !reflect.DeepEqual(stored.Labels, cache.Labels) || !reflect.DeepEqual(stored.Annotations, cache.Annotations) {
		t.Fatal("cache changed")
	}

	stored.Annotations[backingAnnotation] = "false"
	if err := env.Client.Update(t.Context(), &stored); err != nil {
		t.Fatal(err)
	}

	direct := planPod(false)

	baseObjects, err := decodeManifests(env, applyMutator(env.Config.Image(imageRepository), component.ConfigMapPayloadHash(cm)))
	if err != nil {
		t.Fatal(err)
	}

	for _, obj := range baseObjects {
		if obj.GetKind() != "DaemonSet" {
			continue
		}

		if err := configureBackendPod(obj, nil); err != nil {
			t.Fatal(err)
		}

		var baseline appsv1.DaemonSet
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &baseline); err != nil {
			t.Fatal(err)
		}

		if !reflect.DeepEqual(direct.Spec.Template, baseline.Spec.Template) {
			t.Fatal("direct did not restore the entire default pod")
		}
	}

	delete(stored.Annotations, backingAnnotation)

	if err := env.Client.Update(t.Context(), &stored); err != nil {
		t.Fatal(err)
	}

	planPod(false)

	stored.Annotations[backingAnnotation] = "true"
	if err := env.Client.Update(t.Context(), &stored); err != nil {
		t.Fatal(err)
	}

	planPod(true)

	if err := env.Client.Delete(t.Context(), &stored); err != nil {
		t.Fatal(err)
	}

	planPod(false)

	recreated := cache.DeepCopy()
	recreated.UID = "new-cache-uid"

	recreated.ResourceVersion = ""
	if err := env.Client.Create(t.Context(), recreated); err != nil {
		t.Fatal(err)
	}

	second := planPod(true)
	if first.Spec.Template.Annotations[cacheUIDAnnotation] == second.Spec.Template.Annotations[cacheUIDAnnotation] {
		t.Fatal("same-name recreation did not roll pods")
	}

	var preserved corev1.ConfigMap
	if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(cm), &preserved); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(preserved.Data, cm.Data) || !reflect.DeepEqual(preserved.Labels, cm.Labels) {
		t.Fatal("user config changed")
	}
}

func TestDirectPrerequisitesPrecedeWorkload(t *testing.T) {
	plan, _, err := (Component{}).Plan(t.Context(), testEnv(t), []unboundedv1alpha3.Site{racerSite()})
	if err != nil {
		t.Fatal(err)
	}

	var dependencies []component.ObjectRef

	for _, op := range plan.Operations {
		if op.Kind != component.OpDelete && op.Object.GetKind() == "DaemonSet" {
			dependencies = op.DependsOn
		}
	}

	count := 0

	for _, op := range plan.Operations {
		switch op.Object.GetKind() {
		case "Lease", "Role", "RoleBinding", "ServiceAccount":
			count++

			if !slices.Contains(dependencies, op.Ref()) {
				t.Fatalf("workload does not depend on %s", op.Ref())
			}
		}
	}

	if count != 67 {
		t.Fatalf("prerequisites=%d", count)
	}
}

func TestBackingCacheAPIReadFailuresPreservePlan(t *testing.T) {
	for _, failing := range []string{"caches", "nodes", "config"} {
		t.Run(failing, func(t *testing.T) {
			env := testEnv(t, backingCache("gantry"))
			failure := errors.New("API unavailable")
			env.Client = interceptor.NewClient(env.Client.(client.WithWatch), interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					_, cacheList := list.(*racerv1alpha1.P2PCacheList)

					_, nodeList := list.(*corev1.NodeList)
					if (failing == "caches" && cacheList) || (failing == "nodes" && nodeList) {
						return failure
					}

					return c.List(ctx, list, opts...)
				},
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if failing == "config" && key.Name == configName {
						return failure
					}

					return c.Get(ctx, key, obj, opts...)
				},
			})

			plan, _, err := (Component{}).Plan(t.Context(), env, []unboundedv1alpha3.Site{racerSite()})
			if plan != nil || !errors.Is(err, failure) {
				t.Fatalf("API error must preserve existing workload and retry: plan=%v err=%v", plan, err)
			}
		})
	}
}

func TestBackingCacheWatches(t *testing.T) {
	p := backingCachePredicate()

	for _, tc := range []struct {
		name   string
		change func(*racerv1alpha1.P2PCache)
		want   bool
	}{
		{name: "identical relist", change: func(*racerv1alpha1.P2PCache) {}},
		{name: "UID-only recreation", want: true, change: func(c *racerv1alpha1.P2PCache) { c.UID = "replacement-uid" }},
		{name: "resource version only", change: func(c *racerv1alpha1.P2PCache) { c.ResourceVersion = "new" }},
		{name: "annotation", want: true, change: func(c *racerv1alpha1.P2PCache) { c.Annotations[backingAnnotation] = "false" }},
		{name: "annotation removed", want: true, change: func(c *racerv1alpha1.P2PCache) { delete(c.Annotations, backingAnnotation) }},
		{name: "unrelated annotation", change: func(c *racerv1alpha1.P2PCache) { c.Annotations["other"] = "new" }},
		{name: "label", change: func(c *racerv1alpha1.P2PCache) { c.Labels = map[string]string{"other": "new"} }},
		{name: "status", change: func(c *racerv1alpha1.P2PCache) {
			c.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue}}
		}},
		{name: "spec", want: true, change: func(c *racerv1alpha1.P2PCache) { c.Spec.CacheGeneration++ }},
		{name: "deletion timestamp", want: true, change: func(c *racerv1alpha1.P2PCache) { c.DeletionTimestamp = ptr.To(metav1.Now()) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := backingCache("gantry")
			next := old.DeepCopy()
			tc.change(next)

			if p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: next}) != tc.want {
				t.Fatal("wrong watch verdict")
			}
		})
	}

	if !p.Create(event.CreateEvent{Object: backingCache("new")}) || !p.Delete(event.DeleteEvent{Object: backingCache("old")}) || p.Generic(event.GenericEvent{Object: backingCache("gantry")}) {
		t.Fatal("cache event types")
	}

	node := &corev1.Node{}
	nextNode := node.DeepCopy()

	nextNode.DeletionTimestamp = ptr.To(metav1.Now())
	if !gantryNodePredicate().Update(event.UpdateEvent{ObjectOld: node, ObjectNew: nextNode}) {
		t.Fatal("node termination must re-evaluate coverage")
	}

	site := racerSite()
	nextSite := site.DeepCopy()

	nextSite.DeletionTimestamp = ptr.To(metav1.Now())
	if !gantrySitePredicate().Update(event.UpdateEvent{ObjectOld: &site, ObjectNew: nextSite}) {
		t.Fatal("Site termination must re-evaluate coverage")
	}
}
