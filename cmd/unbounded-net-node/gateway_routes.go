// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	unboundednetv1alpha1 "github.com/Azure/unbounded-kube/api/net/v1alpha1"
	"github.com/Azure/unbounded-kube/internal/net/routeplan"
)

// parseSite converts an unstructured object to a Site
func parseSite(obj *unstructured.Unstructured) (*unboundednetv1alpha1.Site, error) {
	data, err := obj.MarshalJSON()
	if err != nil {
		return nil, err
	}

	var site unboundednetv1alpha1.Site
	if err := json.Unmarshal(data, &site); err != nil {
		return nil, err
	}

	return &site, nil
}

// parseSiteNodeSlice converts an unstructured object to a SiteNodeSlice
func parseSiteNodeSlice(obj *unstructured.Unstructured) (*unboundednetv1alpha1.SiteNodeSlice, error) {
	data, err := obj.MarshalJSON()
	if err != nil {
		return nil, err
	}

	var slice unboundednetv1alpha1.SiteNodeSlice
	if err := json.Unmarshal(data, &slice); err != nil {
		return nil, err
	}

	return &slice, nil
}

// parseGatewayPool converts an unstructured object to a GatewayPool
func parseGatewayPool(obj *unstructured.Unstructured) (*unboundednetv1alpha1.GatewayPool, error) {
	data, err := obj.MarshalJSON()
	if err != nil {
		return nil, err
	}

	var pool unboundednetv1alpha1.GatewayPool
	if err := json.Unmarshal(data, &pool); err != nil {
		return nil, err
	}

	return &pool, nil
}

// parseGatewayNode converts an unstructured object to a GatewayNode.
func parseGatewayNode(obj *unstructured.Unstructured) (*unboundednetv1alpha1.GatewayPoolNode, error) {
	data, err := obj.MarshalJSON()
	if err != nil {
		return nil, err
	}

	var gatewayNode unboundednetv1alpha1.GatewayPoolNode
	if err := json.Unmarshal(data, &gatewayNode); err != nil {
		return nil, err
	}

	return &gatewayNode, nil
}

const (
	gatewayPoolTypeExternal = "External"
	gatewayPoolTypeInternal = "Internal"
)

func normalizeGatewayPoolType(poolType string) string {
	if poolType == "" {
		return gatewayPoolTypeExternal
	}

	return poolType
}

// poolReachableNodeCIDRs returns node CIDRs from sites connected to or
// reachable via a gateway pool, excluding the local site. This scopes the
// fallback routed CIDRs to only CIDRs that are actually reachable through
// this specific pool, preventing routes from leaking across unrelated pools.
func poolReachableNodeCIDRs(pool *unboundednetv1alpha1.GatewayPool, siteMap map[string]*unboundednetv1alpha1.Site, mySiteName string) []string {
	if pool == nil {
		return nil
	}

	reachableSites := make(map[string]bool, len(pool.Status.ConnectedSites)+len(pool.Status.ReachableSites))
	for _, s := range pool.Status.ConnectedSites {
		if s != mySiteName {
			reachableSites[s] = true
		}
	}

	for _, s := range pool.Status.ReachableSites {
		if s != mySiteName {
			reachableSites[s] = true
		}
	}

	var cidrs []string

	for siteName := range reachableSites {
		if site, ok := siteMap[siteName]; ok {
			cidrs = append(cidrs, site.Spec.NodeCidrs...)
		}
	}

	return dedupeStrings(cidrs)
}

func buildGatewayPoolRoutedCIDRs(pool *unboundednetv1alpha1.GatewayPool, fallback []string) []string {
	if pool == nil {
		return []string{}
	}

	cidrs := make([]string, 0, len(pool.Spec.RoutedCidrs)+len(fallback))
	cidrs = append(cidrs, pool.Spec.RoutedCidrs...)
	cidrs = append(cidrs, fallback...)

	return dedupeStrings(cidrs)
}

