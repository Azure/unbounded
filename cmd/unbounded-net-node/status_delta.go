// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"cmp"
	"reflect"
	"slices"
	"time"

	netstatus "github.com/Azure/unbounded/internal/net/status"
	statusproto "github.com/Azure/unbounded/internal/net/status/proto"
)

// sortStatusPeers is called only on freshly collected, exclusively owned peers.
// Gateway map iteration must not turn unchanged topology into peer replacements.
func sortStatusPeers(peers []WireGuardPeerStatus) {
	slices.SortFunc(peers, func(a, b WireGuardPeerStatus) int {
		return cmp.Or(
			cmp.Compare(a.Name, b.Name),
			cmp.Compare(a.Tunnel.Protocol, b.Tunnel.Protocol),
			cmp.Compare(a.Tunnel.Interface, b.Tunnel.Interface),
			cmp.Compare(a.Tunnel.PublicKey, b.Tunnel.PublicKey),
		)
	})
}

// typedStatusDelta avoids serializing both snapshots to JSON and decoding again.
// refresh emits a timestamp even when values are unchanged, preserving liveness.
func typedStatusDelta(prev, curr *NodeStatusResponse, compact, refresh bool) *statusproto.NodeStatusDelta {
	if prev == nil || curr == nil {
		return nil
	}

	pb := &statusproto.NodeStatusDelta{}

	add := func(field string) { pb.UpdatedFields = append(pb.UpdatedFields, field) }
	if refresh || !prev.Timestamp.Equal(curr.Timestamp) {
		add("timestamp")

		if !curr.Timestamp.IsZero() {
			pb.TimestampUnixNs = curr.Timestamp.UnixNano()
		}
	}

	if !reflect.DeepEqual(prev.NodeInfo, curr.NodeInfo) {
		add("nodeInfo")

		pb.NodeInfo = nodeInfoToProto(&curr.NodeInfo)
	}

	if !reflect.DeepEqual(prev.Peers, curr.Peers) {
		metadataEqual := compact && len(prev.Peers) == len(curr.Peers)
		if metadataEqual {
			for i := range curr.Peers {
				if !netstatus.PeerMetadataEqual(prev.Peers[i], curr.Peers[i]) {
					metadataEqual = false
					break
				}
			}
		}

		if metadataEqual {
			// Ambiguous identities must use the full replacement path.
			measurements, err := netstatus.PeerMeasurementsToProto(curr.Peers)
			if err == nil {
				pb.PeerMeasurements = measurements
			}
		}

		if pb.PeerMeasurements != nil {
			add("peerMeasurements")
		} else {
			add("peers")

			pb.Peers = peersToProto(curr.Peers)
		}
	}

	if !reflect.DeepEqual(prev.RoutingTable, curr.RoutingTable) {
		add("routingTable")

		pb.RoutingTable = routingTableToProto(&curr.RoutingTable)
	}

	if !reflect.DeepEqual(prev.HealthCheck, curr.HealthCheck) {
		add("healthCheck")

		pb.HealthCheck = healthCheckStatusToProto(curr.HealthCheck)
	}

	if !reflect.DeepEqual(prev.NodeErrors, curr.NodeErrors) {
		add("nodeErrors")

		pb.NodeErrors = nodeErrorsToProto(curr.NodeErrors)
	}

	if !reflect.DeepEqual(prev.BpfEntries, curr.BpfEntries) {
		add("bpfEntries")

		pb.BpfEntries = bpfEntriesToProto(curr.BpfEntries)
	}

	if prev.FetchError != curr.FetchError {
		add("fetchError")

		pb.FetchError = curr.FetchError
	}

	if !reflect.DeepEqual(prev.LastPushTime, curr.LastPushTime) {
		add("lastPushTime")

		if curr.LastPushTime != nil && !curr.LastPushTime.IsZero() {
			pb.LastPushTimeUnixNs = curr.LastPushTime.UnixNano()
		}
	}

	if prev.StatusSource != curr.StatusSource {
		add("statusSource")

		pb.StatusSource = curr.StatusSource
	}

	if !reflect.DeepEqual(prev.NodePodInfo, curr.NodePodInfo) {
		add("nodePodInfo")

		pb.NodePodInfo = nodePodInfoToProto(curr.NodePodInfo)
	}

	if len(pb.UpdatedFields) == 0 {
		return nil
	}

	return pb
}

// criticalStatus preserves the last published measurements while applying current
// metadata. Never modify shared snapshots or their health pointers.
func criticalStatus(prev, curr *NodeStatusResponse) *NodeStatusResponse {
	result := *curr
	result.Timestamp = prev.Timestamp

	if curr.HealthCheck != nil {
		health := *curr.HealthCheck

		health.CheckedAt = time.Time{}
		if prev.HealthCheck != nil {
			health.CheckedAt = prev.HealthCheck.CheckedAt
		}

		result.HealthCheck = &health
	}

	result.Peers = append([]WireGuardPeerStatus(nil), curr.Peers...)

	type identity struct{ name, protocol, iface, key string }

	key := func(peer WireGuardPeerStatus) identity {
		return identity{peer.Name, peer.Tunnel.Protocol, peer.Tunnel.Interface, peer.Tunnel.PublicKey}
	}

	previous := make(map[identity]WireGuardPeerStatus, len(prev.Peers))
	for _, peer := range prev.Peers {
		previous[key(peer)] = peer
	}

	for i := range result.Peers {
		peer := &result.Peers[i]

		old, ok := previous[key(*peer)]
		if !ok {
			continue
		}

		peer.Tunnel.RxBytes = old.Tunnel.RxBytes
		peer.Tunnel.TxBytes = old.Tunnel.TxBytes

		peer.Tunnel.LastHandshake = old.Tunnel.LastHandshake
		if peer.HealthCheck != nil {
			health := *peer.HealthCheck

			health.Uptime, health.RTT = "", ""
			if old.HealthCheck != nil {
				health.Uptime, health.RTT = old.HealthCheck.Uptime, old.HealthCheck.RTT
			}

			peer.HealthCheck = &health
		}
	}

	return &result
}
