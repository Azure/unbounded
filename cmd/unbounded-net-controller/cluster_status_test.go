// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	controllerpkg "github.com/Azure/unbounded-kube/internal/net/controller"
)

type failingNodeLister struct{}

// List lists the requested values.
func (f failingNodeLister) List(selector labels.Selector) ([]*corev1.Node, error) {
	return nil, fmt.Errorf("boom")
}

// Get returns the requested value.
func (f failingNodeLister) Get(name string) (*corev1.Node, error) {
	return nil, fmt.Errorf("not found")
}

// TestFetchClusterStatusFromCacheAndInformers tests FetchClusterStatusFromCacheAndInformers.
func TestFetchClusterStatusFromCacheAndInformers(t *testing.T) {
	clientset := k8sfake.NewClientset()

	nodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	podIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-a",
			Labels: map[string]string{
				controllerpkg.SiteLabelKey: "site-a",
				"role":                     "gateway",
			},
			Annotations: map[string]string{
				controllerpkg.WireGuardPubKeyAnnotation: "pub-key-a",
			},
		},
		Spec: corev1.NodeSpec{
			PodCIDRs:   []string{"10.244.1.0/24"},
			ProviderID: "azure:///subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/node-a",
			PodCIDR:    "10.244.1.0/24",
		},
		Status: corev1.NodeStatus{
			NodeInfo: corev1.NodeSystemInfo{
				OSImage:         "Linux",
				KernelVersion:   "6.6.0",
				KubeletVersion:  "v1.31.0",
				Architecture:    "amd64",
				OperatingSystem: "linux",
			},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue, LastHeartbeatTime: metav1.NewTime(time.Now())}},
			Addresses:  []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.10"}, {Type: corev1.NodeExternalIP, Address: "52.1.2.3"}},
		},
	}
	if err := nodeIndexer.Add(node); err != nil {
		t.Fatalf("failed to add node: %v", err)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "unbounded-net-node-a", Namespace: "kube-system"},
		Spec:       corev1.PodSpec{NodeName: "node-a"},
		Status:     corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{RestartCount: 2}}},
	}
	if err := podIndexer.Add(pod); err != nil {
		t.Fatalf("failed to add pod: %v", err)
	}

	siteInformer := cache.NewSharedIndexInformer(&cache.ListWatch{}, &unstructured.Unstructured{}, 0, cache.Indexers{})
	_ = siteInformer.GetStore().Add(&unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "site-a"},
		"spec":     map[string]interface{}{"nodeCidrs": []interface{}{"10.244.1.0/24"}},
		"status":   map[string]interface{}{"nodeCount": float64(1)},
	}})

	gatewayPoolInformer := cache.NewSharedIndexInformer(&cache.ListWatch{}, &unstructured.Unstructured{}, 0, cache.Indexers{})
	_ = gatewayPoolInformer.GetStore().Add(&unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "pool-a"},
		"spec":     map[string]interface{}{"nodeSelector": map[string]interface{}{"role": "gateway"}},
		"status": map[string]interface{}{
			"nodeCount": float64(1),
			"nodes":     []interface{}{map[string]interface{}{"name": "node-a", "siteName": "site-a"}},
		},
	}})

	cacheStore := NewNodeStatusCache()
	cacheStore.StoreFull("node-a", NodeStatusResponse{
		NodeInfo: NodeInfo{Name: "node-a", SiteName: "site-a", WireGuard: &WireGuardStatusInfo{Interface: "wg51820"}},
	}, "push")

	health := &healthState{
		clientset:           clientset,
		nodeLister:          corev1listers.NewNodeLister(nodeIndexer),
		podLister:           corev1listers.NewPodLister(podIndexer),
		siteInformer:        siteInformer,
		gatewayPoolInformer: gatewayPoolInformer,
		statusCache:         cacheStore,
		staleThreshold:      time.Minute,
		tokenAuth:           &tokenAuthenticator{tokenReviewer: clientset},
		azureTenantID:       "tenant-a",
	}

	status := fetchClusterStatus(context.Background(), health, false)
	if status == nil {
		t.Fatalf("expected status response")
	}

	if status.NodeCount != 1 || len(status.Nodes) != 1 {
		t.Fatalf("expected one node in status, got count=%d nodes=%d", status.NodeCount, len(status.Nodes))
	}

	if status.SiteCount != 1 || len(status.Sites) != 1 {
		t.Fatalf("expected one site in status, got siteCount=%d sites=%d", status.SiteCount, len(status.Sites))
	}

	if len(status.GatewayPools) != 1 || status.GatewayPools[0].Name != "pool-a" {
		t.Fatalf("expected gateway pool data from informer, got %#v", status.GatewayPools)
	}

	nodeStatus := status.Nodes[0]
	if nodeStatus.NodeInfo.Name != "node-a" || nodeStatus.NodeInfo.SiteName != "site-a" {
		t.Fatalf("unexpected node identity in status: %#v", nodeStatus.NodeInfo)
	}

	if nodeStatus.NodeInfo.K8sReady != "Ready" {
		t.Fatalf("expected node readiness to be Ready, got %q", nodeStatus.NodeInfo.K8sReady)
	}

	if len(nodeStatus.NodeInfo.PodCIDRs) == 0 || nodeStatus.NodeInfo.PodCIDRs[0] != "10.244.1.0/24" {
		t.Fatalf("expected pod CIDRs to be populated from node spec, got %#v", nodeStatus.NodeInfo.PodCIDRs)
	}

	if nodeStatus.NodeInfo.WireGuard == nil || nodeStatus.NodeInfo.WireGuard.PublicKey != "pub-key-a" {
		t.Fatalf("expected wireguard public key from node annotation, got %v", nodeStatus.NodeInfo.WireGuard)
	}

	if nodeStatus.NodePodInfo == nil || nodeStatus.NodePodInfo.PodName != "unbounded-net-node-a" {
		t.Fatalf("expected node pod info to be attached, got %#v", nodeStatus.NodePodInfo)
	}
}

