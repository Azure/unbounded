// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import "k8s.io/klog/v2"

// ObserveLegacyDetails bridges accepted legacy publications into the detail API
// while preserving existing full-cache and callback behavior during migration.
// It does not replay existing entries or refresh TTL on reads/source changes.
func (c *NodeStatusCache) ObserveLegacyDetails(manager *nodeDetailRequests) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.legacyObserver = manager
}

// The cache lock preserves publication order against concurrent full/delta
// updates. The observer resolves informer identity but performs no network I/O.
func (c *NodeStatusCache) observeLegacyLocked(nodeName string, entry *CachedNodeStatus) {
	if c.legacyObserver == nil {
		return
	}

	status := *entry.Status
	if status.NodeInfo.Name == "" {
		status.NodeInfo.Name = nodeName
	}

	if err := c.legacyObserver.ObserveLegacy(nodeName, &status, entry.Revision, entry.peerIdentity, nil); err != nil {
		klog.Errorf("Observing legacy node %q details failed: %v", nodeName, err)
	}
}