func resolveGatewayPoolSiteName(
	mySiteName, myPubKey string,
	pool *unboundednetv1alpha1.GatewayPool,
) string {
	if mySiteName != "" {
		return mySiteName
	}

	if pool == nil {
		return ""
	}

	if myPubKey != "" {
		for _, gatewayNode := range pool.Status.Nodes {
			if gatewayNode.WireGuardPublicKey != myPubKey {
				continue
			}

			if gatewayNode.SiteName != "" {
				return gatewayNode.SiteName
			}
		}
	}

	return ""
}

func buildGatewayNodeRoutesForStatus(pool *unboundednetv1alpha1.GatewayPool, site *unboundednetv1alpha1.Site) map[string]unboundednetv1alpha1.GatewayNodeRoute {
	routes := make(map[string]unboundednetv1alpha1.GatewayNodeRoute)

	fullPath := make([]unboundednetv1alpha1.GatewayNodePathHop, 0, 2)
	if site != nil && site.Name != "" {
		fullPath = append(fullPath, unboundednetv1alpha1.GatewayNodePathHop{Type: "Site", Name: site.Name})
	}

	if pool != nil {
		fullPath = append(fullPath, unboundednetv1alpha1.GatewayNodePathHop{Type: "GatewayPool", Name: pool.Name})
	}

	newPathList := func() [][]unboundednetv1alpha1.GatewayNodePathHop {
		if len(fullPath) == 0 {
			return nil
		}

		pathCopy := make([]unboundednetv1alpha1.GatewayNodePathHop, len(fullPath))
		copy(pathCopy, fullPath)

		return [][]unboundednetv1alpha1.GatewayNodePathHop{pathCopy}
	}

	if pool != nil {
		for _, cidr := range pool.Spec.RoutedCidrs {
			if cidr == "" {
				continue
			}

			routes[cidr] = unboundednetv1alpha1.GatewayNodeRoute{
				Type:  "RoutedCidr",
				Paths: newPathList(),
			}
		}
	}

	if site != nil {
		for _, cidr := range site.Spec.NodeCidrs {
			if cidr == "" {
				continue
			}

			routes[cidr] = unboundednetv1alpha1.GatewayNodeRoute{
				Type:  "NodeCidr",
				Paths: newPathList(),
			}
		}

		for _, assignment := range site.Spec.PodCidrAssignments {
			for _, cidr := range assignment.CidrBlocks {
				if cidr == "" {
					continue
				}

				routes[cidr] = unboundednetv1alpha1.GatewayNodeRoute{
					Type:  "PodCidr",
					Paths: newPathList(),
				}
			}
		}
	}

	return routes
}

func buildGatewayNodeRoutesForAssignedSitesStatus(
	poolName string,
	pool *unboundednetv1alpha1.GatewayPool,
	assignments []unboundednetv1alpha1.SiteGatewayPoolAssignment,
	siteMap map[string]*unboundednetv1alpha1.Site,
) map[string]unboundednetv1alpha1.GatewayNodeRoute {
	merged := buildGatewayNodeRoutesForStatus(pool, nil)
	if poolName == "" {
		return merged
	}

	assignedSites := make(map[string]struct{})

	for _, assignment := range assignments {
		if !unboundednetv1alpha1.SpecEnabled(assignment.Spec.Enabled) {
			continue
		}

		poolMatched := false

		for _, assignmentPoolName := range assignment.Spec.GatewayPools {
			if assignmentPoolName == poolName {
				poolMatched = true
				break
			}
		}

		if !poolMatched {
			continue
		}

		for _, siteName := range assignment.Spec.Sites {
			siteName = strings.TrimSpace(siteName)
			if siteName == "" {
				continue
			}

			assignedSites[siteName] = struct{}{}
		}
	}

	for siteName := range assignedSites {
		site := siteMap[siteName]
		if site == nil {
			continue
		}

		siteRoutes := buildGatewayNodeRoutesForStatus(pool, site)
		for cidr, route := range siteRoutes {
			existing, ok := merged[cidr]
			if !ok {
				merged[cidr] = route
				continue
			}

			merged[cidr] = mergeGatewayNodeRoutePaths(existing, route)
		}
	}

	return merged
}

