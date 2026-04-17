// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"

	unboundednetv1alpha1 "github.com/Azure/unbounded-kube/internal/net/apis/unboundednet/v1alpha1"
	unboundednetnetlink "github.com/Azure/unbounded-kube/internal/net/netlink"
)

func newInformerWithObjects(objects ...*unstructured.Unstructured) cache.SharedIndexInformer {
	informer := cache.NewSharedIndexInformer(&cache.ListWatch{}, &unstructured.Unstructured{}, 0, cache.Indexers{})

	for _, obj := range objects {
		if obj == nil {
			continue
		}

		if err := informer.GetStore().Add(obj); err != nil {
			panic(err)
		}
	}

	return informer
}

// TestUpdateWireGuardFromSlices_GatewayMeshPeersUseOnlyDirectConnectedSites tests UpdateWireGuardFromSlices_GatewayMeshPeersUseOnlyDirectConnectedSites.
func TestUpdateWireGuardFromSlices_GatewayMeshPeersUseOnlyDirectConnectedSites(t *testing.T) {
	mySiteName := "site2"
	myPubKey := "pub-self"

	siteInformer := newInformerWithObjects(
		toUnstructured(t, &unboundednetv1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "site2"}}),
		toUnstructured(t, &unboundednetv1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "site-direct"}}),
		toUnstructured(t, &unboundednetv1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "site-transitive"}}),
	)

	sliceInformer := newInformerWithObjects(
		toUnstructured(t, &unboundednetv1alpha1.SiteNodeSlice{
			ObjectMeta: metav1.ObjectMeta{Name: "slice-direct"},
			SiteName:   "site-direct",
			SliceIndex: 0,
			Nodes: []unboundednetv1alpha1.NodeInfo{{
				Name:               "worker-direct",
				WireGuardPublicKey: "pub-direct",
				InternalIPs:        []string{"10.10.0.10"},
				PodCIDRs:           []string{"10.244.10.0/24"},
			}},
		}),
		toUnstructured(t, &unboundednetv1alpha1.SiteNodeSlice{
			ObjectMeta: metav1.ObjectMeta{Name: "slice-transitive"},
			SiteName:   "site-transitive",
			SliceIndex: 0,
			Nodes: []unboundednetv1alpha1.NodeInfo{{
				Name:               "worker-transitive",
				WireGuardPublicKey: "pub-transitive",
				InternalIPs:        []string{"10.20.0.10"},
				PodCIDRs:           []string{"10.244.20.0/24"},
			}},
		}),
	)

	gatewayPoolInformer := newInformerWithObjects(
		toUnstructured(t, &unboundednetv1alpha1.GatewayPool{
			ObjectMeta: metav1.ObjectMeta{Name: "site2extgw1"},
			Spec: unboundednetv1alpha1.GatewayPoolSpec{
				Type:         "External",
				NodeSelector: map[string]string{"role": "gateway"},
			},
			Status: unboundednetv1alpha1.GatewayPoolStatus{
				ConnectedSites: []string{"site-direct"},
				Nodes: []unboundednetv1alpha1.GatewayNodeInfo{{
					Name:                 "gw-self",
					SiteName:             mySiteName,
					WireGuardPublicKey:   myPubKey,
					GatewayWireguardPort: 51821,
					InternalIPs:          []string{"10.2.0.10"},
					PodCIDRs:             []string{"10.244.2.0/24"},
				}},
			},
		}),
	)

	gatewayNodeInformer := newInformerWithObjects()

	sitePeeringInformer := newInformerWithObjects(
		toUnstructured(t, &unboundednetv1alpha1.SitePeering{
			ObjectMeta: metav1.ObjectMeta{Name: "site-transitive-peering"},
			Spec: unboundednetv1alpha1.SitePeeringSpec{
				Sites: []string{mySiteName, "site-transitive"},
			},
		}),
	)
	assignmentInformer := newInformerWithObjects(
		toUnstructured(t, &unboundednetv1alpha1.SiteGatewayPoolAssignment{
			ObjectMeta: metav1.ObjectMeta{Name: "assign-direct"},
			Spec: unboundednetv1alpha1.SiteGatewayPoolAssignmentSpec{
				Sites:        []string{"site-direct"},
				GatewayPools: []string{"site2extgw1"},
			},
		}),
	)
	poolPeeringInformer := newInformerWithObjects()

	state := &wireGuardState{
		clientset: fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-self"}}),
		nodeName:  "node-self",
	}
	cfg := &config{NodeName: "node-self", WireGuardPort: 51820}

	var gotPeers []meshPeerInfo

	origConfigure := configureWireGuardFunc
	configureWireGuardFunc = func(_ context.Context, _ *config, _ string, peers []meshPeerInfo, _ []gatewayPeerInfo, _ string, _, _, _ map[string]bool, _, _, _, _, _ map[string]string, _, _, _, _, _ map[string]int, _ []unboundednetnetlink.DesiredRoute, _ map[string]bool, _ *wireGuardState) error {
		gotPeers = append([]meshPeerInfo(nil), peers...)
		return nil
	}

	defer func() { configureWireGuardFunc = origConfigure }()

	err := updateWireGuardFromSlices(context.Background(), nil, siteInformer, sliceInformer, gatewayPoolInformer, gatewayNodeInformer, sitePeeringInformer, assignmentInformer, poolPeeringInformer, cfg, mySiteName, "priv", myPubKey, true, state)
	if err != nil {
		t.Fatalf("updateWireGuardFromSlices() error = %v", err)
	}

	peerNames := map[string]bool{}
	for _, peer := range gotPeers {
		peerNames[peer.Name] = true
	}

	if !peerNames["worker-direct"] {
		t.Fatalf("expected direct connected site worker to be included, got peers %#v", gotPeers)
	}

	if peerNames["worker-transitive"] {
		t.Fatalf("did not expect transitive-only site worker to be included, got peers %#v", gotPeers)
	}
}

