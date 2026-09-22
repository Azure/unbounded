// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"k8s.io/klog/v2"

	statuspkg "github.com/Azure/unbounded/internal/net/status"
	statusproto "github.com/Azure/unbounded/internal/net/status/proto"
	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

// CachedNodeStatus stores a node's pushed status with timestamp and revision.
type CachedNodeStatus struct {
	Status     *NodeStatusResponse
	ReceivedAt time.Time
	Source     string
	Revision   uint64
	Overview   *statusv1alpha1.NodeStatusOverview

	peerIdentity *peerIdentityDigest
	legacy       bool
}

// NodeStatusCache is a thread-safe cache of node status data pushed from node agents.
type NodeStatusCache struct {
	mu               sync.RWMutex
	entries          map[string]*CachedNodeStatus
	eventSeq         uint64
	onChange         func(nodeName string, status *NodeStatusResponse, eventSeq uint64)
	onOverviewChange func(nodeName string, overview statusv1alpha1.NodeStatusOverview, eventSeq uint64)
	legacyObserver   *nodeDetailRequests
	details          *nodeDetailRequests
}

// NewNodeStatusCache creates an empty NodeStatusCache.
func NewNodeStatusCache() *NodeStatusCache {
	return &NodeStatusCache{entries: make(map[string]*CachedNodeStatus)}
}

// Len returns the number of entries in the cache.
func (c *NodeStatusCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return len(c.entries)
}

// StoreFull stores a full node status payload and returns the new revision.
func (c *NodeStatusCache) StoreFull(nodeName string, status NodeStatusResponse, source string) uint64 {
	revision, err := c.StoreFullChecked(nodeName, status, source)
	if err != nil {
		klog.Errorf("Store node %q status failed: %v", nodeName, err)
	}

	return revision
}

// StoreFullChecked exposes identity/lifecycle failures to ingestion handlers.
func (c *NodeStatusCache) StoreFullChecked(nodeName string, status NodeStatusResponse, source string) (uint64, error) {
	if source == "" {
		source = "push"
	}

	if nodeName == "" || (status.NodeInfo.Name != "" && status.NodeInfo.Name != nodeName) {
		return 0, fmt.Errorf("legacy status identity does not match node %q", nodeName)
	}

	c.mu.Lock()

	if c.details != nil && status.NodeInfo.Name == "" {
		status.NodeInfo.Name = nodeName
	}

	prevRevision := uint64(0)
	if existing, ok := c.entries[nodeName]; ok {
		prevRevision = existing.Revision
	}

	revision := prevRevision + 1

	entry, err := c.legacyEntryLocked(nodeName, &status, revision, nil, source, nil)
	if err != nil {
		c.mu.Unlock()

		return 0, err
	}

	c.entries[nodeName] = entry
	c.observeLegacyLocked(nodeName, entry, 0)
	c.eventSeq++
	eventSeq := c.eventSeq
	fn := c.onChange
	overviewFn := c.onOverviewChange
	c.mu.Unlock()

	if entry.Overview != nil {
		if overviewFn != nil {
			overviewFn(nodeName, entry.overviewForNotification(), eventSeq)
		}
	} else if fn != nil {
		fn(nodeName, entry.Status, eventSeq)
	}

	return revision, nil
}

// parsedDelta holds pre-deserialized delta fields parsed outside the lock.
type parsedDelta struct {
	peerMeasurements *statusproto.PeerMeasurements
	parseError       error
	timestamp        *time.Time
	nodeInfo         *NodeInfo
	peers            []WireGuardPeerStatus
	routingTable     *RoutingTableInfo
	healthCheck      *HealthCheckStatus
	nodeErrors       []NodeError
	fetchError       *string
	lastPushTime     *time.Time
	statusSource     *string
	nodePodInfo      *NodePodInfo
	bpfEntries       []BpfEntry

	// nullFields tracks fields explicitly set to null for clearing.
	nullFields map[string]bool
}

