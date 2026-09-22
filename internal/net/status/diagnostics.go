// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package status

import (
	"fmt"
	"strings"
	"time"

	"github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

// OverviewDiagnosticMessages formats scalar diagnostics without altering node
// errors or CNI status. providerID must be the controller-enriched node value.
func OverviewDiagnosticMessages(overview v1alpha1.NodeStatusOverview, providerID string) []string {
	var messages []string
	if overview.RouteMismatchCount > 0 {
		messages = append(messages, fmt.Sprintf("%d route next-hop mismatches (expected vs present)", overview.RouteMismatchCount))
	}

	if overview.UnhealthyPeerLinks > 0 {
		messages = append(messages, fmt.Sprintf("%d peer links are unhealthy", overview.UnhealthyPeerLinks))
	}

	if overview.UsesIPIP && strings.HasPrefix(providerID, "azure://") {
		messages = append(messages, "IPIP tunnel protocol is not supported on Azure (IP protocol 4 is blocked by the platform)")
	}

	return messages
}

// PeerLinkHealthyForDiagnostics preserves the problem list's normalized probe
// rules. Unlike PeerHealthyForOverview, a nonempty status enables probe checks
// even when Enabled is false. Each tunnel link is counted, not each node name.
func PeerLinkHealthyForDiagnostics(peer *v1alpha1.PeerStatus, now time.Time) bool {
	checkStatus := ""
	if peer.HealthCheck != nil {
		checkStatus = strings.ToLower(strings.TrimSpace(peer.HealthCheck.Status))
	}

	if (peer.HealthCheck != nil && peer.HealthCheck.Enabled) || checkStatus != "" {
		return checkStatus == "up"
	}

	lastHandshake := peer.Tunnel.LastHandshake

	return !lastHandshake.IsZero() && now.Sub(lastHandshake) < 3*time.Minute
}

// UnhealthyPeerLinkCount counts unhealthy links without deduplicating peer names.
func UnhealthyPeerLinkCount(peers []v1alpha1.PeerStatus, now time.Time) int {
	count := 0

	for i := range peers {
		if !PeerLinkHealthyForDiagnostics(&peers[i], now) {
			count++
		}
	}

	return count
}

// RouteMismatchCount counts every annotated expected/present next-hop mismatch.
func RouteMismatchCount(routes []v1alpha1.RouteEntry) int {
	count := 0

	for _, route := range routes {
		for _, hop := range route.NextHops {
			expected := hop.Expected != nil && *hop.Expected

			present := hop.Present != nil && *hop.Present
			if expected != present {
				count++
			}
		}
	}

	return count
}
