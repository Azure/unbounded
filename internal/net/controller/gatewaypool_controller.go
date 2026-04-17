// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package controller implements the GatewayPool controller for managing gateway node pools.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
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

	unboundednetv1alpha1 "github.com/Azure/unbounded-kube/internal/net/apis/unboundednet/v1alpha1"
)

var gatewayPoolGVR = schema.GroupVersionResource{
	Group:    "net.unbounded-kube.io",
	Version:  "v1alpha1",
	Resource: "gatewaypools",
}

var gatewayNodeGVR = schema.GroupVersionResource{
	Group:    "net.unbounded-kube.io",
	Version:  "v1alpha1",
	Resource: "gatewaypoolnodes",
}

const (
	gatewayPoolTypeExternal = "External"
	gatewayPoolTypeInternal = "Internal"

	// gatewayWireguardPortBase is the first WireGuard port assigned to gateway
	// nodes for gateway-to-gateway peering.  Each gateway node gets a unique
	// port starting from this value.
	gatewayWireguardPortBase int32 = 51821
)

// GatewayPoolController manages GatewayPool status based on node selector matches.
type GatewayPoolController struct {
	clientset     kubernetes.Interface
	dynamicClient dynamic.Interface

	nodeLister corev1listers.NodeLister
	nodeSynced cache.InformerSynced

	gatewayPoolInformer cache.SharedIndexInformer
	gatewayPoolSynced   cache.InformerSynced

	gatewayNodeInformer cache.SharedIndexInformer
	gatewayNodeSynced   cache.InformerSynced

	// workqueue for gateway pool reconciliation
	workqueue workqueue.TypedRateLimitingInterface[string]

	// Cache of gateway pools for faster lookups
	poolsCache     []unboundednetv1alpha1.GatewayPool
	poolsCacheLock sync.RWMutex

	// Gateway WireGuard port allocator -- tracks assigned ports across all
	// pools so each gateway node has a globally unique port.
	gwPortMu        sync.Mutex
	gwPortAllocated map[int32]string // port -> node name
	gwPortByNode    map[string]int32 // node name -> port

	// hasSynced indicates whether the informer caches have completed initial sync
	hasSynced bool
}

// NewGatewayPoolController creates a new gateway pool controller.
func NewGatewayPoolController(
	clientset kubernetes.Interface,
	dynamicClient dynamic.Interface,
	dynamicInformerFactory dynamicinformer.DynamicSharedInformerFactory,
	nodeInformerFactory informers.SharedInformerFactory,
) (*GatewayPoolController, error) {
	gatewayPoolInformer := dynamicInformerFactory.ForResource(gatewayPoolGVR).Informer()
	gatewayNodeInformer := dynamicInformerFactory.ForResource(gatewayNodeGVR).Informer()

	nodeInformer := nodeInformerFactory.Core().V1().Nodes()

	gc := &GatewayPoolController{
		clientset:           clientset,
		dynamicClient:       dynamicClient,
		nodeLister:          nodeInformer.Lister(),
		nodeSynced:          nodeInformer.Informer().HasSynced,
		gatewayPoolInformer: gatewayPoolInformer,
		gatewayPoolSynced:   gatewayPoolInformer.HasSynced,
		gatewayNodeInformer: gatewayNodeInformer,
		gatewayNodeSynced:   gatewayNodeInformer.HasSynced,
		workqueue:           workqueue.NewTypedRateLimitingQueueWithConfig(workqueue.DefaultTypedControllerRateLimiter[string](), workqueue.TypedRateLimitingQueueConfig[string]{Name: "GatewayPools"}),
		gwPortAllocated:     make(map[int32]string),
		gwPortByNode:        make(map[string]int32),
	}

	// Set up event handlers for nodes
	if _, err := nodeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			gc.enqueueAllPools()
		},
		UpdateFunc: func(old, new interface{}) {
			oldNode := old.(*corev1.Node) //nolint:errcheck
			newNode := new.(*corev1.Node) //nolint:errcheck
			// Re-process if labels changed, internal/external IPs changed, WireGuard pubkey/port changed, or podCIDRs changed
			if !labels.Equals(labels.Set(oldNode.Labels), labels.Set(newNode.Labels)) ||
				!nodeExternalIPsEqual(oldNode, newNode) ||
				!nodeInternalIPsEqual(oldNode, newNode) ||
				getNodeAnnotation(oldNode, WireGuardPubKeyAnnotation) != getNodeAnnotation(newNode, WireGuardPubKeyAnnotation) ||
				getNodeAnnotation(oldNode, WireGuardPortAnnotation) != getNodeAnnotation(newNode, WireGuardPortAnnotation) ||
				!stringSlicesEqual(oldNode.Spec.PodCIDRs, newNode.Spec.PodCIDRs) {
				gc.enqueueAllPools()
			}
		},
		DeleteFunc: func(obj interface{}) {
			gc.enqueueAllPools()
		},
	}); err != nil {
		return nil, fmt.Errorf("failed to add node event handler: %w", err)
	}

	// Set up event handlers for gateway pools
	if _, err := gatewayPoolInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			gc.enqueuePool(obj)
		},
		UpdateFunc: func(old, new interface{}) {
			gc.enqueuePool(new)
		},
		DeleteFunc: func(obj interface{}) {
			// Nothing to clean up
		},
	}); err != nil {
		return nil, fmt.Errorf("failed to add gateway pool event handler: %w", err)
	}

	return gc, nil
}

