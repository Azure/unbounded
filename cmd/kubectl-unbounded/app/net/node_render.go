// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package net

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"golang.org/x/term"

	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

// clusterStatusResponse is the controller /status/json response used by node list.
type clusterStatusResponse struct {
	PullEnabled  bool                                `json:"pullEnabled"`
	LeaderInfo   *leaderInfo                         `json:"leaderInfo,omitempty"`
	Nodes        []statusv1alpha1.NodeStatusResponse `json:"nodes"`
	Sites        []siteStatus                        `json:"sites"`
	GatewayPools []gatewayPoolStatus                 `json:"gatewayPools"`
	Warnings     []string                            `json:"warnings,omitempty"`
}

// leaderInfo contains the controller leader pod identity.
type leaderInfo struct {
	PodName string `json:"podName"`
}

// siteStatus captures site-level status fields needed by node list.
type siteStatus struct {
	Name            string `json:"name"`
	ManageCniPlugin bool   `json:"manageCniPlugin"`
}

// gatewayPoolStatus captures gateway pool fields needed by node list.
type gatewayPoolStatus struct {
	Name     string   `json:"name"`
	Gateways []string `json:"gateways"`
}

// nodeListRow is one rendered row for node list table output.
type nodeListRow struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Site        string `json:"site"`
	Pool        string `json:"pool"`
	InternalIPs string `json:"internalIPs,omitempty"`
	ExternalIPs string `json:"externalIPs,omitempty"`
	PodCIDRs    string `json:"podCIDRs,omitempty"`
	Peers       string `json:"peers"`
	Gateways    string `json:"gateways"`
	K8sStatus   string `json:"k8sStatus"`
	WGStatus    string `json:"wgStatus"`
	CNI         string `json:"cni"`
	LastUpdate  string `json:"lastUpdate"`
	StatusTone  string `json:"-"`
}

// printNodeInfoPane prints fields similar to the UI node information panel.
func printNodeInfoPane(w io.Writer, status clusterStatusResponse, node statusv1alpha1.NodeStatusResponse, useColor bool) error {
	gatewayByNode := map[string]string{}

	for _, pool := range status.GatewayPools {
		for _, gn := range pool.Gateways {
			gatewayByNode[gn] = pool.Name
		}
	}

	nodeLabels := node.NodeInfo.K8sLabels

	instanceType := nodeLabels["node.kubernetes.io/instance-type"]
	if instanceType == "" {
		instanceType = "-"
	}

	nodeImage := nodeLabels["kubernetes.azure.com/node-image-version"]
	if nodeImage == "" {
		nodeImage = valueOr(node.NodeInfo.OSImage, "-")
	}

	zone := nodeLabels["topology.kubernetes.io/zone"]
	if zone == "" {
		zone = "-"
	}

	region := nodeLabels["topology.kubernetes.io/region"]
	if region == "" {
		region = "-"
	}

	nodeType := "Worker"
	if node.NodeInfo.IsGateway || gatewayByNode[node.NodeInfo.Name] != "" {
		nodeType = "Gateway"
	}

	var buildStr string

	if node.NodeInfo.BuildInfo != nil {
		parts := []string{}
		if node.NodeInfo.BuildInfo.Version != "" {
			parts = append(parts, "Version: "+node.NodeInfo.BuildInfo.Version)
		}

		if node.NodeInfo.BuildInfo.Commit != "" {
			parts = append(parts, "Commit: "+node.NodeInfo.BuildInfo.Commit)
		}

		if node.NodeInfo.BuildInfo.BuildTime != "" {
			parts = append(parts, "Build Time: "+node.NodeInfo.BuildInfo.BuildTime)
		}

		buildStr = strings.Join(parts, " | ")
	}

	if buildStr == "" {
		buildStr = "-"
	}

	k8sReady := valueOr(node.NodeInfo.K8sReady, "NotReady")
	wgStatus := cniStatusLabel(node, status.PullEnabled)

	if useColor {
		k8sReady = colorize(k8sReady, k8sTone(k8sReady))
		wgStatus = colorize(wgStatus, statusTone(node, status.PullEnabled))
	}

	rows := [][2]string{
		{"Name", node.NodeInfo.Name},
		{"Type", nodeType},
		{"Site", valueOr(node.NodeInfo.SiteName, "-")},
		{"Pool", valueOr(gatewayByNode[node.NodeInfo.Name], "-")},
		{"K8s Status", k8sReady},
		{"UN Status", wgStatus},
		{"WireGuard Public Key", func() string {
			if node.NodeInfo.WireGuard != nil {
				return valueOr(node.NodeInfo.WireGuard.PublicKey, "-")
			}

			return "-"
		}()},
		{"Node Agent Build", buildStr},
		{"Internal IPs", joinOrDash(node.NodeInfo.InternalIPs)},
		{"External IPs", joinOrDash(node.NodeInfo.ExternalIPs)},
		{"Pod CIDRs", joinOrDash(node.NodeInfo.PodCIDRs)},
		{"Pod CIDR Gateways", joinOrDash(firstUsableIPs(node.NodeInfo.PodCIDRs))},
		{"Node Image", nodeImage},
		{"Kubelet Version", valueOr(node.NodeInfo.Kubelet, "-")},
		{"Kernel", valueOr(node.NodeInfo.Kernel, "-")},
		{"Instance Type", instanceType},
		{"Region", region},
		{"Availability Zone", zone},
		{"K8s Node Updated", formatAgePtr(node.NodeInfo.K8sUpdatedAt)},
		{"Status Push Updated", formatAgePtr(node.LastPushTime)},
	}
	if err := printKVRows(w, rows); err != nil {
		return err
	}

	if len(node.NodeErrors) > 0 {
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}

		if _, err := fmt.Fprintln(w, "  Node Errors:"); err != nil {
			return err
		}

		for _, ne := range node.NodeErrors {
			msg := strings.TrimSpace(ne.Message)
			if msg == "" {
				continue
			}

			line := fmt.Sprintf("    - [%s] %s", ne.Type, msg)
			if useColor {
				line = colorize(line, "red")
			}

			if _, err := fmt.Fprintln(w, line); err != nil {
				return err
			}
		}
	}

	return nil
}

