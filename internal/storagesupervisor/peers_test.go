// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package storagesupervisor

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const testRingLabel = "unbounded-cloud.io/storage-ring"

// node builds a Node with the given ring label value (empty == no label) and
// InternalIP (empty == no address).
func node(name, ring, ip string) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}

	if ring != "" {
		n.Labels = map[string]string{testRingLabel: ring}
	}

	if ip != "" {
		n.Status.Addresses = []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: ip},
		}
	}

	return n
}

func TestNodeIDStableAndNonZero(t *testing.T) {
	// Deterministic across calls and distinct for distinct names.
	assert.Equal(t, nodeID("node-a"), nodeID("node-a"))
	assert.NotEqual(t, nodeID("node-a"), nodeID("node-b"))
	// Never zero (zero is the daemon's peerless sentinel).
	assert.NotZero(t, nodeID(""))
	assert.NotZero(t, nodeID("node-a"))
}

func TestParseFabricPort(t *testing.T) {
	tests := []struct {
		name     string
		addr     string
		wantPort int
		wantOK   bool
	}{
		{name: "wildcard with port", addr: "0.0.0.0:9000", wantPort: 9000, wantOK: true},
		{name: "ip with port", addr: "10.0.0.1:7000", wantPort: 7000, wantOK: true},
		{name: "ephemeral port", addr: "0.0.0.0:0", wantPort: 0, wantOK: true},
		{name: "empty", addr: "", wantPort: 0, wantOK: false},
		{name: "no port", addr: "0.0.0.0", wantPort: 0, wantOK: false},
		{name: "non-numeric port", addr: "0.0.0.0:abc", wantPort: 0, wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			port, ok := parseFabricPort(tt.addr)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantPort, port)
		})
	}
}

func TestComputeRingMembership(t *testing.T) {
	// self is in ring "red" with two other "red" members; "blue" and unlabeled
	// nodes are excluded.
	nodes := []*corev1.Node{
		node("self", "red", "10.0.0.1"),
		node("peer-a", "red", "10.0.0.2"),
		node("peer-b", "red", "10.0.0.3"),
		node("other", "blue", "10.0.0.4"),
		node("loner", "", "10.0.0.5"),
	}

	ring := computeRing(nodes, "self", testRingLabel, 9000)

	require.True(t, ring.active)
	assert.Equal(t, nodeID("self"), ring.localNodeID)
	assert.Equal(t, "10.0.0.1:9000", ring.selfListenAddr)

	require.Len(t, ring.peers, 2)
	// Peers are sorted by id; verify both red peers are present with their
	// hashed ids and InternalIP:port sockets, and self is excluded.
	got := map[uint64]string{}
	for _, p := range ring.peers {
		got[p.GetId()] = p.GetTcp().GetAddr()
	}

	assert.Equal(t, "10.0.0.2:9000", got[nodeID("peer-a")])
	assert.Equal(t, "10.0.0.3:9000", got[nodeID("peer-b")])
	assert.NotContains(t, got, ring.localNodeID)
}

func TestComputeRingPeersSortedByID(t *testing.T) {
	nodes := []*corev1.Node{
		node("self", "red", "10.0.0.1"),
		node("zeta", "red", "10.0.0.2"),
		node("alpha", "red", "10.0.0.3"),
	}

	ring := computeRing(nodes, "self", testRingLabel, 9000)

	require.Len(t, ring.peers, 2)
	assert.Less(t, ring.peers[0].GetId(), ring.peers[1].GetId())
}

func TestComputeRingSelfNotLabelled(t *testing.T) {
	// self carries no ring label: inactive, per-node config left untouched.
	nodes := []*corev1.Node{
		node("self", "", "10.0.0.1"),
		node("peer-a", "red", "10.0.0.2"),
	}

	ring := computeRing(nodes, "self", testRingLabel, 9000)
	assert.False(t, ring.active)
	assert.Empty(t, ring.peers)
}

func TestComputeRingSelfMissing(t *testing.T) {
	// self not present among the watched nodes: inactive.
	nodes := []*corev1.Node{node("peer-a", "red", "10.0.0.2")}

	ring := computeRing(nodes, "self", testRingLabel, 9000)
	assert.False(t, ring.active)
}

