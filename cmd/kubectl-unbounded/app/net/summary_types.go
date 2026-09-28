// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package net

import (
	"time"

	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

// clusterSummary is the lightweight cluster overview received via the WS
// summary protocol. It mirrors the controller's ClusterSummary type, carrying
// nodeSummary entries instead of full NodeStatusResponse objects.
type clusterSummary struct {
	Seq           uint64              `json:"seq"`
	Timestamp     time.Time           `json:"timestamp"`
	NodeCount     int                 `json:"nodeCount"`
	SiteCount     int                 `json:"siteCount"`
	LeaderInfo    *leaderInfo         `json:"leaderInfo,omitempty"`
	Sites         []siteStatus        `json:"sites"`
	GatewayPools  []gatewayPoolStatus `json:"gatewayPools"`
	Errors        []string            `json:"errors,omitempty"`
	Warnings      []string            `json:"warnings,omitempty"`
	PullEnabled   bool                `json:"pullEnabled"`
	NodeSummaries []nodeSummary       `json:"nodeSummaries"`
}

// nodeSummary is a compact per-node row for the summary table. It matches the
// controller's NodeSummary type and provides pre-computed status fields so the
// plugin can render a useful table without full per-node data.
type nodeSummary struct {
	NodeInfo        *statusv1alpha1.NodeInfo `json:"nodeInfo,omitempty"`
	LastPushTime    *time.Time               `json:"lastPushTime,omitempty"`
	WireGuardOnline bool                     `json:"wireGuardOnline"`

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

func (n nodeSummary) statusMetadata() statusv1alpha1.NodeStatusResponse {
	var info statusv1alpha1.NodeInfo
	if n.NodeInfo != nil {
		info = *n.NodeInfo
	}

	info.Name, info.SiteName, info.IsGateway, info.K8sReady = n.Name, n.SiteName, n.IsGateway, n.K8sReady

	return statusv1alpha1.NodeStatusResponse{
		NodeInfo: info, LastPushTime: n.LastPushTime,
		StatusSource: n.StatusSource, FetchError: n.FetchError,
	}
}

// wsClientMessage is the envelope for client-to-controller WebSocket messages
// such as subscription requests.
type wsClientMessage struct {
	Type     string `json:"type"`
	NodeName string `json:"nodeName,omitempty"`
}