// printNodePeerings prints a peering table and optional details for one peer.
func printNodePeerings(w io.Writer, status clusterStatusResponse, node statusv1alpha1.NodeStatusResponse, peerName string, useColor bool) error {
	nodeByName := map[string]statusv1alpha1.NodeStatusResponse{}
	gatewayByNode := map[string]string{}

	for _, n := range status.Nodes {
		nodeByName[n.NodeInfo.Name] = n
	}

	for _, pool := range status.GatewayPools {
		for _, gn := range pool.Gateways {
			gatewayByNode[gn] = pool.Name
		}
	}

	headers := []string{"DESTINATION NODE", "TYPE", "SITE", "POOL", "PROTOCOL", "K8S", "HEALTH", "ENDPOINT", "ALLOWED IPS", "ROUTES"}
	rows := [][]string{}

	var selected *statusv1alpha1.WireGuardPeerStatus

	for i := range node.Peers {
		peer := node.Peers[i]
		if peerName != "" && !strings.EqualFold(peer.Name, peerName) {
			continue
		}

		dest := valueOr(peer.Name, "-")

		peerType := "Site"
		if strings.EqualFold(peer.PeerType, "gateway") || gatewayByNode[peer.Name] != "" {
			peerType = "Gateway"
		}

		destNode, hasDest := nodeByName[peer.Name]
		site := "-"
		k8s := "Unknown"

		if hasDest {
			site = valueOr(destNode.NodeInfo.SiteName, "-")
			k8s = valueOr(destNode.NodeInfo.K8sReady, "NotReady")
		}

		pool := "-"
		if peerType == "Gateway" {
			pool = valueOr(gatewayByNode[peer.Name], "-")
		}

		hc := "Unknown"
		if peer.HealthCheck != nil && peer.HealthCheck.Status != "" {
			hc = peer.HealthCheck.Status
		} else if peer.HealthCheck != nil && peer.HealthCheck.Enabled {
			hc = "Down"
		}

		k8sOut, hcOut := k8s, hc
		if useColor {
			k8sOut = colorize(k8sOut, k8sTone(k8sOut))

			hcTone := "yellow"
			if strings.EqualFold(hc, "up") {
				hcTone = "green"
			} else if strings.EqualFold(hc, "down") {
				hcTone = "red"
			}

			hcOut = colorize(hcOut, hcTone)
		}

		protocol := valueOr(peer.Tunnel.Protocol, "WireGuard")

		rows = append(rows, []string{
			dest, peerType, site, pool, protocol, k8sOut, hcOut,
			valueOr(peer.Tunnel.Endpoint, "-"),
			fmt.Sprintf("%d", len(peer.Tunnel.AllowedIPs)),
			fmt.Sprintf("%d", len(peer.RouteDestinations)),
		})
		if peerName != "" {
			selected = &peer
		}
	}

	if len(rows) == 0 {
		if peerName != "" {
			return fmt.Errorf("peering for node %q to peer %q not found", node.NodeInfo.Name, peerName)
		}

		_, err := fmt.Fprintln(w, "No peerings.")

		return err
	}

	if err := printSimpleTable(w, headers, rows); err != nil {
		return err
	}

	if selected == nil {
		return nil
	}

	_, _ = fmt.Fprintln(w, "") //nolint:errcheck
	hcStatus := "Disabled"
	hcUptime := "-"
	hcRTT := "-"

	if selected.HealthCheck != nil && selected.HealthCheck.Enabled {
		hcStatus = valueOr(selected.HealthCheck.Status, "Unknown")
		hcUptime = valueOr(selected.HealthCheck.Uptime, "-")
		hcRTT = valueOr(selected.HealthCheck.RTT, "-")
	}

	detailRows := [][2]string{
		{"Destination", valueOr(selected.Name, "-")},
		{"Tunnel Protocol", valueOr(selected.Tunnel.Protocol, "WireGuard")},
		{"Interface", valueOr(selected.Tunnel.Interface, "-")},
		{"Endpoint", valueOr(selected.Tunnel.Endpoint, "-")},
		{"Pod CIDR Gateways", joinOrDash(selected.PodCIDRGateways)},
		{"Health Check Status", hcStatus},
		{"Health Check Uptime", hcUptime},
		{"Health Check RTT", hcRTT},
		{"RX Bytes", fmt.Sprintf("%d", selected.Tunnel.RxBytes)},
		{"TX Bytes", fmt.Sprintf("%d", selected.Tunnel.TxBytes)},
		{"Latest Handshake", formatAge(selected.Tunnel.LastHandshake)},
		{"Allowed IPs", joinOrDash(selected.Tunnel.AllowedIPs)},
		{"Route Destinations", joinOrDash(selected.RouteDestinations)},
	}

	return printKVRows(w, detailRows)
}

