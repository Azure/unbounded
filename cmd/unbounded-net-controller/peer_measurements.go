// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"time"

	netstatus "github.com/Azure/unbounded/internal/net/status"
	statusproto "github.com/Azure/unbounded/internal/net/status/proto"
	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

// peerIdentityDigest records successful name and uniqueness validation. It is
// immutable and bounded to one digest per cached peer topology.
type peerIdentityDigest [sha256.Size]byte

// hashPeerIdentities uses the same ordered, length-prefixed identity encoding as
// status.PeerIdentityDigest, without constructing its duplicate-detection map.
func hashPeerIdentities(peers []WireGuardPeerStatus) peerIdentityDigest {
	digest := sha256.New()

	var size [8]byte

	for _, peer := range peers {
		for _, value := range [4]string{peer.Name, peer.Tunnel.Protocol, peer.Tunnel.Interface, peer.Tunnel.PublicKey} {
			binary.BigEndian.PutUint64(size[:], uint64(len(value)))
			_, _ = digest.Write(size[:])
			_, _ = digest.Write([]byte(value))
		}
	}

	var result peerIdentityDigest

	digest.Sum(result[:0])

	return result
}

func validatePeerIdentity(peers []WireGuardPeerStatus, previous *peerIdentityDigest) (*peerIdentityDigest, error) {
	// StoreFull and Get share nested slices with callers. Re-hashing preserves
	// that ownership contract: even an out-of-band identity change must not use
	// a stale uniqueness result. A matching SHA-256 avoids rebuilding the map.
	if previous != nil && hashPeerIdentities(peers) == *previous {
		return previous, nil
	}

	digest, err := netstatus.PeerIdentityDigest(peers)
	if err != nil {
		return nil, err
	}

	result := peerIdentityDigest(digest)

	return &result, nil
}

// applyPeerMeasurements validates every column and identity before creating a
// replacement slice. Static maps/slices remain shared and immutable.
func applyPeerMeasurements(peers []WireGuardPeerStatus, pb *statusproto.PeerMeasurements) ([]WireGuardPeerStatus, error) {
	result, _, err := applyPeerMeasurementsWithIdentity(peers, pb, nil)

	return result, err
}

func applyPeerMeasurementsWithIdentity(peers []WireGuardPeerStatus, pb *statusproto.PeerMeasurements, previous *peerIdentityDigest) ([]WireGuardPeerStatus, *peerIdentityDigest, error) {
	count := len(peers)
	if uint64(pb.PeerCount) != uint64(count) ||
		len(pb.RxBytes) != count || len(pb.TxBytes) != count ||
		len(pb.LastHandshakeUnixNs) != count || len(pb.Uptime) != count || len(pb.Rtt) != count {
		return nil, nil, fmt.Errorf("peer measurement column length mismatch")
	}

	digest, err := validatePeerIdentity(peers, previous)
	if err != nil {
		return nil, nil, err
	}

	if !bytes.Equal(digest[:], pb.IdentityDigest) {
		return nil, nil, fmt.Errorf("peer measurement identity mismatch")
	}

	for i, peer := range peers {
		if peer.HealthCheck == nil && (pb.Uptime[i] != "" || pb.Rtt[i] != "") {
			return nil, nil, fmt.Errorf("peer measurements require existing health metadata")
		}
	}

	result := make([]WireGuardPeerStatus, count)
	copy(result, peers)

	health := make([]statusv1alpha1.HealthCheckPeerStatus, count)

	for i := range result {
		peer := &result[i]
		peer.Tunnel.RxBytes = pb.RxBytes[i]
		peer.Tunnel.TxBytes = pb.TxBytes[i]

		peer.Tunnel.LastHandshake = time.Time{}
		if pb.LastHandshakeUnixNs[i] != 0 {
			peer.Tunnel.LastHandshake = time.Unix(0, pb.LastHandshakeUnixNs[i])
		}

		if peer.HealthCheck != nil {
			health[i] = *peer.HealthCheck
			health[i].Uptime = pb.Uptime[i]
			health[i].RTT = pb.Rtt[i]
			peer.HealthCheck = &health[i]
		}
	}

	return result, digest, nil
}
