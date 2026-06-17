// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Azure/unbounded/internal/dashboard/contract"
)

// This file maps the controller's in-process ClusterStatusResponse onto the
// generic dashboard contract types. It is pure (no I/O), so it is unit-testable
// without a cluster and is the heart of the "port the net UI onto the new
// dashboard" prototype.

// netManifest is the static manifest describing the net dashboard module.
func netManifest() contract.Manifest {
	return contract.Manifest{
		ID:          "net",
		Title:       "Networking",
		Description: "Sites, gateway pools, routes, tunnels, and CNI health",
		Capabilities: []contract.Capability{
			contract.CapabilitySummary,
			contract.CapabilityOverview,
			contract.CapabilityResources,
			contract.CapabilityDetails,
			contract.CapabilityActions,
			contract.CapabilityStream,
			contract.CapabilityGraph,
			contract.CapabilityMatrix,
		},
		RequiredPermissions: []contract.Permission{
			{APIGroup: "status.net.unbounded-cloud.io", Resource: "status", Verb: "get", Name: "dashboard"},
		},
		ResourceKinds: []contract.ResourceKind{
			{Kind: "sites", Title: "Sites", Singular: "Site"},
			{Kind: "nodes", Title: "Nodes", Singular: "Node"},
			{Kind: "gatewaypools", Title: "Gateway Pools", Singular: "Gateway Pool"},
			{Kind: "peerings", Title: "Peerings", Singular: "Peering"},
		},
	}
}

// netHealthForCounts derives an overall health from problem/error/offline data.
func netSummary(status *ClusterStatusResponse) contract.Summary {
	healthyNodes, totalNodes := nodeHealthCounts(status)
	healthyPeers, totalPeers := peeringHealthCounts(status)

	health := contract.HealthOK
	message := "All sites and nodes healthy."

	switch {
	case len(status.Errors) > 0 || len(status.Problems) > 0:
		health = contract.HealthError
		message = fmt.Sprintf("%d problem(s) reported.", len(status.Problems)+len(status.Errors))
	case len(status.Warnings) > 0 || healthyNodes < totalNodes:
		health = contract.HealthWarning
		message = fmt.Sprintf("%d of %d nodes healthy.", healthyNodes, totalNodes)
	}

	return contract.Summary{
		Health:  health,
		Message: message,
		Metrics: []contract.Metric{
			{Label: "Sites", Value: itoa(status.SiteCount)},
			{Label: "Gateway Pools", Value: itoa(len(status.GatewayPools))},
			{Label: "Nodes", Value: fmt.Sprintf("%d/%d", healthyNodes, totalNodes), Health: ratioHealth(healthyNodes, totalNodes)},
			{Label: "Peerings", Value: fmt.Sprintf("%d/%d", healthyPeers, totalPeers), Health: ratioHealth(healthyPeers, totalPeers)},
		},
		Alerts: netAlerts(status),
	}
}

// netAlerts flattens controller problems, errors, warnings, and per-node errors
// into the contract alert list.
func netAlerts(status *ClusterStatusResponse) []contract.Alert {
	var alerts []contract.Alert

	for _, p := range status.Problems {
		alerts = append(alerts, contract.Alert{
			Health: contract.HealthError,
			Title:  fmt.Sprintf("%s %s", p.Type, p.Name),
			Detail: strings.Join(p.Errors, "; "),
			Source: "net",
		})
	}

	for _, e := range status.Errors {
		alerts = append(alerts, contract.Alert{Health: contract.HealthError, Title: e, Source: "net"})
	}

	for _, wmsg := range status.Warnings {
		alerts = append(alerts, contract.Alert{Health: contract.HealthWarning, Title: wmsg, Source: "net"})
	}

	for _, node := range status.Nodes {
		for _, ne := range node.NodeErrors {
			alerts = append(alerts, contract.Alert{
				Health: contract.HealthWarning,
				Title:  fmt.Sprintf("node %s: %s", node.NodeInfo.Name, ne.Message),
				Detail: ne.Type,
				Source: "net",
			})
		}
	}

	return alerts
}

