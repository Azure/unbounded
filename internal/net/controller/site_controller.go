// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package controller implements site labeling and pod CIDR assignment.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"

	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
	"github.com/Azure/unbounded/internal/net/allocator"
)

const (
	// SiteLabelKey is the label key used to identify which site a node belongs to
	SiteLabelKey = "net.unbounded-kube.io/site"

	// WireGuardPubKeyAnnotation is the annotation key for a node's WireGuard public key
	WireGuardPubKeyAnnotation = "net.unbounded-kube.io/wg-pubkey"

	// WireGuardPortAnnotation is the annotation key for a gateway node's
	// assigned WireGuard port (used for gateway-to-gateway peering).
	WireGuardPortAnnotation = "net.unbounded-kube.io/wireguard-port"

	// TunnelMTUAnnotation is the annotation key for a node's detected
	// maximum tunnel MTU (default-route MTU minus encapsulation
	// overhead). The controller compares this against the configured node MTU
	// to surface warnings when the configured value is too high.
	TunnelMTUAnnotation = "net.unbounded-kube.io/tunnel-mtu"

	// ProtectionFinalizer prevents deletion of Sites and GatewayPools that
	// still have active nodes assigned. The controller adds this finalizer
	// when nodes are present and removes it when the last node is unassigned.
	ProtectionFinalizer = "net.unbounded-kube.io/protection"
)

var siteGVR = schema.GroupVersionResource{
	Group:    "net.unbounded-kube.io",
	Version:  "v1alpha1",
	Resource: "sites",
}

var siteNodeSliceGVR = schema.GroupVersionResource{
	Group:    "net.unbounded-kube.io",
	Version:  "v1alpha1",
	Resource: "sitenodeslices",
}

var gatewayPoolGVRSite = schema.GroupVersionResource{
	Group:    "net.unbounded-kube.io",
	Version:  "v1alpha1",
	Resource: "gatewaypools",
}

type assignmentAllocator struct {
	siteName        string
	assignmentIndex int
	assignment      unboundednetv1alpha1.PodCidrAssignment
	allocator       *allocator.Allocator
	nodeRegexes     []*regexp.Regexp
}

// SiteController manages site labeling and pod CIDR assignment for nodes.
type SiteController struct {
	clientset     kubernetes.Interface
	dynamicClient dynamic.Interface

	nodeLister corev1listers.NodeLister
	nodeSynced cache.InformerSynced

	siteInformer cache.SharedIndexInformer
	siteSynced   cache.InformerSynced

	sliceInformer cache.SharedIndexInformer
	sliceSynced   cache.InformerSynced

	gatewayPoolInformer cache.SharedIndexInformer
	gatewayPoolSynced   cache.InformerSynced

	// workqueue for node reconciliation
	workqueue workqueue.TypedRateLimitingInterface[string]

	// Cache of sites for faster lookups
	sitesCache     []unboundednetv1alpha1.Site
	sitesCacheLock sync.RWMutex

	// Cache of gateway pools for filtering gateway nodes
	gatewayPoolsCache     []unboundednetv1alpha1.GatewayPool
	gatewayPoolsCacheLock sync.RWMutex

	// Allocators for site pod CIDR assignments
	assignmentAllocators     map[string]*assignmentAllocator
	assignmentAllocatorsLock sync.RWMutex

	// Tracks nodes that have internal IPs but no matching site, to log once
	loggedNoSiteNodes     map[string]struct{}
	loggedNoSiteNodesLock sync.Mutex

	// Mutex to prevent concurrent slice updates
	sliceUpdateLock sync.Mutex

	// slicesDirty is set when syncNode changes node state and cleared when
	// updateAllSiteSlices runs. The periodic loop checks this to coalesce
	// slice updates instead of rebuilding after every single node sync.
	slicesDirty atomic.Bool

	// hasSynced indicates whether the informer caches have completed initial sync
	hasSynced atomic.Bool

	// allocatorsReady is set after assignment allocators have been built and
	// seeded with all existing node CIDRs. Pod CIDR allocation is blocked
	// until this flag is true to prevent assigning CIDRs that are already in
	// use by other nodes.
	allocatorsReady atomic.Bool

	// Tracks last duplicate podCIDR report to avoid repetitive log spam
	duplicatePodCIDRReport     string
	duplicatePodCIDRReportLock sync.Mutex
}

// NewSiteController creates a new site controller.
func NewSiteController(
	clientset kubernetes.Interface,
	dynamicClient dynamic.Interface,
	dynamicInformerFactory dynamicinformer.DynamicSharedInformerFactory,
	nodeInformerFactory informers.SharedInformerFactory,
) (*SiteController, error) {
	siteInformer := dynamicInformerFactory.ForResource(siteGVR).Informer()
	sliceInformer := dynamicInformerFactory.ForResource(siteNodeSliceGVR).Informer()
	gatewayPoolInformer := dynamicInformerFactory.ForResource(gatewayPoolGVRSite).Informer()

	nodeInformer := nodeInformerFactory.Core().V1().Nodes()

	sc := &SiteController{
		clientset:            clientset,
		dynamicClient:        dynamicClient,
		nodeLister:           nodeInformer.Lister(),
		nodeSynced:           nodeInformer.Informer().HasSynced,
		siteInformer:         siteInformer,
		siteSynced:           siteInformer.HasSynced,
		sliceInformer:        sliceInformer,
		sliceSynced:          sliceInformer.HasSynced,
		gatewayPoolInformer:  gatewayPoolInformer,
		gatewayPoolSynced:    gatewayPoolInformer.HasSynced,
		workqueue:            workqueue.NewTypedRateLimitingQueueWithConfig(workqueue.DefaultTypedControllerRateLimiter[string](), workqueue.TypedRateLimitingQueueConfig[string]{Name: "Sites"}),
		assignmentAllocators: make(map[string]*assignmentAllocator),
		loggedNoSiteNodes:    make(map[string]struct{}),
	}

	// Set up event handlers for nodes
	if _, err := nodeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			sc.enqueueNode(obj)
		},
		UpdateFunc: func(old, new interface{}) {
			oldNode := old.(*corev1.Node) //nolint:errcheck
			newNode := new.(*corev1.Node) //nolint:errcheck
			// Re-process if addresses changed, site label changed, WireGuard pubkey changed, or podCIDRs changed
			if !nodeAddressesEqual(oldNode, newNode) ||
				getNodeSiteLabel(oldNode) != getNodeSiteLabel(newNode) ||
				getNodeAnnotation(oldNode, WireGuardPubKeyAnnotation) != getNodeAnnotation(newNode, WireGuardPubKeyAnnotation) ||
				!labels.Equals(labels.Set(oldNode.Labels), labels.Set(newNode.Labels)) ||
				oldNode.Spec.PodCIDR != newNode.Spec.PodCIDR ||
				!stringSlicesEqual(oldNode.Spec.PodCIDRs, newNode.Spec.PodCIDRs) {
				sc.enqueueNode(new)
			}
		},
		DeleteFunc: func(obj interface{}) {
			var node *corev1.Node

			switch t := obj.(type) {
			case *corev1.Node:
				node = t
			case cache.DeletedFinalStateUnknown:
				var ok bool

				node, ok = t.Obj.(*corev1.Node)
				if !ok {
					klog.Errorf("DeletedFinalStateUnknown contained non-Node object: %#v", t.Obj)
					sc.enqueueSiteChange()

					return
				}
			default:
				klog.Errorf("Delete event contained non-Node object: %#v", obj)
				sc.enqueueSiteChange()

				return
			}

			sc.releaseNodeCIDRs(node)
			sc.markSlicesDirty()
			sc.enqueueSiteChange()
		},
	}); err != nil {
		return nil, fmt.Errorf("failed to add node event handler: %w", err)
	}

	// Set up event handlers for sites
	if _, err := siteInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			sc.enqueueSiteChange()
		},
		UpdateFunc: func(old, new interface{}) {
			sc.enqueueSiteChange()
		},
		DeleteFunc: func(obj interface{}) {
			// SiteNodeSlices will be garbage collected via ownerReferences
			sc.enqueueSiteChange()
		},
	}); err != nil {
		return nil, fmt.Errorf("failed to add site event handler: %w", err)
	}

	// Set up event handlers for gateway pools
	// When gateway pool selectors change, we need to re-evaluate which nodes are gateways
	if _, err := gatewayPoolInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			sc.updateGatewayPoolsCache()
			sc.enqueueSiteChange()
		},
		UpdateFunc: func(old, new interface{}) {
			sc.updateGatewayPoolsCache()
			sc.enqueueSiteChange()
		},
		DeleteFunc: func(obj interface{}) {
			sc.updateGatewayPoolsCache()
			sc.enqueueSiteChange()
		},
	}); err != nil {
		return nil, fmt.Errorf("failed to add gateway pool event handler: %w", err)
	}

	return sc, nil
}

