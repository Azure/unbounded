// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
	unboundednetnetlink "github.com/Azure/unbounded/internal/net/netlink"
)

func TestUpdateWireGuardFromSlices_LocalGatewayExclusions(t *testing.T) {
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "local"}}
	siteInformer := newInformerWithObjects(toUnstructured(t, site))
	poolInformer := newInformerWithObjects(toUnstructured(t, &unboundednetv1alpha1.GatewayPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool"},
		Spec: unboundednetv1alpha1.GatewayPoolSpec{
			Type: "Internal", RoutedCidrs: []string{"192.168.0.0/16"},
		},
		Status: unboundednetv1alpha1.GatewayPoolStatus{Nodes: []unboundednetv1alpha1.GatewayNodeInfo{{
			Name: "gateway", SiteName: "remote", WireGuardPublicKey: "pub-gateway",
			InternalIPs: []string{"10.2.0.1"}, PodCIDRs: []string{"10.245.0.0/24"},
			GatewayWireguardPort: 51821,
		}}},
	}))
	assignments := newInformerWithObjects(toUnstructured(t, &unboundednetv1alpha1.SiteGatewayPoolAssignment{
		ObjectMeta: metav1.ObjectMeta{Name: "assignment"},
		Spec: unboundednetv1alpha1.SiteGatewayPoolAssignmentSpec{
			Sites: []string{"local"}, GatewayPools: []string{"pool"},
		},
	}))
	empty := newInformerWithObjects()
	cfg := &config{NodeName: "self", WireGuardPort: 51820, PreferredPrivateEncap: "WireGuard"}
	state := &wireGuardState{
		nodeName: "self", clientset: fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "self"}}),
	}

	calls := 0

	var gotGatewayPeers []gatewayPeerInfo

	original := configureWireGuardFunc
	configureWireGuardFunc = func(_ context.Context, _ *config, _ string, _ []meshPeerInfo, gatewayPeers []gatewayPeerInfo, _ string, _, _, _ map[string]bool, _, _, _, _, _ map[string]string, _, _, _, _, _ map[string]int, _ []unboundednetnetlink.DesiredRoute, _ map[string]bool, _ *wireGuardState) error {
		calls++
		gotGatewayPeers = gatewayPeers

		return nil
	}

	t.Cleanup(func() { configureWireGuardFunc = original })

	update := func() error {
		return updateWireGuardFromSlices(context.Background(), nil, siteInformer, empty, poolInformer, empty,
			empty, assignments, empty, cfg, "local", "priv", "pub-self", true, state)
	}

	for i, tt := range []struct {
		name  string
		local []string
		want  []string
	}{
		{name: "baseline", want: []string{"192.168.0.0/16"}},
		{name: "partial exclusion", local: []string{"192.168.0.0/17"}, want: []string{"192.168.128.0/17"}},
		{name: "full exclusion", local: []string{"192.168.0.0/16"}},
		{name: "removed exclusion", want: []string{"192.168.0.0/16"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			site.Spec.LocalCIDRs = tt.local
			if err := siteInformer.GetStore().Update(toUnstructured(t, site)); err != nil {
				t.Fatal(err)
			}

			if err := update(); err != nil {
				t.Fatal(err)
			}

			if calls != i+1 {
				t.Fatalf("configuration calls=%d, want %d", calls, i+1)
			}

			if len(gotGatewayPeers) != 1 || !slices.Equal(gotGatewayPeers[0].RoutedCidrs, tt.want) {
				t.Fatalf("gateway routes=%+v, want %v", gotGatewayPeers, tt.want)
			}

			if gotGatewayPeers[0].WireGuardPublicKey != "pub-gateway" || !slices.Equal(gotGatewayPeers[0].PodCIDRs, []string{"10.245.0.0/24"}) {
				t.Fatal("local exclusions changed gateway identity")
			}

			if err := update(); err != nil {
				t.Fatal(err)
			}

			if calls != i+1 {
				t.Fatal("unchanged exclusions caused another update")
			}
		})
	}
}

