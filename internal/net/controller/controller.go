// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package controller implements the Kubernetes node controller for CIDR allocation.
package controller

import (
	"context"
	"fmt"
	"regexp"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/Azure/unbounded/internal/net/allocator"
)

const (
	// maxRetries is the maximum number of retries for processing a node.
	maxRetries = 5
)

// Controller manages CIDR allocation for Kubernetes nodes.
type Controller struct {
	clientset  kubernetes.Interface
	nodeLister corev1listers.NodeLister
	nodeSynced cache.InformerSynced
	workqueue  workqueue.TypedRateLimitingInterface[string]
	allocator  *allocator.Allocator
	nodeRegex  *regexp.Regexp
	recorder   record.EventRecorder

	// pendingReleases stores CIDRs that need to be released when a deleted
	// node is processed via the workqueue, preventing a race with direct
	// release in the delete event handler.
	pendingReleases     map[string][]string
	pendingReleasesLock sync.Mutex
}

// InformerSynced returns true if the informer cache has been synced.
func (c *Controller) InformerSynced() bool {
	return c.nodeSynced()
}

// NewController creates a new node CIDR controller.
// If nodeRegexPattern is non-empty, only nodes matching the regex will be processed.
func NewController(
	clientset kubernetes.Interface,
	informerFactory informers.SharedInformerFactory,
	alloc *allocator.Allocator,
	nodeRegexPattern string,
	recorder record.EventRecorder,
) (*Controller, error) {
	nodeInformer := informerFactory.Core().V1().Nodes()

	var (
		nodeRegex *regexp.Regexp
		err       error
	)

	if nodeRegexPattern != "" {
		nodeRegex, err = regexp.Compile(nodeRegexPattern)
		if err != nil {
			return nil, fmt.Errorf("invalid node regex pattern %q: %w", nodeRegexPattern, err)
		}

		klog.V(2).Infof("Node filter regex: %s", nodeRegexPattern)
	}

	c := &Controller{
		clientset:       clientset,
		nodeLister:      nodeInformer.Lister(),
		nodeSynced:      nodeInformer.Informer().HasSynced,
		workqueue:       workqueue.NewTypedRateLimitingQueueWithConfig(workqueue.DefaultTypedControllerRateLimiter[string](), workqueue.TypedRateLimitingQueueConfig[string]{Name: "Nodes"}),
		allocator:       alloc,
		nodeRegex:       nodeRegex,
		recorder:        recorder,
		pendingReleases: make(map[string][]string),
	}

	// Set up event handlers
	if _, err := nodeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			node := obj.(*corev1.Node) //nolint:errcheck
			klog.Infof("Node added: %s", node.Name)
			c.enqueueNode(obj)
		},
		UpdateFunc: func(old, new interface{}) {
			c.enqueueNode(new)
		},
		DeleteFunc: func(obj interface{}) {
			// Handle deleted nodes - defer CIDR release to the workqueue
			var node *corev1.Node

			switch t := obj.(type) {
			case *corev1.Node:
				node = t
			case cache.DeletedFinalStateUnknown:
				var ok bool

				node, ok = t.Obj.(*corev1.Node)
				if !ok {
					klog.Errorf("DeletedFinalStateUnknown contained non-Node object: %#v", t.Obj)
					return
				}
			default:
				klog.Errorf("Delete event contained non-Node object: %#v", obj)
				return
			}

			klog.Infof("Node deleted: %s", node.Name)

			// Store CIDRs for deferred release via the workqueue
			var cidrs []string
			if node.Spec.PodCIDR != "" {
				cidrs = append(cidrs, node.Spec.PodCIDR)
			}

			cidrs = append(cidrs, node.Spec.PodCIDRs...)
			if len(cidrs) > 0 {
				c.pendingReleasesLock.Lock()
				c.pendingReleases[node.Name] = cidrs
				c.pendingReleasesLock.Unlock()
			}

			c.enqueueNode(obj)
		},
	}); err != nil {
		return nil, fmt.Errorf("add node informer event handler: %w", err)
	}

	return c, nil
}

// enqueueNode adds a node to the workqueue.
func (c *Controller) enqueueNode(obj interface{}) {
	var (
		key string
		err error
	)

	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		klog.Errorf("Error getting key for object: %v", err)
		return
	}

	c.workqueue.Add(key)
}