// printNodeRoutes prints a route table for one node.
func printNodeRoutes(w io.Writer, node statusv1alpha1.NodeStatusResponse, useColor bool) error {
	headers := []string{"FAMILY", "DESTINATION", "KIND", "DEVICE", "GATEWAY", "DIST", "MTU", "EXPECTED", "PRESENT", "INFO"}
	rows := [][]string{}

	for _, route := range node.RoutingTable.Routes {
		for _, hop := range route.NextHops {
			kind := routeKind(route.Destination, hop)
			expected := boolPtrValue(hop.Expected)
			present := boolPtrValue(hop.Present)
			expText := fmt.Sprintf("%t", expected)
			preText := fmt.Sprintf("%t", present)

			if !isWireguardKind(kind) {
				expText = "n/a"
				preText = "n/a"
			}

			mtuText := "-"
			if hop.MTU > 0 {
				mtuText = fmt.Sprintf("%d", hop.MTU)
			}

			infoParts := []string{}

			if hop.Info != nil {
				if v := strings.TrimSpace(hop.Info.RouteType); v != "" {
					infoParts = append(infoParts, v)
				}

				if v := strings.TrimSpace(hop.Info.ObjectType); v != "" {
					infoParts = append(infoParts, v)
				}

				if v := strings.TrimSpace(hop.Info.ObjectName); v != "" {
					infoParts = append(infoParts, v)
				}
			}

			info := strings.TrimSpace(strings.Join(infoParts, " "))
			if info == "" {
				info = "-"
			}

			if useColor && isWireguardKind(kind) {
				if expected == present {
					expText = colorize(expText, "green")
					preText = colorize(preText, "green")
				} else {
					expText = colorize(expText, "yellow")
					preText = colorize(preText, "yellow")
				}
			}

			rows = append(rows, []string{
				route.Family,
				route.Destination,
				kind,
				valueOr(hop.Device, "-"),
				valueOr(hop.Gateway, "-"),
				fmt.Sprintf("%d", hop.Distance),
				mtuText,
				expText,
				preText,
				info,
			})
		}
	}

	if len(rows) == 0 {
		_, err := fmt.Fprintln(w, "No routes.")
		return err
	}

	return printSimpleTable(w, headers, rows)
}