// TestFetchClusterStatusInformerReadinessErrors tests FetchClusterStatusInformerReadinessErrors.
func TestFetchClusterStatusInformerReadinessErrors(t *testing.T) {
	clientset := k8sfake.NewClientset()
	health := &healthState{
		clientset:      clientset,
		statusCache:    NewNodeStatusCache(),
		staleThreshold: time.Minute,
		azureTenantID:  "tenant-a",
		tokenAuth: &tokenAuthenticator{
			tokenReviewer: clientset,
		},
	}

	status := fetchClusterStatus(context.Background(), health, false)
	if status == nil {
		t.Fatalf("expected non-nil status")
	}

	if status.AzureTenantID != "tenant-a" {
		t.Fatalf("expected tenant id to propagate, got %q", status.AzureTenantID)
	}

	if !slices.Contains(status.Errors, "informers not ready") {
		t.Fatalf("expected informer readiness error, got %#v", status.Errors)
	}

	if len(status.Problems) == 0 {
		t.Fatalf("expected problems to include informer readiness issue")
	}

	if status.PullEnabled {
		t.Fatalf("expected pullEnabled to reflect input=false")
	}
}

// TestFetchClusterStatusTokenVerifierError tests FetchClusterStatusTokenVerifierError.
func TestFetchClusterStatusTokenVerifierError(t *testing.T) {
	clientset := k8sfake.NewClientset()
	health := &healthState{
		clientset:      clientset,
		statusCache:    NewNodeStatusCache(),
		staleThreshold: time.Minute,
		tokenAuth:      &tokenAuthenticator{},
	}

	status := fetchClusterStatus(context.Background(), health, true)
	if status == nil {
		t.Fatalf("expected non-nil status")
	}

	foundTokenErr := false

	for _, errMsg := range status.Errors {
		if strings.Contains(errMsg, "token verifier not ready") {
			foundTokenErr = true
			break
		}
	}

	if !foundTokenErr {
		t.Fatalf("expected token verifier readiness error, got %#v", status.Errors)
	}

	if !slices.Contains(status.Errors, "informers not ready") {
		t.Fatalf("expected informer readiness error, got %#v", status.Errors)
	}
}