// Run starts the gateway pool controller.
func (gc *GatewayPoolController) Run(ctx context.Context, workers int) error {
	defer utilruntime.HandleCrash()
	defer gc.workqueue.ShutDown()

	klog.Info("Starting GatewayPool controller")

	// Wait for caches to sync
	klog.Info("Waiting for GatewayPool informer caches to sync")

	if ok := cache.WaitForCacheSync(ctx.Done(), gc.nodeSynced, gc.gatewayPoolSynced, gc.gatewayNodeSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	gc.hasSynced = true

	// Update pools cache
	gc.updatePoolsCache()

	// Rebuild the gateway port allocator from existing pool statuses
	gc.seedGatewayPorts()

	klog.Info("Starting GatewayPool workers")

	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, gc.runWorker, time.Second)
	}

	klog.Info("GatewayPool controller started")
	<-ctx.Done()
	klog.Info("Shutting down GatewayPool controller")

	return nil
}

func (gc *GatewayPoolController) runWorker(ctx context.Context) {
	for gc.processNextWorkItem(ctx) {
	}
}

func (gc *GatewayPoolController) processNextWorkItem(ctx context.Context) bool {
	key, shutdown := gc.workqueue.Get()
	if shutdown {
		return false
	}
	defer gc.workqueue.Done(key)

	start := time.Now()
	err := gc.syncPool(ctx, key)
	duration := time.Since(start).Seconds()
	reconciliationDuration.WithLabelValues("GatewayPools").Observe(duration)

	if err == nil {
		gc.workqueue.Forget(key)
		reconciliationTotal.WithLabelValues("GatewayPools", "success").Inc()

		return true
	}

	reconciliationErrors.WithLabelValues("GatewayPools").Inc()
	reconciliationTotal.WithLabelValues("GatewayPools", "error").Inc()
	workqueueRetries.WithLabelValues("GatewayPools").Inc()
	utilruntime.HandleError(fmt.Errorf("error syncing gateway pool %s: %v", key, err))
	gc.workqueue.AddRateLimited(key)

	return true
}

// enqueuePool adds a gateway pool to the workqueue.
func (gc *GatewayPoolController) enqueuePool(obj interface{}) {
	unstr, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return
	}

	gc.workqueue.Add(unstr.GetName())
}

// enqueueAllPools adds all gateway pools to the workqueue.
func (gc *GatewayPoolController) enqueueAllPools() {
	if !gc.hasSynced {
		return
	}

	gc.updatePoolsCache()

	gc.poolsCacheLock.RLock()
	defer gc.poolsCacheLock.RUnlock()

	for _, pool := range gc.poolsCache {
		gc.workqueue.Add(pool.Name)
	}
}

