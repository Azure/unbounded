// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"time"

	statuspkg "github.com/Azure/unbounded/internal/net/status"
)

// BindDetails switches routine storage to overview-only for this process.
// Existing full entries lose their heavy ownership; subsequent legacy deltas
// require an unexpired TTL base. A closed manager stays bound and fails closed.
func (c *NodeStatusCache) BindDetails(manager *nodeDetailRequests) {
	if manager == nil {
		panic("node status requires a non-nil detail manager")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.details = manager
	c.legacyObserver = nil

	for name, previous := range c.entries {
		entry := *previous
		if entry.Overview == nil {
			overview := statuspkg.OverviewFromStatus(entry.Status, time.Now())
			entry.Overview = &overview
		}

		metadata := statuspkg.OverviewMetadata(*entry.Overview)
		entry.Status = &metadata
		entry.peerIdentity = nil
		c.entries[name] = &entry
	}
}

func (c *NodeStatusCache) legacyEntryLocked(nodeName string, status *NodeStatusResponse, revision uint64, identity *peerIdentityDigest, source string, base *NodeStatusResponse) (*CachedNodeStatus, error) {
	entry := &CachedNodeStatus{
		Status: status, Revision: revision, Source: source,
		ReceivedAt: time.Now(), peerIdentity: identity, legacy: true,
	}
	if c.details == nil {
		return entry, nil
	}

	if err := c.details.ObserveLegacy(nodeName, status, revision, identity, base); err != nil {
		return nil, err
	}

	overview := statuspkg.OverviewFromStatus(status, entry.ReceivedAt)
	overview.StatusSource = source
	metadata := statuspkg.OverviewMetadata(overview)
	entry.Status = &metadata
	entry.Overview = &overview
	entry.peerIdentity = nil

	return entry, nil
}
