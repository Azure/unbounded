// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"encoding/json"
	"sort"
	"time"

	statusv1alpha1 "github.com/Azure/unbounded-kube/internal/net/status/v1alpha1"
)

// ClusterStatusResponse is the top-level status response for the cluster.
type ClusterStatusResponse struct {
	Seq                uint64                 `json:"seq"`
	Timestamp          time.Time              `json:"timestamp"`
	NodeCount          int                    `json:"nodeCount"`
	SiteCount          int                    `json:"siteCount"`
	AzureTenantID      string                 `json:"azureTenantId,omitempty"`
	LeaderInfo         *LeaderInfo            `json:"leaderInfo,omitempty"`
	BuildInfo          *BuildInfo             `json:"buildInfo,omitempty"`
	Nodes              []*NodeStatusResponse  `json:"nodes"`
	Sites              []SiteStatus           `json:"sites"`
	GatewayPools       []GatewayPoolStatus    `json:"gatewayPools"`
	Peerings           []PeeringStatus        `json:"peerings"`
	Errors             []string               `json:"errors,omitempty"`
	Warnings           []string               `json:"warnings,omitempty"`
	Problems           []StatusProblem        `json:"problems"`
	ConnectivityMatrix map[string]*SiteMatrix `json:"connectivityMatrix,omitempty"`
	PullEnabled        bool                   `json:"pullEnabled"`
}

// ClusterStatusDelta is a WebSocket delta update.
type ClusterStatusDelta struct {
	Seq                uint64                 `json:"seq"`
	Timestamp          time.Time              `json:"timestamp"`
	NodeCount          int                    `json:"nodeCount"`
	SiteCount          int                    `json:"siteCount"`
	AzureTenantID      string                 `json:"azureTenantId,omitempty"`
	LeaderInfo         *LeaderInfo            `json:"leaderInfo,omitempty"`
	Errors             []string               `json:"errors,omitempty"`
	Warnings           []string               `json:"warnings,omitempty"`
	Problems           []StatusProblem        `json:"problems"`
	UpdatedNodes       []json.RawMessage      `json:"updatedNodes,omitempty"`
	RemovedNodes       []string               `json:"removedNodes,omitempty"`
	Sites              []SiteStatus           `json:"sites"`
	GatewayPools       []GatewayPoolStatus    `json:"gatewayPools"`
	Peerings           []PeeringStatus        `json:"peerings"`
	ConnectivityMatrix map[string]*SiteMatrix `json:"connectivityMatrix,omitempty"`
	PullEnabled        bool                   `json:"pullEnabled"`
}

// StatusProblem describes one unhealthy condition surfaced in cluster status.
type StatusProblem struct {
	Name   string   `json:"name"`
	Type   string   `json:"type"`
	Errors []string `json:"errors"`
}

// LeaderInfo contains current leader pod and node identity.
type LeaderInfo struct {
	PodName  string `json:"podName"`
	NodeName string `json:"nodeName"`
}

// BuildInfo contains controller build metadata.
type BuildInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"buildTime"`
}

// NodeStatusResponse is the node status payload exposed by the status API.
type NodeStatusResponse = statusv1alpha1.NodeStatusResponse

// NodeStatusPushEnvelope carries a push status update from a node.
type NodeStatusPushEnvelope struct {
	Mode         string                     `json:"mode,omitempty"`
	NodeName     string                     `json:"nodeName,omitempty"`
	BaseRevision uint64                     `json:"baseRevision,omitempty"`
	Status       *NodeStatusResponse        `json:"status,omitempty"`
	Delta        map[string]json.RawMessage `json:"delta,omitempty"`
}

// NodeStatusPushAck is the acknowledgment returned for push updates.
type NodeStatusPushAck struct {
	Status   string `json:"status"`
	Revision uint64 `json:"revision,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// NodeStatusWSMessage is the status message format used over WebSockets.
type NodeStatusWSMessage struct {
	Type         string                     `json:"type"`
	NodeName     string                     `json:"nodeName,omitempty"`
	BaseRevision uint64                     `json:"baseRevision,omitempty"`
	Status       *NodeStatusResponse        `json:"status,omitempty"`
	Delta        map[string]json.RawMessage `json:"delta,omitempty"`
}

// NodePodInfo aliases the shared node pod status schema.
type NodePodInfo = statusv1alpha1.NodePodInfo

// RoutingTableInfo aliases the shared routing table schema.
type RoutingTableInfo = statusv1alpha1.RoutingTableInfo

// RouteEntry aliases the shared route entry schema.
type RouteEntry = statusv1alpha1.RouteEntry

// NextHop aliases the shared next hop schema.
type NextHop = statusv1alpha1.NextHop

