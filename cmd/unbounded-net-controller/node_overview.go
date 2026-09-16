// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"time"

	statuspkg "github.com/Azure/unbounded/internal/net/status"
	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

// fetchNodeOverview never falls back to the legacy detailed endpoint. Explicit
// observed fields are required so a full-shaped response cannot look healthy
// merely because it omitted summary counts.
func fetchNodeOverview(ctx context.Context, nodeName, nodeIP string, port int) (*statusv1alpha1.NodeStatusOverview, error) {
	var response struct {
		statusv1alpha1.NodeStatusOverview
		PeerCount     *int  `json:"peerCount"`
		HealthyPeers  *int  `json:"healthyPeers"`
		RouteCount    *int  `json:"routeCount"`
		RouteMismatch *bool `json:"routeMismatch"`
	}
	if err := fetchNodeJSON(ctx, nodeIP, port, "/status/summary", &response); err != nil {
		return nil, err
	}

	if response.NodeInfo.Name != nodeName || response.PeerCount == nil ||
		response.HealthyPeers == nil || response.RouteCount == nil || response.RouteMismatch == nil {
		return nil, fmt.Errorf("node %q returned missing or mismatched summary facts", nodeName)
	}

	overview := response.NodeStatusOverview
	overview.PeerCount = *response.PeerCount
	overview.HealthyPeers = *response.HealthyPeers
	overview.RouteCount = *response.RouteCount

	overview.RouteMismatch = *response.RouteMismatch
	if err := validateOverviewCounts(overview); err != nil {
		return nil, fmt.Errorf("node %q returned invalid summary counts: %w", nodeName, err)
	}

	return &overview, nil
}

func validateOverviewCounts(overview statusv1alpha1.NodeStatusOverview) error {
	if overview.PeerCount < 0 || overview.HealthyPeers < 0 ||
		overview.HealthyPeers > overview.PeerCount || overview.RouteCount < 0 ||
		overview.RouteMismatchCount < 0 || overview.UnhealthyPeerLinks < 0 ||
		(overview.RouteMismatchCount > 0 && !overview.RouteMismatch) {
		return fmt.Errorf("summary contains invalid observed counts")
	}

	return nil
}

// StoreOverview replaces routine wire state without retaining diagnostic arrays.
func (c *NodeStatusCache) StoreOverview(nodeName string, overview statusv1alpha1.NodeStatusOverview, source string) (uint64, error) {
	if nodeName == "" || (overview.NodeInfo.Name != "" && overview.NodeInfo.Name != nodeName) {
		return 0, fmt.Errorf("summary identity does not match node %q", nodeName)
	}

	if err := validateOverviewCounts(overview); err != nil {
		return 0, err
	}

	overview.NodeInfo.Name = nodeName

	if source == "" {
		source = "push"
	}

	overview.StatusSource = source
	metadata := statuspkg.OverviewMetadata(overview)

	c.mu.Lock()

	revision := uint64(1)
	if previous := c.entries[nodeName]; previous != nil {
		revision = previous.Revision + 1
	}

	c.entries[nodeName] = &CachedNodeStatus{
		Status: &metadata, Overview: &overview, Source: source,
		Revision: revision, ReceivedAt: time.Now(),
	}
	fn := c.onOverviewChange
	c.mu.Unlock()

	if fn != nil {
		fn(nodeName, overview)
	}

	return revision, nil
}

// SetOnOverviewChange registers the summary-only cache mutation callback.
func (c *NodeStatusCache) SetOnOverviewChange(fn func(string, statusv1alpha1.NodeStatusOverview)) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.onOverviewChange = fn
}