// netOverview composes the net landing page: metrics, topology graph,
// connectivity heatmap, alerts, and the nodes table.
func netOverview(status *ClusterStatusResponse) contract.Overview {
	sum := netSummary(status)
	matrix := netMatrix(status)

	panels := []contract.Panel{
		{Type: contract.PanelMetrics, Title: "Overview", StreamKey: "summary", Metrics: sum.Metrics},
		{Type: contract.PanelGraph, Title: "Topology", Width: 7, StreamKey: "graph"},
	}

	if len(matrix.Rows) > 0 {
		panels = append(panels, contract.Panel{Type: contract.PanelMatrix, Title: "Connectivity", Width: 5, StreamKey: "matrix", Matrix: &matrix})
	}

	if len(sum.Alerts) > 0 {
		panels = append(panels, contract.Panel{Type: contract.PanelAlerts, Title: "Alerts", StreamKey: "summary", Alerts: sum.Alerts})
	}

	panels = append(panels, contract.Panel{Type: contract.PanelTable, Title: "Nodes", StreamKey: "nodes", Table: netNodesList(status)})

	return contract.Overview{Title: "Networking", Panels: panels}
}

// netGraph builds a topology of sites, gateway pools, and nodes connected by
// site membership and peering relationships.
func netGraph(status *ClusterStatusResponse) contract.Graph {
	var g contract.Graph

	seen := make(map[string]bool)

	addNode := func(id, label, kind, group string, health contract.Health) {
		if seen[id] {
			return
		}

		seen[id] = true

		g.Nodes = append(g.Nodes, contract.GraphNode{ID: id, Label: label, Kind: kind, Group: group, Health: health})
	}

	for _, s := range status.Sites {
		h := contract.HealthOK
		if s.OfflineCount > 0 {
			h = contract.HealthWarning
		}

		addNode("site:"+s.Name, s.Name, "site", s.Name, h)
	}

	for _, gp := range status.GatewayPools {
		addNode("pool:"+gp.Name, gp.Name, "gatewaypool", gp.SiteName, contract.HealthOK)

		if gp.SiteName != "" {
			addNode("site:"+gp.SiteName, gp.SiteName, "site", gp.SiteName, contract.HealthOK)
			g.Edges = append(g.Edges, contract.GraphEdge{Source: "site:" + gp.SiteName, Target: "pool:" + gp.Name, Kind: "membership"})
		}
	}

	// Peerings connect sites to each other.
	for _, p := range status.Peerings {
		for i := 0; i < len(p.Sites); i++ {
			for j := i + 1; j < len(p.Sites); j++ {
				h := contract.HealthOK
				if !p.HealthCheckEnabled {
					h = contract.HealthUnknown
				}

				g.Edges = append(g.Edges, contract.GraphEdge{
					Source: "site:" + p.Sites[i],
					Target: "site:" + p.Sites[j],
					Kind:   "peering",
					Health: h,
					Label:  p.Name,
				})
			}
		}
	}

	return g
}

// netMatrix maps the controller's site connectivity matrix onto the contract
// matrix (heatmap).
func netMatrix(status *ClusterStatusResponse) contract.Matrix {
	if len(status.ConnectivityMatrix) == 0 {
		return contract.Matrix{}
	}

	sites := make([]string, 0, len(status.ConnectivityMatrix))
	for src := range status.ConnectivityMatrix {
		sites = append(sites, src)
	}

	sort.Strings(sites)

	cells := make(map[string]map[string]contract.Cell, len(sites))

	for _, src := range sites {
		row := make(map[string]contract.Cell, len(sites))
		sm := status.ConnectivityMatrix[src]

		for _, dst := range sites {
			cell := contract.Cell{Value: "-", Health: contract.HealthUnknown}

			if src == dst {
				row[dst] = cell
				continue
			}

			if sm != nil && sm.Results != nil {
				if dstResults, ok := sm.Results[src]; ok {
					if st, ok := dstResults[dst]; ok {
						cell = matrixCell(st)
					}
				}
			}

			row[dst] = cell
		}

		cells[src] = row
	}

	return contract.Matrix{Rows: sites, Columns: sites, Cells: cells}
}

