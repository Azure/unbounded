// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signaladapter

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
)

func TestCompletedResultAcceptsVerdictAssociatedTerminalState(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		result           Result
		backendSucceeded bool
	}{
		"Pass": {
			result:           Result{ParticipantIndex: 0, ExecutionState: "Succeeded", Verdict: "Pass", Reason: "Healthy"},
			backendSucceeded: true,
		},
		"Fail": {
			result: Result{
				ParticipantIndex: 0,
				ExecutionState:   "Succeeded",
				Verdict:          "Fail",
				Reason:           "DiagnosticFailed",
				ExitCode:         1,
			},
		},
		"Inconclusive": {
			result: Result{
				ParticipantIndex: 0,
				ExecutionState:   "Succeeded",
				Verdict:          "Inconclusive",
				Reason:           "DiagnosticUnavailable",
				ExitCode:         2,
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			result := completedResult(test.result, test.backendSucceeded)
			if result.ExecutionState != "Succeeded" || result.Verdict != test.result.Verdict {
				t.Fatalf("completedResult() = %#v", result)
			}
		})
	}
}

func TestBackendCompletionResultKeepsValidInconclusiveFromFailedBackend(t *testing.T) {
	t.Parallel()

	logContent := encodedResultRecord("Inconclusive", "DiagnosticUnavailable", "device_state=unknown", 2)

	result := backendCompletionResult(logContent, nil, 0, false)
	if result.ExecutionState != "Succeeded" ||
		result.Verdict != "Inconclusive" ||
		result.Reason != "DiagnosticUnavailable" {
		t.Fatalf("backendCompletionResult() = %#v", result)
	}
}

func TestBackendCompletionResultFailsClosedWithoutValidResult(t *testing.T) {
	t.Parallel()

	for name, logContent := range map[string]string{
		"malformed result": resultPrefix + "%%% bm90LXVzZWQ= - 2\n",
		"missing result":   "diagnostic output only\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			result := backendCompletionResult(logContent, nil, 0, false)
			if result.ExecutionState != "Error" ||
				result.Verdict != "Inconclusive" ||
				result.Reason != "BackendExecutionFailed" {
				t.Fatalf("backendCompletionResult() = %#v", result)
			}
		})
	}
}

func TestBackendCompletionFromLogsRecoversPreviousResult(t *testing.T) {
	t.Parallel()

	logs := resultLogSnapshot{
		current: resultLogSourceWithContent("launcher restarted\n", nil),
		previous: resultLogSourceWithContent(
			encodedResultRecord("Inconclusive", "DiagnosticUnavailable", "state=unknown", 2),
			nil,
		),
	}

	result, hardFailure := backendCompletionFromLogs(logs, 0, false)
	if hardFailure ||
		result.ExecutionState != "Succeeded" ||
		result.Verdict != "Inconclusive" ||
		result.Reason != "DiagnosticUnavailable" ||
		result.ResultLog == nil ||
		result.ResultLog.SelectedSource != "previous" ||
		result.ResultLog.Current.Status != "missing" ||
		result.ResultLog.Previous.Status != "valid" {
		t.Fatalf("backendCompletionFromLogs() = %#v, hardFailure=%t", result, hardFailure)
	}
}

func TestBackendCompletionFromLogsPrefersCurrentResult(t *testing.T) {
	t.Parallel()

	record := encodedResultRecord("Pass", "Healthy", "state=ready", 0)
	logs := resultLogSnapshot{
		current:  resultLogSourceWithContent(record, nil),
		previous: resultLogSourceWithContent(record, nil),
	}

	result, hardFailure := backendCompletionFromLogs(logs, 0, true)
	if hardFailure ||
		result.ExecutionState != "Succeeded" ||
		result.Verdict != "Pass" ||
		result.ResultLog == nil ||
		result.ResultLog.SelectedSource != "current" {
		t.Fatalf("backendCompletionFromLogs() = %#v, hardFailure=%t", result, hardFailure)
	}
}

func TestBackendCompletionFromLogsRejectsConflicts(t *testing.T) {
	t.Parallel()

	logs := resultLogSnapshot{
		current: resultLogSourceWithContent(
			encodedResultRecord("Pass", "Healthy", "state=ready", 0),
			nil,
		),
		previous: resultLogSourceWithContent(
			encodedResultRecord("Inconclusive", "DiagnosticUnavailable", "state=unknown", 2),
			nil,
		),
	}

	result, hardFailure := backendCompletionFromLogs(logs, 0, false)
	if !hardFailure ||
		result.ExecutionState != "Error" ||
		result.Verdict != "Inconclusive" ||
		!strings.Contains(result.Reason, "conflicting results") ||
		result.ResultLog == nil ||
		result.ResultLog.Current.Status != "valid" ||
		result.ResultLog.Previous.Status != "valid" {
		t.Fatalf("backendCompletionFromLogs() = %#v, hardFailure=%t", result, hardFailure)
	}
}

func TestBackendCompletionFromLogsRejectsMalformedCurrentBeforePreviousFallback(t *testing.T) {
	t.Parallel()

	logs := resultLogSnapshot{
		current: resultLogSourceWithContent(resultPrefix+"%%% bm90LXVzZWQ= - 2\n", nil),
		previous: resultLogSourceWithContent(
			encodedResultRecord("Inconclusive", "DiagnosticUnavailable", "state=unknown", 2),
			nil,
		),
	}

	result, hardFailure := backendCompletionFromLogs(logs, 0, false)
	if !hardFailure ||
		result.ExecutionState != "Error" ||
		result.Reason != "BackendExecutionFailed" ||
		result.ResultLog == nil ||
		result.ResultLog.Current.Status != "invalid" {
		t.Fatalf("backendCompletionFromLogs() = %#v, hardFailure=%t", result, hardFailure)
	}
}

