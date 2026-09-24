// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package component

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestApplyExistingRequiresObservedIdentity(t *testing.T) {
	desired := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "retained", "namespace": "test"},
		"data": map[string]any{"key": "new"},
	}}
	base := desired.DeepCopy()
	base.SetUID("observed")
	base.SetResourceVersion("123")

	for _, tc := range []struct {
		name   string
		mutate func(*Operation)
	}{
		{"no base", func(op *Operation) { op.Base = nil }},
		{"no UID", func(op *Operation) { op.Base.SetUID("") }},
		{"no version", func(op *Operation) { op.Base.SetResourceVersion("") }},
		{"wrong target", func(op *Operation) { op.Base.SetName("other") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writes := 0
			env := &Env{Client: fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
				writes++
				return nil
			}}).Build()}
			op := Operation{Kind: OpApplyExisting, Object: desired.DeepCopy(), Base: base.DeepCopy(), Component: "retained"}
			tc.mutate(&op)

			plan := NewPlan()
			plan.Add(op)

			result, err := env.Execute(t.Context(), plan)
			if err != nil || result.Err() == nil || writes != 0 {
				t.Fatalf("invalid preconditions reached API: result=%+v error=%v writes=%d", result, err, writes)
			}
		})
	}
}

func TestApplyExistingTerminatingSnapshotIsWriteFree(t *testing.T) {
	now := metav1.Now()
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("apps/v1")
	obj.SetKind("Deployment")
	obj.SetName("retained")
	obj.SetUID("uid")
	obj.SetResourceVersion("123")
	obj.SetDeletionTimestamp(&now)

	plan := NewPlan()
	plan.Add(Operation{Kind: OpApplyExisting, Object: obj.DeepCopy(), Base: obj.DeepCopy()})
	// No client is needed: a terminating snapshot must never reach any API call.
	result, err := (&Env{}).Execute(t.Context(), plan)
	if err != nil || result.Err() != nil {
		t.Fatalf("terminating snapshot: %+v %v", result, err)
	}
}