// updatePoolsCache updates the local cache of gateway pools.
func (gc *GatewayPoolController) updatePoolsCache() {
	items := gc.gatewayPoolInformer.GetStore().List()
	pools := make([]unboundednetv1alpha1.GatewayPool, 0, len(items))

	for _, item := range items {
		unstr, ok := item.(*unstructured.Unstructured)
		if !ok {
			continue
		}

		pool, err := parseGatewayPool(unstr)
		if err != nil {
			klog.Warningf("Failed to parse GatewayPool: %v", err)
			continue
		}

		pools = append(pools, *pool)
	}

	gc.poolsCacheLock.Lock()
	gc.poolsCache = pools
	gc.poolsCacheLock.Unlock()
}

// seedGatewayPorts rebuilds the gateway WireGuard port allocator from the
// current status of all GatewayPool objects and from node annotations.  This
// must be called on startup (after informer sync) so that ports already
// assigned to existing gateway nodes are preserved across controller restarts.
func (gc *GatewayPoolController) seedGatewayPorts() {
	gc.poolsCacheLock.RLock()
	defer gc.poolsCacheLock.RUnlock()

	gc.gwPortMu.Lock()
	defer gc.gwPortMu.Unlock()

	// Reset maps
	gc.gwPortAllocated = make(map[int32]string)
	gc.gwPortByNode = make(map[string]int32)

	// First pass: discover ports from GatewayPool status objects.
	for _, pool := range gc.poolsCache {
		for _, node := range pool.Status.Nodes {
			if node.GatewayWireguardPort != 0 {
				gc.gwPortAllocated[node.GatewayWireguardPort] = node.Name
				gc.gwPortByNode[node.Name] = node.GatewayWireguardPort
			}
		}
	}

	// Second pass: discover ports from node annotations.  This catches
	// ports that were assigned but may not yet be reflected in pool status
	// (e.g. if the controller restarted mid-reconciliation).
	nodes, err := gc.nodeLister.List(labels.Everything())
	if err != nil {
		klog.Warningf("seedGatewayPorts: failed to list nodes for annotation discovery: %v", err)
	} else {
		for _, node := range nodes {
			if node.Annotations == nil {
				continue
			}

			portStr := node.Annotations[WireGuardPortAnnotation]
			if portStr == "" {
				continue
			}

			port, err := strconv.ParseInt(portStr, 10, 32)
			if err != nil {
				klog.Warningf("seedGatewayPorts: node %s has invalid %s annotation %q: %v", node.Name, WireGuardPortAnnotation, portStr, err)
				continue
			}

			p := int32(port)
			// Only record if not already known (pool status is authoritative).
			if _, exists := gc.gwPortByNode[node.Name]; !exists {
				if existing, taken := gc.gwPortAllocated[p]; taken && existing != node.Name {
					klog.Warningf("seedGatewayPorts: port %d claimed by annotation on node %s but already allocated to %s -- skipping", p, node.Name, existing)
					continue
				}

				gc.gwPortAllocated[p] = node.Name
				gc.gwPortByNode[node.Name] = p
			}
		}
	}

	klog.Infof("Seeded gateway WireGuard port allocator: %d ports in use", len(gc.gwPortAllocated))
}

// allocateGatewayPort returns the existing port for a node, or allocates the
// next available port starting at gatewayWireguardPortBase.
func (gc *GatewayPoolController) allocateGatewayPort(nodeName string) int32 {
	gc.gwPortMu.Lock()
	defer gc.gwPortMu.Unlock()

	if port, ok := gc.gwPortByNode[nodeName]; ok {
		return port
	}

	// Find the next unused port
	port := gatewayWireguardPortBase
	for gc.gwPortAllocated[port] != "" {
		port++
	}

	gc.gwPortAllocated[port] = nodeName
	gc.gwPortByNode[nodeName] = port
	klog.Infof("Allocated gateway WireGuard port %d to node %s", port, nodeName)

	return port
}

