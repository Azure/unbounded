// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package playpen

import (
	"context"
	"net"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"

	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
)

func TestClientNode(t *testing.T) {
	cfg := ClientConfig{Namespace: "pxe-outside", Site: "outside"}
	node := clientNode(cfg, net.ParseIP("172.31.1.2"), "10.250.1.0/24", "public-key", "owner")

	if node.Name != clientNodeName(cfg.Namespace) || !node.Spec.Unschedulable {
		t.Fatalf("node identity = %#v", node)
	}

	if node.Spec.PodCIDR != "10.250.1.0/24" || len(node.Spec.PodCIDRs) != 1 {
		t.Fatalf("node pod CIDRs = %#v", node.Spec)
	}

	if node.Labels[unboundednetv1alpha1.SiteLabelKey] != "outside" || node.Annotations[clientWireGuardPubKey] != "public-key" {
		t.Fatalf("node network metadata = labels %#v, annotations %#v", node.Labels, node.Annotations)
	}

	if len(node.Status.Addresses) != 1 || node.Status.Addresses[0].Address != "172.31.1.2" {
		t.Fatalf("node addresses = %#v", node.Status.Addresses)
	}
}

func TestClientWireGuardHostLinkName(t *testing.T) {
	name := clientWireGuardHostLinkName("pxe-outside")
	if len(name) > maxInterfaceNameLen {
		t.Fatalf("WireGuard host link name %q exceeds Linux limit", name)
	}

	if name == clientWireGuardHostLinkName("pxe-other") {
		t.Fatalf("different namespaces produced the same WireGuard link name %q", name)
	}
}

func TestSelectClientGateway(t *testing.T) {
	gateways := []unboundednetv1alpha1.GatewayNodeInfo{
		{Name: "z-ipv6", ExternalIPs: []string{"2001:db8::2"}, WireGuardPublicKey: "ipv6-key"},
		{Name: "b", ExternalIPs: []string{"203.0.113.2"}, WireGuardPublicKey: "b-key"},
		{Name: "a", ExternalIPs: []string{"203.0.113.1"}, WireGuardPublicKey: "a-key"},
	}

	gateway, err := selectClientGateway(gateways, net.ParseIP("172.30.1.2"))
	if err != nil {
		t.Fatalf("selectClientGateway() error = %v", err)
	}

	if gateway.Name != "a" {
		t.Fatalf("gateway = %q, want deterministic first gateway a", gateway.Name)
	}
}

func TestClientSiteAssignedToPool(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := unboundednetv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	disabled := false
	client := dynamicfake.NewSimpleDynamicClient(scheme,
		&unboundednetv1alpha1.SiteGatewayPoolAssignment{
			TypeMeta:   metav1.TypeMeta{APIVersion: unboundednetv1alpha1.GroupVersion.String(), Kind: "SiteGatewayPoolAssignment"},
			ObjectMeta: metav1.ObjectMeta{Name: "disabled"},
			Spec:       unboundednetv1alpha1.SiteGatewayPoolAssignmentSpec{Enabled: &disabled, Sites: []string{"outside"}, GatewayPools: []string{"public"}},
		},
		&unboundednetv1alpha1.SiteGatewayPoolAssignment{
			TypeMeta:   metav1.TypeMeta{APIVersion: unboundednetv1alpha1.GroupVersion.String(), Kind: "SiteGatewayPoolAssignment"},
			ObjectMeta: metav1.ObjectMeta{Name: "enabled"},
			Spec:       unboundednetv1alpha1.SiteGatewayPoolAssignmentSpec{Sites: []string{"outside"}, GatewayPools: []string{"public"}},
		},
	)

	assigned, err := clientSiteAssignedToPool(context.Background(), client, "outside", "public")
	if err != nil {
		t.Fatalf("clientSiteAssignedToPool() error = %v", err)
	}

	if !assigned {
		t.Fatal("expected site to be assigned to pool")
	}

	assigned, err = clientSiteAssignedToPool(context.Background(), client, "other", "public")
	if err != nil {
		t.Fatalf("clientSiteAssignedToPool() error = %v", err)
	}

	if assigned {
		t.Fatal("unexpected assignment for other site")
	}
}

func TestClientNodeAddressUsesFirstUsableIP(t *testing.T) {
	address, network, err := clientNodeAddress("10.250.1.0/24")
	if err != nil {
		t.Fatalf("clientNodeAddress() error = %v", err)
	}

	if address.String() != "10.250.1.1" || network.String() != "10.250.1.0/24" {
		t.Fatalf("address = %s, network = %s", address, network)
	}
}

func TestClientNodeAddressHostPrefix(t *testing.T) {
	address, network, err := clientNodeAddress("10.250.1.9/32")
	if err != nil {
		t.Fatalf("clientNodeAddress() error = %v", err)
	}

	if address.String() != "10.250.1.9" || network.String() != "10.250.1.9/32" {
		t.Fatalf("address = %s, network = %s", address, network)
	}
}

func TestValidateClientNodeCIDR(t *testing.T) {
	site := unboundednetv1alpha1.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "outside"},
		Spec: unboundednetv1alpha1.SiteSpec{PodCidrAssignments: []unboundednetv1alpha1.PodCidrAssignment{{
			CidrBlocks: []string{"10.250.0.0/16"},
		}}},
	}
	client := kubefake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "existing"},
		Spec:       corev1.NodeSpec{PodCIDR: "10.250.2.0/24"},
	})

	_, valid, _ := net.ParseCIDR("10.250.1.0/24")
	if err := validateClientNodeCIDR(context.Background(), client, site, valid, net.ParseIP("10.244.0.8"), "192.168.100.1/24"); err != nil {
		t.Fatalf("validateClientNodeCIDR() error = %v", err)
	}

	_, overlapping, _ := net.ParseCIDR("10.250.2.0/24")
	if err := validateClientNodeCIDR(context.Background(), client, site, overlapping, net.ParseIP("10.244.0.8"), "192.168.100.1/24"); err == nil {
		t.Fatal("validateClientNodeCIDR() accepted overlap")
	}

	_, wrongSize, _ := net.ParseCIDR("10.250.4.0/23")
	if err := validateClientNodeCIDR(context.Background(), client, site, wrongSize, net.ParseIP("10.244.0.8"), "192.168.100.1/24"); err == nil {
		t.Fatal("validateClientNodeCIDR() accepted wrong block size")
	}
}
