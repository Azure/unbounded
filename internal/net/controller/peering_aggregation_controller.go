// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package controller implements the Peering aggregation controller.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
)

var sitePeeringGVR = schema.GroupVersionResource{
	Group:    "net.unbounded-kube.io",
	Version:  "v1alpha1",
	Resource: "sitepeerings",
}

var siteGatewayPoolAssignmentGVR = schema.GroupVersionResource{
	Group:    "net.unbounded-kube.io",
	Version:  "v1alpha1",
	Resource: "sitegatewaypoolassignments",
}

var gatewayPoolPeeringGVR = schema.GroupVersionResource{
	Group:    "net.unbounded-kube.io",
	Version:  "v1alpha1",
	Resource: "gatewaypoolpeerings",
}

var siteGVRPeering = schema.GroupVersionResource{
	Group:    "net.unbounded-kube.io",
	Version:  "v1alpha1",
	Resource: "sites",
}

var gatewayPoolGVRPeering = schema.GroupVersionResource{
	Group:    "net.unbounded-kube.io",
	Version:  "v1alpha1",
	Resource: "gatewaypools",
}

const defaultPoolWeight = 100

const (
	routeTypePodCIDR    = "podCidr"
	routeTypeNodeCIDR   = "nodeCidr"
	routeTypeRoutedCIDR = "routedCidr"
)

const (
	// peeringMaxRetries is the maximum number of consecutive retries before
	// switching to a longer requeue delay.
	peeringMaxRetries = 10
	// peeringBackoffDelay is the requeue delay used after exceeding peeringMaxRetries.
	peeringBackoffDelay = 5 * time.Minute
)

// PeeringAggregationController updates GatewayPool status based on Peering relationships.
type PeeringAggregationController struct {
	dynamicClient dynamic.Interface

	sitePeeringInformer cache.SharedIndexInformer
	sitePeeringSynced   cache.InformerSynced
	assignmentInformer  cache.SharedIndexInformer
	assignmentSynced    cache.InformerSynced
	poolPeeringInformer cache.SharedIndexInformer
	poolPeeringSynced   cache.InformerSynced
	siteInformer        cache.SharedIndexInformer
	siteSynced          cache.InformerSynced
	gatewayPoolInformer cache.SharedIndexInformer
	gatewayPoolSynced   cache.InformerSynced

	workqueue workqueue.TypedRateLimitingInterface[string]

	// retryCount tracks consecutive sync failures per pool for backoff.
	retryMu    sync.Mutex
	retryCount map[string]int

	hasSynced bool
}

// NewPeeringAggregationController creates a new peering aggregation controller.
func NewPeeringAggregationController(
	dynamicClient dynamic.Interface,
	dynamicInformerFactory dynamicinformer.DynamicSharedInformerFactory,
) (*PeeringAggregationController, error) {
	sitePeeringInformer := dynamicInformerFactory.ForResource(sitePeeringGVR).Informer()
	assignmentInformer := dynamicInformerFactory.ForResource(siteGatewayPoolAssignmentGVR).Informer()
	poolPeeringInformer := dynamicInformerFactory.ForResource(gatewayPoolPeeringGVR).Informer()
	siteInformer := dynamicInformerFactory.ForResource(siteGVRPeering).Informer()
	gatewayPoolInformer := dynamicInformerFactory.ForResource(gatewayPoolGVRPeering).Informer()

	pc := &PeeringAggregationController{
		dynamicClient:       dynamicClient,
		sitePeeringInformer: sitePeeringInformer,
		sitePeeringSynced:   sitePeeringInformer.HasSynced,
		assignmentInformer:  assignmentInformer,
		assignmentSynced:    assignmentInformer.HasSynced,
		poolPeeringInformer: poolPeeringInformer,
		poolPeeringSynced:   poolPeeringInformer.HasSynced,
		siteInformer:        siteInformer,
		siteSynced:          siteInformer.HasSynced,
		gatewayPoolInformer: gatewayPoolInformer,
		gatewayPoolSynced:   gatewayPoolInformer.HasSynced,
		workqueue:           workqueue.NewTypedRateLimitingQueueWithConfig(workqueue.DefaultTypedControllerRateLimiter[string](), workqueue.TypedRateLimitingQueueConfig[string]{Name: "Peerings"}),
		retryCount:          make(map[string]int),
	}

	enqueueAll := func() {
		pc.enqueueAllPools()
	}

	if _, err := sitePeeringInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { enqueueAll() },
		UpdateFunc: func(old, new interface{}) { enqueueAll() },
		DeleteFunc: func(obj interface{}) { enqueueAll() },
	}); err != nil {
		return nil, fmt.Errorf("failed to add site peering event handler: %w", err)
	}

	if _, err := assignmentInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { enqueueAll() },
		UpdateFunc: func(old, new interface{}) { enqueueAll() },
		DeleteFunc: func(obj interface{}) { enqueueAll() },
	}); err != nil {
		return nil, fmt.Errorf("failed to add site gateway pool assignment event handler: %w", err)
	}

	if _, err := poolPeeringInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { enqueueAll() },
		UpdateFunc: func(old, new interface{}) { enqueueAll() },
		DeleteFunc: func(obj interface{}) { enqueueAll() },
	}); err != nil {
		return nil, fmt.Errorf("failed to add gateway pool peering event handler: %w", err)
	}

	if _, err := siteInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { enqueueAll() },
		UpdateFunc: func(old, new interface{}) { enqueueAll() },
		DeleteFunc: func(obj interface{}) { enqueueAll() },
	}); err != nil {
		return nil, fmt.Errorf("failed to add site event handler: %w", err)
	}

	if _, err := gatewayPoolInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { pc.enqueuePool(obj) },
		UpdateFunc: func(old, new interface{}) { pc.enqueuePool(new) },
		DeleteFunc: func(obj interface{}) {},
	}); err != nil {
		return nil, fmt.Errorf("failed to add gateway pool event handler: %w", err)
	}

	return pc, nil
}

