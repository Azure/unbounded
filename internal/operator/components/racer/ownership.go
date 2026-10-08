// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/operator/component"
)

func bindRuntime(obj client.Object, installation types.UID) {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}

	annotations[managerAnnotation] = component.FieldOwner
	annotations[installationAnnotation] = string(installation)
	obj.SetAnnotations(annotations)
}

func validateRuntimeOwner(obj client.Object, installation types.UID) error {
	if installation == "" || obj.GetUID() == "" || obj.GetResourceVersion() == "" || obj.GetDeletionTimestamp() != nil || obj.GetAnnotations()[managerAnnotation] != component.FieldOwner || obj.GetAnnotations()[installationAnnotation] != string(installation) {
		return fmt.Errorf("racer resource %s is not owned by this installation; refusing adoption", obj.GetName())
	}

	return nil
}

// Create wins ownership atomically. Later applies carry the observed UID and
// revision so a replacement or ownership edit cannot win between read and write.
func ownedRuntimeOperation(ctx context.Context, env *component.Env, op component.Operation, installation types.UID) (component.Operation, error) {
	bindRuntime(op.Object, installation)
	current := op.Object.DeepCopy()

	err := env.LiveReader().Get(ctx, client.ObjectKeyFromObject(current), current)
	if apierrors.IsNotFound(err) {
		op.Kind = component.OpCreateIfAbsent
		return op, nil
	}

	if err != nil {
		return op, err
	}

	if err := validateRuntimeOwner(current, installation); err != nil {
		return op, err
	}

	op.Object.SetUID(current.GetUID())
	op.Object.SetResourceVersion(current.GetResourceVersion())

	return op, nil
}