// TestUpdateWireGuardFromSlices_ExternalGatewayIncludesAssignedNonDirectSites tests UpdateWireGuardFromSlices_ExternalGatewayIncludesAssignedNonDirectSites.
func TestUpdateWireGuardFromSlices_ExternalGatewayIncludesAssignedNonDirectSites(t *testing.T) {
	mySiteName := "site2"
	myPubKey := "pub-self"

	siteInformer := newInformerWithObjects(
		toUnstructured(t, &unboundednetv1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "site2"}}),
		toUnstructured(t, &unboundednetv1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "site-direct"}}),
		toUnstructured(t, &unboundednetv1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "site-remote-assigned"}}),
	)

	sliceInformer := newInformerWithObjects(
		toUnstructured(t, &unboundednetv1alpha1.SiteNodeSlice{
			ObjectMeta: metav1.ObjectMeta{Name: "slice-direct"},
			SiteName:   "site-direct",
			SliceIndex: 0,
			Nodes: []unboundednetv1alpha1.NodeInfo{{
				Name:               "worker-direct",
				WireGuardPublicKey: "pub-direct",
				InternalIPs:        []string{"10.10.0.10"},
				PodCIDRs:           []string{"10.244.10.0/24"},
			}},
		}),
		toUnstructured(t, &unboundednetv1alpha1.SiteNodeSlice{
			ObjectMeta: metav1.ObjectMeta{Name: "slice-remote-assigned"},
			SiteName:   "site-remote-assigned",
			SliceIndex: 0,
			Nodes: []unboundednetv1alpha1.NodeInfo{{
				Name:               "worker-remote-assigned",
				WireGuardPublicKey: "pub-remote-assigned",
				InternalIPs:        []string{"10.30.0.10"},
				PodCIDRs:           []string{"10.244.30.0/24"},
			}},
		}),
	)

	gatewayPoolInformer := newInformerWithObjects(
		toUnstructured(t, &unboundednetv1alpha1.GatewayPool{
			ObjectMeta: metav1.ObjectMeta{Name: "site2extgw1"},
			Spec: unboundednetv1alpha1.GatewayPoolSpec{
				Type:         "External",
				NodeSelector: map[string]string{"role": "gateway"},
			},
			Status: unboundednetv1alpha1.GatewayPoolStatus{
				ConnectedSites: []string{"site-direct"},
				Nodes: []unboundednetv1alpha1.GatewayNodeInfo{{
					Name:                 "gw-self",
					SiteName:             mySiteName,
					WireGuardPublicKey:   myPubKey,
					GatewayWireguardPort: 51821,
					InternalIPs:          []string{"10.2.0.10"},
					PodCIDRs:             []string{"10.244.2.0/24"},
				}},
			},
		}),
	)

	gatewayNodeInformer := newInformerWithObjects()
	sitePeeringInformer := newInformerWithObjects()
	assignmentInformer := newInformerWithObjects(
		toUnstructured(t, &unboundednetv1alpha1.SiteGatewayPoolAssignment{
			ObjectMeta: metav1.ObjectMeta{Name: "assign-direct-and-remote"},
			Spec: unboundednetv1alpha1.SiteGatewayPoolAssignmentSpec{
				Sites:        []string{"site-direct", "site-remote-assigned"},
				GatewayPools: []string{"site2extgw1"},
			},
		}),
	)
	poolPeeringInformer := newInformerWithObjects()

	state := &wireGuardState{
		clientset: fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-self"}}),
		nodeName:  "node-self",
	}
	cfg := &config{NodeName: "node-self", WireGuardPort: 51820}

	var gotPeers []meshPeerInfo

	origConfigure := configureWireGuardFunc
	configureWireGuardFunc = func(_ context.Context, _ *config, _ string, peers []meshPeerInfo, _ []gatewayPeerInfo, _ string, _, _, _ map[string]bool, _, _, _, _, _ map[string]string, _, _, _, _, _ map[string]int, _ []unboundednetnetlink.DesiredRoute, _ map[string]bool, _ *wireGuardState) error {
		gotPeers = append([]meshPeerInfo(nil), peers...)
		return nil
	}

	defer func() { configureWireGuardFunc = origConfigure }()

	err := updateWireGuardFromSlices(context.Background(), nil, siteInformer, sliceInformer, gatewayPoolInformer, gatewayNodeInformer, sitePeeringInformer, assignmentInformer, poolPeeringInformer, cfg, mySiteName, "priv", myPubKey, true, state)
	if err != nil {
		t.Fatalf("updateWireGuardFromSlices() error = %v", err)
	}

	peerNames := map[string]bool{}
	for _, peer := range gotPeers {
		peerNames[peer.Name] = true
	}

	if !peerNames["worker-direct"] {
		t.Fatalf("expected direct connected site worker to be included, got peers %#v", gotPeers)
	}

	if !peerNames["worker-remote-assigned"] {
		t.Fatalf("expected assigned non-direct site worker to be included for external pool, got peers %#v", gotPeers)
	}
}

