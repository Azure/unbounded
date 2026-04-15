// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package ipallocator

import (
	"context"
	"net"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha3 "github.com/Azure/unbounded-kube/api/v1alpha3"
	"github.com/Azure/unbounded-kube/internal/metalman/indexing"
)

// --- helper functions for IP math ---

func TestSubnetMaskString(t *testing.T) {
	tests := []struct {
		cidr string
		want string
	}{
		{"192.168.1.0/24", "255.255.255.0"},
		{"10.0.0.0/16", "255.255.0.0"},
		{"172.16.0.0/12", "255.240.0.0"},
		{"10.0.0.0/8", "255.0.0.0"},
		{"10.0.0.0/28", "255.255.255.240"},
	}

	for _, tt := range tests {
		_, ipNet, err := net.ParseCIDR(tt.cidr)
		if err != nil {
			t.Fatal(err)
		}

		got := subnetMaskString(ipNet)
		if got != tt.want {
			t.Errorf("subnetMaskString(%s) = %s, want %s", tt.cidr, got, tt.want)
		}
	}
}

func TestFirstUsableIP(t *testing.T) {
	tests := []struct {
		cidr string
		want string
	}{
		{"192.168.1.0/24", "192.168.1.1"},
		{"10.0.0.0/16", "10.0.0.1"},
		{"172.20.0.0/16", "172.20.0.1"},
	}

	for _, tt := range tests {
		_, ipNet, err := net.ParseCIDR(tt.cidr)
		if err != nil {
			t.Fatal(err)
		}

		got := firstUsableIP(ipNet)
		if got.String() != tt.want {
			t.Errorf("firstUsableIP(%s) = %s, want %s", tt.cidr, got, tt.want)
		}
	}
}

func TestLastUsableIP(t *testing.T) {
	tests := []struct {
		cidr string
		want string
	}{
		{"192.168.1.0/24", "192.168.1.254"},
		{"10.0.0.0/16", "10.0.255.254"},
		{"10.0.0.0/28", "10.0.0.14"},
	}

	for _, tt := range tests {
		_, ipNet, err := net.ParseCIDR(tt.cidr)
		if err != nil {
			t.Fatal(err)
		}

		got := lastUsableIP(ipNet)
		if got.String() != tt.want {
			t.Errorf("lastUsableIP(%s) = %s, want %s", tt.cidr, got, tt.want)
		}
	}
}

func TestSubnetSize(t *testing.T) {
	tests := []struct {
		cidr string
		want uint32
	}{
		{"192.168.1.0/24", 253}, // 256 - network - broadcast - gateway
		{"10.0.0.0/16", 65533},  // 65536 - 3
		{"10.0.0.0/28", 13},     // 16 - 3
		{"10.0.0.0/30", 1},      // 4 - 3
		{"10.0.0.0/31", 0},      // 2 - too small
		{"10.0.0.0/32", 0},      // 1 - too small
	}

	for _, tt := range tests {
		_, ipNet, err := net.ParseCIDR(tt.cidr)
		if err != nil {
			t.Fatal(err)
		}

		got := subnetSize(ipNet)
		if got != tt.want {
			t.Errorf("subnetSize(%s) = %d, want %d", tt.cidr, got, tt.want)
		}
	}
}

func TestRandomAvailableIP(t *testing.T) {
	_, subnet, _ := net.ParseCIDR("192.168.1.0/24")

	t.Run("picks address in subnet", func(t *testing.T) {
		allocated := map[string]bool{}

		ip, err := RandomAvailableIP(subnet, allocated)
		if err != nil {
			t.Fatal(err)
		}

		if !subnet.Contains(ip) {
			t.Errorf("allocated IP %s not in subnet %s", ip, subnet)
		}

		// Must not be network, broadcast, or gateway.
		if ip.String() == "192.168.1.0" {
			t.Error("allocated IP is the network address")
		}

		if ip.String() == "192.168.1.255" {
			t.Error("allocated IP is the broadcast address")
		}

		if ip.String() == "192.168.1.1" {
			t.Error("allocated IP is the gateway address")
		}
	})

	t.Run("avoids allocated addresses", func(t *testing.T) {
		allocated := map[string]bool{
			"192.168.1.50": true,
			"192.168.1.51": true,
		}

		for range 100 {
			ip, err := RandomAvailableIP(subnet, allocated)
			if err != nil {
				t.Fatal(err)
			}

			if allocated[ip.String()] {
				t.Errorf("allocated IP %s that was already taken", ip)
			}
		}
	})

	t.Run("returns error when exhausted", func(t *testing.T) {
		_, small, _ := net.ParseCIDR("10.0.0.0/30")
		// /30 has 1 usable address (4 - network - broadcast - gateway = 1).
		allocated := map[string]bool{
			"10.0.0.2": true, // The only usable address.
		}

		_, err := RandomAvailableIP(small, allocated)
		if err == nil {
			t.Error("expected error when subnet is exhausted")
		}
	})

	t.Run("finds last remaining address", func(t *testing.T) {
		_, small, _ := net.ParseCIDR("10.0.0.0/30")
		allocated := map[string]bool{}

		ip, err := RandomAvailableIP(small, allocated)
		if err != nil {
			t.Fatal(err)
		}

		if ip.String() != "10.0.0.2" {
			t.Errorf("expected 10.0.0.2, got %s", ip)
		}
	})
}