// Run starts the peering aggregation controller.
func (pc *PeeringAggregationController) Run(ctx context.Context, workers int) error {
	defer utilruntime.HandleCrash()
	defer pc.workqueue.ShutDown()

	klog.Info("Starting Peering aggregation controller")

	if ok := cache.WaitForCacheSync(ctx.Done(), pc.sitePeeringSynced, pc.assignmentSynced, pc.poolPeeringSynced, pc.siteSynced, pc.gatewayPoolSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	pc.hasSynced = true
	pc.enqueueAllPools()

	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, pc.runWorker, time.Second)
	}

	klog.Info("Peering aggregation controller started")
	<-ctx.Done()
	klog.Info("Shutting down Peering aggregation controller")

	return nil
}

func (pc *PeeringAggregationController) runWorker(ctx context.Context) {
	for pc.processNextWorkItem(ctx) {
	}
}

func (pc *PeeringAggregationController) processNextWorkItem(ctx context.Context) bool {
	key, shutdown := pc.workqueue.Get()
	if shutdown {
		return false
	}
	defer pc.workqueue.Done(key)

	start := time.Now()
	err := pc.syncPool(ctx, key)
	duration := time.Since(start).Seconds()
	reconciliationDuration.WithLabelValues("Peerings").Observe(duration)

	if err == nil {
		pc.workqueue.Forget(key)
		pc.resetRetryCount(key)
		reconciliationTotal.WithLabelValues("Peerings", "success").Inc()

		return true
	}

	reconciliationErrors.WithLabelValues("Peerings").Inc()
	reconciliationTotal.WithLabelValues("Peerings", "error").Inc()

	retries := pc.incrementRetryCount(key)
	if retries > peeringMaxRetries {
		klog.Warningf("Peering aggregation for gateway pool %s exceeded %d retries, requeueing after %v", key, peeringMaxRetries, peeringBackoffDelay)
		pc.workqueue.Forget(key)
		pc.workqueue.AddAfter(key, peeringBackoffDelay)

		return true
	}

	workqueueRetries.WithLabelValues("Peerings").Inc()
	utilruntime.HandleError(fmt.Errorf("error syncing peering aggregation for gateway pool %s (retry %d/%d): %v", key, retries, peeringMaxRetries, err))
	pc.workqueue.AddRateLimited(key)

	return true
}

// incrementRetryCount increments and returns the retry count for a key.
func (pc *PeeringAggregationController) incrementRetryCount(key string) int {
	pc.retryMu.Lock()
	defer pc.retryMu.Unlock()

	pc.retryCount[key]++

	return pc.retryCount[key]
}

// resetRetryCount resets the retry counter for a key after a successful sync.
func (pc *PeeringAggregationController) resetRetryCount(key string) {
	pc.retryMu.Lock()
	defer pc.retryMu.Unlock()

	delete(pc.retryCount, key)
}

