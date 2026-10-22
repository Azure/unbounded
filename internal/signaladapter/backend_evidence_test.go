// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signaladapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
)

func TestCaptureBackendEvidencePreservesBackoffFailureAndPreviousInvalidResult(t *testing.T) {
	t.Parallel()

	const (
		namespace      = "project-signal"
		mpiJobUID      = types.UID("mpi-job-uid")
		launcherJobUID = types.UID("launcher-job-uid")
	)

	mpiJob := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubeflow.org/v2beta1",
		"kind":       "MPIJob",
		"metadata": map[string]any{
			"name":      "run-p0",
			"namespace": namespace,
			"uid":       string(mpiJobUID),
		},
		"status": map[string]any{
			"conditions": []any{map[string]any{
				"type":    "Failed",
				"status":  "True",
				"reason":  "LauncherJobFailed",
				"message": "launcher failed token=secret-value",
			}},
			"replicaStatuses": map[string]any{
				"Launcher": map[string]any{"failed": int64(1)},
				"Worker":   map[string]any{"active": int64(1)},
			},
		},
	}}
	launcherJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "run-p0-launcher",
			Namespace:       namespace,
			UID:             launcherJobUID,
			OwnerReferences: []metav1.OwnerReference{{UID: mpiJobUID}},
		},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type:    batchv1.JobFailed,
			Status:  corev1.ConditionTrue,
			Reason:  batchv1.JobReasonBackoffLimitExceeded,
			Message: "Job has reached the specified backoff limit",
		}}},
	}
	launcher := evidenceTestPod(
		"run-p0-launcher-pod",
		"launcher-pod-uid",
		"launcher",
		[]metav1.OwnerReference{{UID: launcherJobUID}},
		1,
	)
	worker := evidenceTestPod(
		"run-p0-worker-0",
		"worker-pod-uid",
		"worker",
		[]metav1.OwnerReference{{UID: mpiJobUID}},
		0,
	)

	dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{mpiJobGVR: "MPIJobList"},
		mpiJob,
	)
	runner := &Runner{
		Dynamic:    dynamicClient,
		Kubernetes: kubernetesfake.NewSimpleClientset(launcherJob, launcher, worker),
		ReadPodLog: func(
			_ context.Context,
			_, podName, containerName string,
			previous bool,
		) (string, error) {
			switch {
			case podName == launcher.Name && containerName == "launcher" && !previous:
				return "current launcher log password=hunter2\n", nil
			case podName == launcher.Name && containerName == "launcher" && previous:
				return resultsStatusPrefix +
					"verdict=present reason=present measurements=missing\n" +
					resultPrefix + "%%% bm90LXVzZWQ= - 2\n", nil
			case podName == worker.Name && containerName == "worker" && !previous:
				return resultsStatusPrefix + "prepared\nworker ready\n", nil
			default:
				return "", errors.New("unexpected log request")
			}
		},
	}

	content, complete, err := runner.captureBackendEvidence(
		t.Context(),
		Config{Namespace: namespace, RunName: "run"},
		[]createdResource{{
			gvr:              mpiJobGVR,
			resource:         mpiJob,
			participantIndex: 0,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}

	if !complete {
		t.Fatalf("captureBackendEvidence complete = false, content=%s", content)
	}

	var evidence BackendEvidence
	if err := json.Unmarshal([]byte(content), &evidence); err != nil {
		t.Fatal(err)
	}

	participant := evidence.Participants[0]
	if participant.LauncherJob == nil ||
		len(participant.LauncherJob.Conditions) != 1 ||
		participant.LauncherJob.Conditions[0].Reason != batchv1.JobReasonBackoffLimitExceeded {
		t.Fatalf("launcher Job evidence = %#v", participant.LauncherJob)
	}

	launcherEvidence := podEvidenceByRole(t, participant.Pods, "launcher")

	container := launcherEvidence.Containers[0]
	if container.RestartCount != 1 ||
		container.State.ExitCode != 1 ||
		container.PreviousLog.ResultMarkerStatus != "invalid" ||
		container.PreviousLog.ResultDirectoryStatus !=
			"verdict=present reason=present measurements=missing" {
		t.Fatalf("launcher container evidence = %#v", container)
	}

	if strings.Contains(container.CurrentLog.Content, "hunter2") ||
		!strings.Contains(container.CurrentLog.Content, "password=[REDACTED]") {
		t.Fatalf("current log was not redacted: %q", container.CurrentLog.Content)
	}

	result, hardFailure := backendCompletionFromLogs(
		resultLogSnapshot{
			current: resultLogSourceWithContent("current launcher log\n", nil),
			previous: resultLogSourceWithContent(
				resultPrefix+"%%% bm90LXVzZWQ= - 2\n",
				nil,
			),
		},
		0,
		false,
	)
	if !hardFailure {
		t.Fatal("invalid previous result did not fail closed")
	}

	output, err := Aggregate(1, testBackendResources()[:1], []Result{result})
	if err != nil {
		t.Fatal(err)
	}

	output.BackendEvidence = content
	recordCleanupFailure(&output)

	if output.Reason != "BackendExecutionFailed" ||
		output.CleanupFailure != "BackendCleanupFailed" ||
		output.Verdict != "Inconclusive" {
		t.Fatalf("output = %#v", output)
	}
}

func TestCaptureBackendEvidenceProjectsParticipantPod(t *testing.T) {
	t.Parallel()

	backend := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":      "run-p0",
			"namespace": "project-signal",
			"uid":       "pod-uid",
		},
		"status": map[string]any{"phase": "Succeeded"},
	}}
	pod := evidenceTestPod("run-p0", "pod-uid", "main", nil, 0)
	pod.Status.Phase = corev1.PodSucceeded
	pod.Status.ContainerStatuses[0].State.Terminated.ExitCode = 0
	pod.Status.ContainerStatuses[0].State.Terminated.Reason = "Completed"

	runner := &Runner{
		Dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
			runtime.NewScheme(),
			map[schema.GroupVersionResource]string{podGVR: "PodList"},
			backend,
		),
		Kubernetes: kubernetesfake.NewSimpleClientset(pod),
		ReadPodLog: func(
			_ context.Context,
			_, _, _ string,
			previous bool,
		) (string, error) {
			if previous {
				t.Fatal("read previous log without a restart")
			}

			return resultsStatusPrefix + "prepared\n", nil
		},
	}

	content, complete, err := runner.captureBackendEvidence(
		context.Background(),
		Config{Namespace: "project-signal", RunName: "run"},
		[]createdResource{{
			gvr:              podGVR,
			resource:         backend,
			participantIndex: 0,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}

	if !complete {
		t.Fatalf("captureBackendEvidence complete = false, content=%s", content)
	}

	var evidence BackendEvidence
	if err := json.Unmarshal([]byte(content), &evidence); err != nil {
		t.Fatal(err)
	}

	participant := evidence.Participants[0]
	if participant.Resource.Phase != "Succeeded" ||
		len(participant.Pods) != 1 ||
		participant.Pods[0].Role != "participant" ||
		participant.Pods[0].Containers[0].PreviousLog.Status != "not-applicable" {
		t.Fatalf("participant evidence = %#v", participant)
	}
}

func TestCaptureBackendEvidenceFailurePreventsSuccess(t *testing.T) {
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

	recordEvidenceCaptureFailure(&output)

	if output.ExecutionState != "Error" ||
		output.Verdict != "Inconclusive" ||
		output.Reason != "BackendEvidenceCaptureFailed" {
		t.Fatalf("output = %#v", output)
	}
}

func TestPersistBackendEvidenceFailurePreventsSuccess(t *testing.T) {
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

	output.BackendEvidence = `{"version":"v1","complete":true,"participants":[{}]}`
	runner := &Runner{
		PersistBackendEvidence: func(string) error {
			return errors.New("write failed")
		},
	}

	if err := runner.persistEvidenceBeforeCleanup(&output); err != nil {
		t.Fatal(err)
	}

	var evidence BackendEvidence
	if err := json.Unmarshal([]byte(output.BackendEvidence), &evidence); err != nil {
		t.Fatal(err)
	}

	if output.ExecutionState != "Error" ||
		output.Verdict != "Inconclusive" ||
		output.Reason != "BackendEvidenceCaptureFailed" ||
		evidence.Complete ||
		!strings.Contains(evidence.CaptureError, "persist backend evidence before cleanup") {
		t.Fatalf("output = %#v, evidence = %#v", output, evidence)
	}
}

func TestCapturedLogIsBoundedAndRedacted(t *testing.T) {
	t.Parallel()

	content := strings.Repeat("x", maxEvidenceLogBytes+64) +
		"\nAuthorization: Bearer secret-token\n" +
		"https://user:password@example.test/path\n"
	captured := capturedLog(content, nil)

	if captured.CapturedBytes > maxEvidenceLogBytes ||
		!captured.Truncated ||
		strings.Contains(captured.Content, "secret-token") ||
		strings.Contains(captured.Content, "user:password") {
		t.Fatalf("captured log = %#v", captured)
	}
}

func TestCapturedLogRedactsSensitiveValuesInRetainedTail(t *testing.T) {
	t.Parallel()

	content := strings.Repeat("x", maxEvidenceLogBytes+64) +
		"\nAuthorization: " + "Bearer " + "opaque-value\n" +
		"https://" + "alice:credential-value@" + "example.test/path\n"
	captured := capturedLog(content, nil)

	if strings.Contains(captured.Content, "opaque-value") ||
		strings.Contains(captured.Content, "alice:credential-value") {
		t.Fatalf("captured log = %#v", captured)
	}
}

func TestResultDirectoryStatusRejectsUnstructuredMarker(t *testing.T) {
	t.Parallel()

	const value = "do-not-retain"

	status := resultDirectoryStatus(resultsStatusPrefix + value)
	if status != "invalid" || strings.Contains(status, value) {
		t.Fatalf("resultDirectoryStatus() = %q", status)
	}
}

func TestMarshalBackendEvidenceBoundsOversizedProjection(t *testing.T) {
	t.Parallel()

	evidence := BackendEvidence{
		Version:      backendEvidenceVersion,
		Complete:     true,
		Participants: make([]ParticipantBackendEvidence, 64),
	}
	for participantIndex := range evidence.Participants {
		participant := &evidence.Participants[participantIndex]
		participant.ParticipantIndex = participantIndex
		participant.Resource = BackendObjectEvidence{
			APIVersion: "kubeflow.org/v2beta1",
			Kind:       "MPIJob",
			Namespace:  "project-signal",
			Name:       fmt.Sprintf("run-p%d", participantIndex),
			UID:        types.UID(fmt.Sprintf("mpi-%d", participantIndex)),
			Message:    strings.Repeat("m", maxEvidenceTextBytes),
		}

		participant.Pods = make([]PodEvidence, 4)
		for podIndex := range participant.Pods {
			pod := &participant.Pods[podIndex]
			pod.Name = fmt.Sprintf("run-p%d-%d", participantIndex, podIndex)
			pod.UID = types.UID(fmt.Sprintf("pod-%d-%d", participantIndex, podIndex))
			pod.Containers = []ContainerEvidence{{
				Name: "participant",
				CurrentLog: CapturedLogEvidence{
					Status:        "captured",
					Bytes:         maxEvidenceLogBytes,
					CapturedBytes: maxEvidenceLogBytes,
					Content:       strings.Repeat("x", maxEvidenceLogBytes),
				},
				PreviousLog: CapturedLogEvidence{
					Status:        "captured",
					Bytes:         maxEvidenceLogBytes,
					CapturedBytes: maxEvidenceLogBytes,
					Content:       strings.Repeat("y", maxEvidenceLogBytes),
				},
			}}
		}
	}

	content, complete, err := marshalBackendEvidence(evidence)
	if err != nil {
		t.Fatal(err)
	}

	if complete {
		t.Fatal("oversized evidence remained complete")
	}

	if len(content) > MaxBackendEvidenceBytes {
		t.Fatalf("backend evidence size = %d, want <= %d", len(content), MaxBackendEvidenceBytes)
	}

	if err := ValidateBackendEvidence(string(content)); err != nil {
		t.Fatalf("ValidateBackendEvidence() error = %v", err)
	}

	var bounded BackendEvidence
	if err := json.Unmarshal(content, &bounded); err != nil {
		t.Fatal(err)
	}

	if bounded.Complete || bounded.Participants[0].CaptureError == "" {
		t.Fatalf("bounded evidence = %#v", bounded)
	}
}

func evidenceTestPod(
	name string,
	uid types.UID,
	containerName string,
	owners []metav1.OwnerReference,
	restarts int32,
) *corev1.Pod {
	lastState := corev1.ContainerState{}
	if restarts > 0 {
		lastState.Terminated = &corev1.ContainerStateTerminated{
			ExitCode: 1,
			Reason:   "Error",
		}
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       "project-signal",
			UID:             uid,
			OwnerReferences: owners,
			Labels: map[string]string{
				"signal.unbounded-cloud.io/run":               "run",
				"signal.unbounded-cloud.io/participant-index": "0",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: containerName}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         containerName,
				RestartCount: restarts,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 1,
					Reason:   "Error",
				}},
				LastTerminationState: lastState,
			}},
		},
	}
}

func podEvidenceByRole(t *testing.T, pods []PodEvidence, role string) PodEvidence {
	t.Helper()

	for _, pod := range pods {
		if pod.Role == role {
			return pod
		}
	}

	t.Fatalf("pod role %q was not captured: %#v", role, pods)

	return PodEvidence{}
}