func matrixCell(status string) contract.Cell {
	switch strings.ToLower(status) {
	case "ok", "healthy", "up", "reachable":
		return contract.Cell{Value: "OK", Health: contract.HealthOK}
	case "degraded", "partial", "warning":
		return contract.Cell{Value: "!", Health: contract.HealthWarning}
	case "", "unknown":
		return contract.Cell{Value: "?", Health: contract.HealthUnknown}
	default:
		return contract.Cell{Value: "x", Health: contract.HealthError}
	}
}

// netResourceList returns the table for a given resource kind.
func netResourceList(status *ClusterStatusResponse, kind string) (*contract.ResourceList, bool) {
	switch kind {
	case "sites":
		return netSitesList(status), true
	case "nodes":
		return netNodesList(status), true
	case "gatewaypools":
		return netGatewayPoolsList(status), true
	case "peerings":
		return netPeeringsList(status), true
	default:
		return nil, false
	}
}

func netSitesList(status *ClusterStatusResponse) *contract.ResourceList {
	list := &contract.ResourceList{
		Kind:  "sites",
		Title: "Sites",
		Columns: []contract.Column{
			{Key: "name", Title: "Name"},
			{Key: "nodes", Title: "Nodes"},
			{Key: "online", Title: "Online"},
			{Key: "cni", Title: "Managed CNI"},
		},
	}

	for _, s := range status.Sites {
		h := contract.HealthOK
		if s.OfflineCount > 0 {
			h = contract.HealthWarning
		}

		list.Rows = append(list.Rows, contract.ResourceRow{
			Name:   s.Name,
			Health: h,
			Cells: map[string]string{
				"name":   s.Name,
				"nodes":  itoa(s.NodeCount),
				"online": fmt.Sprintf("%d/%d", s.OnlineCount, s.NodeCount),
				"cni":    boolText(s.ManageCniPlugin),
			},
		})
	}

	return list
}

func netNodesList(status *ClusterStatusResponse) *contract.ResourceList {
	list := &contract.ResourceList{
		Kind:  "nodes",
		Title: "Nodes",
		Columns: []contract.Column{
			{Key: "name", Title: "Name"},
			{Key: "site", Title: "Site"},
			{Key: "role", Title: "Role"},
			{Key: "k8s", Title: "K8s"},
			{Key: "peers", Title: "Peers"},
		},
	}

	for _, node := range status.Nodes {
		ni := node.NodeInfo
		healthyPeers, totalPeers := nodePeerHealth(node)

		list.Rows = append(list.Rows, contract.ResourceRow{
			Name:   ni.Name,
			Health: nodeHealth(node),
			Cells: map[string]string{
				"name":  ni.Name,
				"site":  ni.SiteName,
				"role":  roleText(ni.IsGateway),
				"k8s":   defaultText(ni.K8sReady, "Unknown"),
				"peers": fmt.Sprintf("%d/%d", healthyPeers, totalPeers),
			},
		})
	}

	return list
}

func netGatewayPoolsList(status *ClusterStatusResponse) *contract.ResourceList {
	list := &contract.ResourceList{
		Kind:  "gatewaypools",
		Title: "Gateway Pools",
		Columns: []contract.Column{
			{Key: "name", Title: "Name"},
			{Key: "site", Title: "Site"},
			{Key: "gateways", Title: "Gateways"},
			{Key: "connected", Title: "Connected Sites"},
		},
	}

	for _, gp := range status.GatewayPools {
		list.Rows = append(list.Rows, contract.ResourceRow{
			Name:   gp.Name,
			Health: contract.HealthOK,
			Cells: map[string]string{
				"name":      gp.Name,
				"site":      gp.SiteName,
				"gateways":  itoa(len(gp.Gateways)),
				"connected": itoa(len(gp.ConnectedSites)),
			},
		})
	}

	return list
}

