// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package status

import (
	"time"

	"github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

// OverviewFromStatus projects a legacy detailed publication into observed facts.
// Node summary collection should compute these facts without collecting details.
func OverviewFromStatus(full *v1alpha1.NodeStatusResponse, now time.Time) v1alpha1.NodeStatusOverview {
	overview := v1alpha1.NodeStatusOverview{
		Timestamp: full.Timestamp, NodeInfo: full.NodeInfo, HealthCheck: full.HealthCheck,
		NodeErrors: full.NodeErrors, FetchError: full.FetchError,
		LastPushTime: full.LastPushTime, StatusSource: full.StatusSource, NodePodInfo: full.NodePodInfo,
		PeerCount: len(full.Peers), RouteCount: len(full.RoutingTable.Routes),
	}
	for i := range full.Peers {
		if PeerHealthyForOverview(&full.Peers[i], now) {
			overview.HealthyPeers++
		}
	}

	for _, route := range full.RoutingTable.Routes {
		if RouteMismatchForOverview(route) {
			overview.RouteMismatch = true
			break
		}
	}

	return overview
}

// OverviewMetadata preserves lightweight fields for controller enrichment.
// It contains no details and must not be returned as a diagnostic snapshot.
func OverviewMetadata(overview v1alpha1.NodeStatusOverview) v1alpha1.NodeStatusResponse {
	return v1alpha1.NodeStatusResponse{
		Timestamp: overview.Timestamp, NodeInfo: overview.NodeInfo, HealthCheck: overview.HealthCheck,
		NodeErrors: overview.NodeErrors, FetchError: overview.FetchError,
		LastPushTime: overview.LastPushTime, StatusSource: overview.StatusSource, NodePodInfo: overview.NodePodInfo,
	}
}

// PeerHealthyForOverview preserves the dashboard's probe/handshake fallback.
func PeerHealthyForOverview(peer *v1alpha1.PeerStatus, now time.Time) bool {
	if peer.HealthCheck != nil && peer.HealthCheck.Enabled {
		return peer.HealthCheck.Status == "up" || peer.HealthCheck.Status == "Up"
	}

	return !peer.Tunnel.LastHandshake.IsZero() && now.Sub(peer.Tunnel.LastHandshake) < 3*time.Minute
}

// RouteMismatchForOverview compares observed and expected next-hop presence.
func RouteMismatchForOverview(route v1alpha1.RouteEntry) bool {
	for _, hop := range route.NextHops {
		expected := hop.Expected != nil && *hop.Expected

		present := hop.Present != nil && *hop.Present
		if expected != present {
			return true
		}
	}

	return false
}