// TestFetchClusterStatusNodeListerError tests FetchClusterStatusNodeListerError.
func TestFetchClusterStatusNodeListerError(t *testing.T) {
	clientset := k8sfake.NewClientset()
	siteInformer := cache.NewSharedIndexInformer(&cache.ListWatch{}, &unstructured.Unstructured{}, 0, cache.Indexers{})

	health := &healthState{
		clientset:      clientset,
		nodeLister:     failingNodeLister{},
		siteInformer:   siteInformer,
		statusCache:    NewNodeStatusCache(),
		staleThreshold: time.Minute,
		tokenAuth:      &tokenAuthenticator{tokenReviewer: clientset},
	}

	status := fetchClusterStatus(context.Background(), health, true)
	if status == nil {
		t.Fatalf("expected non-nil status")
	}

	found := false

	for _, errMsg := range status.Errors {
		if strings.Contains(errMsg, "failed to list nodes") {
			found = true
			break
		}
	}

	if !found {
		t.Fatalf("expected failed-to-list-nodes error, got %#v", status.Errors)
	}
}

// TestFetchClusterStatusStaleCacheWhenPullDisabled tests FetchClusterStatusStaleCacheWhenPullDisabled.
func TestFetchClusterStatusStaleCacheWhenPullDisabled(t *testing.T) {
	clientset := k8sfake.NewClientset()
	nodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}
	if err := nodeIndexer.Add(node); err != nil {
		t.Fatalf("failed to add node: %v", err)
	}

	siteInformer := cache.NewSharedIndexInformer(&cache.ListWatch{}, &unstructured.Unstructured{}, 0, cache.Indexers{})
	cacheStore := NewNodeStatusCache()
	cacheStore.entries["node-a"] = &CachedNodeStatus{
		Status:     &NodeStatusResponse{NodeInfo: NodeInfo{Name: "node-a"}},
		ReceivedAt: time.Now().Add(-5 * time.Minute),
		Source:     "push",
		Revision:   1,
	}

	health := &healthState{
		clientset:      clientset,
		nodeLister:     corev1listers.NewNodeLister(nodeIndexer),
		siteInformer:   siteInformer,
		statusCache:    cacheStore,
		staleThreshold: 30 * time.Second,
		tokenAuth:      &tokenAuthenticator{tokenReviewer: clientset},
	}

	status := fetchClusterStatus(context.Background(), health, false)
	if status == nil || len(status.Nodes) != 1 {
		t.Fatalf("expected one node status result, got %#v", status)
	}

	nodeStatus := status.Nodes[0]
	if nodeStatus.StatusSource != "stale-cache" {
		t.Fatalf("expected stale-cache source, got %q", nodeStatus.StatusSource)
	}

	if !strings.Contains(nodeStatus.FetchError, "stale status") || !strings.Contains(nodeStatus.FetchError, "pull disabled") {
		t.Fatalf("expected stale pull-disabled fetch error, got %q", nodeStatus.FetchError)
	}
}