// ApplyDelta merges a top-level delta payload into a cached node status.
// JSON deserialization happens outside the lock; under the lock only typed
// struct field assignments are performed.
func (c *NodeStatusCache) ApplyDelta(nodeName string, baseRevision uint64, delta map[string]json.RawMessage, source string) (uint64, bool, error) {
	if source == "" {
		source = "push"
	}

	// Phase 1: Deserialize delta fields OUTSIDE the lock.
	var pd parsedDelta

	pd.nullFields = make(map[string]bool)

	for key, raw := range delta {
		if string(raw) == "null" {
			pd.nullFields[key] = true
			continue
		}

		switch key {
		case "timestamp":
			var v time.Time
			if err := json.Unmarshal(raw, &v); err != nil {
				return 0, false, fmt.Errorf("decode delta field %q: %w", key, err)
			}

			pd.timestamp = &v
		case "nodeInfo":
			var v NodeInfo
			if err := json.Unmarshal(raw, &v); err != nil {
				return 0, false, fmt.Errorf("decode delta field %q: %w", key, err)
			}

			pd.nodeInfo = &v
		case "peers":
			if err := json.Unmarshal(raw, &pd.peers); err != nil {
				return 0, false, fmt.Errorf("decode delta field %q: %w", key, err)
			}
		case "routingTable":
			var v RoutingTableInfo
			if err := json.Unmarshal(raw, &v); err != nil {
				return 0, false, fmt.Errorf("decode delta field %q: %w", key, err)
			}

			pd.routingTable = &v
		case "healthCheck":
			var v HealthCheckStatus
			if err := json.Unmarshal(raw, &v); err != nil {
				return 0, false, fmt.Errorf("decode delta field %q: %w", key, err)
			}

			pd.healthCheck = &v
		case "nodeErrors":
			if err := json.Unmarshal(raw, &pd.nodeErrors); err != nil {
				return 0, false, fmt.Errorf("decode delta field %q: %w", key, err)
			}
		case "fetchError":
			var v string
			if err := json.Unmarshal(raw, &v); err != nil {
				return 0, false, fmt.Errorf("decode delta field %q: %w", key, err)
			}

			pd.fetchError = &v
		case "lastPushTime":
			var v time.Time
			if err := json.Unmarshal(raw, &v); err != nil {
				return 0, false, fmt.Errorf("decode delta field %q: %w", key, err)
			}

			pd.lastPushTime = &v
		case "statusSource":
			var v string
			if err := json.Unmarshal(raw, &v); err != nil {
				return 0, false, fmt.Errorf("decode delta field %q: %w", key, err)
			}

			pd.statusSource = &v
		case "nodePodInfo":
			var v NodePodInfo
			if err := json.Unmarshal(raw, &v); err != nil {
				return 0, false, fmt.Errorf("decode delta field %q: %w", key, err)
			}

			pd.nodePodInfo = &v
		case "bpfEntries":
			if err := json.Unmarshal(raw, &pd.bpfEntries); err != nil {
				return 0, false, fmt.Errorf("decode delta field %q: %w", key, err)
			}
		default:
			// Unknown fields are silently ignored to stay forward-compatible.
		}
	}

	// Phase 2: Apply pre-parsed values under the lock.
	return c.applyParsedDelta(nodeName, baseRevision, pd, source)
}

// ApplyParsedDelta applies a pre-parsed delta directly, bypassing JSON
// deserialization. This is the entry point for protobuf delta payloads.
func (c *NodeStatusCache) ApplyParsedDelta(nodeName string, baseRevision uint64, pd parsedDelta, source string) (uint64, bool, error) {
	if source == "" {
		source = "push"
	}

	rev, conflict, err := c.applyParsedDelta(nodeName, baseRevision, pd, source)
	if pd.peerMeasurements != nil || pd.parseError != nil {
		outcome := "applied"
		if err != nil {
			outcome = "error"
		} else if conflict {
			outcome = "resync"
		}

		peerMeasurementUpdatesTotal.WithLabelValues(outcome).Inc()
	}

	return rev, conflict, err
}

