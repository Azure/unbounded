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

func nodeWithAnnotations(name, ring, ip string, annotations map[string]string) *corev1.Node {
	n := node(name, ring, ip)
	n.Annotations = annotations

	return n
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

	ring := computeRing(nodes, "self", testRingLabel, 9101)

	require.True(t, ring.active)
	assert.Equal(t, "self", ring.selfName)

	require.Len(t, ring.peers, 3)
	// Peers are sorted by name; verify all red peers are present with their
	// InternalIP:port sockets, including self.
	got := map[string]string{}
	for _, p := range ring.peers {
		got[p.GetName()] = p.GetDiscoveryAddr()
	}

	assert.Equal(t, "10.0.0.1:9101", got["self"])
	assert.Equal(t, "10.0.0.2:9101", got["peer-a"])
	assert.Equal(t, "10.0.0.3:9101", got["peer-b"])
}

func TestComputeRingPeersSortedByName(t *testing.T) {
	nodes := []*corev1.Node{
		node("self", "red", "10.0.0.1"),
		node("zeta", "red", "10.0.0.2"),
		node("alpha", "red", "10.0.0.3"),
	}

	ring := computeRing(nodes, "self", testRingLabel, 9000)

	require.Len(t, ring.peers, 3)
	assert.Equal(t, "alpha", ring.peers[0].GetName())
	assert.Equal(t, "self", ring.peers[1].GetName())
	assert.Equal(t, "zeta", ring.peers[2].GetName())
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
	// self has no InternalIP: cannot form a routable discovery endpoint, inactive.
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
	require.Len(t, ring.peers, 2)
	assert.Equal(t, "peer-a", ring.peers[0].GetName())
	assert.Equal(t, "self", ring.peers[1].GetName())
}

func TestComputeRingSingleMember(t *testing.T) {
	// A lone ring member still activates with self in the peer roster.
	nodes := []*corev1.Node{node("self", "red", "10.0.0.1")}

	ring := computeRing(nodes, "self", testRingLabel, 9000)

	require.True(t, ring.active)
	require.Len(t, ring.peers, 1)
	assert.Equal(t, "self", ring.peers[0].GetName())
	assert.Equal(t, "10.0.0.1:9000", ring.peers[0].GetDiscoveryAddr())
}

func TestComputeRDMARingMembership(t *testing.T) {
	nodes := []*corev1.Node{
		node("self", "red", "10.0.0.1"),
		node("peer-a", "red", "10.0.0.2"),
		node("peer-b", "red", "fd00::3"),
		node("no-ip", "red", ""),
		node("other", "blue", "10.0.0.6"),
	}

	ring := computeRing(nodes, "self", testRingLabel, 9101)

	require.True(t, ring.active)
	assert.Equal(t, "self", ring.selfName)
	require.Len(t, ring.peers, 3)

	got := map[string]string{}
	for _, p := range ring.peers {
		got[p.GetName()] = p.GetDiscoveryAddr()
	}

	assert.Equal(t, "10.0.0.1:9101", got["self"])
	assert.Equal(t, "10.0.0.2:9101", got["peer-a"])
	assert.Equal(t, "[fd00::3]:9101", got["peer-b"])
}

func TestComputeRDMARingInactiveWithoutSelfInternalIP(t *testing.T) {
	nodes := []*corev1.Node{
		node("self", "red", ""),
		node("peer-a", "red", "10.0.0.2"),
	}

	ring := computeRing(nodes, "self", testRingLabel, 9101)

	assert.False(t, ring.active)
}

func TestComputeRDMARingSingleMember(t *testing.T) {
	nodes := []*corev1.Node{node("self", "red", "10.0.0.1")}

	ring := computeRing(nodes, "self", testRingLabel, 9101)

	require.True(t, ring.active)
	assert.Equal(t, "self", ring.selfName)
	require.Len(t, ring.peers, 1)
	assert.Equal(t, "self", ring.peers[0].GetName())
	assert.Equal(t, "10.0.0.1:9101", ring.peers[0].GetDiscoveryAddr())
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

	// portOK=false -> inactive regardless of membership.
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

	state := w.snapshot(9101, true)
	ring := state.ring

	require.True(t, ring.active)
	require.Len(t, ring.peers, 2)
	assert.Equal(t, "peer-a", ring.peers[0].GetName())
	assert.Equal(t, "10.0.0.2:9101", ring.peers[0].GetDiscoveryAddr())
	assert.Equal(t, "self", ring.peers[1].GetName())
	assert.Equal(t, "10.0.0.1:9101", ring.peers[1].GetDiscoveryAddr())
}

func TestPeerWatcherSnapshotRdmaFromInformer(t *testing.T) {
	cs := fake.NewSimpleClientset(
		node("self", "red", "10.0.0.1"),
		node("peer-a", "red", "10.0.0.2"),
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

	state := w.snapshot(9101, true)
	require.True(t, state.ring.active)
	require.Len(t, state.ring.peers, 2)
	assert.Equal(t, "peer-a", state.ring.peers[0].GetName())
	assert.Equal(t, "10.0.0.2:9101", state.ring.peers[0].GetDiscoveryAddr())
	assert.Equal(t, "self", state.ring.peers[1].GetName())
	assert.Equal(t, "10.0.0.1:9101", state.ring.peers[1].GetDiscoveryAddr())
}

func TestPeerWatcherSnapshotIncludesSelfAnnotations(t *testing.T) {
	cs := fake.NewSimpleClientset(
		nodeWithAnnotations("self", "", "10.0.0.1", map[string]string{
			allocatedDisksAnnotation:  "/dev/nvme1n1",
			storageFileSizeAnnotation: "4294967296",
		}),
		nodeWithAnnotations("peer-a", "red", "10.0.0.2", map[string]string{
			allocatedDisksAnnotation: "/dev/nvme2n1",
		}),
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

	state := w.snapshot(0, false)

	assert.False(t, state.ring.active)
	assert.Equal(t, "/dev/nvme1n1", state.annotations[allocatedDisksAnnotation])
	assert.Equal(t, "4294967296", state.annotations[storageFileSizeAnnotation])
}