// TestFetchClusterStatusNoCacheMissingInternalIP tests FetchClusterStatusNoCacheMissingInternalIP.
func TestFetchClusterStatusNoCacheMissingInternalIP(t *testing.T) {
	clientset := k8sfake.NewClientset()

	nodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := nodeIndexer.Add(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}); err != nil {
		t.Fatalf("failed to add node: %v", err)
	}

	siteInformer := cache.NewSharedIndexInformer(&cache.ListWatch{}, &unstructured.Unstructured{}, 0, cache.Indexers{})
	health := &healthState{
		clientset:      clientset,
		nodeLister:     corev1listers.NewNodeLister(nodeIndexer),
		siteInformer:   siteInformer,
		statusCache:    NewNodeStatusCache(),
		staleThreshold: 30 * time.Second,
		tokenAuth:      &tokenAuthenticator{tokenReviewer: clientset},
	}

	status := fetchClusterStatus(context.Background(), health, true)
	if status == nil || len(status.Nodes) != 1 {
		t.Fatalf("expected one node status result, got %#v", status)
	}

	nodeStatus := status.Nodes[0]
	if nodeStatus.StatusSource != "no-data" {
		t.Fatalf("expected no-data status source, got %q", nodeStatus.StatusSource)
	}

	if nodeStatus.FetchError != "" {
		t.Fatalf("expected empty FetchError for no-data, got %q", nodeStatus.FetchError)
	}
}

// TestFetchClusterStatusNoCachePullDisabled tests FetchClusterStatusNoCachePullDisabled.
func TestFetchClusterStatusNoCachePullDisabled(t *testing.T) {
	clientset := k8sfake.NewClientset()

	nodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := nodeIndexer.Add(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}); err != nil {
		t.Fatalf("failed to add node: %v", err)
	}

	siteInformer := cache.NewSharedIndexInformer(&cache.ListWatch{}, &unstructured.Unstructured{}, 0, cache.Indexers{})
	health := &healthState{
		clientset:      clientset,
		nodeLister:     corev1listers.NewNodeLister(nodeIndexer),
		siteInformer:   siteInformer,
		statusCache:    NewNodeStatusCache(),
		staleThreshold: 30 * time.Second,
		tokenAuth:      &tokenAuthenticator{tokenReviewer: clientset},
	}

	status := fetchClusterStatus(context.Background(), health, false)
	if status == nil || len(status.Nodes) != 1 {
		t.Fatalf("expected one node status result, got %#v", status)
	}

	nodeStatus := status.Nodes[0]
	if nodeStatus.StatusSource != "no-data" {
		t.Fatalf("expected no-data status source, got %q", nodeStatus.StatusSource)
	}

	if nodeStatus.FetchError != "" {
		t.Fatalf("expected empty FetchError for no-data, got %q", nodeStatus.FetchError)
	}
}

// TestNodeReadinessAndLatestUpdateTime tests NodeReadinessAndLatestUpdateTime.
func TestNodeReadinessAndLatestUpdateTime(t *testing.T) {
	readyNode := &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	if !isNodeReady(readyNode) {
		t.Fatalf("expected ready node")
	}

	notReadyNode := &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}}}
	if isNodeReady(notReadyNode) {
		t.Fatalf("expected not-ready node")
	}

	if isNodeReady(&corev1.Node{}) {
		t.Fatalf("expected node without Ready condition to be not ready")
	}

	if !latestNodeUpdateTime(nil).IsZero() {
		t.Fatalf("expected zero time for nil node")
	}

	created := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	hb := created.Add(2 * time.Minute)
	transition := created.Add(4 * time.Minute)

	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(created)},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
			Type:               corev1.NodeReady,
			Status:             corev1.ConditionTrue,
			LastHeartbeatTime:  metav1.NewTime(hb),
			LastTransitionTime: metav1.NewTime(transition),
		}}},
	}
	if got := latestNodeUpdateTime(n); !got.Equal(transition) {
		t.Fatalf("expected latest transition time, got %v", got)
	}
}

