// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
)

// fakeInformer is a minimal cache.SharedIndexInformer for tests that only need GetStore().
type fakeInformer struct {
	cache.SharedIndexInformer
	store cache.Store
}

func (f *fakeInformer) GetStore() cache.Store { return f.store }
func (f *fakeInformer) HasSynced() bool       { return true }

type recordingRateLimitingQueue struct {
	workqueue.TypedRateLimitingInterface[string]
	delayed map[string]time.Duration
}

func (q *recordingRateLimitingQueue) AddAfter(item string, duration time.Duration) {
	q.delayed[item] = duration
}

func toGatewayPoolUnstructured(t *testing.T, pool *unboundednetv1alpha1.GatewayPool) *unstructured.Unstructured {
	t.Helper()

	data, err := json.Marshal(pool)
	if err != nil {
		t.Fatalf("marshal gateway pool: %v", err)
	}

	obj := map[string]interface{}{}
	if err := json.Unmarshal(data, &obj); err != nil {
		t.Fatalf("unmarshal gateway pool: %v", err)
	}

	return &unstructured.Unstructured{Object: obj}
}

func makeGatewayNode(name string, labels map[string]string, hasExternal bool, wgKey string) *corev1.Node {
	addresses := []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.10"}}
	if hasExternal {
		addresses = append(addresses, corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: "52.0.0.10"})
	}

	annotations := map[string]string{}
	if wgKey != "" {
		annotations[WireGuardPubKeyAnnotation] = wgKey
	}

	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Labels:      labels,
			Annotations: annotations,
		},
		Status: corev1.NodeStatus{Addresses: addresses},
	}
}

// TestParseAndNormalizeGatewayPoolHelpers tests parse and normalize gateway pool helpers.
func TestParseAndNormalizeGatewayPoolHelpers(t *testing.T) {
	src := &unboundednetv1alpha1.GatewayPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a"},
		Spec: unboundednetv1alpha1.GatewayPoolSpec{
			Type:         "External",
			NodeSelector: map[string]string{"role": "gateway"},
			RoutedCidrs:  []string{"100.64.0.0/16"},
		},
	}

	got, err := parseGatewayPool(toGatewayPoolUnstructured(t, src))
	if err != nil {
		t.Fatalf("parseGatewayPool() error = %v", err)
	}

	if got.Name != "pool-a" || got.Spec.Type != "External" {
		t.Fatalf("unexpected parsed gateway pool: %#v", got)
	}

	if normalizeGatewayPoolType("") != gatewayPoolTypeExternal {
		t.Fatalf("expected empty pool type to default to External")
	}

	if normalizeGatewayPoolType(gatewayPoolTypeInternal) != gatewayPoolTypeInternal {
		t.Fatalf("expected Internal pool type to be preserved")
	}
}

// TestNodeEligibilityAndPreferredPoolHelpers tests node eligibility and preferred pool helpers.
func TestNodeEligibilityAndPreferredPoolHelpers(t *testing.T) {
	now := time.Now()

	poolExternalA := &unboundednetv1alpha1.GatewayPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-b"},
		Spec: unboundednetv1alpha1.GatewayPoolSpec{
			Type:         gatewayPoolTypeExternal,
			NodeSelector: map[string]string{"role": "gateway"},
		},
	}
	poolExternalB := &unboundednetv1alpha1.GatewayPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a"},
		Spec: unboundednetv1alpha1.GatewayPoolSpec{
			Type:         gatewayPoolTypeExternal,
			NodeSelector: map[string]string{"role": "gateway"},
		},
	}
	poolInternal := &unboundednetv1alpha1.GatewayPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-internal"},
		Spec: unboundednetv1alpha1.GatewayPoolSpec{
			Type:         gatewayPoolTypeInternal,
			NodeSelector: map[string]string{"role": "gateway"},
		},
	}

	if nodeEligibleForGatewayPool(nil, poolExternalA, now) || nodeEligibleForGatewayPool(makeGatewayNode("n", nil, true, "pub"), nil, now) {
		t.Fatalf("expected nil node/pool to be ineligible")
	}

	if nodeEligibleForGatewayPool(makeGatewayNode("n", map[string]string{"role": "worker"}, true, "pub"), poolExternalA, now) {
		t.Fatalf("expected selector mismatch to be ineligible")
	}

	if nodeEligibleForGatewayPool(makeGatewayNode("n", map[string]string{"role": "gateway"}, false, "pub"), poolExternalA, now) {
		t.Fatalf("expected external pool without external IP to be ineligible")
	}

	discovered := makeGatewayNode("n", map[string]string{"role": "gateway"}, false, "pub")

	discovered.Annotations[unboundednetv1alpha1.NodeDiscoveredPublicIPAnnotation] = "203.0.113.10"

	discovered.Annotations[unboundednetv1alpha1.NodeDiscoveredPublicIPExpiresAtAnnotation] = now.Add(time.Hour).Format(time.RFC3339)
	if !nodeEligibleForGatewayPool(discovered, poolExternalA, now) {
		t.Fatal("expected STUN-discovered IP to make the node eligible")
	}

	if nodeEligibleForGatewayPool(makeGatewayNode("n", map[string]string{"role": "gateway"}, true, ""), poolExternalA, now) {
		t.Fatalf("expected missing wireguard key to be ineligible")
	}

	if !nodeEligibleForGatewayPool(makeGatewayNode("n", map[string]string{"role": "gateway", canonicalSiteLabelKey: "site-a"}, true, "pub"), poolExternalA, now) {
		t.Fatalf("expected eligible external gateway node")
	}

	if !nodeEligibleForGatewayPool(makeGatewayNode("n", map[string]string{"role": "gateway", canonicalSiteLabelKey: "site-a"}, false, "pub"), poolInternal, now) {
		t.Fatalf("expected internal gateway pool to allow node without external IP")
	}

	controller := &GatewayPoolController{
		poolsCache: []unboundednetv1alpha1.GatewayPool{*poolExternalA, *poolExternalB},
	}

	chosen, ok := controller.preferredGatewayPoolForNode(makeGatewayNode("node-a", map[string]string{"role": "gateway", canonicalSiteLabelKey: "site-a"}, true, "pub"), now)
	if !ok || chosen != "pool-a" {
		t.Fatalf("expected lexicographically first eligible pool, got ok=%v chosen=%s", ok, chosen)
	}

	if _, ok := controller.preferredGatewayPoolForNode(nil, now); ok {
		t.Fatalf("expected nil node to be ineligible")
	}
}