// TestUpdateWireGuardFromSlices_NonGatewayMeshPeersUseOnlyPeeredSites tests UpdateWireGuardFromSlices_NonGatewayMeshPeersUseOnlyPeeredSites.
func TestUpdateWireGuardFromSlices_NonGatewayMeshPeersUseOnlyPeeredSites(t *testing.T) {
	mySiteName := "site1"
	myPubKey := "pub-self"

	siteInformer := newInformerWithObjects(
		toUnstructured(t, &unboundednetv1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "site1"}}),
		toUnstructured(t, &unboundednetv1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "site2"}}),
		toUnstructured(t, &unboundednetv1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "site3"}}),
	)

	sliceInformer := newInformerWithObjects(
		toUnstructured(t, &unboundednetv1alpha1.SiteNodeSlice{
			ObjectMeta: metav1.ObjectMeta{Name: "slice-site2"},
			SiteName:   "site2",
			SliceIndex: 0,
			Nodes: []unboundednetv1alpha1.NodeInfo{{
				Name:               "worker-site2",
				WireGuardPublicKey: "pub-site2",
				InternalIPs:        []string{"10.2.0.10"},
				PodCIDRs:           []string{"10.244.2.0/24"},
			}},
		}),
		toUnstructured(t, &unboundednetv1alpha1.SiteNodeSlice{
			ObjectMeta: metav1.ObjectMeta{Name: "slice-site3"},
			SiteName:   "site3",
			SliceIndex: 0,
			Nodes: []unboundednetv1alpha1.NodeInfo{{
				Name:               "worker-site3",
				WireGuardPublicKey: "pub-site3",
				InternalIPs:        []string{"10.3.0.10"},
				PodCIDRs:           []string{"10.244.3.0/24"},
			}},
		}),
	)

	gatewayPoolInformer := newInformerWithObjects()
	gatewayNodeInformer := newInformerWithObjects()

	sitePeeringInformer := newInformerWithObjects(
		toUnstructured(t, &unboundednetv1alpha1.SitePeering{
			ObjectMeta: metav1.ObjectMeta{Name: "site1-site2"},
			Spec: unboundednetv1alpha1.SitePeeringSpec{
				Sites: []string{mySiteName, "site2"},
			},
		}),
	)
	assignmentInformer := newInformerWithObjects()
	poolPeeringInformer := newInformerWithObjects()

	state := &wireGuardState{
		clientset: fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-self"}}),
		nodeName:  "node-self",
	}
	cfg := &config{NodeName: "node-self", WireGuardPort: 51820}

	var gotPeers []meshPeerInfo

	origConfigure := configureWireGuardFunc
	configureWireGuardFunc = func(_ context.Context, _ *config, _ string, peers []meshPeerInfo, _ []gatewayPeerInfo, _ string, _, _, _ map[string]bool, _, _, _, _, _ map[string]string, _, _, _, _, _ map[string]int, _ []unboundednetnetlink.DesiredRoute, _ map[string]bool, _ *wireGuardState) error {
		gotPeers = append([]meshPeerInfo(nil), peers...)
		return nil
	}

	defer func() { configureWireGuardFunc = origConfigure }()

	err := updateWireGuardFromSlices(context.Background(), nil, siteInformer, sliceInformer, gatewayPoolInformer, gatewayNodeInformer, sitePeeringInformer, assignmentInformer, poolPeeringInformer, cfg, mySiteName, "priv", myPubKey, true, state)
	if err != nil {
		t.Fatalf("updateWireGuardFromSlices() error = %v", err)
	}

	peerNames := map[string]bool{}
	for _, peer := range gotPeers {
		peerNames[peer.Name] = true
	}

	if !peerNames["worker-site2"] {
		t.Fatalf("expected worker in peered site to be included, got peers %#v", gotPeers)
	}

	if peerNames["worker-site3"] {
		t.Fatalf("did not expect worker in non-peered site to be included, got peers %#v", gotPeers)
	}
}