// TestBuildConnectivityMatrix tests BuildConnectivityMatrix.
func TestBuildConnectivityMatrix(t *testing.T) {
	now := time.Now().Add(-75 * time.Second)

	nodes := []*NodeStatusResponse{
		{
			NodeInfo: NodeInfo{Name: "node-a", SiteName: "site-a", WireGuard: &WireGuardStatusInfo{Interface: "wg51820"}},
			Peers: []WireGuardPeerStatus{
				{Name: "node-b", PeerType: "site", HealthCheck: &HealthCheckPeerStatus{Status: "up", Uptime: "15s"}},
				{Name: "gw-a", PeerType: "gateway", SiteName: "site-a", Tunnel: PeerTunnelStatus{LastHandshake: now}},
				{Name: "gw-remote", PeerType: "gateway", SiteName: "site-b", Tunnel: PeerTunnelStatus{LastHandshake: now}},
			},
		},
		{
			NodeInfo: NodeInfo{Name: "node-b", SiteName: "site-a"},
			Peers: []WireGuardPeerStatus{
				{Name: "node-a", PeerType: "site", HealthCheck: &HealthCheckPeerStatus{Status: "down", Uptime: "3s"}},
			},
		},
	}
	for i := 0; i < 101; i++ {
		nodes = append(nodes, &NodeStatusResponse{NodeInfo: NodeInfo{Name: "big-" + strconv.Itoa(i), SiteName: "site-big"}})
	}

	gatewayPools := []GatewayPoolStatus{{
		Name:     "pool-a",
		Gateways: []string{"gw-a"},
	}}

	matrix := buildConnectivityMatrix(nodes, gatewayPools)
	if matrix == nil {
		t.Fatalf("expected non-nil connectivity matrix")
	}

	if _, ok := matrix["site-big"]; ok {
		t.Fatalf("expected site-big to be skipped when >100 nodes")
	}

	site := matrix["site-a"]
	if site == nil {
		t.Fatalf("expected site-a matrix")
	}

	if !slices.Equal(site.Nodes, []string{"gw-a", "node-a", "node-b"}) {
		t.Fatalf("unexpected node list: %#v", site.Nodes)
	}

	if got := site.Results["node-a"]["node-b"]; got != "up" {
		t.Fatalf("unexpected node-a->node-b status: %q", got)
	}

	gatewayCell := site.Results["node-a"]["gw-a"]
	if gatewayCell != "up" {
		t.Fatalf("unexpected gateway fallback cell: %q", gatewayCell)
	}

	if _, ok := site.Results["node-a"]["gw-remote"]; ok {
		t.Fatalf("did not expect remote-site gateway in site matrix")
	}

	if got := site.Results["node-a"]["node-a"]; got != "up" {
		t.Fatalf("expected self cell for node-a to be up from CNI health, got %q", got)
	}

	if got := site.Results["node-b"]["node-b"]; got != "" {
		t.Fatalf("expected self cell for node-b to be unknown when CNI health is unavailable, got %q", got)
	}

	pool := matrix["pool:pool-a"]
	if pool == nil {
		t.Fatalf("expected pool:pool-a matrix")
	}

	if !slices.Equal(pool.Nodes, []string{"gw-a", "node-a"}) {
		t.Fatalf("unexpected pool node list: %#v", pool.Nodes)
	}

	if got := pool.Results["node-a"]["gw-a"]; got != "up" {
		t.Fatalf("unexpected node-a->gw-a pool status: %q", got)
	}
}

