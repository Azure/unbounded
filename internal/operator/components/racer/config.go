// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"maps"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/operator/component"
)

// Use the operator's create-if-absent plus optimistic merge-patch pipeline.
// Only installation wiring is owned; administrator payload and deletions survive.
func preservedConfig(ctx context.Context, env *component.Env, plan *component.Plan, defaults *corev1.ConfigMap, wiring map[string]string) (*corev1.ConfigMap, error) {
	defaults.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}
	current := &corev1.ConfigMap{}

	err := env.LiveReader().Get(ctx, client.ObjectKeyFromObject(defaults), current)
	if apierrors.IsNotFound(err) {
		add(plan, component.OpCreateIfAbsent, defaults)
		return defaults, nil
	}

	if err != nil {
		return nil, err
	}

	current.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}

	before := current.DeepCopy()
	if current.Data == nil {
		current.Data = map[string]string{}
	}

	maps.Copy(current.Data, wiring)

	if !maps.Equal(before.Data, current.Data) {
		plan.Add(component.Operation{Kind: component.OpMergePatch, Object: component.ToUnstructured(current), Base: component.ToUnstructured(before), Component: name})
	}

	return current, nil
}