func (pc *PeeringAggregationController) enqueuePool(obj interface{}) {
	unstr, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return
	}

	pc.workqueue.Add(unstr.GetName())
}

func (pc *PeeringAggregationController) enqueueAllPools() {
	if !pc.hasSynced {
		return
	}

	items := pc.gatewayPoolInformer.GetStore().List()
	for _, item := range items {
		unstr, ok := item.(*unstructured.Unstructured)
		if !ok {
			continue
		}

		pc.workqueue.Add(unstr.GetName())
	}
}

func (pc *PeeringAggregationController) syncPool(ctx context.Context, poolName string) error {
	obj, exists, err := pc.gatewayPoolInformer.GetStore().GetByKey(poolName)
	if err != nil {
		return fmt.Errorf("failed to get gateway pool %s: %w", poolName, err)
	}

	if !exists {
		return nil
	}

	poolUnstr, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("unexpected object type for gateway pool %s", poolName)
	}

	pool, err := parseGatewayPoolPeering(poolUnstr)
	if err != nil {
		return fmt.Errorf("failed to parse gateway pool %s: %w", poolName, err)
	}

	sitePeerings, err := pc.listSitePeerings()
	if err != nil {
		return err
	}

	assignments, err := pc.listSiteGatewayPoolAssignments()
	if err != nil {
		return err
	}

	poolPeerings, err := pc.listGatewayPoolPeerings()
	if err != nil {
		return err
	}

	sites, err := pc.listSites()
	if err != nil {
		return err
	}

	pools, err := pc.listPools()
	if err != nil {
		return err
	}

	siteMap := make(map[string]*unboundednetv1alpha1.Site, len(sites))
	for i := range sites {
		site := &sites[i]
		siteMap[site.Name] = site
	}

	poolMap := make(map[string]*unboundednetv1alpha1.GatewayPool, len(pools))
	for i := range pools {
		p := &pools[i]
		poolMap[p.Name] = p
	}

	connectedSites := map[string]map[string]struct{}{}
	poolAdjacency := map[string]map[string]struct{}{}

	for _, assignment := range assignments {
		if len(assignment.Spec.GatewayPools) == 0 {
			continue
		}

		for _, poolName := range assignment.Spec.GatewayPools {
			if _, ok := poolMap[poolName]; !ok {
				continue
			}

			siteSet := connectedSites[poolName]
			if siteSet == nil {
				siteSet = make(map[string]struct{})
				connectedSites[poolName] = siteSet
			}

			for _, siteName := range assignment.Spec.Sites {
				if _, ok := siteMap[siteName]; ok {
					siteSet[siteName] = struct{}{}
				}
			}
		}
	}

	for _, peering := range poolPeerings {
		if len(peering.Spec.GatewayPools) > 1 {
			for i := 0; i < len(peering.Spec.GatewayPools); i++ {
				left := peering.Spec.GatewayPools[i]
				if _, ok := poolMap[left]; !ok {
					continue
				}

				adj := poolAdjacency[left]
				if adj == nil {
					adj = make(map[string]struct{})
					poolAdjacency[left] = adj
				}

				for j := 0; j < len(peering.Spec.GatewayPools); j++ {
					if i == j {
						continue
					}

					right := peering.Spec.GatewayPools[j]
					if _, ok := poolMap[right]; !ok {
						continue
					}

					adj[right] = struct{}{}
				}
			}
		}
	}

	for _, peering := range sitePeerings {
		if len(peering.Spec.Sites) == 0 {
			continue
		}

		for _, assignment := range assignments {
			if len(assignment.Spec.GatewayPools) == 0 || len(assignment.Spec.Sites) == 0 {
				continue
			}
			// If assignment references any site in this SitePeering, treat all sites
			// in the same SitePeering as connected for these pools.
			intersects := false

			for _, assignmentSite := range assignment.Spec.Sites {
				for _, peeredSite := range peering.Spec.Sites {
					if assignmentSite == peeredSite {
						intersects = true
						break
					}
				}

				if intersects {
					break
				}
			}

			if !intersects {
				continue
			}

			for _, poolName := range assignment.Spec.GatewayPools {
				if _, ok := poolMap[poolName]; !ok {
					continue
				}

				siteSet := connectedSites[poolName]
				if siteSet == nil {
					siteSet = make(map[string]struct{})
					connectedSites[poolName] = siteSet
				}

				for _, peeredSite := range peering.Spec.Sites {
					if _, ok := siteMap[peeredSite]; ok {
						siteSet[peeredSite] = struct{}{}
					}
				}
			}
		}
	}

	reachableSites, _ := computeReachable(pool.Name, poolMap, siteMap, connectedSites, poolAdjacency)
	GatewayPoolReachableSitesGauge.WithLabelValues(pool.Name).Set(float64(len(reachableSites.reachable)))

	// Reconcile SitePeering statuses alongside pool updates since we
	// already have the site/peering data loaded.
	if err := pc.reconcileSitePeeringStatuses(ctx, sitePeerings, siteMap); err != nil {
		klog.Warningf("Failed to reconcile SitePeering statuses: %v", err)
	}

	if equalStringSlices(pool.Status.ConnectedSites, reachableSites.connected) &&
		equalStringSlices(pool.Status.ReachableSites, reachableSites.reachable) {
		return nil
	}

	statusPatch := map[string]interface{}{
		"connectedSites": reachableSites.connected,
		"reachableSites": reachableSites.reachable,
	}

	patchBytes, err := json.Marshal(map[string]interface{}{"status": statusPatch})
	if err != nil {
		return fmt.Errorf("failed to marshal status patch: %w", err)
	}

	_, err = pc.dynamicClient.Resource(gatewayPoolGVRPeering).Patch(
		ctx,
		poolName,
		types.MergePatchType,
		patchBytes,
		metav1.PatchOptions{},
		"status",
	)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		}

		return err
	}

	return nil
}

