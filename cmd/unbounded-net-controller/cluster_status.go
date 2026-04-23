// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"

	"github.com/Azure/unbounded/internal/net/controller"
	"github.com/Azure/unbounded/internal/version"
)

func formatDurationAgo(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	}

	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}

	return fmt.Sprintf("%dh ago", int(d.Hours()))
}

// fetchClusterStatus collects status from all nodes in the cluster.
func fetchClusterStatus(ctx context.Context, health *healthState, pullEnabled bool) *ClusterStatusResponse {
	status := &ClusterStatusResponse{
		Timestamp:     time.Now(),
		Nodes:         []*NodeStatusResponse{},
		Sites:         []SiteStatus{},
		GatewayPools:  []GatewayPoolStatus{},
		Peerings:      []PeeringStatus{},
		Problems:      []StatusProblem{},
		PullEnabled:   pullEnabled,
		AzureTenantID: health.azureTenantID,
		BuildInfo:     &BuildInfo{Version: version.Version, Commit: version.GitCommit, BuildTime: version.BuildTime},
	}

	if leaderInfo, err := health.getLeaderInfo(ctx); err == nil {
		status.LeaderInfo = leaderInfo
	} else {
		klog.V(4).Infof("Failed to get leader info: %v", err)
	}

	tokenAuthReady, tokenAuthReason := health.tokenAuthStatus()
	if !tokenAuthReady {
		status.Errors = append(status.Errors, fmt.Sprintf("token verifier not ready: %s", tokenAuthReason))
	} else if tokenAuthReason != "ok" {
		status.Warnings = append(status.Warnings, tokenAuthReason)
	}

	if health.kubeProxyMonitor != nil {
		if w := health.kubeProxyMonitor.GetWarning(); w != "" {
			status.Warnings = append(status.Warnings, fmt.Sprintf("kube-proxy on controller node: %s", w))
		}
	}

	if health.nodeLister == nil || health.siteInformer == nil {
		status.Errors = append(status.Errors, "informers not ready")
		status.Problems = collectClusterProblems(status)

		return status
	}

	siteMap := make(map[string]*SiteStatus)
	siteNodeCIDRInputs := make(map[string][]string)

	for _, item := range health.siteInformer.GetStore().List() {
		unstr, ok := item.(*unstructured.Unstructured)
		if !ok {
			continue
		}

		siteName := unstr.GetName()
		nodeCount := 0

		if statusObj, ok := unstr.Object["status"].(map[string]interface{}); ok {
			if nc, ok := statusObj["nodeCount"].(int64); ok {
				nodeCount = int(nc)
			} else if nc, ok := statusObj["nodeCount"].(float64); ok {
				nodeCount = int(nc)
			}
		}

		var (
			nodeCidrs []string
			podCidrs  []string
		)

		manageCniPlugin := true

		if specObj, ok := unstr.Object["spec"].(map[string]interface{}); ok {
			if cidrs, ok := specObj["nodeCidrs"].([]interface{}); ok {
				for _, cidr := range cidrs {
					if cidrStr, ok := cidr.(string); ok {
						nodeCidrs = append(nodeCidrs, cidrStr)
					}
				}
			}

			if assignments, ok := specObj["podCidrAssignments"].([]interface{}); ok {
				for _, assignment := range assignments {
					assignMap, ok := assignment.(map[string]interface{})
					if !ok {
						continue
					}

					if blocks, ok := assignMap["cidrBlocks"].([]interface{}); ok {
						for _, block := range blocks {
							if cidrStr, ok := block.(string); ok {
								podCidrs = append(podCidrs, cidrStr)
							}
						}
					}
				}
			}

			if manageObj, exists := specObj["manageCniPlugin"]; exists {
				if manageValue, ok := manageObj.(bool); ok {
					manageCniPlugin = manageValue
				}
			}
		}

		siteMap[siteName] = &SiteStatus{Name: siteName, NodeCount: nodeCount, OnlineCount: 0, OfflineCount: nodeCount, NodeCidrs: nodeCidrs, PodCidrs: podCidrs, ManageCniPlugin: manageCniPlugin}
		siteNodeCIDRInputs[siteName] = append([]string(nil), nodeCidrs...)
	}

	nodeList, err := health.nodeLister.List(labels.Everything())
	if err != nil {
		status.Errors = append(status.Errors, fmt.Sprintf("failed to list nodes: %v", err))
		status.Problems = collectClusterProblems(status)

		return status
	}

	status.NodeCount = len(nodeList)

	gatewayPoolsByNode := make(map[string]map[string]struct{})
	selectorMatchesByPool := make(map[string]map[string]struct{})
	gatewayPoolRoutedCIDRInputs := make(map[string][]string)

	if health.gatewayPoolInformer != nil {
		for _, item := range health.gatewayPoolInformer.GetStore().List() {
			unstr, ok := item.(*unstructured.Unstructured)
			if !ok {
				continue
			}

			poolName := unstr.GetName()
			if poolName == "" {
				continue
			}

			specObj, ok := unstr.Object["spec"].(map[string]interface{})
			if !ok {
				continue
			}

			if routedCIDRsObj, okStr := specObj["routedCidrs"].([]interface{}); okStr {
				routedCIDRs := make([]string, 0, len(routedCIDRsObj))
				for _, cidrObj := range routedCIDRsObj {
					cidr, okStr := cidrObj.(string)
					if !okStr {
						continue
					}

					cidr = strings.TrimSpace(cidr)
					if cidr == "" {
						continue
					}

					routedCIDRs = append(routedCIDRs, cidr)
				}

				if len(routedCIDRs) > 0 {
					gatewayPoolRoutedCIDRInputs[poolName] = routedCIDRs
				}
			}

			nodeSelectorObj, ok := specObj["nodeSelector"].(map[string]interface{})
			if !ok || len(nodeSelectorObj) == 0 {
				continue
			}

			nodeSelector := make(map[string]string, len(nodeSelectorObj))
			for k, v := range nodeSelectorObj {
				if s, ok := v.(string); ok {
					nodeSelector[k] = s
				}
			}

			if len(nodeSelector) == 0 {
				continue
			}

			selector := labels.SelectorFromSet(nodeSelector)
			for _, node := range nodeList {
				if !selector.Matches(labels.Set(node.Labels)) {
					continue
				}

				nodeName := node.Name
				if nodeName == "" {
					continue
				}

				if gatewayPoolsByNode[nodeName] == nil {
					gatewayPoolsByNode[nodeName] = make(map[string]struct{})
				}

				gatewayPoolsByNode[nodeName][poolName] = struct{}{}
				if selectorMatchesByPool[poolName] == nil {
					selectorMatchesByPool[poolName] = make(map[string]struct{})
				}

				selectorMatchesByPool[poolName][nodeName] = struct{}{}
			}
		}
	}

	nodeReadyByName := make(map[string]bool, len(nodeList))
	for _, node := range nodeList {
		nodeReadyByName[node.Name] = isNodeReady(node)
	}

	cachedStatuses := health.statusCache.GetAll()

	type pullNode struct{ nodeName, nodeIP string }

	var nodesToPull []pullNode

	cachedResults := make(map[string]NodeStatusResponse)

	for _, node := range nodeList {
		nodeName := node.Name

		var nodeIP string

		for _, addr := range node.Status.Addresses {
			if addr.Type == "InternalIP" {
				nodeIP = addr.Address
				break
			}
		}

		if cached, ok := cachedStatuses[nodeName]; ok {
			age := time.Since(cached.ReceivedAt)
			if age < health.staleThreshold {
				result := *cached.Status
				t := cached.ReceivedAt

				result.LastPushTime = &t
				if cached.Source != "" {
					result.StatusSource = cached.Source
				} else {
					result.StatusSource = "push"
				}

				cachedResults[nodeName] = result

				continue
			}

			if pullEnabled && nodeIP != "" {
				nodesToPull = append(nodesToPull, pullNode{nodeName: nodeName, nodeIP: nodeIP})
			} else {
				result := *cached.Status
				t := cached.ReceivedAt
				result.LastPushTime = &t

				result.StatusSource = "stale-cache"
				if !pullEnabled {
					result.FetchError = fmt.Sprintf("stale status (%s old), pull disabled", formatDurationAgo(age))
				} else {
					result.FetchError = fmt.Sprintf("stale status (%s old), no IP for pull fallback", formatDurationAgo(age))
				}

				cachedResults[nodeName] = result
			}
		} else {
			if pullEnabled && nodeIP != "" {
				nodesToPull = append(nodesToPull, pullNode{nodeName: nodeName, nodeIP: nodeIP})
			} else {
				cachedResults[nodeName] = NodeStatusResponse{NodeInfo: NodeInfo{Name: nodeName}, StatusSource: "no-data"}
			}
		}
	}

	// Include nodes whose K8s Node object is gone but whose agent is still
	// pushing status.  These will be surfaced with K8sReady="Missing".
	for nodeName, cached := range cachedStatuses {
		if _, alreadyHandled := cachedResults[nodeName]; alreadyHandled {
			continue
		}

		result := *cached.Status
		t := cached.ReceivedAt

		result.LastPushTime = &t
		if cached.Source != "" {
			result.StatusSource = cached.Source
		} else {
			result.StatusSource = "push"
		}

		cachedResults[nodeName] = result
	}

	type nodeResult struct {
		nodeName string
		status   *NodeStatusResponse
		err      error
	}

	if len(nodesToPull) > 0 {
		resultCh := make(chan nodeResult, len(nodesToPull))

		concurrency := health.maxPullConcurrency
		if concurrency <= 0 {
			concurrency = defaultMaxPullConcurrency
		}

		sem := make(chan struct{}, concurrency)

		var wg sync.WaitGroup
		for _, pn := range nodesToPull {
			wg.Add(1)

			go func(nodeName, ip string) {
				defer wg.Done()

				sem <- struct{}{}

				defer func() { <-sem }()

				nodeStatus, fetchErr := fetchNodeStatus(ctx, ip, health.nodeAgentHealthPort)
				resultCh <- nodeResult{nodeName: nodeName, status: nodeStatus, err: fetchErr}
			}(pn.nodeName, pn.nodeIP)
		}

		go func() {
			wg.Wait()
			close(resultCh)
		}()

		for result := range resultCh {
			if result.err != nil {
				if cached, ok := cachedStatuses[result.nodeName]; ok {
					age := time.Since(cached.ReceivedAt)
					staleResult := *cached.Status
					t := cached.ReceivedAt
					staleResult.LastPushTime = &t
					staleResult.StatusSource = "stale-cache"
					staleResult.FetchError = fmt.Sprintf("stale status (%s old), pull failed: %v", formatDurationAgo(age), result.err)
					cachedResults[result.nodeName] = staleResult
				} else {
					cachedResults[result.nodeName] = NodeStatusResponse{NodeInfo: NodeInfo{Name: result.nodeName}, StatusSource: "pull", FetchError: result.err.Error()}
				}
			} else if result.status != nil {
				result.status.StatusSource = "pull"
				cachedResults[result.nodeName] = *result.status
			}
		}
	}

	nodePodMap := make(map[string]*NodePodInfo)

	if health.podLister != nil {
		allPods, listErr := health.podLister.List(labels.Everything())
		if listErr == nil {
			for _, pod := range allPods {
				if pod.Spec.NodeName == "" {
					continue
				}

				podInfo := &NodePodInfo{PodName: pod.Name}
				if pod.Status.StartTime != nil {
					podInfo.StartTime = pod.Status.StartTime.Time
				}

				if len(pod.Status.ContainerStatuses) > 0 {
					podInfo.Restarts = pod.Status.ContainerStatuses[0].RestartCount
				}

				nodePodMap[pod.Spec.NodeName] = podInfo
			}
		}
	}

	for _, node := range nodeList {
		nodeStatus, ok := cachedResults[node.Name]
		if !ok {
			continue
		}

		if poolSet := gatewayPoolsByNode[node.Name]; len(poolSet) > 0 {
			nodeStatus.NodeInfo.IsGateway = true
		}

		nodeStatus.NodeInfo.K8sReady = "NotReady"
		if nodeReadyByName[node.Name] {
			nodeStatus.NodeInfo.K8sReady = "Ready"
		}

		if len(nodeStatus.NodeInfo.PodCIDRs) == 0 {
			if len(node.Spec.PodCIDRs) > 0 {
				nodeStatus.NodeInfo.PodCIDRs = append([]string(nil), node.Spec.PodCIDRs...)
			} else if node.Spec.PodCIDR != "" {
				nodeStatus.NodeInfo.PodCIDRs = []string{node.Spec.PodCIDR}
			}
		}

		if len(nodeStatus.NodeInfo.InternalIPs) == 0 || len(nodeStatus.NodeInfo.ExternalIPs) == 0 {
			internalIPs := make([]string, 0)
			externalIPs := make([]string, 0)

			for _, addr := range node.Status.Addresses {
				switch addr.Type {
				case corev1.NodeInternalIP:
					if addr.Address != "" {
						internalIPs = append(internalIPs, addr.Address)
					}
				case corev1.NodeExternalIP:
					if addr.Address != "" {
						externalIPs = append(externalIPs, addr.Address)
					}
				}
			}

			if len(nodeStatus.NodeInfo.InternalIPs) == 0 && len(internalIPs) > 0 {
				nodeStatus.NodeInfo.InternalIPs = internalIPs
			}

			if len(nodeStatus.NodeInfo.ExternalIPs) == 0 && len(externalIPs) > 0 {
				nodeStatus.NodeInfo.ExternalIPs = externalIPs
			}
		}

		if (nodeStatus.NodeInfo.WireGuard == nil || nodeStatus.NodeInfo.WireGuard.PublicKey == "") && node.Annotations != nil {
			if pubKey := node.Annotations[controller.WireGuardPubKeyAnnotation]; pubKey != "" {
				if nodeStatus.NodeInfo.WireGuard == nil {
					nodeStatus.NodeInfo.WireGuard = &WireGuardStatusInfo{}
				}

				nodeStatus.NodeInfo.WireGuard.PublicKey = pubKey
			}
		}

		nodeStatus.NodeInfo.ProviderID = node.Spec.ProviderID
		nodeStatus.NodeInfo.OSImage = node.Status.NodeInfo.OSImage
		nodeStatus.NodeInfo.Kernel = node.Status.NodeInfo.KernelVersion
		nodeStatus.NodeInfo.Kubelet = node.Status.NodeInfo.KubeletVersion
		nodeStatus.NodeInfo.Arch = node.Status.NodeInfo.Architecture

		nodeStatus.NodeInfo.NodeOS = node.Status.NodeInfo.OperatingSystem
		if len(node.Labels) > 0 {
			labelsCopy := make(map[string]string, len(node.Labels))
			for k, v := range node.Labels {
				labelsCopy[k] = v
			}

			nodeStatus.NodeInfo.K8sLabels = labelsCopy
		}

		if updatedAt := latestNodeUpdateTime(node); !updatedAt.IsZero() {
			t := updatedAt
			nodeStatus.NodeInfo.K8sUpdatedAt = &t
		}

		if nodeStatus.NodeInfo.SiteName == "" && node.Labels != nil {
			if siteLabel, ok := node.Labels[controller.SiteLabelKey]; ok && siteLabel != "" {
				nodeStatus.NodeInfo.SiteName = siteLabel
			}
		}

		if podInfo, ok := nodePodMap[node.Name]; ok {
			nodeStatus.NodePodInfo = podInfo
		}

		status.Nodes = append(status.Nodes, &nodeStatus)

		siteName := nodeStatus.NodeInfo.SiteName
		if siteName != "" && nodeStatus.NodeInfo.WireGuard != nil && nodeStatus.NodeInfo.WireGuard.Interface != "" {
			if site, ok := siteMap[siteName]; ok {
				site.OnlineCount++

				site.OfflineCount = site.NodeCount - site.OnlineCount
				if site.OfflineCount < 0 {
					site.OfflineCount = 0
				}
			} else {
				siteMap[siteName] = &SiteStatus{Name: siteName, NodeCount: 1, OnlineCount: 1, OfflineCount: 0, ManageCniPlugin: true}
			}
		}
	}

	// Emit entries for nodes still pushing status whose K8s Node object has
	// been deleted.  The node agent runs on the VM and continues reporting
	// even after the Node resource is removed from the API server.
	nodeNameSet := make(map[string]struct{}, len(nodeList))
	for _, n := range nodeList {
		nodeNameSet[n.Name] = struct{}{}
	}

	for name, nodeStatus := range cachedResults {
		if _, exists := nodeNameSet[name]; exists {
			continue // already emitted above
		}

		if poolSet := gatewayPoolsByNode[name]; len(poolSet) > 0 {
			nodeStatus.NodeInfo.IsGateway = true
		}

		nodeStatus.NodeInfo.K8sReady = "Missing"
		status.Nodes = append(status.Nodes, &nodeStatus)
	}

	// Route annotation is done incrementally per-node in PatchNode using
	// the cached CIDRIndex and infrastructure maps on ClusterStatusCache.
	// The directSitePeerings, siteNodeCIDRs, and gatewayPoolRoutedCIDRs
	// are built in ClusterStatusCache.Rebuild() instead of here.

	for _, site := range siteMap {
		status.Sites = append(status.Sites, *site)
	}

	sort.Slice(status.Sites, func(i, j int) bool { return status.Sites[i].Name < status.Sites[j].Name })
	status.SiteCount = len(status.Sites)

	if health.gatewayPoolInformer != nil {
		for _, item := range health.gatewayPoolInformer.GetStore().List() {
			unstr, ok := item.(*unstructured.Unstructured)
			if !ok {
				continue
			}

			poolStatus := GatewayPoolStatus{Name: unstr.GetName()}
			if statusObj, ok := unstr.Object["status"].(map[string]interface{}); ok {
				if nc, ok := statusObj["nodeCount"].(int64); ok {
					poolStatus.NodeCount = int(nc)
				} else if nc, ok := statusObj["nodeCount"].(float64); ok {
					poolStatus.NodeCount = int(nc)
				}

				if nodesObj, ok := statusObj["nodes"].([]interface{}); ok {
					siteNames := make(map[string]bool)

					for _, nodeObj := range nodesObj {
						nodeMap, ok := nodeObj.(map[string]interface{})
						if !ok {
							continue
						}

						if name, ok := nodeMap["name"].(string); ok && name != "" {
							poolStatus.Gateways = append(poolStatus.Gateways, name)
						}

						if siteName, ok := nodeMap["siteName"].(string); ok && siteName != "" {
							siteNames[siteName] = true
						}
					}

					if len(siteNames) == 1 {
						for siteName := range siteNames {
							poolStatus.SiteName = siteName
						}
					}
				}

				if connectedSites, ok := statusObj["connectedSites"].([]interface{}); ok {
					for _, siteObj := range connectedSites {
						if siteName, ok := siteObj.(string); ok {
							poolStatus.ConnectedSites = append(poolStatus.ConnectedSites, siteName)
						}
					}
				}

				if reachableSites, ok := statusObj["reachableSites"].([]interface{}); ok {
					for _, siteObj := range reachableSites {
						if siteName, ok := siteObj.(string); ok {
							poolStatus.ReachableSites = append(poolStatus.ReachableSites, siteName)
						}
					}
				}
			}

			if selectorMatches := selectorMatchesByPool[poolStatus.Name]; len(selectorMatches) > 0 {
				gatewaySet := make(map[string]struct{}, len(poolStatus.Gateways)+len(selectorMatches))
				for _, name := range poolStatus.Gateways {
					if name != "" {
						gatewaySet[name] = struct{}{}
					}
				}

				for name := range selectorMatches {
					gatewaySet[name] = struct{}{}
				}

				poolStatus.Gateways = make([]string, 0, len(gatewaySet))
				for name := range gatewaySet {
					poolStatus.Gateways = append(poolStatus.Gateways, name)
				}

				sort.Strings(poolStatus.Gateways)

				if len(poolStatus.Gateways) > poolStatus.NodeCount {
					poolStatus.NodeCount = len(poolStatus.Gateways)
				}
			}

			status.GatewayPools = append(status.GatewayPools, poolStatus)
		}

		sort.Slice(status.GatewayPools, func(i, j int) bool { return status.GatewayPools[i].Name < status.GatewayPools[j].Name })
	}

	if health.sitePeeringInformer != nil {
		for _, item := range health.sitePeeringInformer.GetStore().List() {
			unstr, ok := item.(*unstructured.Unstructured)
			if !ok {
				continue
			}

			peeringStatus := PeeringStatus{Name: "sitepeering/" + unstr.GetName()}
			if specObj, ok := unstr.Object["spec"].(map[string]interface{}); ok {
				if sitesObj, ok := specObj["sites"].([]interface{}); ok {
					for _, siteObj := range sitesObj {
						if siteName, ok := siteObj.(string); ok {
							peeringStatus.Sites = append(peeringStatus.Sites, siteName)
						}
					}
				}

				if _, ok := specObj["healthCheckSettings"]; ok {
					peeringStatus.HealthCheckEnabled = true
				}
			}

			status.Peerings = append(status.Peerings, peeringStatus)
		}
	}

	if health.assignmentInformer != nil {
		for _, item := range health.assignmentInformer.GetStore().List() {
			unstr, ok := item.(*unstructured.Unstructured)
			if !ok {
				continue
			}

			peeringStatus := PeeringStatus{Name: "assignment/" + unstr.GetName()}
			if specObj, ok := unstr.Object["spec"].(map[string]interface{}); ok {
				if sitesObj, ok := specObj["sites"].([]interface{}); ok {
					for _, siteObj := range sitesObj {
						if siteName, ok := siteObj.(string); ok {
							peeringStatus.Sites = append(peeringStatus.Sites, siteName)
						}
					}
				}

				if poolsObj, ok := specObj["gatewayPools"].([]interface{}); ok {
					for _, poolObj := range poolsObj {
						if poolName, ok := poolObj.(string); ok {
							peeringStatus.GatewayPools = append(peeringStatus.GatewayPools, poolName)
						}
					}
				}

				if _, ok := specObj["healthCheckSettings"]; ok {
					peeringStatus.HealthCheckEnabled = true
				}
			}

			status.Peerings = append(status.Peerings, peeringStatus)
		}
	}

	if health.poolPeeringInformer != nil {
		for _, item := range health.poolPeeringInformer.GetStore().List() {
			unstr, ok := item.(*unstructured.Unstructured)
			if !ok {
				continue
			}

			peeringStatus := PeeringStatus{Name: "poolpeering/" + unstr.GetName()}
			if specObj, ok := unstr.Object["spec"].(map[string]interface{}); ok {
				if poolsObj, ok := specObj["gatewayPools"].([]interface{}); ok {
					for _, poolObj := range poolsObj {
						if poolName, ok := poolObj.(string); ok {
							peeringStatus.GatewayPools = append(peeringStatus.GatewayPools, poolName)
						}
					}
				}

				if _, ok := specObj["healthCheckSettings"]; ok {
					peeringStatus.HealthCheckEnabled = true
				}
			}

			status.Peerings = append(status.Peerings, peeringStatus)
		}
	}

	sort.Slice(status.Peerings, func(i, j int) bool { return status.Peerings[i].Name < status.Peerings[j].Name })
	status.ConnectivityMatrix = buildConnectivityMatrix(status.Nodes, status.GatewayPools)
	status.Problems = collectClusterProblems(status)

	return status
}