// releaseGatewayPort frees the port allocated to a node (if any).
func (gc *GatewayPoolController) releaseGatewayPort(nodeName string) {
	gc.gwPortMu.Lock()
	defer gc.gwPortMu.Unlock()

	if port, ok := gc.gwPortByNode[nodeName]; ok {
		delete(gc.gwPortAllocated, port)
		delete(gc.gwPortByNode, nodeName)
		klog.Infof("Released gateway WireGuard port %d from node %s", port, nodeName)
	}
}

// setWireGuardPortAnnotation patches the node to set the wireguard-port annotation.
func (gc *GatewayPoolController) setWireGuardPortAnnotation(ctx context.Context, nodeName string, port int32) error {
	patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{"%s":"%d"}}}`, WireGuardPortAnnotation, port))
	_, err := gc.clientset.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, patch, metav1.PatchOptions{})

	return err
}

// removeWireGuardPortAnnotation patches the node to remove the wireguard-port annotation.
func (gc *GatewayPoolController) removeWireGuardPortAnnotation(ctx context.Context, nodeName string) error {
	patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{"%s":null}}}`, WireGuardPortAnnotation))
	_, err := gc.clientset.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, patch, metav1.PatchOptions{})

	return err
}

// syncPool reconciles a single gateway pool.
func (gc *GatewayPoolController) syncPool(ctx context.Context, poolName string) error {
	gc.updatePoolsCache()

	// Get the gateway pool
	obj, exists, err := gc.gatewayPoolInformer.GetStore().GetByKey(poolName)
	if err != nil {
		return fmt.Errorf("failed to get gateway pool %s: %w", poolName, err)
	}

	if !exists {
		klog.V(2).Infof("GatewayPool %s no longer exists", poolName)
		return nil
	}

	unstr, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("unexpected object type for gateway pool %s", poolName)
	}

	pool, err := parseGatewayPool(unstr)
	if err != nil {
		return fmt.Errorf("failed to parse gateway pool %s: %w", poolName, err)
	}

	// Get all nodes
	nodes, err := gc.nodeLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}

	nodeByName := make(map[string]*corev1.Node, len(nodes))
	for _, n := range nodes {
		nodeByName[n.Name] = n
	}

	// Find matching nodes
	var matchingNodes []unboundednetv1alpha1.GatewayNodeInfo

	selector := labels.SelectorFromSet(pool.Spec.NodeSelector)
	poolType := normalizeGatewayPoolType(pool.Spec.Type)

	for _, node := range nodes {
		preferredPool, ok := gc.preferredGatewayPoolForNode(node)
		if !ok || preferredPool != poolName {
			continue
		}

		// Check if node matches the selector
		if !selector.Matches(labels.Set(node.Labels)) {
			continue
		}

		// Get external IPs
		externalIPs := getNodeExternalIPs(node)
		if poolType == gatewayPoolTypeExternal && len(externalIPs) == 0 {
			klog.V(2).Infof("GatewayPool %s: node %s not added to external pool because it has no external IPs", poolName, node.Name)
			continue
		}

		// Get internal IPs
		internalIPs := getNodeInternalIPs(node)

		// Get WireGuard public key
		wgPubKey := ""
		if node.Annotations != nil {
			wgPubKey = node.Annotations[WireGuardPubKeyAnnotation]
		}

		if wgPubKey == "" {
			klog.Warningf("Node %s matches GatewayPool %s selector but has no WireGuard public key annotation - node will not be added to pool until annotation is set", node.Name, poolName)
			continue
		}

		// Get site name from node label
		siteName := ""
		if node.Labels != nil {
			siteName = node.Labels[SiteLabelKey]
		}

		// Calculate health endpoint IPs from podCIDRs (first IP of each CIDR is the gateway IP)
		healthEndpoints := getHealthIPsFromPodCIDRs(node.Spec.PodCIDRs)

		matchingNodes = append(matchingNodes, unboundednetv1alpha1.GatewayNodeInfo{
			Name:                 node.Name,
			SiteName:             siteName,
			InternalIPs:          internalIPs,
			ExternalIPs:          externalIPs,
			HealthEndpoints:      healthEndpoints,
			WireGuardPublicKey:   wgPubKey,
			GatewayWireguardPort: gc.allocateGatewayPort(node.Name),
			PodCIDRs:             node.Spec.PodCIDRs,
		})

		if err := gc.ensureGatewayPoolNode(ctx, pool, node, siteName); err != nil {
			klog.Warningf("GatewayPool %s: failed to ensure GatewayPoolNode %s: %v", poolName, node.Name, err)
		}
	}

	if err := gc.cleanupStaleGatewayPoolNodes(ctx, pool, matchingNodes); err != nil {
		klog.Warningf("GatewayPool %s: failed to clean up stale GatewayPoolNodes: %v", poolName, err)
	}

	// Manage protection finalizer: add when nodes match, remove when no
	// nodes remain so the pool can be deleted.
	hasFinalizer := containsFinalizer(unstr.GetFinalizers(), ProtectionFinalizer)
	if len(matchingNodes) > 0 && !hasFinalizer {
		if err := ensureFinalizer(ctx, gc.dynamicClient, gatewayPoolGVR, poolName, unstr.GetFinalizers()); err != nil {
			klog.Warningf("Failed to add protection finalizer to gateway pool %s: %v", poolName, err)
		} else {
			klog.V(2).Infof("Added protection finalizer to gateway pool %s (%d nodes)", poolName, len(matchingNodes))
		}
	} else if len(matchingNodes) == 0 && hasFinalizer {
		if err := removeFinalizer(ctx, gc.dynamicClient, gatewayPoolGVR, poolName, unstr.GetFinalizers()); err != nil {
			klog.Warningf("Failed to remove protection finalizer from gateway pool %s: %v", poolName, err)
		} else {
			klog.V(2).Infof("Removed protection finalizer from gateway pool %s (no nodes)", poolName)
		}
	}

	// Sort nodes by name for consistent ordering
	sort.Slice(matchingNodes, func(i, j int) bool {
		return matchingNodes[i].Name < matchingNodes[j].Name
	})

	// Check if status needs update
	if gatewayNodesEqual(pool.Status.Nodes, matchingNodes) && pool.Status.NodeCount == len(matchingNodes) {
		klog.V(3).Infof("GatewayPool %s status unchanged (%d nodes)", poolName, len(matchingNodes))
		return nil
	}

	// Log added and removed nodes
	oldNodeNames := make(map[string]bool)
	for _, node := range pool.Status.Nodes {
		oldNodeNames[node.Name] = true
	}

	newNodeNames := make(map[string]bool)
	for _, node := range matchingNodes {
		newNodeNames[node.Name] = true
	}

	for _, node := range matchingNodes {
		if !oldNodeNames[node.Name] {
			klog.Infof("GatewayPool %s: added node %s, total %d nodes", poolName, node.Name, len(matchingNodes))
		}
	}

	for _, node := range pool.Status.Nodes {
		if !newNodeNames[node.Name] {
			klog.Infof("GatewayPool %s: removed node %s, total %d nodes", poolName, node.Name, len(matchingNodes))

			retain := false

			if currentNode, exists := nodeByName[node.Name]; exists {
				if preferredPool, ok := gc.preferredGatewayPoolForNode(currentNode); ok && preferredPool != "" {
					retain = true
				}
			}

			if retain {
				klog.V(2).Infof("GatewayPool %s: node %s removed from this pool but retained as gateway in another pool", poolName, node.Name)
			} else {
				gc.releaseGatewayPort(node.Name)
				// Remove the wireguard-port annotation from the node.
				if err := gc.removeWireGuardPortAnnotation(ctx, node.Name); err != nil {
					klog.Warningf("GatewayPool %s: failed to remove wireguard-port annotation from node %s: %v", poolName, node.Name, err)
				}
			}
		}
	}

	// Annotate each matching node with its assigned WireGuard port.
	for _, node := range matchingNodes {
		if err := gc.setWireGuardPortAnnotation(ctx, node.Name, node.GatewayWireguardPort); err != nil {
			klog.Warningf("GatewayPool %s: failed to annotate node %s with wireguard port %d: %v", poolName, node.Name, node.GatewayWireguardPort, err)
		}
	}

	// Update status
	newStatus := unboundednetv1alpha1.GatewayPoolStatus{
		Nodes:     matchingNodes,
		NodeCount: len(matchingNodes),
	}
	GatewayPoolNodesGauge.WithLabelValues(poolName).Set(float64(len(matchingNodes)))

	if err := gc.updateGatewayPoolStatus(ctx, poolName, newStatus); err != nil {
		return fmt.Errorf("failed to update gateway pool %s status: %w", poolName, err)
	}

	return nil
}