// reconcileSitePeeringStatuses computes and patches the status for each
// SitePeering. peeredSiteCount is the number of referenced sites that
// actually exist, totalNodeCount is the sum of those sites' nodeCount.
func (pc *PeeringAggregationController) reconcileSitePeeringStatuses(ctx context.Context, peerings []unboundednetv1alpha1.SitePeering, siteMap map[string]*unboundednetv1alpha1.Site) error {
	for _, peering := range peerings {
		peeredSiteCount := 0
		totalNodeCount := 0

		for _, siteName := range peering.Spec.Sites {
			if site, ok := siteMap[siteName]; ok {
				peeredSiteCount++
				totalNodeCount += site.Status.NodeCount
			}
		}

		if peering.Status.PeeredSiteCount == peeredSiteCount &&
			peering.Status.TotalNodeCount == totalNodeCount {
			continue
		}

		statusPatch := map[string]interface{}{
			"peeredSiteCount": peeredSiteCount,
			"totalNodeCount":  totalNodeCount,
		}

		patchBytes, err := json.Marshal(map[string]interface{}{"status": statusPatch})
		if err != nil {
			klog.Warningf("Failed to marshal SitePeering %s status patch: %v", peering.Name, err)
			continue
		}

		_, err = pc.dynamicClient.Resource(sitePeeringGVR).Patch(
			ctx,
			peering.Name,
			types.MergePatchType,
			patchBytes,
			metav1.PatchOptions{},
			"status",
		)
		if err != nil {
			if errors.IsNotFound(err) {
				continue
			}

			klog.Warningf("Failed to patch SitePeering %s status: %v", peering.Name, err)
		}
	}

	return nil
}

func computeReachableRoutes(startPool string, pools map[string]*unboundednetv1alpha1.GatewayPool, sites map[string]*unboundednetv1alpha1.Site, connectedSites, adjacency map[string]map[string]struct{}) []unboundednetv1alpha1.GatewayPoolRoute {
	_, routes := computeReachable(startPool, pools, sites, connectedSites, adjacency)
	return routes
}

type reachableSiteSets struct {
	connected []string
	reachable []string
}

type routeInfo struct {
	weight      int
	description string
	routeType   string
	origin      unboundednetv1alpha1.GatewayPoolRouteOrigin
}