// printNodeBpf prints a table of BPF trie entries for one node.
func printNodeBpf(w io.Writer, node statusv1alpha1.NodeStatusResponse) error {
	headers := []string{"CIDR", "REMOTE", "NODE", "IFACE", "PROTO", "VNI", "MTU"}
	rows := [][]string{}

	for _, e := range node.BpfEntries {
		mtuText := "-"
		if e.MTU > 0 {
			mtuText = fmt.Sprintf("%d", e.MTU)
		}

		rows = append(rows, []string{
			e.CIDR,
			valueOr(e.Remote, "-"),
			valueOr(e.Node, "-"),
			valueOr(e.Interface, "-"),
			valueOr(e.Protocol, "-"),
			fmt.Sprintf("%d", e.VNI),
			mtuText,
		})
	}

	if len(rows) == 0 {
		_, err := fmt.Fprintln(w, "No BPF entries.")
		return err
	}

	return printSimpleTable(w, headers, rows)
}

// printSimpleTable prints fixed-width tabular output with two-space separators.
func printSimpleTable(w io.Writer, headers []string, rows [][]string) error {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}

	for _, row := range rows {
		for i, col := range row {
			widths[i] = maxInt(widths[i], visibleLen(col))
		}
	}

	if _, err := fmt.Fprintln(w, joinColumns(headers, widths)); err != nil {
		return err
	}

	for _, row := range rows {
		if _, err := fmt.Fprintln(w, joinColumns(row, widths)); err != nil {
			return err
		}
	}

	return nil
}

// printKVRows prints key/value rows.
func printKVRows(w io.Writer, rows [][2]string) error {
	maxKey := 0
	for _, row := range rows {
		maxKey = maxInt(maxKey, len(row[0]))
	}

	for _, row := range rows {
		if _, err := fmt.Fprintf(w, "%-*s  %s\n", maxKey, row[0], row[1]); err != nil {
			return err
		}
	}

	return nil
}

// buildNodeRows builds rows for node list output using frontend-like semantics.
func buildNodeRows(status clusterStatusResponse) []nodeListRow {
	gatewayByNode := map[string]string{}

	for _, pool := range status.GatewayPools {
		for _, nodeName := range pool.Gateways {
			gatewayByNode[nodeName] = pool.Name
		}
	}

	siteManagedCNI := map[string]bool{}
	for _, site := range status.Sites {
		siteManagedCNI[site.Name] = site.ManageCniPlugin
	}

	rows := make([]nodeListRow, 0, len(status.Nodes))
	for _, node := range status.Nodes {
		nodeName := node.NodeInfo.Name

		nodeType := "Worker"
		if node.NodeInfo.IsGateway || gatewayByNode[nodeName] != "" {
			nodeType = "Gateway"
		}

		site := node.NodeInfo.SiteName
		if site == "" {
			site = "-"
		}

		pool := ""
		if p := gatewayByNode[nodeName]; p != "" {
			pool = p
		}

		peerOnline, peerTotal := countPeers(node.Peers, func(p statusv1alpha1.WireGuardPeerStatus) bool {
			return p.PeerType != "gateway"
		})
		gwOnline, gwTotal := countPeers(node.Peers, func(p statusv1alpha1.WireGuardPeerStatus) bool {
			return p.PeerType == "gateway"
		})

		k8sStatus := node.NodeInfo.K8sReady
		if k8sStatus == "" {
			k8sStatus = "NotReady"
		}

		cniStatus := cniStatusLabel(node, status.PullEnabled)
		cniCol := "-"

		if site != "-" {
			if managed, ok := siteManagedCNI[site]; ok && managed {
				cniCol = "yes"
			}
		}

		lastUpdate := "Never"
		if node.StatusSource == "ws" || node.StatusSource == "apiserver-ws" {
			lastUpdate = "Live"
		} else if node.LastPushTime != nil {
			lastUpdate = formatAge(*node.LastPushTime)
		}

		rows = append(rows, nodeListRow{
			Name:        nodeName,
			Type:        nodeType,
			Site:        site,
			Pool:        pool,
			InternalIPs: joinOrDash(node.NodeInfo.InternalIPs),
			ExternalIPs: joinOrDash(node.NodeInfo.ExternalIPs),
			PodCIDRs:    joinOrDash(node.NodeInfo.PodCIDRs),
			Peers:       fmt.Sprintf("%d/%d", peerOnline, peerTotal),
			Gateways:    fmt.Sprintf("%d/%d", gwOnline, gwTotal),
			K8sStatus:   k8sStatus,
			WGStatus:    cniStatus,
			CNI:         cniCol,
			LastUpdate:  lastUpdate,
			StatusTone:  statusTone(node, status.PullEnabled),
		})
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })

	return rows
}