// GetNodeLister returns the node lister for use by other components.
func (sc *SiteController) GetNodeLister() corev1listers.NodeLister {
	return sc.nodeLister
}

// GetSiteInformer returns the site informer for use by other components.
func (sc *SiteController) GetSiteInformer() cache.SharedIndexInformer {
	return sc.siteInformer
}

// AssignmentAllocatorDebugState contains debug info for one assignment allocator.
type AssignmentAllocatorDebugState struct {
	Key             string                        `json:"key"`
	SiteName        string                        `json:"siteName"`
	AssignmentIndex int                           `json:"assignmentIndex"`
	CidrBlocks      []string                      `json:"cidrBlocks"`
	NodeRegexes     []string                      `json:"nodeRegexes,omitempty"`
	Allocator       allocator.AllocatorDebugState `json:"allocator"`
}

// SiteControllerDebugState contains debug info for the site controller.
type SiteControllerDebugState struct {
	HasSynced       bool                            `json:"hasSynced"`
	AllocatorsReady bool                            `json:"allocatorsReady"`
	WorkqueueLength int                             `json:"workqueueLength"`
	SiteCount       int                             `json:"siteCount"`
	Allocators      []AssignmentAllocatorDebugState `json:"allocators"`
	InformerCounts  map[string]int                  `json:"informerCounts"`
}

// DebugState returns a snapshot of the site controller's internal state.
func (sc *SiteController) DebugState() SiteControllerDebugState {
	state := SiteControllerDebugState{
		HasSynced:       sc.hasSynced.Load(),
		AllocatorsReady: sc.allocatorsReady.Load(),
		WorkqueueLength: sc.workqueue.Len(),
		InformerCounts:  make(map[string]int),
	}

	sc.sitesCacheLock.RLock()
	state.SiteCount = len(sc.sitesCache)
	sc.sitesCacheLock.RUnlock()

	sc.assignmentAllocatorsLock.RLock()

	for key, aa := range sc.assignmentAllocators {
		entry := AssignmentAllocatorDebugState{
			Key:             key,
			SiteName:        aa.siteName,
			AssignmentIndex: aa.assignmentIndex,
			CidrBlocks:      aa.assignment.CidrBlocks,
			Allocator:       aa.allocator.DebugState(),
		}
		for _, re := range aa.nodeRegexes {
			entry.NodeRegexes = append(entry.NodeRegexes, re.String())
		}

		state.Allocators = append(state.Allocators, entry)
	}

	sc.assignmentAllocatorsLock.RUnlock()

	if sc.siteInformer != nil {
		state.InformerCounts["sites"] = len(sc.siteInformer.GetStore().List())
	}

	if sc.sliceInformer != nil {
		state.InformerCounts["siteNodeSlices"] = len(sc.sliceInformer.GetStore().List())
	}

	if sc.gatewayPoolInformer != nil {
		state.InformerCounts["gatewayPools"] = len(sc.gatewayPoolInformer.GetStore().List())
	}

	nodes, err := sc.nodeLister.List(labels.Everything())
	if err == nil {
		state.InformerCounts["nodes"] = len(nodes)
	}

	return state
}

// enqueueNode adds a node to the workqueue
func (sc *SiteController) enqueueNode(obj interface{}) {
	var (
		key string
		err error
	)

	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		return
	}

	sc.workqueue.Add(key)
}

// enqueueSiteChange enqueues all nodes for reconciliation when a site changes
func (sc *SiteController) enqueueSiteChange() {
	// Update the sites cache
	sc.updateSitesCache()
	sc.markSlicesDirty()

	// Don't try to list nodes until caches have synced
	if !sc.hasSynced.Load() {
		klog.V(3).Info("Skipping node enqueue - caches not yet synced")
		return
	}

	// Enqueue all nodes for re-evaluation
	nodes, err := sc.nodeLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to list nodes for site change: %v", err)
		return
	}

	for _, node := range nodes {
		sc.enqueueNode(node)
	}
}

// updateSitesCache updates the cached list of sites from the informer
func (sc *SiteController) updateSitesCache() {
	items := sc.siteInformer.GetStore().List()
	sites := make([]unboundednetv1alpha1.Site, 0, len(items))

	for _, item := range items {
		unstr, ok := item.(*unstructured.Unstructured)
		if !ok {
			continue
		}

		site := unboundednetv1alpha1.Site{}

		data, err := unstr.MarshalJSON()
		if err != nil {
			klog.Warningf("Failed to marshal site: %v", err)
			continue
		}

		if err := json.Unmarshal(data, &site); err != nil {
			klog.Warningf("Failed to unmarshal site: %v", err)
			continue
		}

		sites = append(sites, site)
	}

	sc.sitesCacheLock.Lock()
	sc.sitesCache = sites
	sc.sitesCacheLock.Unlock()

	sc.updateAssignmentAllocators(sites)

	// Validate that no sites have overlapping CIDRs
	if err := validateSiteCIDRsNoOverlap(sites); err != nil {
		klog.Fatalf("Site CIDR validation failed: %v", err)
	}

	klog.V(3).Infof("Updated sites cache: %d sites", len(sites))
}

// updateGatewayPoolsCache updates the cached list of gateway pools from the informer
func (sc *SiteController) updateGatewayPoolsCache() {
	items := sc.gatewayPoolInformer.GetStore().List()
	pools := make([]unboundednetv1alpha1.GatewayPool, 0, len(items))

	for _, item := range items {
		unstr, ok := item.(*unstructured.Unstructured)
		if !ok {
			continue
		}

		pool := unboundednetv1alpha1.GatewayPool{}

		data, err := unstr.MarshalJSON()
		if err != nil {
			klog.Warningf("Failed to marshal gateway pool: %v", err)
			continue
		}

		if err := json.Unmarshal(data, &pool); err != nil {
			klog.Warningf("Failed to unmarshal gateway pool: %v", err)
			continue
		}

		pools = append(pools, pool)
	}

	sc.gatewayPoolsCacheLock.Lock()
	sc.gatewayPoolsCache = pools
	sc.gatewayPoolsCacheLock.Unlock()

	klog.V(3).Infof("Updated gateway pools cache: %d pools", len(pools))
}

type assignmentRef struct {
	site       unboundednetv1alpha1.Site
	index      int
	assignment unboundednetv1alpha1.PodCidrAssignment
}

func assignmentKey(siteName string, assignmentIndex int) string {
	return fmt.Sprintf("%s/%d", siteName, assignmentIndex)
}

func assignmentEnabled(enabled *bool) bool {
	if enabled == nil {
		return true
	}

	return *enabled
}

func assignmentPriority(priority *int32) int32 {
	if priority == nil {
		return 100
	}

	return *priority
}

func assignmentMatchConfigEqual(a, b unboundednetv1alpha1.PodCidrAssignment) bool {
	if !stringSlicesEqual(a.NodeRegex, b.NodeRegex) {
		return false
	}

	return assignmentPriority(a.Priority) == assignmentPriority(b.Priority)
}

func (sc *SiteController) collectEnabledAssignments(sites []unboundednetv1alpha1.Site) []assignmentRef {
	enabled := make([]assignmentRef, 0)

	for _, site := range sites {
		for i, assignment := range site.Spec.PodCidrAssignments {
			if !assignmentEnabled(assignment.AssignmentEnabled) {
				continue
			}

			enabled = append(enabled, assignmentRef{site: site, index: i, assignment: assignment})
		}
	}

	return enabled
}

