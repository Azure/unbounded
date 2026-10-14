// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

func requiresWorkflowCleanup(run *signalv1.CertificationRun) bool {
	return supportedNodeWorkflow(run.Spec.Workflow.Name) || run.Spec.Workflow.Name == RackCertificationWorkflow
}

func (r *RunReconciler) strictCleanupReady(ctx context.Context, run *signalv1.CertificationRun) (bool, error) {
	if run.Spec.Admission != nil || len(run.Spec.Participants) > 1 {
		return r.ensureWorkflowStopped(ctx, run)
	}

	if !requiresWorkflowCleanup(run) {
		return true, nil
	}

	_, _, planErr := effectiveTestPlan(run)
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

		if err := admissionReader(r.Reader, r.Client).Get(
			ctx, client.ObjectKey{Namespace: run.Namespace, Name: run.Status.Workflow.Name}, workflow,
		); err != nil {
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
	workflow, strict := cleanupNodeWorkflow(run)

	explicit := run.Spec.Admission != nil || len(run.Spec.Participants) > 1
	if !strict || workflow == BurnCanaryWorkflow || !explicit && run.Status.Workflow.Name == "" {
		return true, nil
	}

	if !explicit && run.Status.Workflow.UID == "" {
		return false, fmt.Errorf("workflow UID is required before verifying backend cleanup")
	}

	version, err := schema.ParseGroupVersion(run.Spec.Backend.APIVersion)
	if err != nil || run.Spec.Backend.Kind == "" {
		return false, fmt.Errorf("invalid backend identity for cleanup: %s %s", run.Spec.Backend.APIVersion, run.Spec.Backend.Kind)
	}

	backends := &unstructured.UnstructuredList{}
	backends.SetGroupVersionKind(version.WithKind(run.Spec.Backend.Kind + "List"))

	if err := admissionReader(r.Reader, r.Client).List(ctx, backends, client.InNamespace(run.Namespace)); err != nil {
		return false, fmt.Errorf("verify owned backend cleanup: %w", err)
	}

	for index := range backends.Items {
		backend := &backends.Items[index]
		if explicit && backend.GetLabels()[SnapshotRunLabel] == run.Name {
			return false, nil
		}

		for _, owner := range backend.GetOwnerReferences() {
			if ownedByWorkflow(owner, run) || owner.UID == run.UID && owner.Kind == "CertificationRun" &&
				owner.APIVersion == signalv1.GroupVersion.String() {
				return false, nil
			}
		}
	}

	return true, nil
}

func (r *RunReconciler) ensureGroupWorkflowsStopped(ctx context.Context, run *signalv1.CertificationRun) (bool, error) {
	reader := admissionReader(r.Reader, r.Client)
	workflows := &unstructured.UnstructuredList{}
	workflows.SetGroupVersionKind(workflowGVK.GroupVersion().WithKind("WorkflowList"))

	if err := reader.List(ctx, workflows, client.InNamespace(run.Namespace)); err != nil {
		return false, fmt.Errorf("list group Workflows before cleanup: %w", err)
	}

	gone := true

	var errs []error

	for index := range workflows.Items {
		workflow := &workflows.Items[index]

		owned := metav1.IsControlledBy(workflow, run)
		if !owned && workflow.GetName() != run.Status.Workflow.Name {
			continue
		}

		if !owned || workflow.GetName() == run.Status.Workflow.Name && run.Status.Workflow.UID != "" &&
			string(workflow.GetUID()) != run.Status.Workflow.UID {
			errs = append(errs, fmt.Errorf("refuse to delete group Workflow %s without matching ownership and UID", workflow.GetName()))

			continue
		}

		uid := workflow.GetUID()
		if err := r.Delete(ctx, workflow, client.PropagationPolicy(metav1.DeletePropagationForeground),
			client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete group Workflow %s: %w", workflow.GetName(), err))

			continue
		}

		if err := reader.Get(ctx, client.ObjectKeyFromObject(workflow), workflow); err == nil {
			gone = false
		} else if !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("confirm group Workflow deletion: %w", err))
		}
	}

	if err := errors.Join(errs...); err != nil {
		return false, err
	}

	if !gone {
		return false, nil
	}

	return r.stopGroupBackends(ctx, run)
}

