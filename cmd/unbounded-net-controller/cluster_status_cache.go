// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

// ClusterStatusCache maintains a pre-built ClusterStatusResponse in memory,
// updated when events signal that the status is dirty. This avoids expensive
// O(N*P*R) fetchClusterStatus calls on every HTTP request or WS broadcast.
type ClusterStatusCache struct {
	mu     sync.RWMutex
	status *ClusterStatusResponse
	seq    uint64
	health *healthState

	// nodeIndex maps node name to index in status.Nodes for fast patching.
	nodeIndex map[string]int

	// fullRebuildCh signals that infrastructure changed (sites/pools/peerings)
	// and a full rebuild is needed. Buffered 1 for coalescing.
	fullRebuildCh chan struct{}
	// nodeUpdateCh signals that a node's status changed and only that node
	// needs to be patched in the pre-built response. Buffered for coalescing.
	nodeUpdateCh chan struct{}
}

// NewClusterStatusCache creates a new ClusterStatusCache.
func NewClusterStatusCache(health *healthState) *ClusterStatusCache {
	return &ClusterStatusCache{
		health:        health,
		nodeIndex:     make(map[string]int),
		fullRebuildCh: make(chan struct{}, 1),
		nodeUpdateCh:  make(chan struct{}, 1),
	}
}

// Rebuild rebuilds the full status from scratch. Called on startup and
// periodically as a safety net.
func (c *ClusterStatusCache) Rebuild(ctx context.Context) {
	start := time.Now()
	status := fetchClusterStatus(ctx, c.health, c.health.pullEnabled.Load())

	idx := make(map[string]int, len(status.Nodes))
	for i := range status.Nodes {
		idx[status.Nodes[i].NodeInfo.Name] = i
	}

	c.mu.Lock()
	c.seq++
	status.Seq = c.seq
	c.status = status
	c.nodeIndex = idx
	c.mu.Unlock()

	duration := time.Since(start)
	if duration > 5*time.Second {
		klog.V(2).Infof("ClusterStatusCache: rebuild took %v (seq=%d, nodes=%d)", duration, status.Seq, status.NodeCount)
	} else {
		klog.V(4).Infof("ClusterStatusCache: rebuilt in %v (seq=%d, nodes=%d)", duration, status.Seq, status.NodeCount)
	}
}

// PatchNode updates a single node's cached status in-place without a full
// rebuild.
func (c *ClusterStatusCache) PatchNode(nodeName string, nodeStatus *NodeStatusResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.status == nil {
		return
	}

	if i, ok := c.nodeIndex[nodeName]; ok && i < len(c.status.Nodes) {
		// Preserve controller-enriched fields that the node agent doesn't set
		existing := c.status.Nodes[i]
		if nodeStatus.StatusSource == "" {
			nodeStatus.StatusSource = existing.StatusSource
		}

		if nodeStatus.NodeInfo.K8sReady == "" {
			nodeStatus.NodeInfo.K8sReady = existing.NodeInfo.K8sReady
		}

		if !nodeStatus.NodeInfo.IsGateway && existing.NodeInfo.IsGateway {
			nodeStatus.NodeInfo.IsGateway = true
		}

		if nodeStatus.NodeInfo.ProviderID == "" {
			nodeStatus.NodeInfo.ProviderID = existing.NodeInfo.ProviderID
		}

		if nodeStatus.NodeInfo.OSImage == "" {
			nodeStatus.NodeInfo.OSImage = existing.NodeInfo.OSImage
		}

		if nodeStatus.NodeInfo.Kernel == "" {
			nodeStatus.NodeInfo.Kernel = existing.NodeInfo.Kernel
		}

		if nodeStatus.NodeInfo.Kubelet == "" {
			nodeStatus.NodeInfo.Kubelet = existing.NodeInfo.Kubelet
		}

		if nodeStatus.NodeInfo.Arch == "" {
			nodeStatus.NodeInfo.Arch = existing.NodeInfo.Arch
		}

		if nodeStatus.NodeInfo.NodeOS == "" {
			nodeStatus.NodeInfo.NodeOS = existing.NodeInfo.NodeOS
		}

		if len(nodeStatus.NodeInfo.K8sLabels) == 0 && len(existing.NodeInfo.K8sLabels) > 0 {
			nodeStatus.NodeInfo.K8sLabels = existing.NodeInfo.K8sLabels
		}

		if nodeStatus.NodeInfo.K8sUpdatedAt == nil && existing.NodeInfo.K8sUpdatedAt != nil {
			nodeStatus.NodeInfo.K8sUpdatedAt = existing.NodeInfo.K8sUpdatedAt
		}

		if nodeStatus.NodePodInfo == nil && existing.NodePodInfo != nil {
			nodeStatus.NodePodInfo = existing.NodePodInfo
		}

		c.status.Nodes[i] = nodeStatus
	} else {
		c.nodeIndex[nodeName] = len(c.status.Nodes)
		c.status.Nodes = append(c.status.Nodes, nodeStatus)
		c.status.NodeCount = len(c.status.Nodes)
	}

	c.seq++
	c.status.Seq = c.seq
	c.status.Timestamp = time.Now()
}

// MarkDirty signals that a node status changed. The cached response is
// patched in-place by PatchNode; this just notifies the broadcaster.
func (c *ClusterStatusCache) MarkDirty() {
	select {
	case c.nodeUpdateCh <- struct{}{}:
	default:
	}
}

// MarkFullRebuildNeeded signals that infrastructure changed (sites, pools,
// peerings) and a full rebuild is needed.
func (c *ClusterStatusCache) MarkFullRebuildNeeded() {
	select {
	case c.fullRebuildCh <- struct{}{}:
	default:
	}
}

// Get returns the current pre-built status (read-locked, fast).
// Returns nil if the status has not been built yet.
func (c *ClusterStatusCache) Get() *ClusterStatusResponse {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.status
}

// GetSeq returns the current sequence number.
func (c *ClusterStatusCache) GetSeq() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.seq
}

// Run is the main loop that rebuilds the status when dirty.
// It coalesces: after receiving a dirty signal it waits up to 2s for more
// changes before rebuilding. A 60s ticker acts as a safety-net rebuild.
func (c *ClusterStatusCache) Run(ctx context.Context) {
	// Perform the initial build immediately.
	c.Rebuild(ctx)

	const coalesceDelay = 2 * time.Second

	safetyTicker := time.NewTicker(60 * time.Second)
	defer safetyTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.fullRebuildCh:
			// Infrastructure changed -- coalesce briefly then full rebuild.
			coalesceTimer := time.NewTimer(coalesceDelay)

		drain:
			for {
				select {
				case <-ctx.Done():
					coalesceTimer.Stop()
					return
				case <-c.fullRebuildCh:
				case <-c.nodeUpdateCh:
					// Consume node updates too; they'll be covered by the rebuild.
				case <-coalesceTimer.C:
					break drain
				}
			}

			c.Rebuild(ctx)
		case <-c.nodeUpdateCh:
			// Node status changed -- no rebuild needed. PatchNode already
			// updated the cached response in-place. Drain queued updates.
			draining := true
			for draining {
				select {
				case <-c.nodeUpdateCh:
				default:
					draining = false
				}
			}
		case <-safetyTicker.C:
			// Periodic safety-net rebuild (e.g., to pick up new K8s node objects).
			c.Rebuild(ctx)
		}
	}
}