func TestSyncPoolSchedulesDiscoveryExpiryForFallbackPool(t *testing.T) {
	expiresAt := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	node := makeGatewayNode("node-a", map[string]string{"role": "gateway"}, false, "pub")
	node.Annotations[unboundednetv1alpha1.NodeDiscoveredPublicIPAnnotation] = "203.0.113.10"
	node.Annotations[unboundednetv1alpha1.NodeDiscoveredPublicIPExpiresAtAnnotation] = expiresAt.Format(time.RFC3339)

	externalPool := &unboundednetv1alpha1.GatewayPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a"},
		Spec: unboundednetv1alpha1.GatewayPoolSpec{
			Type:         gatewayPoolTypeExternal,
			NodeSelector: map[string]string{"role": "gateway"},
		},
	}
	internalPool := &unboundednetv1alpha1.GatewayPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-b"},
		Spec: unboundednetv1alpha1.GatewayPoolSpec{
			Type:         gatewayPoolTypeInternal,
			NodeSelector: map[string]string{"role": "gateway"},
		},
	}

	poolStore := cache.NewStore(cache.MetaNamespaceKeyFunc)
	for _, pool := range []*unboundednetv1alpha1.GatewayPool{externalPool, internalPool} {
		if err := poolStore.Add(toGatewayPoolUnstructured(t, pool)); err != nil {
			t.Fatalf("add gateway pool %s: %v", pool.Name, err)
		}
	}

	nodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := nodeIndexer.Add(node); err != nil {
		t.Fatalf("add node: %v", err)
	}

	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]())
	defer queue.ShutDown()

	recordingQueue := &recordingRateLimitingQueue{
		TypedRateLimitingInterface: queue,
		delayed:                    map[string]time.Duration{},
	}
	gc := &GatewayPoolController{
		dynamicClient: fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
			gatewayNodeGVR: "GatewayPoolNodeList",
		}),
		nodeLister:          corev1listers.NewNodeLister(nodeIndexer),
		gatewayPoolInformer: &fakeInformer{store: poolStore},
		gatewayNodeInformer: &fakeInformer{store: cache.NewStore(cache.MetaNamespaceKeyFunc)},
		workqueue:           recordingQueue,
	}

	if err := gc.syncPool(t.Context(), internalPool.Name); err != nil {
		t.Fatalf("sync fallback pool: %v", err)
	}

	if delay, ok := recordingQueue.delayed[internalPool.Name]; !ok || delay <= 0 {
		t.Fatalf("fallback pool expiry delay = %v, scheduled = %v", delay, ok)
	}

	if preferred, ok := gc.preferredGatewayPoolForNode(node, expiresAt.Add(-time.Minute)); !ok || preferred != externalPool.Name {
		t.Fatalf("preferred pool before expiry = %q, ok = %v", preferred, ok)
	}

	if preferred, ok := gc.preferredGatewayPoolForNode(node, expiresAt); !ok || preferred != internalPool.Name {
		t.Fatalf("preferred pool at expiry = %q, ok = %v", preferred, ok)
	}
}

// TestGatewayNodeIPAndEqualityHelpers tests gateway node ipand equality helpers.
func TestGatewayNodeIPAndEqualityHelpers(t *testing.T) {
	nodeA := makeGatewayNode("node-a", map[string]string{"role": "gateway"}, true, "pub")
	nodeB := nodeA.DeepCopy()
	nodeB.Status.Addresses = []corev1.NodeAddress{
		{Type: corev1.NodeExternalIP, Address: "52.0.0.10"},
		{Type: corev1.NodeInternalIP, Address: "10.0.0.10"},
	}

	ext := getNodeExternalIPs(nodeA)

	ints := getNodeInternalIPs(nodeA)
	if len(ext) != 1 || len(ints) != 1 {
		t.Fatalf("unexpected extracted IPs ext=%#v int=%#v", ext, ints)
	}

	if !nodeExternalIPsEqual(nodeA, nodeB) || !nodeInternalIPsEqual(nodeA, nodeB) {
		t.Fatalf("expected IP equality helpers to ignore ordering")
	}

	nodeB.Status.Addresses[0].Address = "52.0.0.11"
	if nodeExternalIPsEqual(nodeA, nodeB) {
		t.Fatalf("expected changed external IPs to differ")
	}

	nodesA := []unboundednetv1alpha1.GatewayNodeInfo{{
		Name:                 "node-a",
		SiteName:             "site-a",
		WireGuardPublicKey:   "pub",
		GatewayWireguardPort: 51821,
		InternalIPs:          []string{"10.0.0.10"},
		ExternalIPs:          []string{"52.0.0.10"},
		HealthEndpoints:      []string{"10.244.1.1"},
		PodCIDRs:             []string{"10.244.1.0/24"},
	}}

	nodesB := []unboundednetv1alpha1.GatewayNodeInfo{{
		Name:                 "node-a",
		SiteName:             "site-a",
		WireGuardPublicKey:   "pub",
		GatewayWireguardPort: 51821,
		InternalIPs:          []string{"10.0.0.10"},
		ExternalIPs:          []string{"52.0.0.10"},
		HealthEndpoints:      []string{"10.244.1.1"},
		PodCIDRs:             []string{"10.244.1.0/24"},
	}}
	if !gatewayNodesEqual(nodesA, nodesB) {
		t.Fatalf("expected gateway node slices to be equal")
	}

	nodesB[0].PodCIDRs = []string{"10.244.2.0/24"}
	if gatewayNodesEqual(nodesA, nodesB) {
		t.Fatalf("expected changed gateway node slices to differ")
	}
}

