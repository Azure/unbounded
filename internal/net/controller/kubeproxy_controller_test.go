// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"errors"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	appsv1listers "k8s.io/client-go/listers/apps/v1"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
)

func TestShouldManageKubeProxyForNode(t *testing.T) {
	providerDS := &appsv1.DaemonSet{Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "kubernetes.azure.com/cluster", Operator: corev1.NodeSelectorOpExists}}}}}}}}}}}

	tests := []struct {
		name string
		node *corev1.Node
		want bool
	}{
		{name: "site node without provider coverage", node: nodeWithLabels(map[string]string{canonicalSiteLabelKey: "test"}), want: true},
		{name: "no site label", node: nodeWithLabels(map[string]string{}), want: false},
		{name: "aks cluster node excluded", node: nodeWithLabels(map[string]string{canonicalSiteLabelKey: "cluster", "kubernetes.azure.com/cluster": "rg"}), want: false},
		{name: "provider managed node excluded", node: nodeWithLabels(map[string]string{canonicalSiteLabelKey: "cluster", "kubernetes.azure.com/managedby": "aks"}), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldManageKubeProxyForNode(tt.node, []*appsv1.DaemonSet{providerDS}); got != tt.want {
				t.Fatalf("shouldManageKubeProxyForNode() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestManagedKubeProxyNodeUpdateAffectsReconcile(t *testing.T) {
	t.Parallel()

	oldNode := nodeWithLabels(map[string]string{canonicalSiteLabelKey: "site-a"})

	tests := []struct {
		name string
		old  any
		new  any
		want bool
	}{
		{
			name: "unchanged labels",
			old:  oldNode,
			new:  oldNode.DeepCopy(),
		},
		{
			name: "annotation only",
			old:  oldNode,
			new: func() *corev1.Node {
				node := oldNode.DeepCopy()
				node.Annotations = map[string]string{"net.unbounded-cloud.io/discovered-public-ip": "203.0.113.10"}

				return node
			}(),
		},
		{
			name: "label value changed",
			old:  oldNode,
			new:  nodeWithLabels(map[string]string{canonicalSiteLabelKey: "site-b"}),
			want: true,
		},
		{
			name: "label added",
			old:  oldNode,
			new:  nodeWithLabels(map[string]string{canonicalSiteLabelKey: "site-a", "provider": "covered"}),
			want: true,
		},
		{
			name: "unexpected object",
			old:  oldNode,
			new:  &corev1.Pod{},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := managedKubeProxyNodeUpdateAffectsReconcile(tt.old, tt.new); got != tt.want {
				t.Fatalf("managedKubeProxyNodeUpdateAffectsReconcile() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestManagedKubeProxyDaemonSetEventRelevant(t *testing.T) {
	t.Parallel()

	managed := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
		Name:      managedKubeProxyDaemonSetName("site-a"),
		Namespace: "unbounded-net",
	}}
	provider := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-proxy", Namespace: "kube-system"},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "kube-proxy"}}},
		}},
	}
	unrelated := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "metrics", Namespace: "monitoring"}}

	tests := []struct {
		name string
		obj  any
		want bool
	}{
		{name: "managed by name", obj: managed, want: true},
		{name: "provider", obj: provider, want: true},
		{name: "managed tombstone", obj: cache.DeletedFinalStateUnknown{Obj: managed}, want: true},
		{name: "unrelated", obj: unrelated},
		{name: "unexpected object", obj: &corev1.Pod{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := managedKubeProxyDaemonSetEventRelevant("unbounded-net", tt.obj); got != tt.want {
				t.Fatalf("managedKubeProxyDaemonSetEventRelevant() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestManagedKubeProxyDaemonSetUpdateAffectsReconcile(t *testing.T) {
	t.Parallel()

	managed := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
		Name:       managedKubeProxyDaemonSetName("site-a"),
		Namespace:  "unbounded-net",
		Generation: 2,
	}}
	provider := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-proxy", Namespace: "kube-system", Generation: 3},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "kube-proxy"}}},
		}},
	}
	unrelated := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "metrics", Namespace: "monitoring", Generation: 1}}

	tests := []struct {
		name string
		old  any
		new  any
		want bool
	}{
		{
			name: "managed status only",
			old:  managed,
			new: func() *appsv1.DaemonSet {
				ds := managed.DeepCopy()
				ds.Status.NumberReady = 1

				return ds
			}(),
		},
		{
			name: "managed spec drift",
			old:  managed,
			new: func() *appsv1.DaemonSet {
				ds := managed.DeepCopy()
				ds.Generation++

				return ds
			}(),
			want: true,
		},
		{
			name: "managed label drift",
			old:  managed,
			new: func() *appsv1.DaemonSet {
				ds := managed.DeepCopy()
				ds.Labels = map[string]string{"drift": "true"}

				return ds
			}(),
			want: true,
		},
		{
			name: "provider status only",
			old:  provider,
			new: func() *appsv1.DaemonSet {
				ds := provider.DeepCopy()
				ds.Status.NumberReady = 1

				return ds
			}(),
		},
		{
			name: "provider scheduling change",
			old:  provider,
			new: func() *appsv1.DaemonSet {
				ds := provider.DeepCopy()
				ds.Generation++
				ds.Spec.Template.Spec.NodeSelector = map[string]string{"provider": "covered"}

				return ds
			}(),
			want: true,
		},
		{
			name: "provider classification removed",
			old:  provider,
			new: func() *appsv1.DaemonSet {
				ds := provider.DeepCopy()
				ds.Generation++
				ds.Spec.Template.Spec.Containers[0].Name = "network-agent"

				return ds
			}(),
			want: true,
		},
		{
			name: "provider classification added",
			old:  unrelated,
			new: func() *appsv1.DaemonSet {
				ds := unrelated.DeepCopy()
				ds.Generation++
				ds.Spec.Template.Spec.Containers = []corev1.Container{{Image: "registry.example/kube-proxy:v1"}}

				return ds
			}(),
			want: true,
		},
		{
			name: "unrelated status only",
			old:  unrelated,
			new: func() *appsv1.DaemonSet {
				ds := unrelated.DeepCopy()
				ds.Status.NumberReady = 1

				return ds
			}(),
		},
		{
			name: "unexpected object",
			old:  unrelated,
			new:  &corev1.Pod{},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := managedKubeProxyDaemonSetUpdateAffectsReconcile("unbounded-net", tt.old, tt.new); got != tt.want {
				t.Fatalf("managedKubeProxyDaemonSetUpdateAffectsReconcile() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestManagedKubeProxySiteUpdateAffectsReconcile(t *testing.T) {
	t.Parallel()

	oldSite := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "site-a"},
		"spec":     map[string]any{"podCidrAssignments": []any{map[string]any{"cidrBlocks": []any{"100.125.0.0/16"}}}},
		"status":   map[string]any{"ready": false},
	}}
	statusUpdate := oldSite.DeepCopy()
	statusUpdate.Object["status"] = map[string]any{"ready": true}
	specUpdate := oldSite.DeepCopy()
	specUpdate.Object["spec"] = map[string]any{"podCidrAssignments": []any{map[string]any{"cidrBlocks": []any{"100.126.0.0/16"}}}}

	if managedKubeProxySiteUpdateAffectsReconcile(oldSite, statusUpdate) {
		t.Fatal("status-only Site update should not affect reconciliation")
	}

	if !managedKubeProxySiteUpdateAffectsReconcile(oldSite, specUpdate) {
		t.Fatal("Site spec update should affect reconciliation")
	}
}

