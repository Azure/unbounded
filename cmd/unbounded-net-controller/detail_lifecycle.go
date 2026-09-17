// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"net"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

func (h *healthState) getDetailRequests() *nodeDetailRequests {
	h.detailMu.Lock()
	defer h.detailMu.Unlock()

	return h.detailRequests
}

func (h *healthState) stopDetailRequests() {
	h.detailMu.Lock()
	manager := h.detailRequests
	h.detailRequests = nil
	h.detailMu.Unlock()

	if manager != nil {
		manager.Close()
	}
}

// startDetailRequests is called once per leadership term, with that term's
// context and node informer. Neither hooks nor workers use the HTTP caller's
// context, so disconnecting a viewer does not cancel another viewer's request.
func (h *healthState) startDetailRequests(ctx context.Context, nodeInformer cache.SharedIndexInformer) (*nodeDetailRequests, error) {
	if nodeInformer == nil {
		return nil, errors.New("node detail requests require a node informer")
	}

	detailCache, err := newNodeDetailCache(h.statusDetailCacheTTL)
	if err != nil {
		return nil, err
	}

	lister := corev1listers.NewNodeLister(nodeInformer.GetIndexer())

	manager, err := newNodeDetailRequests(ctx, detailCache, h.statusDetailRequestTimeout, nodeDetailRequestHooks{
		Dispatch: h.dispatchNodeDetail,
		Resolve: func(name string) (types.UID, error) {
			node, err := lister.Get(name)
			if err != nil {
				return "", err
			}

			return node.UID, nil
		},
		Pull: func(ctx context.Context, name string) (*NodeStatusResponse, error) {
			node, err := lister.Get(name)
			if err != nil {
				return nil, err
			}

			for _, address := range node.Status.Addresses {
				if address.Type != corev1.NodeInternalIP || net.ParseIP(address.Address) == nil {
					continue
				}

				host := address.Address
				if net.ParseIP(host).To4() == nil {
					host = "[" + host + "]"
				}

				return fetchNodeStatus(ctx, host, h.nodeAgentHealthPort)
			}

			return nil, fmt.Errorf("node %q has no valid InternalIP", name)
		},
	})
	if err != nil {
		return nil, err
	}

	registration, err := nodeInformer.AddEventHandler(detailNodeEvents(manager))
	if err != nil {
		manager.Close()

		return nil, fmt.Errorf("register detail node invalidation: %w", err)
	}

	h.detailMu.Lock()
	if !h.isLeader.Load() || ctx.Err() != nil || h.detailRequests != nil {
		h.detailMu.Unlock()
		manager.Close()

		if err := nodeInformer.RemoveEventHandler(registration); err != nil {
			klog.Warningf("Removing node detail event handler: %v", err)
		}

		return nil, errors.New("detail request leadership is unavailable or already initialized")
	}

	h.detailRequests = manager
	h.detailMu.Unlock()

	go func() {
		<-manager.done

		if err := nodeInformer.RemoveEventHandler(registration); err != nil {
			klog.Warningf("Removing node detail event handler: %v", err)
		}

		h.detailMu.Lock()
		if h.detailRequests == manager {
			h.detailRequests = nil
		}
		h.detailMu.Unlock()
	}()

	return manager, nil
}

func detailNodeEvents(manager *nodeDetailRequests) cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj any) {
			oldNode, oldOK := oldObj.(*corev1.Node)

			newNode, newOK := newObj.(*corev1.Node)
			if oldOK && newOK && oldNode.UID != newNode.UID {
				manager.InvalidateNode(oldNode.Name, oldNode.UID)
			}
		},
		DeleteFunc: func(obj any) {
			if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = tombstone.Obj
			}

			if node, ok := obj.(*corev1.Node); ok {
				manager.InvalidateNode(node.Name, node.UID)
			}
		},
	}
}