// printNodeRowsTable emits node list rows as a terminal-friendly table.
func printNodeRowsTable(w io.Writer, rows []nodeListRow, useColor, wide bool) error {
	headers := []string{"NAME", "TYPE", "SITE", "POOL", "PEERS", "GATEWAYS", "K8S STATUS", "UN STATUS", "CNI", "LAST UPDATE"}
	if wide {
		headers = []string{"NAME", "TYPE", "SITE", "POOL", "INTERNAL IPS", "EXTERNAL IPS", "POD CIDRS", "PEERS", "GATEWAYS", "K8S STATUS", "UN STATUS", "CNI", "LAST UPDATE"}
	}

	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}

	for _, row := range rows {
		cols := rowColumns(row, wide, useColor)
		for i, col := range cols {
			widths[i] = maxInt(widths[i], visibleLen(col))
		}
	}

	if _, err := fmt.Fprintln(w, joinColumns(headers, widths)); err != nil {
		return err
	}

	for _, row := range rows {
		cols := rowColumns(row, wide, useColor)
		if _, err := fmt.Fprintln(w, joinColumns(cols, widths)); err != nil {
			return err
		}
	}

	return nil
}

// rowColumns returns table columns for one node row.
func rowColumns(row nodeListRow, wide, useColor bool) []string {
	peerValue := row.Peers
	gatewayValue := row.Gateways
	k8sValue := row.K8sStatus

	wgValue := row.WGStatus
	if useColor {
		peerValue = colorize(peerValue, countTone(row.Peers))
		gatewayValue = colorize(gatewayValue, countTone(row.Gateways))
		k8sValue = colorize(k8sValue, k8sTone(row.K8sStatus))
		wgValue = colorize(wgValue, row.StatusTone)
	}

	if wide {
		return []string{
			row.Name, row.Type, row.Site, row.Pool, row.InternalIPs, row.ExternalIPs, row.PodCIDRs, peerValue, gatewayValue, k8sValue, wgValue, row.CNI, row.LastUpdate,
		}
	}

	return []string{
		row.Name, row.Type, row.Site, row.Pool, peerValue, gatewayValue, k8sValue, wgValue, row.CNI, row.LastUpdate,
	}
}

// shouldUseColor resolves color mode and TTY behavior.
func shouldUseColor(out io.Writer, mode string) bool {
	switch strings.ToLower(mode) {
	case "always":
		return true
	case "never":
		return false
	case "auto", "":
		if file, ok := out.(*os.File); ok {
			return term.IsTerminal(int(file.Fd()))
		}

		return false
	default:
		return false
	}
}

// countPeers returns online and total counts for peers matching a predicate.
func countPeers(peers []statusv1alpha1.WireGuardPeerStatus, include func(statusv1alpha1.WireGuardPeerStatus) bool) (int, int) {
	total := 0
	online := 0

	for _, p := range peers {
		if !include(p) {
			continue
		}

		total++

		if peerOnline(p) {
			online++
		}
	}

	return online, total
}

