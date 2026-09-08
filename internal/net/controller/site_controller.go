// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
	"github.com/Azure/unbounded/internal/net/allocator"
)

const (
	// canonicalSiteLabelKey is the canonical site-membership label the
	// controller applies to Nodes (shared with the Machine site label). It
	// supersedes deprecatedSiteLabelKey.
	canonicalSiteLabelKey = unboundedv1alpha3.MachineSiteLabelKey

	// deprecatedSiteLabelKey is the pre-rename site label. During the
	// deprecation window the controller dual-writes it alongside the canonical
	// label and falls back to reading it, so in-flight upgrades and older
	// consumers keep working. A future release removes it.
	deprecatedSiteLabelKey = unboundednetv1alpha1.SiteLabelKey

	// WireGuardPubKeyAnnotation is the annotation key for a node's WireGuard public key
	WireGuardPubKeyAnnotation = "net.unbounded-cloud.io/wg-pubkey"

	// WireGuardPortAnnotation is the annotation key for a gateway node's
	// assigned WireGuard port (used for gateway-to-gateway peering).
	WireGuardPortAnnotation = "net.unbounded-cloud.io/wireguard-port"

	// TunnelMTUAnnotation is the annotation key for a node's detected
	// maximum tunnel MTU (default-route MTU minus encapsulation
	// overhead). The controller compares this against the configured node MTU
	// to surface warnings when the configured value is too high.
	TunnelMTUAnnotation = "net.unbounded-cloud.io/tunnel-mtu"

	// ProtectionFinalizer prevents deletion of Sites and GatewayPools that
	// still have active nodes assigned. The controller adds this finalizer
	// when nodes are present and removes it when the last node is unassigned.
	ProtectionFinalizer = "net.unbounded-cloud.io/protection"
)

var siteGVR = schema.GroupVersionResource{
	Group:    unboundedv1alpha3.GroupVersion.Group,
	Version:  unboundedv1alpha3.GroupVersion.Version,
	Resource: "sites",
}

var siteNodeSliceGVR = schema.GroupVersionResource{
	Group:    "net.unbounded-cloud.io",
	Version:  "v1alpha1",
	Resource: "sitenodeslices",
}

// legacySiteGVR is the pre-migration net-group Site resource. During the
// unbounded-system migration a SiteNodeSlice may still reference a Site that
// has not yet been translated into the machina group (siteGVR); that Site
// continues to exist here until the operator's reaper deletes the legacy CRD.
// Orphan cleanup consults this group so it never deletes a live site's slices
// mid-migration.
var legacySiteGVR = schema.GroupVersionResource{
	Group:    "net.unbounded-cloud.io",
	Version:  "v1alpha1",
	Resource: "sites",
}

