// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signaladapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

var (
	workflowGVR = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "workflows"}
	mpiJobGVR   = schema.GroupVersionResource{Group: "kubeflow.org", Version: "v2beta1", Resource: "mpijobs"}
	podGVR      = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
)

type Runner struct {
	Dynamic                dynamic.Interface
	Kubernetes             kubernetes.Interface
	PollInterval           time.Duration
	CleanupWait            time.Duration
	ReadPodLog             func(context.Context, string, string, string, bool) (string, error)
	PersistBackendEvidence func(string) error
}

type createdResource struct {
	gvr              schema.GroupVersionResource
	resource         *unstructured.Unstructured
	participantIndex int32
}

const maxResultLogBytes = 64 * 1024

type resultLogSource struct {
	content  string
	err      error
	metadata ResultLogSourceMetadata
}

type resultLogSnapshot struct {
	current  resultLogSource
	previous resultLogSource
}

func (r *Runner) Execute(ctx context.Context, config Config) (AggregateOutput, error) {
	if r.Dynamic == nil || r.Kubernetes == nil {
		return AggregateOutput{}, errors.New("kubernetes clients are required")
	}

	participants, tolerations, environment, err := ValidateConfig(config)
	if err != nil {
		return AggregateOutput{}, err
	}

	workflow, err := r.Dynamic.Resource(workflowGVR).Namespace(config.Namespace).
		Get(ctx, config.WorkflowName, metav1.GetOptions{})
	if err != nil {
		return AggregateOutput{}, fmt.Errorf("get owning Workflow: %w", err)
	}

	activeDeadlineSeconds, found, err := unstructured.NestedInt64(
		workflow.Object,
		"spec",
		"activeDeadlineSeconds",
	)
	if err != nil {
		return AggregateOutput{}, fmt.Errorf("read Workflow active deadline: %w", err)
	}

	if !found || activeDeadlineSeconds < 1 {
		return AggregateOutput{}, errors.New("owning Workflow requires a positive activeDeadlineSeconds")
	}

	if err := signalv1.ValidateRuntimeContract(
		config.ScriptRequiresStage,
		runtimeStage(config.RuntimeStageName),
		time.Duration(config.BackendDeadlineSeconds)*time.Second,
		time.Duration(activeDeadlineSeconds)*time.Second,
	); err != nil {
		return AggregateOutput{}, fmt.Errorf("validate backend and Workflow deadlines: %w", err)
	}

	executionContext, cancelExecution := context.WithTimeout(
		ctx,
		time.Duration(activeDeadlineSeconds)*time.Second,
	)
	defer cancelExecution()

	controller := true
	owner := metav1.OwnerReference{
		APIVersion: workflow.GetAPIVersion(),
		Kind:       workflow.GetKind(),
		Name:       workflow.GetName(),
		UID:        workflow.GetUID(),
		Controller: &controller,
	}

	resources, err := RenderResources(
		config,
		participants,
		tolerations,
		environment,
		owner,
	)
	if err != nil {
		return AggregateOutput{}, err
	}

	created := make([]createdResource, 0, len(resources))
	references := make([]signalv1.BackendResourceReference, 0, len(resources))
	finishBackendFailure := func(cause error) (AggregateOutput, error) {
		output, err := r.backendFailureOutput(config, len(participants), created, references, cause)
		if err != nil {
			return AggregateOutput{}, err
		}

		cleanupContext, cancel := context.WithTimeout(context.Background(), r.cleanupWait())
		defer cancel()

		if err := r.cleanup(cleanupContext, config.Namespace, created); err != nil {
			recordCleanupFailure(&output)
		}

		return output, nil
	}

	for index, resource := range resources {
		gvr := podGVR
		if resource.GetKind() == "MPIJob" {
			gvr = mpiJobGVR
		}

		createdObject, createErr := r.Dynamic.Resource(gvr).Namespace(config.Namespace).
			Create(executionContext, resource, metav1.CreateOptions{})
		if createErr != nil {
			if len(created) > 0 {
				return finishBackendFailure(fmt.Errorf("create participant %d %s: %w", index, resource.GetKind(), createErr))
			}

			return AggregateOutput{}, fmt.Errorf("create participant %d %s: %w", index, resource.GetKind(), createErr)
		}

		created = append(created, createdResource{
			gvr:              gvr,
			resource:         createdObject,
			participantIndex: int32(index),
		})

		reference, referenceErr := BackendResource(createdObject, int32(index))
		if referenceErr != nil {
			fallbackReference, identityErr := backendResourceIdentity(createdObject, int32(index))
			if identityErr != nil {
				return AggregateOutput{}, errors.Join(
					fmt.Errorf("record participant %d backend identity: %w", index, referenceErr),
					identityErr,
				)
			}

			references = append(references, fallbackReference)

			return finishBackendFailure(fmt.Errorf("record participant %d backend identity: %w", index, referenceErr))
		}

		references = append(references, reference)
	}

	results := make([]Result, len(created))

	var waitGroup sync.WaitGroup

	for index := range created {
		waitGroup.Add(1)

		go func() {
			defer waitGroup.Done()

			results[index] = r.waitForResult(executionContext, config, created[index])
		}()
	}

	waitGroup.Wait()

	output, err := Aggregate(len(participants), references, results)
	if err != nil {
		return finishBackendFailure(fmt.Errorf("aggregate participant results: %w", err))
	}

	captureContext, cancelCapture := context.WithTimeout(context.Background(), r.cleanupWait())
	defer cancelCapture()

	var evidenceComplete bool

	output.BackendEvidence, evidenceComplete, err = r.captureBackendEvidence(
		captureContext,
		config,
		created,
	)
	if err != nil {
		return finishBackendFailure(fmt.Errorf("capture backend evidence: %w", err))
	}

	if !evidenceComplete {
		recordEvidenceCaptureFailure(&output)
	}

	if err := r.persistEvidenceBeforeCleanup(&output, config.BackendEvidenceBytes); err != nil {
		return finishBackendFailure(fmt.Errorf("persist backend evidence: %w", err))
	}

	cleanupContext, cancel := context.WithTimeout(context.Background(), r.cleanupWait())
	defer cancel()

	if err := r.cleanup(cleanupContext, config.Namespace, created); err != nil {
		recordCleanupFailure(&output)

		return output, nil
	}

	return output, nil
}

