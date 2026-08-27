// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package discovery

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"

	"github.com/Azure/unbounded/internal/gantry/address"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

func TestDigestToCID_Deterministic(t *testing.T) {
	d := digest.MustParse("sha256:" + zeros(64))

	c1, err := DigestToCID(d)
	if err != nil {
		t.Fatalf("DigestToCID: %v", err)
	}

	c2, err := DigestToCID(d)
	if err != nil {
		t.Fatalf("DigestToCID (2nd): %v", err)
	}

	if !c1.Equals(c2) {
		t.Errorf("CIDs differ across calls: %s vs %s", c1, c2)
	}

	if c1.Version() != 1 {
		t.Errorf("CID version = %d, want 1", c1.Version())
	}
	// Two distinct digests must produce two distinct CIDs.
	d2 := digest.MustParse("sha256:" + ones(64))

	c3, err := DigestToCID(d2)
	if err != nil {
		t.Fatalf("DigestToCID (d2): %v", err)
	}

	if c1.Equals(c3) {
		t.Error("CIDs equal across different digests")
	}
}

func TestMergePeerAddrInfoCombinesAddressesByPeer(t *testing.T) {
	peerID := peer.ID("peer-a")
	tcp := multiaddr.StringCast("/ip4/10.0.0.1/tcp/4001")
	quic := multiaddr.StringCast("/ip4/10.0.0.1/udp/4001/quic-v1")
	pool := []peer.AddrInfo{}
	positions := map[peer.ID]int{}

	mergePeerAddrInfo(&pool, positions, peer.AddrInfo{ID: peerID, Addrs: []multiaddr.Multiaddr{tcp}})
	mergePeerAddrInfo(&pool, positions, peer.AddrInfo{ID: peerID, Addrs: []multiaddr.Multiaddr{tcp, quic}})

	if len(pool) != 1 {
		t.Fatalf("peer count = %d, want 1", len(pool))
	}

	if len(pool[0].Addrs) != 2 {
		t.Fatalf("address count = %d, want 2", len(pool[0].Addrs))
	}
}

func TestHostBringUpEphemeral(t *testing.T) {
	// Smoke test: New with an ephemeral identity returns a usable host;
	// Provide on a fresh DHT errors because there are no peers yet, but
	// the host itself must boot cleanly and Close cleanly.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	h, err := New(ctx, Options{
		IdentityPath:   "",
		ListenAddrs:    []string{"/ip4/127.0.0.1/tcp/0"},
		BootstrapPeers: nil,
		ProtocolPrefix: "/gantry",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(func() { _ = h.Close() }) //nolint:errcheck // best-effort close

	if h.PeerID() == "" {
		t.Error("PeerID empty")
	}

	if len(h.Addrs()) == 0 {
		t.Error("no listen addrs")
	}

	if got, want := h.Health(), 1.0; got != want {
		t.Errorf("Health() = %v, want %v (no monitor wired in test mode)", got, want)
	}
}

func TestHostPersistsIdentity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "libp2p.key")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	first, err := New(ctx, Options{
		IdentityPath:   path,
		ListenAddrs:    []string{"/ip4/127.0.0.1/tcp/0"},
		ProtocolPrefix: "/gantry-test",
	})
	if err != nil {
		t.Fatalf("first New: %v", err)
	}

	id1 := first.PeerID()
	_ = first.Close() //nolint:errcheck // best-effort close

	second, err := New(ctx, Options{
		IdentityPath:   path,
		ListenAddrs:    []string{"/ip4/127.0.0.1/tcp/0"},
		ProtocolPrefix: "/gantry-test",
	})
	if err != nil {
		t.Fatalf("second New: %v", err)
	}

	defer func() { _ = second.Close() }() //nolint:errcheck // best-effort close

	if second.PeerID() != id1 {
		t.Errorf("PeerID changed across restarts: %s vs %s", id1, second.PeerID())
	}
}

func TestClosestPeersIncludesSelfOnEmptyDHT(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	host, err := New(ctx, Options{
		ListenAddrs:    []string{"/ip4/0.0.0.0/tcp/0"},
		ProtocolPrefix: "/gantry-closest-test",
		AddrsFactory:   address.Factory("10.42.0.7"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(func() { _ = host.Close() })

	nodes, _ := host.ClosestPeers(ctx, digest.MustParse("sha256:"+zeros(64)), 3)
	if len(nodes) != 1 || nodes[0].ID != ifaces.NodeID(host.PeerID().String()) {
		t.Fatalf("ClosestPeers = %+v, want self", nodes)
	}
}

func TestConnectPeersDetailedCountsUniquePeerAttempts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	server, err := New(ctx, Options{
		ListenAddrs:    []string{"/ip4/127.0.0.1/tcp/0"},
		ProtocolPrefix: "/gantry-dial-result-test",
	})
	if err != nil {
		t.Fatalf("server New: %v", err)
	}

	t.Cleanup(func() { _ = server.Close() })

	client, err := New(ctx, Options{
		ListenAddrs:    []string{"/ip4/127.0.0.1/tcp/0"},
		ProtocolPrefix: "/gantry-dial-result-test",
	})
	if err != nil {
		t.Fatalf("client New: %v", err)
	}

	t.Cleanup(func() { _ = client.Close() })

	info := peer.AddrInfo{ID: server.PeerID(), Addrs: server.Addrs()}

	full, err := peer.AddrInfoToP2pAddrs(&info)
	if err != nil {
		t.Fatalf("AddrInfoToP2pAddrs: %v", err)
	}

	if len(full) == 0 {
		t.Fatal("server has no dialable address")
	}

	result := client.ConnectPeersDetailed(ctx, []string{full[0].String(), full[0].String()})
	if result.Attempted != 1 || result.Connected != 1 {
		t.Fatalf("ConnectPeersDetailed = %+v, want one unique attempted and connected peer", result)
	}
}

func TestTransferAddrWithPortSkipsLoopback(t *testing.T) {
	tests := []struct {
		name  string
		addrs []string
		want  string
	}{
		{
			name:  "IPv4 pod address after loopback",
			addrs: []string{"/ip4/127.0.0.1/tcp/4001", "/ip4/10.245.17.155/tcp/4001"},
			want:  "10.245.17.155:5001",
		},
		{
			name:  "IPv6 pod address after loopback",
			addrs: []string{"/ip6/::1/tcp/4001", "/ip6/fd00::42/tcp/4001"},
			want:  "[fd00::42]:5001",
		},
		{
			name:  "loopback only is unusable",
			addrs: []string{"/ip4/127.0.0.1/tcp/4001", "/ip6/::1/tcp/4001"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ai := peer.AddrInfo{Addrs: make([]multiaddr.Multiaddr, 0, len(tt.addrs))}
			for _, addr := range tt.addrs {
				ai.Addrs = append(ai.Addrs, multiaddr.StringCast(addr))
			}

			if got := transferAddrWithPort(ai, 5001); got != tt.want {
				t.Fatalf("transferAddrWithPort() = %q, want %q", got, tt.want)
			}
		})
	}
}

func zeros(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = '0'
	}

	return string(b)
}

func ones(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = '1'
	}

	return string(b)
}