func TestProviderKubeProxyDaemonSetsIgnoresManagedDaemonSets(t *testing.T) {
	dsList := []*appsv1.DaemonSet{
		{ObjectMeta: metav1.ObjectMeta{Name: "kube-proxy"}, Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "kube-proxy", Image: "kube-proxy:v1"}}}}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "unbounded-net-kube-proxy-test", Labels: map[string]string{"app.kubernetes.io/name": managedKubeProxyAppName}}, Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "kube-proxy", Image: "kube-proxy:v1"}}}}}},
	}

	got := providerKubeProxyDaemonSets(dsList)
	if len(got) != 1 || got[0].Name != "kube-proxy" {
		t.Fatalf("providerKubeProxyDaemonSets() = %#v, want only kube-proxy", got)
	}
}

func TestSiteKubeProxyClusterCIDR(t *testing.T) {
	falseValue := false
	site := unboundedv1alpha3.Site{Spec: unboundedv1alpha3.SiteSpec{PodCidrAssignments: []unboundednetv1alpha1.PodCidrAssignment{
		{AssignmentEnabled: &falseValue, CidrBlocks: []string{"10.99.0.0/16"}},
		{CidrBlocks: []string{"100.125.0.0/16", "fd00:1::/64"}},
	}}}

	got, ok := siteKubeProxyClusterCIDR(site)
	if !ok || got != "100.125.0.0/16,fd00:1::/64" {
		t.Fatalf("siteKubeProxyClusterCIDR() = %q,%v", got, ok)
	}

	site.Spec.ManageCniPlugin = new(false)
	if got, ok := siteKubeProxyClusterCIDR(site); !ok || got != "100.125.0.0/16,fd00:1::/64" {
		t.Fatalf("unmanaged omitted assignment lost cluster CIDRs: %q, %v", got, ok)
	}

	site.Spec.PodCidrAssignments[1].AssignmentEnabled = new(true)
	if got, ok := siteKubeProxyClusterCIDR(site); !ok || got != "100.125.0.0/16,fd00:1::/64" {
		t.Fatalf("explicit unmanaged assignment lost cluster CIDRs: %q, %v", got, ok)
	}
}