func (r *Runner) backendFailureOutput(
	config Config,
	participantCount int,
	created []createdResource,
	references []signalv1.BackendResourceReference,
	cause error,
) (AggregateOutput, error) {
	captureContext, cancelCapture := context.WithTimeout(context.Background(), r.cleanupWait())
	defer cancelCapture()

	evidence, _, err := r.captureBackendEvidence(
		captureContext,
		config,
		created,
	)
	if err != nil {
		return AggregateOutput{}, fmt.Errorf("capture partial backend evidence: %w", err)
	}

	backendJSON, err := json.Marshal(references)
	if err != nil {
		return AggregateOutput{}, fmt.Errorf("encode partial backend resources: %w", err)
	}

	reason := boundedRedactedText(cause.Error(), maxEvidenceTextBytes)

	results := make([]Result, participantCount)
	for index := range results {
		results[index] = Result{
			ParticipantIndex: index,
			ExecutionState:   "Error",
			Verdict:          "Inconclusive",
			Reason:           "BackendSetupFailed",
		}
	}

	measurements, err := json.Marshal(map[string]any{
		"participantCount": participantCount,
		"createdBackends":  len(created),
		"error":            reason,
		"results":          results,
	})
	if err != nil {
		return AggregateOutput{}, fmt.Errorf("encode partial backend measurements: %w", err)
	}

	output := AggregateOutput{
		ExecutionState:   "Error",
		Verdict:          "Inconclusive",
		Reason:           "BackendSetupFailed",
		Measurements:     string(measurements),
		BackendResources: references,
		BackendJSON:      string(backendJSON),
		BackendEvidence:  evidence,
	}

	if err := r.persistEvidenceBeforeCleanup(&output, config.BackendEvidenceBytes); err != nil {
		return AggregateOutput{}, err
	}

	return output, nil
}

func (r *Runner) persistEvidenceBeforeCleanup(output *AggregateOutput, limit int) error {
	if r.PersistBackendEvidence == nil {
		return nil
	}

	if err := r.PersistBackendEvidence(output.BackendEvidence); err != nil {
		var markErr error

		output.BackendEvidence, markErr = markBackendEvidenceIncomplete(
			output.BackendEvidence,
			"persist backend evidence before cleanup: "+err.Error(),
			limit,
		)
		if markErr != nil {
			return errors.Join(
				fmt.Errorf("persist backend evidence before cleanup: %w", err),
				markErr,
			)
		}

		recordEvidenceCaptureFailure(output)

		return fmt.Errorf("persist backend evidence before cleanup: %w", err)
	}

	return nil
}