// collectClusterProblems builds top-level problem entries from cluster and node health signals.
func collectClusterProblems(status *ClusterStatusResponse) []StatusProblem {
	if status == nil {
		return []StatusProblem{}
	}

	type groupedProblem struct {
		problemType string
		name        string
		errors      map[string]struct{}
	}

	problemsByObject := make(map[string]*groupedProblem)
	appendProblem := func(problemType, name, errMsg string) {
		problemType = strings.TrimSpace(problemType)
		name = strings.TrimSpace(name)

		errMsg = strings.TrimSpace(errMsg)
		if problemType == "" || name == "" || errMsg == "" {
			return
		}

		groupKey := problemType + "|" + name

		group := problemsByObject[groupKey]
		if group == nil {
			group = &groupedProblem{problemType: problemType, name: name, errors: make(map[string]struct{})}
			problemsByObject[groupKey] = group
		}

		group.errors[errMsg] = struct{}{}
	}

	for _, errMsg := range status.Errors {
		appendProblem("controller", "controller", errMsg)
	}

	now := time.Now()

	for _, node := range status.Nodes {
		nodeName := strings.TrimSpace(node.NodeInfo.Name)
		if nodeName == "" {
			nodeName = "unknown-node"
		}

		if node.NodeInfo.K8sReady != "Ready" {
			msg := "Kubernetes node is NotReady"
			if node.NodeInfo.K8sReady == "Missing" {
				msg = "Kubernetes node object is missing"
			}

			appendProblem("node", nodeName, msg)
		}

		if node.NodeInfo.WireGuard == nil || strings.TrimSpace(node.NodeInfo.WireGuard.Interface) == "" {
			appendProblem("node", nodeName, "WireGuard interface is not configured")
		}

		if node.FetchError != "" {
			appendProblem("node", nodeName, node.FetchError)
		}

		switch node.StatusSource {
		case "apiserver-push", "apiserver-ws":
			appendProblem("node", nodeName, "Direct node-to-controller status transport failed; API server fallback is active")
		case "stale-cache":
			appendProblem("node", nodeName, "Node status is stale cache data")
		case "error":
			appendProblem("node", nodeName, "No current node status data available")
		}

		for _, nodeError := range node.NodeErrors {
			if shouldSuppressNodeErrorForStatusSource(node.StatusSource, nodeError.Type) {
				continue
			}

			errMsg := strings.TrimSpace(nodeError.Message)
			if errMsg == "" {
				continue
			}

			appendProblem("node", nodeName, errMsg)
		}

		if node.HealthCheck != nil && !node.HealthCheck.Healthy {
			summary := strings.TrimSpace(node.HealthCheck.Summary)
			if summary == "" {
				summary = "health check reported unhealthy"
			}

			appendProblem("node", nodeName, summary)
		}

		if mismatchCount := routeMismatchCount(node); mismatchCount > 0 {
			appendProblem("node", nodeName, fmt.Sprintf("%d route next-hop mismatches (expected vs present)", mismatchCount))
		}

		if unhealthyPeers := unhealthyPeerLinkCount(node, now); unhealthyPeers > 0 {
			appendProblem("node", nodeName, fmt.Sprintf("%d peer links are unhealthy", unhealthyPeers))
		}

		// IPIP is not supported on Azure -- the platform filters IP protocol 4.
		if strings.HasPrefix(node.NodeInfo.ProviderID, "azure://") {
			for _, peer := range node.Peers {
				if peer.Tunnel.Protocol == "IPIP" {
					appendProblem("node", nodeName, "IPIP tunnel protocol is not supported on Azure (IP protocol 4 is blocked by the platform)")
					break
				}
			}
		}
	}

	for _, site := range status.Sites {
		siteName := strings.TrimSpace(site.Name)
		if siteName == "" {
			continue
		}

		if site.OfflineCount > 0 {
			appendProblem("site", "site", fmt.Sprintf("%s: %d/%d nodes are offline", siteName, site.OfflineCount, site.NodeCount))
		}
	}

	nodeStatusByName := make(map[string]*NodeStatusResponse, len(status.Nodes))
	for _, node := range status.Nodes {
		nodeName := strings.TrimSpace(node.NodeInfo.Name)
		if nodeName == "" {
			continue
		}

		nodeStatusByName[nodeName] = node
	}

	for _, pool := range status.GatewayPools {
		poolName := strings.TrimSpace(pool.Name)
		if poolName == "" {
			continue
		}

		total := pool.NodeCount
		if total <= 0 {
			total = len(pool.Gateways)
		}

		if total <= 0 {
			continue
		}

		online := 0

		for _, gatewayName := range pool.Gateways {
			node, ok := nodeStatusByName[gatewayName]
			if !ok {
				continue
			}

			if node.NodeInfo.WireGuard != nil && strings.TrimSpace(node.NodeInfo.WireGuard.Interface) != "" {
				online++
			}
		}

		if online < total {
			appendProblem("gatewayPool", poolName, fmt.Sprintf("%d/%d gateways are online", online, total))
		}
	}

	problemKeys := make([]string, 0, len(problemsByObject))
	for problemKey := range problemsByObject {
		problemKeys = append(problemKeys, problemKey)
	}

	sort.Strings(problemKeys)

	problems := make([]StatusProblem, 0, len(problemKeys))
	for _, problemKey := range problemKeys {
		group := problemsByObject[problemKey]

		errors := make([]string, 0, len(group.errors))
		for errMsg := range group.errors {
			errors = append(errors, errMsg)
		}

		sort.Strings(errors)
		problems = append(problems, StatusProblem{
			Name:   group.name,
			Type:   group.problemType,
			Errors: errors,
		})
	}

	return problems
}