// updateGatewayPoolStatus updates the status of a gateway pool.
func (gc *GatewayPoolController) updateGatewayPoolStatus(ctx context.Context, poolName string, status unboundednetv1alpha1.GatewayPoolStatus) error {
	statusMap := map[string]interface{}{
		"nodeCount": status.NodeCount,
	}

	if len(status.Nodes) > 0 {
		nodesData := make([]map[string]interface{}, len(status.Nodes))
		for i, node := range status.Nodes {
			nodeData := map[string]interface{}{
				"name":               node.Name,
				"externalIPs":        node.ExternalIPs,
				"wireGuardPublicKey": node.WireGuardPublicKey,
			}
			if node.GatewayWireguardPort != 0 {
				nodeData["gatewayWireguardPort"] = node.GatewayWireguardPort
			}

			if node.SiteName != "" {
				nodeData["siteName"] = node.SiteName
			}

			if len(node.InternalIPs) > 0 {
				nodeData["internalIPs"] = node.InternalIPs
			}

			if len(node.HealthEndpoints) > 0 {
				nodeData["healthEndpoints"] = node.HealthEndpoints
			}

			if len(node.PodCIDRs) > 0 {
				nodeData["podCIDRs"] = node.PodCIDRs
			}

			nodesData[i] = nodeData
		}

		statusMap["nodes"] = nodesData
	} else {
		statusMap["nodes"] = []map[string]interface{}{}
	}

	patch := map[string]interface{}{
		"status": statusMap,
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("failed to marshal status patch: %w", err)
	}

	_, err = gc.dynamicClient.Resource(gatewayPoolGVR).Patch(
		ctx,
		poolName,
		types.MergePatchType,
		patchBytes,
		metav1.PatchOptions{},
		"status",
	)
	if errors.IsNotFound(err) {
		return nil
	}

	return err
}

