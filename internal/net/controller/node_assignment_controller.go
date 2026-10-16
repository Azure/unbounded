// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
)

const (
	NodeAgentPodNodeNameIndex = "unboundedNetNodeName"
	nodeAgentDaemonSetName    = "unbounded-net-node"
)

// NodeAssignmentController replaces only node-agent Pods whose acknowledged
// effective assignment differs from the desired labels on their Node.
type NodeAssignmentController struct {
	clientset kubernetes.Interface

	nodeLister corev1listers.NodeLister
	nodeSynced cache.InformerSynced
	podIndexer cache.Indexer
	podSynced  cache.InformerSynced

	queue workqueue.TypedRateLimitingInterface[string]
}

func NewNodeAssignmentController(
	clientset kubernetes.Interface,
	nodeInformer coreinformers.NodeInformer,
	podInformer coreinformers.PodInformer,
) (*NodeAssignmentController, error) {
	controller := &NodeAssignmentController{
		clientset:  clientset,
		nodeLister: nodeInformer.Lister(),
		nodeSynced: nodeInformer.Informer().HasSynced,
		podIndexer: podInformer.Informer().GetIndexer(),
		podSynced:  podInformer.Informer().HasSynced,
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "NodeAssignments"},
		),
	}

	if _, err := nodeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if node, ok := obj.(*corev1.Node); ok {
				controller.queue.Add(node.Name)
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldNode, oldOK := oldObj.(*corev1.Node)

			newNode, newOK := newObj.(*corev1.Node)
			if !oldOK || !newOK {
				return
			}

			if NodeSiteLabel(oldNode) != NodeSiteLabel(newNode) ||
				nodeGatewayPoolLabel(oldNode) != nodeGatewayPoolLabel(newNode) {
				controller.queue.Add(newNode.Name)
			}
		},
		DeleteFunc: func(obj interface{}) {
			if node := nodeFromDeleteEvent(obj); node != nil {
				controller.queue.Add(node.Name)
			}
		},
	}); err != nil {
		return nil, fmt.Errorf("add Node assignment event handler: %w", err)
	}

	if _, err := podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.enqueuePod,
		UpdateFunc: func(_, newObj interface{}) { controller.enqueuePod(newObj) },
		DeleteFunc: controller.enqueuePod,
	}); err != nil {
		return nil, fmt.Errorf("add node-agent Pod assignment event handler: %w", err)
	}

	return controller, nil
}

func NodeAgentPodNodeNameIndexFunc(obj interface{}) ([]string, error) {
	pod, ok := obj.(*corev1.Pod)
	if !ok || pod.Spec.NodeName == "" {
		return nil, nil
	}

	return []string{pod.Spec.NodeName}, nil
}

func (c *NodeAssignmentController) Run(ctx context.Context, workers int) error {
	defer c.queue.ShutDown()

	if !cache.WaitForCacheSync(ctx.Done(), c.nodeSynced, c.podSynced) {
		return fmt.Errorf("sync Node assignment controller caches")
	}

	for _, obj := range c.podIndexer.List() {
		c.enqueuePod(obj)
	}

	for i := 0; i < workers; i++ {
		go c.runWorker(ctx)
	}

	<-ctx.Done()

	return nil
}

func (c *NodeAssignmentController) runWorker(ctx context.Context) {
	for c.processNext(ctx) {
	}
}

func (c *NodeAssignmentController) processNext(ctx context.Context) bool {
	nodeName, shutdown := c.queue.Get()
	if shutdown {
		return false
	}
	defer c.queue.Done(nodeName)

	if err := c.syncNode(ctx, nodeName); err != nil {
		klog.Errorf("Failed to reconcile node-agent Pod assignment for Node %s: %v", nodeName, err)
		c.queue.AddRateLimited(nodeName)

		return true
	}

	c.queue.Forget(nodeName)

	return true
}