func (sc *SiteController) updateAssignmentAllocators(sites []unboundednetv1alpha1.Site) {
	enabledAssignments := sc.collectEnabledAssignments(sites)

	desired := make(map[string]assignmentRef, len(enabledAssignments))
	for _, ref := range enabledAssignments {
		desired[assignmentKey(ref.site.Name, ref.index)] = ref
	}

	keysToSeed := make(map[string]struct{})

	sc.assignmentAllocatorsLock.Lock()
	// Remove allocators for assignments that no longer exist
	for key := range sc.assignmentAllocators {
		if _, ok := desired[key]; !ok {
			klog.Infof("Assignment allocator %s: removing (assignment no longer exists)", key)
			delete(sc.assignmentAllocators, key)
		}
	}

	for key, ref := range desired {
		existing := sc.assignmentAllocators[key]
		if existing == nil {
			// New assignment -- needs a fresh allocator and seeding
			keysToSeed[key] = struct{}{}
			sc.assignmentAllocatorsLock.Unlock()
			state, err := sc.buildAssignmentAllocator(ref)
			sc.assignmentAllocatorsLock.Lock()
			if err != nil {
				klog.Errorf("Failed to build allocator for site %s assignment %d: %v", ref.site.Name, ref.index, err)
				continue
			}

			klog.Infof("Assignment allocator %s: created", key)
			sc.assignmentAllocators[key] = state

			continue
		}

		// Allocator exists -- update match config (regex/priority) in place.
		// Never replace the allocator; its allocated map is the source of truth.
		if !assignmentMatchConfigEqual(existing.assignment, ref.assignment) {
			nodeRegexes, err := compileNodeRegexes(ref.assignment.NodeRegex)
			if err != nil {
				klog.Errorf("Site %s assignment %d has invalid nodeRegex: %v", ref.site.Name, ref.index, err)
				continue
			}

			existing.nodeRegexes = nodeRegexes
		}
		// Update the stored assignment reference (keeps allocation config
		// comparison stable even if unrelated spec fields change)
		existing.assignment = ref.assignment
	}
	sc.assignmentAllocatorsLock.Unlock()

	if len(keysToSeed) == 0 {
		return
	}

	if err := sc.seedAllocatorsForNodes(keysToSeed); err != nil {
		klog.Errorf("Failed to seed assignment allocators: %v", err)
	}
}

func compileNodeRegexes(patterns []string) ([]*regexp.Regexp, error) {
	if len(patterns) == 0 {
		return nil, nil
	}

	regexes := make([]*regexp.Regexp, 0, len(patterns))
	for _, pattern := range patterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid node regex %q: %w", pattern, err)
		}

		regexes = append(regexes, re)
	}

	return regexes, nil
}

func splitCIDRBlocks(blocks []string) ([]*net.IPNet, []*net.IPNet, error) {
	var (
		ipv4Pools []*net.IPNet
		ipv6Pools []*net.IPNet
	)

	for _, block := range blocks {
		ip, ipNet, err := net.ParseCIDR(block)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid CIDR %q: %w", block, err)
		}

		if ip.To4() != nil {
			ipv4Pools = append(ipv4Pools, ipNet)
		} else {
			ipv6Pools = append(ipv6Pools, ipNet)
		}
	}

	return ipv4Pools, ipv6Pools, nil
}

func resolveMaskSizes(blockSizes *unboundednetv1alpha1.NodeBlockSizes, ipv4Pools, ipv6Pools []*net.IPNet) (int, int) {
	ipv4Mask := 0
	ipv6Mask := 0

	if blockSizes != nil {
		ipv4Mask = blockSizes.IPv4
		ipv6Mask = blockSizes.IPv6
	}

	if len(ipv4Pools) > 0 && ipv4Mask == 0 {
		ipv4Mask = 24
	}

	if len(ipv6Pools) > 0 && ipv6Mask == 0 {
		ones, _ := ipv6Pools[0].Mask.Size()

		ipv6Mask = ones + 16
		if ipv6Mask > 128 {
			ipv6Mask = 128
		}
	}

	return ipv4Mask, ipv6Mask
}

func (sc *SiteController) buildAssignmentAllocator(ref assignmentRef) (*assignmentAllocator, error) {
	ipv4Pools, ipv6Pools, err := splitCIDRBlocks(ref.assignment.CidrBlocks)
	if err != nil {
		return nil, err
	}

	if len(ipv4Pools) == 0 && len(ipv6Pools) == 0 {
		return nil, fmt.Errorf("no CIDR pools configured")
	}

	mask4, mask6 := resolveMaskSizes(ref.assignment.NodeBlockSizes, ipv4Pools, ipv6Pools)

	alloc, err := allocator.NewAllocator(ipv4Pools, ipv6Pools, mask4, mask6)
	if err != nil {
		return nil, err
	}

	nodeRegexes, err := compileNodeRegexes(ref.assignment.NodeRegex)
	if err != nil {
		return nil, err
	}

	return &assignmentAllocator{
		siteName:        ref.site.Name,
		assignmentIndex: ref.index,
		assignment:      ref.assignment,
		allocator:       alloc,
		nodeRegexes:     nodeRegexes,
	}, nil
}

func (sc *SiteController) seedAllocatorsForNodes(keysToSeed map[string]struct{}) error {
	nodes, err := sc.nodeLister.List(labels.Everything())
	if err != nil {
		return err
	}

	allocatedCIDRs := make(map[string]struct{})

	for _, node := range nodes {
		for _, cidr := range nodePodCIDRs(node) {
			allocatedCIDRs[cidr] = struct{}{}
		}
	}

	if len(allocatedCIDRs) == 0 {
		return nil
	}

	keys := make([]string, 0, len(keysToSeed))
	for key := range keysToSeed {
		keys = append(keys, key)
	}

	for _, key := range keys {
		sc.assignmentAllocatorsLock.RLock()
		state := sc.assignmentAllocators[key]
		sc.assignmentAllocatorsLock.RUnlock()

		if state == nil {
			continue
		}

		for cidr := range allocatedCIDRs {
			state.allocator.MarkAllocated(cidr)
		}
	}

	return nil
}

// isNodeGateway checks if a node matches any gateway pool's node selector.
// Gateway nodes should not be included in SiteNodeSlices because they are
// accessed via GatewayPool.Status.Nodes instead.
func (sc *SiteController) isNodeGateway(node *corev1.Node) bool {
	sc.gatewayPoolsCacheLock.RLock()
	pools := sc.gatewayPoolsCache
	sc.gatewayPoolsCacheLock.RUnlock()

	nodeLabels := labels.Set(node.Labels)

	for _, pool := range pools {
		selector := labels.SelectorFromSet(pool.Spec.NodeSelector)
		if selector.Matches(nodeLabels) {
			klog.V(3).Infof("Node %s matches GatewayPool %s selector - excluding from SiteNodeSlice", node.Name, pool.Name)
			return true
		}
	}

	return false
}

// Run starts the site controller
func (sc *SiteController) Run(ctx context.Context, workers int) error {
	defer utilruntime.HandleCrash()
	defer sc.workqueue.ShutDown()

	klog.Info("Starting site controller")

	// Wait for caches to sync
	klog.Info("Waiting for site controller informer caches to sync")

	if ok := cache.WaitForCacheSync(ctx.Done(), sc.nodeSynced, sc.siteSynced, sc.sliceSynced, sc.gatewayPoolSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	// Mark caches as synced so event handlers can now enqueue nodes
	sc.hasSynced.Store(true)

	// Initial cache update
	sc.updateSitesCache()
	sc.updateGatewayPoolsCache()

	// Do initial reconciliation of all nodes
	sc.reconcileAllNodes(ctx)
	sc.reportDuplicateNodePodCIDRs()
	sc.markSlicesDirty()

	// Mark allocators as ready now that all existing CIDRs have been seeded.
	// Pod CIDR allocation is blocked until this point.
	sc.allocatorsReady.Store(true)
	klog.Info("Pod CIDR allocators seeded and ready")

	klog.Info("Starting site controller workers")

	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, sc.runWorker, time.Second)
	}

	// Periodically update site statuses and slices when dirty or every 30s
	go wait.UntilWithContext(ctx, func(ctx context.Context) {
		sc.reportDuplicateNodePodCIDRs()

		if sc.slicesDirty.CompareAndSwap(true, false) {
			sc.updateAllSiteSlices(ctx)
		}
	}, 5*time.Second)

	klog.Info("Site controller started")
	<-ctx.Done()
	klog.Info("Shutting down site controller")

	return nil
}

// runWorker processes items from the workqueue
func (sc *SiteController) runWorker(ctx context.Context) {
	for sc.processNextWorkItem(ctx) {
	}
}

