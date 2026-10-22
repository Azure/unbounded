// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/operator/component"
)

const (
	// managerAnnotation marks objects the operator applies.
	managerAnnotation = "unbounded-cloud.io/racer-manager"

	// managerLabel marks objects the operator creates once for the runtime.
	// Admission fixes runtime-written labels, but not all annotations.
	managerLabel = managerAnnotation
)

// errNotOwned marks a resource this operator does not own and must not adopt.
var errNotOwned = errors.New("not managed by the unbounded operator; refusing adoption")

func markManaged(obj client.Object) {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}

	annotations[managerAnnotation] = component.FieldOwner
	obj.SetAnnotations(annotations)
}

// owned reports whether a live object is a usable operator-managed object.
func owned(obj client.Object) bool {
	if obj.GetUID() == "" || obj.GetResourceVersion() == "" || obj.GetDeletionTimestamp() != nil {
		return false
	}

	return obj.GetAnnotations()[managerAnnotation] == component.FieldOwner || obj.GetLabels()[managerLabel] == component.FieldOwner
}

func validateOwner(obj client.Object) error {
	if !owned(obj) {
		return fmt.Errorf("racer resource %s %s is %w", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName(), errNotOwned)
	}

	return nil
}

// ownedOperation creates the object when absent, which wins ownership
// atomically, or applies it pinned to the UID of the owned live object so a
// replacement is rejected rather than adopted.
func ownedOperation(ctx context.Context, env *component.Env, op component.Operation) (component.Operation, error) {
	markManaged(op.Object)
	current := op.Object.DeepCopy()

	err := env.LiveReader().Get(ctx, client.ObjectKeyFromObject(current), current)
	if apierrors.IsNotFound(err) {
		op.Kind = component.OpCreateIfAbsent
		return op, nil
	}

	if err != nil {
		return op, err
	}

	if err := validateAdoptable(ctx, env, current); err != nil {
		return op, err
	}

	op.Object.SetUID(current.GetUID())
	op.Object.SetResourceVersion("")

	return op, nil
}