func TestResolveNodeExternalIPs(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 22, 0, 0, 0, 0, time.UTC)
	validExpiry := now.Add(time.Hour)
	expired := now.Add(-time.Minute)
	declaredKey := unboundednetv1alpha1.NodeDeclaredPublicIPAnnotation
	discoveredKey := unboundednetv1alpha1.NodeDiscoveredPublicIPAnnotation
	expiresAtKey := unboundednetv1alpha1.NodeDiscoveredPublicIPExpiresAtAnnotation
	node := func(externalIP string, annotations map[string]string) *corev1.Node {
		result := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Annotations: annotations}}
		if externalIP != "" {
			result.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeExternalIP, Address: externalIP}}
		}

		return result
	}

	tests := []struct {
		name       string
		node       *corev1.Node
		want       []string
		wantExpiry time.Time
		wantErr    bool
	}{
		{
			name: "declared address takes precedence over provider and discovery",
			node: node("2001:0db8:0:0::10", map[string]string{declaredKey: "203.0.113.20", discoveredKey: "203.0.113.30"}),
			want: []string{"203.0.113.20"},
		},
		{
			name: "provider takes precedence over discovery",
			node: node("2001:0db8:0:0::10", map[string]string{discoveredKey: "203.0.113.30", expiresAtKey: validExpiry.Format(time.RFC3339)}),
			want: []string{"2001:0db8:0:0::10"},
		},
		{
			name: "provider address preserves legacy behavior",
			node: node("not-an-ip", nil),
			want: []string{"not-an-ip"},
		},
		{
			name:       "discovery is used without a declared or provider address",
			node:       node("", map[string]string{discoveredKey: "2001:0db8:0:0::10", expiresAtKey: validExpiry.Format(time.RFC3339)}),
			want:       []string{"2001:db8::10"},
			wantExpiry: validExpiry,
		},
		{
			name: "declared address takes precedence over discovery",
			node: node("", map[string]string{declaredKey: "203.0.113.20", discoveredKey: "203.0.113.30"}),
			want: []string{"203.0.113.20"},
		},
		{
			name: "IPv4-mapped IPv6 declared address is normalized",
			node: node("", map[string]string{declaredKey: "::ffff:192.0.2.10"}),
			want: []string{"192.0.2.10"},
		},
		{
			name:    "invalid declared address blocks provider and discovery fallback",
			node:    node("192.0.2.20", map[string]string{declaredKey: "not-an-ip", discoveredKey: "203.0.113.30"}),
			wantErr: true,
		},
		{
			name:    "IPv6 zone is rejected",
			node:    node("", map[string]string{declaredKey: "fe80::1%eth0"}),
			wantErr: true,
		},
		{
			name:    "long invalid declared address has a bounded error",
			node:    node("", map[string]string{declaredKey: strings.Repeat("x", 4096)}),
			wantErr: true,
		},
		{
			name:    "empty declared address blocks provider and discovery fallback",
			node:    node("192.0.2.20", map[string]string{declaredKey: "", discoveredKey: "203.0.113.30"}),
			wantErr: true,
		},
		{
			name:    "discovery without expiry is rejected",
			node:    node("", map[string]string{discoveredKey: "203.0.113.30"}),
			wantErr: true,
		},
		{
			name:    "discovery with invalid expiry is rejected",
			node:    node("", map[string]string{discoveredKey: "203.0.113.30", expiresAtKey: "not-a-time"}),
			wantErr: true,
		},
		{
			name:    "long invalid expiry has a bounded error",
			node:    node("", map[string]string{discoveredKey: "203.0.113.30", expiresAtKey: strings.Repeat("x", 4096)}),
			wantErr: true,
		},
		{
			name:       "expired discovery is ignored",
			node:       node("", map[string]string{discoveredKey: "203.0.113.30", expiresAtKey: expired.Format(time.RFC3339)}),
			wantExpiry: expired,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, expiry, err := ResolveNodeExternalIPsAt(tt.node, now)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ResolveNodeExternalIPsAt() error = %v, wantErr %v", err, tt.wantErr)
			}

			if err != nil && len(err.Error()) > 256 {
				t.Fatalf("ResolveNodeExternalIPsAt() error length = %d, want at most 256: %v", len(err.Error()), err)
			}

			if !slices.Equal(got, tt.want) {
				t.Fatalf("ResolveNodeExternalIPsAt() = %#v, want %#v", got, tt.want)
			}

			if !expiry.Equal(tt.wantExpiry) {
				t.Fatalf("expiry = %s, want %s", expiry, tt.wantExpiry)
			}
		})
	}
}