// processNextWorkItem processes a single item from the workqueue
func (sc *SiteController) processNextWorkItem(ctx context.Context) bool {
	key, shutdown := sc.workqueue.Get()
	if shutdown {
		return false
	}
	defer sc.workqueue.Done(key)

	start := time.Now()

	err := sc.syncNode(ctx, key)
	if err != nil {
		sc.workqueue.AddRateLimited(key)
		workqueueRetries.WithLabelValues("Sites").Inc()

		err = fmt.Errorf("error syncing node '%s': %s, requeuing", key, err.Error())
	} else {
		sc.workqueue.Forget(key)
	}

	duration := time.Since(start).Seconds()
	reconciliationDuration.WithLabelValues("Sites").Observe(duration)

	if err != nil {
		reconciliationErrors.WithLabelValues("Sites").Inc()
		reconciliationTotal.WithLabelValues("Sites", "error").Inc()
		utilruntime.HandleError(err)
	} else {
		reconciliationTotal.WithLabelValues("Sites", "success").Inc()
	}

	return true
}

// syncNode reconciles a single node's site label and pod CIDR assignment
func (sc *SiteController) syncNode(ctx context.Context, key string) error {
	node, err := sc.nodeLister.Get(key)
	if err != nil {
		// Node was deleted, nothing to do
		return nil
	}

	// Get current sites
	sc.sitesCacheLock.RLock()
	sites := sc.sitesCache
	sc.sitesCacheLock.RUnlock()

	// Find which site this node belongs to
	siteName := sc.findSiteForNode(node, sites)
	hasAssignedPodCIDRs := nodeHasPodCIDRs(node)

	internalIPs := getNodeInternalIPStrings(node)
	if siteName == "" && len(internalIPs) > 0 && !hasAssignedPodCIDRs {
		sc.logNoSiteMatchOnce(node.Name, internalIPs)
	}

	// Get current site label
	currentSite := getNodeSiteLabel(node)
	needsLabel := currentSite != siteName
	needsCIDRs := !hasAssignedPodCIDRs

	// If node needs both label and CIDRs, do them in a single combined patch
	if needsLabel && needsCIDRs && siteName != "" {
		if err := sc.assignPodCIDRsForNodeWithLabel(ctx, node, sites, siteName); err != nil {
			return err
		}

		sc.markSlicesDirty()

		return nil
	}

	// Update label if needed
	if needsLabel {
		var patchOps []map[string]interface{}
		if siteName != "" {
			patchOps = append(patchOps, map[string]interface{}{
				"op":    "add",
				"path":  "/metadata/labels/" + escapeJSONPointer(SiteLabelKey),
				"value": siteName,
			})
		} else if currentSite != "" {
			patchOps = append(patchOps, map[string]interface{}{
				"op":   "remove",
				"path": "/metadata/labels/" + escapeJSONPointer(SiteLabelKey),
			})
		}

		if len(patchOps) > 0 {
			patchData, err := json.Marshal(patchOps)
			if err != nil {
				return fmt.Errorf("failed to marshal patch: %w", err)
			}

			_, err = sc.clientset.CoreV1().Nodes().Patch(ctx, node.Name, types.JSONPatchType, patchData, metav1.PatchOptions{})
			if err != nil {
				return fmt.Errorf("failed to patch node: %w", err)
			}

			if siteName != "" {
				klog.Infof("Labeled node %s with site %s", node.Name, siteName)
			} else {
				klog.Infof("Removed site label from node %s", node.Name)
			}
		}

		sc.markSlicesDirty()
	}

	if needsCIDRs {
		if err := sc.assignPodCIDRsForNode(ctx, node, sites, siteName); err != nil {
			return err
		}
	} else if hasAssignedPodCIDRs && siteName != "" {
		// Node already has CIDRs -- ensure they are marked as allocated so the
		// allocator never hands them out to another node. This is needed because
		// the allocator may have been rebuilt since the initial seeding.
		sc.markNodeCIDRsAllocated(node, sites, siteName)
	}

	// Always mark slices dirty when a node has a site -- the slice content
	// includes the node's WireGuard public key and other fields that may
	// have changed without a label or CIDR change.
	if siteName != "" {
		sc.markSlicesDirty()
	}

	return nil
}

// reconcileAllNodes processes all nodes
func (sc *SiteController) reconcileAllNodes(_ context.Context) {
	nodes, err := sc.nodeLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to list nodes: %v", err)
		return
	}

	for _, node := range nodes {
		sc.enqueueNode(node)
	}
}

// updateAllSiteSlices updates the SiteNodeSlice objects for all sites
func (sc *SiteController) updateAllSiteSlices(ctx context.Context) {
	// Ensure only one slice update runs at a time to prevent conflicts
	sc.sliceUpdateLock.Lock()
	defer sc.sliceUpdateLock.Unlock()

	sc.sitesCacheLock.RLock()
	sites := sc.sitesCache
	sc.sitesCacheLock.RUnlock()

	if len(sites) == 0 {
		return
	}

	nodes, err := sc.nodeLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to list nodes for slice update: %v", err)
		return
	}

	// Track which nodes belong to each site
	// Gateway nodes are excluded - they are accessed via GatewayPool.Status.Nodes instead
	siteNodesInfo := make(map[string][]unboundednetv1alpha1.NodeInfo)

	for _, node := range nodes {
		// Skip gateway nodes - they should not be in SiteNodeSlices
		if sc.isNodeGateway(node) {
			continue
		}

		siteName := sc.findSiteForNode(node, sites)
		if siteName != "" {
			nodeInfo := sc.buildNodeInfo(node)
			siteNodesInfo[siteName] = append(siteNodesInfo[siteName], nodeInfo)
		}
	}

	// Count ALL nodes per site (including gateway nodes) for finalizer decisions.
	// Gateway nodes are excluded from SiteNodeSlices but still count as assigned
	// to a site for deletion-protection purposes.
	allSiteNodeCounts := make(map[string]int)

	for _, node := range nodes {
		siteName := sc.findSiteForNode(node, sites)
		if siteName != "" {
			allSiteNodeCounts[siteName]++
		}
	}

	// Update slices for each site
	for _, site := range sites {
		nodesInfo := siteNodesInfo[site.Name]
		// Sort by node name for consistent ordering
		sort.Slice(nodesInfo, func(i, j int) bool {
			return nodesInfo[i].Name < nodesInfo[j].Name
		})

		sc.updateSiteSlices(ctx, site, nodesInfo)
	}

	// Manage protection finalizers: add when nodes are assigned, remove when
	// no nodes remain so the site can be deleted.
	for _, site := range sites {
		nodeCount := allSiteNodeCounts[site.Name]
		hasFinalizer := containsFinalizer(site.Finalizers, ProtectionFinalizer)

		if nodeCount > 0 && !hasFinalizer {
			if err := ensureFinalizer(ctx, sc.dynamicClient, siteGVR, site.Name, site.Finalizers); err != nil {
				klog.Warningf("Failed to add protection finalizer to site %s: %v", site.Name, err)
			} else {
				klog.V(2).Infof("Added protection finalizer to site %s (%d nodes)", site.Name, nodeCount)
			}
		} else if nodeCount == 0 && hasFinalizer {
			if err := removeFinalizer(ctx, sc.dynamicClient, siteGVR, site.Name, site.Finalizers); err != nil {
				klog.Warningf("Failed to remove protection finalizer from site %s: %v", site.Name, err)
			} else {
				klog.V(2).Infof("Removed protection finalizer from site %s (no nodes)", site.Name)
			}
		}
	}
}

// updateSiteSlices creates/updates/deletes SiteNodeSlice objects for a site
func (sc *SiteController) updateSiteSlices(ctx context.Context, site unboundednetv1alpha1.Site, nodesInfo []unboundednetv1alpha1.NodeInfo) {
	// Calculate how many slices we need
	numSlices := (len(nodesInfo) + unboundednetv1alpha1.MaxNodesPerSlice - 1) / unboundednetv1alpha1.MaxNodesPerSlice
	if numSlices == 0 {
		numSlices = 0 // No nodes, no slices needed
	}

	// Get existing slices for this site
	existingSlices := sc.getExistingSlices(site.Name)

	// Create or update slices
	for i := 0; i < numSlices; i++ {
		start := i * unboundednetv1alpha1.MaxNodesPerSlice

		end := start + unboundednetv1alpha1.MaxNodesPerSlice
		if end > len(nodesInfo) {
			end = len(nodesInfo)
		}

		sliceNodes := nodesInfo[start:end]

		sc.createOrUpdateSlice(ctx, site, i, sliceNodes)
	}

	// Delete extra slices
	for i := numSlices; i < len(existingSlices); i++ {
		sliceName := fmt.Sprintf("%s-%d", site.Name, i)

		err := sc.dynamicClient.Resource(siteNodeSliceGVR).Delete(ctx, sliceName, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			klog.Errorf("Failed to delete extra slice %s: %v", sliceName, err)
		} else {
			klog.V(2).Infof("Deleted extra slice %s", sliceName)
		}
	}

	// Update site status only if it changed
	sc.updateSiteStatusIfChanged(ctx, site, len(nodesInfo), numSlices)
}