// peerOnline evaluates frontend-equivalent online status for one peer.
func peerOnline(p statusv1alpha1.WireGuardPeerStatus) bool {
	hcEnabled := (p.HealthCheck != nil && p.HealthCheck.Enabled) || (p.HealthCheck != nil && strings.TrimSpace(strings.ToLower(p.HealthCheck.Status)) != "")
	if hcEnabled {
		return p.HealthCheck != nil && strings.EqualFold(strings.TrimSpace(p.HealthCheck.Status), "up")
	}

	hs := p.Tunnel.LastHandshake
	if hs.IsZero() || hs.Unix() <= 0 {
		return false
	}

	return time.Since(hs) < 3*time.Minute
}

// cniStatusLabel evaluates UN status label similar to frontend logic.
func cniStatusLabel(node statusv1alpha1.NodeStatusResponse, pullEnabled bool) string {
	if len(node.NodeErrors) > 0 {
		return "Errors"
	}

	if routeMismatchCount(node) > 0 {
		return "Route mismatch"
	}

	switch node.StatusSource {
	case "apiserver-push", "apiserver-ws":
		return "Fallback"
	case "push", "ws":
		return "Healthy"
	}

	if pullEnabled {
		if node.FetchError != "" || node.StatusSource == "stale-cache" || node.StatusSource == "error" || node.LastPushTime == nil {
			return "Pull failed"
		}

		if node.StatusSource == "pull" {
			return "Pulling"
		}

		return "Pull failed"
	}

	if node.StatusSource == "stale-cache" {
		return "Stale"
	}

	if node.StatusSource == "error" {
		return "No data"
	}

	return "Stale"
}

// statusTone maps node status to ui-like tone category.
func statusTone(node statusv1alpha1.NodeStatusResponse, pullEnabled bool) string {
	if routeMismatchCount(node) > 0 {
		return "yellow"
	}

	switch cniStatusLabel(node, pullEnabled) {
	case "Healthy":
		return "green"
	case "Errors":
		return "red"
	case "Fallback", "Pulling", "Stale":
		return "yellow"
	case "Pull failed", "No data":
		return "red"
	default:
		return "yellow"
	}
}

// k8sTone maps Kubernetes Ready status to color categories.
func k8sTone(status string) string {
	switch strings.ToLower(status) {
	case "ready":
		return "green"
	case "notready", "missing":
		return "red"
	default:
		return "yellow"
	}
}

// countTone maps online/total text (for example 1/2) to tone categories.
func countTone(v string) string {
	parts := strings.SplitN(v, "/", 2)
	if len(parts) != 2 {
		return "yellow"
	}

	var online, total int

	_, errA := fmt.Sscanf(parts[0], "%d", &online)

	_, errB := fmt.Sscanf(parts[1], "%d", &total)
	if errA != nil || errB != nil {
		return "yellow"
	}

	if total == 0 && online == 0 {
		return "dim"
	}

	if online == 0 {
		return "red"
	}

	if online < total {
		return "yellow"
	}

	return "green"
}

// routeMismatchCount returns the count of expected/present mismatches.
func routeMismatchCount(node statusv1alpha1.NodeStatusResponse) int {
	mismatch := 0

	for _, route := range node.RoutingTable.Routes {
		for _, hop := range route.NextHops {
			if boolPtrValue(hop.Expected) != boolPtrValue(hop.Present) {
				mismatch++
			}
		}
	}

	return mismatch
}

// boolPtrValue returns false for nil pointers.
func boolPtrValue(v *bool) bool {
	if v == nil {
		return false
	}

	return *v
}

// collectWarnings aggregates controller warnings and per-node errors, matching
// the frontend logic in App.tsx.
func collectWarnings(status clusterStatusResponse) []string {
	var items []string

	for _, msg := range status.Warnings {
		msg = strings.TrimSpace(msg)
		if msg != "" {
			items = append(items, "controller: "+msg)
		}
	}

	for _, node := range status.Nodes {
		name := node.NodeInfo.Name
		if name == "" {
			name = "unknown"
		}

		for _, ne := range node.NodeErrors {
			msg := strings.TrimSpace(ne.Message)
			if msg != "" {
				items = append(items, "node "+name+": "+msg)
			}
		}
	}

	sort.Strings(items)

	return items
}