func TestEnsureDaemonSetForUnmanagedSite(t *testing.T) {
	site := unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "external"},
		Spec: unboundedv1alpha3.SiteSpec{
			ManageCniPlugin: new(false),
			PodCidrAssignments: []unboundednetv1alpha1.PodCidrAssignment{{
				CidrBlocks: []string{"10.244.0.0/16", "fd00:244::/48"},
			}},
		},
	}
	clientset := k8sfake.NewClientset()

	c := &ManagedKubeProxyController{
		clientset: clientset,
		options:   ManagedKubeProxyOptions{Namespace: "unbounded-net", Image: "kube-proxy:v1"},
	}
	if err := c.ensureDaemonSet(t.Context(), site); err != nil {
		t.Fatal(err)
	}

	ds, err := clientset.AppsV1().DaemonSets("unbounded-net").Get(t.Context(), managedKubeProxyDaemonSetName(site.Name), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("unmanaged Site has no kube-proxy DaemonSet: %v", err)
	}

	if got := ds.Spec.Template.Spec.Containers[0].Command[3]; got != "--cluster-cidr=10.244.0.0/16,fd00:244::/48" {
		t.Fatalf("unexpected cluster-cidr arg: %s", got)
	}
}

func TestDaemonSetForSite(t *testing.T) {
	c := &ManagedKubeProxyController{options: ManagedKubeProxyOptions{Namespace: "unbounded-net", Image: "kube-proxy:v1"}}
	ds := c.daemonSetForSite(unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "test"}}, "100.125.0.0/16")

	if ds.Name != "unbounded-net-kube-proxy-test" {
		t.Fatalf("unexpected daemonset name: %s", ds.Name)
	}

	if ds.Spec.Template.Spec.NodeSelector[ManagedKubeProxyNodeLabelKey] != ManagedKubeProxyNodeLabelValue {
		t.Fatalf("missing managed kube-proxy selector: %#v", ds.Spec.Template.Spec.NodeSelector)
	}

	if ds.Spec.Template.Spec.NodeSelector[canonicalSiteLabelKey] != "test" {
		t.Fatalf("missing site selector: %#v", ds.Spec.Template.Spec.NodeSelector)
	}

	if got := ds.Spec.Template.Spec.Containers[0].Command[3]; got != "--cluster-cidr=100.125.0.0/16" {
		t.Fatalf("unexpected cluster-cidr arg: %s", got)
	}
}

func nodeWithLabels(labels map[string]string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: labels}}
}

func TestEnsureDaemonSetCacheStableAvoidsClientCalls(t *testing.T) {
	t.Parallel()

	site := kubeProxyTestSite()
	c, clientset := newKubeProxyTestController(t, nil, nil)
	want := c.daemonSetForSite(site, "100.125.0.0/16")
	addDaemonSetToLister(t, c, want)

	if err := c.ensureDaemonSet(t.Context(), site); err != nil {
		t.Fatalf("ensureDaemonSet: %v", err)
	}

	if actions := clientset.Actions(); len(actions) != 0 {
		t.Fatalf("client actions = %v, want none", actionVerbs(actions))
	}
}

func TestEnsureDaemonSetNormalizesServerDefaults(t *testing.T) {
	t.Parallel()

	site := kubeProxyTestSite()
	c, clientset := newKubeProxyTestController(t, nil, nil)
	serverObject := c.daemonSetForSite(site, "100.125.0.0/16")
	applyManagedKubeProxyServerDefaults(serverObject)
	addDaemonSetToLister(t, c, serverObject)

	if err := c.ensureDaemonSet(t.Context(), site); err != nil {
		t.Fatalf("ensureDaemonSet: %v", err)
	}

	if actions := clientset.Actions(); len(actions) != 0 {
		t.Fatalf("client actions = %v, want none", actionVerbs(actions))
	}
}

