// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"sync"
	"time"

	"github.com/coder/websocket"
	"k8s.io/klog/v2"
)

// ---- WebSocket types and broadcaster ----
type WSMessage struct {
	Type     string      `json:"type"`
	Data     interface{} `json:"data,omitempty"`
	Message  string      `json:"message,omitempty"`
	NodeName string      `json:"nodeName,omitempty"`
}

// WSClientMessage is the client->server message
type WSClientMessage struct {
	Type     string `json:"type"`
	Enabled  bool   `json:"enabled,omitempty"`  // for set_pull_enabled
	NodeName string `json:"nodeName,omitempty"` // for node_detail_request / subscribe / unsubscribe
}

// WSClient represents a single WebSocket connection
type WSClient struct {
	conn   *websocket.Conn
	send   chan []byte // buffered outbound messages
	ctx    context.Context
	cancel context.CancelFunc

	// Summary protocol fields (protected by WSBroadcaster.mu during broadcast)
	summarySubscribed       bool
	nodeDetailSubscriptions map[string]bool
}

// WSBroadcaster manages all WebSocket clients and broadcasts updates
type WSBroadcaster struct {
	mu               sync.RWMutex
	clients          map[*WSClient]struct{}
	health           *healthState
	notify           chan struct{}     // buffered 1, coalesces notifications
	seq              uint64            // monotonic broadcast counter
	lastNodeJSON     map[string][]byte // nodeName → JSON bytes of last-broadcast NodeStatusResponse
	lastNodeFullJSON map[string][]byte // nodeName → full JSON for field-level diffing
	lastSummary      *ClusterSummary   // previous summary for delta computation
}

// NewWSBroadcaster creates a new WebSocket broadcaster
func NewWSBroadcaster(health *healthState) *WSBroadcaster {
	return &WSBroadcaster{
		clients:      make(map[*WSClient]struct{}),
		health:       health,
		notify:       make(chan struct{}, 1),
		lastNodeJSON: nil, // nil signals first broadcast should be full snapshot
	}
}

// getSeq returns the current broadcast sequence number (thread-safe)
func (b *WSBroadcaster) getSeq() uint64 {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return b.seq
}

// Register adds a client to the broadcaster
func (b *WSBroadcaster) Register(client *WSClient) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.clients[client] = struct{}{}
	klog.V(3).Infof("WebSocket client registered (total: %d)", len(b.clients))
}

// Unregister removes a client from the broadcaster and closes its send channel
func (b *WSBroadcaster) Unregister(client *WSClient) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, ok := b.clients[client]; ok {
		delete(b.clients, client)
		close(client.send)
		klog.V(3).Infof("WebSocket client unregistered (total: %d)", len(b.clients))
	}
}

// Notify signals that new data is available (non-blocking, coalesces duplicates)
func (b *WSBroadcaster) Notify() {
	select {
	case b.notify <- struct{}{}:
	default:
		// Already notified, skip
	}
}

// ClientCount returns the number of connected clients
func (b *WSBroadcaster) ClientCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return len(b.clients)
}

// Run is the main broadcast loop
func (b *WSBroadcaster) Run(ctx context.Context) {
	coalesceTicker := time.NewTicker(2 * time.Second)
	defer coalesceTicker.Stop()

	maxTicker := time.NewTicker(10 * time.Second)
	defer maxTicker.Stop()

	dirty := false

	for {
		select {
		case <-ctx.Done():
			return
		case <-b.notify:
			dirty = true
		case <-coalesceTicker.C:
			if dirty && b.ClientCount() > 0 {
				b.broadcastUpdate(ctx)

				dirty = false
			}
		case <-maxTicker.C:
			if b.ClientCount() > 0 {
				b.broadcastUpdate(ctx)

				dirty = false
			}
		}
	}
}