func syncGatewayNodeRoutesStatus(ctx context.Context, dynamicClient dynamic.Interface, gatewayNodeInformer cache.SharedIndexInformer, nodeName string, routes map[string]unboundednetv1alpha1.GatewayNodeRoute) error {
	if dynamicClient == nil || nodeName == "" {
		return nil
	}

	// Build the routes patch. MergePatch preserves map keys that are absent
	// from the patch, so we must explicitly null out any CIDRs that were
	// previously advertised but are no longer present. Check the informer
	// cache for the current GatewayPoolNode to discover which keys need removal.
	routesPatch := make(map[string]interface{}, len(routes))
	for cidr, route := range routes {
		routesPatch[cidr] = route
	}

	if gatewayNodeInformer != nil {
		existingObj, exists, _ := gatewayNodeInformer.GetStore().GetByKey(nodeName) //nolint:errcheck
		if exists {
			if existing, ok := existingObj.(*unstructured.Unstructured); ok {
				if statusObj, ok := existing.Object["status"].(map[string]interface{}); ok {
					if existingRoutes, ok := statusObj["routes"].(map[string]interface{}); ok {
						for cidr := range existingRoutes {
							if _, stillPresent := routes[cidr]; !stillPresent {
								routesPatch[cidr] = nil // null tells MergePatch to delete this key
							}
						}
					}
				}
			}
		}
	}

	statusPatch := map[string]interface{}{
		"status": map[string]interface{}{
			"lastUpdated": time.Now().UTC().Format(time.RFC3339),
			"routes":      routesPatch,
		},
	}

	return patchGatewayNodeStatus(ctx, dynamicClient, nodeName, statusPatch)
}

func patchGatewayNodeStatus(ctx context.Context, dynamicClient dynamic.Interface, nodeName string, statusPatch map[string]interface{}) error {
	patchData, err := json.Marshal(statusPatch)
	if err != nil {
		return err
	}

	// First try the status subresource. If the CRD was installed without a
	// status subresource, fall back to patching the main object.
	_, err = dynamicClient.Resource(gatewayNodeGVR).Patch(ctx, nodeName, types.MergePatchType, patchData, metav1.PatchOptions{}, "status")
	if err == nil {
		return nil
	}

	_, fallbackErr := dynamicClient.Resource(gatewayNodeGVR).Patch(ctx, nodeName, types.MergePatchType, patchData, metav1.PatchOptions{})
	if fallbackErr != nil {
		if apierrors.IsNotFound(fallbackErr) {
			return fmt.Errorf("gatewaypoolnode %q not found while patching status: %w", nodeName, fallbackErr)
		}

		return fallbackErr
	}

	return nil
}

func gatewayNodePathHasHop(path []unboundednetv1alpha1.GatewayNodePathHop, hopType, hopName string) bool {
	converted := make([]routeplan.PathHop, len(path))
	for i, hop := range path {
		converted[i] = routeplan.PathHop{Type: hop.Type, Name: hop.Name}
	}

	return routeplan.PathHasHop(converted, hopType, hopName)
}

func gatewayNodeRoutePathLength(route unboundednetv1alpha1.GatewayNodeRoute) int {
	converted := routeplan.LearnedRoute{Paths: make([][]routeplan.PathHop, 0, len(route.Paths))}
	for _, path := range route.Paths {
		convertedPath := make([]routeplan.PathHop, len(path))
		for i, hop := range path {
			convertedPath[i] = routeplan.PathHop{Type: hop.Type, Name: hop.Name}
		}

		converted.Paths = append(converted.Paths, convertedPath)
	}

	return routeplan.RoutePathLength(converted)
}

