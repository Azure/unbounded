// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build integrationtest

package inttest

import (
	"context"
	"sync"

	"github.com/Azure/unbounded/internal/orca/cluster"
)

// StaticPeerSource implements cluster.PeerSource with a mutable peer
// list. Each replica in the harness owns its own StaticPeerSource so
// tests can mutate one replica's view of the cluster independently
// (used by TestPeerNotCoordinatorFallback to induce membership
// disagreement).
//
// The source knows its calling replica's identity (selfIP, selfPort)
// so it can stamp Peer.Self correctly even when multiple peers share
// an IP (the case in tests where every replica is on 127.0.0.1).
type StaticPeerSource struct {
	mu       sync.Mutex
	selfIP   string
	selfPort int
	peers    []cluster.Peer
}

// NewStaticPeerSource returns a peer source that stamps Self=true on
// any peer whose (IP, Port) matches the constructor arguments.
func NewStaticPeerSource(selfIP string, selfPort int, peers []cluster.Peer) *StaticPeerSource {
	s := &StaticPeerSource{
		selfIP:   selfIP,
		selfPort: selfPort,
	}
	s.SetPeers(peers)

	return s
}

// SetPeers replaces the current peer list. Each peer's Self bit is
// recomputed against the source's stored (selfIP, selfPort).
func (s *StaticPeerSource) SetPeers(peers []cluster.Peer) {
	out := make([]cluster.Peer, len(peers))
	for i, p := range peers {
		p.Self = p.IP == s.selfIP && p.Port == s.selfPort
		out[i] = p
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.peers = out
}

// Peers satisfies cluster.PeerSource.
func (s *StaticPeerSource) Peers(_ context.Context) ([]cluster.Peer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]cluster.Peer, len(s.peers))
	copy(out, s.peers)

	return out, nil
}