// getExistingSlices returns the existing slices for a site
func (sc *SiteController) getExistingSlices(siteName string) []unboundednetv1alpha1.SiteNodeSlice {
	items := sc.sliceInformer.GetStore().List()

	var slices []unboundednetv1alpha1.SiteNodeSlice

	for _, item := range items {
		unstr, ok := item.(*unstructured.Unstructured)
		if !ok {
			continue
		}

		sliceSiteName, found, _ := unstructured.NestedString(unstr.Object, "siteName") //nolint:errcheck
		if !found || sliceSiteName != siteName {
			continue
		}

		slice := unboundednetv1alpha1.SiteNodeSlice{}

		data, err := unstr.MarshalJSON()
		if err != nil {
			continue
		}

		if err := json.Unmarshal(data, &slice); err != nil {
			continue
		}

		slices = append(slices, slice)
	}

	// Sort by slice index
	sort.Slice(slices, func(i, j int) bool {
		return slices[i].SliceIndex < slices[j].SliceIndex
	})

	return slices
}

// createOrUpdateSlice creates or updates a SiteNodeSlice with retry logic for conflicts
func (sc *SiteController) createOrUpdateSlice(ctx context.Context, site unboundednetv1alpha1.Site, sliceIndex int, nodes []unboundednetv1alpha1.NodeInfo) {
	sliceName := fmt.Sprintf("%s-%d", site.Name, sliceIndex)

	// Convert nodes to unstructured format
	nodesData := make([]interface{}, len(nodes))
	for i, ni := range nodes {
		nodeData := map[string]interface{}{
			"name": ni.Name,
		}
		if ni.WireGuardPublicKey != "" {
			nodeData["wireGuardPublicKey"] = ni.WireGuardPublicKey
		}

		if len(ni.InternalIPs) > 0 {
			nodeData["internalIPs"] = ni.InternalIPs
		}

		if len(ni.PodCIDRs) > 0 {
			nodeData["podCIDRs"] = ni.PodCIDRs
		}

		nodesData[i] = nodeData
	}

	// Retry logic for conflicts
	maxRetries := 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff: 100ms, 200ms, 400ms, 800ms
			backoff := time.Duration(100<<uint(attempt-1)) * time.Millisecond
			klog.V(2).Infof("Retrying slice %s update (attempt %d/%d) after %v", sliceName, attempt+1, maxRetries, backoff)
			time.Sleep(backoff)
		}

		// Check if slice exists using informer cache
		existingObj, exists, err := sc.sliceInformer.GetStore().GetByKey(sliceName)
		if err != nil {
			klog.Errorf("Failed to get slice %s from cache: %v", sliceName, err)
			return
		}

		if !exists {
			// Create new slice
			sliceObj := sc.buildSliceObject(site, sliceName, sliceIndex, nodesData)

			_, err = sc.dynamicClient.Resource(siteNodeSliceGVR).Create(ctx, sliceObj, metav1.CreateOptions{})
			if err != nil {
				if apierrors.IsAlreadyExists(err) {
					// Race condition: slice was created by another process, retry to update it
					klog.V(2).Infof("Slice %s was created by another process, retrying", sliceName)
					continue
				}

				klog.Errorf("Failed to create slice %s: %v", sliceName, err)
			} else {
				klog.V(2).Infof("Created slice %s with %d nodes", sliceName, len(nodes))
			}

			return
		}

		existing := existingObj.(*unstructured.Unstructured) //nolint:errcheck

		// Check if update is needed
		existingNodes, _, _ := unstructured.NestedSlice(existing.Object, "nodes") //nolint:errcheck

		existingNodeCount, foundNodeCount, _ := unstructured.NestedInt64(existing.Object, "nodeCount") //nolint:errcheck
		if sc.sliceNodesEqual(existingNodes, nodesData) && foundNodeCount && existingNodeCount == int64(len(nodesData)) {
			return
		}

		// Update existing slice
		sliceObj := sc.buildSliceObject(site, sliceName, sliceIndex, nodesData)
		sliceObj.SetResourceVersion(existing.GetResourceVersion())

		_, err = sc.dynamicClient.Resource(siteNodeSliceGVR).Update(ctx, sliceObj, metav1.UpdateOptions{})
		if err != nil {
			if apierrors.IsConflict(err) {
				// Conflict: resource was modified, retry with fresh version
				klog.V(2).Infof("Conflict updating slice %s, will retry", sliceName)
				continue
			}

			klog.Errorf("Failed to update slice %s: %v", sliceName, err)

			return
		}

		klog.V(2).Infof("Updated slice %s with %d nodes", sliceName, len(nodes))

		return
	}

	klog.Errorf("Failed to update slice %s after %d retries", sliceName, maxRetries)
}

// buildSliceObject constructs the unstructured SiteNodeSlice object
func (sc *SiteController) buildSliceObject(site unboundednetv1alpha1.Site, sliceName string, sliceIndex int, nodesData []interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "net.unbounded-kube.io/v1alpha1",
			"kind":       "SiteNodeSlice",
			"metadata": map[string]interface{}{
				"name": sliceName,
				"ownerReferences": []interface{}{
					map[string]interface{}{
						"apiVersion":         "net.unbounded-kube.io/v1alpha1",
						"kind":               "Site",
						"name":               site.Name,
						"uid":                string(site.UID),
						"controller":         true,
						"blockOwnerDeletion": true,
					},
				},
			},
			"siteName":   site.Name,
			"sliceIndex": int64(sliceIndex),
			"nodes":      nodesData,
			"nodeCount":  int64(len(nodesData)),
		},
	}
}

// sliceNodesEqual compares two node lists for equality
// It normalizes the data before comparison to handle type differences
// (e.g., []interface{} from API vs []string from local build)
func (sc *SiteController) sliceNodesEqual(a, b []interface{}) bool {
	if len(a) != len(b) {
		klog.V(4).Infof("sliceNodesEqual: length mismatch %d vs %d", len(a), len(b))
		return false
	}

	// Normalize both slices to ensure consistent comparison
	aNorm := normalizeNodeSlice(a)
	bNorm := normalizeNodeSlice(b)

	aJSON, _ := json.Marshal(aNorm) //nolint:errcheck
	bJSON, _ := json.Marshal(bNorm) //nolint:errcheck

	equal := string(aJSON) == string(bJSON)
	if !equal {
		klog.V(4).Infof("sliceNodesEqual: nodes differ (existing=%d bytes, new=%d bytes)", len(aJSON), len(bJSON))
	}

	return equal
}

// normalizeNodeSlice converts a node slice to a canonical form for comparison.
// This handles type differences like []interface{} vs []string that occur when
// comparing data from the API with locally-built data.
func normalizeNodeSlice(nodes []interface{}) []map[string]interface{} {
	result := make([]map[string]interface{}, 0, len(nodes))
	for _, node := range nodes {
		nodeMap, ok := node.(map[string]interface{})
		if !ok {
			continue
		}

		normalized := make(map[string]interface{})
		for k, v := range nodeMap {
			normalized[k] = normalizeValue(v)
		}

		result = append(result, normalized)
	}

	return result
}

// normalizeValue converts values to a canonical form.
// Specifically, it converts []interface{} to []string for string arrays.
func normalizeValue(v interface{}) interface{} {
	switch val := v.(type) {
	case []interface{}:
		// Convert []interface{} to []string if all elements are strings
		strSlice := make([]string, 0, len(val))
		for _, elem := range val {
			if s, ok := elem.(string); ok {
				strSlice = append(strSlice, s)
			} else {
				// Not all strings, return as-is
				return val
			}
		}

		return strSlice
	case []string:
		// Already a string slice, return as-is
		return val
	default:
		return v
	}
}

// buildNodeInfo constructs a NodeInfo struct from a Node object
func (sc *SiteController) buildNodeInfo(node *corev1.Node) unboundednetv1alpha1.NodeInfo {
	info := unboundednetv1alpha1.NodeInfo{
		Name:     node.Name,
		PodCIDRs: node.Spec.PodCIDRs,
	}

	// Get WireGuard public key from annotation
	if node.Annotations != nil {
		info.WireGuardPublicKey = node.Annotations[WireGuardPubKeyAnnotation]
	}

	// Get internal IPs
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			info.InternalIPs = append(info.InternalIPs, addr.Address)
		}
	}

	return info
}