func appendGatewayPoolHop(route unboundednetv1alpha1.GatewayNodeRoute, poolName string) unboundednetv1alpha1.GatewayNodeRoute {
	if poolName == "" {
		return route
	}

	if len(route.Paths) == 0 {
		route.Paths = [][]unboundednetv1alpha1.GatewayNodePathHop{{{Type: "GatewayPool", Name: poolName}}}
		return route
	}

	updatedPaths := make([][]unboundednetv1alpha1.GatewayNodePathHop, 0, len(route.Paths))
	for _, path := range route.Paths {
		newPath := make([]unboundednetv1alpha1.GatewayNodePathHop, len(path))
		copy(newPath, path)

		if gatewayNodePathHasHop(newPath, "GatewayPool", poolName) {
			// Drop paths where our pool already appears -- re-advertising
			// them would create a loop (traffic hops away and back to us).
			continue
		}

		newPath = append(newPath, unboundednetv1alpha1.GatewayNodePathHop{Type: "GatewayPool", Name: poolName})
		updatedPaths = append(updatedPaths, newPath)
	}

	route.Paths = dedupeGatewayNodeRoutePaths(updatedPaths)

	return route
}

func mergeGatewayNodeAdvertisedRoutes(
	localRoutes map[string]unboundednetv1alpha1.GatewayNodeRoute,
	gatewayPeers []gatewayPeerInfo,
	localPoolName string,
) map[string]unboundednetv1alpha1.GatewayNodeRoute {
	merged := make(map[string]unboundednetv1alpha1.GatewayNodeRoute, len(localRoutes))
	for cidr, route := range localRoutes {
		if cidr == "" {
			continue
		}

		merged[cidr] = route
	}

	for _, peer := range gatewayPeers {
		for cidr, route := range peer.LearnedRoutes {
			if cidr == "" {
				continue
			}

			candidate := appendGatewayPoolHop(route, localPoolName)
			if len(candidate.Paths) == 0 {
				// All paths contained our pool (loop) -- nothing to advertise.
				continue
			}

			existing, ok := merged[cidr]
			if !ok {
				merged[cidr] = candidate
				continue
			}

			merged[cidr] = mergeGatewayNodeRoutePaths(existing, candidate)
		}
	}

	return merged
}

func mergeGatewayNodeRoutePaths(existing, candidate unboundednetv1alpha1.GatewayNodeRoute) unboundednetv1alpha1.GatewayNodeRoute {
	merged := existing

	allPaths := make([][]unboundednetv1alpha1.GatewayNodePathHop, 0, len(existing.Paths)+len(candidate.Paths))
	for _, path := range existing.Paths {
		copied := make([]unboundednetv1alpha1.GatewayNodePathHop, len(path))
		copy(copied, path)
		allPaths = append(allPaths, copied)
	}

	for _, path := range candidate.Paths {
		copied := make([]unboundednetv1alpha1.GatewayNodePathHop, len(path))
		copy(copied, path)
		allPaths = append(allPaths, copied)
	}

	if merged.Type == "" {
		merged.Type = candidate.Type
	}

	merged.Paths = dedupeGatewayNodeRoutePaths(allPaths)

	return merged
}

func dedupeGatewayNodeRoutePaths(paths [][]unboundednetv1alpha1.GatewayNodePathHop) [][]unboundednetv1alpha1.GatewayNodePathHop {
	converted := make([][]routeplan.PathHop, 0, len(paths))
	for _, path := range paths {
		convertedPath := make([]routeplan.PathHop, len(path))
		for i, hop := range path {
			convertedPath[i] = routeplan.PathHop{Type: hop.Type, Name: hop.Name}
		}

		converted = append(converted, convertedPath)
	}

	deduped := routeplan.DedupePathHops(converted)

	result := make([][]unboundednetv1alpha1.GatewayNodePathHop, 0, len(deduped))
	for _, path := range deduped {
		convertedPath := make([]unboundednetv1alpha1.GatewayNodePathHop, len(path))
		for i, hop := range path {
			convertedPath[i] = unboundednetv1alpha1.GatewayNodePathHop{Type: hop.Type, Name: hop.Name}
		}

		result = append(result, convertedPath)
	}

	return result
}