func TestIPConversion(t *testing.T) {
	tests := []string{
		"192.168.1.1",
		"10.0.0.1",
		"255.255.255.255",
		"0.0.0.0",
	}

	for _, s := range tests {
		ip := net.ParseIP(s).To4()
		n := ipToUint32(ip)
		back := uint32ToIP(n)

		if !ip.Equal(back) {
			t.Errorf("round-trip failed: %s -> %d -> %s", ip, n, back)
		}
	}
}

// --- reconciler integration tests ---

func newFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := v1alpha3.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha3.Machine{}).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()
}

func makeSite(name string, nodeCidrs []string) *unstructured.Unstructured {
	site := &unstructured.Unstructured{}
	site.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "net.unbounded-kube.io",
		Version: "v1alpha1",
		Kind:    "Site",
	})
	site.SetName(name)
	site.Object["spec"] = map[string]interface{}{
		"nodeCidrs": func() []interface{} {
			out := make([]interface{}, len(nodeCidrs))
			for i, c := range nodeCidrs {
				out[i] = c
			}

			return out
		}(),
	}

	return site
}

func TestReconcileAllocatesIP(t *testing.T) {
	site := makeSite("test-site", []string{"192.168.10.0/24"})

	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-01",
			Labels: map[string]string{siteLabel: "test-site"},
		},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image: "test-image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{
					MAC: "aa:bb:cc:dd:ee:01",
				}},
			},
		},
	}

	scheme := runtime.NewScheme()
	if err := v1alpha3.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(machine, site).
		WithStatusSubresource(&v1alpha3.Machine{}).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	r := &Reconciler{Client: cl, APIReader: cl}

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "node-01"},
	})
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	// Re-read the Machine to check the allocated fields.
	var updated v1alpha3.Machine
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "node-01"}, &updated); err != nil {
		t.Fatal(err)
	}

	lease := updated.Spec.PXE.DHCPLeases[0]

	if lease.IPv4 == "" {
		t.Error("expected IPv4 to be allocated")
	}

	if lease.SubnetMask != "255.255.255.0" {
		t.Errorf("expected subnet mask 255.255.255.0, got %s", lease.SubnetMask)
	}

	if lease.Gateway != "192.168.10.1" {
		t.Errorf("expected gateway 192.168.10.1, got %s", lease.Gateway)
	}

	// Verify the allocated IP is in the subnet.
	_, subnet, _ := net.ParseCIDR("192.168.10.0/24")
	ip := net.ParseIP(lease.IPv4)

	if !subnet.Contains(ip) {
		t.Errorf("allocated IP %s not in subnet %s", ip, subnet)
	}

	// Must not be network, broadcast, or gateway.
	if lease.IPv4 == "192.168.10.0" || lease.IPv4 == "192.168.10.255" || lease.IPv4 == "192.168.10.1" {
		t.Errorf("allocated a reserved address: %s", lease.IPv4)
	}
}

func TestReconcileSkipsFullySpecifiedLease(t *testing.T) {
	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-01",
			Labels: map[string]string{siteLabel: "test-site"},
		},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image: "test-image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{
					MAC:        "aa:bb:cc:dd:ee:01",
					IPv4:       "10.0.0.50",
					SubnetMask: "255.255.255.0",
					Gateway:    "10.0.0.1",
				}},
			},
		},
	}

	cl := newFakeClient(t, machine)

	r := &Reconciler{Client: cl, APIReader: cl}

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "node-01"},
	})
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	// Re-read - fields should be unchanged.
	var updated v1alpha3.Machine
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "node-01"}, &updated); err != nil {
		t.Fatal(err)
	}

	lease := updated.Spec.PXE.DHCPLeases[0]
	if lease.IPv4 != "10.0.0.50" {
		t.Errorf("expected IPv4 unchanged, got %s", lease.IPv4)
	}
}

