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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
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
