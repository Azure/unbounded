// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clientfeatures "k8s.io/client-go/features"
	clientfeaturestesting "k8s.io/client-go/features/testing"
	"k8s.io/client-go/tools/cache"
)

type recordedInformerRequest struct {
	verb    string
	options metav1.ListOptions
}

type recordingDynamicClient struct {
	dynamic.Interface

	mu       sync.Mutex
	requests []recordedInformerRequest
}

func (c *recordingDynamicClient) Resource(resource schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return &recordingNamespaceableResource{
		NamespaceableResourceInterface: c.Interface.Resource(resource),
		client:                         c,
	}
}

func (c *recordingDynamicClient) record(verb string, options metav1.ListOptions) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.requests = append(c.requests, recordedInformerRequest{verb: verb, options: cloneListOptions(options)})
}

func (c *recordingDynamicClient) snapshot() []recordedInformerRequest {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]recordedInformerRequest(nil), c.requests...)
}

type recordingNamespaceableResource struct {
	dynamic.NamespaceableResourceInterface

	client *recordingDynamicClient
}

func (r *recordingNamespaceableResource) Namespace(namespace string) dynamic.ResourceInterface {
	return &recordingResource{
		ResourceInterface: r.NamespaceableResourceInterface.Namespace(namespace),
		client:            r.client,
	}
}

type recordingResource struct {
	dynamic.ResourceInterface

	client *recordingDynamicClient
}

func (r *recordingResource) List(ctx context.Context, options metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	r.client.record("list", options)

	return r.ResourceInterface.List(ctx, options)
}

func (r *recordingResource) Watch(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
	r.client.record("watch", options)

	if options.SendInitialEvents != nil {
		return nil, errors.New("watch-list unsupported")
	}

	return r.ResourceInterface.Watch(ctx, options)
}

func cloneListOptions(options metav1.ListOptions) metav1.ListOptions {
	cloned := options
	if options.TimeoutSeconds != nil {
		timeoutSeconds := *options.TimeoutSeconds
		cloned.TimeoutSeconds = &timeoutSeconds
	}

	if options.SendInitialEvents != nil {
		sendInitialEvents := *options.SendInitialEvents
		cloned.SendInitialEvents = &sendInitialEvents
	}

	return cloned
}

func TestNodeInformerFactoryWatchTimeoutRequests(t *testing.T) {
	clientfeaturestesting.SetFeatureDuringTest(t, clientfeatures.WatchListClient, true)

	gvr := schema.GroupVersionResource{Group: "net.unbounded-cloud.io", Version: "v1alpha1", Resource: "testresources"}
	fakeClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "TestResourceList"},
	)
	client := &recordingDynamicClient{Interface: fakeClient}
	factory := newNodeDynamicInformerFactory(client, 0)
	informer := factory.ForResource(gvr).Informer()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	factory.Start(ctx.Done())

	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		t.Fatal("informer cache did not sync")
	}

	deadline := time.Now().Add(time.Second)

	for {
		requests := client.snapshot()
		if assertInformerRequestOptions(t, requests) {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("did not observe watch-list, LIST, and watch requests: %#v", requests)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

func assertInformerRequestOptions(t *testing.T, requests []recordedInformerRequest) bool {
	t.Helper()

	var watchList, list, normalWatch *metav1.ListOptions

	for i := range requests {
		request := &requests[i]
		switch {
		case request.verb == "watch" && request.options.SendInitialEvents != nil:
			watchList = &request.options
		case request.verb == "list":
			list = &request.options
		case request.verb == "watch":
			normalWatch = &request.options
		}
	}

	if watchList == nil || list == nil || normalWatch == nil {
		return false
	}

	assertDoubledWatchTimeout(t, "watch-list", *watchList)

	if watchList.SendInitialEvents == nil || !*watchList.SendInitialEvents {
		t.Fatal("watch-list request did not preserve sendInitialEvents=true")
	}

	if !watchList.AllowWatchBookmarks || watchList.ResourceVersionMatch != metav1.ResourceVersionMatchNotOlderThan {
		t.Fatalf("watch-list request fields changed: %#v", *watchList)
	}

	if list.Watch || list.TimeoutSeconds != nil {
		t.Fatalf("normal LIST timeout changed: %#v", *list)
	}

	assertDoubledWatchTimeout(t, "watch", *normalWatch)

	if !normalWatch.AllowWatchBookmarks || normalWatch.SendInitialEvents != nil {
		t.Fatalf("watch request fields changed: %#v", *normalWatch)
	}

	return true
}

func assertDoubledWatchTimeout(t *testing.T, requestType string, options metav1.ListOptions) {
	t.Helper()

	if options.TimeoutSeconds == nil {
		t.Fatalf("%s request has no timeoutSeconds", requestType)
	}

	if timeout := *options.TimeoutSeconds; timeout < 600 || timeout >= 1200 {
		t.Fatalf("%s timeoutSeconds = %d, want randomized [600,1200)", requestType, timeout)
	}
}