// printWarnings prints warning lines above the table output.
func printWarnings(w io.Writer, warnings []string, useColor bool) {
	for _, msg := range warnings {
		prefix := "WARNING: "
		if useColor {
			// Yellow text for the entire warning line
			_, _ = fmt.Fprintf(w, "\033[33m%s%s\033[0m\n", prefix, msg) //nolint:errcheck
		} else {
			_, _ = fmt.Fprintf(w, "%s%s\n", prefix, msg) //nolint:errcheck
		}
	}

	if len(warnings) > 0 {
		_, _ = fmt.Fprintln(w) //nolint:errcheck
	}
}

// buildNodeRowsFromSummary builds rows for node list output from a cluster
// summary. Because the summary omits per-peer type breakdown, the GATEWAYS
// column shows "-" and PEERS shows the combined healthy/total count. The
// LastUpdate column shows the status source label instead of a precise age.
func buildNodeRowsFromSummary(summary clusterSummary) []nodeListRow {
	gatewayByNode := map[string]string{}

	for _, pool := range summary.GatewayPools {
		for _, nodeName := range pool.Gateways {
			gatewayByNode[nodeName] = pool.Name
		}
	}

	siteManagedCNI := map[string]bool{}
	for _, site := range summary.Sites {
		siteManagedCNI[site.Name] = site.ManageCniPlugin
	}

	rows := make([]nodeListRow, 0, len(summary.NodeSummaries))
	for _, ns := range summary.NodeSummaries {
		nodeType := "Worker"
		if ns.IsGateway || gatewayByNode[ns.Name] != "" {
			nodeType = "Gateway"
		}

		site := ns.SiteName
		if site == "" {
			site = "-"
		}

		pool := gatewayByNode[ns.Name]

		k8s := ns.K8sReady
		if k8s == "" {
			k8s = "NotReady"
		}

		cniStatus := ns.CniStatus
		if cniStatus == "" {
			cniStatus = "Unknown"
		}

		cniCol := "-"

		if site != "-" {
			if managed, ok := siteManagedCNI[site]; ok && managed {
				cniCol = "yes"
			}
		}

		var lastUpdate string

		switch ns.StatusSource {
		case "ws", "apiserver-ws":
			lastUpdate = "Live"
		case "push":
			lastUpdate = "Push"
		case "apiserver-push":
			lastUpdate = "Fallback"
		case "pull":
			lastUpdate = "Pull"
		case "stale-cache":
			lastUpdate = "Stale"
		case "":
			lastUpdate = "Never"
		default:
			lastUpdate = ns.StatusSource
		}

		tone := ns.CniTone
		if tone == "" {
			tone = "yellow"
		}

		rows = append(rows, nodeListRow{
			Name:       ns.Name,
			Type:       nodeType,
			Site:       site,
			Pool:       pool,
			Peers:      fmt.Sprintf("%d/%d", ns.HealthyPeers, ns.PeerCount),
			Gateways:   "-",
			K8sStatus:  k8s,
			WGStatus:   cniStatus,
			CNI:        cniCol,
			LastUpdate: lastUpdate,
			StatusTone: tone,
		})
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })

	return rows
}

// collectWarningsFromSummary aggregates controller warnings and per-node error
// counts from a cluster summary. Because the summary omits individual error
// messages, nodes with errors are shown with their error count.
func collectWarningsFromSummary(summary clusterSummary) []string {
	var items []string

	for _, msg := range summary.Warnings {
		msg = strings.TrimSpace(msg)
		if msg != "" {
			items = append(items, "controller: "+msg)
		}
	}

	for _, ns := range summary.NodeSummaries {
		if ns.ErrorCount == 1 && ns.FirstError != "" {
			items = append(items, fmt.Sprintf("node %s: %s", ns.Name, ns.FirstError))
		} else if ns.ErrorCount > 0 {
			items = append(items, fmt.Sprintf("node %s: %d error(s)", ns.Name, ns.ErrorCount))
		}
	}

	sort.Strings(items)

	return items
}