var gatewayPoolGVRSite = schema.GroupVersionResource{
	Group:    "net.unbounded-cloud.io",
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
	sitesCache     []unboundedv1alpha3.Site
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

	// slicesDirty is set when node or slice state changes. The periodic loop
	// atomically consumes it to coalesce updates and restores it after failures.
	slicesDirty atomic.Bool

	// hasSynced indicates whether the informer caches have completed initial sync
	hasSynced atomic.Bool

	// allocatorsReady is set after assignment allocators have been built and
	// seeded with all existing node CIDRs. Pod CIDR allocation is blocked
	// until this flag is true to prevent assigning CIDRs that are already in
	// use by other nodes.
	allocatorsReady atomic.Bool
	ready           chan struct{}
	readyOnce       sync.Once

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
		ready:                make(chan struct{}),
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

	// Reconcile externally modified slices. Events caused by this controller's
	// own writes may trigger one redundant pass, which is harmless.
	if _, err := sliceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			sc.markSlicesDirty()
		},
		UpdateFunc: func(old, new interface{}) {
			sc.markSlicesDirty()
		},
		DeleteFunc: func(obj interface{}) {
			sc.markSlicesDirty()
		},
	}); err != nil {
		return nil, fmt.Errorf("failed to add site node slice event handler: %w", err)
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

// Ready is closed after informer sync, initial reconciliation, and allocator seeding.
func (sc *SiteController) Ready() <-chan struct{} {
	return sc.ready
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
	sites := make([]unboundedv1alpha3.Site, 0, len(items))

	for _, item := range items {
		unstr, ok := item.(*unstructured.Unstructured)
		if !ok {
			continue
		}

		site := unboundedv1alpha3.Site{}

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
	site       unboundedv1alpha3.Site
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

func (sc *SiteController) collectEnabledAssignments(sites []unboundedv1alpha3.Site) []assignmentRef {
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

func (sc *SiteController) updateAssignmentAllocators(sites []unboundedv1alpha3.Site) {
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

	// Periodically update site statuses and slices when dirty.
	go wait.UntilWithContext(ctx, func(ctx context.Context) {
		sc.reportDuplicateNodePodCIDRs()

		if err := sc.updateSiteSlicesIfDirty(ctx); err != nil {
			klog.Errorf("Failed to update SiteNodeSlices: %v", err)
		}
	}, 5*time.Second)

	sc.readyOnce.Do(func() { close(sc.ready) })
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
	needsLabel := !nodeSiteLabelsCurrent(node, siteName)
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
		if siteName != "" {
			patchData, err := siteLabelAddMergePatch(siteName)
			if err != nil {
				return fmt.Errorf("failed to marshal patch: %w", err)
			}

			if _, err := sc.clientset.CoreV1().Nodes().Patch(ctx, node.Name, types.MergePatchType, patchData, metav1.PatchOptions{}); err != nil {
				return fmt.Errorf("failed to patch node: %w", err)
			}

			klog.Infof("Labeled node %s with site %s", node.Name, siteName)
		} else if currentSite != "" {
			patchData, err := siteLabelRemoveMergePatch()
			if err != nil {
				return fmt.Errorf("failed to marshal patch: %w", err)
			}

			if _, err := sc.clientset.CoreV1().Nodes().Patch(ctx, node.Name, types.MergePatchType, patchData, metav1.PatchOptions{}); err != nil {
				return fmt.Errorf("failed to patch node: %w", err)
			}

			klog.Infof("Removed site label from node %s", node.Name)
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

// updateSiteSlicesIfDirty updates slices after atomically consuming the dirty
// flag. A failed pass restores the flag so the periodic loop retries it.
func (sc *SiteController) updateSiteSlicesIfDirty(ctx context.Context) error {
	if !sc.slicesDirty.CompareAndSwap(true, false) {
		return nil
	}

	if err := sc.updateAllSiteSlices(ctx); err != nil {
		sc.markSlicesDirty()

		return err
	}

	return nil
}

// updateAllSiteSlices updates the SiteNodeSlice objects for all sites
func (sc *SiteController) updateAllSiteSlices(ctx context.Context) error {
	// Ensure only one slice update runs at a time to prevent conflicts
	sc.sliceUpdateLock.Lock()
	defer sc.sliceUpdateLock.Unlock()

	sc.sitesCacheLock.RLock()
	cachedSites := append([]unboundedv1alpha3.Site(nil), sc.sitesCache...)
	sc.sitesCacheLock.RUnlock()

	// Continue converging independent resources, but return every error so the
	// periodic loop schedules another pass.
	var updateErrors []error

	if err := sc.cleanupOrphanSiteNodeSlices(ctx); err != nil {
		updateErrors = append(updateErrors, fmt.Errorf("clean up orphan site node slices: %w", err))
	}

	// Never use an informer-cached Site as a mutation input. Besides protecting
	// against stale fields, the UID check prevents a delete/recreate with the
	// same name from receiving writes intended for the deleted Site.
	sites := make([]unboundedv1alpha3.Site, 0, len(cachedSites))

	for _, cachedSite := range cachedSites {
		liveSite, err := sc.getLiveSiteWithUID(ctx, cachedSite.Name, cachedSite.UID)
		if err != nil {
			updateErrors = append(updateErrors, fmt.Errorf("site %s: %w", cachedSite.Name, err))

			continue
		}

		sites = append(sites, liveSite)
	}

	if len(sites) == 0 {
		return errors.Join(updateErrors...)
	}

	nodes, err := sc.nodeLister.List(labels.Everything())
	if err != nil {
		updateErrors = append(updateErrors, fmt.Errorf("failed to list nodes for slice update: %w", err))

		return errors.Join(updateErrors...)
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

	for _, site := range sites {
		nodesInfo := siteNodesInfo[site.Name]
		// Sort by node name for consistent ordering
		sort.Slice(nodesInfo, func(i, j int) bool {
			return nodesInfo[i].Name < nodesInfo[j].Name
		})

		if err := sc.updateSiteSlices(ctx, site, nodesInfo, allSiteNodeCounts[site.Name]); err != nil {
			updateErrors = append(updateErrors, fmt.Errorf("site %s: %w", site.Name, err))
		}
	}

	// Manage protection finalizers: add when nodes are assigned, remove when
	// no nodes remain so the site can be deleted.
	for _, site := range sites {
		nodeCount := allSiteNodeCounts[site.Name]
		if nodeCount > 0 {
			if err := ensureFinalizer(ctx, sc.dynamicClient, siteGVR, site.Name, site.Finalizers, site.UID); err != nil {
				updateErrors = append(updateErrors, fmt.Errorf("site %s protection finalizer: %w", site.Name, err))
			}
		} else {
			if err := removeFinalizer(ctx, sc.dynamicClient, siteGVR, site.Name, site.Finalizers, site.UID); err != nil {
				updateErrors = append(updateErrors, fmt.Errorf("site %s protection finalizer: %w", site.Name, err))
			}
		}
	}

	return errors.Join(updateErrors...)
}

// updateSiteSlices creates/updates/deletes SiteNodeSlice objects for a site
func (sc *SiteController) updateSiteSlices(
	ctx context.Context,
	site unboundedv1alpha3.Site,
	nodesInfo []unboundednetv1alpha1.NodeInfo,
	allNodeCount int,
) error {
	var err error

	site, err = sc.getLiveSiteWithUID(ctx, site.Name, site.UID)
	if err != nil {
		return err
	}

	// Calculate how many slices we need
	numSlices := (len(nodesInfo) + unboundednetv1alpha1.MaxNodesPerSlice - 1) / unboundednetv1alpha1.MaxNodesPerSlice
	if numSlices == 0 {
		numSlices = 0 // No nodes, no slices needed
	}

	// Get existing slices for this site
	existingSlices := sc.getExistingSlices(site.Name)
	desiredSliceNames := make(map[string]struct{}, numSlices)

	// Create or update slices
	for i := 0; i < numSlices; i++ {
		desiredSliceNames[fmt.Sprintf("%s-%d", site.Name, i)] = struct{}{}

		start := i * unboundednetv1alpha1.MaxNodesPerSlice

		end := start + unboundednetv1alpha1.MaxNodesPerSlice
		if end > len(nodesInfo) {
			end = len(nodesInfo)
		}

		sliceNodes := nodesInfo[start:end]

		if err := sc.createOrUpdateSlice(ctx, site, i, sliceNodes); err != nil {
			return err
		}
	}

	resource := sc.dynamicClient.Resource(siteNodeSliceGVR)

	// Delete any slice whose index is no longer desired. Re-read and revalidate
	// each object so a stale informer entry cannot delete a reassigned slice.
	for _, existingSlice := range existingSlices {
		if _, desired := desiredSliceNames[existingSlice.Name]; desired {
			continue
		}

		sliceName := existingSlice.Name

		liveSlice, err := resource.Get(ctx, sliceName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}

		if err != nil {
			return fmt.Errorf("failed to get extra slice %s: %w", sliceName, err)
		}

		liveSiteName, found, err := unstructured.NestedString(liveSlice.Object, "siteName")
		if err != nil {
			return fmt.Errorf("failed to read siteName from extra slice %s: %w", sliceName, err)
		}

		if !found || liveSiteName != site.Name {
			continue
		}

		if _, desired := desiredSliceNames[liveSlice.GetName()]; desired {
			continue
		}

		if _, err := sc.getLiveSiteWithUID(ctx, site.Name, site.UID); err != nil {
			return err
		}

		err = deleteLiveSiteNodeSlice(ctx, resource, liveSlice)
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete extra slice %s: %w", sliceName, err)
		}

		if err == nil {
			klog.V(2).Infof("Deleted extra slice %s", sliceName)
		}
	}

	// Update site status only if it changed
	if err := sc.updateSiteStatusIfChanged(ctx, site, allNodeCount, numSlices); err != nil {
		return err
	}

	return nil
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

func (sc *SiteController) getLiveSiteWithUID(ctx context.Context, name string, expectedUID types.UID) (unboundedv1alpha3.Site, error) {
	live, err := sc.dynamicClient.Resource(siteGVR).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return unboundedv1alpha3.Site{}, fmt.Errorf("expected site %s with UID %q no longer exists: %w", name, expectedUID, err)
	}

	if err != nil {
		return unboundedv1alpha3.Site{}, fmt.Errorf("failed to get site %s: %w", name, err)
	}

	if live.GetUID() != expectedUID {
		return unboundedv1alpha3.Site{}, fmt.Errorf("site %s UID changed from %q to %q", name, expectedUID, live.GetUID())
	}

	site := unboundedv1alpha3.Site{}

	data, err := live.MarshalJSON()
	if err != nil {
		return unboundedv1alpha3.Site{}, fmt.Errorf("failed to marshal live site %s: %w", name, err)
	}

	if err := json.Unmarshal(data, &site); err != nil {
		return unboundedv1alpha3.Site{}, fmt.Errorf("failed to unmarshal live site %s: %w", name, err)
	}

	return site, nil
}

func (sc *SiteController) cleanupOrphanSiteNodeSlices(ctx context.Context) error {
	resource := sc.dynamicClient.Resource(siteNodeSliceGVR)

	cachedSlices := sc.sliceInformer.GetStore().List()
	if len(cachedSlices) == 0 {
		return nil
	}

	// Establish that the Site source is healthy and populated before concluding
	// that any slice is orphaned. A failed or empty live Site listing (Sites not
	// yet translated into the machina group during the unbounded-system
	// migration, or the Site CRD briefly unavailable) must never be read as
	// "every Site was deleted": doing so would delete every SiteNodeSlice
	// cluster-wide and tear down all inter-node tunnels. Legitimate whole-Site
	// deletion is handled by owner-reference garbage collection, so skipping
	// cleanup in that state is safe.
	liveSites, err := sc.dynamicClient.Resource(siteGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list sites for orphan slice cleanup: %w", err)
	}

	if len(liveSites.Items) == 0 {
		klog.V(2).Infof(
			"Skipping orphan SiteNodeSlice cleanup: no Sites present but %d slice(s) exist (Site source not yet populated)",
			len(cachedSlices),
		)

		return nil
	}

	liveSiteNames := make(map[string]struct{}, len(liveSites.Items))
	for i := range liveSites.Items {
		liveSiteNames[liveSites.Items[i].GetName()] = struct{}{}
	}

	var cleanupErrors []error

	for _, item := range cachedSlices {
		cachedSlice, ok := item.(*unstructured.Unstructured)
		if !ok {
			continue
		}

		name := cachedSlice.GetName()

		liveSlice, err := resource.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}

		if err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("get slice %s: %w", name, err))

			continue
		}

		siteName, found, err := unstructured.NestedString(liveSlice.Object, "siteName")
		if err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("read siteName from slice %s: %w", name, err))

			continue
		}

		if found && siteName != "" {
			// The referenced Site still exists in the machina group: keep it.
			if _, live := liveSiteNames[siteName]; live {
				continue
			}

			// Migration guard: the Site may not have been translated into the
			// machina group yet while its legacy net-group Site still exists.
			// Such a slice is not an orphan; deleting it would tear down a live
			// site's tunnels mid-migration. Only a Site absent from BOTH groups
			// is genuinely gone.
			legacyPresent, err := sc.legacySiteExists(ctx, siteName)
			if err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("check legacy site %s for slice %s: %w", siteName, name, err))

				continue
			}

			if legacyPresent {
				continue
			}
		}

		// Either the slice carries no siteName (it can never be reconciled) or
		// its Site is absent from both the machina and legacy groups. The Site
		// source is confirmed healthy and non-empty, so this is a genuine orphan.
		if err := deleteLiveSiteNodeSlice(ctx, resource, liveSlice); err != nil && !apierrors.IsNotFound(err) {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("delete orphan slice %s: %w", name, err))
		}
	}

	return errors.Join(cleanupErrors...)
}