// parseGatewayPool converts an unstructured object to a GatewayPool.
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

func normalizeGatewayPoolType(poolType string) string {
	if poolType == "" {
		return gatewayPoolTypeExternal
	}

	return poolType
}

func nodeEligibleForGatewayPool(node *corev1.Node, pool *unboundednetv1alpha1.GatewayPool) bool {
	if node == nil || pool == nil {
		return false
	}

	selector := labels.SelectorFromSet(pool.Spec.NodeSelector)
	if !selector.Matches(labels.Set(node.Labels)) {
		return false
	}

	poolType := normalizeGatewayPoolType(pool.Spec.Type)
	if poolType == gatewayPoolTypeExternal && len(getNodeExternalIPs(node)) == 0 {
		klog.V(2).Infof("GatewayPool %s: node %s not eligible for external pool because it has no external IPs", pool.Name, node.Name)
		return false
	}

	wgPubKey := ""
	if node.Annotations != nil {
		wgPubKey = node.Annotations[WireGuardPubKeyAnnotation]
	}

	if wgPubKey == "" {
		return false
	}

	return true
}

// preferredGatewayPoolForNode returns the deterministic pool assignment for a node.
// A node may only belong to one pool, so the lexicographically first eligible
// pool name wins.
func (gc *GatewayPoolController) preferredGatewayPoolForNode(node *corev1.Node) (string, bool) {
	if node == nil {
		return "", false
	}

	gc.poolsCacheLock.RLock()
	pools := make([]unboundednetv1alpha1.GatewayPool, len(gc.poolsCache))
	copy(pools, gc.poolsCache)
	gc.poolsCacheLock.RUnlock()

	eligible := make([]string, 0, len(pools))
	for i := range pools {
		pool := &pools[i]
		if nodeEligibleForGatewayPool(node, pool) {
			eligible = append(eligible, pool.Name)
		}
	}

	if len(eligible) == 0 {
		return "", false
	}

	sort.Strings(eligible)

	if len(eligible) > 1 {
		klog.V(2).Infof("Node %s matches multiple gateway pools %v, selecting %s", node.Name, eligible, eligible[0])
	}

	return eligible[0], true
}