// applyParsedDelta merges pre-parsed delta fields into a cached node status
// under the lock. Both ApplyDelta (JSON) and ApplyParsedDelta (protobuf)
// converge here.
func (c *NodeStatusCache) applyParsedDelta(nodeName string, baseRevision uint64, pd parsedDelta, source string) (uint64, bool, error) {
	if pd.parseError != nil {
		return 0, false, pd.parseError
	}
	// Phase 1: Read entry under lock, copy it, release lock.
	c.mu.RLock()

	entry, ok := c.entries[nodeName]
	if !ok {
		c.mu.RUnlock()
		return 0, true, nil
	}

	if (entry.Overview != nil && !entry.legacy) || (pd.peerMeasurements != nil && baseRevision == 0) || (baseRevision != 0 && entry.Revision != baseRevision) {
		rev := entry.Revision

		c.mu.RUnlock()

		return rev, true, nil
	}
	// Snapshot values we need under lock
	prevStatus := entry.Status
	prevRevision := entry.Revision
	peerIdentity := entry.peerIdentity

	if c.details != nil {
		var exists bool

		prevStatus, peerIdentity, exists = c.details.LegacyBase(nodeName, prevRevision)
		if !exists {
			c.mu.RUnlock()

			return prevRevision, true, nil
		}
	}

	c.mu.RUnlock()

	// Phase 2: Copy and merge OUTSIDE the lock. This is the expensive
	// part (~1MB copy per node) and must not block other goroutines.
	merged := *prevStatus

	if pd.peerMeasurements != nil {
		if pd.peers != nil || pd.nullFields["peers"] {
			return prevRevision, false, fmt.Errorf("peer replacement conflicts with measurements")
		}

		peers, identity, err := applyPeerMeasurementsWithIdentity(prevStatus.Peers, pd.peerMeasurements, peerIdentity)
		if err != nil {
			return prevRevision, false, err
		}

		merged.Peers = peers
		peerIdentity = identity
	}

	if pd.timestamp != nil {
		merged.Timestamp = *pd.timestamp
	} else if pd.nullFields["timestamp"] {
		merged.Timestamp = time.Time{}
	}

	if pd.nodeInfo != nil {
		merged.NodeInfo = *pd.nodeInfo
	} else if pd.nullFields["nodeInfo"] {
		merged.NodeInfo = NodeInfo{}
	}

	if pd.peers != nil {
		merged.Peers = pd.peers
		peerIdentity = nil
	} else if pd.nullFields["peers"] {
		merged.Peers = nil
		peerIdentity = nil
	}

	if pd.routingTable != nil {
		merged.RoutingTable = *pd.routingTable
	} else if pd.nullFields["routingTable"] {
		merged.RoutingTable = RoutingTableInfo{}
	}

	if pd.healthCheck != nil {
		merged.HealthCheck = pd.healthCheck
	} else if pd.nullFields["healthCheck"] {
		merged.HealthCheck = nil
	}

	if pd.nodeErrors != nil {
		merged.NodeErrors = pd.nodeErrors
	} else if pd.nullFields["nodeErrors"] {
		merged.NodeErrors = nil
	}

	if pd.fetchError != nil {
		merged.FetchError = *pd.fetchError
	} else if pd.nullFields["fetchError"] {
		merged.FetchError = ""
	}

	if pd.lastPushTime != nil {
		merged.LastPushTime = pd.lastPushTime
	} else if pd.nullFields["lastPushTime"] {
		merged.LastPushTime = nil
	}

	if pd.statusSource != nil {
		merged.StatusSource = *pd.statusSource
	} else if pd.nullFields["statusSource"] {
		merged.StatusSource = ""
	}

	if pd.nodePodInfo != nil {
		merged.NodePodInfo = pd.nodePodInfo
	} else if pd.nullFields["nodePodInfo"] {
		merged.NodePodInfo = nil
	}

	if pd.bpfEntries != nil {
		merged.BpfEntries = pd.bpfEntries
	} else if pd.nullFields["bpfEntries"] {
		merged.BpfEntries = nil
	}

	if merged.NodeInfo.Name == "" {
		merged.NodeInfo.Name = nodeName
	}

	return c.commitParsedDeltaBase(nodeName, entry, &merged, peerIdentity, source, prevStatus)
}

func (c *NodeStatusCache) commitParsedDelta(nodeName string, previous *CachedNodeStatus, merged *NodeStatusResponse, peerIdentity *peerIdentityDigest, source string) (uint64, bool, error) {
	return c.commitParsedDeltaBase(nodeName, previous, merged, peerIdentity, source, previous.Status)
}

func (c *NodeStatusCache) commitParsedDeltaBase(nodeName string, previous *CachedNodeStatus, merged *NodeStatusResponse, peerIdentity *peerIdentityDigest, source string, base *NodeStatusResponse) (uint64, bool, error) {
	// Phase 3: Write lock for the brief pointer swap.
	c.mu.Lock()
	// Deletion and recreation can reuse a revision. The identity memo and
	// merged status must still belong to the same entry, not just its number.
	entry, ok := c.entries[nodeName]
	if !ok {
		c.mu.Unlock()
		return 0, true, nil
	}

	if entry != previous {
		// Another goroutine updated this node while we were merging.
		// Our merge is stale; signal resync.
		rev := entry.Revision
		c.mu.Unlock()

		return rev, true, nil
	}

	revision := entry.Revision + 1

	next, err := c.legacyEntryLocked(nodeName, merged, revision, peerIdentity, source, base)
	if err != nil {
		c.mu.Unlock()

		if errors.Is(err, errLegacyDetailBaseUnavailable) {
			return entry.Revision, true, nil
		}

		return entry.Revision, false, err
	}

	c.entries[nodeName] = next
	c.observeLegacyLocked(nodeName, next, previous.Revision)
	c.eventSeq++
	eventSeq := c.eventSeq
	fn := c.onChange
	overviewFn := c.onOverviewChange
	c.mu.Unlock()

	if next.Overview != nil {
		if overviewFn != nil {
			overviewFn(nodeName, next.overviewForNotification(), eventSeq)
		}
	} else if fn != nil {
		fn(nodeName, next.Status, eventSeq)
	}

	return revision, false, nil
}