func recordCleanupFailure(output *AggregateOutput) {
	output.ExecutionState = "Error"
	output.Verdict = "Inconclusive"
	output.CleanupFailure = "BackendCleanupFailed"

	if output.Reason == "" || output.Reason == "AllParticipantsPassed" {
		output.Reason = output.CleanupFailure
	}
}

func runtimeStage(name string) *signalv1.RuntimeStage {
	if name == "" {
		return nil
	}

	return &signalv1.RuntimeStage{Name: name}
}

func (r *Runner) waitForResult(ctx context.Context, config Config, backend createdResource) Result {
	result := Result{
		ParticipantIndex: int(backend.participantIndex),
		ExecutionState:   "Error",
		Verdict:          "Inconclusive",
		Reason:           "BackendExecutionFailed",
	}

	ticker := time.NewTicker(r.pollInterval())
	defer ticker.Stop()

	var capturedResult *Result

	for {
		object, err := r.Dynamic.Resource(backend.gvr).Namespace(config.Namespace).
			Get(ctx, backend.resource.GetName(), metav1.GetOptions{})
		if err != nil {
			result.Reason = "BackendObservationFailed: " + err.Error()

			return result
		}

		terminal, succeeded, stateErr := backendState(object)
		if stateErr != nil {
			result.Reason = "BackendStatusInvalid: " + stateErr.Error()

			return result
		}

		if backend.resource.GetKind() == "MPIJob" && capturedResult == nil {
			current := r.resultLogs(ctx, config, backend, false).current
			if current.err == nil {
				parsed, parseErr := ParseResultLog(current.content, int(backend.participantIndex))
				if parseErr == nil {
					parsed.ResultLog = &ResultLogMetadata{
						Current:        current.metadata,
						Previous:       ResultLogSourceMetadata{Status: "not-read"},
						SelectedSource: "current",
					}
					capturedResult = &parsed
				}
			}
		}

		if terminal {
			logs := r.resultLogs(ctx, config, backend, backend.resource.GetKind() == "MPIJob")

			completed, hardFailure := backendCompletionFromLogs(
				logs,
				int(backend.participantIndex),
				succeeded,
			)
			if completed.ExecutionState == "Succeeded" || hardFailure || capturedResult == nil {
				return completed
			}

			capturedResult.ResultLog = &ResultLogMetadata{
				Current:        capturedResult.ResultLog.Current,
				Previous:       logs.previous.metadata,
				SelectedSource: "captured-current",
			}

			return completedResult(*capturedResult, succeeded)
		}

		select {
		case <-ctx.Done():
			result.Reason = "BackendDeadlineExceeded"

			return result
		case <-ticker.C:
		}
	}
}

func backendCompletionFromLogs(
	logs resultLogSnapshot,
	participantIndex int,
	backendSucceeded bool,
) (Result, bool) {
	result := Result{
		ParticipantIndex: participantIndex,
		ExecutionState:   "Error",
		Verdict:          "Inconclusive",
		Reason:           "BackendExecutionFailed",
		ResultLog: &ResultLogMetadata{
			Current:  logs.current.metadata,
			Previous: logs.previous.metadata,
		},
	}

	current, currentErr := parseResultLogSource(logs.current, participantIndex)
	previous, previousErr := parseResultLogSource(logs.previous, participantIndex)

	if currentErr == nil {
		if previousErr == nil && !sameResult(current, previous) {
			result.Reason = "BackendResultInvalid: current and previous launcher logs contain conflicting results"

			return result, true
		}

		current.ResultLog = result.ResultLog
		current.ResultLog.SelectedSource = "current"

		return completedResult(current, backendSucceeded), false
	}

	if logs.current.metadata.Status == "invalid" {
		if backendSucceeded {
			result.Reason = "BackendResultInvalid: " + currentErr.Error()
		}

		return result, true
	}

	if previousErr == nil {
		previous.ResultLog = result.ResultLog
		previous.ResultLog.SelectedSource = "previous"

		return completedResult(previous, backendSucceeded), false
	}

	hardFailure := logs.previous.metadata.Status == "invalid"

	if backendSucceeded {
		switch {
		case hardFailure:
			result.Reason = "BackendResultInvalid: " + previousErr.Error()
		case logs.current.err != nil:
			result.Reason = "BackendResultUnavailable: " + logs.current.err.Error()
		default:
			result.Reason = "BackendResultInvalid: " + currentErr.Error()
		}
	}

	return result, hardFailure
}