func TestDiscoveredPublicIPChangeAffectsPools(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 22, 0, 0, 0, 0, time.UTC)
	node := func(ip string, expiry time.Time) *corev1.Node {
		annotations := map[string]string{}

		if ip != "" {
			annotations[unboundednetv1alpha1.NodeDiscoveredPublicIPAnnotation] = ip
		}

		if !expiry.IsZero() {
			annotations[unboundednetv1alpha1.NodeDiscoveredPublicIPExpiresAtAnnotation] = expiry.Format(time.RFC3339)
		}

		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Annotations: annotations}}
	}
	emptyIP := node("", time.Time{})
	emptyIP.Annotations[unboundednetv1alpha1.NodeDiscoveredPublicIPAnnotation] = ""

	tests := []struct {
		name string
		old  *corev1.Node
		new  *corev1.Node
		want bool
	}{
		{name: "IP changed", old: node("203.0.113.10", now.Add(time.Hour)), new: node("203.0.113.11", now.Add(time.Hour)), want: true},
		{name: "empty IP added", old: node("", time.Time{}), new: emptyIP, want: true},
		{name: "expiry added", old: node("203.0.113.10", time.Time{}), new: node("203.0.113.10", now.Add(time.Hour)), want: true},
		{name: "valid expiry extended", old: node("203.0.113.10", now.Add(time.Hour)), new: node("203.0.113.10", now.Add(2*time.Hour))},
		{name: "valid expiry shortened", old: node("203.0.113.10", now.Add(2*time.Hour)), new: node("203.0.113.10", now.Add(time.Hour)), want: true},
		{name: "expired to valid", old: node("203.0.113.10", now.Add(-time.Minute)), new: node("203.0.113.10", now.Add(time.Hour)), want: true},
		{name: "unchanged", old: node("203.0.113.10", now.Add(time.Hour)), new: node("203.0.113.10", now.Add(time.Hour))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := discoveredPublicIPChangeAffectsPools(tt.old, tt.new, now); got != tt.want {
				t.Fatalf("discoveredPublicIPChangeAffectsPools() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestGetHealthIPsFromPodCIDRs tests get health ips from pod cidrs.
func TestGetHealthIPsFromPodCIDRs(t *testing.T) {
	got := getHealthIPsFromPodCIDRs([]string{"10.244.1.0/24", "fd00::/64", "invalid"})
	if len(got) != 2 {
		t.Fatalf("expected two valid health IPs, got %#v", got)
	}

	if got[0] != "10.244.1.1" {
		t.Fatalf("unexpected IPv4 health IP: %s", got[0])
	}

	if got[1] != "fd00::1" {
		t.Fatalf("unexpected IPv6 health IP: %s", got[1])
	}
}

// TestGatewayPortAllocationReleaseAndSeed tests gateway port allocation release and seed.
func TestGatewayPortAllocationReleaseAndSeed(t *testing.T) {
	gc := &GatewayPoolController{
		gwPortAllocated: map[int32]string{},
		gwPortByNode:    map[string]int32{},
	}

	if got := gc.allocateGatewayPort("node-a"); got != gatewayWireguardPortBase {
		t.Fatalf("expected first allocated port %d, got %d", gatewayWireguardPortBase, got)
	}

	if got := gc.allocateGatewayPort("node-b"); got != gatewayWireguardPortBase+1 {
		t.Fatalf("expected second allocated port %d, got %d", gatewayWireguardPortBase+1, got)
	}

	if got := gc.allocateGatewayPort("node-a"); got != gatewayWireguardPortBase {
		t.Fatalf("expected stable allocation for existing node, got %d", got)
	}

	gc.releaseGatewayPort("node-a")

	if got := gc.allocateGatewayPort("node-c"); got != gatewayWireguardPortBase {
		t.Fatalf("expected released port to be reusable, got %d", got)
	}

	pool := unboundednetv1alpha1.GatewayPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a"},
		Status: unboundednetv1alpha1.GatewayPoolStatus{
			Nodes: []unboundednetv1alpha1.GatewayNodeInfo{{Name: "node-status", GatewayWireguardPort: 52000}},
		},
	}

	nodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := nodeIndexer.Add(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-annotated", Annotations: map[string]string{WireGuardPortAnnotation: "52001"}}}); err != nil {
		t.Fatalf("failed adding node-annotated: %v", err)
	}

	if err := nodeIndexer.Add(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-conflict", Annotations: map[string]string{WireGuardPortAnnotation: "52000"}}}); err != nil {
		t.Fatalf("failed adding node-conflict: %v", err)
	}

	if err := nodeIndexer.Add(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-invalid", Annotations: map[string]string{WireGuardPortAnnotation: "bad"}}}); err != nil {
		t.Fatalf("failed adding node-invalid: %v", err)
	}

	gc.poolsCache = []unboundednetv1alpha1.GatewayPool{pool}
	gc.nodeLister = corev1listers.NewNodeLister(nodeIndexer)
	gc.seedGatewayPorts()

	if got := gc.gwPortByNode["node-status"]; got != 52000 {
		t.Fatalf("expected status node seeded port 52000, got %d", got)
	}

	if got := gc.gwPortByNode["node-annotated"]; got != 52001 {
		t.Fatalf("expected annotated node seeded port 52001, got %d", got)
	}

	if _, ok := gc.gwPortByNode["node-conflict"]; ok {
		t.Fatalf("expected conflicting annotated port not to be allocated")
	}

	if _, ok := gc.gwPortByNode["node-invalid"]; ok {
		t.Fatalf("expected invalid annotated port not to be allocated")
	}
}

// TestSetAndRemoveWireGuardPortAnnotation tests set and remove wire guard port annotation.
func TestSetAndRemoveWireGuardPortAnnotation(t *testing.T) {
	sourceNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}
	client := kubefake.NewClientset(sourceNode)
	gc := &GatewayPoolController{clientset: client}

	if err := gc.setWireGuardPortAnnotation(context.Background(), sourceNode, 51821); err != nil {
		t.Fatalf("setWireGuardPortAnnotation() error = %v", err)
	}

	node, err := client.CoreV1().Nodes().Get(context.Background(), "node-a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get node after set annotation: %v", err)
	}

	if got := node.Annotations[WireGuardPortAnnotation]; got != "51821" {
		t.Fatalf("expected annotation value 51821, got %q", got)
	}

	client.ClearActions()

	if err := gc.setWireGuardPortAnnotation(context.Background(), node, 51821); err != nil {
		t.Fatalf("setWireGuardPortAnnotation() no-op error = %v", err)
	}

	if actions := client.Actions(); len(actions) != 0 {
		t.Fatalf("unchanged wireguard port emitted API actions: %#v", actions)
	}

	if err := gc.removeWireGuardPortAnnotation(context.Background(), "node-a"); err != nil {
		t.Fatalf("removeWireGuardPortAnnotation() error = %v", err)
	}

	node, err = client.CoreV1().Nodes().Get(context.Background(), "node-a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get node after remove annotation: %v", err)
	}

	if _, ok := node.Annotations[WireGuardPortAnnotation]; ok {
		t.Fatalf("expected wireguard annotation to be removed")
	}
}

