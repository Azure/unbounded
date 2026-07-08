// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

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

	clientset := k8sfake.NewClientset(old)
	c := &ManagedKubeProxyController{clientset: clientset, options: ManagedKubeProxyOptions{Namespace: "unbounded-net", Image: "kube-proxy:v1"}}

	if err := c.ensureDaemonSet(t.Context(), site); err != nil {
		t.Fatalf("ensureDaemonSet: %v", err)
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
