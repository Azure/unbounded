// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/Azure/unbounded/internal/operator/component"
)

// CRDReconciler repairs deletion or drift of the CRDs owned by the operator.
// Startup bootstrap remains responsible for making the API available before
// the manager starts; this controller handles day-2 changes without polling.
type CRDReconciler struct {
	client.Client
	desired map[string]*unstructured.Unstructured
}

func (r *CRDReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	desired, owned := r.desired[req.Name]
	if !owned {
		return ctrl.Result{}, nil
	}

	var current apiextensionsv1.CustomResourceDefinition

	err := r.Get(ctx, client.ObjectKey{Name: req.Name}, &current)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("get CRD %s: %w", req.Name, err)
	}

	if err == nil && current.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	if err == nil && crdMatchesDesired(&current, desired) {
		return ctrl.Result{}, nil
	}

	applyCfg := client.ApplyConfigurationFromUnstructured(desired.DeepCopy())
	if err := r.Apply(ctx, applyCfg, client.FieldOwner(FieldOwner), client.ForceOwnership); err != nil {
		return ctrl.Result{}, fmt.Errorf("apply CRD %s: %w", req.Name, err)
	}

	return ctrl.Result{}, nil
}

func crdMatchesDesired(current *apiextensionsv1.CustomResourceDefinition, desired *unstructured.Unstructured) bool {
	expectedHash := desired.GetLabels()[component.AppliedHashLabel]

	return expectedHash != "" && current.Labels[component.AppliedHashLabel] == expectedHash
}

// SetupWithManager watches only operator-owned CRDs. Startup bootstrap ensures
// they exist before the informer starts, so the initial add events reconcile
// the complete set and later update/delete events repair day-2 drift.
func (r *CRDReconciler) SetupWithManager(mgr ctrl.Manager) error {
	desired, err := desiredCRDs()
	if err != nil {
		return err
	}

	r.desired = desired
	owned := func(obj client.Object) bool {
		_, ok := desired[obj.GetName()]

		return ok
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("owned-crd").
		For(&apiextensionsv1.CustomResourceDefinition{}, builder.WithPredicates(predicate.Funcs{
			CreateFunc:  func(ev event.CreateEvent) bool { return owned(ev.Object) },
			DeleteFunc:  func(ev event.DeleteEvent) bool { return owned(ev.Object) },
			UpdateFunc:  func(ev event.UpdateEvent) bool { return owned(ev.ObjectNew) },
			GenericFunc: func(event.GenericEvent) bool { return false },
		})).
		Complete(r)
}

func desiredCRDs() (map[string]*unstructured.Unstructured, error) {
	desired := make(map[string]*unstructured.Unstructured, len(RequiredCRDNames))

	for _, manifests := range bootstrapManifestSets() {
		files, err := component.YamlFiles(manifests)
		if err != nil {
			return nil, err
		}

		for _, file := range files {
			if err := decodeCRDs(manifests, file, desired); err != nil {
				return nil, err
			}
		}
	}

	return desired, nil
}

func decodeCRDs(manifests fs.FS, file string, desired map[string]*unstructured.Unstructured) error {
	data, err := fs.ReadFile(manifests, file)
	if err != nil {
		return fmt.Errorf("read manifest %s: %w", file, err)
	}

	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)

	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(obj); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}

			return fmt.Errorf("decode %s: %w", file, err)
		}

		if obj.Object != nil && obj.GetKind() == component.CRDKind {
			hash, err := component.AppliedPayloadHash(obj)
			if err != nil {
				return fmt.Errorf("hash CRD %s: %w", obj.GetName(), err)
			}

			labels := obj.GetLabels()
			if labels == nil {
				labels = map[string]string{}
			}

			labels[component.AppliedHashLabel] = hash
			obj.SetLabels(labels)
			desired[obj.GetName()] = obj
		}
	}
}
