// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestRegistryMutationLeaseSerializesHolders(t *testing.T) {
	client := fake.NewSimpleClientset()
	if err := acquireRegistryMutationLease(t.Context(), client, "gantry-system", "holder-a"); err != nil {
		t.Fatalf("acquire holder-a: %v", err)
	}

	blockedCtx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	if err := acquireRegistryMutationLease(blockedCtx, client, "gantry-system", "holder-b"); err == nil {
		t.Fatal("holder-b acquired a lease still owned by holder-a")
	}

	if err := releaseRegistryMutationLease(t.Context(), client, "gantry-system", "holder-a"); err != nil {
		t.Fatalf("release holder-a: %v", err)
	}

	if err := acquireRegistryMutationLease(t.Context(), client, "gantry-system", "holder-b"); err != nil {
		t.Fatalf("acquire holder-b after release: %v", err)
	}
}

func TestRegistryMutationLeaseRefusesUnownedCollision(t *testing.T) {
	client := fake.NewSimpleClientset(&coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
		Name: registryMutationLeaseName, Namespace: "gantry-system",
	}})

	err := acquireRegistryMutationLease(t.Context(), client, "gantry-system", "holder")
	if err == nil || !strings.Contains(err.Error(), "not owned by gantryctl") {
		t.Fatalf("want ownership refusal, got %v", err)
	}
}