// updateSiteStatusIfChanged updates the status of a site only if it has changed
func (sc *SiteController) updateSiteStatusIfChanged(ctx context.Context, site unboundednetv1alpha1.Site, nodeCount, sliceCount int) {
	// Always update gauges so they reflect the latest state.
	SiteNodesGauge.WithLabelValues(site.Name).Set(float64(nodeCount))
	SiteNodeSlicesGauge.WithLabelValues(site.Name).Set(float64(sliceCount))

	// Check if status actually changed
	if site.Status.NodeCount == nodeCount && site.Status.SliceCount == sliceCount {
		klog.V(4).Infof("Site %s status unchanged (%d nodes, %d slices), skipping update", site.Name, nodeCount, sliceCount)
		return
	}

	statusPatch := map[string]interface{}{
		"status": map[string]interface{}{
			"nodeCount":  nodeCount,
			"sliceCount": sliceCount,
		},
	}

	patchData, err := json.Marshal(statusPatch)
	if err != nil {
		klog.Errorf("Failed to marshal status patch for site %s: %v", site.Name, err)
		return
	}

	// Retry logic for conflicts
	maxRetries := 3
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(50<<uint(attempt-1)) * time.Millisecond
			time.Sleep(backoff)
		}

		_, err = sc.dynamicClient.Resource(siteGVR).Patch(ctx, site.Name, types.MergePatchType, patchData, metav1.PatchOptions{}, "status")
		if err == nil {
			klog.Infof("Updated site %s status: %d nodes, %d slices", site.Name, nodeCount, sliceCount)
			return
		}

		if apierrors.IsConflict(err) {
			klog.V(2).Infof("Conflict updating site %s status, retrying", site.Name)
			continue
		}

		if apierrors.IsNotFound(err) {
			klog.V(2).Infof("Site %s no longer exists, skipping status update", site.Name)
			return
		}

		klog.Errorf("Failed to update status for site %s: %v", site.Name, err)

		return
	}

	klog.Errorf("Failed to update site %s status after %d retries", site.Name, maxRetries)
}

func (sc *SiteController) logNoSiteMatchOnce(nodeName string, internalIPs []string) {
	key := nodeName

	sc.loggedNoSiteNodesLock.Lock()
	defer sc.loggedNoSiteNodesLock.Unlock()

	if _, exists := sc.loggedNoSiteNodes[key]; exists {
		return
	}

	sc.loggedNoSiteNodes[key] = struct{}{}

	klog.Errorf("Node %s has internal IPs %v but does not match any Site; skipping pod CIDR assignment", nodeName, internalIPs)
}

func getNodeInternalIPStrings(node *corev1.Node) []string {
	internalIPs := make([]string, 0)

	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			internalIPs = append(internalIPs, addr.Address)
		}
	}

	return internalIPs
}

func nodeHasPodCIDRs(node *corev1.Node) bool {
	return node.Spec.PodCIDR != "" || len(node.Spec.PodCIDRs) > 0
}

func nodePodCIDRs(node *corev1.Node) []string {
	if node.Spec.PodCIDR == "" {
		return node.Spec.PodCIDRs
	}

	if len(node.Spec.PodCIDRs) == 0 {
		return []string{node.Spec.PodCIDR}
	}

	return node.Spec.PodCIDRs
}

func assignmentMatchesNode(state *assignmentAllocator, nodeName string) bool {
	if len(state.assignment.NodeRegex) == 0 {
		return true
	}

	for _, re := range state.nodeRegexes {
		if re.MatchString(nodeName) {
			return true
		}
	}

	return false
}

func (sc *SiteController) getAssignmentAllocator(siteName string, assignmentIndex int) *assignmentAllocator {
	key := assignmentKey(siteName, assignmentIndex)

	sc.assignmentAllocatorsLock.RLock()
	state := sc.assignmentAllocators[key]
	sc.assignmentAllocatorsLock.RUnlock()

	return state
}

func (sc *SiteController) selectAssignmentForNode(site unboundednetv1alpha1.Site, nodeName string) *assignmentAllocator {
	enabledAssignments := sc.collectEnabledAssignments([]unboundednetv1alpha1.Site{site})

	var (
		selected         *assignmentAllocator
		selectedPriority int32
		selectedIndex    int
	)

	for _, ref := range enabledAssignments {
		state := sc.getAssignmentAllocator(ref.site.Name, ref.index)
		if state == nil {
			continue
		}

		if !assignmentMatchesNode(state, nodeName) {
			continue
		}

		priority := assignmentPriority(ref.assignment.Priority)
		if selected == nil || priority < selectedPriority || (priority == selectedPriority && ref.index < selectedIndex) {
			selected = state
			selectedPriority = priority
			selectedIndex = ref.index
		}
	}

	return selected
}

func (sc *SiteController) assignPodCIDRsForNode(ctx context.Context, node *corev1.Node, sites []unboundednetv1alpha1.Site, siteName string) error {
	if !sc.hasSynced.Load() {
		return fmt.Errorf("informer caches not synced; refusing pod CIDR assignment for node %s", node.Name)
	}

	if !sc.allocatorsReady.Load() {
		return fmt.Errorf("allocators not yet seeded; refusing pod CIDR assignment for node %s", node.Name)
	}

	if siteName == "" {
		return nil
	}

	var site *unboundednetv1alpha1.Site

	for i := range sites {
		if sites[i].Name == siteName {
			site = &sites[i]
			break
		}
	}

	if site == nil {
		return nil
	}

	// When manageCniPlugin is false, pod CIDR assignment is disabled.
	// The CIDRs are still used for inter-site routing but nodes are not
	// assigned individual pod CIDRs by the controller.
	if site.Spec.ManageCniPlugin != nil && !*site.Spec.ManageCniPlugin {
		return nil
	}

	state := sc.selectAssignmentForNode(*site, node.Name)
	if state == nil {
		return nil
	}

	if nodeHasPodCIDRs(node) {
		for _, cidr := range nodePodCIDRs(node) {
			state.allocator.MarkAllocated(cidr)
		}

		return nil
	}

	freshNode, err := sc.nodeLister.Get(node.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		return err
	}

	if nodeHasPodCIDRs(freshNode) {
		for _, cidr := range nodePodCIDRs(freshNode) {
			state.allocator.MarkAllocated(cidr)
		}

		return nil
	}

	podCIDR, podCIDRs, err := sc.computePodCIDRsForNode(state)
	if err != nil {
		return err
	}

	return sc.patchNodeCIDRs(ctx, node.Name, podCIDR, podCIDRs)
}

func findDuplicateNodePodCIDRs(nodes []*corev1.Node) map[string][]string {
	ownersByCIDR := make(map[string][]string)

	for _, node := range nodes {
		if node == nil {
			continue
		}

		for _, cidr := range nodePodCIDRs(node) {
			ownersByCIDR[cidr] = append(ownersByCIDR[cidr], node.Name)
		}
	}

	conflicts := make(map[string][]string)

	for cidr, names := range ownersByCIDR {
		if len(names) < 2 {
			continue
		}

		sort.Strings(names)

		names = dedupeSortedStrings(names)
		if len(names) < 2 {
			continue
		}

		conflicts[cidr] = names
	}

	return conflicts
}

func (sc *SiteController) reportDuplicateNodePodCIDRs() {
	nodes, err := sc.nodeLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to list nodes for duplicate podCIDR audit: %v", err)
		return
	}

	currentReport := formatCIDRConflicts(findDuplicateNodePodCIDRs(nodes))

	sc.duplicatePodCIDRReportLock.Lock()

	previousReport := sc.duplicatePodCIDRReport
	if currentReport == previousReport {
		sc.duplicatePodCIDRReportLock.Unlock()
		return
	}

	sc.duplicatePodCIDRReport = currentReport
	sc.duplicatePodCIDRReportLock.Unlock()

	if currentReport == "" {
		if previousReport != "" {
			klog.Infof("Duplicate podCIDR audit: no conflicts detected")
		}

		return
	}

	klog.Warningf("Duplicate podCIDR audit detected conflicts: %s", currentReport)
}

func formatCIDRConflicts(conflicts map[string][]string) string {
	if len(conflicts) == 0 {
		return ""
	}

	keys := make([]string, 0, len(conflicts))
	for cidr := range conflicts {
		keys = append(keys, cidr)
	}

	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, cidr := range keys {
		names := append([]string(nil), conflicts[cidr]...)
		sort.Strings(names)
		parts = append(parts, fmt.Sprintf("%s -> [%s]", cidr, strings.Join(names, ",")))
	}

	return strings.Join(parts, "; ")
}