// SetOnChange sets a callback invoked after cache mutations.
func (c *NodeStatusCache) SetOnChange(fn func(nodeName string, status *NodeStatusResponse, eventSeq uint64)) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.onChange = fn
}

// Get returns shallow copies of the cached entry and status when present.
// Nested slices and maps remain shared.
func (c *NodeStatusCache) Get(nodeName string) (*CachedNodeStatus, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, ok := c.entries[nodeName]
	if !ok {
		return nil, false
	}

	statusCopy := *entry.Status
	copy := *entry
	copy.Status = &statusCopy

	if entry.Overview != nil {
		overviewCopy := *entry.Overview
		copy.Overview = &overviewCopy
	}

	return &copy, true
}

// GetAll returns a snapshot of all cached node statuses. The returned map
// values are shared pointers -- callers must NOT modify the CachedNodeStatus
// or its Status field. This avoids a deep copy of 412+ node statuses.
func (c *NodeStatusCache) GetAll() map[string]*CachedNodeStatus {
	c.mu.RLock()
	defer c.mu.RUnlock()

	snapshot := make(map[string]*CachedNodeStatus, len(c.entries))
	for k, v := range c.entries {
		snapshot[k] = v
	}

	return snapshot
}

// Delete removes a node entry from the cache.
func (c *NodeStatusCache) Delete(nodeName string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.entries, nodeName)

	if c.details != nil {
		c.details.Forget(nodeName)
	}
}

// UpdateSource updates the cached status source for a node without changing
// the cached payload or ReceivedAt timestamp.
func (c *NodeStatusCache) UpdateSource(nodeName, source string) bool {
	return c.UpdateSourceIf(nodeName, "", source)
}

// UpdateSourceIf changes the source only if the expected transport still owns it.
// An empty expected source preserves the unconditional UpdateSource behavior.
func (c *NodeStatusCache) UpdateSourceIf(nodeName, expectedSource, source string) bool {
	if source == "" {
		return false
	}

	c.mu.Lock()

	entry, ok := c.entries[nodeName]
	if !ok || (expectedSource != "" && entry.Source != expectedSource) {
		c.mu.Unlock()
		return false
	}

	if entry.Source == source {
		c.mu.Unlock()
		return true
	}

	updated := *entry
	updated.Source = source

	if entry.Overview != nil {
		overview := *entry.Overview
		overview.StatusSource = source
		metadata := statuspkg.OverviewMetadata(overview)
		updated.Overview = &overview
		updated.Status = &metadata
	}

	c.entries[nodeName] = &updated
	c.eventSeq++
	eventSeq := c.eventSeq
	fn := c.onChange
	overviewFn := c.onOverviewChange
	statusCopy := updated.Status
	c.mu.Unlock()

	if updated.Overview != nil {
		if overviewFn != nil {
			overviewFn(nodeName, updated.overviewForNotification(), eventSeq)
		}
	} else if fn != nil {
		fn(nodeName, statusCopy, eventSeq)
	}

	return true
}

// CleanupStaleEntries removes cached entries for nodes not in validNodes.
func (c *NodeStatusCache) CleanupStaleEntries(validNodes map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for name := range c.entries {
		if !validNodes[name] {
			delete(c.entries, name)

			if c.details != nil {
				c.details.Forget(name)
			}
		}
	}
}

func fetchNodeStatus(ctx context.Context, nodeIP string, port int) (*NodeStatusResponse, error) {
	client := &http.Client{Timeout: 5 * time.Second}

	url := fmt.Sprintf("http://%s:%d/status/json", nodeIP, port)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var nodeStatus NodeStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&nodeStatus); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &nodeStatus, nil
}
