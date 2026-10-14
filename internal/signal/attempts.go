// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

func (r *RunReconciler) preserveAttempt(
	ctx context.Context,
	run *signalv1.CertificationRun,
	workflow *unstructured.Unstructured,
	executionState, verdict, reason string,
) error {
	if r.Evidence == nil {
		return fmt.Errorf("evidence store is required before completing an attempt")
	}

	index := attemptIndex(run)
	if index >= 0 && run.Status.Attempts[index].Evidence.URI != "" {
		return nil
	}

	if index < 0 {
		before := run.DeepCopy()

		collection := collectTestResults(run, workflow, true)
		if collection.strict {
			run.Status.TestResults = mergeTestResults(run.Status.TestResults, collection.results, run.Status.Workflow.Attempt)
		}

		if phase := workflowPhase(workflow); phase != "" {
			run.Status.Workflow.Phase = phase
		} else if executionState == "Completed" {
			run.Status.Workflow.Phase = "Succeeded"
		} else {
			run.Status.Workflow.Phase = executionState
		}

		run.Status.Attempts = append(run.Status.Attempts, signalv1.AttemptStatus{
			Workflow:       run.Status.Workflow,
			ExecutionState: executionState,
			Verdict:        verdict,
			Reason:         reason,
			FinishedAt:     attemptFinishedAt(workflow, r.now()),
		})
		if err := r.Status().Patch(ctx, run, client.MergeFrom(before)); err != nil {
			return fmt.Errorf("record attempt outcome before evidence: %w", err)
		}

		index = len(run.Status.Attempts) - 1
	}

	attempt := run.Status.Attempts[index]
	path := attemptEvidencePath(run, attempt)

	evidence, found, err := r.recoverEvidence(ctx, run, path, attempt)
	if err != nil {
		return err
	}

	if !found {
		data, err := r.evidenceDocument(ctx, run, workflow, attempt)
		if err != nil {
			return err
		}

		uri, digest, err := r.Evidence.Put(ctx, path, data)
		if err != nil {
			return fmt.Errorf("preserve attempt evidence: %w", err)
		}

		evidence = signalv1.EvidenceStatus{URI: uri, Digest: digest}
	}

	before := run.DeepCopy()
	run.Status.Attempts[index].Evidence = evidence

	for i := range run.Status.TestResults {
		if run.Status.TestResults[i].Attempt == attempt.Workflow.Attempt {
			run.Status.TestResults[i].Evidence = evidence
		}
	}

	return r.Status().Patch(ctx, run, client.MergeFrom(before))
}

func (r *RunReconciler) preserveFinalEvidence(
	ctx context.Context,
	run *signalv1.CertificationRun,
	workflow *unstructured.Unstructured,
	outcome *signalv1.AttemptStatus,
) error {
	if run.Status.Evidence.URI != "" {
		return nil
	}

	index := attemptIndex(run)
	if index < 0 || run.Status.Attempts[index].Evidence.URI == "" {
		return fmt.Errorf("attempt evidence must be preserved before final evidence")
	}

	attempt := run.Status.Attempts[index]
	if outcome != nil {
		attempt = *outcome
	}

	evidence, found, err := r.recoverEvidence(ctx, run, run.Spec.EvidencePath, attempt)
	if err != nil {
		return err
	}

	if !found {
		workflow, err = r.storedWorkflowSnapshot(ctx, run, workflow, run.Status.Attempts[index])
		if err != nil {
			return err
		}

		data, err := r.evidenceDocument(ctx, run, workflow, attempt)
		if err != nil {
			return err
		}

		uri, digest, err := r.Evidence.Put(ctx, run.Spec.EvidencePath, data)
		if err != nil {
			return fmt.Errorf("preserve final evidence: %w", err)
		}

		evidence = signalv1.EvidenceStatus{URI: uri, Digest: digest}
	}

	before := run.DeepCopy()
	run.Status.Evidence = evidence

	return r.Status().Patch(ctx, run, client.MergeFrom(before))
}