func TestReconcileAvoidsExistingAllocations(t *testing.T) {
	site := makeSite("test-site", []string{"10.0.0.0/30"})

	// /30 has exactly one usable host: 10.0.0.2.
	existing := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "existing-node",
			Labels: map[string]string{siteLabel: "test-site"},
		},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image: "test-image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{
					MAC:        "aa:bb:cc:dd:ee:10",
					IPv4:       "10.0.0.2",
					SubnetMask: "255.255.255.252",
					Gateway:    "10.0.0.1",
				}},
			},
		},
	}

	newMachine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "new-node",
			Labels: map[string]string{siteLabel: "test-site"},
		},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image: "test-image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{
					MAC: "aa:bb:cc:dd:ee:11",
				}},
			},
		},
	}

	scheme := runtime.NewScheme()
	if err := v1alpha3.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(existing, newMachine, site).
		WithStatusSubresource(&v1alpha3.Machine{}).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	r := &Reconciler{Client: cl, APIReader: cl}

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "new-node"},
	})
	if err == nil {
		t.Fatal("expected error when subnet is exhausted, got nil")
	}
}

func TestReconcileNoSiteLabel(t *testing.T) {
	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-no-site",
			// No site label.
		},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image: "test-image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{
					MAC: "aa:bb:cc:dd:ee:01",
				}},
			},
		},
	}

	cl := newFakeClient(t, machine)

	r := &Reconciler{Client: cl, APIReader: cl}

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "node-no-site"},
	})
	// Should not error - just skip gracefully.
	if err != nil {
		t.Fatalf("expected no error for missing site label, got: %v", err)
	}

	// Verify the lease is still empty.
	var updated v1alpha3.Machine
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "node-no-site"}, &updated); err != nil {
		t.Fatal(err)
	}

	if updated.Spec.PXE.DHCPLeases[0].IPv4 != "" {
		t.Error("expected IPv4 to remain empty when no site label is set")
	}
}

func TestReconcileNoPXE(t *testing.T) {
	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-no-pxe"},
		Spec:       v1alpha3.MachineSpec{},
	}

	cl := newFakeClient(t, machine)

	r := &Reconciler{Client: cl, APIReader: cl}

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "node-no-pxe"},
	})
	if err != nil {
		t.Fatalf("expected no error for non-PXE machine, got: %v", err)
	}
}

func TestReconcilePartialLease(t *testing.T) {
	// Test a lease that has IPv4 set but is missing SubnetMask and Gateway.
	site := makeSite("test-site", []string{"10.10.0.0/16"})

	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-partial",
			Labels: map[string]string{siteLabel: "test-site"},
		},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image: "test-image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{
					MAC:  "aa:bb:cc:dd:ee:01",
					IPv4: "10.10.5.50",
					// SubnetMask and Gateway omitted.
				}},
			},
		},
	}

	scheme := runtime.NewScheme()
	if err := v1alpha3.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(machine, site).
		WithStatusSubresource(&v1alpha3.Machine{}).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	r := &Reconciler{Client: cl, APIReader: cl}

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "node-partial"},
	})
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	var updated v1alpha3.Machine
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "node-partial"}, &updated); err != nil {
		t.Fatal(err)
	}

	lease := updated.Spec.PXE.DHCPLeases[0]

	// IPv4 should remain unchanged.
	if lease.IPv4 != "10.10.5.50" {
		t.Errorf("expected IPv4 to remain 10.10.5.50, got %s", lease.IPv4)
	}

	if lease.SubnetMask != "255.255.0.0" {
		t.Errorf("expected subnet mask 255.255.0.0, got %s", lease.SubnetMask)
	}

	if lease.Gateway != "10.10.0.1" {
		t.Errorf("expected gateway 10.10.0.1, got %s", lease.Gateway)
	}
}

func TestReconcileMultipleLeases(t *testing.T) {
	site := makeSite("test-site", []string{"192.168.50.0/24"})

	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-multi",
			Labels: map[string]string{siteLabel: "test-site"},
		},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image: "test-image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{
					{MAC: "aa:bb:cc:dd:ee:01"},
					{MAC: "aa:bb:cc:dd:ee:02"},
				},
			},
		},
	}

	scheme := runtime.NewScheme()
	if err := v1alpha3.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(machine, site).
		WithStatusSubresource(&v1alpha3.Machine{}).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	r := &Reconciler{Client: cl, APIReader: cl}

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "node-multi"},
	})
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	var updated v1alpha3.Machine
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "node-multi"}, &updated); err != nil {
		t.Fatal(err)
	}

	lease0 := updated.Spec.PXE.DHCPLeases[0]
	lease1 := updated.Spec.PXE.DHCPLeases[1]

	if lease0.IPv4 == "" || lease1.IPv4 == "" {
		t.Error("expected both leases to have IPv4 allocated")
	}

	if lease0.IPv4 == lease1.IPv4 {
		t.Errorf("both leases got the same IP: %s", lease0.IPv4)
	}
}

