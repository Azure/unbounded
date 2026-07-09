// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package network

import (
	"context"
	"testing"
	"time"

	netv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Azure/unbounded/internal/playpen/labels"
)

func TestPeersFromExternalGatewayPool(t *testing.T) {
	pool := gatewayPool("pool-a", "External", []string{"10.250.0.0/16"}, []netv1alpha1.GatewayNodeInfo{{
		Name:                 "gw-a",
		SiteName:             "site-a",
		WireGuardPublicKey:   "pub",
		ExternalIPs:          []string{"203.0.113.10"},
		InternalIPs:          []string{"10.0.0.10"},
		GatewayWireguardPort: 51821,
		PodCIDRs:             []string{"10.1.0.0/24"},
	}})

	peers := peersFromPool(pool)
	if len(peers) != 1 {
		t.Fatalf("peer count = %d, want 1", len(peers))
	}
	if got := peers[0].Endpoints[0]; got != "203.0.113.10:51820" {
		t.Fatalf("endpoint = %q, want external mesh endpoint", got)
	}
	if got := peers[0].InternalIPs[0]; got != "10.0.0.10" {
		t.Fatalf("internal IP = %q, want gateway internal IP", got)
	}
	if got := peers[0].RoutedCIDRs[0]; got != "10.250.0.0/16" {
		t.Fatalf("routed CIDR = %q, want pool routed CIDR", got)
	}
	if requiresEndpoint(pool) {
		t.Fatalf("external pool with endpoint should not require playpen endpoint")
	}
}

func TestInternalGatewayPoolRequiresEndpoint(t *testing.T) {
	pool := gatewayPool("pool-a", "Internal", nil, []netv1alpha1.GatewayNodeInfo{{
		Name:               "gw-a",
		WireGuardPublicKey: "pub",
		InternalIPs:        []string{"10.0.0.10"},
	}})

	if !requiresEndpoint(pool) {
		t.Fatalf("internal pool should require playpen endpoint")
	}
}

func TestAllocateCreatesAssignmentForAutoSelectedGatewayPool(t *testing.T) {
	ctx := context.Background()
	pool := gatewayPool("pool-a", "External", nil, []netv1alpha1.GatewayNodeInfo{{
		Name:                 "gw-a",
		SiteName:             "site-a",
		WireGuardPublicKey:   "pub",
		ExternalIPs:          []string{"203.0.113.10"},
		InternalIPs:          []string{"10.0.0.10"},
		GatewayWireguardPort: 51821,
		PodCIDRs:             []string{"10.1.0.0/24"},
	}})
	ctrlClient := newFakeClient(t, pool)
	mgr := NewManager(ctrlClient, Config{DefaultSite: "playpen"})

	alloc, err := mgr.Allocate(ctx, "pp-test", time.Now().Add(time.Hour), AllocateRequest{ClientWireGuardPublicKey: "clientpub", ClientInternalIP: "169.254.10.2"}, "10.241.1.0/24")
	if err != nil {
		t.Fatalf("Allocate() error = %v", err)
	}
	if alloc.GatewayPool != "pool-a" {
		t.Fatalf("gateway pool = %q, want pool-a", alloc.GatewayPool)
	}

	assignment := &netv1alpha1.SiteGatewayPoolAssignment{}
	if err := ctrlClient.Get(ctx, client.ObjectKey{Name: "playpen-pp-test"}, assignment); err != nil {
		t.Fatalf("get assignment: %v", err)
	}
	if len(assignment.Spec.GatewayPools) != 1 || assignment.Spec.GatewayPools[0] != "pool-a" {
		t.Fatalf("assignment gateway pools = %#v, want pool-a", assignment.Spec.GatewayPools)
	}
	if assignment.Spec.TunnelProtocol == nil || *assignment.Spec.TunnelProtocol != netv1alpha1.TunnelProtocolVXLAN {
		t.Fatalf("assignment tunnel protocol = %v, want VXLAN", assignment.Spec.TunnelProtocol)
	}
}