// computeNodeDelta returns a partial JSON object containing only the top-level fields
// that differ between prev and curr. Always includes nodeInfo for identification.
func computeNodeDelta(prev, curr []byte) json.RawMessage {
	var prevMap, currMap map[string]json.RawMessage
	if err := json.Unmarshal(prev, &prevMap); err != nil {
		return json.RawMessage(curr)
	}

	if err := json.Unmarshal(curr, &currMap); err != nil {
		return json.RawMessage(curr)
	}

	delta := make(map[string]json.RawMessage)
	// Always include nodeInfo for identification
	if ni, ok := currMap["nodeInfo"]; ok {
		delta["nodeInfo"] = ni
	}

	for key, currVal := range currMap {
		if key == "nodeInfo" {
			continue
		}

		prevVal, existed := prevMap[key]
		if !existed || !bytes.Equal(prevVal, currVal) {
			delta[key] = currVal
		}
	}
	// Handle removed keys
	for key := range prevMap {
		if key == "nodeInfo" {
			continue
		}

		if _, exists := currMap[key]; !exists {
			delta[key] = json.RawMessage("null")
		}
	}

	result, err := json.Marshal(delta)
	if err != nil {
		return json.RawMessage(curr)
	}

	return result
}

// broadcastUpdate fetches cluster status and sends a delta (or full snapshot on first broadcast) to all clients
func (b *WSBroadcaster) broadcastUpdate(ctx context.Context) {
	broadcastStart := time.Now()

	var status *ClusterStatusResponse
	if b.health.clusterStatusCache != nil {
		status = b.health.clusterStatusCache.Get()
	}

	if status == nil {
		return
	}

	// Check if any clients need legacy full/delta data (non-summary-subscribed).
	// Skip the expensive per-node JSON marshaling if all clients use summary mode.
	b.mu.RLock()

	hasLegacyClients := false
	hasNodeDetailSubs := false

	for c := range b.clients {
		if !c.summarySubscribed {
			hasLegacyClients = true
		}

		if len(c.nodeDetailSubscriptions) > 0 {
			hasNodeDetailSubs = true
		}
	}

	b.mu.RUnlock()

	// Only marshal per-node JSON when legacy clients or node detail subscribers need it
	var currentNodeFullJSON map[string][]byte
	if hasLegacyClients || hasNodeDetailSubs {
		currentNodeFullJSON = make(map[string][]byte, len(status.Nodes))
		for i := range status.Nodes {
			nodeData, err := json.Marshal(status.Nodes[i])
			if err != nil {
				klog.Errorf("WebSocket: failed to marshal node %s: %v", status.Nodes[i].NodeInfo.Name, err)
				continue
			}

			currentNodeFullJSON[status.Nodes[i].NodeInfo.Name] = nodeData
		}
	}

	b.mu.Lock()
	b.seq++
	currentSeq := b.seq
	isFirstBroadcast := b.lastNodeJSON == nil && b.lastSummary == nil
	b.mu.Unlock()

	if isFirstBroadcast {
		// First broadcast: send full snapshot to all clients.
		status.Seq = currentSeq
		fullMsg := WSMessage{Type: "cluster_status", Data: status}

		fullData, err := json.Marshal(fullMsg)
		if err != nil {
			klog.Errorf("WebSocket: failed to marshal cluster status: %v", err)
			return
		}

		summary := buildClusterSummary(status)
		summaryMsg := WSMessage{Type: "cluster_summary", Data: summary}

		summaryData, err := json.Marshal(summaryMsg)
		if err != nil {
			klog.Errorf("WebSocket: failed to marshal cluster summary: %v", err)
			return
		}

		b.mu.RLock()

		clients := make([]*WSClient, 0, len(b.clients))
		for c := range b.clients {
			clients = append(clients, c)
		}

		b.mu.RUnlock()

		for _, c := range clients {
			payload := fullData
			if c.summarySubscribed {
				payload = summaryData
			}

			select {
			case c.send <- payload:
			default:
				klog.V(4).Info("WebSocket: client send buffer full, dropping cluster_status")
			}
		}

		// Send node_detail_update for subscribed nodes
		b.sendNodeDetailUpdates(status, nil, clients, currentNodeFullJSON)

		b.mu.Lock()
		b.lastNodeJSON = currentNodeFullJSON
		b.lastNodeFullJSON = currentNodeFullJSON
		b.lastSummary = buildClusterSummary(status)
		b.mu.Unlock()

		return
	}

	// Fast path: if all clients use summary mode and no node detail
	// subscriptions exist, skip the expensive per-node delta computation.
	if !hasLegacyClients && !hasNodeDetailSubs {
		summaryStart := time.Now()
		summary := buildClusterSummary(status)
		summaryDur := time.Since(summaryStart)

		b.mu.RLock()
		prevSummary := b.lastSummary
		b.mu.RUnlock()

		var summaryData []byte

		deltaStart := time.Now()

		if prevSummary != nil {
			delta := computeClusterSummaryDelta(prevSummary, summary)
			if delta == nil {
				if dur := time.Since(broadcastStart); dur > 100*time.Millisecond {
					klog.V(2).Infof("WebSocket: broadcast (no-op) took %v (summary=%v)", dur, summaryDur)
				}

				return
			}

			klog.V(4).Infof("WebSocket: summary delta: %d nodeSummaries, %d removed, sites=%v pools=%v matrix=%v",
				len(delta.NodeSummaries), len(delta.RemovedNodes), delta.Sites != nil, delta.GatewayPools != nil, delta.ConnectivityMatrix != nil)
			msg := WSMessage{Type: "cluster_summary_delta", Data: delta}
			summaryData, _ = json.Marshal(msg) //nolint:errcheck
		} else {
			msg := WSMessage{Type: "cluster_summary", Data: summary}
			summaryData, _ = json.Marshal(msg) //nolint:errcheck
		}

		deltaDur := time.Since(deltaStart)

		if summaryData != nil {
			b.mu.RLock()

			clients := make([]*WSClient, 0, len(b.clients))
			for c := range b.clients {
				clients = append(clients, c)
			}

			b.mu.RUnlock()

			for _, c := range clients {
				select {
				case c.send <- summaryData:
				default:
					klog.V(4).Info("WebSocket: client send buffer full, dropping cluster_summary")
				}
			}
		}

		b.mu.Lock()
		b.lastSummary = summary
		b.mu.Unlock()

		if dur := time.Since(broadcastStart); dur > 100*time.Millisecond {
			klog.Infof("WebSocket: broadcast took %v (summary=%v, delta=%v, marshal=%v, nodes=%d)",
				dur, summaryDur, deltaDur, dur-summaryDur-deltaDur, len(status.Nodes))
		}

		return
	}

	// Compute delta: compare current vs last
	b.mu.RLock()
	lastJSON := b.lastNodeJSON
	lastFullJSON := b.lastNodeFullJSON
	b.mu.RUnlock()

	var (
		updatedNodes []json.RawMessage
		removedNodes []string
	)

	// Find updated or new nodes

	for i := range status.Nodes {
		name := status.Nodes[i].NodeInfo.Name
		lastData, existed := lastJSON[name]

		currData := currentNodeFullJSON[name]
		if !existed {
			// New node: send full object
			updatedNodes = append(updatedNodes, json.RawMessage(currData))
		} else if !bytes.Equal(currData, lastData) {
			// Changed node: compute field-level delta
			prevFull := lastFullJSON[name]
			if prevFull == nil {
				// No previous full JSON available, send full object
				updatedNodes = append(updatedNodes, json.RawMessage(currData))
			} else {
				updatedNodes = append(updatedNodes, computeNodeDelta(prevFull, currData))
			}
		}
	}

	// Find removed nodes (in last but not in current)
	for name := range lastJSON {
		if _, exists := currentNodeFullJSON[name]; !exists {
			removedNodes = append(removedNodes, name)
		}
	}

	// Build and send delta
	delta := ClusterStatusDelta{
		Seq:           currentSeq,
		Timestamp:     status.Timestamp,
		NodeCount:     status.NodeCount,
		SiteCount:     status.SiteCount,
		AzureTenantID: status.AzureTenantID,
		LeaderInfo:    status.LeaderInfo,
		Errors:        status.Errors,
		Warnings:      status.Warnings,
		Problems:      status.Problems,
		UpdatedNodes:  updatedNodes,
		RemovedNodes:  removedNodes,
		Sites:         status.Sites,
		GatewayPools:  status.GatewayPools,
		Peerings:      status.Peerings,
		PullEnabled:   status.PullEnabled,
	}

	// Always include ConnectivityMatrix so link-state-only changes refresh clients.
	delta.ConnectivityMatrix = status.ConnectivityMatrix

	msg := WSMessage{Type: "cluster_status_delta", Data: delta}

	deltaData, err := json.Marshal(msg)
	if err != nil {
		klog.Errorf("WebSocket: failed to marshal cluster status delta: %v", err)
		return
	}

	if len(updatedNodes) > 0 || len(removedNodes) > 0 {
		klog.V(4).Infof("WebSocket: delta seq=%d: %d updated, %d removed (of %d total), %d bytes",
			currentSeq, len(updatedNodes), len(removedNodes), len(status.Nodes), len(deltaData))
	}

	// Build summary delta for subscribed clients
	summary := buildClusterSummary(status)

	b.mu.RLock()
	prevSummary := b.lastSummary
	b.mu.RUnlock()

	var summaryData []byte

	if prevSummary != nil {
		summaryDelta := computeClusterSummaryDelta(prevSummary, summary)
		if summaryDelta != nil {
			msg := WSMessage{Type: "cluster_summary_delta", Data: summaryDelta}
			summaryData, _ = json.Marshal(msg) //nolint:errcheck
		}
		// If delta is nil, no summary update needed for subscribed clients
	} else {
		msg := WSMessage{Type: "cluster_summary", Data: summary}
		summaryData, _ = json.Marshal(msg) //nolint:errcheck
	}

	// Build set of changed node names for node_detail_update.
	// Cap the allocation hint to avoid overflow from adding two lengths.
	changedCap := len(updatedNodes)
	if len(removedNodes) <= math.MaxInt-changedCap {
		changedCap += len(removedNodes)
	}

	changedNodes := make(map[string]bool, changedCap)

	for _, raw := range updatedNodes {
		var partial struct {
			NodeInfo struct {
				Name string `json:"name"`
			} `json:"nodeInfo"`
		}
		if json.Unmarshal(raw, &partial) == nil && partial.NodeInfo.Name != "" {
			changedNodes[partial.NodeInfo.Name] = true
		}
	}

	for _, name := range removedNodes {
		changedNodes[name] = true
	}

	// Send to clients based on subscription mode
	b.mu.RLock()

	clients := make([]*WSClient, 0, len(b.clients))
	for c := range b.clients {
		clients = append(clients, c)
	}

	b.mu.RUnlock()

	for _, c := range clients {
		if c.summarySubscribed {
			// Summary-subscribed clients get the lightweight summary
			if summaryData != nil {
				select {
				case c.send <- summaryData:
				default:
					klog.V(4).Info("WebSocket: client send buffer full, dropping cluster_summary")
				}
			}
		} else {
			// Legacy clients get the full delta
			select {
			case c.send <- deltaData:
			default:
				klog.V(4).Info("WebSocket: client send buffer full, dropping cluster_status_delta")
			}
		}
	}

	// Send node_detail_update for any node subscriptions on changed nodes
	b.sendNodeDetailUpdates(status, changedNodes, clients, currentNodeFullJSON)

	// Update tracking state
	b.mu.Lock()
	b.lastNodeJSON = currentNodeFullJSON
	b.lastNodeFullJSON = currentNodeFullJSON
	b.lastSummary = summary
	b.mu.Unlock()
}