type evidenceCheckpoint struct {
	RunID              string                         `json:"runID"`
	RunName            string                         `json:"runName"`
	RunSpec            *signalv1.CertificationRunSpec `json:"runSpec"`
	Workflow           signalv1.WorkflowStatus        `json:"workflow"`
	ExecutionState     string                         `json:"executionState"`
	Verdict            string                         `json:"verdict"`
	Reason             string                         `json:"reason"`
	FinishedAt         string                         `json:"finishedAt"`
	TestResults        []signalv1.TestResult          `json:"testResults"`
	Attempts           []signalv1.AttemptStatus       `json:"attempts"`
	WorkflowNodes      map[string]any                 `json:"workflowNodes"`
	WorkflowNodeStatus map[string]any                 `json:"workflowNodeStatus"`
}

func (r *RunReconciler) recoverEvidence(
	ctx context.Context,
	run *signalv1.CertificationRun,
	path string,
	attempt signalv1.AttemptStatus,
) (signalv1.EvidenceStatus, bool, error) {
	reader, ok := r.Evidence.(EvidenceReader)
	if !ok {
		return signalv1.EvidenceStatus{}, false, nil
	}

	data, uri, digest, err := reader.Get(ctx, path)
	if apierrors.IsNotFound(err) {
		return signalv1.EvidenceStatus{}, false, nil
	}

	if err != nil {
		return signalv1.EvidenceStatus{}, false, fmt.Errorf("recover immutable evidence: %w", err)
	}

	var checkpoint evidenceCheckpoint
	if err := json.Unmarshal(data, &checkpoint); err != nil {
		return signalv1.EvidenceStatus{}, false, fmt.Errorf("decode existing evidence: %w", err)
	}

	expectedSpec := run.Spec

	expectedSpec.Cancel = false
	if checkpoint.RunSpec != nil {
		checkpoint.RunSpec.Cancel = false
	}

	if checkpoint.RunID != string(run.UID) || checkpoint.RunName != run.Name ||
		checkpoint.RunSpec == nil || !reflect.DeepEqual(*checkpoint.RunSpec, expectedSpec) ||
		checkpoint.Workflow != attempt.Workflow || checkpoint.ExecutionState != attempt.ExecutionState ||
		checkpoint.Verdict != attempt.Verdict || checkpoint.Reason != attempt.Reason ||
		checkpoint.FinishedAt != attempt.FinishedAt.UTC().Format(time.RFC3339Nano) ||
		!reflect.DeepEqual(checkpoint.TestResults, run.Status.TestResults) ||
		!reflect.DeepEqual(checkpoint.Attempts, run.Status.Attempts) {
		return signalv1.EvidenceStatus{}, false, fmt.Errorf("existing evidence does not match the immutable run and recorded attempt")
	}

	return signalv1.EvidenceStatus{URI: uri, Digest: digest}, true, nil
}

func (r *RunReconciler) storedWorkflowSnapshot(
	ctx context.Context,
	run *signalv1.CertificationRun,
	workflow *unstructured.Unstructured,
	attempt signalv1.AttemptStatus,
) (*unstructured.Unstructured, error) {
	reader, ok := r.Evidence.(EvidenceReader)
	if !ok {
		return workflow, nil
	}

	data, uri, digest, err := reader.Get(ctx, attemptEvidencePath(run, attempt))
	if err != nil {
		return nil, fmt.Errorf("read recorded attempt snapshot: %w", err)
	}

	if uri != attempt.Evidence.URI || digest != attempt.Evidence.Digest {
		return nil, fmt.Errorf("recorded attempt snapshot does not match its evidence reference")
	}

	var checkpoint evidenceCheckpoint
	if err := json.Unmarshal(data, &checkpoint); err != nil {
		return nil, fmt.Errorf("decode recorded attempt snapshot: %w", err)
	}

	status := map[string]any{"nodes": checkpoint.WorkflowNodes}
	if len(checkpoint.WorkflowNodeStatus) != 0 {
		status = checkpoint.WorkflowNodeStatus
	}

	return &unstructured.Unstructured{Object: map[string]any{"status": status}}, nil
}

