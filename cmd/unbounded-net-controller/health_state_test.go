// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1unstructured "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/informers"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
)

// TestHealthStateSetInformersAndTokenAuthStatus tests HealthStateSetInformersAndTokenAuthStatus.
func TestHealthStateSetInformersAndTokenAuthStatus(t *testing.T) {
	h := &healthState{}
	if ok, reason := h.tokenAuthStatus(); ok || !strings.Contains(reason, "not initialized") {
		t.Fatalf("expected token auth status to report uninitialized authenticator, got ok=%v reason=%q", ok, reason)
	}

	client := k8sfake.NewClientset()
	informerFactory := informers.NewSharedInformerFactory(client, 0)
	nodeLister := informerFactory.Core().V1().Nodes().Lister()
	podLister := informerFactory.Core().V1().Pods().Lister()

	siteInformer := cache.NewSharedIndexInformer(&cache.ListWatch{}, &metav1unstructured.Unstructured{}, 0, cache.Indexers{})
	gatewayPoolInformer := cache.NewSharedIndexInformer(&cache.ListWatch{}, &metav1unstructured.Unstructured{}, 0, cache.Indexers{})
	sitePeeringInformer := cache.NewSharedIndexInformer(&cache.ListWatch{}, &metav1unstructured.Unstructured{}, 0, cache.Indexers{})
	assignmentInformer := cache.NewSharedIndexInformer(&cache.ListWatch{}, &metav1unstructured.Unstructured{}, 0, cache.Indexers{})
	poolPeeringInformer := cache.NewSharedIndexInformer(&cache.ListWatch{}, &metav1unstructured.Unstructured{}, 0, cache.Indexers{})

	h.setInformers(
		nodeLister,
		podLister,
		siteInformer,
		gatewayPoolInformer,
		sitePeeringInformer,
		assignmentInformer,
		poolPeeringInformer,
	)

	if h.siteInformer != siteInformer || h.gatewayPoolInformer != gatewayPoolInformer || h.sitePeeringInformer != sitePeeringInformer || h.assignmentInformer != assignmentInformer || h.poolPeeringInformer != poolPeeringInformer {
		t.Fatalf("expected setInformers to assign informer references")
	}

	if h.nodeLister == nil || h.podLister == nil {
		t.Fatalf("expected typed listers to be assigned")
	}
}

// TestHealthStateHelpersAndLeaderInfo tests HealthStateHelpersAndLeaderInfo.
func TestHealthStateHelpersAndLeaderInfo(t *testing.T) {
	holder := "leader-pod"
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: "leader-lock", Namespace: "kube-system"},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder},
	}
	client := k8sfake.NewClientset(lease)
	client.Discovery().(*fake.FakeDiscovery).FakedServerVersion = &version.Info{GitVersion: "v1.30.0"}

	h := &healthState{
		clientset:          client,
		leaderElectionNS:   "kube-system",
		leaderElectionName: "leader-lock",
		nodeName:           "node-a",
		tokenAuth:          &tokenAuthenticator{tokenReviewer: client},
	}

	h.setLeader(true)

	if !h.isLeader.Load() {
		t.Fatalf("expected leader flag to be true")
	}

	h.setLeader(false)

	if h.isLeader.Load() {
		t.Fatalf("expected leader flag to be false")
	}

	if !h.isHealthy(context.Background()) {
		t.Fatalf("expected healthy with fake discovery server version")
	}

	if !h.isReady(context.Background()) {
		t.Fatalf("expected ready with fake discovery server version")
	}

	t.Setenv("POD_NAME", "leader-pod")

	info, err := h.getLeaderInfo(context.Background())
	if err != nil {
		t.Fatalf("getLeaderInfo returned error: %v", err)
	}

	if info.PodName != "leader-pod" || info.NodeName != "node-a" {
		t.Fatalf("unexpected leader info: %#v", info)
	}
}

// TestHealthStateReadinessFailure tests HealthStateReadinessFailure.
func TestHealthStateReadinessFailure(t *testing.T) {
	client := k8sfake.NewClientset()
	client.Discovery().(*fake.FakeDiscovery).PrependReactor("get", "version", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("apiserver unavailable")
	})

	h := &healthState{
		clientset: client,
		tokenAuth: &tokenAuthenticator{tokenReviewer: client},
	}

	if h.isHealthy(context.Background()) {
		t.Fatalf("expected isHealthy=false when discovery version lookup fails")
	}

	if h.isReady(context.Background()) {
		t.Fatalf("expected isReady=false when discovery version lookup fails")
	}
}

// TestUpdateAndClearServiceEndpoints tests UpdateAndClearServiceEndpoints.
func TestUpdateAndClearServiceEndpoints(t *testing.T) {
	client := k8sfake.NewClientset(&corev1.Endpoints{ObjectMeta: metav1.ObjectMeta{Name: "unbounded-net-controller", Namespace: "kube-system"}}) //nolint:staticcheck // testing cleanup of deprecated Endpoints resource
	h := &healthState{
		clientset:        client,
		healthPort:       9090,
		leaderElectionNS: "kube-system",
		podIP:            "10.20.30.40",
	}

	if err := h.updateServiceEndpoints(context.Background()); err != nil {
		t.Fatalf("updateServiceEndpoints error: %v", err)
	}

	if _, err := client.CoreV1().Endpoints("kube-system").Get(context.Background(), "unbounded-net-controller", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected stale Endpoints to be removed, got err=%v", err)
	}

	slice, err := client.DiscoveryV1().EndpointSlices("kube-system").Get(context.Background(), "unbounded-net-controller", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected endpoint slice to be created: %v", err)
	}

	if len(slice.Endpoints) != 1 || len(slice.Endpoints[0].Addresses) != 1 || slice.Endpoints[0].Addresses[0] != "10.20.30.40" {
		t.Fatalf("unexpected endpoint addresses: %#v", slice.Endpoints)
	}

	if len(slice.Ports) != 1 || slice.Ports[0].Port == nil || *slice.Ports[0].Port != 9090 {
		t.Fatalf("unexpected endpoint ports: %#v", slice.Ports)
	}

	if slice.AddressType != discoveryv1.AddressTypeIPv4 {
		t.Fatalf("unexpected address type: %s", slice.AddressType)
	}

	h.clearServiceEndpoints(context.Background())

	if _, err := client.DiscoveryV1().EndpointSlices("kube-system").Get(context.Background(), "unbounded-net-controller", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected endpoint slice to be deleted, got err=%v", err)
	}

	h.clearServiceEndpoints(context.Background())
}

// TestHealthStateGetLeaderInfoErrors tests HealthStateGetLeaderInfoErrors.
func TestHealthStateGetLeaderInfoErrors(t *testing.T) {
	client := k8sfake.NewClientset(&coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "leader-lock", Namespace: "kube-system"}})
	h := &healthState{clientset: client, leaderElectionNS: "kube-system", leaderElectionName: "leader-lock"}

	_, err := h.getLeaderInfo(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no leader currently elected") {
		t.Fatalf("expected no leader error, got %v", err)
	}

	h2 := &healthState{clientset: k8sfake.NewClientset(), leaderElectionNS: "kube-system", leaderElectionName: "missing"}

	_, err = h2.getLeaderInfo(context.Background())
	if err == nil || !strings.Contains(err.Error(), "failed to get leader lease") {
		t.Fatalf("expected get lease error, got %v", err)
	}
}