// TestEnqueueHelpersAndPoolsCacheUpdate tests enqueue helpers and pools cache update.
func TestEnqueueHelpersAndPoolsCacheUpdate(t *testing.T) {
	q := workqueue.NewTypedRateLimitingQueueWithConfig(workqueue.DefaultTypedControllerRateLimiter[string](), workqueue.TypedRateLimitingQueueConfig[string]{Name: "gatewaypool-test"})
	defer q.ShutDown()

	informer := cache.NewSharedIndexInformer(&cache.ListWatch{}, &unstructured.Unstructured{}, 0, cache.Indexers{})
	valid := toGatewayPoolUnstructured(t, &unboundednetv1alpha1.GatewayPool{ObjectMeta: metav1.ObjectMeta{Name: "pool-a"}})
	invalid := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "net.unbounded-cloud.io/v1alpha1",
		"kind":       "GatewayPool",
		"metadata":   map[string]interface{}{"name": "pool-bad"},
		"spec":       map[string]interface{}{"nodeSelector": "invalid-type"},
	}}

	if err := informer.GetStore().Add(valid); err != nil {
		t.Fatalf("failed adding valid gateway pool to store: %v", err)
	}

	if err := informer.GetStore().Add(invalid); err != nil {
		t.Fatalf("failed adding invalid gateway pool to store: %v", err)
	}

	gc := &GatewayPoolController{workqueue: q, gatewayPoolInformer: informer}

	gc.enqueuePool("not-unstructured")

	if got := q.Len(); got != 0 {
		t.Fatalf("expected queue length 0 for non-unstructured enqueue, got %d", got)
	}

	gc.enqueuePool(valid)

	if got := q.Len(); got != 1 {
		t.Fatalf("expected queue length 1 after enqueuePool valid object, got %d", got)
	}

	gc.updatePoolsCache()

	if len(gc.poolsCache) != 1 || gc.poolsCache[0].Name != "pool-a" {
		t.Fatalf("expected only valid gateway pool cached, got %#v", gc.poolsCache)
	}
}

func TestNodeFromDeleteEvent(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}

	for _, tt := range []struct {
		name string
		obj  interface{}
		want *corev1.Node
	}{
		{
			name: "node",
			obj:  node,
			want: node,
		},
		{
			name: "tombstone",
			obj:  cache.DeletedFinalStateUnknown{Obj: node},
			want: node,
		},
		{
			name: "malformed tombstone",
			obj:  cache.DeletedFinalStateUnknown{Obj: &corev1.Pod{}},
		},
		{
			name: "unexpected object",
			obj:  &corev1.Pod{},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := nodeFromDeleteEvent(tt.obj); got != tt.want {
				t.Fatalf("nodeFromDeleteEvent() = %p, want %p", got, tt.want)
			}
		})
	}
}

func TestEnqueuePoolsForNodesUsesOldAndNewSelectors(t *testing.T) {
	q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]())
	defer q.ShutDown()

	informer := cache.NewSharedIndexInformer(&cache.ListWatch{}, &unstructured.Unstructured{}, 0, cache.Indexers{})

	for name, role := range map[string]string{
		"pool-old":   "old",
		"pool-new":   "new",
		"pool-other": "other",
	} {
		pool := &unboundednetv1alpha1.GatewayPool{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       unboundednetv1alpha1.GatewayPoolSpec{NodeSelector: map[string]string{"role": role}},
		}
		if err := informer.GetStore().Add(toGatewayPoolUnstructured(t, pool)); err != nil {
			t.Fatalf("add %s to store: %v", name, err)
		}
	}

	gc := &GatewayPoolController{workqueue: q, gatewayPoolInformer: informer}
	gc.enqueuePoolsForNodes(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"role": "old"}}})

	if got := q.Len(); got != 0 {
		t.Fatalf("queued %d pools before informer sync, want 0", got)
	}

	gc.hasSynced.Store(true)
	gc.enqueuePoolsForNodes(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"role": "old"}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"role": "new"}}},
	)

	if got := q.Len(); got != 2 {
		t.Fatalf("queued pools = %d, want 2", got)
	}

	queued := map[string]bool{}

	for range 2 {
		key, _ := q.Get()
		queued[key] = true
		q.Done(key)
	}

	if !queued["pool-old"] || !queued["pool-new"] || queued["pool-other"] {
		t.Fatalf("queued pools = %v, want pool-old and pool-new", queued)
	}
}