func TestAllocateEnsuresSharedSiteAndMergesCIDRs(t *testing.T) {
	ctx := context.Background()
	pool := gatewayPool("pool-a", "External", nil, []netv1alpha1.GatewayNodeInfo{{
		Name:               "gw-a",
		SiteName:           "site-a",
		WireGuardPublicKey: "pub",
		ExternalIPs:        []string{"203.0.113.10"},
		InternalIPs:        []string{"10.0.0.10"},
		PodCIDRs:           []string{"10.1.0.0/24"},
	}})
	existingSite := &netv1alpha1.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "playpen"},
		Spec:       netv1alpha1.SiteSpec{NodeCidrs: []string{"10.200.0.0/16"}},
	}
	ctrlClient := newFakeClient(t, existingSite, pool)
	mgr := NewManager(ctrlClient, Config{DefaultSite: "playpen", DefaultPodCIDRBase: "10.241.0.0/16"})

	_, err := mgr.Allocate(ctx, "pp-test", time.Now().Add(time.Hour), AllocateRequest{ClientWireGuardPublicKey: "clientpub", ClientInternalIP: "169.254.10.2"}, "10.241.9.0/24")
	if err != nil {
		t.Fatalf("Allocate() error = %v", err)
	}

	site := &netv1alpha1.Site{}
	if err := ctrlClient.Get(ctx, client.ObjectKey{Name: "playpen"}, site); err != nil {
		t.Fatalf("get site: %v", err)
	}
	want := map[string]bool{"10.200.0.0/16": true, "10.241.0.0/16": true, "10.241.9.0/24": true}
	if len(site.Spec.NodeCidrs) != len(want) {
		t.Fatalf("site nodeCidrs = %#v, want %#v", site.Spec.NodeCidrs, want)
	}
	for _, cidr := range site.Spec.NodeCidrs {
		if !want[cidr] {
			t.Fatalf("unexpected site nodeCidrs entry %q in %#v", cidr, site.Spec.NodeCidrs)
		}
	}
	if site.Spec.TunnelProtocol == nil || *site.Spec.TunnelProtocol != netv1alpha1.TunnelProtocolVXLAN {
		t.Fatalf("site tunnel protocol = %v, want VXLAN", site.Spec.TunnelProtocol)
	}
	if site.Spec.ManageCniPlugin == nil || *site.Spec.ManageCniPlugin {
		t.Fatalf("site manageCniPlugin = true, want false")
	}
	if len(site.Spec.PodCidrAssignments) != 1 {
		t.Fatalf("site podCidrAssignments = %#v, want one disabled assignment", site.Spec.PodCidrAssignments)
	}
	assignment := site.Spec.PodCidrAssignments[0]
	if assignment.AssignmentEnabled == nil || *assignment.AssignmentEnabled {
		t.Fatalf("site podCidrAssignment should be disabled: %#v", assignment)
	}
}

func TestDeleteExpiredDeletesAssignmentForExpiredNode(t *testing.T) {
	ctx := context.Background()
	allocationID := "pp-expired"
	ctrlClient := newFakeClient(t,
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name: "playpen-" + allocationID,
			Labels: map[string]string{
				labels.OwnedLabel:        "true",
				labels.AllocationIDLabel: allocationID,
			},
			Annotations: map[string]string{labels.ExpiresAtAnnotation: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)},
		}},
		&netv1alpha1.SiteGatewayPoolAssignment{ObjectMeta: metav1.ObjectMeta{Name: "playpen-" + allocationID}},
	)
	mgr := NewManager(ctrlClient, Config{})

	if err := mgr.DeleteExpired(ctx, time.Now().UTC()); err != nil {
		t.Fatalf("DeleteExpired() error = %v", err)
	}

	assignment := &netv1alpha1.SiteGatewayPoolAssignment{}
	err := ctrlClient.Get(ctx, client.ObjectKey{Name: "playpen-" + allocationID}, assignment)
	if err == nil {
		t.Fatalf("expired assignment still exists")
	}
}

func gatewayPool(name, poolType string, routedCIDRs []string, nodes []netv1alpha1.GatewayNodeInfo) *netv1alpha1.GatewayPool {
	return &netv1alpha1.GatewayPool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: netv1alpha1.GatewayPoolSpec{
			Type:        poolType,
			RoutedCidrs: routedCIDRs,
		},
		Status: netv1alpha1.GatewayPoolStatus{Nodes: nodes},
	}
}

func newFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := netv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add net scheme: %v", err)
	}

	return ctrlfake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1.Node{}).WithObjects(objs...).Build()
}