func netPeeringsList(status *ClusterStatusResponse) *contract.ResourceList {
	list := &contract.ResourceList{
		Kind:  "peerings",
		Title: "Peerings",
		Columns: []contract.Column{
			{Key: "name", Title: "Name"},
			{Key: "sites", Title: "Sites"},
			{Key: "hc", Title: "Health Check"},
		},
	}

	for _, p := range status.Peerings {
		h := contract.HealthUnknown
		if p.HealthCheckEnabled {
			h = contract.HealthOK
		}

		list.Rows = append(list.Rows, contract.ResourceRow{
			Name:   p.Name,
			Health: h,
			Cells: map[string]string{
				"name":  p.Name,
				"sites": strings.Join(p.Sites, ", "),
				"hc":    boolText(p.HealthCheckEnabled),
			},
		})
	}

	return list
}

// netResourceDetail returns the detail view for one resource.
func netResourceDetail(status *ClusterStatusResponse, kind, name string) (*contract.ResourceDetail, bool) {
	switch kind {
	case "sites":
		return netSiteDetail(status, name)
	case "nodes":
		return netNodeDetail(status, name)
	default:
		return nil, false
	}
}

func netSiteDetail(status *ClusterStatusResponse, name string) (*contract.ResourceDetail, bool) {
	for _, s := range status.Sites {
		if s.Name != name {
			continue
		}

		h := contract.HealthOK
		if s.OfflineCount > 0 {
			h = contract.HealthWarning
		}

		return &contract.ResourceDetail{
			Kind:   "sites",
			Name:   s.Name,
			Title:  "Site " + s.Name,
			Health: h,
			Sections: []contract.DetailSection{
				{
					Title: "Status",
					Fields: []contract.DetailField{
						{Label: "Nodes", Value: itoa(s.NodeCount)},
						{Label: "Online", Value: itoa(s.OnlineCount), Health: contract.HealthOK},
						{Label: "Offline", Value: itoa(s.OfflineCount), Health: ratioHealth(s.NodeCount-s.OfflineCount, s.NodeCount)},
						{Label: "Managed CNI", Value: boolText(s.ManageCniPlugin)},
					},
				},
				{
					Title: "CIDRs",
					Fields: []contract.DetailField{
						{Label: "Node CIDRs", Value: defaultText(strings.Join(s.NodeCidrs, ", "), "none")},
						{Label: "Pod CIDRs", Value: defaultText(strings.Join(s.PodCidrs, ", "), "none")},
					},
				},
			},
		}, true
	}

	return nil, false
}

func netNodeDetail(status *ClusterStatusResponse, name string) (*contract.ResourceDetail, bool) {
	for _, node := range status.Nodes {
		if node.NodeInfo.Name != name {
			continue
		}

		ni := node.NodeInfo
		detail := &contract.ResourceDetail{
			Kind:   "nodes",
			Name:   ni.Name,
			Title:  "Node " + ni.Name,
			Health: nodeHealth(node),
			Sections: []contract.DetailSection{
				{
					Title: "Node",
					Fields: []contract.DetailField{
						{Label: "Site", Value: ni.SiteName},
						{Label: "Role", Value: roleText(ni.IsGateway)},
						{Label: "K8s Ready", Value: defaultText(ni.K8sReady, "Unknown")},
						{Label: "Internal IPs", Value: defaultText(strings.Join(ni.InternalIPs, ", "), "none")},
						{Label: "Pod CIDRs", Value: defaultText(strings.Join(ni.PodCIDRs, ", "), "none")},
						{Label: "OS", Value: defaultText(ni.OSImage, "unknown")},
						{Label: "Kernel", Value: defaultText(ni.Kernel, "unknown")},
					},
				},
				netRoutingSection(node),
			},
		}

		detail.Sections = append(detail.Sections, netPeerSections(node)...)

		if hc := node.HealthCheck; hc != nil {
			detail.Sections = append(detail.Sections, contract.DetailSection{
				Title: "Health Check",
				Fields: []contract.DetailField{
					{Label: "Healthy", Value: boolText(hc.Healthy), Health: boolHealth(hc.Healthy)},
					{Label: "Summary", Value: hc.Summary},
					{Label: "Peers", Value: itoa(hc.PeerCount)},
				},
			})
		}

		if len(node.NodeErrors) > 0 {
			fields := make([]contract.DetailField, 0, len(node.NodeErrors))
			for _, ne := range node.NodeErrors {
				fields = append(fields, contract.DetailField{Label: ne.Type, Value: ne.Message, Health: contract.HealthError})
			}

			detail.Sections = append(detail.Sections, contract.DetailSection{Title: "Errors", Fields: fields})
		}

		return detail, true
	}

	return nil, false
}