func (c *NodeAssignmentController) syncNode(ctx context.Context, nodeName string) error {
	node, err := c.nodeLister.Get(nodeName)
	if apierrors.IsNotFound(err) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("get Node from cache: %w", err)
	}

	podObjects, err := c.podIndexer.ByIndex(NodeAgentPodNodeNameIndex, nodeName)
	if err != nil {
		return fmt.Errorf("list node-agent Pods by Node: %w", err)
	}

	desiredSite := NodeSiteLabel(node)
	desiredGatewayPool := nodeGatewayPoolLabel(node)

	for _, obj := range podObjects {
		pod, ok := obj.(*corev1.Pod)
		if !ok || !isTargetNodeAgentPod(pod) || pod.DeletionTimestamp != nil {
			continue
		}

		effectiveSite, siteAcknowledged := pod.Annotations[unboundednetv1alpha1.EffectiveSiteAnnotation]

		effectiveGatewayPool, gatewayAcknowledged := pod.Annotations[unboundednetv1alpha1.EffectiveGatewayPoolAnnotation]
		if !siteAcknowledged || !gatewayAcknowledged {
			continue
		}

		if effectiveSite == desiredSite && effectiveGatewayPool == desiredGatewayPool {
			continue
		}

		if err := c.deleteStaleAgentPod(ctx, nodeName, pod); err != nil {
			return err
		}
	}

	return nil
}

// deleteStaleAgentPod revalidates a cached assignment mismatch against the
// live Node and Pod before deleting. The Node and Pod informers are separate,
// so an agent may already have acknowledged a Node update that the cache has
// not observed.
func (c *NodeAssignmentController) deleteStaleAgentPod(ctx context.Context, nodeName string, cachedPod *corev1.Pod) error {
	node, err := c.clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("get live Node %s before replacing node-agent Pod: %w", nodeName, err)
	}

	pod, err := c.clientset.CoreV1().Pods(cachedPod.Namespace).Get(ctx, cachedPod.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("get live node-agent Pod %s/%s: %w", cachedPod.Namespace, cachedPod.Name, err)
	}

	if pod.UID != cachedPod.UID || pod.DeletionTimestamp != nil || !isTargetNodeAgentPod(pod) {
		return nil
	}

	effectiveSite, siteAcknowledged := pod.Annotations[unboundednetv1alpha1.EffectiveSiteAnnotation]

	effectiveGatewayPool, gatewayAcknowledged := pod.Annotations[unboundednetv1alpha1.EffectiveGatewayPoolAnnotation]
	if !siteAcknowledged || !gatewayAcknowledged {
		return nil
	}

	desiredSite := NodeSiteLabel(node)

	desiredGatewayPool := nodeGatewayPoolLabel(node)
	if effectiveSite == desiredSite && effectiveGatewayPool == desiredGatewayPool {
		return nil
	}

	uid, resourceVersion := pod.UID, pod.ResourceVersion

	propagation := metav1.DeletePropagationBackground
	if err := c.clientset.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
		Preconditions:     &metav1.Preconditions{UID: &uid, ResourceVersion: &resourceVersion},
		PropagationPolicy: &propagation,
	}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete stale node-agent Pod %s/%s with UID %s: %w", pod.Namespace, pod.Name, pod.UID, err)
	}

	klog.Infof("Deleted node-agent Pod %s/%s to adopt Node %s assignment change (Site %q -> %q, GatewayPool %q -> %q)", pod.Namespace, pod.Name, nodeName, effectiveSite, desiredSite, effectiveGatewayPool, desiredGatewayPool)

	return nil
}

func (c *NodeAssignmentController) enqueuePod(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		tombstone, tombstoneOK := obj.(cache.DeletedFinalStateUnknown)
		if !tombstoneOK {
			return
		}

		pod, ok = tombstone.Obj.(*corev1.Pod)
		if !ok {
			return
		}
	}

	if pod.Spec.NodeName != "" {
		c.queue.Add(pod.Spec.NodeName)
	}
}

func nodeGatewayPoolLabel(node *corev1.Node) string {
	if node == nil || node.Labels == nil {
		return ""
	}

	return node.Labels[unboundednetv1alpha1.GatewayPoolLabelKey]
}

func isTargetNodeAgentPod(pod *corev1.Pod) bool {
	if pod == nil ||
		pod.Labels["app.kubernetes.io/name"] != nodeAgentDaemonSetName ||
		pod.Labels["app.kubernetes.io/component"] != "node" {
		return false
	}

	owner := metav1.GetControllerOf(pod)

	return owner != nil &&
		owner.APIVersion == appsv1.SchemeGroupVersion.String() &&
		owner.Kind == "DaemonSet" &&
		owner.Name == nodeAgentDaemonSetName
}
