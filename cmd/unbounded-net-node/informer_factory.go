// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	clientwatchlist "k8s.io/client-go/util/watchlist"
)

func newNodeDynamicInformerFactory(
	client dynamic.Interface,
	resync time.Duration,
) dynamicinformer.DynamicSharedInformerFactory {
	return dynamicinformer.NewDynamicSharedInformerFactory(
		&watchTimeoutDynamicClient{Interface: client},
		resync,
	)
}

func doubleWatchTimeout(options *metav1.ListOptions) {
	if options == nil || options.TimeoutSeconds == nil || *options.TimeoutSeconds <= 0 {
		return
	}

	timeoutSeconds := *options.TimeoutSeconds * 2
	options.TimeoutSeconds = &timeoutSeconds
}

type watchTimeoutDynamicClient struct {
	dynamic.Interface
}

func (c *watchTimeoutDynamicClient) Resource(resource schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return &watchTimeoutNamespaceableResource{
		NamespaceableResourceInterface: c.Interface.Resource(resource),
	}
}

func (c *watchTimeoutDynamicClient) IsWatchListSemanticsUnSupported() bool {
	return clientwatchlist.DoesClientNotSupportWatchListSemantics(c.Interface)
}

type watchTimeoutNamespaceableResource struct {
	dynamic.NamespaceableResourceInterface
}

func (r *watchTimeoutNamespaceableResource) Namespace(namespace string) dynamic.ResourceInterface {
	return &watchTimeoutResource{
		ResourceInterface: r.NamespaceableResourceInterface.Namespace(namespace),
	}
}

func (r *watchTimeoutNamespaceableResource) Watch(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
	doubleWatchTimeout(&options)

	return r.NamespaceableResourceInterface.Watch(ctx, options)
}

type watchTimeoutResource struct {
	dynamic.ResourceInterface
}

func (r *watchTimeoutResource) Watch(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
	doubleWatchTimeout(&options)

	return r.ResourceInterface.Watch(ctx, options)
}

var (
	_ dynamic.ResourceInterface              = (*watchTimeoutResource)(nil)
	_ dynamic.NamespaceableResourceInterface = (*watchTimeoutNamespaceableResource)(nil)
	_ dynamic.Interface                      = (*watchTimeoutDynamicClient)(nil)
)
