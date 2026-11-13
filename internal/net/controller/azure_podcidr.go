// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	corev1 "k8s.io/api/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/Azure/unbounded/internal/net/azurepodcidr"
)

func nodeForAzurePodCIDREvent(obj any, nodes corelisters.NodeLister) *corev1.Node {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		return nil
	}

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil || namespace != "kube-system" {
		return nil
	}

	node, err := nodes.Get(name)
	if err != nil || !azurepodcidr.UsesManagedCilium(node) || len(node.Spec.PodCIDRs) > 0 {
		return nil
	}

	return node
}

func (sc *SiteController) handleAzurePodCIDRChange(obj any) {
	if node := nodeForAzurePodCIDREvent(obj, sc.nodeLister); node != nil {
		sc.markSiteSlicesDirty(sc.nodeSliceSites(node)...)
	}
}

func (gc *GatewayPoolController) handleAzurePodCIDRChange(obj any) {
	if node := nodeForAzurePodCIDREvent(obj, gc.nodeLister); node != nil {
		gc.enqueuePoolsForNodes(node)
		gc.workqueue.Add(gatewayAssignmentsQueueKey)
	}
}