// TestUpdateGatewayPoolStatusPatchPaths tests update gateway pool status patch paths.
func TestUpdateGatewayPoolStatusPatchPaths(t *testing.T) {
	scheme := runtime.NewScheme()
	client := fake.NewSimpleDynamicClient(scheme, &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "net.unbounded-cloud.io/v1alpha1",
		"kind":       "GatewayPool",
		"metadata":   map[string]interface{}{"name": "pool-a"},
	}})
	gc := &GatewayPoolController{dynamicClient: client}

	status := unboundednetv1alpha1.GatewayPoolStatus{
		NodeCount: 1,
		Nodes: []unboundednetv1alpha1.GatewayNodeInfo{{
			Name:                 "node-a",
			SiteName:             "site-a",
			WireGuardPublicKey:   "pub",
			GatewayWireguardPort: 51821,
			InternalIPs:          []string{"10.0.0.10"},
			ExternalIPs:          []string{"52.0.0.10"},
			HealthEndpoints:      []string{"10.244.1.1"},
			PodCIDRs:             []string{"10.244.1.0/24"},
		}},
	}

	if err := gc.updateGatewayPoolStatus(context.Background(), "pool-a", status); err != nil {
		t.Fatalf("updateGatewayPoolStatus() error = %v", err)
	}

	obj, err := client.Resource(gatewayPoolGVR).Get(context.Background(), "pool-a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed fetching patched gateway pool: %v", err)
	}

	nodeCount, found, err := unstructured.NestedInt64(obj.Object, "status", "nodeCount")
	if err != nil || !found {
		t.Fatalf("expected status.nodeCount to be set, found=%v err=%v", found, err)
	}

	if nodeCount != 1 {
		t.Fatalf("expected status.nodeCount=1, got %d", nodeCount)
	}

	// NotFound should be treated as non-fatal by the update helper.
	if err := gc.updateGatewayPoolStatus(context.Background(), "pool-missing", unboundednetv1alpha1.GatewayPoolStatus{}); err != nil {
		t.Fatalf("expected missing pool status update to be ignored, got error %v", err)
	}
}

// TestCleanupStaleGatewayPoolNodesNilPool tests cleanup stale gateway pool nodes nil pool.
func TestCleanupStaleGatewayPoolNodesNilPool(t *testing.T) {
	scheme := runtime.NewScheme()
	client := fake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		gatewayNodeGVR: "GatewayPoolNodeList",
	})
	gwNodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	gwNodeInformer := &fakeInformer{store: gwNodeIndexer}
	gc := &GatewayPoolController{dynamicClient: client, gatewayNodeInformer: gwNodeInformer}

	if err := gc.cleanupStaleGatewayPoolNodes(context.Background(), nil, nil, time.Now()); err != nil {
		t.Fatalf("cleanupStaleGatewayPoolNodes(nil) error = %v", err)
	}

	if err := gc.cleanupStaleGatewayPoolNodes(context.Background(), &unboundednetv1alpha1.GatewayPool{}, nil, time.Now()); err != nil {
		t.Fatalf("cleanupStaleGatewayPoolNodes(empty pool) error = %v", err)
	}

	nodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	gc.nodeLister = corev1listers.NewNodeLister(nodeIndexer)

	pool := &unboundednetv1alpha1.GatewayPool{ObjectMeta: metav1.ObjectMeta{Name: "pool-a"}, Spec: unboundednetv1alpha1.GatewayPoolSpec{NodeSelector: map[string]string{"role": "gw"}}}
	if err := gc.cleanupStaleGatewayPoolNodes(context.Background(), pool, []unboundednetv1alpha1.GatewayNodeInfo{{Name: "kept"}}, time.Now()); err != nil {
		t.Fatalf("cleanupStaleGatewayPoolNodes(non-empty) error = %v", err)
	}
}