func TestResultLogsReadsCurrentAndPreviousLauncherLogs(t *testing.T) {
	t.Parallel()

	job := &unstructured.Unstructured{}
	job.SetAPIVersion("kubeflow.org/v2beta1")
	job.SetKind("MPIJob")
	job.SetName("run-p0")
	job.SetNamespace("project-signal")
	job.SetUID("job-uid")

	launcher := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "run-p0-launcher",
			Namespace: "project-signal",
			Labels: map[string]string{
				"signal.unbounded-cloud.io/run":               "run",
				"signal.unbounded-cloud.io/participant-index": "0",
			},
			OwnerReferences: []metav1.OwnerReference{{UID: "job-uid"}},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "launcher"}}},
	}

	var previousValues []bool

	runner := &Runner{
		Kubernetes: kubernetesfake.NewSimpleClientset(launcher),
		ReadPodLog: func(
			_ context.Context,
			namespace, podName, containerName string,
			previous bool,
		) (string, error) {
			if namespace != launcher.Namespace ||
				podName != launcher.Name ||
				containerName != "launcher" {
				t.Fatalf("read log identity = %s/%s %s", namespace, podName, containerName)
			}

			previousValues = append(previousValues, previous)

			return encodedResultRecord("Pass", "Healthy", "", 0), nil
		},
	}

	logs := runner.resultLogs(
		t.Context(),
		Config{Namespace: "project-signal", RunName: "run"},
		createdResource{resource: job, participantIndex: 0},
		true,
	)
	if len(previousValues) != 2 || previousValues[0] || !previousValues[1] {
		t.Fatalf("previous log reads = %#v", previousValues)
	}

	if logs.current.metadata.Status != "valid" || logs.previous.metadata.Status != "valid" {
		t.Fatalf("result log metadata = %#v", logs)
	}
}

func TestCleanupFailurePreservesExecutionFailure(t *testing.T) {
	t.Parallel()

	output, err := Aggregate(1, testBackendResources()[:1], []Result{{
		ParticipantIndex: 0,
		ExecutionState:   "Error",
		Verdict:          "Inconclusive",
		Reason:           "BackendExecutionFailed",
	}})
	if err != nil {
		t.Fatal(err)
	}

	recordCleanupFailure(&output)

	if output.ExecutionState != "Error" ||
		output.Verdict != "Inconclusive" ||
		output.Reason != "BackendExecutionFailed" ||
		output.CleanupFailure != "BackendCleanupFailed" {
		t.Fatalf("output = %#v", output)
	}
}

func TestCleanupFailurePreventsPass(t *testing.T) {
	t.Parallel()

	output, err := Aggregate(1, testBackendResources()[:1], []Result{{
		ParticipantIndex: 0,
		ExecutionState:   "Succeeded",
		Verdict:          "Pass",
		Reason:           "Healthy",
	}})
	if err != nil {
		t.Fatal(err)
	}

	recordCleanupFailure(&output)

	if output.ExecutionState != "Error" ||
		output.Verdict != "Inconclusive" ||
		output.Reason != "BackendCleanupFailed" ||
		output.CleanupFailure != "BackendCleanupFailed" {
		t.Fatalf("output = %#v", output)
	}
}

func TestCompletedResultRejectsTerminalStateMismatch(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		exitCode         int
		backendSucceeded bool
	}{
		"zero exit with failed backend":       {backendSucceeded: false},
		"nonzero exit with succeeded backend": {exitCode: 2, backendSucceeded: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			result := completedResult(Result{
				ParticipantIndex: 1,
				ExecutionState:   "Succeeded",
				Verdict:          "Inconclusive",
				Reason:           "SyntheticReason",
				ExitCode:         test.exitCode,
			}, test.backendSucceeded)
			if result.ExecutionState != "Error" ||
				result.Verdict != "Inconclusive" ||
				!strings.Contains(result.Reason, "contradicts backend terminal state") {
				t.Fatalf("completedResult() = %#v", result)
			}
		})
	}
}

func encodedResultRecord(verdict, reason, measurements string, exitCode int) string {
	encode := func(value string) string {
		if value == "" {
			return "-"
		}

		return base64.StdEncoding.EncodeToString([]byte(value))
	}

	return fmt.Sprintf(
		"%s%s %s %s %d\n",
		resultPrefix,
		encode(verdict),
		encode(reason),
		encode(measurements),
		exitCode,
	)
}

func TestCleanupDeletesCreatedBackends(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	pod := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":      "run-p0",
			"namespace": "project-signal",
			"uid":       "pod-uid",
		},
	}}
	job := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubeflow.org/v2beta1",
		"kind":       "MPIJob",
		"metadata": map[string]any{
			"name":      "run-p1",
			"namespace": "project-signal",
			"uid":       "job-uid",
		},
	}}

	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		scheme,
		map[schema.GroupVersionResource]string{
			podGVR:    "PodList",
			mpiJobGVR: "MPIJobList",
		},
		pod,
		job,
	)
	runner := &Runner{Dynamic: client, PollInterval: time.Millisecond}

	err := runner.cleanup(context.Background(), "project-signal", []createdResource{
		{gvr: podGVR, resource: pod, participantIndex: 0},
		{gvr: mpiJobGVR, resource: job, participantIndex: 1},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, item := range []struct {
		gvr  schema.GroupVersionResource
		name string
	}{
		{gvr: podGVR, name: pod.GetName()},
		{gvr: mpiJobGVR, name: job.GetName()},
	} {
		_, err := client.Resource(item.gvr).Namespace("project-signal").
			Get(context.Background(), item.name, metav1.GetOptions{})
		if err == nil {
			t.Fatalf("%s still exists", item.name)
		}
	}
}
