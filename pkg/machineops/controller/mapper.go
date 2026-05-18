// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// NewTypedMapper maps MachineOperation events to caller-specific typed requests.
func NewTypedMapper[T comparable](
	r *Reconciler,
	newRequest func(string) T,
) handler.TypedMapFunc[client.Object, T] {
	return func(ctx context.Context, obj client.Object) []T {
		op, ok := obj.(*machinav1alpha3.MachineOperation)
		if !ok || !r.ShouldEnqueue(ctx, op) {
			return nil
		}

		return []T{newRequest(op.Name)}
	}
}