// NextHopInfo aliases the shared next hop metadata schema.
type NextHopInfo = statusv1alpha1.NextHopInfo

// RouteType aliases the shared route type schema.
type RouteType = statusv1alpha1.RouteType

// NodeInfo aliases the shared node info schema.
type NodeInfo = statusv1alpha1.NodeInfo

// WireGuardStatusInfo aliases the shared WireGuard status schema.
type WireGuardStatusInfo = statusv1alpha1.WireGuardStatusInfo

// HealthCheckPeerStatus aliases the shared health check peer status schema.
type HealthCheckPeerStatus = statusv1alpha1.HealthCheckPeerStatus

// HealthCheckStatus aliases the shared health check status schema.
type HealthCheckStatus = statusv1alpha1.HealthCheckStatus

// PeerTunnelStatus aliases shared tunnel link status details.
type PeerTunnelStatus = statusv1alpha1.PeerTunnelStatus

// WireGuardPeerLinkStatus aliases shared WireGuard link status schema.
type WireGuardPeerLinkStatus = statusv1alpha1.WireGuardPeerLinkStatus

// NodeError aliases the shared node error schema.
type NodeError = statusv1alpha1.NodeError

// WireGuardPeerStatus aliases the shared WireGuard peer status schema.
type WireGuardPeerStatus = statusv1alpha1.WireGuardPeerStatus

// BpfEntry aliases the shared BPF trie entry schema.
type BpfEntry = statusv1alpha1.BpfEntry

// ClusterSummary is a lightweight overview of cluster state. It contains
// everything from ClusterStatusResponse except detailed per-node status
// (routes, peers, health check details), replacing those with NodeSummary rows.
type ClusterSummary struct {
	Seq                uint64                 `json:"seq"`
	Timestamp          time.Time              `json:"timestamp"`
	NodeCount          int                    `json:"nodeCount"`
	SiteCount          int                    `json:"siteCount"`
	AzureTenantID      string                 `json:"azureTenantId,omitempty"`
	LeaderInfo         *LeaderInfo            `json:"leaderInfo,omitempty"`
	BuildInfo          *BuildInfo             `json:"buildInfo,omitempty"`
	Sites              []SiteStatus           `json:"sites"`
	GatewayPools       []GatewayPoolStatus    `json:"gatewayPools"`
	Peerings           []PeeringStatus        `json:"peerings"`
	Errors             []string               `json:"errors,omitempty"`
	Warnings           []string               `json:"warnings,omitempty"`
	Problems           []StatusProblem        `json:"problems"`
	PullEnabled        bool                   `json:"pullEnabled"`
	NodeSummaries      []NodeSummary          `json:"nodeSummaries"`
	ConnectivityMatrix map[string]*SiteMatrix `json:"connectivityMatrix,omitempty"`
}

// NodeSummary is a compact per-node summary for use in ClusterSummary.
type NodeSummary struct {
	Name          string `json:"name"`
	SiteName      string `json:"siteName,omitempty"`
	IsGateway     bool   `json:"isGateway,omitempty"`
	K8sReady      string `json:"k8sReady,omitempty"`
	StatusSource  string `json:"statusSource,omitempty"`
	CniStatus     string `json:"cniStatus,omitempty"`
	CniTone       string `json:"cniTone,omitempty"`
	ErrorCount    int    `json:"errorCount,omitempty"`
	FirstError    string `json:"firstError,omitempty"`
	PeerCount     int    `json:"peerCount,omitempty"`
	HealthyPeers  int    `json:"healthyPeers,omitempty"`
	RouteCount    int    `json:"routeCount,omitempty"`
	RouteMismatch bool   `json:"routeMismatch,omitempty"`
	FetchError    string `json:"fetchError,omitempty"`
}