// TestCollectClusterProblemsIncludesUnhealthySignals tests CollectClusterProblemsIncludesUnhealthySignals.
func TestCollectClusterProblemsIncludesUnhealthySignals(t *testing.T) {
	expectedTrue := true
	presentFalse := false
	now := time.Now()

	status := &ClusterStatusResponse{
		Errors: []string{"controller API server communication failed"},
		Nodes: []*NodeStatusResponse{
			{
				NodeInfo:     NodeInfo{Name: "node-a", K8sReady: "NotReady"},
				StatusSource: "apiserver-push",
				FetchError:   "stale status (5m old), pull failed: timeout",
				NodeErrors:   []NodeError{{Type: "directPush", Message: "dial tcp timeout"}},
				HealthCheck: &HealthCheckStatus{
					Healthy: false,
					Summary: "health check reported unhealthy",
				},
				RoutingTable: RoutingTableInfo{
					Routes: []RouteEntry{{
						Destination: "10.244.0.0/24",
						Family:      "IPv4",
						NextHops: []NextHop{{
							Expected: &expectedTrue,
							Present:  &presentFalse,
						}},
					}},
				},
				Peers: []WireGuardPeerStatus{
					{PeerType: "site", HealthCheck: &HealthCheckPeerStatus{Enabled: true, Status: "down"}},
					{PeerType: "gateway", Tunnel: PeerTunnelStatus{LastHandshake: now.Add(-10 * time.Minute)}},
				},
			},
		},
	}

	problems := collectClusterProblems(status)
	if len(problems) == 0 {
		t.Fatalf("expected non-empty problems")
	}

	assertProblemType := func(problemType string) StatusProblem {
		t.Helper()

		for _, problem := range problems {
			if problem.Type == problemType {
				return problem
			}
		}

		t.Fatalf("expected problem type %q in %#v", problemType, problems)

		return StatusProblem{}
	}

	controllerProblem := assertProblemType("controller")
	if controllerProblem.Name != "controller" {
		t.Fatalf("expected controller problem name, got %q", controllerProblem.Name)
	}

	if len(controllerProblem.Errors) == 0 {
		t.Fatalf("expected controller errors to be present")
	}

	nodeProblem := assertProblemType("node")
	if nodeProblem.Name != "node-a" {
		t.Fatalf("expected node problem name to be node-a, got %q", nodeProblem.Name)
	}

	if len(nodeProblem.Errors) < 6 {
		t.Fatalf("expected grouped node errors, got %#v", nodeProblem.Errors)
	}
}

// TestCollectClusterProblemsGatewayPoolProblemUsesPoolName tests CollectClusterProblemsGatewayPoolProblemUsesPoolName.
func TestCollectClusterProblemsGatewayPoolProblemUsesPoolName(t *testing.T) {
	status := &ClusterStatusResponse{
		Nodes: []*NodeStatusResponse{
			{
				NodeInfo: NodeInfo{Name: "gateway-a", WireGuard: &WireGuardStatusInfo{Interface: "wg51820"}},
			},
		},
		GatewayPools: []GatewayPoolStatus{
			{
				Name:      "pool-a",
				NodeCount: 2,
				Gateways:  []string{"gateway-a", "gateway-b"},
			},
		},
	}

	problems := collectClusterProblems(status)
	for _, problem := range problems {
		if problem.Type != "gatewayPool" {
			continue
		}

		if problem.Name != "pool-a" {
			t.Fatalf("expected gatewayPool problem name to be pool-a, got %q", problem.Name)
		}

		if len(problem.Errors) == 0 {
			t.Fatalf("expected gatewayPool problem errors, got %#v", problem.Errors)
		}

		return
	}

	t.Fatalf("expected gatewayPool problem in %#v", problems)
}