// TestUpdateWireGuardFromSlices_ManageCniPluginFalseSkipsPodCIDRRoutesForSameSiteGatewayLinks tests ManageCniPluginFalseSkipsPodCIDRRoutesForSameSiteGatewayLinks.
func TestUpdateWireGuardFromSlices_ManageCniPluginFalseSkipsPodCIDRRoutesForSameSiteGatewayLinks(t *testing.T) {
	t.Run("non-gateway node marks same-site gateway peer podCIDR routes to skip", func(t *testing.T) {
		mySiteName := "site1"
		myPubKey := "pub-self"

		siteInformer := newInformerWithObjects(
			toUnstructured(t, &unboundednetv1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "site1"}}),
		)
		sliceInformer := newInformerWithObjects(
			toUnstructured(t, &unboundednetv1alpha1.SiteNodeSlice{
				ObjectMeta: metav1.ObjectMeta{Name: "slice-site1"},
				SiteName:   "site1",
				SliceIndex: 0,
				Nodes: []unboundednetv1alpha1.NodeInfo{{
					Name:               "node-self",
					WireGuardPublicKey: myPubKey,
					InternalIPs:        []string{"10.1.0.10"},
					PodCIDRs:           []string{"10.244.1.0/24"},
				}},
			}),
		)
		gatewayPoolInformer := newInformerWithObjects(
			toUnstructured(t, &unboundednetv1alpha1.GatewayPool{
				ObjectMeta: metav1.ObjectMeta{Name: "pool-site1"},
				Spec:       unboundednetv1alpha1.GatewayPoolSpec{Type: "Internal"},
				Status: unboundednetv1alpha1.GatewayPoolStatus{
					Nodes: []unboundednetv1alpha1.GatewayNodeInfo{{
						Name:                 "gw-site1",
						SiteName:             mySiteName,
						WireGuardPublicKey:   "pub-gw-site1",
						GatewayWireguardPort: 51821,
						InternalIPs:          []string{"10.1.0.20"},
						PodCIDRs:             []string{"10.244.2.0/24"},
					}},
				},
			}),
		)
		gatewayNodeInformer := newInformerWithObjects()
		sitePeeringInformer := newInformerWithObjects()
		assignmentInformer := newInformerWithObjects(
			toUnstructured(t, &unboundednetv1alpha1.SiteGatewayPoolAssignment{
				ObjectMeta: metav1.ObjectMeta{Name: "assign-site1"},
				Spec: unboundednetv1alpha1.SiteGatewayPoolAssignmentSpec{
					Sites:        []string{mySiteName},
					GatewayPools: []string{"pool-site1"},
				},
			}),
		)
		poolPeeringInformer := newInformerWithObjects()

		state := &wireGuardState{
			clientset: fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-self"}}),
			nodeName:  "node-self",
		}
		cfg := &config{NodeName: "node-self", WireGuardPort: 51820}

		var gotGatewayPeers []gatewayPeerInfo

		origConfigure := configureWireGuardFunc
		configureWireGuardFunc = func(_ context.Context, _ *config, _ string, _ []meshPeerInfo, gatewayPeers []gatewayPeerInfo, _ string, _, _, _ map[string]bool, _, _, _, _, _ map[string]string, _, _, _, _, _ map[string]int, _ []unboundednetnetlink.DesiredRoute, _ map[string]bool, _ *wireGuardState) error {
			gotGatewayPeers = append([]gatewayPeerInfo(nil), gatewayPeers...)
			return nil
		}

		defer func() { configureWireGuardFunc = origConfigure }()

		if err := updateWireGuardFromSlices(context.Background(), nil, siteInformer, sliceInformer, gatewayPoolInformer, gatewayNodeInformer, sitePeeringInformer, assignmentInformer, poolPeeringInformer, cfg, mySiteName, "priv", myPubKey, false, state); err != nil {
			t.Fatalf("updateWireGuardFromSlices() error = %v", err)
		}

		if len(gotGatewayPeers) != 1 {
			t.Fatalf("expected one gateway peer, got %#v", gotGatewayPeers)
		}

		if !gotGatewayPeers[0].SkipPodCIDRRoutes {
			t.Fatalf("expected same-site gateway peer to skip podCIDR routes when manageCniPlugin is false, got %#v", gotGatewayPeers[0])
		}
	})

	t.Run("gateway node marks same-site mesh peer podCIDR routes to skip", func(t *testing.T) {
		mySiteName := "site1"
		myPubKey := "pub-gw-self"

		siteInformer := newInformerWithObjects(
			toUnstructured(t, &unboundednetv1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "site1"}}),
		)
		sliceInformer := newInformerWithObjects(
			toUnstructured(t, &unboundednetv1alpha1.SiteNodeSlice{
				ObjectMeta: metav1.ObjectMeta{Name: "slice-site1"},
				SiteName:   mySiteName,
				SliceIndex: 0,
				Nodes: []unboundednetv1alpha1.NodeInfo{
					{
						Name:               "worker-site1",
						WireGuardPublicKey: "pub-worker-site1",
						InternalIPs:        []string{"10.1.0.11"},
						PodCIDRs:           []string{"10.244.11.0/24"},
					},
					{
						Name:               "gw-self",
						WireGuardPublicKey: myPubKey,
						InternalIPs:        []string{"10.1.0.20"},
						PodCIDRs:           []string{"10.244.12.0/24"},
					},
				},
			}),
		)
		gatewayPoolInformer := newInformerWithObjects(
			toUnstructured(t, &unboundednetv1alpha1.GatewayPool{
				ObjectMeta: metav1.ObjectMeta{Name: "pool-site1"},
				Spec:       unboundednetv1alpha1.GatewayPoolSpec{Type: "Internal"},
				Status: unboundednetv1alpha1.GatewayPoolStatus{
					ConnectedSites: []string{mySiteName},
					Nodes: []unboundednetv1alpha1.GatewayNodeInfo{{
						Name:                 "gw-self",
						SiteName:             mySiteName,
						WireGuardPublicKey:   myPubKey,
						GatewayWireguardPort: 51821,
						InternalIPs:          []string{"10.1.0.20"},
						PodCIDRs:             []string{"10.244.12.0/24"},
					}},
				},
			}),
		)
		gatewayNodeInformer := newInformerWithObjects()
		sitePeeringInformer := newInformerWithObjects()
		assignmentInformer := newInformerWithObjects(
			toUnstructured(t, &unboundednetv1alpha1.SiteGatewayPoolAssignment{
				ObjectMeta: metav1.ObjectMeta{Name: "assign-site1"},
				Spec: unboundednetv1alpha1.SiteGatewayPoolAssignmentSpec{
					Sites:        []string{mySiteName},
					GatewayPools: []string{"pool-site1"},
				},
			}),
		)
		poolPeeringInformer := newInformerWithObjects()

		state := &wireGuardState{
			clientset: fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gw-self"}}),
			nodeName:  "gw-self",
		}
		cfg := &config{NodeName: "gw-self", WireGuardPort: 51820}

		var gotPeers []meshPeerInfo

		origConfigure := configureWireGuardFunc
		configureWireGuardFunc = func(_ context.Context, _ *config, _ string, peers []meshPeerInfo, _ []gatewayPeerInfo, _ string, _, _, _ map[string]bool, _, _, _, _, _ map[string]string, _, _, _, _, _ map[string]int, _ []unboundednetnetlink.DesiredRoute, _ map[string]bool, _ *wireGuardState) error {
			gotPeers = append([]meshPeerInfo(nil), peers...)
			return nil
		}

		defer func() { configureWireGuardFunc = origConfigure }()

		if err := updateWireGuardFromSlices(context.Background(), nil, siteInformer, sliceInformer, gatewayPoolInformer, gatewayNodeInformer, sitePeeringInformer, assignmentInformer, poolPeeringInformer, cfg, mySiteName, "priv", myPubKey, false, state); err != nil {
			t.Fatalf("updateWireGuardFromSlices() error = %v", err)
		}

		var workerPeer *meshPeerInfo

		for i := range gotPeers {
			if gotPeers[i].Name == "worker-site1" {
				workerPeer = &gotPeers[i]
				break
			}
		}

		if workerPeer == nil {
			t.Fatalf("expected same-site worker to be included as mesh peer, got %#v", gotPeers)
		}

		if !workerPeer.SkipPodCIDRRoutes {
			t.Fatalf("expected same-site worker mesh peer to skip podCIDR routes when manageCniPlugin is false, got %#v", *workerPeer)
		}
	})
}