// matchesNodeFilter returns true if the node name matches the configured regex filter.
// If no filter is configured, all nodes match.
func (c *Controller) matchesNodeFilter(nodeName string) bool {
	if c.nodeRegex == nil {
		return true
	}

	matches := c.nodeRegex.MatchString(nodeName)
	if !matches {
		klog.V(4).Infof("Node %s does not match filter regex, skipping", nodeName)
	}

	return matches
}

// Run starts the controller.
func (c *Controller) Run(ctx context.Context, workers int) error {
	defer c.workqueue.ShutDown()

	klog.Info("Starting node CIDR controller")

	// Wait for caches to sync
	klog.Info("Waiting for informer caches to sync")

	if ok := cache.WaitForCacheSync(ctx.Done(), c.nodeSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	klog.Info("Starting workers")

	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, c.runWorker, time.Second)
	}

	klog.Info("Controller started")
	<-ctx.Done()
	klog.Info("Shutting down controller")

	return nil
}

// runWorker processes items from the workqueue.
func (c *Controller) runWorker(ctx context.Context) {
	for c.processNextWorkItem(ctx) {
	}
}

// processNextWorkItem processes a single item from the workqueue.
func (c *Controller) processNextWorkItem(ctx context.Context) bool {
	key, shutdown := c.workqueue.Get()
	if shutdown {
		return false
	}
	defer c.workqueue.Done(key)

	start := time.Now()

	err := c.syncHandler(ctx, key)
	if err != nil {
		if c.workqueue.NumRequeues(key) < maxRetries {
			c.workqueue.AddRateLimited(key)
			workqueueRetries.WithLabelValues("Nodes").Inc()

			err = fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
		} else {
			c.workqueue.Forget(key)
			klog.Errorf("Dropping node %q out of the queue after %d retries: %v", key, maxRetries, err)
		}
	} else {
		c.workqueue.Forget(key)
	}

	duration := time.Since(start).Seconds()
	reconciliationDuration.WithLabelValues("Nodes").Observe(duration)

	if err != nil {
		reconciliationErrors.WithLabelValues("Nodes").Inc()
		reconciliationTotal.WithLabelValues("Nodes", "error").Inc()
		klog.Error(err)
	} else {
		reconciliationTotal.WithLabelValues("Nodes", "success").Inc()
	}

	return true
}

// syncHandler processes a node and allocates CIDRs if needed.
func (c *Controller) syncHandler(ctx context.Context, key string) error {
	node, err := c.nodeLister.Get(key)
	if err != nil {
		if errors.IsNotFound(err) {
			klog.V(4).Infof("Node %s has been deleted", key)
			// Release any pending CIDRs for this deleted node
			c.pendingReleasesLock.Lock()

			cidrs, exists := c.pendingReleases[key]
			if exists {
				delete(c.pendingReleases, key)
			}
			c.pendingReleasesLock.Unlock()

			if exists {
				for _, cidr := range cidrs {
					c.allocator.Release(cidr)
					PodCIDRReleases.Inc()
				}

				klog.Infof("Released CIDRs %v for deleted node %s via workqueue", cidrs, key)
			}

			return nil
		}

		return err
	}

	// Check if node matches the filter
	if !c.matchesNodeFilter(node.Name) {
		return nil
	}

	// Check if node already has podCIDRs assigned (from cache - fast path)
	if len(node.Spec.PodCIDRs) > 0 || node.Spec.PodCIDR != "" {
		// Mark existing CIDRs as allocated (idempotent)
		if node.Spec.PodCIDR != "" {
			c.allocator.MarkAllocated(node.Spec.PodCIDR)
		}

		for _, cidr := range node.Spec.PodCIDRs {
			c.allocator.MarkAllocated(cidr)
		}

		klog.V(4).Infof("Node %s already has podCIDRs assigned: %v", node.Name, node.Spec.PodCIDRs)

		return nil
	}

	// Node needs CIDRs - do a direct API fetch to get the latest state
	// This prevents double-allocation when the cache is stale
	freshNode, err := c.clientset.CoreV1().Nodes().Get(ctx, key, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			klog.V(4).Infof("Node %s has been deleted", key)
			return nil
		}

		return err
	}

	// Re-check with fresh data
	if len(freshNode.Spec.PodCIDRs) > 0 || freshNode.Spec.PodCIDR != "" {
		// Node was updated between cache read and API fetch - mark as allocated
		if freshNode.Spec.PodCIDR != "" {
			c.allocator.MarkAllocated(freshNode.Spec.PodCIDR)
		}

		for _, cidr := range freshNode.Spec.PodCIDRs {
			c.allocator.MarkAllocated(cidr)
		}

		klog.V(2).Infof("Node %s already has podCIDRs assigned (confirmed from API): %v", freshNode.Name, freshNode.Spec.PodCIDRs)

		return nil
	}

	// Allocate CIDRs for this node
	return c.allocateCIDRsForNode(ctx, freshNode)
}