func netRoutingSection(node *NodeStatusResponse) contract.DetailSection {
	rt := node.RoutingTable

	return contract.DetailSection{
		Title: "Routing",
		Fields: []contract.DetailField{
			{Label: "Routes", Value: itoa(len(rt.Routes))},
			{Label: "Managed Routes", Value: itoa(rt.ManagedRouteCount)},
			{Label: "Pending Routes", Value: itoa(rt.PendingRouteCount), Health: zeroHealth(rt.PendingRouteCount)},
		},
	}
}

func netPeerSections(node *NodeStatusResponse) []contract.DetailSection {
	if len(node.Peers) == 0 {
		return nil
	}

	fields := make([]contract.DetailField, 0, len(node.Peers))

	for _, p := range node.Peers {
		val := p.Tunnel.Protocol
		if p.Tunnel.Endpoint != "" {
			val = fmt.Sprintf("%s via %s", p.Tunnel.Protocol, p.Tunnel.Endpoint)
		}

		h := contract.HealthUnknown
		if p.HealthCheck != nil {
			h = peerHealthCheckHealth(p.HealthCheck.Status)
		}

		fields = append(fields, contract.DetailField{Label: p.Name, Value: defaultText(val, p.PeerType), Health: h})
	}

	return []contract.DetailSection{{Title: "WireGuard Peers", Fields: fields}}
}

// --- helpers -------------------------------------------------------------

func nodeHealthCounts(status *ClusterStatusResponse) (healthy, total int) {
	for _, node := range status.Nodes {
		total++

		if nodeHealth(node) == contract.HealthOK {
			healthy++
		}
	}

	return healthy, total
}

func nodeHealth(node *NodeStatusResponse) contract.Health {
	if len(node.NodeErrors) > 0 || node.FetchError != "" {
		return contract.HealthError
	}

	if node.NodeInfo.K8sReady != "" && !strings.EqualFold(node.NodeInfo.K8sReady, "true") && !strings.EqualFold(node.NodeInfo.K8sReady, "ready") {
		return contract.HealthWarning
	}

	if node.HealthCheck != nil && !node.HealthCheck.Healthy {
		return contract.HealthWarning
	}

	return contract.HealthOK
}

func nodePeerHealth(node *NodeStatusResponse) (healthy, total int) {
	for _, p := range node.Peers {
		total++

		if p.HealthCheck == nil || peerHealthCheckHealth(p.HealthCheck.Status) == contract.HealthOK {
			healthy++
		}
	}

	return healthy, total
}

func peeringHealthCounts(status *ClusterStatusResponse) (healthy, total int) {
	for _, p := range status.Peerings {
		total++

		if p.HealthCheckEnabled {
			healthy++
		}
	}

	return healthy, total
}

func peerHealthCheckHealth(s string) contract.Health {
	switch strings.ToLower(s) {
	case "up", "ok", "healthy":
		return contract.HealthOK
	case "down", "failed", "error":
		return contract.HealthError
	default:
		return contract.HealthUnknown
	}
}

func ratioHealth(have, total int) contract.Health {
	switch {
	case total == 0:
		return contract.HealthUnknown
	case have >= total:
		return contract.HealthOK
	case have == 0:
		return contract.HealthError
	default:
		return contract.HealthWarning
	}
}

func zeroHealth(n int) contract.Health {
	if n == 0 {
		return contract.HealthOK
	}

	return contract.HealthWarning
}

func boolHealth(ok bool) contract.Health {
	if ok {
		return contract.HealthOK
	}

	return contract.HealthError
}

func roleText(isGateway bool) string {
	if isGateway {
		return "Gateway"
	}

	return "Worker"
}

func boolText(b bool) string {
	if b {
		return "yes"
	}

	return "no"
}

func defaultText(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}

	return s
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}
