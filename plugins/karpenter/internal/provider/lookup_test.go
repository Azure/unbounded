// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/Azure/unbounded/plugins/karpenter/internal/capacity"
)

type missingAllocationCache struct {
	client.Reader
}

func (reader missingAllocationCache) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, allocation := object.(*unstructured.Unstructured); allocation {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "machineallocations"}, key.Name)
	}

	return reader.Reader.Get(ctx, key, object, options...)
}

func (reader missingAllocationCache) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	if allocations, ok := list.(*unstructured.UnstructuredList); ok {
		allocations.Items = nil

		return nil
	}

	return reader.Reader.List(ctx, list, options...)
}

type missingOnceReader struct {
	client.Reader
	missed bool
}

func (reader *missingOnceReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, allocation := object.(*unstructured.Unstructured); allocation && !reader.missed {
		reader.missed = true

		return apierrors.NewNotFound(schema.GroupResource{Resource: "machineallocations"}, key.Name)
	}

	return reader.Reader.Get(ctx, key, object, options...)
}

type unavailableReader struct {
	client.Reader
}

func (reader unavailableReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("API unavailable")
}

func TestAlreadyExistsAndStaleCacheRecoverTheOriginalAllocation(t *testing.T) {
	ctx := t.Context()
	original, store := testProvider(t, testClass())
	claim := testClaim()

	_, err := original.Create(ctx, claim)
	if err == nil {
		t.Fatal("expected pending allocation")
	}

	object, err := original.readAllocation(ctx, capacity.AllocationName(claim.UID))
	must(t, err)
	setAllocated(t, store, object)
	reader := &missingOnceReader{Reader: store}
	restarted, err := New(missingAllocationCache{Reader: store}, reader, store, Options{SnapshotWait: -1})
	must(t, err)

	hydrated, err := restarted.Create(ctx, claim)
	must(t, err)

	if hydrated.Status.ProviderID != "unbounded:///machineallocations/"+allocationTestUID {
		t.Fatal("AlreadyExists recovery did not retain the original identity")
	}

	inventory, err := restarted.Get(ctx, hydrated.Status.ProviderID)
	must(t, err)

	if inventory.Status.ProviderID != hydrated.Status.ProviderID {
		t.Fatal("uncached bounded UID-label lookup returned another allocation")
	}
}

func TestTransientLookupFailureIsNeverAbsence(t *testing.T) {
	_, store := testProvider(t, testClass())
	provider, err := New(
		missingAllocationCache{Reader: store},
		unavailableReader{Reader: store},
		store,
		Options{SnapshotWait: -1},
	)
	must(t, err)

	_, err = provider.Get(t.Context(), "unbounded:///machineallocations/"+allocationTestUID)
	if err == nil || cloudprovider.IsNodeClaimNotFoundError(err) {
		t.Fatal("an API failure must not be interpreted as released infrastructure")
	}
}

func TestStaleReleaseConditionDoesNotAuthorizeAbsence(t *testing.T) {
	ctx := t.Context()
	provider, store := testProvider(t, testClass())
	claim := testClaim()

	_, err := provider.Create(ctx, claim)
	if err == nil {
		t.Fatal("expected pending allocation")
	}

	object, err := provider.readAllocation(ctx, capacity.AllocationName(claim.UID))
	must(t, err)
	setAllocated(t, store, object)
	allocation, err := decodeAllocation(object)
	must(t, err)

	allocation.Generation = 2
	allocation.Spec.Lifecycle = "Released"
	allocation.Status.Conditions = append(allocation.Status.Conditions, metav1.Condition{
		Type:               "Released",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: 1,
		Reason:             "StaleObservation",
		LastTransitionTime: metav1.Now(),
	})
	writeAllocation(t, store, object, allocation)

	must(t, provider.Delete(ctx, claim))

	if _, err := provider.Get(ctx, allocation.Status.ProviderID); err != nil {
		t.Fatalf("unconfirmed release must retain infrastructure discovery: %v", err)
	}

	claims, err := provider.List(ctx)
	must(t, err)

	if len(claims) != 1 {
		t.Fatal("cached consumer index dropped a still-existing worker")
	}
}

func TestDriftRequiresSupportedNodeClaimIdentity(t *testing.T) {
	ctx := t.Context()
	provider, _ := testProvider(t, testClass())
	claim := testClaim()
	claim.Spec.NodeClassRef = nil

	if _, err := provider.IsDrifted(ctx, claim); err == nil {
		t.Fatal("missing MachineClass reference must fail drift observation")
	}

	claim = testClaim()
	claim.UID = ""

	if _, err := provider.IsDrifted(ctx, claim); err == nil {
		t.Fatal("missing consumer UID must fail drift observation")
	}
}