// sendToClient sends a single message to a specific client (non-blocking)
func (b *WSBroadcaster) sendToClient(client *WSClient, msg WSMessage) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}

	select {
	case client.send <- data:
	default:
		// Buffer full
	}
}

// sendNodeDetailUpdates sends node_detail_update messages to clients that have
// node detail subscriptions. If changedNodes is nil (first broadcast), all
// subscribed nodes are sent. Otherwise only nodes in changedNodes are sent.
func (b *WSBroadcaster) sendNodeDetailUpdates(status *ClusterStatusResponse, changedNodes map[string]bool, clients []*WSClient, currentNodeFullJSON map[string][]byte) {
	// Build a quick lookup of node index by name
	nodeByName := make(map[string]int, len(status.Nodes))
	for i := range status.Nodes {
		nodeByName[status.Nodes[i].NodeInfo.Name] = i
	}

	// Pre-marshal node detail messages to avoid repeated marshaling
	detailCache := make(map[string][]byte)

	for _, c := range clients {
		if len(c.nodeDetailSubscriptions) == 0 {
			continue
		}

		for nodeName := range c.nodeDetailSubscriptions {
			// On first broadcast (changedNodes == nil) send all; otherwise only changed
			if changedNodes != nil && !changedNodes[nodeName] {
				continue
			}

			detailData, ok := detailCache[nodeName]
			if !ok {
				idx, exists := nodeByName[nodeName]
				if !exists {
					continue
				}

				msg := WSMessage{Type: "node_detail_update", Data: status.Nodes[idx], NodeName: nodeName}

				var err error

				detailData, err = json.Marshal(msg)
				if err != nil {
					continue
				}

				detailCache[nodeName] = detailData
			}

			select {
			case c.send <- detailData:
			default:
				klog.V(4).Infof("WebSocket: client send buffer full, dropping node_detail_update for %s", nodeName)
			}
		}
	}
}