// allocateCIDRsForNode allocates and assigns CIDRs to a node.
func (c *Controller) allocateCIDRsForNode(ctx context.Context, node *corev1.Node) error {
	var (
		podCIDR  string
		podCIDRs []string
	)

	hasIPv4 := c.allocator.HasIPv4Pools()
	hasIPv6 := c.allocator.HasIPv6Pools()

	if hasIPv4 {
		ipv4CIDR, err := c.allocator.AllocateIPv4()
		if err != nil {
			PodCIDRExhaustion.Inc()
			klog.Errorf("Failed to allocate IPv4 CIDR for node %s: %v -- CIDR pool exhausted", node.Name, err)

			if c.recorder != nil {
				c.recorder.Eventf(node, corev1.EventTypeWarning, "CIDRExhausted", "Failed to allocate IPv4 CIDR: %v", err)
			}

			return fmt.Errorf("failed to allocate IPv4 CIDR for node %s: %w", node.Name, err)
		}

		podCIDR = ipv4CIDR
		podCIDRs = append(podCIDRs, ipv4CIDR)

		PodCIDRAllocations.Inc()
		klog.Infof("Allocated IPv4 CIDR %s for node %s", ipv4CIDR, node.Name)
	}

	if hasIPv6 {
		ipv6CIDR, err := c.allocator.AllocateIPv6()
		if err != nil {
			PodCIDRExhaustion.Inc()
			klog.Errorf("Failed to allocate IPv6 CIDR for node %s: %v -- CIDR pool exhausted", node.Name, err)

			if c.recorder != nil {
				c.recorder.Eventf(node, corev1.EventTypeWarning, "CIDRExhausted", "Failed to allocate IPv6 CIDR: %v", err)
			}

			return fmt.Errorf("failed to allocate IPv6 CIDR for node %s: %w", node.Name, err)
		}

		if podCIDR == "" {
			podCIDR = ipv6CIDR
		}

		podCIDRs = append(podCIDRs, ipv6CIDR)

		PodCIDRAllocations.Inc()
		klog.Infof("Allocated IPv6 CIDR %s for node %s", ipv6CIDR, node.Name)
	}

	// Patch the node with the allocated CIDRs
	if err := c.patchNodeCIDRs(ctx, node.Name, podCIDR, podCIDRs); err != nil {
		return err
	}

	// Emit success event
	if c.recorder != nil {
		c.recorder.Eventf(node, corev1.EventTypeNormal, "CIDRAssigned", "Assigned podCIDRs %v", podCIDRs)
	}

	return nil
}

// patchNodeCIDRs patches a node's spec with the allocated CIDRs.
func (c *Controller) patchNodeCIDRs(ctx context.Context, nodeName, podCIDR string, podCIDRs []string) error {
	// Build the patch
	podCIDRsJSON := "["

	for i, cidr := range podCIDRs {
		if i > 0 {
			podCIDRsJSON += ","
		}

		podCIDRsJSON += fmt.Sprintf("%q", cidr)
	}

	podCIDRsJSON += "]"

	patch := fmt.Sprintf(`{"spec":{"podCIDR":%q,"podCIDRs":%s}}`, podCIDR, podCIDRsJSON)

	_, err := c.clientset.CoreV1().Nodes().Patch(
		ctx,
		nodeName,
		types.StrategicMergePatchType,
		[]byte(patch),
		metav1.PatchOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to patch node %s: %w", nodeName, err)
	}

	klog.Infof("Successfully assigned podCIDR=%s, podCIDRs=%v to node %s", podCIDR, podCIDRs, nodeName)

	return nil
}

