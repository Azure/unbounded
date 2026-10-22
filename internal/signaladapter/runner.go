// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signaladapter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
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
	Dynamic      dynamic.Interface
	Kubernetes   kubernetes.Interface
	PollInterval time.Duration
	CleanupWait  time.Duration
}

type createdResource struct {
	gvr              schema.GroupVersionResource
	resource         *unstructured.Unstructured
	participantIndex int32
}

func (r *Runner) Execute(ctx context.Context, config Config) (output AggregateOutput, resultErr error) {
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
		activeDeadlineSeconds,
	)
	if err != nil {
		return AggregateOutput{}, err
	}

	created := make([]createdResource, 0, len(resources))
	defer func() {
		if len(created) == 0 {
			return
		}

		cleanupContext, cancel := context.WithTimeout(context.Background(), r.cleanupWait())
		defer cancel()

		if err := r.cleanup(cleanupContext, config.Namespace, created); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("cleanup after adapter exit: %w", err))
		}
	}()

	references := make([]signalv1.BackendResourceReference, 0, len(resources))
	for index, resource := range resources {
		gvr := podGVR
		if resource.GetKind() == "MPIJob" {
			gvr = mpiJobGVR
		}

		createdObject, createErr := r.Dynamic.Resource(gvr).Namespace(config.Namespace).
			Create(executionContext, resource, metav1.CreateOptions{})
		if createErr != nil {
			return AggregateOutput{}, fmt.Errorf("create participant %d %s: %w", index, resource.GetKind(), createErr)
		}

		created = append(created, createdResource{
			gvr:              gvr,
			resource:         createdObject,
			participantIndex: int32(index),
		})

		reference, referenceErr := BackendResource(createdObject, int32(index))
		if referenceErr != nil {
			return AggregateOutput{}, fmt.Errorf("record participant %d backend identity: %w", index, referenceErr)
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

	output, err = Aggregate(len(participants), references, results)
	if err != nil {
		return AggregateOutput{}, err
	}

	cleanupContext, cancel := context.WithTimeout(context.Background(), r.cleanupWait())
	defer cancel()

	if err := r.cleanup(cleanupContext, config.Namespace, created); err != nil {
		output.ExecutionState = "Error"
		output.Verdict = "Inconclusive"
		output.Reason = "BackendCleanupFailed"

		return output, nil
	}

	created = nil

	return output, nil
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
			logContent, logErr := r.resultLog(ctx, config, backend)
			if logErr == nil {
				parsed, parseErr := ParseResultLog(logContent, int(backend.participantIndex))
				if parseErr == nil {
					capturedResult = &parsed
				}
			}
		}

		if terminal {
			if capturedResult != nil {
				return completedResult(*capturedResult, succeeded)
			}

			logContent, logErr := r.resultLog(ctx, config, backend)

			return backendCompletionResult(
				logContent,
				logErr,
				int(backend.participantIndex),
				succeeded,
			)
		}

		select {
		case <-ctx.Done():
			result.Reason = "BackendDeadlineExceeded"

			return result
		case <-ticker.C:
		}
	}
}

func backendCompletionResult(
	logContent string,
	logErr error,
	participantIndex int,
	backendSucceeded bool,
) Result {
	result := Result{
		ParticipantIndex: participantIndex,
		ExecutionState:   "Error",
		Verdict:          "Inconclusive",
		Reason:           "BackendExecutionFailed",
	}

	if logErr != nil {
		if backendSucceeded {
			result.Reason = "BackendResultUnavailable: " + logErr.Error()
		}

		return result
	}

	parsed, err := ParseResultLog(logContent, participantIndex)
	if err != nil {
		if backendSucceeded {
			result.Reason = "BackendResultInvalid: " + err.Error()
		}

		return result
	}

	return completedResult(parsed, backendSucceeded)
}

func completedResult(result Result, backendSucceeded bool) Result {
	if (result.ExitCode == 0) != backendSucceeded {
		return Result{
			ParticipantIndex: result.ParticipantIndex,
			ExecutionState:   "Error",
			Verdict:          "Inconclusive",
			Reason:           "BackendResultInvalid: result exit code contradicts backend terminal state",
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

func (r *Runner) resultLog(
	ctx context.Context,
	config Config,
	backend createdResource,
) (string, error) {
	podName := backend.resource.GetName()
	containerName := "main"

	if backend.resource.GetKind() == "MPIJob" {
		pods, err := r.Kubernetes.CoreV1().Pods(config.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: strings.Join([]string{
				"signal.unbounded-cloud.io/run=" + config.RunName,
				"signal.unbounded-cloud.io/participant-index=" +
					strconv.FormatInt(int64(backend.participantIndex), 10),
			}, ","),
		})
		if err != nil {
			return "", fmt.Errorf("list MPIJob Pods: %w", err)
		}

		podName = ""

		for index := range pods.Items {
			pod := &pods.Items[index]
			if !ownedBy(pod.OwnerReferences, backend.resource.GetUID()) {
				continue
			}

			for _, container := range pod.Spec.Containers {
				if container.Name == "launcher" {
					podName = pod.Name
					containerName = container.Name

					break
				}
			}
		}

		if podName == "" {
			return "", errors.New("MPIJob launcher Pod was not found")
		}
	}

	stream, err := r.Kubernetes.CoreV1().Pods(config.Namespace).
		GetLogs(podName, &corev1.PodLogOptions{Container: containerName}).
		Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("open result log: %w", err)
	}

	content, err := io.ReadAll(io.LimitReader(stream, 64*1024))
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

	return string(content), nil
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