func attemptEvidencePath(run *signalv1.CertificationRun, attempt signalv1.AttemptStatus) string {
	return fmt.Sprintf("%s-a%d.json", strings.TrimSuffix(run.Spec.EvidencePath, ".json"), attempt.Workflow.Attempt)
}

func (r *RunReconciler) preserveInterrupted(ctx context.Context, run *signalv1.CertificationRun) error {
	if run.Status.Evidence.URI != "" {
		return nil
	}

	var workflow *unstructured.Unstructured
	if run.Status.Workflow.Name != "" {
		workflow = &unstructured.Unstructured{}
		workflow.SetGroupVersionKind(workflowGVK)

		if err := r.Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: run.Status.Workflow.Name}, workflow); err != nil {
			if !apierrors.IsNotFound(err) {
				return err
			}

			workflow = nil
		}
	}

	reason := "canceled by operator"
	if !run.DeletionTimestamp.IsZero() {
		reason = "run deleted by operator"
	}

	if err := r.preserveAttempt(ctx, run, workflow, "Interrupted", "Inconclusive", reason); err != nil {
		return err
	}

	outcome := run.Status.Attempts[attemptIndex(run)]
	outcome.ExecutionState = "Interrupted"
	outcome.Verdict = "Inconclusive"
	outcome.Reason = reason

	return r.preserveFinalEvidence(ctx, run, workflow, &outcome)
}

func attemptIndex(run *signalv1.CertificationRun) int {
	for index := range run.Status.Attempts {
		if run.Status.Attempts[index].Workflow.Attempt == run.Status.Workflow.Attempt {
			return index
		}
	}

	return -1
}

func attemptFinishedAt(workflow *unstructured.Unstructured, now time.Time) metav1.Time {
	if workflow != nil {
		if value, _, err := unstructured.NestedString(workflow.Object, "status", "finishedAt"); err == nil && value != "" {
			if finished, err := time.Parse(time.RFC3339Nano, value); err == nil {
				return metav1.NewTime(finished.UTC().Truncate(time.Second))
			}
		}
	}

	// Kubernetes metav1.Time persists whole seconds. Keep retries byte-identical
	// even when the evidence was saved before a status update failed.
	return metav1.NewTime(now.UTC().Truncate(time.Second))
}

func (r *RunReconciler) recoverExplicitWorkflow(ctx context.Context, run *signalv1.CertificationRun) error {
	workflows := &unstructured.UnstructuredList{}
	workflows.SetGroupVersionKind(workflowGVK.GroupVersion().WithKind("WorkflowList"))

	if err := admissionReader(r.Reader, r.Client).List(ctx, workflows, client.InNamespace(run.Namespace)); err != nil {
		return fmt.Errorf("recover group Workflow identity: %w", err)
	}

	latest := run.Status.Workflow

	for index := range workflows.Items {
		workflow := &workflows.Items[index]
		if !metav1.IsControlledBy(workflow, run) {
			continue
		}

		suffix, found := strings.CutPrefix(workflow.GetName(), run.Name+"-a")
		if !found {
			continue
		}

		attempt, err := strconv.ParseInt(suffix, 10, 32)
		if err != nil || attempt < 1 || attempt > int64(run.Spec.MaxAttempts) {
			continue
		}

		if int32(attempt) < latest.Attempt {
			continue
		}

		if int32(attempt) == latest.Attempt && latest.UID != "" && latest.UID != string(workflow.GetUID()) {
			return fmt.Errorf("group Workflow attempt %d UID changed", attempt)
		}

		latest = signalv1.WorkflowStatus{
			Name: workflow.GetName(), UID: string(workflow.GetUID()), Attempt: int32(attempt), Phase: workflowPhase(workflow),
		}
	}

	if latest.Name == "" || latest == run.Status.Workflow {
		return nil
	}

	before := run.DeepCopy()
	run.Status.Workflow = latest

	return r.Status().Patch(ctx, run, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}