// checkNodeMTUAnnotation logs a warning if the configured node MTU exceeds
// the given node's detected maximum WireGuard MTU annotation. Called from
// node informer event handlers (add/update/resync) so it fires only when
// the node object changes, not on every status fetch.
func checkNodeMTUAnnotation(configuredMTU int, node *corev1.Node) {
	if node == nil || node.Annotations == nil {
		return
	}

	mtuStr, ok := node.Annotations[controller.TunnelMTUAnnotation]
	if !ok || mtuStr == "" {
		return
	}

	nodeMTU, err := strconv.Atoi(mtuStr)
	if err != nil {
		klog.V(4).Infof("Node %s has invalid %s annotation %q: %v", node.Name, controller.TunnelMTUAnnotation, mtuStr, err)
		return
	}

	if configuredMTU > nodeMTU {
		klog.Warningf("MTU mismatch on %s: configured node.mtu %d exceeds detected maximum tunnel MTU %d",
			node.Name, configuredMTU, nodeMTU)
	}
}

// routeMismatchCount returns the number of expected/present route mismatches for a node.
func routeMismatchCount(node *NodeStatusResponse) int {
	allRoutes := node.RoutingTable.Routes
	mismatchCount := 0

	for _, route := range allRoutes {
		for _, hop := range route.NextHops {
			hopExpected := hop.Expected != nil && *hop.Expected

			hopPresent := hop.Present != nil && *hop.Present
			if hopExpected != hopPresent {
				mismatchCount++
			}
		}
	}

	return mismatchCount
}

