// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"time"

	statuspkg "github.com/Azure/unbounded/internal/net/status"
	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

// StoreOverview replaces routine wire state without retaining diagnostic arrays.
func (c *NodeStatusCache) StoreOverview(nodeName string, overview statusv1alpha1.NodeStatusOverview, source string) (uint64, error) {
	if nodeName == "" || (overview.NodeInfo.Name != "" && overview.NodeInfo.Name != nodeName) {
		return 0, fmt.Errorf("summary identity does not match node %q", nodeName)
	}

	if overview.PeerCount < 0 || overview.HealthyPeers < 0 ||
		overview.HealthyPeers > overview.PeerCount || overview.RouteCount < 0 ||
		overview.RouteMismatchCount < 0 || overview.UnhealthyPeerLinks < 0 ||
		(overview.RouteMismatchCount > 0 && !overview.RouteMismatch) {
		return 0, fmt.Errorf("summary contains invalid observed counts")
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