func computeReachable(startPool string, pools map[string]*unboundednetv1alpha1.GatewayPool, sites map[string]*unboundednetv1alpha1.Site, connectedSites, adjacency map[string]map[string]struct{}) (reachableSiteSets, []unboundednetv1alpha1.GatewayPoolRoute) {
	connected := setToSortedSlice(connectedSites[startPool])

	minDepth := map[string]int{startPool: 1}

	queue := []string{startPool}
	for len(queue) > 0 {
		pool := queue[0]
		queue = queue[1:]

		for neighbor := range adjacency[pool] {
			if _, seen := minDepth[neighbor]; seen {
				continue
			}

			minDepth[neighbor] = minDepth[pool] + 1
			queue = append(queue, neighbor)
		}
	}

	reachableSiteSet := make(map[string]struct{})
	routeInfoByCIDR := make(map[string]routeInfo)

	for poolName, depth := range minDepth {
		weight := depth * defaultPoolWeight

		pool := pools[poolName]
		if pool == nil {
			continue
		}

		for _, cidr := range pool.Spec.RoutedCidrs {
			description := fmt.Sprintf("routed CIDR from gateway pool %s", poolName)
			setRouteInfo(routeInfoByCIDR, cidr, description, routeTypeRoutedCIDR, unboundednetv1alpha1.GatewayPoolRouteOrigin{GatewayPool: poolName}, weight)
		}

		for siteName := range connectedSites[poolName] {
			site := sites[siteName]
			if site == nil {
				continue
			}

			reachableSiteSet[siteName] = struct{}{}
			for _, cidr := range site.Spec.NodeCidrs {
				description := fmt.Sprintf("node CIDR from site %s", siteName)
				setRouteInfo(routeInfoByCIDR, cidr, description, routeTypeNodeCIDR, unboundednetv1alpha1.GatewayPoolRouteOrigin{Site: siteName}, weight)
			}

			for _, assignment := range site.Spec.PodCidrAssignments {
				for _, cidr := range assignment.CidrBlocks {
					description := fmt.Sprintf("pod CIDR from site %s", siteName)
					setRouteInfo(routeInfoByCIDR, cidr, description, routeTypePodCIDR, unboundednetv1alpha1.GatewayPoolRouteOrigin{Site: siteName}, weight)
				}
			}
		}
	}

	reachableSites := setToSortedSlice(reachableSiteSet)

	routes := make([]unboundednetv1alpha1.GatewayPoolRoute, 0, len(routeInfoByCIDR))
	for cidr, info := range routeInfoByCIDR {
		routes = append(routes, unboundednetv1alpha1.GatewayPoolRoute{CIDR: cidr, Weight: info.weight, Type: info.routeType, Origin: info.origin, Description: info.description})
	}

	sort.Slice(routes, func(i, j int) bool {
		if routes[i].CIDR == routes[j].CIDR {
			if routes[i].Weight == routes[j].Weight {
				if routes[i].Type == routes[j].Type {
					if routes[i].Origin.Site == routes[j].Origin.Site {
						if routes[i].Origin.GatewayPool == routes[j].Origin.GatewayPool {
							return routes[i].Description < routes[j].Description
						}

						return routes[i].Origin.GatewayPool < routes[j].Origin.GatewayPool
					}

					return routes[i].Origin.Site < routes[j].Origin.Site
				}

				return routes[i].Type < routes[j].Type
			}

			return routes[i].Weight < routes[j].Weight
		}

		return routes[i].CIDR < routes[j].CIDR
	})

	return reachableSiteSets{connected: connected, reachable: reachableSites}, routes
}

func setRouteInfo(routeInfoByCIDR map[string]routeInfo, cidr, description, routeType string, origin unboundednetv1alpha1.GatewayPoolRouteOrigin, weight int) {
	current, ok := routeInfoByCIDR[cidr]
	if !ok || weight < current.weight || (weight == current.weight && routeInfoKey(routeType, origin, description) < routeInfoKey(current.routeType, current.origin, current.description)) {
		routeInfoByCIDR[cidr] = routeInfo{weight: weight, description: description, routeType: routeType, origin: origin}
	}
}

func routeInfoKey(routeType string, origin unboundednetv1alpha1.GatewayPoolRouteOrigin, description string) string {
	return routeType + "|" + origin.Site + "|" + origin.GatewayPool + "|" + description
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

func setToSortedSlice(set map[string]struct{}) []string {
	if len(set) == 0 {
		return []string{}
	}

	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}

	sort.Strings(out)

	return out
}

func (pc *PeeringAggregationController) listSitePeerings() ([]unboundednetv1alpha1.SitePeering, error) {
	items := pc.sitePeeringInformer.GetStore().List()

	peerings := make([]unboundednetv1alpha1.SitePeering, 0, len(items))
	for _, item := range items {
		unstr, ok := item.(*unstructured.Unstructured)
		if !ok {
			continue
		}

		peering, err := parseSitePeering(unstr)
		if err != nil {
			klog.Warningf("Failed to parse SitePeering: %v", err)
			continue
		}

		if !unboundednetv1alpha1.SpecEnabled(peering.Spec.Enabled) {
			continue
		}

		peerings = append(peerings, *peering)
	}

	return peerings, nil
}