// buildClusterSummary extracts a ClusterSummary from a full ClusterStatusResponse.
// This is O(N) in nodes with simple field reads -- no route annotation work.
func buildClusterSummary(status *ClusterStatusResponse) *ClusterSummary {
	summaries := make([]NodeSummary, 0, len(status.Nodes))
	now := time.Now()

	for i := range status.Nodes {
		node := status.Nodes[i]
		ns := NodeSummary{
			Name:         node.NodeInfo.Name,
			SiteName:     node.NodeInfo.SiteName,
			IsGateway:    node.NodeInfo.IsGateway,
			K8sReady:     node.NodeInfo.K8sReady,
			StatusSource: node.StatusSource,
			PeerCount:    len(node.Peers),
			RouteCount:   len(node.RoutingTable.Routes),
			FetchError:   node.FetchError,
			ErrorCount:   len(node.NodeErrors),
		}

		// Include first error message so the frontend can show it inline
		// when there is exactly one error, rather than a generic count.
		if len(node.NodeErrors) > 0 {
			ns.FirstError = node.NodeErrors[0].Message
		}

		// Count healthy peers
		for j := range node.Peers {
			peer := &node.Peers[j]
			if peer.HealthCheck != nil && peer.HealthCheck.Enabled {
				if peer.HealthCheck.Status == "up" || peer.HealthCheck.Status == "Up" {
					ns.HealthyPeers++
				}
			} else {
				// Fall back to handshake freshness
				if !peer.Tunnel.LastHandshake.IsZero() && now.Sub(peer.Tunnel.LastHandshake) < 3*time.Minute {
					ns.HealthyPeers++
				}
			}
		}

		// Route mismatch check
		for _, route := range node.RoutingTable.Routes {
			for _, hop := range route.NextHops {
				expected := hop.Expected != nil && *hop.Expected

				present := hop.Present != nil && *hop.Present
				if expected != present {
					ns.RouteMismatch = true
					break
				}
			}

			if ns.RouteMismatch {
				break
			}
		}

		// Derive CNI status and tone
		ns.CniStatus, ns.CniTone = deriveCniStatusAndTone(node, ns.RouteMismatch)
		summaries = append(summaries, ns)
	}

	sort.Slice(summaries, func(i, j int) bool { return summaries[i].Name < summaries[j].Name })

	return &ClusterSummary{
		Seq:                status.Seq,
		Timestamp:          status.Timestamp,
		NodeCount:          status.NodeCount,
		SiteCount:          status.SiteCount,
		AzureTenantID:      status.AzureTenantID,
		LeaderInfo:         status.LeaderInfo,
		BuildInfo:          status.BuildInfo,
		Sites:              status.Sites,
		GatewayPools:       status.GatewayPools,
		Peerings:           status.Peerings,
		Errors:             status.Errors,
		Warnings:           status.Warnings,
		Problems:           status.Problems,
		PullEnabled:        status.PullEnabled,
		NodeSummaries:      summaries,
		ConnectivityMatrix: status.ConnectivityMatrix,
	}
}

// deriveCniStatusAndTone computes a human-readable CNI status label and its
// corresponding UI tone from a single node's status.
func deriveCniStatusAndTone(node *NodeStatusResponse, routeMismatch bool) (string, string) {
	src := node.StatusSource
	if src == "no-data" {
		return "No data", "warning"
	}

	if node.FetchError != "" {
		return "Fetch error", "danger"
	}

	if len(node.NodeErrors) > 0 {
		return "Errors", "danger"
	}

	if routeMismatch {
		return "Route mismatch", "warning"
	}

	if src == "stale" || src == "error" {
		return "Stale", "warning"
	}

	if src == "" {
		return "Unknown", "warning"
	}

	return "Healthy", "success"
}

// SiteStatus summarizes node health for a site.
type SiteStatus struct {
	Name            string   `json:"name"`
	NodeCount       int      `json:"nodeCount"`
	OnlineCount     int      `json:"onlineCount"`
	OfflineCount    int      `json:"offlineCount"`
	NodeCidrs       []string `json:"nodeCidrs"`
	PodCidrs        []string `json:"podCidrs,omitempty"`
	ManageCniPlugin bool     `json:"manageCniPlugin"`
}

// GatewayPoolStatus summarizes gateway pool connectivity state.
type GatewayPoolStatus struct {
	Name           string   `json:"name"`
	SiteName       string   `json:"siteName,omitempty"`
	NodeCount      int      `json:"nodeCount"`
	Gateways       []string `json:"gateways"`
	ConnectedSites []string `json:"connectedSites,omitempty"`
	ReachableSites []string `json:"reachableSites,omitempty"`
}

// PeeringStatus summarizes effective peering relationships.
type PeeringStatus struct {
	Name               string   `json:"name"`
	Sites              []string `json:"sites"`
	GatewayPools       []string `json:"gatewayPools,omitempty"`
	HealthCheckEnabled bool     `json:"healthCheckEnabled,omitempty"`
}

// SiteMatrix contains connectivity results across site nodes.
type SiteMatrix struct {
	Nodes   []string                     `json:"nodes"`
	Results map[string]map[string]string `json:"results"` // src -> dst -> status
}