func TestReconcileDeletedMachine(t *testing.T) {
	// Reconciling a deleted Machine should be a no-op.
	cl := newFakeClient(t)

	r := &Reconciler{Client: cl, APIReader: cl}

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "deleted-node"},
	})
	if err != nil {
		t.Fatalf("expected no error for deleted machine, got: %v", err)
	}
}

func TestReconcileIdempotent(t *testing.T) {
	// Running reconcile twice should not change the allocated IP.
	site := makeSite("test-site", []string{"192.168.10.0/24"})

	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-idem",
			Labels: map[string]string{siteLabel: "test-site"},
		},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image: "test-image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{
					MAC: "aa:bb:cc:dd:ee:01",
				}},
			},
		},
	}

	scheme := runtime.NewScheme()
	if err := v1alpha3.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(machine, site).
		WithStatusSubresource(&v1alpha3.Machine{}).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	r := &Reconciler{Client: cl, APIReader: cl}

	// First reconcile.
	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "node-idem"},
	})
	if err != nil {
		t.Fatalf("first reconcile failed: %v", err)
	}

	var first v1alpha3.Machine
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "node-idem"}, &first); err != nil {
		t.Fatal(err)
	}

	firstIP := first.Spec.PXE.DHCPLeases[0].IPv4

	// Second reconcile.
	_, err = r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "node-idem"},
	})
	if err != nil {
		t.Fatalf("second reconcile failed: %v", err)
	}

	var second v1alpha3.Machine
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "node-idem"}, &second); err != nil {
		t.Fatal(err)
	}

	if second.Spec.PXE.DHCPLeases[0].IPv4 != firstIP {
		t.Errorf("IP changed between reconciles: %s -> %s", firstIP, second.Spec.PXE.DHCPLeases[0].IPv4)
	}
}

func TestReconcileIPv6OnlyNodeCidrs(t *testing.T) {
	// If the site only has IPv6 nodeCidrs, we should get a "no nodeCidrs" error
	// since DHCP leases are IPv4-only.
	site := makeSite("ipv6-site", []string{"fd00::/64"})

	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-ipv6",
			Labels: map[string]string{siteLabel: "ipv6-site"},
		},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image: "test-image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{
					MAC: "aa:bb:cc:dd:ee:01",
				}},
			},
		},
	}

	scheme := runtime.NewScheme()
	if err := v1alpha3.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(machine, site).
		WithStatusSubresource(&v1alpha3.Machine{}).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	r := &Reconciler{Client: cl, APIReader: cl}

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "node-ipv6"},
	})
	// Should log warning but not error - no IPv4 subnets means we cannot
	// allocate, but it's reported as "no nodeCidrs".
	if err != nil {
		t.Fatalf("expected no error for IPv6-only site (graceful skip), got: %v", err)
	}
}

func TestNeedsAllocation(t *testing.T) {
	tests := []struct {
		name  string
		lease v1alpha3.DHCPLease
		want  bool
	}{
		{
			name:  "fully specified",
			lease: v1alpha3.DHCPLease{MAC: "aa:bb:cc:dd:ee:01", IPv4: "10.0.0.2", SubnetMask: "255.255.255.0", Gateway: "10.0.0.1"},
			want:  false,
		},
		{
			name:  "missing ipv4",
			lease: v1alpha3.DHCPLease{MAC: "aa:bb:cc:dd:ee:01", SubnetMask: "255.255.255.0", Gateway: "10.0.0.1"},
			want:  true,
		},
		{
			name:  "missing subnet mask",
			lease: v1alpha3.DHCPLease{MAC: "aa:bb:cc:dd:ee:01", IPv4: "10.0.0.2", Gateway: "10.0.0.1"},
			want:  true,
		},
		{
			name:  "missing gateway",
			lease: v1alpha3.DHCPLease{MAC: "aa:bb:cc:dd:ee:01", IPv4: "10.0.0.2", SubnetMask: "255.255.255.0"},
			want:  true,
		},
		{
			name:  "all missing",
			lease: v1alpha3.DHCPLease{MAC: "aa:bb:cc:dd:ee:01"},
			want:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := leaseNeedsAllocation(&tt.lease)
			if got != tt.want {
				t.Errorf("leaseNeedsAllocation() = %v, want %v", got, tt.want)
			}
		})
	}
}