func dedupeSortedStrings(values []string) []string {
	if len(values) == 0 {
		return values
	}

	out := values[:1]
	for i := 1; i < len(values); i++ {
		if values[i] == values[i-1] {
			continue
		}

		out = append(out, values[i])
	}

	return out
}

func (sc *SiteController) computePodCIDRsForNode(state *assignmentAllocator) (string, []string, error) {
	var (
		podCIDR  string
		podCIDRs []string
	)

	if state.allocator.HasIPv4Pools() {
		ipv4CIDR, err := state.allocator.AllocateIPv4()
		if err != nil {
			if errors.Is(err, allocator.ErrPoolExhausted) {
				PodCIDRExhaustion.Inc()
				klog.Fatalf("IPv4 CIDR pool exhausted for site %s assignment %d", state.siteName, state.assignmentIndex)
			}

			return "", nil, err
		}

		podCIDR = ipv4CIDR
		podCIDRs = append(podCIDRs, ipv4CIDR)

		PodCIDRAllocations.Inc()
	}

	if state.allocator.HasIPv6Pools() {
		ipv6CIDR, err := state.allocator.AllocateIPv6()
		if err != nil {
			if errors.Is(err, allocator.ErrPoolExhausted) {
				PodCIDRExhaustion.Inc()
				klog.Fatalf("IPv6 CIDR pool exhausted for site %s assignment %d", state.siteName, state.assignmentIndex)
			}

			return "", nil, err
		}

		if podCIDR == "" {
			podCIDR = ipv6CIDR
		}

		podCIDRs = append(podCIDRs, ipv6CIDR)

		PodCIDRAllocations.Inc()
	}

	if len(podCIDRs) == 0 {
		return "", nil, fmt.Errorf("no CIDR pools configured for site %s assignment %d", state.siteName, state.assignmentIndex)
	}

	return podCIDR, podCIDRs, nil
}

func (sc *SiteController) patchNodeCIDRs(ctx context.Context, nodeName, podCIDR string, podCIDRs []string) error {
	podCIDRsJSON := "["

	for i, cidr := range podCIDRs {
		if i > 0 {
			podCIDRsJSON += ","
		}

		podCIDRsJSON += fmt.Sprintf("%q", cidr)
	}

	podCIDRsJSON += "]"

	patch := fmt.Sprintf(`{"spec":{"podCIDR":%q,"podCIDRs":%s}}`, podCIDR, podCIDRsJSON)

	_, err := sc.clientset.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	if err != nil {
		return err
	}

	klog.Infof("Assigned podCIDR=%s, podCIDRs=%v to node %s", podCIDR, podCIDRs, nodeName)

	return nil
}

// patchNodeLabelAndCIDRs applies both a site label and pod CIDRs in a single
// MergePatch to cut the number of API calls in half during scale-in.
func (sc *SiteController) patchNodeLabelAndCIDRs(ctx context.Context, nodeName, siteName, podCIDR string, podCIDRs []string) error {
	podCIDRsJSON := "["

	for i, cidr := range podCIDRs {
		if i > 0 {
			podCIDRsJSON += ","
		}

		podCIDRsJSON += fmt.Sprintf("%q", cidr)
	}

	podCIDRsJSON += "]"

	patch := fmt.Sprintf(`{"metadata":{"labels":{%q:%q}},"spec":{"podCIDR":%q,"podCIDRs":%s}}`,
		SiteLabelKey, siteName, podCIDR, podCIDRsJSON)

	_, err := sc.clientset.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	if err != nil {
		return err
	}

	klog.Infof("Labeled node %s with site %s and assigned podCIDR=%s, podCIDRs=%v", nodeName, siteName, podCIDR, podCIDRs)

	return nil
}

// assignPodCIDRsForNodeWithLabel combines site labeling and pod CIDR assignment
// into a single API call for new nodes that need both.
func (sc *SiteController) assignPodCIDRsForNodeWithLabel(ctx context.Context, node *corev1.Node, sites []unboundednetv1alpha1.Site, siteName string) error {
	if !sc.hasSynced.Load() {
		return fmt.Errorf("informer caches not synced; refusing pod CIDR assignment for node %s", node.Name)
	}

	if !sc.allocatorsReady.Load() {
		return fmt.Errorf("allocators not yet seeded; refusing pod CIDR assignment for node %s", node.Name)
	}

	if siteName == "" {
		return nil
	}

	var site *unboundednetv1alpha1.Site

	for i := range sites {
		if sites[i].Name == siteName {
			site = &sites[i]
			break
		}
	}

	if site == nil {
		return nil
	}

	state := sc.selectAssignmentForNode(*site, node.Name)
	if state == nil {
		return nil
	}

	if nodeHasPodCIDRs(node) {
		for _, cidr := range nodePodCIDRs(node) {
			state.allocator.MarkAllocated(cidr)
		}

		return nil
	}

	freshNode, err := sc.nodeLister.Get(node.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		return err
	}

	if nodeHasPodCIDRs(freshNode) {
		for _, cidr := range nodePodCIDRs(freshNode) {
			state.allocator.MarkAllocated(cidr)
		}

		return nil
	}

	podCIDR, podCIDRs, err := sc.computePodCIDRsForNode(state)
	if err != nil {
		return err
	}

	return sc.patchNodeLabelAndCIDRs(ctx, node.Name, siteName, podCIDR, podCIDRs)
}

// markSlicesDirty signals that SiteNodeSlice objects need rebuilding.
func (sc *SiteController) markSlicesDirty() {
	sc.slicesDirty.Store(true)
}

// markNodeCIDRsAllocated ensures a node's existing pod CIDRs are marked in the
// allocator so they are never handed out to another node. Called for nodes that
// already have CIDRs assigned (e.g., gateway and system nodes).
func (sc *SiteController) markNodeCIDRsAllocated(node *corev1.Node, sites []unboundednetv1alpha1.Site, siteName string) {
	var site *unboundednetv1alpha1.Site

	for i := range sites {
		if sites[i].Name == siteName {
			site = &sites[i]
			break
		}
	}

	if site == nil {
		return
	}

	state := sc.selectAssignmentForNode(*site, node.Name)
	if state == nil {
		return
	}

	for _, cidr := range nodePodCIDRs(node) {
		state.allocator.MarkAllocated(cidr)
	}
}

func (sc *SiteController) releaseNodeCIDRs(node *corev1.Node) {
	sc.sitesCacheLock.RLock()
	sites := sc.sitesCache
	sc.sitesCacheLock.RUnlock()

	siteName := sc.findSiteForNode(node, sites)
	if siteName == "" {
		return
	}

	var site *unboundednetv1alpha1.Site

	for i := range sites {
		if sites[i].Name == siteName {
			site = &sites[i]
			break
		}
	}

	if site == nil {
		return
	}

	state := sc.selectAssignmentForNode(*site, node.Name)
	if state == nil {
		return
	}

	// Only release CIDRs that no other node currently owns, to prevent
	// freeing a CIDR that was duplicated across nodes (e.g., a gateway
	// node and a user node assigned the same CIDR from different scale
	// cycles).
	nodeCIDRs := nodePodCIDRs(node)
	if len(nodeCIDRs) == 0 {
		return
	}

	otherNodes, err := sc.nodeLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to list nodes during CIDR release for %s: %v", node.Name, err)
		return
	}

	ownedByOthers := make(map[string]bool)

	for _, other := range otherNodes {
		if other.Name == node.Name {
			continue
		}

		for _, cidr := range nodePodCIDRs(other) {
			ownedByOthers[cidr] = true
		}
	}

	for _, cidr := range nodeCIDRs {
		if ownedByOthers[cidr] {
			klog.Warningf("Not releasing CIDR %s from deleted node %s: still assigned to another node", cidr, node.Name)
			continue
		}

		state.allocator.Release(cidr)
		PodCIDRReleases.Inc()
	}
}