func routedCIDRsForGatewayPeer(
	gatewayNode *unboundednetv1alpha1.GatewayPoolNode,
	fallback []string,
	mySiteName string,
	localGatewayPools []string,
	excludedNodeCIDRSites map[string]bool,
	now time.Time,
	staleAfter time.Duration,
) ([]string, map[string]int, map[string]unboundednetv1alpha1.GatewayNodeRoute) {
	if gatewayNode == nil {
		return fallback, map[string]int{}, map[string]unboundednetv1alpha1.GatewayNodeRoute{}
	}

	planRoutes := make(map[string]routeplan.LearnedRoute, len(gatewayNode.Status.Routes))
	for cidr, route := range gatewayNode.Status.Routes {
		paths := make([][]routeplan.PathHop, 0, len(route.Paths))
		for _, path := range route.Paths {
			convertedPath := make([]routeplan.PathHop, len(path))
			for index, hop := range path {
				convertedPath[index] = routeplan.PathHop{Type: hop.Type, Name: hop.Name}
			}

			paths = append(paths, convertedPath)
		}

		planRoutes[cidr] = routeplan.LearnedRoute{Paths: paths}
	}

	_, distances, selectedPlan := routeplan.FilterGatewayAdvertisedRoutes(
		routeplan.GatewayRouteAdvertisement{
			Name:        gatewayNode.Name,
			LastUpdated: gatewayNode.Status.LastUpdated.Time,
			Routes:      planRoutes,
		},
		fallback,
		mySiteName,
		localGatewayPools,
		now,
		staleAfter,
	)

	if len(excludedNodeCIDRSites) > 0 {
		for cidr, route := range selectedPlan {
			baseRoute := gatewayNode.Status.Routes[cidr]

			baseRouteType := strings.ToLower(strings.TrimSpace(baseRoute.Type))
			if baseRouteType != "nodecidr" && baseRouteType != "routedcidr" {
				continue
			}

			filteredPaths := make([][]routeplan.PathHop, 0, len(route.Paths))
			for _, path := range route.Paths {
				if pathHasSiteInSet(path, excludedNodeCIDRSites) {
					continue
				}

				filteredPaths = append(filteredPaths, path)
			}

			if len(filteredPaths) == 0 {
				delete(selectedPlan, cidr)
				delete(distances, cidr)

				continue
			}

			route.Paths = routeplan.DedupePathHops(filteredPaths)
			selectedPlan[cidr] = route

			pathLength := routeplan.RoutePathLength(route)
			if pathLength <= 1 {
				distances[cidr] = 1
			} else {
				distances[cidr] = pathLength - 1
			}
		}
	}

	if gatewayNode.Status.LastUpdated.IsZero() || gatewayNode.Status.LastUpdated.Add(staleAfter).Before(now) {
		klog.V(3).Infof("GatewayNode %s route advertisement is stale or missing heartbeat, using fallback routes", gatewayNode.Name)
	}

	selected := make(map[string]unboundednetv1alpha1.GatewayNodeRoute, len(selectedPlan))
	for cidr, route := range selectedPlan {
		baseRoute := gatewayNode.Status.Routes[cidr]

		paths := make([][]unboundednetv1alpha1.GatewayNodePathHop, 0, len(route.Paths))
		for _, path := range route.Paths {
			convertedPath := make([]unboundednetv1alpha1.GatewayNodePathHop, len(path))
			for index, hop := range path {
				convertedPath[index] = unboundednetv1alpha1.GatewayNodePathHop{Type: hop.Type, Name: hop.Name}
			}

			paths = append(paths, convertedPath)
		}

		baseRoute.Paths = paths
		selected[cidr] = baseRoute
	}

	routes := make([]string, 0, len(selected))
	for cidr := range selected {
		routes = append(routes, cidr)
	}

	sort.Strings(routes)

	return routes, distances, selected
}

func pathHasSiteInSet(path []routeplan.PathHop, sites map[string]bool) bool {
	if len(sites) == 0 {
		return false
	}

	for _, hop := range path {
		if hop.Type != "Site" {
			continue
		}

		if sites[hop.Name] {
			return true
		}
	}

	return false
}

func dedupeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}

		seen[value] = struct{}{}
	}

	return setToSortedStrings(seen)
}
