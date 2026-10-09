// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

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
			name: "status only",
			old:  oldNode,
			new: func() *corev1.Node {
				node := oldNode.DeepCopy()
				node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}

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
			t.Parallel()

			if got := managedKubeProxyNodeUpdateAffectsReconcile(tt.old, tt.new); got != tt.want {
				t.Fatalf("managedKubeProxyNodeUpdateAffectsReconcile() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestManagedKubeProxyDaemonSetUpdateAffectsReconcile(t *testing.T) {
	t.Parallel()

	oldDaemonSet := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "kube-proxy",
			ResourceVersion: "1",
			Generation:      1,
			Labels:          map[string]string{"app.kubernetes.io/name": "kube-proxy"},
		},
		Spec: appsv1.DaemonSetSpec{
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "kube-proxy", Image: "kube-proxy:v1"}}}},
		},
	}
	statusChanged := oldDaemonSet.DeepCopy()
	statusChanged.ResourceVersion = "2"
	statusChanged.Status.NumberReady = 1
	specChanged := oldDaemonSet.DeepCopy()
	specChanged.Spec.Template.Spec.Containers[0].Image = "kube-proxy:v2"
	generationChanged := oldDaemonSet.DeepCopy()
	generationChanged.Generation++
	labelChanged := oldDaemonSet.DeepCopy()
	labelChanged.Labels["app.kubernetes.io/name"] = managedKubeProxyAppName

	tests := []struct {
		name string
		old  any
		new  any
		want bool
	}{
		{name: "status and resource version only", old: oldDaemonSet, new: statusChanged},
		{name: "spec changed", old: oldDaemonSet, new: specChanged, want: true},
		{name: "generation changed", old: oldDaemonSet, new: generationChanged, want: true},
		{name: "classification label changed", old: oldDaemonSet, new: labelChanged, want: true},
		{name: "unexpected object", old: oldDaemonSet, new: &corev1.Pod{}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := managedKubeProxyDaemonSetUpdateAffectsReconcile(tt.old, tt.new); got != tt.want {
				t.Fatalf("managedKubeProxyDaemonSetUpdateAffectsReconcile() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestManagedKubeProxySiteUpdateAffectsReconcile(t *testing.T) {
	t.Parallel()

	oldSite := siteUnstructured(t, managedKubeProxyTestSite())
	statusOnly := oldSite.DeepCopy()
	statusOnly.SetResourceVersion("2")
	statusOnly.Object["status"] = map[string]any{"state": "Ready"}
	specChanged := managedKubeProxyTestSite()
	specChanged.Spec.PodCidrAssignments[0].CidrBlocks = []string{"10.0.0.0/16"}

	tests := []struct {
		name string
		old  any
		new  any
		want bool
	}{
		{name: "status and resource version only", old: oldSite, new: statusOnly},
		{name: "spec changed", old: oldSite, new: siteUnstructured(t, specChanged), want: true},
		{name: "unexpected object", old: oldSite, new: &corev1.Pod{}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := managedKubeProxySiteUpdateAffectsReconcile(tt.old, tt.new); got != tt.want {
				t.Fatalf("managedKubeProxySiteUpdateAffectsReconcile() = %v, want %v", got, tt.want)
			}
		})
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

func TestEnsureDaemonSetRecreatesOnSelectorChange(t *testing.T) {
	t.Parallel()

	site := managedKubeProxyTestSite()
	c := &ManagedKubeProxyController{options: ManagedKubeProxyOptions{Namespace: "unbounded-net", Image: "kube-proxy:v1"}}
	old := c.daemonSetForSite(site, "100.125.0.0/16")
	old.Spec.Selector.MatchExpressions = []metav1.LabelSelectorRequirement{{
		Key:      "legacy-site",
		Operator: metav1.LabelSelectorOpExists,
	}}

	clientset := k8sfake.NewClientset(old)
	c.clientset = clientset

	if err := c.ensureDaemonSet(t.Context(), site); err != nil {
		t.Fatalf("ensureDaemonSet: %v", err)
	}

	got, err := clientset.AppsV1().DaemonSets("unbounded-net").Get(t.Context(), managedKubeProxyDaemonSetName("test"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get daemonset: %v", err)
	}

	if len(got.Spec.Selector.MatchExpressions) != 0 {
		t.Fatalf("selector was not recreated: %#v", got.Spec.Selector)
	}
}

func TestEnsureDaemonSetIgnoresAPIServerDefaults(t *testing.T) {
	t.Parallel()

	site := managedKubeProxyTestSite()
	c := &ManagedKubeProxyController{options: ManagedKubeProxyOptions{Namespace: "unbounded-net", Image: "kube-proxy:v1"}}
	existing := c.daemonSetForSite(site, "100.125.0.0/16")
	maxSurge := intstr.FromInt32(0)
	existing.Spec.UpdateStrategy.RollingUpdate.MaxSurge = &maxSurge
	existing.Spec.Template.Spec.DNSPolicy = corev1.DNSClusterFirst
	existing.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyAlways
	existing.Spec.Template.Spec.SecurityContext = &corev1.PodSecurityContext{}
	terminationGracePeriodSeconds := int64(corev1.DefaultTerminationGracePeriodSeconds)
	existing.Spec.Template.Spec.TerminationGracePeriodSeconds = &terminationGracePeriodSeconds
	existing.Spec.Template.Spec.SchedulerName = corev1.DefaultSchedulerName

	for i := range existing.Spec.Template.Spec.InitContainers {
		existing.Spec.Template.Spec.InitContainers[i].TerminationMessagePath = corev1.TerminationMessagePathDefault
		existing.Spec.Template.Spec.InitContainers[i].TerminationMessagePolicy = corev1.TerminationMessageReadFile
	}

	for i := range existing.Spec.Template.Spec.Containers {
		existing.Spec.Template.Spec.Containers[i].TerminationMessagePath = corev1.TerminationMessagePathDefault
		existing.Spec.Template.Spec.Containers[i].TerminationMessagePolicy = corev1.TerminationMessageReadFile
	}

	clientset := k8sfake.NewClientset(existing)
	c.clientset = clientset

	if err := c.ensureDaemonSet(t.Context(), site); err != nil {
		t.Fatalf("ensureDaemonSet: %v", err)
	}

	for _, action := range clientset.Actions() {
		if action.Matches("update", "daemonsets") {
			t.Fatalf("converged daemonset was updated: %#v", action)
		}
	}
}

func TestEnsureDaemonSetBecomesQuietAfterCorrectingTemplate(t *testing.T) {
	t.Parallel()

	site := managedKubeProxyTestSite()
	c := &ManagedKubeProxyController{options: ManagedKubeProxyOptions{Namespace: "unbounded-net", Image: "kube-proxy:v2"}}
	existing := c.daemonSetForSite(site, "100.125.0.0/16")
	existing.Spec.Template.Spec.Containers[0].Image = "kube-proxy:v1"
	clientset := k8sfake.NewClientset(existing)
	c.clientset = clientset

	if err := c.ensureDaemonSet(t.Context(), site); err != nil {
		t.Fatalf("first ensureDaemonSet: %v", err)
	}

	if err := c.ensureDaemonSet(t.Context(), site); err != nil {
		t.Fatalf("second ensureDaemonSet: %v", err)
	}

	updates := 0

	for _, action := range clientset.Actions() {
		if action.Matches("update", "daemonsets") {
			updates++
		}
	}

	if updates != 1 {
		t.Fatalf("daemonset update count = %d, want 1", updates)
	}
}

func TestEnsureDaemonSetCorrectsUpdateStrategy(t *testing.T) {
	t.Parallel()

	site := managedKubeProxyTestSite()
	c := &ManagedKubeProxyController{options: ManagedKubeProxyOptions{Namespace: "unbounded-net", Image: "kube-proxy:v1"}}
	existing := c.daemonSetForSite(site, "100.125.0.0/16")
	maxUnavailable := intstr.FromInt32(2)
	existing.Spec.UpdateStrategy.RollingUpdate.MaxUnavailable = &maxUnavailable
	clientset := k8sfake.NewClientset(existing)
	c.clientset = clientset

	if err := c.ensureDaemonSet(t.Context(), site); err != nil {
		t.Fatalf("ensureDaemonSet: %v", err)
	}

	got, err := clientset.AppsV1().DaemonSets("unbounded-net").Get(t.Context(), existing.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get daemonset: %v", err)
	}

	if got.Spec.UpdateStrategy.RollingUpdate.MaxUnavailable.IntValue() != 1 {
		t.Fatalf("maxUnavailable = %s, want 1", got.Spec.UpdateStrategy.RollingUpdate.MaxUnavailable.String())
	}
}

func TestEnsureDaemonSetRetriesConflict(t *testing.T) {
	t.Parallel()

	site := managedKubeProxyTestSite()
	c := &ManagedKubeProxyController{options: ManagedKubeProxyOptions{Namespace: "unbounded-net", Image: "kube-proxy:v2"}}
	existing := c.daemonSetForSite(site, "100.125.0.0/16")
	existing.Spec.Template.Spec.Containers[0].Image = "kube-proxy:v1"
	clientset := k8sfake.NewClientset(existing)
	c.clientset = clientset
	updates := 0

	clientset.PrependReactor("update", "daemonsets", func(k8stesting.Action) (bool, runtime.Object, error) {
		updates++
		if updates == 1 {
			return true, nil, apierrors.NewConflict(appsv1.Resource("daemonsets"), existing.Name, errors.New("test conflict"))
		}

		return false, nil, nil
	})

	if err := c.ensureDaemonSet(t.Context(), site); err != nil {
		t.Fatalf("ensureDaemonSet: %v", err)
	}

	gets := 0

	for _, action := range clientset.Actions() {
		if action.Matches("get", "daemonsets") {
			gets++
		}
	}

	if updates != 2 || gets != 2 {
		t.Fatalf("daemonset attempts = %d updates and %d gets, want 2 of each", updates, gets)
	}
}

func managedKubeProxyTestSite() unboundedv1alpha3.Site {
	return unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec: unboundedv1alpha3.SiteSpec{PodCidrAssignments: []unboundednetv1alpha1.PodCidrAssignment{
			{CidrBlocks: []string{"100.125.0.0/16"}},
		}},
	}
}