func parseResultLogSource(source resultLogSource, participantIndex int) (Result, error) {
	if source.err != nil {
		return Result{}, source.err
	}

	return ParseResultLog(source.content, participantIndex)
}

func sameResult(left, right Result) bool {
	return left.Verdict == right.Verdict &&
		left.Reason == right.Reason &&
		left.Measurements == right.Measurements &&
		left.ExitCode == right.ExitCode
}

func completedResult(result Result, backendSucceeded bool) Result {
	if (result.ExitCode == 0) != backendSucceeded {
		return Result{
			ParticipantIndex: result.ParticipantIndex,
			ExecutionState:   "Error",
			Verdict:          "Inconclusive",
			Reason:           "BackendResultInvalid: result exit code contradicts backend terminal state",
			ResultLog:        result.ResultLog,
		}
	}

	return result
}

func backendState(resource *unstructured.Unstructured) (bool, bool, error) {
	switch resource.GetKind() {
	case "Pod":
		phase, found, err := unstructured.NestedString(resource.Object, "status", "phase")
		if err != nil {
			return false, false, err
		}

		if !found {
			return false, false, nil
		}

		switch corev1.PodPhase(phase) {
		case corev1.PodSucceeded:
			return true, true, nil
		case corev1.PodFailed:
			return true, false, nil
		default:
			return false, false, nil
		}
	case "MPIJob":
		succeeded, _, err := unstructured.NestedInt64(
			resource.Object,
			"status",
			"replicaStatuses",
			"Launcher",
			"succeeded",
		)
		if err != nil {
			return false, false, err
		}

		if succeeded > 0 {
			return true, true, nil
		}

		failed, _, err := unstructured.NestedInt64(
			resource.Object,
			"status",
			"replicaStatuses",
			"Launcher",
			"failed",
		)
		if err != nil {
			return false, false, err
		}

		if failed > 0 {
			return true, false, nil
		}

		return false, false, nil
	default:
		return false, false, fmt.Errorf("unsupported backend kind %q", resource.GetKind())
	}
}

func (r *Runner) resultLogs(
	ctx context.Context,
	config Config,
	backend createdResource,
	includePrevious bool,
) resultLogSnapshot {
	podName := backend.resource.GetName()
	containerName := "main"

	if backend.resource.GetKind() == "MPIJob" {
		jobs, err := r.Kubernetes.BatchV1().Jobs(config.Namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			source := resultLogSourceFromError(fmt.Errorf("list MPIJob launcher Jobs: %w", err))

			return resultLogSnapshot{
				current:  source,
				previous: resultLogSourceMetadataOnly("not-read"),
			}
		}

		jobUIDs := make(map[types.UID]struct{})

		for index := range jobs.Items {
			job := &jobs.Items[index]
			if ownedBy(job.OwnerReferences, backend.resource.GetUID()) {
				jobUIDs[job.UID] = struct{}{}
			}
		}

		pods, err := r.Kubernetes.CoreV1().Pods(config.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: participantLabelSelector(config.RunName, backend.participantIndex),
		})
		if err != nil {
			source := resultLogSourceFromError(fmt.Errorf("list MPIJob Pods: %w", err))

			return resultLogSnapshot{
				current:  source,
				previous: resultLogSourceMetadataOnly("not-read"),
			}
		}

		podName = ""

		for index := range pods.Items {
			pod := &pods.Items[index]
			if mpiPodRole(pod, backend.resource.GetUID(), jobUIDs) == "launcher" {
				podName = pod.Name
				containerName = "launcher"

				break
			}
		}

		if podName == "" {
			source := resultLogSourceFromError(errors.New("MPIJob launcher Pod was not found"))

			return resultLogSnapshot{
				current:  source,
				previous: resultLogSourceMetadataOnly("not-read"),
			}
		}
	}

	currentContent, currentErr := r.readPodLog(ctx, config.Namespace, podName, containerName, false)
	snapshot := resultLogSnapshot{
		current:  resultLogSourceWithContent(currentContent, currentErr),
		previous: resultLogSourceMetadataOnly("not-read"),
	}

	if includePrevious {
		previousContent, previousErr := r.readPodLog(ctx, config.Namespace, podName, containerName, true)
		snapshot.previous = resultLogSourceWithContent(previousContent, previousErr)
	}

	return snapshot
}