// InitializeAllocator scans all existing nodes and marks their CIDRs as allocated.
// Note: This marks CIDRs from ALL nodes as allocated, regardless of the node filter,
// to avoid allocating CIDRs that are already in use by nodes outside the filter.
func (c *Controller) InitializeAllocator(ctx context.Context) error {
	nodes, err := c.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}

	matchingNodes := 0

	for _, node := range nodes.Items {
		if node.Spec.PodCIDR != "" {
			c.allocator.MarkAllocated(node.Spec.PodCIDR)
			klog.V(2).Infof("Marked existing CIDR %s as allocated (node %s)", node.Spec.PodCIDR, node.Name)
		}

		for _, cidr := range node.Spec.PodCIDRs {
			c.allocator.MarkAllocated(cidr)
			klog.V(2).Infof("Marked existing CIDR %s as allocated (node %s)", cidr, node.Name)
		}

		if c.matchesNodeFilter(node.Name) {
			matchingNodes++
		}
	}

	if c.nodeRegex != nil {
		klog.Infof("Initialized allocator with %d existing nodes (%d matching filter)", len(nodes.Items), matchingNodes)
	} else {
		klog.Infof("Initialized allocator with %d existing nodes", len(nodes.Items))
	}

	return nil
}

// DryRun performs a single evaluation pass and prints proposed changes without applying them.
func (c *Controller) DryRun(ctx context.Context) error {
	nodes, err := c.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}

	// First pass: mark existing allocations from ALL nodes (regardless of filter)
	for _, node := range nodes.Items {
		if node.Spec.PodCIDR != "" {
			c.allocator.MarkAllocated(node.Spec.PodCIDR)
		}

		for _, cidr := range node.Spec.PodCIDRs {
			c.allocator.MarkAllocated(cidr)
		}
	}

	// Second pass: calculate proposed allocations (only for matching nodes)
	hasChanges := false

	for _, node := range nodes.Items {
		// Check if node matches the filter
		if !c.matchesNodeFilter(node.Name) {
			klog.V(3).Infof("[DRY-RUN] Node %s: skipped (does not match filter)", node.Name)
			continue
		}

		if len(node.Spec.PodCIDRs) > 0 || node.Spec.PodCIDR != "" {
			klog.Infof("[DRY-RUN] Node %s: already has podCIDR=%s, podCIDRs=%v (no changes)", node.Name, node.Spec.PodCIDR, node.Spec.PodCIDRs)
			continue
		}

		var (
			podCIDR  string
			podCIDRs []string
		)

		hasIPv4 := c.allocator.HasIPv4Pools()
		hasIPv6 := c.allocator.HasIPv6Pools()

		if hasIPv4 {
			ipv4CIDR, err := c.allocator.AllocateIPv4()
			if err != nil {
				klog.Errorf("[DRY-RUN] Node %s: failed to allocate IPv4 CIDR: %v", node.Name, err)
				return fmt.Errorf("IPv4 CIDR pool exhausted")
			}

			podCIDR = ipv4CIDR
			podCIDRs = append(podCIDRs, ipv4CIDR)
		}

		if hasIPv6 {
			ipv6CIDR, err := c.allocator.AllocateIPv6()
			if err != nil {
				klog.Errorf("[DRY-RUN] Node %s: failed to allocate IPv6 CIDR: %v", node.Name, err)
				return fmt.Errorf("IPv6 CIDR pool exhausted")
			}

			if podCIDR == "" {
				podCIDR = ipv6CIDR
			}

			podCIDRs = append(podCIDRs, ipv6CIDR)
		}

		fmt.Printf("[DRY-RUN] Node %s: would assign podCIDR=%s, podCIDRs=%v\n", node.Name, podCIDR, podCIDRs)

		hasChanges = true
	}

	if !hasChanges {
		fmt.Println("[DRY-RUN] No changes needed - all matching nodes already have podCIDRs assigned")
	}

	return nil
}