func TestEnsureDaemonSetUpdatesGenuineDrift(t *testing.T) {
	t.Parallel()

	site := kubeProxyTestSite()
	c, clientset := newKubeProxyTestController(t, nil, nil)
	drifted := c.daemonSetForSite(site, "100.125.0.0/16")
	drifted.Spec.Template.Spec.Containers[0].Image = "kube-proxy:old"
	addDaemonSetToLister(t, c, drifted)

	if _, err := clientset.AppsV1().DaemonSets("unbounded-net").Create(t.Context(), drifted.DeepCopy(), metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed live daemonset: %v", err)
	}

	clientset.ClearActions()

	if err := c.ensureDaemonSet(t.Context(), site); err != nil {
		t.Fatalf("ensureDaemonSet: %v", err)
	}

	if got, want := actionVerbs(clientset.Actions()), []string{"get", "update"}; !slices.Equal(got, want) {
		t.Fatalf("client actions = %v, want %v", got, want)
	}

	got, err := clientset.AppsV1().DaemonSets("unbounded-net").Get(t.Context(), drifted.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get updated daemonset: %v", err)
	}

	if got.Spec.Template.Spec.Containers[0].Image != "kube-proxy:v1" {
		t.Fatalf("updated image = %q, want kube-proxy:v1", got.Spec.Template.Spec.Containers[0].Image)
	}
}

func TestEnsureDaemonSetRepairsDisabledServiceLinks(t *testing.T) {
	t.Parallel()

	site := kubeProxyTestSite()
	c, clientset := newKubeProxyTestController(t, nil, nil)
	drifted := c.daemonSetForSite(site, "100.125.0.0/16")
	drifted.Spec.Template.Spec.EnableServiceLinks = boolPtr(false)
	addDaemonSetToLister(t, c, drifted)

	if _, err := clientset.AppsV1().DaemonSets("unbounded-net").Create(t.Context(), drifted.DeepCopy(), metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed live daemonset: %v", err)
	}

	clientset.ClearActions()

	if err := c.ensureDaemonSet(t.Context(), site); err != nil {
		t.Fatalf("ensureDaemonSet: %v", err)
	}

	if got, want := actionVerbs(clientset.Actions()), []string{"get", "update"}; !slices.Equal(got, want) {
		t.Fatalf("client actions = %v, want %v", got, want)
	}
}

func TestEnsureDaemonSetRepairsOwnedLabelDrift(t *testing.T) {
	t.Parallel()

	site := kubeProxyTestSite()
	c, clientset := newKubeProxyTestController(t, nil, nil)
	drifted := c.daemonSetForSite(site, "100.125.0.0/16")
	delete(drifted.Labels, "app.kubernetes.io/component")
	drifted.Labels["example.com/preserved"] = "true"
	addDaemonSetToLister(t, c, drifted)

	if _, err := clientset.AppsV1().DaemonSets("unbounded-net").Create(t.Context(), drifted.DeepCopy(), metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed live daemonset: %v", err)
	}

	clientset.ClearActions()

	if err := c.ensureDaemonSet(t.Context(), site); err != nil {
		t.Fatalf("ensureDaemonSet: %v", err)
	}

	got, err := clientset.AppsV1().DaemonSets("unbounded-net").Get(t.Context(), drifted.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get updated daemonset: %v", err)
	}

	if got.Labels["app.kubernetes.io/component"] != "kube-proxy" {
		t.Fatalf("owned labels not repaired: %#v", got.Labels)
	}

	if got.Labels["example.com/preserved"] != "true" {
		t.Fatalf("unowned label not preserved: %#v", got.Labels)
	}
}

func TestEnsureDaemonSetStaleCacheLiveNoOp(t *testing.T) {
	t.Parallel()

	site := kubeProxyTestSite()
	c, clientset := newKubeProxyTestController(t, nil, nil)
	stale := c.daemonSetForSite(site, "100.125.0.0/16")
	stale.Spec.Template.Spec.Containers[0].Image = "kube-proxy:old"
	addDaemonSetToLister(t, c, stale)

	live := c.daemonSetForSite(site, "100.125.0.0/16")
	if _, err := clientset.AppsV1().DaemonSets("unbounded-net").Create(t.Context(), live, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed live daemonset: %v", err)
	}

	clientset.ClearActions()

	if err := c.ensureDaemonSet(t.Context(), site); err != nil {
		t.Fatalf("ensureDaemonSet: %v", err)
	}

	if got, want := actionVerbs(clientset.Actions()), []string{"get"}; !slices.Equal(got, want) {
		t.Fatalf("client actions = %v, want %v", got, want)
	}
}

