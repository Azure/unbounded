// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package status

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"maps"
	"slices"

	statusproto "github.com/Azure/unbounded/internal/net/status/proto"
	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

// PeerIdentityDigest guards snapshot indices against reordered or replaced peers.
// Length prefixes avoid ambiguous concatenations; duplicate identities are invalid.
func PeerIdentityDigest(peers []statusv1alpha1.PeerStatus) ([]byte, error) {
	digest := sha256.New()
	seen := make(map[[4]string]struct{}, len(peers))

	var size [8]byte

	for _, peer := range peers {
		identity := [4]string{peer.Name, peer.Tunnel.Protocol, peer.Tunnel.Interface, peer.Tunnel.PublicKey}
		if peer.Name == "" {
			return nil, fmt.Errorf("peer identity has no name")
		}

		if _, exists := seen[identity]; exists {
			return nil, fmt.Errorf("duplicate peer identity")
		}

		seen[identity] = struct{}{}
		for _, value := range identity {
			binary.BigEndian.PutUint64(size[:], uint64(len(value)))
			_, _ = digest.Write(size[:])
			_, _ = digest.Write([]byte(value))
		}
	}

	return digest.Sum(nil), nil
}

// PeerMetadataEqual excludes only measurements, not health state or topology.
func PeerMetadataEqual(a, b statusv1alpha1.PeerStatus) bool {
	ah, bh := a.HealthCheck, b.HealthCheck
	if a.Name != b.Name || a.PeerType != b.PeerType || a.SiteName != b.SiteName ||
		a.SkipPodCIDRRoutes != b.SkipPodCIDRRoutes ||
		!slices.Equal(a.PodCIDRGateways, b.PodCIDRGateways) ||
		!slices.Equal(a.RouteDestinations, b.RouteDestinations) ||
		!maps.Equal(a.RouteDistances, b.RouteDistances) ||
		a.Tunnel.Protocol != b.Tunnel.Protocol || a.Tunnel.Interface != b.Tunnel.Interface ||
		a.Tunnel.PublicKey != b.Tunnel.PublicKey || a.Tunnel.Endpoint != b.Tunnel.Endpoint ||
		!slices.Equal(a.Tunnel.AllowedIPs, b.Tunnel.AllowedIPs) {
		return false
	}

	if ah == nil || bh == nil {
		return ah == bh
	}

	return ah.Enabled == bh.Enabled && ah.Status == bh.Status
}

// PeerMeasurementsToProto uses packed scalar columns instead of nested peer DTOs.
func PeerMeasurementsToProto(peers []statusv1alpha1.PeerStatus) (*statusproto.PeerMeasurements, error) {
	digest, err := PeerIdentityDigest(peers)
	if err != nil {
		return nil, err
	}

	count := len(peers)

	pb := &statusproto.PeerMeasurements{
		PeerCount:           uint32(count),
		IdentityDigest:      digest,
		RxBytes:             make([]int64, count),
		TxBytes:             make([]int64, count),
		LastHandshakeUnixNs: make([]int64, count),
		Uptime:              make([]string, count),
		Rtt:                 make([]string, count),
	}
	for i, peer := range peers {
		pb.RxBytes[i] = peer.Tunnel.RxBytes

		pb.TxBytes[i] = peer.Tunnel.TxBytes
		if !peer.Tunnel.LastHandshake.IsZero() {
			pb.LastHandshakeUnixNs[i] = peer.Tunnel.LastHandshake.UnixNano()
		}

		if peer.HealthCheck != nil {
			pb.Uptime[i] = peer.HealthCheck.Uptime
			pb.Rtt[i] = peer.HealthCheck.RTT
		}
	}

	return pb, nil
}