// unhealthyPeerLinkCount returns the number of peer links considered unhealthy for a node.
func unhealthyPeerLinkCount(node *NodeStatusResponse, now time.Time) int {
	unhealthyCount := 0

	for _, peer := range node.Peers {
		hcStatus := ""
		if peer.HealthCheck != nil {
			hcStatus = strings.ToLower(strings.TrimSpace(peer.HealthCheck.Status))
		}

		hcEnabled := (peer.HealthCheck != nil && peer.HealthCheck.Enabled) || hcStatus != ""
		if hcEnabled {
			if hcStatus != "up" {
				unhealthyCount++
			}

			continue
		}

		lastHandshake := peer.Tunnel.LastHandshake
		if lastHandshake.IsZero() || now.Sub(lastHandshake) >= 3*time.Minute {
			unhealthyCount++
		}
	}

	return unhealthyCount
}

func shouldSuppressNodeErrorForStatusSource(statusSource, errorType string) bool {
	statusSource = strings.TrimSpace(statusSource)

	errorType = strings.TrimSpace(errorType)
	if errorType == "" {
		return false
	}

	websocketOnline := statusSource == "ws" || statusSource == "apiserver-ws"
	pushHealthy := websocketOnline || statusSource == "push" || statusSource == "apiserver-push"

	if websocketOnline {
		switch errorType {
		case "directWebsocket", "fallbackWebsocket", "directPush", "fallbackPush":
			return true
		}
	}

	if pushHealthy {
		switch errorType {
		case "directPush", "fallbackPush":
			return true
		}
	}

	return false
}

func isNodeReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}

	return false
}

func latestNodeUpdateTime(node *corev1.Node) time.Time {
	if node == nil {
		return time.Time{}
	}

	latest := node.CreationTimestamp.Time
	for _, condition := range node.Status.Conditions {
		if !condition.LastHeartbeatTime.IsZero() && condition.LastHeartbeatTime.After(latest) {
			latest = condition.LastHeartbeatTime.Time
		}

		if !condition.LastTransitionTime.IsZero() && condition.LastTransitionTime.After(latest) {
			latest = condition.LastTransitionTime.Time
		}
	}

	return latest
}

// buildConnectivityMatrix builds health check connectivity matrices from node peer data.
func buildConnectivityMatrix(nodes []*NodeStatusResponse, gatewayPools []GatewayPoolStatus) map[string]*SiteMatrix {
	siteNodes := make(map[string]map[string]bool)
	nodePeers := make(map[string][]WireGuardPeerStatus)
	nodeByName := make(map[string]*NodeStatusResponse)

	for _, n := range nodes {
		name := n.NodeInfo.Name

		site := n.NodeInfo.SiteName
		if name == "" || site == "" {
			continue
		}

		nodeByName[name] = n

		if siteNodes[site] == nil {
			siteNodes[site] = make(map[string]bool)
		}

		siteNodes[site][name] = true

		var allPeers []WireGuardPeerStatus

		for _, p := range n.Peers {
			if p.PeerType == "site" || p.PeerType == "gateway" {
				allPeers = append(allPeers, p)
			}
		}

		nodePeers[name] = allPeers

		for _, p := range n.Peers {
			if p.PeerType == "gateway" && p.Name != "" && p.SiteName == site {
				siteNodes[site][p.Name] = true
			}
		}
	}

	if len(siteNodes) == 0 {
		siteNodes = make(map[string]map[string]bool)
	}

	result := make(map[string]*SiteMatrix)
	selfMatrixStatusFromCNI := func(node *NodeStatusResponse) string {
		if node.NodeInfo.WireGuard != nil && strings.TrimSpace(node.NodeInfo.WireGuard.Interface) != "" {
			return "up"
		}

		return ""
	}

	buildScopeMatrix := func(nodeSet map[string]bool) *SiteMatrix {
		if len(nodeSet) == 0 || len(nodeSet) > 100 {
			return nil
		}

		nodeNames := make([]string, 0, len(nodeSet))
		for name := range nodeSet {
			nodeNames = append(nodeNames, name)
		}

		sort.Strings(nodeNames)

		results := make(map[string]map[string]string)
		for _, srcNode := range nodeNames {
			results[srcNode] = make(map[string]string)
			if node, ok := nodeByName[srcNode]; ok {
				results[srcNode][srcNode] = selfMatrixStatusFromCNI(node)
			}

			for _, peer := range nodePeers[srcNode] {
				tgtNode := peer.Name
				if tgtNode == "" || tgtNode == srcNode || !nodeSet[tgtNode] {
					continue
				}

				cellStatus := ""
				if peer.HealthCheck != nil {
					cellStatus = peer.HealthCheck.Status
				} else if peer.PeerType == "gateway" && !peer.Tunnel.LastHandshake.IsZero() {
					cellStatus = "up"
				}

				results[srcNode][tgtNode] = cellStatus
			}
		}

		return &SiteMatrix{Nodes: nodeNames, Results: results}
	}

	for site, nodeSet := range siteNodes {
		scopeMatrix := buildScopeMatrix(nodeSet)
		if scopeMatrix != nil {
			result[site] = scopeMatrix
		}
	}

	for _, pool := range gatewayPools {
		poolName := strings.TrimSpace(pool.Name)
		if poolName == "" {
			continue
		}

		poolNodeSet := make(map[string]bool)

		for _, gatewayName := range pool.Gateways {
			name := strings.TrimSpace(gatewayName)
			if name == "" {
				continue
			}

			poolNodeSet[name] = true
			for _, peer := range nodePeers[name] {
				peerName := strings.TrimSpace(peer.Name)
				if peerName == "" {
					continue
				}

				if _, ok := nodeByName[peerName]; ok {
					poolNodeSet[peerName] = true
				}
			}

			for srcNodeName, peers := range nodePeers {
				for _, peer := range peers {
					if strings.TrimSpace(peer.Name) == name {
						if _, ok := nodeByName[srcNodeName]; ok {
							poolNodeSet[srcNodeName] = true
						}

						break
					}
				}
			}
		}

		scopeMatrix := buildScopeMatrix(poolNodeSet)
		if scopeMatrix != nil {
			result["pool:"+poolName] = scopeMatrix
		}
	}

	if len(result) == 0 {
		return nil
	}

	return result
}