func TestEnsureDaemonSetReturnsLiveGetError(t *testing.T) {
	t.Parallel()

	site := kubeProxyTestSite()
	c, clientset := newKubeProxyTestController(t, nil, nil)
	drifted := c.daemonSetForSite(site, "100.125.0.0/16")
	drifted.Spec.Template.Spec.Containers[0].Image = "kube-proxy:old"
	addDaemonSetToLister(t, c, drifted)

	wantErr := errors.New("API unavailable")

	clientset.PrependReactor("get", "daemonsets", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, wantErr
	})

	err := c.ensureDaemonSet(t.Context(), site)
	if !errors.Is(err, wantErr) {
		t.Fatalf("ensureDaemonSet error = %v, want %v", err, wantErr)
	}

	if got, want := actionVerbs(clientset.Actions()), []string{"get"}; !slices.Equal(got, want) {
		t.Fatalf("client actions = %v, want %v", got, want)
	}
}

func TestEnsureDaemonSetReturnsUpdateConflict(t *testing.T) {
	t.Parallel()

	site := kubeProxyTestSite()
	templateController, _ := newKubeProxyTestController(t, nil, nil)
	cached := templateController.daemonSetForSite(site, "100.125.0.0/16")
	cached.ResourceVersion = "6"
	cached.Spec.Template.Spec.Containers[0].Image = "kube-proxy:old"
	live := cached.DeepCopy()
	live.ResourceVersion = "7"
	c, clientset := newKubeProxyTestController(t, cached, live)
	clientset.PrependReactor("update", "daemonsets", func(action clienttesting.Action) (bool, runtime.Object, error) {
		updateAction, ok := action.(clienttesting.UpdateAction)
		if !ok {
			t.Fatalf("action type = %T, want UpdateAction", action)
		}

		updated, ok := updateAction.GetObject().(*appsv1.DaemonSet)
		if !ok {
			t.Fatalf("updated object type = %T, want *DaemonSet", updateAction.GetObject())
		}

		if updated.ResourceVersion != live.ResourceVersion {
			t.Fatalf("updated resource version = %q, want live %q", updated.ResourceVersion, live.ResourceVersion)
		}

		return true, nil, apierrors.NewConflict(
			schema.GroupResource{Group: appsv1.GroupName, Resource: "daemonsets"},
			live.Name,
			errors.New("resource version changed"),
		)
	})

	err := c.ensureDaemonSet(t.Context(), site)
	if !apierrors.IsConflict(err) {
		t.Fatalf("ensureDaemonSet error = %v, want conflict", err)
	}

	if got, want := actionVerbs(clientset.Actions()), []string{"get", "update"}; !slices.Equal(got, want) {
		t.Fatalf("client actions = %v, want %v", got, want)
	}
}

func TestEnsureDaemonSetCreatesAfterDeletion(t *testing.T) {
	t.Parallel()

	site := kubeProxyTestSite()
	c, clientset := newKubeProxyTestController(t, nil, nil)

	if err := c.ensureDaemonSet(t.Context(), site); err != nil {
		t.Fatalf("ensureDaemonSet: %v", err)
	}

	if got, want := actionVerbs(clientset.Actions()), []string{"get", "create"}; !slices.Equal(got, want) {
		t.Fatalf("client actions = %v, want %v", got, want)
	}
}

