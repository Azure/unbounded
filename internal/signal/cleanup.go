// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

func (r *RunReconciler) strictCleanupReady(ctx context.Context, run *signalv1.CertificationRun) (bool, error) {
	_, strict, planErr := effectiveTestPlan(run)
	if !strict {
		return true, nil
	}

	if planErr != nil {
		return r.ensureWorkflowStopped(ctx, run)
	}

	gone, err := r.ownedBackendsGone(ctx, run)
	if err != nil {
		return false, err
	}

	stop := !gone || run.Status.ExecutionState != "Completed" || run.Status.Workflow.Phase != "Succeeded"
	if run.Status.Workflow.Name != "" {
		workflow := &unstructured.Unstructured{}
		workflow.SetGroupVersionKind(workflowGVK)

		if err := r.Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: run.Status.Workflow.Name}, workflow); err != nil {
			if !apierrors.IsNotFound(err) {
				return false, err
			}
		} else {
			nodes, nodesErr := workflowNodes(workflow)
			if nodesErr != nil {
				stop = true
			}

			for _, raw := range nodes {
				node, ok := raw.(map[string]any)
				if !ok {
					stop = true

					break
				}

				switch nodePhase(node) {
				case "Succeeded", "Skipped", "Omitted":
				default:
					stop = true
				}
			}
		}
	}

	if stop {
		return r.ensureWorkflowStopped(ctx, run)
	}

	return true, nil
}

func (r *RunReconciler) ownedBackendsGone(ctx context.Context, run *signalv1.CertificationRun) (bool, error) {
	workflow, strict, err := nodeWorkflow(run.Spec.Workflow.Name, run.Spec.Parameters)
	if !strict || workflow == BurnCanaryWorkflow || run.Status.Workflow.Name == "" {
		return true, nil
	}

	if err != nil {
		return false, err
	}

	if run.Status.Workflow.UID == "" {
		return false, fmt.Errorf("workflow UID is required before verifying backend cleanup")
	}

	version, err := schema.ParseGroupVersion(run.Spec.Backend.APIVersion)
	if err != nil || run.Spec.Backend.Kind == "" {
		return false, fmt.Errorf("invalid backend identity for cleanup: %s %s", run.Spec.Backend.APIVersion, run.Spec.Backend.Kind)
	}

	backends := &unstructured.UnstructuredList{}
	backends.SetGroupVersionKind(version.WithKind(run.Spec.Backend.Kind + "List"))

	if err := r.List(ctx, backends, client.InNamespace(run.Namespace)); err != nil {
		return false, fmt.Errorf("verify owned backend cleanup: %w", err)
	}

	for index := range backends.Items {
		backend := &backends.Items[index]
		for _, owner := range backend.GetOwnerReferences() {
			if ownedByWorkflow(owner, run) || owner.UID == run.UID && owner.Kind == "CertificationRun" &&
				owner.APIVersion == signalv1.GroupVersion.String() {
				return false, nil
			}
		}
	}

	return true, nil
}

func ownedByWorkflow(owner metav1.OwnerReference, run *signalv1.CertificationRun) bool {
	return string(owner.UID) == run.Status.Workflow.UID &&
		owner.Kind == workflowGVK.Kind && owner.APIVersion == workflowGVK.GroupVersion().String()
}