func (pc *PeeringAggregationController) listSiteGatewayPoolAssignments() ([]unboundednetv1alpha1.SiteGatewayPoolAssignment, error) {
	items := pc.assignmentInformer.GetStore().List()

	assignments := make([]unboundednetv1alpha1.SiteGatewayPoolAssignment, 0, len(items))
	for _, item := range items {
		unstr, ok := item.(*unstructured.Unstructured)
		if !ok {
			continue
		}

		assignment, err := parseSiteGatewayPoolAssignment(unstr)
		if err != nil {
			klog.Warningf("Failed to parse SiteGatewayPoolAssignment: %v", err)
			continue
		}

		if !unboundednetv1alpha1.SpecEnabled(assignment.Spec.Enabled) {
			continue
		}

		assignments = append(assignments, *assignment)
	}

	return assignments, nil
}

func (pc *PeeringAggregationController) listGatewayPoolPeerings() ([]unboundednetv1alpha1.GatewayPoolPeering, error) {
	items := pc.poolPeeringInformer.GetStore().List()

	peerings := make([]unboundednetv1alpha1.GatewayPoolPeering, 0, len(items))
	for _, item := range items {
		unstr, ok := item.(*unstructured.Unstructured)
		if !ok {
			continue
		}

		peering, err := parseGatewayPoolPeeringObj(unstr)
		if err != nil {
			klog.Warningf("Failed to parse GatewayPoolPeering: %v", err)
			continue
		}

		if !unboundednetv1alpha1.SpecEnabled(peering.Spec.Enabled) {
			continue
		}

		peerings = append(peerings, *peering)
	}

	return peerings, nil
}

func (pc *PeeringAggregationController) listSites() ([]unboundednetv1alpha1.Site, error) {
	items := pc.siteInformer.GetStore().List()

	sites := make([]unboundednetv1alpha1.Site, 0, len(items))
	for _, item := range items {
		unstr, ok := item.(*unstructured.Unstructured)
		if !ok {
			continue
		}

		site, err := parseSiteAggregation(unstr)
		if err != nil {
			klog.Warningf("Failed to parse Site: %v", err)
			continue
		}

		sites = append(sites, *site)
	}

	return sites, nil
}

func (pc *PeeringAggregationController) listPools() ([]unboundednetv1alpha1.GatewayPool, error) {
	items := pc.gatewayPoolInformer.GetStore().List()

	pools := make([]unboundednetv1alpha1.GatewayPool, 0, len(items))
	for _, item := range items {
		unstr, ok := item.(*unstructured.Unstructured)
		if !ok {
			continue
		}

		pool, err := parseGatewayPoolPeering(unstr)
		if err != nil {
			klog.Warningf("Failed to parse GatewayPool: %v", err)
			continue
		}

		pools = append(pools, *pool)
	}

	return pools, nil
}

func parseSitePeering(obj *unstructured.Unstructured) (*unboundednetv1alpha1.SitePeering, error) {
	data, err := obj.MarshalJSON()
	if err != nil {
		return nil, err
	}

	var peering unboundednetv1alpha1.SitePeering
	if err := json.Unmarshal(data, &peering); err != nil {
		return nil, err
	}

	return &peering, nil
}

func parseSiteGatewayPoolAssignment(obj *unstructured.Unstructured) (*unboundednetv1alpha1.SiteGatewayPoolAssignment, error) {
	data, err := obj.MarshalJSON()
	if err != nil {
		return nil, err
	}

	var assignment unboundednetv1alpha1.SiteGatewayPoolAssignment
	if err := json.Unmarshal(data, &assignment); err != nil {
		return nil, err
	}

	return &assignment, nil
}

func parseGatewayPoolPeeringObj(obj *unstructured.Unstructured) (*unboundednetv1alpha1.GatewayPoolPeering, error) {
	data, err := obj.MarshalJSON()
	if err != nil {
		return nil, err
	}

	var peering unboundednetv1alpha1.GatewayPoolPeering
	if err := json.Unmarshal(data, &peering); err != nil {
		return nil, err
	}

	return &peering, nil
}

func parseSiteAggregation(obj *unstructured.Unstructured) (*unboundednetv1alpha1.Site, error) {
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

func parseGatewayPoolPeering(obj *unstructured.Unstructured) (*unboundednetv1alpha1.GatewayPool, error) {
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