// TestEnsureDaemonSetRecreatesOnSelectorChange guards the automated migration of
// the managed kube-proxy DaemonSet from the deprecated site label in its
// (immutable) selector to the canonical one.
func TestEnsureDaemonSetRecreatesOnSelectorChange(t *testing.T) {
	site := unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec: unboundedv1alpha3.SiteSpec{PodCidrAssignments: []unboundednetv1alpha1.PodCidrAssignment{
			{CidrBlocks: []string{"100.125.0.0/16"}},
		}},
	}

	// Seed an existing DaemonSet built with the deprecated site label in its
	// selector (as a pre-rename controller would have created it).
	deprecatedSelector := map[string]string{"app.kubernetes.io/name": managedKubeProxyAppName, unboundednetv1alpha1.SiteLabelKey: "test"}
	old := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: managedKubeProxyDaemonSetName("test"), Namespace: "unbounded-net"},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: deprecatedSelector},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: deprecatedSelector}},
		},
	}

	c, clientset := newKubeProxyTestController(t, old, old)

	if err := c.ensureDaemonSet(t.Context(), site); err != nil {
		t.Fatalf("ensureDaemonSet: %v", err)
	}

	if got, want := actionVerbs(clientset.Actions()), []string{"get", "delete", "create"}; !slices.Equal(got, want) {
		t.Fatalf("client actions = %v, want %v", got, want)
	}

	got, err := clientset.AppsV1().DaemonSets("unbounded-net").Get(t.Context(), managedKubeProxyDaemonSetName("test"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get daemonset: %v", err)
	}

	if got.Spec.Selector.MatchLabels[canonicalSiteLabelKey] != "test" {
		t.Fatalf("selector not migrated to canonical label: %#v", got.Spec.Selector.MatchLabels)
	}

	if _, ok := got.Spec.Selector.MatchLabels[unboundednetv1alpha1.SiteLabelKey]; ok {
		t.Fatalf("selector still carries deprecated label: %#v", got.Spec.Selector.MatchLabels)
	}
}

func kubeProxyTestSite() unboundedv1alpha3.Site {
	return unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec: unboundedv1alpha3.SiteSpec{PodCidrAssignments: []unboundednetv1alpha1.PodCidrAssignment{
			{CidrBlocks: []string{"100.125.0.0/16"}},
		}},
	}
}

func applyManagedKubeProxyServerDefaults(ds *appsv1.DaemonSet) {
	maxSurge := intstr.FromInt32(0)
	ds.Spec.UpdateStrategy.RollingUpdate.MaxSurge = &maxSurge

	podSpec := &ds.Spec.Template.Spec
	podSpec.DeprecatedServiceAccount = podSpec.ServiceAccountName
	podSpec.DNSPolicy = corev1.DNSClusterFirst
	podSpec.RestartPolicy = corev1.RestartPolicyAlways
	podSpec.SecurityContext = &corev1.PodSecurityContext{}
	terminationGracePeriodSeconds := int64(corev1.DefaultTerminationGracePeriodSeconds)
	podSpec.TerminationGracePeriodSeconds = &terminationGracePeriodSeconds
	podSpec.SchedulerName = corev1.DefaultSchedulerName
	podSpec.EnableServiceLinks = boolPtr(corev1.DefaultEnableServiceLinks)

	for i := range podSpec.InitContainers {
		podSpec.InitContainers[i].TerminationMessagePath = corev1.TerminationMessagePathDefault
		podSpec.InitContainers[i].TerminationMessagePolicy = corev1.TerminationMessageReadFile
	}

	for i := range podSpec.Containers {
		podSpec.Containers[i].TerminationMessagePath = corev1.TerminationMessagePathDefault
		podSpec.Containers[i].TerminationMessagePolicy = corev1.TerminationMessageReadFile
	}
}

func newKubeProxyTestController(
	t *testing.T,
	cached *appsv1.DaemonSet,
	live *appsv1.DaemonSet,
) (*ManagedKubeProxyController, *k8sfake.Clientset) {
	t.Helper()

	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if cached != nil {
		if err := indexer.Add(cached.DeepCopy()); err != nil {
			t.Fatalf("add cached daemonset: %v", err)
		}
	}

	var objects []runtime.Object
	if live != nil {
		objects = append(objects, live.DeepCopy())
	}

	clientset := k8sfake.NewClientset(objects...)

	return &ManagedKubeProxyController{
		clientset: clientset,
		options:   ManagedKubeProxyOptions{Namespace: "unbounded-net", Image: "kube-proxy:v1"},
		dsLister:  appsv1listers.NewDaemonSetLister(indexer),
	}, clientset
}

func addDaemonSetToLister(t *testing.T, c *ManagedKubeProxyController, ds *appsv1.DaemonSet) {
	t.Helper()

	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := indexer.Add(ds.DeepCopy()); err != nil {
		t.Fatalf("add cached daemonset: %v", err)
	}

	c.dsLister = appsv1listers.NewDaemonSetLister(indexer)
}

func actionVerbs(actions []clienttesting.Action) []string {
	verbs := make([]string, 0, len(actions))
	for _, action := range actions {
		verbs = append(verbs, action.GetVerb())
	}

	return verbs
}