func (r *Runner) readPodLog(
	ctx context.Context,
	namespace, podName, containerName string,
	previous bool,
) (string, error) {
	if r.ReadPodLog != nil {
		return r.ReadPodLog(ctx, namespace, podName, containerName, previous)
	}

	tailLines := int64(64)

	stream, err := r.Kubernetes.CoreV1().Pods(namespace).
		GetLogs(podName, &corev1.PodLogOptions{Container: containerName, Previous: previous, TailLines: &tailLines}).
		Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("open result log: %w", err)
	}

	tail := &resultLogTail{}

	_, err = io.Copy(tail, stream)
	if err != nil {
		closeErr := stream.Close()
		if closeErr != nil {
			return "", errors.Join(
				fmt.Errorf("read result log: %w", err),
				fmt.Errorf("close result log: %w", closeErr),
			)
		}

		return "", fmt.Errorf("read result log: %w", err)
	}

	if err := stream.Close(); err != nil {
		return "", fmt.Errorf("close result log: %w", err)
	}

	return string(tail.content), nil
}

type resultLogTail struct {
	content []byte
}

func (tail *resultLogTail) Write(content []byte) (int, error) {
	count := len(content)
	if len(content) >= maxResultLogBytes {
		tail.content = append(tail.content[:0], content[len(content)-maxResultLogBytes:]...)

		return count, nil
	}

	if excess := len(tail.content) + len(content) - maxResultLogBytes; excess > 0 {
		tail.content = tail.content[excess:]
	}

	tail.content = append(tail.content, content...)

	return count, nil
}

func resultLogSourceWithContent(content string, err error) resultLogSource {
	return resultLogSource{
		content:  content,
		err:      err,
		metadata: resultLogSourceMetadata(content, err),
	}
}

func resultLogSourceFromError(err error) resultLogSource {
	return resultLogSourceWithContent("", err)
}

func resultLogSourceMetadataOnly(status string) resultLogSource {
	return resultLogSource{metadata: ResultLogSourceMetadata{Status: status}}
}

func resultLogSourceMetadata(content string, err error) ResultLogSourceMetadata {
	if err != nil {
		return ResultLogSourceMetadata{Status: "unavailable"}
	}

	status := "missing"

	if strings.Contains(content, resultPrefix) {
		if _, parseErr := ParseResultLog(content, 0); parseErr != nil {
			status = "invalid"
		} else {
			status = "valid"
		}
	}

	return ResultLogSourceMetadata{
		Status: status,
		Bytes:  len(content),
	}
}

func (r *Runner) cleanup(
	ctx context.Context,
	namespace string,
	resources []createdResource,
) error {
	propagation := metav1.DeletePropagationForeground

	var errs []error

	for _, resource := range resources {
		err := r.Dynamic.Resource(resource.gvr).Namespace(namespace).Delete(
			ctx,
			resource.resource.GetName(),
			metav1.DeleteOptions{
				Preconditions:     &metav1.Preconditions{UID: ptr(resource.resource.GetUID())},
				PropagationPolicy: &propagation,
			},
		)
		if err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete %s %s: %w", resource.resource.GetKind(), resource.resource.GetName(), err))
		}
	}

	ticker := time.NewTicker(r.pollInterval())
	defer ticker.Stop()

	for _, resource := range resources {
		for {
			_, err := r.Dynamic.Resource(resource.gvr).Namespace(namespace).
				Get(ctx, resource.resource.GetName(), metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				break
			}

			if err != nil {
				errs = append(errs, fmt.Errorf("observe deletion of %s %s: %w", resource.resource.GetKind(), resource.resource.GetName(), err))

				break
			}

			select {
			case <-ctx.Done():
				errs = append(errs, fmt.Errorf(
					"wait for deletion of %s %s: %w",
					resource.resource.GetKind(),
					resource.resource.GetName(),
					ctx.Err(),
				))

				return errors.Join(errs...)
			case <-ticker.C:
			}
		}
	}

	return errors.Join(errs...)
}

func (r *Runner) pollInterval() time.Duration {
	if r.PollInterval > 0 {
		return r.PollInterval
	}

	return 2 * time.Second
}

func (r *Runner) cleanupWait() time.Duration {
	if r.CleanupWait > 0 {
		return r.CleanupWait
	}

	return 30 * time.Second
}

func ownedBy(references []metav1.OwnerReference, uid types.UID) bool {
	for _, reference := range references {
		if reference.UID == uid {
			return true
		}
	}

	return false
}