// getNodeExternalIPs returns the external IPs of a node.
func getNodeExternalIPs(node *corev1.Node) []string {
	var externalIPs []string

	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeExternalIP {
			externalIPs = append(externalIPs, addr.Address)
		}
	}

	return externalIPs
}

// getNodeInternalIPs returns the internal IPs of a node.
func getNodeInternalIPs(node *corev1.Node) []string {
	var internalIPs []string

	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			internalIPs = append(internalIPs, addr.Address)
		}
	}

	return internalIPs
}

// nodeExternalIPsEqual checks if two nodes have the same external IPs.
func nodeExternalIPsEqual(a, b *corev1.Node) bool {
	aIPs := getNodeExternalIPs(a)
	bIPs := getNodeExternalIPs(b)

	if len(aIPs) != len(bIPs) {
		return false
	}

	sort.Strings(aIPs)
	sort.Strings(bIPs)

	for i := range aIPs {
		if aIPs[i] != bIPs[i] {
			return false
		}
	}

	return true
}

// nodeInternalIPsEqual checks if two nodes have the same internal IPs.
func nodeInternalIPsEqual(a, b *corev1.Node) bool {
	aIPs := getNodeInternalIPs(a)
	bIPs := getNodeInternalIPs(b)

	if len(aIPs) != len(bIPs) {
		return false
	}

	sort.Strings(aIPs)
	sort.Strings(bIPs)

	for i := range aIPs {
		if aIPs[i] != bIPs[i] {
			return false
		}
	}

	return true
}

// gatewayNodesEqual checks if two gateway node slices are equal.
func gatewayNodesEqual(a, b []unboundednetv1alpha1.GatewayNodeInfo) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i].Name != b[i].Name ||
			a[i].SiteName != b[i].SiteName ||
			a[i].WireGuardPublicKey != b[i].WireGuardPublicKey ||
			a[i].GatewayWireguardPort != b[i].GatewayWireguardPort ||
			!stringSlicesEqual(a[i].InternalIPs, b[i].InternalIPs) ||
			!stringSlicesEqual(a[i].ExternalIPs, b[i].ExternalIPs) ||
			!stringSlicesEqual(a[i].HealthEndpoints, b[i].HealthEndpoints) ||
			!stringSlicesEqual(a[i].PodCIDRs, b[i].PodCIDRs) {
			return false
		}
	}

	return true
}

// getHealthIPsFromPodCIDRs returns health check IP addresses for each podCIDR.
// Returns the gateway IP (first usable IP) in each CIDR, e.g. "10.0.1.1" or "fd00::1".
// The node agent constructs the full health check URL from this IP + its configured port and path.
func getHealthIPsFromPodCIDRs(podCIDRs []string) []string {
	var endpoints []string

	for _, cidr := range podCIDRs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			klog.V(3).Infof("Invalid CIDR %s: %v", cidr, err)
			continue
		}

		// Get the first IP in the subnet (the gateway IP)
		ip := ipNet.IP
		result := make(net.IP, len(ip))
		copy(result, ip)

		// Increment the last byte to get .1 or ::1
		if ip4 := result.To4(); ip4 != nil {
			ip4[3]++
			endpoints = append(endpoints, ip4.String())
		} else {
			result[15]++
			endpoints = append(endpoints, result.String())
		}
	}

	return endpoints
}