// findSiteForNode returns the name of the site that contains the node's internal IP.
// Returns empty string if no site matches.
func (sc *SiteController) findSiteForNode(node *corev1.Node, sites []unboundednetv1alpha1.Site) string {
	// Get node's internal IPs
	var internalIPs []net.IP

	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			ip := net.ParseIP(addr.Address)
			if ip != nil {
				internalIPs = append(internalIPs, ip)
			}
		}
	}

	if len(internalIPs) == 0 {
		klog.V(3).Infof("Node %s has no internal IPs", node.Name)
		return ""
	}

	// Check each site
	for _, site := range sites {
		for _, cidrStr := range site.Spec.NodeCidrs {
			_, cidr, err := net.ParseCIDR(cidrStr)
			if err != nil {
				klog.Warningf("Site %s has invalid nodeCIDR %s: %v", site.Name, cidrStr, err)
				continue
			}

			for _, ip := range internalIPs {
				if cidr.Contains(ip) {
					klog.V(3).Infof("Node %s (IP %s) matches site %s (CIDR %s)", node.Name, ip, site.Name, cidrStr)
					return site.Name
				}
			}
		}
	}

	return ""
}

// GetSiteForNode looks up which site a node belongs to using the cached sites.
// This is a faster lookup for use by other components.
func (sc *SiteController) GetSiteForNode(node *corev1.Node) string {
	sc.sitesCacheLock.RLock()
	defer sc.sitesCacheLock.RUnlock()

	return sc.findSiteForNode(node, sc.sitesCache)
}

// Helper functions

func getNodeSiteLabel(node *corev1.Node) string {
	if node.Labels == nil {
		return ""
	}

	return node.Labels[SiteLabelKey]
}

func getNodeAnnotation(node *corev1.Node, key string) string {
	if node.Annotations == nil {
		return ""
	}

	return node.Annotations[key]
}

func nodeAddressesEqual(a, b *corev1.Node) bool {
	if len(a.Status.Addresses) != len(b.Status.Addresses) {
		return false
	}

	for i := range a.Status.Addresses {
		if a.Status.Addresses[i].Type != b.Status.Addresses[i].Type ||
			a.Status.Addresses[i].Address != b.Status.Addresses[i].Address {
			return false
		}
	}

	return true
}

// stringSlicesEqual compares two string slices for equality.
func stringSlicesEqual(a, b []string) bool {
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

// escapeJSONPointer escapes a string for use in a JSON Pointer (RFC 6901)
func escapeJSONPointer(s string) string {
	// Replace ~ with ~0 and / with ~1
	result := ""

	for _, c := range s {
		switch c {
		case '~':
			result += "~0"
		case '/':
			result += "~1"
		default:
			result += string(c)
		}
	}

	return result
}

// validateSiteCIDRsNoOverlap checks that no two sites have overlapping CIDRs.
// This prevents routing conflicts where the same CIDR would be routed to multiple sites.
func validateSiteCIDRsNoOverlap(sites []unboundednetv1alpha1.Site) error {
	// Build a map of all CIDRs to the site that owns them
	type cidrOwner struct {
		siteName string
		cidrType string // "nodeCidr"
	}

	cidrMap := make(map[string]cidrOwner)

	for _, site := range sites {
		// Check nodeCidrs
		for _, cidrStr := range site.Spec.NodeCidrs {
			_, cidr, err := net.ParseCIDR(cidrStr)
			if err != nil {
				klog.Warningf("Site %s has invalid nodeCIDR %s: %v", site.Name, cidrStr, err)
				continue
			}
			// Normalize the CIDR string
			normalizedCIDR := cidr.String()

			if existing, exists := cidrMap[normalizedCIDR]; exists {
				return fmt.Errorf("overlapping nodeCIDR %s: site %q and site %q both claim this CIDR (found in %s of %s)",
					normalizedCIDR, existing.siteName, site.Name, existing.cidrType, existing.siteName)
			}

			cidrMap[normalizedCIDR] = cidrOwner{siteName: site.Name, cidrType: "nodeCidr"}
		}
	}

	// Also check for overlapping ranges (one CIDR contains another)
	var allCIDRs []struct {
		cidr     *net.IPNet
		siteName string
		cidrType string
		cidrStr  string
	}

	for _, site := range sites {
		for _, cidrStr := range site.Spec.NodeCidrs {
			_, cidr, err := net.ParseCIDR(cidrStr)
			if err != nil {
				continue
			}

			allCIDRs = append(allCIDRs, struct {
				cidr     *net.IPNet
				siteName string
				cidrType string
				cidrStr  string
			}{cidr: cidr, siteName: site.Name, cidrType: "nodeCidr", cidrStr: cidr.String()})
		}
	}

	// Check each pair of CIDRs for overlap
	for i := 0; i < len(allCIDRs); i++ {
		for j := i + 1; j < len(allCIDRs); j++ {
			a := allCIDRs[i]
			b := allCIDRs[j]

			// Skip if same site
			if a.siteName == b.siteName {
				continue
			}

			// Check if either CIDR contains the other's first IP
			if a.cidr.Contains(b.cidr.IP) || b.cidr.Contains(a.cidr.IP) {
				return fmt.Errorf("overlapping CIDRs between sites: site %q %s %s overlaps with site %q %s %s",
					a.siteName, a.cidrType, a.cidrStr, b.siteName, b.cidrType, b.cidrStr)
			}
		}
	}

	return nil
}

// TryAllocateForNode attempts to allocate pod CIDRs for a node based on its
// internal IPs. Returns (podCIDR, podCIDRs, true) on success or
// ("", nil, false) if allocation is not possible.
func (sc *SiteController) TryAllocateForNode(nodeName string, internalIPs []string) (string, []string, string, bool) {
	if !sc.allocatorsReady.Load() {
		return "", nil, "", false
	}

	sc.sitesCacheLock.RLock()
	sites := sc.sitesCache
	sc.sitesCacheLock.RUnlock()

	// Find matching site by checking if any internal IP falls in a site's nodeCidrs
	siteName := ""

	for _, site := range sites {
		for _, nodeCidr := range site.Spec.NodeCidrs {
			_, cidrNet, err := net.ParseCIDR(nodeCidr)
			if err != nil {
				continue
			}

			for _, ip := range internalIPs {
				if cidrNet.Contains(net.ParseIP(ip)) {
					siteName = site.Name
					break
				}
			}

			if siteName != "" {
				break
			}
		}

		if siteName != "" {
			break
		}
	}

	if siteName == "" {
		return "", nil, "", false
	}

	var site *unboundednetv1alpha1.Site

	for i := range sites {
		if sites[i].Name == siteName {
			site = &sites[i]
			break
		}
	}

	if site == nil {
		return "", nil, "", false
	}

	state := sc.selectAssignmentForNode(*site, nodeName)
	if state == nil {
		return "", nil, "", false
	}

	podCIDR, podCIDRs, err := sc.computePodCIDRsForNode(state)
	if err != nil {
		return "", nil, "", false
	}

	return podCIDR, podCIDRs, siteName, true
}

// containsFinalizer returns true if the given finalizer is in the list.
func containsFinalizer(finalizers []string, finalizer string) bool {
	for _, f := range finalizers {
		if f == finalizer {
			return true
		}
	}

	return false
}

// ensureFinalizer adds the protection finalizer to a resource if not already
// present. It uses a merge patch to set the full finalizers list.
func ensureFinalizer(ctx context.Context, client dynamic.Interface, gvr schema.GroupVersionResource, name string, finalizers []string) error {
	if containsFinalizer(finalizers, ProtectionFinalizer) {
		return nil
	}

	newFinalizers := make([]string, len(finalizers), len(finalizers)+1)
	copy(newFinalizers, finalizers)
	newFinalizers = append(newFinalizers, ProtectionFinalizer)

	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"finalizers": newFinalizers,
		},
	}

	patchData, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("failed to marshal finalizer patch: %w", err)
	}

	_, err = client.Resource(gvr).Patch(ctx, name, types.MergePatchType, patchData, metav1.PatchOptions{})

	return err
}

// removeFinalizer removes the protection finalizer from a resource if present.
// It uses a merge patch to replace the finalizers list with the filtered list.
func removeFinalizer(ctx context.Context, client dynamic.Interface, gvr schema.GroupVersionResource, name string, finalizers []string) error {
	if !containsFinalizer(finalizers, ProtectionFinalizer) {
		return nil
	}

	newFinalizers := make([]string, 0, len(finalizers))
	for _, f := range finalizers {
		if f != ProtectionFinalizer {
			newFinalizers = append(newFinalizers, f)
		}
	}

	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"finalizers": newFinalizers,
		},
	}

	patchData, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("failed to marshal finalizer patch: %w", err)
	}

	_, err = client.Resource(gvr).Patch(ctx, name, types.MergePatchType, patchData, metav1.PatchOptions{})

	return err
}