func TestUpdateWireGuardFromSlices_GatewayRoutingChanges(t *testing.T) {
	for _, tt := range []struct {
		name          string
		change        func(local, remote *unboundedv1alpha3.Site, pool *unboundednetv1alpha1.GatewayPool)
		failConfigure bool
		wantChange    bool
	}{
		{
			name: "remote pod pools",
			change: func(_, remote *unboundedv1alpha3.Site, _ *unboundednetv1alpha1.GatewayPool) {
				remote.Spec.PodCidrAssignments[0].CidrBlocks = []string{"10.246.0.0/16"}
			},
			wantChange: true,
		},
		{
			name: "remote node CIDRs",
			change: func(_, remote *unboundedv1alpha3.Site, _ *unboundednetv1alpha1.GatewayPool) {
				remote.Spec.NodeCidrs = []string{"10.3.0.0/16"}
			},
			wantChange: true,
		},
		{
			name: "local node CIDRs",
			change: func(local, _ *unboundedv1alpha3.Site, _ *unboundednetv1alpha1.GatewayPool) {
				local.Spec.NodeCidrs = []string{"10.4.0.0/16"}
			},
			wantChange: true,
		},
		{
			name: "local CIDR exclusions",
			change: func(local, _ *unboundedv1alpha3.Site, _ *unboundednetv1alpha1.GatewayPool) {
				local.Spec.LocalCIDRs = []string{"192.168.1.0/24"}
			},
			wantChange: true,
		},
		{
			name: "gateway pools failure retries",
			change: func(_, _ *unboundedv1alpha3.Site, pool *unboundednetv1alpha1.GatewayPool) {
				pool.Spec.RoutedCidrs = []string{"10.241.0.0/16"}
			},
			failConfigure: true,
			wantChange:    true,
		},
		{
			name: "gateway Site inputs failure retries",
			change: func(_, remote *unboundedv1alpha3.Site, _ *unboundednetv1alpha1.GatewayPool) {
				remote.Spec.NodeCidrs = []string{"10.3.0.0/16"}
			},
			failConfigure: true,
			wantChange:    true,
		},
		{
			name: "equivalent gateway pools",
			change: func(_, _ *unboundedv1alpha3.Site, pool *unboundednetv1alpha1.GatewayPool) {
				pool.Spec.RoutedCidrs = []string{"10.240.0.0/16", "10.240.0.0/16"}
			},
		},
		{
			name: "equivalent Site inputs",
			change: func(_, remote *unboundedv1alpha3.Site, _ *unboundednetv1alpha1.GatewayPool) {
				remote.Spec.NodeCidrs = []string{"10.2.0.0/16", "10.2.0.0/16"}
				remote.Spec.PodCidrAssignments[0].CidrBlocks = []string{"10.245.0.0/16", "10.245.0.0/16"}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			local := &unboundedv1alpha3.Site{
				ObjectMeta: metav1.ObjectMeta{Name: "local"},
				Spec: unboundedv1alpha3.SiteSpec{
					NodeCidrs:          []string{"10.1.0.0/16"},
					PodCidrAssignments: []unboundednetv1alpha1.PodCidrAssignment{{CidrBlocks: []string{"10.244.0.0/16"}}},
				},
			}

			remote := &unboundedv1alpha3.Site{
				ObjectMeta: metav1.ObjectMeta{Name: "remote"},
				Spec: unboundedv1alpha3.SiteSpec{
					NodeCidrs:          []string{"10.2.0.0/16"},
					PodCidrAssignments: []unboundednetv1alpha1.PodCidrAssignment{{CidrBlocks: []string{"10.245.0.0/16"}}},
				},
			}
			pool := &unboundednetv1alpha1.GatewayPool{
				ObjectMeta: metav1.ObjectMeta{Name: "local-pool"},
				Spec:       unboundednetv1alpha1.GatewayPoolSpec{RoutedCidrs: []string{"10.240.0.0/16"}},
				Status: unboundednetv1alpha1.GatewayPoolStatus{Nodes: []unboundednetv1alpha1.GatewayNodeInfo{{
					Name: "self", SiteName: "local", WireGuardPublicKey: "pub-self", GatewayWireguardPort: 51821,
				}}},
			}
			siteInformer := newInformerWithObjects(toUnstructured(t, local), toUnstructured(t, remote))
			poolInformer := newInformerWithObjects(toUnstructured(t, pool))
			empty := newInformerWithObjects()
			cfg := &config{NodeName: "self", WireGuardPort: 51820, PreferredPrivateEncap: "WireGuard"}
			state := &wireGuardState{
				nodeName:  "self",
				clientset: fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "self"}}),
			}

			calls := 0

			var configureErr error

			original := configureWireGuardFunc
			configureWireGuardFunc = func(context.Context, *config, string, []meshPeerInfo, []gatewayPeerInfo, string, map[string]bool, map[string]bool, map[string]bool, map[string]string, map[string]string, map[string]string, map[string]string, map[string]string, map[string]int, map[string]int, map[string]int, map[string]int, map[string]int, []unboundednetnetlink.DesiredRoute, map[string]bool, *wireGuardState) error {
				calls++
				return configureErr
			}

			t.Cleanup(func() { configureWireGuardFunc = original })

			update := func() error {
				return updateWireGuardFromSlices(context.Background(), nil, siteInformer, empty, poolInformer, empty,
					empty, empty, empty, cfg, "local", "priv", "pub-self", true, state)
			}
			if err := update(); err != nil {
				t.Fatal(err)
			}

			if calls != 1 {
				t.Fatalf("initial calls=%d, want 1", calls)
			}

			tt.change(local, remote, pool)

			for _, site := range []*unboundedv1alpha3.Site{local, remote} {
				if err := siteInformer.GetStore().Update(toUnstructured(t, site)); err != nil {
					t.Fatal(err)
				}
			}

			if err := poolInformer.GetStore().Update(toUnstructured(t, pool)); err != nil {
				t.Fatal(err)
			}

			wantCalls := 1

			if tt.failConfigure {
				configureErr = errors.New("configuration failed")
				if err := update(); !errors.Is(err, configureErr) {
					t.Fatalf("expected configuration failure, got %v", err)
				}

				if state.reconcileCount != 1 {
					t.Fatal("failed attempt committed routing state")
				}

				wantCalls++
				configureErr = nil
			}

			if err := update(); err != nil {
				t.Fatal(err)
			}

			if tt.wantChange {
				wantCalls++
			}

			if calls != wantCalls {
				t.Fatalf("configuration calls=%d, want %d", calls, wantCalls)
			}

			if err := update(); err != nil {
				t.Fatal(err)
			}

			if calls != wantCalls {
				t.Fatal("stable routing inputs caused another update")
			}
		})
	}
}