// TestUpdateWireGuardFromSlices_ManageCniPluginFalseKeepsRemotePeeredMeshPeers tests unmanaged-site remote peering behavior.
func TestUpdateWireGuardFromSlices_ManageCniPluginFalseKeepsRemotePeeredMeshPeers(t *testing.T) {
	mySiteName := "site1"
	myPubKey := "pub-self"

	siteInformer := newInformerWithObjects(
		toUnstructured(t, &unboundednetv1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "site1"}}),
		toUnstructured(t, &unboundednetv1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: "site2"}}),
	)
	sliceInformer := newInformerWithObjects(
		toUnstructured(t, &unboundednetv1alpha1.SiteNodeSlice{
			ObjectMeta: metav1.ObjectMeta{Name: "slice-site1"},
			SiteName:   "site1",
			SliceIndex: 0,
			Nodes: []unboundednetv1alpha1.NodeInfo{
				{
					Name:               "node-self",
					WireGuardPublicKey: myPubKey,
					InternalIPs:        []string{"10.1.0.10"},
					PodCIDRs:           []string{"10.244.1.0/24"},
				},
				{
					Name:               "worker-site1",
					WireGuardPublicKey: "pub-site1-worker",
					InternalIPs:        []string{"10.1.0.11"},
					PodCIDRs:           []string{"10.244.11.0/24"},
				},
			},
		}),
		toUnstructured(t, &unboundednetv1alpha1.SiteNodeSlice{
			ObjectMeta: metav1.ObjectMeta{Name: "slice-site2"},
			SiteName:   "site2",
			SliceIndex: 0,
			Nodes: []unboundednetv1alpha1.NodeInfo{
				{
					Name:               "worker-site2",
					WireGuardPublicKey: "pub-site2-worker",
					InternalIPs:        []string{"10.2.0.11"},
					PodCIDRs:           []string{"10.244.2.0/24"},
				},
			},
		}),
	)

	gatewayPoolInformer := newInformerWithObjects()
	gatewayNodeInformer := newInformerWithObjects()
	sitePeeringInformer := newInformerWithObjects(
		toUnstructured(t, &unboundednetv1alpha1.SitePeering{
			ObjectMeta: metav1.ObjectMeta{Name: "site1-site2"},
			Spec:       unboundednetv1alpha1.SitePeeringSpec{Sites: []string{"site1", "site2"}},
		}),
	)
	assignmentInformer := newInformerWithObjects()
	poolPeeringInformer := newInformerWithObjects()

	state := &wireGuardState{
		clientset: fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-self"}}),
		nodeName:  "node-self",
	}
	cfg := &config{NodeName: "node-self", WireGuardPort: 51820}

	var gotPeers []meshPeerInfo

	origConfigure := configureWireGuardFunc
	configureWireGuardFunc = func(_ context.Context, _ *config, _ string, peers []meshPeerInfo, _ []gatewayPeerInfo, _ string, _, _, _ map[string]bool, _, _, _, _, _ map[string]string, _, _, _, _, _ map[string]int, _ []unboundednetnetlink.DesiredRoute, _ map[string]bool, _ *wireGuardState) error {
		gotPeers = append([]meshPeerInfo(nil), peers...)
		return nil
	}

	defer func() { configureWireGuardFunc = origConfigure }()

	if err := updateWireGuardFromSlices(context.Background(), nil, siteInformer, sliceInformer, gatewayPoolInformer, gatewayNodeInformer, sitePeeringInformer, assignmentInformer, poolPeeringInformer, cfg, mySiteName, "priv", myPubKey, false, state); err != nil {
		t.Fatalf("updateWireGuardFromSlices() error = %v", err)
	}

	peerNames := map[string]bool{}
	for _, peer := range gotPeers {
		peerNames[peer.Name] = true
	}

	if !peerNames["worker-site2"] {
		t.Fatalf("expected peered remote worker to be included when manageCniPlugin=false, got peers %#v", gotPeers)
	}

	if peerNames["worker-site1"] {
		t.Fatalf("expected same-site worker to be skipped when manageCniPlugin=false, got peers %#v", gotPeers)
	}
}