// legacySiteExists reports whether a pre-migration net-group Site with the given
// name still exists. It lets orphan cleanup avoid deleting SiteNodeSlices for
// Sites that have not yet been translated into the machina group during the
// unbounded-system migration. A missing legacy Site object, or a legacy Site CRD
// that has already been reaped (its API path returns NotFound), both report
// false.
func (sc *SiteController) legacySiteExists(ctx context.Context, name string) (bool, error) {
	_, err := sc.dynamicClient.Resource(legacySiteGVR).Get(ctx, name, metav1.GetOptions{})
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err):
		return false, nil
	default:
		return false, fmt.Errorf("get legacy site %s: %w", name, err)
	}
}

func deleteLiveSiteNodeSlice(ctx context.Context, resource dynamic.ResourceInterface, liveSlice *unstructured.Unstructured) error {
	uid := liveSlice.GetUID()
	resourceVersion := liveSlice.GetResourceVersion()

	return resource.Delete(ctx, liveSlice.GetName(), metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{
			UID:             &uid,
			ResourceVersion: &resourceVersion,
		},
	})
}

// createOrUpdateSlice creates or updates a SiteNodeSlice with retry logic for conflicts
func (sc *SiteController) createOrUpdateSlice(ctx context.Context, site unboundedv1alpha3.Site, sliceIndex int, nodes []unboundednetv1alpha1.NodeInfo) error {
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
			nodeData["internalIPs"] = stringSliceToInterfaceSlice(ni.InternalIPs)
		}

		if len(ni.PodCIDRs) > 0 {
			nodeData["podCIDRs"] = stringSliceToInterfaceSlice(ni.PodCIDRs)
		}

		nodesData[i] = nodeData
	}

	resource := sc.dynamicClient.Resource(siteNodeSliceGVR)
	backoff := wait.Backoff{
		Duration: 100 * time.Millisecond,
		Factor:   2,
		Steps:    5,
	}

	err := wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		// Always read directly from the API. The informer may remain stale after
		// an AlreadyExists or Conflict response.
		existing, err := resource.Get(ctx, sliceName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			liveSite, siteErr := sc.getLiveSiteWithUID(ctx, site.Name, site.UID)
			if siteErr != nil {
				return false, siteErr
			}

			// Create new slice
			desired := sc.buildSliceObject(liveSite, sliceName, sliceIndex, nodesData)

			_, err = resource.Create(ctx, desired, metav1.CreateOptions{})
			if err != nil {
				if apierrors.IsAlreadyExists(err) {
					klog.V(2).Infof("Slice %s was created by another process, retrying", sliceName)

					return false, nil
				}

				return false, fmt.Errorf("failed to create slice %s: %w", sliceName, err)
			}

			klog.V(2).Infof("Created slice %s with %d nodes", sliceName, len(nodes))

			return true, nil
		}

		if err != nil {
			return false, fmt.Errorf("failed to get slice %s: %w", sliceName, err)
		}

		liveSite, err := sc.getLiveSiteWithUID(ctx, site.Name, site.UID)
		if err != nil {
			return false, err
		}

		desired := sc.buildSliceObject(liveSite, sliceName, sliceIndex, nodesData)

		// Check if update is needed
		existingNodes, _, _ := unstructured.NestedSlice(existing.Object, "nodes")                         //nolint:errcheck
		existingNodeCount, foundNodeCount, _ := unstructured.NestedInt64(existing.Object, "nodeCount")    //nolint:errcheck
		existingSiteName, foundSiteName, _ := unstructured.NestedString(existing.Object, "siteName")      //nolint:errcheck
		existingSliceIndex, foundSliceIndex, _ := unstructured.NestedInt64(existing.Object, "sliceIndex") //nolint:errcheck

		if sc.sliceNodesEqual(existingNodes, nodesData) &&
			foundNodeCount && existingNodeCount == int64(len(nodesData)) &&
			foundSiteName && existingSiteName == liveSite.Name &&
			foundSliceIndex && existingSliceIndex == int64(sliceIndex) &&
			hasExactSiteOwnerReference(existing.GetOwnerReferences(), liveSite) {
			return true, nil
		}

		// Mutate a copy of the live object so labels, annotations, finalizers,
		// and metadata owned by other actors survive this update.
		sliceObj := existing.DeepCopy()
		sliceObj.SetOwnerReferences(desired.GetOwnerReferences())
		sliceObj.Object["siteName"] = liveSite.Name
		sliceObj.Object["sliceIndex"] = int64(sliceIndex)
		sliceObj.Object["nodes"] = nodesData
		sliceObj.Object["nodeCount"] = int64(len(nodesData))

		_, err = resource.Update(ctx, sliceObj, metav1.UpdateOptions{})
		if err != nil {
			if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
				klog.V(2).Infof("Slice %s changed during update, will retry", sliceName)

				return false, nil
			}

			return false, fmt.Errorf("failed to update slice %s: %w", sliceName, err)
		}

		klog.V(2).Infof("Updated slice %s with %d nodes", sliceName, len(nodes))

		return true, nil
	})
	if err != nil {
		if wait.Interrupted(err) {
			return fmt.Errorf("failed to converge slice %s after %d attempts: %w", sliceName, backoff.Steps, err)
		}

		return err
	}

	return nil
}