func (gc *GatewayPoolController) ensureGatewayPoolNode(ctx context.Context, pool *unboundednetv1alpha1.GatewayPool, node *corev1.Node, siteName string) error {
	if pool == nil || node == nil {
		return nil
	}

	labelsMap := map[string]interface{}{
		"net.unbounded-kube.io/gateway-pool": pool.Name,
		"net.unbounded-kube.io/node":         node.Name,
	}
	if siteName != "" {
		labelsMap["net.unbounded-kube.io/site"] = siteName
	}

	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "net.unbounded-kube.io/v1alpha1",
		"kind":       "GatewayPoolNode",
		"metadata": map[string]interface{}{
			"name":   node.Name,
			"labels": labelsMap,
			"ownerReferences": []interface{}{
				map[string]interface{}{
					"apiVersion":         "v1",
					"kind":               "Node",
					"name":               node.Name,
					"uid":                string(node.UID),
					"controller":         true,
					"blockOwnerDeletion": false,
				},
				map[string]interface{}{
					"apiVersion":         "net.unbounded-kube.io/v1alpha1",
					"kind":               "GatewayPool",
					"name":               pool.Name,
					"uid":                string(pool.UID),
					"controller":         false,
					"blockOwnerDeletion": false,
				},
			},
		},
		"spec": map[string]interface{}{
			"nodeName":    node.Name,
			"gatewayPool": pool.Name,
			"site":        siteName,
		},
	}}

	existingObj, exists, err := gc.gatewayNodeInformer.GetStore().GetByKey(node.Name)
	if err != nil {
		return fmt.Errorf("failed to get GatewayPoolNode %s from cache: %w", node.Name, err)
	}

	if !exists {
		_, createErr := gc.dynamicClient.Resource(gatewayNodeGVR).Create(ctx, obj, metav1.CreateOptions{})
		return createErr
	}

	existing := existingObj.(*unstructured.Unstructured) //nolint:errcheck

	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"labels":          obj.GetLabels(),
			"ownerReferences": obj.GetOwnerReferences(),
		},
		"spec": obj.Object["spec"],
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return err
	}

	_, err = gc.dynamicClient.Resource(gatewayNodeGVR).Patch(ctx, existing.GetName(), types.MergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		if errors.IsConflict(err) {
			return nil
		}

		return err
	}

	return nil
}

func (gc *GatewayPoolController) cleanupStaleGatewayPoolNodes(ctx context.Context, pool *unboundednetv1alpha1.GatewayPool, matchingNodes []unboundednetv1alpha1.GatewayNodeInfo) error {
	if pool == nil || pool.Name == "" {
		return nil
	}

	poolName := pool.Name
	selector := labels.SelectorFromSet(pool.Spec.NodeSelector)

	keep := make(map[string]struct{}, len(matchingNodes))
	for _, n := range matchingNodes {
		if n.Name != "" {
			keep[n.Name] = struct{}{}
		}
	}

	allItems := gc.gatewayNodeInformer.GetStore().List()

	for _, obj := range allItems {
		item, ok := obj.(*unstructured.Unstructured)
		if !ok {
			continue
		}

		specPool, found, _ := unstructured.NestedString(item.Object, "spec", "gatewayPool") //nolint:errcheck
		if !found || specPool != poolName {
			continue
		}

		name := item.GetName()
		if _, ok := keep[name]; ok {
			continue
		}

		// Be conservative with deletes: if the backing node still exists and still
		// matches this pool selector, keep the GatewayPoolNode to avoid transient
		// delete/recreate churn while node annotations/IPs converge.
		if node, err := gc.nodeLister.Get(name); err == nil && node != nil {
			if selector.Matches(labels.Set(node.Labels)) {
				if preferredPool, ok := gc.preferredGatewayPoolForNode(node); ok && preferredPool == poolName {
					klog.V(2).Infof("GatewayPool %s: preserving GatewayPoolNode %s during transient eligibility changes", poolName, name)
					continue
				}
			}
		}

		if err := gc.dynamicClient.Resource(gatewayNodeGVR).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
			klog.Warningf("GatewayPool %s: failed deleting stale GatewayPoolNode %s: %v", poolName, name, err)
		}
	}

	return nil
}