// sendNodeDetail looks up a node in the cached status and sends it to the client.
func (b *WSBroadcaster) sendNodeDetail(client *WSClient, nodeName, msgType string) {
	status := b.getCachedStatus()
	if status == nil {
		return
	}

	for i := range status.Nodes {
		if status.Nodes[i].NodeInfo.Name == nodeName {
			b.sendToClient(client, WSMessage{Type: msgType, Data: status.Nodes[i], NodeName: nodeName})
			return
		}
	}
}

// getCachedStatus returns the current pre-built cluster status, or nil if unavailable.
func (b *WSBroadcaster) getCachedStatus() *ClusterStatusResponse {
	if b.health == nil {
		return nil
	}

	if b.health.clusterStatusCache != nil {
		if s := b.health.clusterStatusCache.Get(); s != nil {
			return s
		}
	}

	return nil
}

// readPump reads messages from the client
func (c *WSClient) readPump(b *WSBroadcaster) {
	defer func() {
		b.Unregister(c)

		if err := c.conn.CloseNow(); err != nil {
			klog.V(4).Infof("WebSocket close failed: %v", err)
		}
	}()

	for {
		_, data, err := c.conn.Read(c.ctx)
		if err != nil {
			return
		}

		var msg WSClientMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}

		switch msg.Type {
		case "refresh":
			// Send immediate cluster status snapshot to this client only
			var status *ClusterStatusResponse
			if b.health.clusterStatusCache != nil {
				status = b.health.clusterStatusCache.Get()
			}

			if status == nil {
				fetchCtx, cancel := context.WithTimeout(c.ctx, 30*time.Second)
				status = fetchClusterStatus(fetchCtx, b.health, b.health.pullEnabled.Load())

				cancel()
			}

			status.Seq = b.getSeq()
			b.sendToClient(c, WSMessage{Type: "cluster_status", Data: status})
		case "set_pull_enabled":
			b.health.pullEnabled.Store(msg.Enabled)

			enabledStr := "disabled"
			if msg.Enabled {
				enabledStr = "enabled"
			}

			klog.V(3).Infof("WebSocket: pull fallback %s by client", enabledStr)
			// Trigger a broadcast so all clients see the updated pullEnabled state
			b.Notify()
		case "cluster_summary_subscribe":
			b.mu.Lock()
			c.summarySubscribed = true
			b.mu.Unlock()
			klog.V(4).Info("WebSocket: client subscribed to cluster_summary")
			// Trigger an immediate broadcast so the client gets data quickly.
			// The broadcast loop handles initial vs delta logic.
			b.Notify()
		case "cluster_summary_unsubscribe":
			b.mu.Lock()
			c.summarySubscribed = false
			b.mu.Unlock()
			klog.V(4).Info("WebSocket: client unsubscribed from cluster_summary")
		case "node_detail_request":
			if msg.NodeName == "" {
				continue
			}

			b.sendNodeDetail(c, msg.NodeName, "node_detail_response")
		case "node_detail_subscribe":
			if msg.NodeName == "" {
				continue
			}

			b.mu.Lock()
			c.nodeDetailSubscriptions[msg.NodeName] = true
			b.mu.Unlock()
			klog.V(4).Infof("WebSocket: client subscribed to node_detail for %s", msg.NodeName)
			// Send current detail immediately
			b.sendNodeDetail(c, msg.NodeName, "node_detail_response")
		case "node_detail_unsubscribe":
			if msg.NodeName == "" {
				continue
			}

			b.mu.Lock()
			delete(c.nodeDetailSubscriptions, msg.NodeName)
			b.mu.Unlock()
			klog.V(4).Infof("WebSocket: client unsubscribed from node_detail for %s", msg.NodeName)
		}
	}
}

// writePump writes messages from the send channel to the WebSocket connection
func (c *WSClient) writePump() {
	defer func() {
		if err := c.conn.CloseNow(); err != nil {
			klog.V(4).Infof("WebSocket close failed: %v", err)
		}
	}()

	for {
		select {
		case <-c.ctx.Done():
			return
		case msg, ok := <-c.send:
			if !ok {
				// Channel closed
				_ = c.conn.Close(websocket.StatusNormalClosure, "closing") //nolint:errcheck
				return
			}

			if err := c.conn.Write(c.ctx, websocket.MessageText, msg); err != nil {
				return
			}
		}
	}
}