// ClusterSummaryDelta contains only the fields of ClusterSummary that changed
// since the last broadcast. NodeSummaries contains only added/changed entries;
// RemovedNodes lists nodes that disappeared.
type ClusterSummaryDelta struct {
	Seq                uint64                 `json:"seq"`
	Timestamp          time.Time              `json:"timestamp"`
	NodeCount          *int                   `json:"nodeCount,omitempty"`
	SiteCount          *int                   `json:"siteCount,omitempty"`
	AzureTenantID      *string                `json:"azureTenantId,omitempty"`
	LeaderInfo         *LeaderInfo            `json:"leaderInfo,omitempty"`
	BuildInfo          *BuildInfo             `json:"buildInfo,omitempty"`
	Sites              []SiteStatus           `json:"sites,omitempty"`
	GatewayPools       []GatewayPoolStatus    `json:"gatewayPools,omitempty"`
	Peerings           []PeeringStatus        `json:"peerings,omitempty"`
	Errors             []string               `json:"errors,omitempty"`
	Warnings           []string               `json:"warnings,omitempty"`
	Problems           []StatusProblem        `json:"problems,omitempty"`
	PullEnabled        *bool                  `json:"pullEnabled,omitempty"`
	NodeSummaries      []NodeSummary          `json:"nodeSummaries,omitempty"`
	RemovedNodes       []string               `json:"removedNodes,omitempty"`
	ConnectivityMatrix map[string]*SiteMatrix `json:"connectivityMatrix,omitempty"`
}

// computeClusterSummaryDelta computes a delta between two ClusterSummary snapshots.
// Returns nil if nothing changed.
func computeClusterSummaryDelta(prev, curr *ClusterSummary) *ClusterSummaryDelta {
	if prev == nil || curr == nil {
		return nil
	}

	delta := &ClusterSummaryDelta{
		Seq:       curr.Seq,
		Timestamp: curr.Timestamp,
	}
	changed := false

	if prev.NodeCount != curr.NodeCount {
		v := curr.NodeCount
		delta.NodeCount = &v
		changed = true
	}

	if prev.SiteCount != curr.SiteCount {
		v := curr.SiteCount
		delta.SiteCount = &v
		changed = true
	}

	if prev.AzureTenantID != curr.AzureTenantID {
		v := curr.AzureTenantID
		delta.AzureTenantID = &v
		changed = true
	}

	if prev.PullEnabled != curr.PullEnabled {
		v := curr.PullEnabled
		delta.PullEnabled = &v
		changed = true
	}

	// Compare LeaderInfo by JSON
	if !jsonEqual(prev.LeaderInfo, curr.LeaderInfo) {
		delta.LeaderInfo = curr.LeaderInfo
		changed = true
	}

	if !jsonEqual(prev.BuildInfo, curr.BuildInfo) {
		delta.BuildInfo = curr.BuildInfo
		changed = true
	}

	if !jsonEqual(prev.Sites, curr.Sites) {
		delta.Sites = curr.Sites
		changed = true
	}

	if !jsonEqual(prev.GatewayPools, curr.GatewayPools) {
		delta.GatewayPools = curr.GatewayPools
		changed = true
	}

	if !jsonEqual(prev.Peerings, curr.Peerings) {
		delta.Peerings = curr.Peerings
		changed = true
	}

	if !jsonEqual(prev.Errors, curr.Errors) {
		delta.Errors = curr.Errors
		changed = true
	}

	if !jsonEqual(prev.Warnings, curr.Warnings) {
		delta.Warnings = curr.Warnings
		changed = true
	}

	if !jsonEqual(prev.Problems, curr.Problems) {
		delta.Problems = curr.Problems
		changed = true
	}

	if !jsonEqual(prev.ConnectivityMatrix, curr.ConnectivityMatrix) {
		delta.ConnectivityMatrix = curr.ConnectivityMatrix
		changed = true
	}

	// NodeSummaries: diff by name
	prevByName := make(map[string]NodeSummary, len(prev.NodeSummaries))
	for _, ns := range prev.NodeSummaries {
		prevByName[ns.Name] = ns
	}

	for _, ns := range curr.NodeSummaries {
		old, existed := prevByName[ns.Name]
		if !existed || ns != old {
			delta.NodeSummaries = append(delta.NodeSummaries, ns)
			changed = true
		}
	}

	currByName := make(map[string]struct{}, len(curr.NodeSummaries))
	for _, ns := range curr.NodeSummaries {
		currByName[ns.Name] = struct{}{}
	}

	for name := range prevByName {
		if _, exists := currByName[name]; !exists {
			delta.RemovedNodes = append(delta.RemovedNodes, name)
			changed = true
		}
	}

	if len(delta.RemovedNodes) > 0 {
		sort.Strings(delta.RemovedNodes)
	}

	if !changed {
		return nil
	}

	return delta
}

// jsonEqual compares two values by their JSON representation.
func jsonEqual(a, b interface{}) bool {
	aj, _ := json.Marshal(a) //nolint:errcheck
	bj, _ := json.Marshal(b) //nolint:errcheck

	return bytes.Equal(aj, bj)
}