func TestEnsureGatewayPoolNodeCreatePatchAndNoOp(t *testing.T) {
	scheme := runtime.NewScheme()
	pool := &unboundednetv1alpha1.GatewayPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", UID: "pool-uid"},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a", UID: "node-uid"},
	}

	emptyNodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	emptyNodeInformer := &fakeInformer{store: emptyNodeIndexer}

	clientCreate := fake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		gatewayNodeGVR: "GatewayPoolNodeList",
	})

	gcCreate := &GatewayPoolController{dynamicClient: clientCreate, gatewayNodeInformer: emptyNodeInformer}
	if err := gcCreate.ensureGatewayPoolNode(context.Background(), pool, node, "site-a"); err != nil {
		t.Fatalf("ensureGatewayPoolNode(create) error = %v", err)
	}

	created, err := clientCreate.Resource(gatewayNodeGVR).Get(context.Background(), "node-a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed fetching created gateway pool node: %v", err)
	}

	if got, _, _ := unstructured.NestedString(created.Object, "spec", "gatewayPool"); got != "pool-a" {
		t.Fatalf("expected created spec.gatewayPool=pool-a, got %q", got)
	}

	existingObj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "net.unbounded-cloud.io/v1alpha1",
		"kind":       "GatewayPoolNode",
		"metadata": map[string]interface{}{
			"name": "node-a",
			"labels": map[string]interface{}{
				"example.com/unowned": "preserved",
			},
		},
		"spec": map[string]interface{}{
			"gatewayPool": "old-pool",
		},
	}}
	patchNodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	_ = patchNodeIndexer.Add(existingObj)
	patchNodeInformer := &fakeInformer{store: patchNodeIndexer}

	clientPatch := fake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		gatewayNodeGVR: "GatewayPoolNodeList",
	}, existingObj)

	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]())
	defer queue.ShutDown()

	gcPatch := &GatewayPoolController{dynamicClient: clientPatch, gatewayNodeInformer: patchNodeInformer, workqueue: queue}
	if err := gcPatch.ensureGatewayPoolNode(context.Background(), pool, node, "site-a"); err != nil {
		t.Fatalf("ensureGatewayPoolNode(patch) error = %v", err)
	}

	if got := queue.Len(); got != 1 {
		t.Fatalf("queued pools after ownership change = %d, want 1", got)
	}

	queuedPool, shutdown := queue.Get()
	if shutdown {
		t.Fatal("workqueue shut down before previous pool was retrieved")
	}

	queue.Done(queuedPool)

	if queuedPool != "old-pool" {
		t.Fatalf("queued pool after ownership change = %q, want %q", queuedPool, "old-pool")
	}

	patched, err := clientPatch.Resource(gatewayNodeGVR).Get(context.Background(), "node-a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed fetching patched gateway pool node: %v", err)
	}

	if got, _, _ := unstructured.NestedString(patched.Object, "spec", "gatewayPool"); got != "pool-a" {
		t.Fatalf("expected patched spec.gatewayPool=pool-a, got %q", got)
	}

	if err := patchNodeIndexer.Update(patched); err != nil {
		t.Fatalf("update informer cache: %v", err)
	}

	clientPatch.ClearActions()

	if err := gcPatch.ensureGatewayPoolNode(context.Background(), pool, node, "site-a"); err != nil {
		t.Fatalf("ensureGatewayPoolNode(no-op) error = %v", err)
	}

	if actions := clientPatch.Actions(); len(actions) != 0 {
		t.Fatalf("converged GatewayPoolNode emitted API actions: %#v", actions)
	}

	if got := queue.Len(); got != 0 {
		t.Fatalf("converged GatewayPoolNode queued %d pools, want 0", got)
	}

	withoutSite := patched.DeepCopy()
	if err := unstructured.SetNestedField(withoutSite.Object, "", "spec", "site"); err != nil {
		t.Fatalf("clear informer site: %v", err)
	}

	if err := patchNodeIndexer.Update(withoutSite); err != nil {
		t.Fatalf("update informer cache without site: %v", err)
	}

	clientPatch.ClearActions()

	if err := gcPatch.ensureGatewayPoolNode(context.Background(), pool, node, ""); err != nil {
		t.Fatalf("ensureGatewayPoolNode(empty site no-op) error = %v", err)
	}

	if actions := clientPatch.Actions(); len(actions) != 0 {
		t.Fatalf("empty site name rewrote existing canonical site label: %#v", actions)
	}

	gcNil := &GatewayPoolController{dynamicClient: clientCreate, gatewayNodeInformer: emptyNodeInformer}
	if err := gcNil.ensureGatewayPoolNode(context.Background(), nil, node, "site-a"); err != nil {
		t.Fatalf("ensureGatewayPoolNode(nil pool) error = %v", err)
	}

	if err := gcNil.ensureGatewayPoolNode(context.Background(), pool, nil, "site-a"); err != nil {
		t.Fatalf("ensureGatewayPoolNode(nil node) error = %v", err)
	}
}

