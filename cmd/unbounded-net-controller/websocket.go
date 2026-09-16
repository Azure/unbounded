// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
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
	mu          sync.RWMutex
	clients     map[*WSClient]struct{}
	health      *healthState
	notify      chan struct{}   // buffered 1, coalesces notifications
	seq         uint64          // monotonic broadcast counter
	lastSummary *ClusterSummary // previous summary for delta computation
}

// NewWSBroadcaster creates a new WebSocket broadcaster
func NewWSBroadcaster(health *healthState) *WSBroadcaster {
	return &WSBroadcaster{
		clients: make(map[*WSClient]struct{}),
		health:  health,
		notify:  make(chan struct{}, 1),
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
	defer func() {
		b.mu.Lock()
		defer b.mu.Unlock()

		b.lastSummary = nil

		for client := range b.clients {
			if client.cancel != nil {
				client.cancel()
			}
		}
	}()

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

// broadcastUpdate sends only overview snapshots/deltas, including to clients
// that never subscribed to the summary protocol. History never contains details.
func (b *WSBroadcaster) broadcastUpdate(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}

	status := b.getCachedStatus()
	if status == nil {
		return
	}

	summary := buildClusterSummary(status)

	b.mu.Lock()
	b.seq++
	summary.Seq = b.seq
	previous := b.lastSummary
	b.mu.Unlock()

	message := WSMessage{Type: "cluster_summary", Data: summary}
	if previous != nil {
		delta := computeClusterSummaryDelta(previous, summary)
		if delta == nil {
			return
		}

		message = WSMessage{Type: "cluster_summary_delta", Data: delta}
	}

	data, err := json.Marshal(message)
	if err != nil {
		klog.Errorf("WebSocket: failed to marshal cluster overview: %v", err)

		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	b.lastSummary = summary

	for client := range b.clients {
		select {
		case client.send <- data:
		default:
			klog.V(4).Info("WebSocket: client send buffer full, dropping cluster overview")
		}
	}
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
			b.sendToClient(c, WSMessage{Type: "cluster_summary", Data: buildClusterSummary(status)})
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
