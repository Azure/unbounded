// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubescheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers/nodeclaim/lifecycle"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/state/nodepoolhealth"

	"github.com/Azure/unbounded/plugins/karpenter/internal/capacity"
	"github.com/Azure/unbounded/plugins/karpenter/internal/nodeclass"
)

func TestPinnedUpstreamLifecycleRecoversAndRegistersTheAllocation(t *testing.T) {
	nodeclass.AddToScheme(kubescheme.Scheme)

	scheme := runtime.NewScheme()
	must(t, corev1.AddToScheme(scheme))
	nodeclass.AddToScheme(scheme)
	scheme.AddKnownTypes(
		schema.GroupVersion{Group: "karpenter.sh", Version: "v1"},
		&v1.NodeClaim{}, &v1.NodeClaimList{},
		&v1.NodePool{}, &v1.NodePoolList{},
	)
	store := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(testClass()).
		WithStatusSubresource(&v1.NodeClaim{}).
		WithIndex(&corev1.Node{}, "spec.providerID", func(object client.Object) []string {
			node, ok := object.(*corev1.Node)
			if !ok {
				return nil
			}

			return []string{node.Spec.ProviderID}
		}).
		Build()
	provider, err := New(store, store, store, Options{SnapshotWait: -1})
	must(t, err)

	ctx := options.ToContext(t.Context(), &options.Options{IgnoreDRARequests: true})
	claim := testClaim()
	claim.CreationTimestamp = metav1.NewTime(time.Now())
	must(t, store.Create(ctx, claim))

	controller := upstreamLifecycle(store, provider)

	_, err = controller.Reconcile(ctx, claim)
	if err == nil || claim.StatusConditions().Get(v1.ConditionTypeLaunched).IsTrue() {
		t.Fatal("upstream launch must remain retryable until the allocation has a snapshot")
	}

	object, err := provider.readAllocation(ctx, capacity.AllocationName(claim.UID))
	must(t, err)
	// The snapshot alone, before any host exists, is what Karpenter records.
	setSnapshot(t, store, object)
	must(t, store.Get(ctx, client.ObjectKeyFromObject(claim), claim))

	// Discard upstream's launch cache as well as the provider instance.
	restarted, err := New(store, store, store, Options{SnapshotWait: -1})
	must(t, err)

	controller = upstreamLifecycle(store, restarted)
	_, err = controller.Reconcile(ctx, claim)
	must(t, err)
	must(t, store.Get(ctx, client.ObjectKeyFromObject(claim), claim))

	if !claim.StatusConditions().Get(v1.ConditionTypeLaunched).IsTrue() ||
		claim.Status.ProviderID != "unbounded:///machineallocations/"+allocationTestUID {
		t.Fatal("pinned upstream lifecycle did not recover the durable allocation")
	}

	node := lifecycleNode(claim)
	must(t, store.Create(ctx, node))
	must(t, store.Get(ctx, client.ObjectKeyFromObject(claim), claim))
	_, err = controller.Reconcile(ctx, claim)
	must(t, err)
	must(t, store.Get(ctx, client.ObjectKeyFromObject(claim), claim))

	if !claim.StatusConditions().Get(v1.ConditionTypeRegistered).IsTrue() ||
		!claim.StatusConditions().Get(v1.ConditionTypeInitialized).IsTrue() {
		t.Fatal("pinned upstream lifecycle did not register and initialize the worker")
	}

	must(t, store.Get(ctx, client.ObjectKeyFromObject(node), node))

	if node.Spec.ProviderID != claim.Status.ProviderID ||
		node.Labels[v1.CapacityTypeLabelKey] != v1.CapacityTypeOnDemand {
		t.Fatal("registered Node did not receive the confirmed identity and capacity labels")
	}

	for _, taint := range node.Spec.Taints {
		if taint.MatchTaint(&v1.UnregisteredNoExecuteTaint) {
			t.Fatal("upstream registration must own removal of the unregistered startup taint")
		}
	}
}

func upstreamLifecycle(store client.Client, provider *Provider) *lifecycle.Controller {
	return lifecycle.NewController(
		clock.RealClock{},
		store,
		provider,
		events.NewRecorder(record.NewFakeRecorder(100)),
		state.NewCluster(clock.RealClock{}, store, provider),
		nodepoolhealth.NewState(),
		nil,
	)
}

func lifecycleNode(claim *v1.NodeClaim) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ub-46ed7c08437e45beae8ba13f73d17128",
			UID:  "503dc926-a8b0-4093-8b47-876cd4b6d771",
		},
		Spec: corev1.NodeSpec{
			ProviderID: claim.Status.ProviderID,
			Taints:     []corev1.Taint{v1.UnregisteredNoExecuteTaint},
		},
		Status: corev1.NodeStatus{
			Capacity:    claim.Status.Capacity.DeepCopy(),
			Allocatable: claim.Status.Allocatable.DeepCopy(),
			Conditions: []corev1.NodeCondition{
				{
					Type:               corev1.NodeReady,
					Status:             corev1.ConditionTrue,
					LastTransitionTime: metav1.Now(),
				},
			},
		},
	}
}