// TestCleanupStaleGatewayPoolNodesDeleteAndPreserve tests cleanup stale gateway pool nodes delete and preserve.
func TestCleanupStaleGatewayPoolNodesDeleteAndPreserve(t *testing.T) {
	scheme := runtime.NewScheme()
	pool := &unboundednetv1alpha1.GatewayPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a"},
		Spec:       unboundednetv1alpha1.GatewayPoolSpec{NodeSelector: map[string]string{"role": "gw"}, Type: gatewayPoolTypeInternal},
	}

	client := fake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		gatewayNodeGVR: "GatewayPoolNodeList",
	}, &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "net.unbounded-cloud.io/v1alpha1",
		"kind":       "GatewayPoolNode",
		"metadata": map[string]interface{}{
			"name": "stale-delete",
		},
		"spec": map[string]interface{}{
			"gatewayPool": "pool-a",
		},
	}}, &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "net.unbounded-cloud.io/v1alpha1",
		"kind":       "GatewayPoolNode",
		"metadata": map[string]interface{}{
			"name": "stale-preserve",
		},
		"spec": map[string]interface{}{
			"gatewayPool": "pool-a",
		},
	}})

	nodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := nodeIndexer.Add(&corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:        "stale-preserve",
		Labels:      map[string]string{"role": "gw"},
		Annotations: map[string]string{WireGuardPubKeyAnnotation: "pub"},
	}}); err != nil {
		t.Fatalf("failed adding stale-preserve node: %v", err)
	}

	gwNodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	staleDelete := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "net.unbounded-cloud.io/v1alpha1",
		"kind":       "GatewayPoolNode",
		"metadata":   map[string]interface{}{"name": "stale-delete"},
		"spec":       map[string]interface{}{"gatewayPool": "pool-a"},
	}}
	stalePreserve := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "net.unbounded-cloud.io/v1alpha1",
		"kind":       "GatewayPoolNode",
		"metadata":   map[string]interface{}{"name": "stale-preserve"},
		"spec":       map[string]interface{}{"gatewayPool": "pool-a"},
	}}
	_ = gwNodeIndexer.Add(staleDelete)
	_ = gwNodeIndexer.Add(stalePreserve)
	gwNodeInf := &fakeInformer{store: gwNodeIndexer}

	gc := &GatewayPoolController{
		dynamicClient:       client,
		nodeLister:          corev1listers.NewNodeLister(nodeIndexer),
		poolsCache:          []unboundednetv1alpha1.GatewayPool{*pool},
		gatewayNodeInformer: gwNodeInf,
	}

	if err := gc.cleanupStaleGatewayPoolNodes(context.Background(), pool, nil, time.Now()); err != nil {
		t.Fatalf("cleanupStaleGatewayPoolNodes() error = %v", err)
	}

	if _, err := client.Resource(gatewayNodeGVR).Get(context.Background(), "stale-delete", metav1.GetOptions{}); err == nil {
		t.Fatalf("expected stale-delete gateway pool node to be removed")
	}

	if _, err := client.Resource(gatewayNodeGVR).Get(context.Background(), "stale-preserve", metav1.GetOptions{}); err != nil {
		t.Fatalf("expected stale-preserve gateway pool node to be retained, got %v", err)
	}
}

func TestCleanupStaleGatewayPoolNodesPreservesPreferredPoolHandoff(t *testing.T) {
	now := time.Date(2026, time.August, 22, 0, 0, 0, 0, time.UTC)
	externalPool := &unboundednetv1alpha1.GatewayPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a"},
		Spec: unboundednetv1alpha1.GatewayPoolSpec{
			Type:         gatewayPoolTypeExternal,
			NodeSelector: map[string]string{"role": "gw"},
		},
	}
	internalPool := &unboundednetv1alpha1.GatewayPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-b"},
		Spec: unboundednetv1alpha1.GatewayPoolSpec{
			Type:         gatewayPoolTypeInternal,
			NodeSelector: map[string]string{"role": "gw"},
		},
	}
	node := makeGatewayNode("node-a", map[string]string{"role": "gw"}, false, "pub")
	node.Annotations[unboundednetv1alpha1.NodeDiscoveredPublicIPAnnotation] = "203.0.113.10"
	node.Annotations[unboundednetv1alpha1.NodeDiscoveredPublicIPExpiresAtAnnotation] = now.Format(time.RFC3339)

	existing := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "net.unbounded-cloud.io/v1alpha1",
		"kind":       "GatewayPoolNode",
		"metadata":   map[string]interface{}{"name": node.Name},
		"spec":       map[string]interface{}{"gatewayPool": externalPool.Name},
	}}
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		gatewayNodeGVR: "GatewayPoolNodeList",
	}, existing)

	nodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := nodeIndexer.Add(node); err != nil {
		t.Fatalf("add node: %v", err)
	}

	gatewayNodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if err := gatewayNodeIndexer.Add(existing); err != nil {
		t.Fatalf("add GatewayPoolNode: %v", err)
	}

	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]())
	defer queue.ShutDown()

	gc := &GatewayPoolController{
		dynamicClient:       client,
		nodeLister:          corev1listers.NewNodeLister(nodeIndexer),
		poolsCache:          []unboundednetv1alpha1.GatewayPool{*externalPool, *internalPool},
		gatewayNodeInformer: &fakeInformer{store: gatewayNodeIndexer},
		workqueue:           queue,
	}
	if preferred, ok := gc.preferredGatewayPoolForNode(node, now); !ok || preferred != internalPool.Name {
		t.Fatalf("preferred pool at expiry = %q, ok = %v", preferred, ok)
	}

	if err := gc.cleanupStaleGatewayPoolNodes(t.Context(), externalPool, nil, now); err != nil {
		t.Fatalf("clean up old pool: %v", err)
	}

	if _, err := client.Resource(gatewayNodeGVR).Get(t.Context(), node.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("GatewayPoolNode was deleted during handoff: %v", err)
	}

	if got := queue.Len(); got != 1 {
		t.Fatalf("queued pools = %d, want 1", got)
	}

	queuedPool, shutdown := queue.Get()
	if shutdown {
		t.Fatal("workqueue shut down before preferred pool was retrieved")
	}

	queue.Done(queuedPool)

	if queuedPool != internalPool.Name {
		t.Fatalf("queued pool = %q, want %q", queuedPool, internalPool.Name)
	}

	if err := gc.ensureGatewayPoolNode(t.Context(), internalPool, node, ""); err != nil {
		t.Fatalf("update GatewayPoolNode for fallback pool: %v", err)
	}

	updated, err := client.Resource(gatewayNodeGVR).Get(t.Context(), node.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get updated GatewayPoolNode: %v", err)
	}

	if got, _, _ := unstructured.NestedString(updated.Object, "spec", "gatewayPool"); got != internalPool.Name {
		t.Fatalf("spec.gatewayPool = %q, want %q", got, internalPool.Name)
	}
}