func ownedByWorkflow(owner metav1.OwnerReference, run *signalv1.CertificationRun) bool {
	if owner.UID == "" || owner.Kind != workflowGVK.Kind || owner.APIVersion != workflowGVK.GroupVersion().String() {
		return false
	}

	if string(owner.UID) == run.Status.Workflow.UID {
		return true
	}

	for _, attempt := range run.Status.Attempts {
		if string(owner.UID) == attempt.Workflow.UID {
			return true
		}
	}

	return false
}

func (r *RunReconciler) stopGroupBackends(ctx context.Context, run *signalv1.CertificationRun) (bool, error) {
	workflow, strict := cleanupNodeWorkflow(run)
	if !strict || workflow == BurnCanaryWorkflow {
		return true, nil
	}

	version, err := schema.ParseGroupVersion(run.Spec.Backend.APIVersion)
	if err != nil || run.Spec.Backend.Kind == "" {
		return false, fmt.Errorf("invalid group backend identity for cleanup")
	}

	reader := admissionReader(r.Reader, r.Client)
	backends := &unstructured.UnstructuredList{}
	backends.SetGroupVersionKind(version.WithKind(run.Spec.Backend.Kind + "List"))

	if err := reader.List(ctx, backends, client.InNamespace(run.Namespace)); err != nil {
		return false, fmt.Errorf("list group backends before cleanup: %w", err)
	}

	gone := true

	var errs []error

	for index := range backends.Items {
		backend := &backends.Items[index]
		owned := false

		for _, owner := range backend.GetOwnerReferences() {
			if ownedByWorkflow(owner, run) || owner.UID == run.UID && owner.Kind == "CertificationRun" &&
				owner.APIVersion == signalv1.GroupVersion.String() {
				owned = true
			}
		}

		if !owned {
			if backend.GetLabels()[SnapshotRunLabel] == run.Name {
				errs = append(errs, fmt.Errorf("refuse to delete group backend %s without pinned ownership", backend.GetName()))
			}

			continue
		}

		uid := backend.GetUID()
		if err := r.Delete(ctx, backend, client.PropagationPolicy(metav1.DeletePropagationForeground),
			client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete group backend %s: %w", backend.GetName(), err))

			continue
		}

		if err := reader.Get(ctx, client.ObjectKeyFromObject(backend), backend); err == nil {
			gone = false
		} else if !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("confirm group backend deletion: %w", err))
		}
	}

	return gone && len(errs) == 0, errors.Join(errs...)
}

func cleanupNodeWorkflow(run *signalv1.CertificationRun) (string, bool) {
	if run.Spec.Workflow.Name == RackCertificationWorkflow {
		return run.Spec.Parameters[NodeWorkflowTemplateParameter], true
	}

	return run.Spec.Workflow.Name, supportedNodeWorkflow(run.Spec.Workflow.Name)
}

func cleanupParticipants(run *signalv1.CertificationRun) []signalv1.Participant {
	if run.Spec.Admission == nil {
		return run.Spec.Participants
	}

	participants := make([]signalv1.Participant, 0, len(run.Spec.Participants))
	for _, participant := range run.Spec.Participants {
		if !containsString(run.Spec.Admission.Participants.NodeNames, participant.NodeName) || participant.NodeUID == "" {
			continue
		}

		for _, snapshot := range run.Spec.ParticipantPlan {
			if snapshot.NodeName == participant.NodeName && snapshot.NodeUID == participant.NodeUID {
				participants = append(participants, participant)

				break
			}
		}
	}

	return participants
}