func stringSliceToInterfaceSlice(values []string) []interface{} {
	result := make([]interface{}, len(values))
	for i, value := range values {
		result[i] = value
	}

	return result
}

// buildSliceObject constructs the unstructured SiteNodeSlice object
func (sc *SiteController) buildSliceObject(site unboundedv1alpha3.Site, sliceName string, sliceIndex int, nodesData []interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "net.unbounded-cloud.io/v1alpha1",
			"kind":       "SiteNodeSlice",
			"metadata": map[string]interface{}{
				"name": sliceName,
				"ownerReferences": []interface{}{
					map[string]interface{}{
						// Site now lives in the machina group
						// (unbounded-cloud.io/v1alpha3); the ownerRef must match
						// the owner's real GVK or the garbage collector will
						// orphan or wrongly collect the slice after migration.
						"apiVersion":         unboundedv1alpha3.GroupVersion.String(),
						"kind":               "Site",
						"name":               site.Name,
						"uid":                string(site.UID),
						"controller":         true,
						"blockOwnerDeletion": false,
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

func hasExactSiteOwnerReference(refs []metav1.OwnerReference, site unboundedv1alpha3.Site) bool {
	if len(refs) != 1 {
		return false
	}

	ref := refs[0]

	return ref.APIVersion == unboundedv1alpha3.GroupVersion.String() &&
		ref.Kind == "Site" &&
		ref.Name == site.Name &&
		ref.UID == site.UID &&
		ref.Controller != nil && *ref.Controller &&
		(ref.BlockOwnerDeletion == nil || !*ref.BlockOwnerDeletion)
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

// updateSiteStatusIfChanged updates the status of a site only if it has changed.
func (sc *SiteController) updateSiteStatusIfChanged(ctx context.Context, site unboundedv1alpha3.Site, nodeCount, sliceCount int) error {
	// Always update gauges so they reflect the latest state.
	SiteNodesGauge.WithLabelValues(site.Name).Set(float64(nodeCount))
	SiteNodeSlicesGauge.WithLabelValues(site.Name).Set(float64(sliceCount))

	backoff := wait.Backoff{
		Duration: 50 * time.Millisecond,
		Factor:   2,
		Steps:    3,
	}

	err := wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		liveSite, err := sc.getLiveSiteWithUID(ctx, site.Name, site.UID)
		if err != nil {
			return false, err
		}

		if liveSite.Status.NodeCount == nodeCount && liveSite.Status.SliceCount == sliceCount {
			klog.V(4).Infof("Site %s status unchanged (%d nodes, %d slices), skipping update", site.Name, nodeCount, sliceCount)

			return true, nil
		}

		patchData, err := json.Marshal(map[string]interface{}{
			"metadata": map[string]interface{}{
				"resourceVersion": liveSite.ResourceVersion,
			},
			"status": map[string]interface{}{
				"nodeCount":  nodeCount,
				"sliceCount": sliceCount,
			},
		})
		if err != nil {
			return false, fmt.Errorf("failed to marshal status patch for site %s: %w", site.Name, err)
		}

		_, err = sc.dynamicClient.Resource(siteGVR).Patch(ctx, site.Name, types.MergePatchType, patchData, metav1.PatchOptions{}, "status")
		if err == nil {
			klog.Infof("Updated site %s status: %d nodes, %d slices", site.Name, nodeCount, sliceCount)

			return true, nil
		}

		if apierrors.IsConflict(err) {
			klog.V(2).Infof("Conflict updating site %s status, retrying", site.Name)

			return false, nil
		}

		if apierrors.IsNotFound(err) {
			klog.V(2).Infof("Site %s changed during status update, retrying", site.Name)

			return false, nil
		}

		return false, fmt.Errorf("failed to update status for site %s: %w", site.Name, err)
	})
	if err != nil {
		if wait.Interrupted(err) {
			return fmt.Errorf("failed to update site %s status after %d attempts: %w", site.Name, backoff.Steps, err)
		}

		return err
	}

	return nil
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

func (sc *SiteController) selectAssignmentForNode(site unboundedv1alpha3.Site, nodeName string) *assignmentAllocator {
	enabledAssignments := sc.collectEnabledAssignments([]unboundedv1alpha3.Site{site})

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

func (sc *SiteController) assignPodCIDRsForNode(ctx context.Context, node *corev1.Node, sites []unboundedv1alpha3.Site, siteName string) error {
	if !sc.hasSynced.Load() {
		return fmt.Errorf("informer caches not synced; refusing pod CIDR assignment for node %s", node.Name)
	}

	if !sc.allocatorsReady.Load() {
		return fmt.Errorf("allocators not yet seeded; refusing pod CIDR assignment for node %s", node.Name)
	}

	if siteName == "" {
		return nil
	}

	var site *unboundedv1alpha3.Site

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
// MergePatch to cut the number of API calls in half during scale-in. It sets
// every site-membership label key (canonical + deprecated) during the
// deprecation window.
func (sc *SiteController) patchNodeLabelAndCIDRs(ctx context.Context, nodeName, siteName, podCIDR string, podCIDRs []string) error {
	labels := map[string]interface{}{}
	for _, key := range siteLabelKeys() {
		labels[key] = siteName
	}

	spec := map[string]interface{}{"podCIDR": podCIDR}
	if podCIDRs != nil {
		spec["podCIDRs"] = podCIDRs
	} else {
		spec["podCIDRs"] = []string{}
	}

	patch, err := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{"labels": labels},
		"spec":     spec,
	})
	if err != nil {
		return err
	}

	_, err = sc.clientset.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return err
	}

	klog.Infof("Labeled node %s with site %s and assigned podCIDR=%s, podCIDRs=%v", nodeName, siteName, podCIDR, podCIDRs)

	return nil
}

// assignPodCIDRsForNodeWithLabel combines site labeling and pod CIDR assignment
// into a single API call for new nodes that need both.
func (sc *SiteController) assignPodCIDRsForNodeWithLabel(ctx context.Context, node *corev1.Node, sites []unboundedv1alpha3.Site, siteName string) error {
	if !sc.hasSynced.Load() {
		return fmt.Errorf("informer caches not synced; refusing pod CIDR assignment for node %s", node.Name)
	}

	if !sc.allocatorsReady.Load() {
		return fmt.Errorf("allocators not yet seeded; refusing pod CIDR assignment for node %s", node.Name)
	}

	if siteName == "" {
		return nil
	}

	var site *unboundedv1alpha3.Site

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
func (sc *SiteController) markNodeCIDRsAllocated(node *corev1.Node, sites []unboundedv1alpha3.Site, siteName string) {
	var site *unboundedv1alpha3.Site

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

	var site *unboundedv1alpha3.Site

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
func (sc *SiteController) findSiteForNode(node *corev1.Node, sites []unboundedv1alpha3.Site) string {
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

// siteLabelKeys are the node site-membership label keys in priority order:
// canonical first, deprecated fallback.
func siteLabelKeys() []string {
	return []string{canonicalSiteLabelKey, deprecatedSiteLabelKey}
}

// NodeSiteLabel returns the site a Node belongs to, preferring the canonical
// label (unbounded-cloud.io/site) and falling back to the deprecated
// net.unbounded-cloud.io/site.
func NodeSiteLabel(node *corev1.Node) string {
	if node.Labels == nil {
		return ""
	}

	for _, key := range siteLabelKeys() {
		if value := node.Labels[key]; value != "" {
			return value
		}
	}

	return ""
}

// nodeSiteLabelsCurrent reports whether the Node already carries siteName under
// every site-membership key (or, when siteName is empty, carries none). It
// drives dual-write so a Node labeled only with the deprecated key by an older
// controller is converged to also carry the canonical key.
func nodeSiteLabelsCurrent(node *corev1.Node, siteName string) bool {
	if siteName == "" {
		return NodeSiteLabel(node) == ""
	}

	for _, key := range siteLabelKeys() {
		if node.Labels[key] != siteName {
			return false
		}
	}

	return true
}

// siteLabelAddMergePatch builds a merge patch that sets every site-membership
// label key to siteName. A merge patch tolerates Nodes whose metadata.labels map
// is absent, unlike a JSONPatch add to /metadata/labels/<key>.
func siteLabelAddMergePatch(siteName string) ([]byte, error) {
	labels := map[string]interface{}{}
	for _, key := range siteLabelKeys() {
		labels[key] = siteName
	}

	return json.Marshal(map[string]interface{}{"metadata": map[string]interface{}{"labels": labels}})
}

// siteLabelRemoveMergePatch builds a merge patch that clears every
// site-membership label key. A merge patch (null value) is used instead of a
// JSONPatch remove so it tolerates keys that are already absent.
func siteLabelRemoveMergePatch() ([]byte, error) {
	labels := map[string]interface{}{}
	for _, key := range siteLabelKeys() {
		labels[key] = nil
	}

	return json.Marshal(map[string]interface{}{"metadata": map[string]interface{}{"labels": labels}})
}

func getNodeSiteLabel(node *corev1.Node) string {
	return NodeSiteLabel(node)
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
func validateSiteCIDRsNoOverlap(sites []unboundedv1alpha3.Site) error {
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

	var site *unboundedv1alpha3.Site

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

// ensureFinalizer adds the protection finalizer using the live resource state.
func ensureFinalizer(ctx context.Context, client dynamic.Interface, gvr schema.GroupVersionResource, name string, _ []string, expectedUID ...types.UID) error {
	return updateProtectionFinalizer(ctx, client, gvr, name, true, expectedUID...)
}

// removeFinalizer removes the protection finalizer using the live resource state.
func removeFinalizer(ctx context.Context, client dynamic.Interface, gvr schema.GroupVersionResource, name string, _ []string, expectedUID ...types.UID) error {
	return updateProtectionFinalizer(ctx, client, gvr, name, false, expectedUID...)
}

func updateProtectionFinalizer(
	ctx context.Context,
	client dynamic.Interface,
	gvr schema.GroupVersionResource,
	name string,
	wantFinalizer bool,
	expectedUID ...types.UID,
) error {
	if gvr == siteGVR && len(expectedUID) != 1 {
		return fmt.Errorf("expected exactly one Site UID for finalizer update of %s", name)
	}

	resource := client.Resource(gvr)
	backoff := wait.Backoff{
		Duration: 50 * time.Millisecond,
		Factor:   2,
		Steps:    3,
	}

	err := wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		live, err := resource.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			if len(expectedUID) == 1 {
				return false, fmt.Errorf("expected %s %s with UID %q no longer exists: %w", gvr.Resource, name, expectedUID[0], err)
			}

			return true, nil
		}

		if err != nil {
			return false, fmt.Errorf("failed to get %s %s for finalizer update: %w", gvr.Resource, name, err)
		}

		if len(expectedUID) == 1 && live.GetUID() != expectedUID[0] {
			return false, fmt.Errorf("%s %s UID changed from %q to %q", gvr.Resource, name, expectedUID[0], live.GetUID())
		}

		finalizers := live.GetFinalizers()

		hasFinalizer := containsFinalizer(finalizers, ProtectionFinalizer)
		if hasFinalizer == wantFinalizer {
			return true, nil
		}

		updated := live.DeepCopy()

		if wantFinalizer {
			newFinalizers := append([]string(nil), finalizers...)
			updated.SetFinalizers(append(newFinalizers, ProtectionFinalizer))
		} else {
			newFinalizers := make([]string, 0, len(finalizers)-1)
			for _, finalizer := range finalizers {
				if finalizer != ProtectionFinalizer {
					newFinalizers = append(newFinalizers, finalizer)
				}
			}

			updated.SetFinalizers(newFinalizers)
		}

		_, err = resource.Update(ctx, updated, metav1.UpdateOptions{})
		if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			return false, nil
		}

		if err != nil {
			return false, fmt.Errorf("failed to update %s %s finalizers: %w", gvr.Resource, name, err)
		}

		return true, nil
	})
	if err != nil && wait.Interrupted(err) {
		return fmt.Errorf("failed to update %s %s finalizers after %d attempts: %w", gvr.Resource, name, backoff.Steps, err)
	}

	return err
}