func TestComputeRingSelfNoInternalIP(t *testing.T) {
	// self has no InternalIP: cannot form a routable fabric addr, inactive.
	nodes := []*corev1.Node{
		node("self", "red", ""),
		node("peer-a", "red", "10.0.0.2"),
	}

	ring := computeRing(nodes, "self", testRingLabel, 9000)
	assert.False(t, ring.active)
}

func TestComputeRingSkipsPeerWithoutInternalIP(t *testing.T) {
	// A peer with no InternalIP is skipped, not emitted with an empty socket.
	nodes := []*corev1.Node{
		node("self", "red", "10.0.0.1"),
		node("peer-a", "red", "10.0.0.2"),
		node("peer-no-ip", "red", ""),
	}

	ring := computeRing(nodes, "self", testRingLabel, 9000)

	require.True(t, ring.active)
	require.Len(t, ring.peers, 1)
	assert.Equal(t, nodeID("peer-a"), ring.peers[0].GetId())
}

func TestComputeRingSingleMember(t *testing.T) {
	// A lone ring member still activates so its fabric addr is made routable,
	// with an empty peer set.
	nodes := []*corev1.Node{node("self", "red", "10.0.0.1")}

	ring := computeRing(nodes, "self", testRingLabel, 9000)

	require.True(t, ring.active)
	assert.Equal(t, "10.0.0.1:9000", ring.selfListenAddr)
	assert.Empty(t, ring.peers)
}

func TestNewPeerWatcherDisabledWithoutNodeName(t *testing.T) {
	// No NodeName means peer discovery is disabled; the run loop falls back to
	// startup-only rendering.
	w, err := newPeerWatcher(Config{StorageRingLabel: testRingLabel}, fake.NewSimpleClientset())
	require.NoError(t, err)
	assert.Nil(t, w)
}

func TestPeerWatcherSnapshotInactiveWithoutPort(t *testing.T) {
	cs := fake.NewSimpleClientset(node("self", "red", "10.0.0.1"))

	w, err := newPeerWatcher(Config{
		NodeName:         "self",
		StorageRingLabel: testRingLabel,
	}, cs)
	require.NoError(t, err)
	require.NotNil(t, w)

	defer w.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, w.Start(ctx))

	// portOK=false -> ring inactive regardless of membership.
	assert.False(t, w.snapshot(0, false).ring.active)
}

func TestPeerWatcherSnapshotFromInformer(t *testing.T) {
	cs := fake.NewSimpleClientset(
		node("self", "red", "10.0.0.1"),
		node("peer-a", "red", "10.0.0.2"),
		node("other", "blue", "10.0.0.9"),
	)

	w, err := newPeerWatcher(Config{
		NodeName:         "self",
		StorageRingLabel: testRingLabel,
	}, cs)
	require.NoError(t, err)
	require.NotNil(t, w)

	defer w.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, w.Start(ctx))

	snapshot := w.snapshot(9000, true)
	ring := snapshot.ring

	require.True(t, ring.active)
	assert.Equal(t, "10.0.0.1:9000", ring.selfListenAddr)
	require.Len(t, ring.peers, 1)
	assert.Equal(t, nodeID("peer-a"), ring.peers[0].GetId())
	assert.Equal(t, "10.0.0.2:9000", ring.peers[0].GetTcp().GetAddr())
}

func TestPeerWatcherSnapshotIncludesBenchmarksWithoutTCPPort(t *testing.T) {
	source := node("self", "red", "10.0.0.1")
	source.Annotations = map[string]string{
		benchmarkScenarioAnnotation:   rdmaLoadgenScenario,
		benchmarkTargetNodeAnnotation: "peer-a",
		benchmarkRdmaAddrAnnotation:   "hex:0102",
	}

	target := node("peer-a", "red", "10.0.0.2")
	target.Annotations = map[string]string{benchmarkRdmaAddrAnnotation: "hex:0304"}

	cs := fake.NewSimpleClientset(source, target)

	w, err := newPeerWatcher(Config{
		NodeName:         "self",
		StorageRingLabel: testRingLabel,
	}, cs)
	require.NoError(t, err)
	require.NotNil(t, w)

	defer w.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, w.Start(ctx))

	snapshot := w.snapshot(0, false)

	assert.False(t, snapshot.ring.active)
	require.Len(t, snapshot.benchmarks.rdmaLoadgens, 1)
	assert.True(t, snapshot.benchmarks.rdmaLoadgens[0].runLoadgen)
	assert.Equal(t, nodeID("peer-a"), snapshot.benchmarks.rdmaLoadgens[0].peerNodeID)
}