// TestFetchClusterStatusDeletedK8sNodeShowsMissing verifies that when a
// gateway node's K8s Node object is deleted but the node agent is still
// pushing status, the node appears in status.Nodes with K8sReady="Missing"
// rather than disappearing entirely.
func TestFetchClusterStatusDeletedK8sNodeShowsMissing(t *testing.T) {
	clientset := k8sfake.NewClientset()

	nodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	podIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})

	// Only gateway-a exists as a K8s node; gateway-b's Node object has been deleted.
	nodeA := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gateway-a",
			Labels: map[string]string{
				controllerpkg.SiteLabelKey: "site-a",
				"role":                     "gateway",
			},
			Annotations: map[string]string{
				controllerpkg.WireGuardPubKeyAnnotation: "pub-key-a",
			},
		},
		Spec: corev1.NodeSpec{
			PodCIDRs:   []string{"10.244.1.0/24"},
			ProviderID: "azure:///subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/gateway-a",
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue, LastHeartbeatTime: metav1.NewTime(time.Now())}},
			Addresses:  []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.10"}},
		},
	}
	if err := nodeIndexer.Add(nodeA); err != nil {
		t.Fatalf("failed to add node: %v", err)
	}

	siteInformer := cache.NewSharedIndexInformer(&cache.ListWatch{}, &unstructured.Unstructured{}, 0, cache.Indexers{})

	// The GatewayPool CRD still references both gateway-a and gateway-b.
	gatewayPoolInformer := cache.NewSharedIndexInformer(&cache.ListWatch{}, &unstructured.Unstructured{}, 0, cache.Indexers{})
	_ = gatewayPoolInformer.GetStore().Add(&unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "pool-a"},
		"spec":     map[string]interface{}{"nodeSelector": map[string]interface{}{"role": "gateway"}},
		"status": map[string]interface{}{
			"nodeCount": float64(2),
			"nodes": []interface{}{
				map[string]interface{}{"name": "gateway-a", "siteName": "site-a"},
				map[string]interface{}{"name": "gateway-b", "siteName": "site-a"},
			},
		},
	}})

	cacheStore := NewNodeStatusCache()
	cacheStore.StoreFull("gateway-a", NodeStatusResponse{
		NodeInfo: NodeInfo{Name: "gateway-a", SiteName: "site-a", WireGuard: &WireGuardStatusInfo{Interface: "wg51820"}},
	}, "push")
	// gateway-b's agent is still pushing status even though its K8s Node is gone.
	cacheStore.StoreFull("gateway-b", NodeStatusResponse{
		NodeInfo: NodeInfo{Name: "gateway-b", SiteName: "site-a", WireGuard: &WireGuardStatusInfo{Interface: "wg51820"}},
	}, "push")

	health := &healthState{
		clientset:           clientset,
		nodeLister:          corev1listers.NewNodeLister(nodeIndexer),
		podLister:           corev1listers.NewPodLister(podIndexer),
		siteInformer:        siteInformer,
		gatewayPoolInformer: gatewayPoolInformer,
		statusCache:         cacheStore,
		staleThreshold:      5 * time.Minute,
	}

	status := fetchClusterStatus(context.Background(), health, true)
	if status == nil {
		t.Fatalf("fetchClusterStatus() returned nil")
	}

	// Both nodes should appear in status.Nodes.
	if len(status.Nodes) != 2 {
		t.Fatalf("expected 2 nodes in status, got %d", len(status.Nodes))
	}

	nodeByName := make(map[string]*NodeStatusResponse, len(status.Nodes))
	for _, n := range status.Nodes {
		nodeByName[n.NodeInfo.Name] = n
	}

	gwA, ok := nodeByName["gateway-a"]
	if !ok {
		t.Fatal("expected gateway-a in status.Nodes")
	}

	if gwA.NodeInfo.K8sReady != "Ready" {
		t.Fatalf("expected gateway-a K8sReady=Ready, got %q", gwA.NodeInfo.K8sReady)
	}

	gwB, ok := nodeByName["gateway-b"]
	if !ok {
		t.Fatal("expected gateway-b in status.Nodes (agent still pushing)")
	}

	if gwB.NodeInfo.K8sReady != "Missing" {
		t.Fatalf("expected gateway-b K8sReady=Missing, got %q", gwB.NodeInfo.K8sReady)
	}

	// The gateway pool should still list both gateways.
	if len(status.GatewayPools) != 1 {
		t.Fatalf("expected 1 gateway pool, got %d", len(status.GatewayPools))
	}

	pool := status.GatewayPools[0]
	if !slices.Contains(pool.Gateways, "gateway-a") || !slices.Contains(pool.Gateways, "gateway-b") {
		t.Fatalf("expected both gateways in pool, got %v", pool.Gateways)
	}
}
