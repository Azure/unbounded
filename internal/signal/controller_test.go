// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

func TestValidateCheckPlanVariableLengths(t *testing.T) {
	t.Parallel()

	for _, count := range []int{
		signalv1.MinCertificationChecks,
		16,
		signalv1.MaxCertificationChecks,
	} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			t.Parallel()

			checks := profileCheckPlan(count, 2*time.Minute)

			deadline, err := validateCheckPlan(checks)
			if err != nil {
				t.Fatalf("validateCheckPlan() error = %v", err)
			}

			want := time.Duration(count) * (2*time.Minute + signalv1.MinimumBackendDeadlineReserve)
			if deadline != want {
				t.Fatalf("deadline = %s, want %s", deadline, want)
			}
		})
	}

	for _, count := range []int{0, signalv1.MaxCertificationChecks + 1} {
		t.Run("reject-"+strconv.Itoa(count), func(t *testing.T) {
			t.Parallel()

			if _, err := validateCheckPlan(profileCheckPlan(count, time.Minute)); err == nil {
				t.Fatalf("validateCheckPlan() accepted %d checks", count)
			}
		})
	}
}

func TestValidateRunCheckPlanAssignsPositionalIdentity(t *testing.T) {
	t.Parallel()

	checks := runCheckPlan(16, 90*time.Second)

	deadline, err := validateRunCheckPlan(checks)
	if err != nil {
		t.Fatalf("validateRunCheckPlan() error = %v", err)
	}

	wantDeadline := 16 * (90*time.Second + signalv1.MinimumBackendDeadlineReserve)
	if deadline != wantDeadline {
		t.Fatalf("deadline = %s, want %s", deadline, wantDeadline)
	}

	for index := range checks {
		wantID := signalv1.CheckID("R" + fmtCheckNumber(index+1))
		if checks[index].ID != wantID {
			t.Fatalf("checks[%d].ID = %q, want %q", index, checks[index].ID, wantID)
		}

		if checks[index].Order != int32(index) {
			t.Fatalf("checks[%d].Order = %d, want %d", index, checks[index].Order, index)
		}
	}

	for _, count := range []int{0, signalv1.MaxCertificationChecks + 1} {
		if _, err := validateRunCheckPlan(runCheckPlan(count, time.Minute)); err == nil {
			t.Fatalf("validateRunCheckPlan() accepted %d checks", count)
		}
	}
}

func TestProfileChecksAssignsPositionalIdentity(t *testing.T) {
	t.Parallel()

	profile := &signalv1.CertificationProfile{
		Spec: signalv1.CertificationProfileSpec{
			Checks: profileCheckPlan(signalv1.MaxCertificationChecks, time.Minute),
		},
	}

	checks := profileChecks(profile)

	for _, index := range []int{0, 15, signalv1.MaxCertificationChecks - 1} {
		wantID := signalv1.CheckID("R" + fmtCheckNumber(index+1))
		if checks[index].ID != wantID {
			t.Fatalf("checks[%d].ID = %q, want %q", index, checks[index].ID, wantID)
		}

		if checks[index].Order != int32(index) {
			t.Fatalf("checks[%d].Order = %d, want %d", index, checks[index].Order, index)
		}
	}
}

func TestBuildWorkflowVariableSuiteIsSequentialAndBounded(t *testing.T) {
	t.Parallel()

	for _, count := range []int{1, 16, signalv1.MaxCertificationChecks} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			t.Parallel()

			run := certificationRun(count)

			workflow, err := buildWorkflow(run)
			if err != nil {
				t.Fatalf("buildWorkflow() error = %v", err)
			}

			content, err := json.Marshal(workflow.Object)
			if err != nil {
				t.Fatalf("encode Workflow: %v", err)
			}

			if len(content) > maximumWorkflowSpecBytes {
				t.Fatalf("Workflow size = %d, maximum = %d", len(content), maximumWorkflowSpecBytes)
			}

			templates, found, err := unstructured.NestedSlice(workflow.Object, "spec", "templates")
			if err != nil || !found || len(templates) != 1 {
				t.Fatalf("Workflow templates = %#v, found = %t, error = %v", templates, found, err)
			}

			template := templates[0].(map[string]any)
			dag := template["dag"].(map[string]any)
			tasks := dag["tasks"].([]any)

			if len(tasks) != count {
				t.Fatalf("task count = %d, want %d", len(tasks), count)
			}

			resultNames := make(map[string]struct{}, count)

			for index := range tasks {
				task := tasks[index].(map[string]any)
				if index == 0 {
					if _, exists := task["depends"]; exists {
						t.Fatal("first task has a dependency")
					}
				} else {
					previous := tasks[index-1].(map[string]any)["name"].(string)
					if task["depends"] != previous+".Succeeded" {
						t.Fatalf("tasks[%d].depends = %q, want %q", index, task["depends"], previous+".Succeeded")
					}
				}

				arguments := task["arguments"].(map[string]any)
				parameters := arguments["parameters"].([]any)

				if len(parameters) != 5 {
					t.Fatalf("tasks[%d] parameter count = %d, want 5", index, len(parameters))
				}

				values := make(map[string]string, len(parameters))
				for _, parameter := range parameters {
					entry := parameter.(map[string]any)
					name := entry["name"].(string)

					switch name {
					case "run-name", "execution-name", "run-uid", "check-index", "result-configmap":
					default:
						t.Fatalf("tasks[%d] repeats unbounded parameter %q", index, name)
					}

					values[name] = entry["value"].(string)
				}

				if values["check-index"] != strconv.Itoa(index) {
					t.Fatalf("tasks[%d] check-index = %q, want %d", index, values["check-index"], index)
				}

				if len(values["result-configmap"]) > 63 {
					t.Fatalf("tasks[%d] result ConfigMap name has %d characters", index, len(values["result-configmap"]))
				}

				if _, duplicate := resultNames[values["result-configmap"]]; duplicate {
					t.Fatalf("tasks[%d] duplicates result ConfigMap %q", index, values["result-configmap"])
				}

				resultNames[values["result-configmap"]] = struct{}{}
			}
		})
	}
}

func TestReadCheckResultEnforcesBoundedImmutableIdentity(t *testing.T) {
	t.Parallel()

	run := certificationRun(1)
	check := &run.Spec.Checks[0]

	if _, err := validateRunCheckPlan(run.Spec.Checks); err != nil {
		t.Fatalf("validateRunCheckPlan() error = %v", err)
	}

	workflow := &unstructured.Unstructured{}
	workflow.SetGroupVersionKind(workflowGVK)
	workflow.SetName(run.Name + "-a1")
	workflow.SetNamespace(run.Namespace)
	workflow.SetUID(types.UID("workflow-uid"))

	content, err := json.Marshal(adapterCheckResult{
		ExecutionState:   "Completed",
		Verdict:          "Pass",
		Reason:           "AllParticipantsPassed",
		Measurements:     "{}",
		BackendResources: "[]",
		BackendEvidence:  "{}",
	})
	if err != nil {
		t.Fatalf("encode result: %v", err)
	}

	immutable := true
	result := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      checkResultName(run.Name, check.ID),
			Namespace: run.Namespace,
			Labels: map[string]string{
				SnapshotRunLabel: run.Name,
			},
			Annotations: map[string]string{
				checkResultDigest:     digestBytes(content),
				checkResultRunUID:     string(run.UID),
				checkResultCheckID:    string(check.ID),
				checkResultCheckOrder: strconv.FormatInt(int64(check.Order), 10),
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(workflow, workflowGVK),
			},
		},
		Immutable: &immutable,
		BinaryData: map[string][]byte{
			checkResultDataKey: content,
		},
	}

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}

	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(result).Build()
	reconciler := &RunReconciler{Client: kube, APIReader: kube}

	if _, err := reconciler.readCheckResult(context.Background(), workflow, run, check); err != nil {
		t.Fatalf("readCheckResult() error = %v", err)
	}

	mutable := result.DeepCopy()
	mutable.Immutable = nil
	mutableClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mutable).Build()
	mutableReconciler := &RunReconciler{Client: mutableClient, APIReader: mutableClient}

	if _, err := mutableReconciler.readCheckResult(context.Background(), workflow, run, check); err == nil {
		t.Fatal("readCheckResult() accepted a mutable ConfigMap")
	}

	wrongDigest := result.DeepCopy()
	wrongDigest.Annotations[checkResultDigest] = digestBytes([]byte("different"))
	digestClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(wrongDigest).Build()
	digestReconciler := &RunReconciler{Client: digestClient, APIReader: digestClient}

	if _, err := digestReconciler.readCheckResult(context.Background(), workflow, run, check); err == nil {
		t.Fatal("readCheckResult() accepted a mismatched digest")
	}

	maximum := adapterCheckResult{
		ExecutionState:   "Completed",
		Verdict:          strings.Repeat("v", checkResultFieldLimits["verdict"]),
		Reason:           strings.Repeat("\x00", maxAdapterReasonBytes),
		Measurements:     strings.Repeat("\x00", maxAdapterMeasurementsBytes),
		BackendResources: strings.Repeat("\x00", maxAdapterBackendOutputBytes),
		BackendEvidence:  strings.Repeat("\x00", maxAdapterBackendEvidenceBytes),
	}

	maximumContent, err := json.Marshal(maximum)
	if err != nil {
		t.Fatalf("encode maximum result: %v", err)
	}

	if len(maximumContent) > maximumCheckResultBytes {
		t.Fatalf("maximum encoded result size = %d, limit = %d", len(maximumContent), maximumCheckResultBytes)
	}

	if _, err := decodeCheckResult(maximumContent); err != nil {
		t.Fatalf("decodeCheckResult() rejected maximum result: %v", err)
	}

	oversized := adapterCheckResult{
		ExecutionState:   "Completed",
		Verdict:          "Pass",
		Reason:           "AllParticipantsPassed",
		Measurements:     strings.Repeat("x", maxAdapterMeasurementsBytes+1),
		BackendResources: "[]",
		BackendEvidence:  "{}",
	}

	oversizedContent, err := json.Marshal(oversized)
	if err != nil {
		t.Fatalf("encode oversized result: %v", err)
	}

	if _, err := decodeCheckResult(oversizedContent); err == nil {
		t.Fatal("decodeCheckResult() accepted oversized measurements")
	}
}

func profileCheckPlan(count int, deadline time.Duration) []signalv1.CertificationCheck {
	checks := make([]signalv1.CertificationCheck, count)
	for index := range checks {
		checks[index] = signalv1.CertificationCheck{
			ID:                    signalv1.CheckID("R" + fmtCheckNumber(index+1)),
			Order:                 int32(index),
			Backend:               signalv1.ExecutionBackendPod,
			BackendActiveDeadline: metav1.Duration{Duration: deadline},
		}
	}

	return checks
}

func runCheckPlan(count int, deadline time.Duration) []signalv1.CertificationCheckSnapshot {
	checks := make([]signalv1.CertificationCheckSnapshot, count)
	for index := range checks {
		checks[index] = signalv1.CertificationCheckSnapshot{
			Backend:               signalv1.ExecutionBackendPod,
			BackendActiveDeadline: metav1.Duration{Duration: deadline},
		}
	}

	return checks
}

func certificationRun(count int) *signalv1.CertificationRun {
	return &signalv1.CertificationRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "variable-suite-" + strings.Repeat("a", 40),
			Namespace: "project-signal",
			UID:       types.UID("run-uid"),
		},
		Spec: signalv1.CertificationRunSpec{
			Checks: runCheckPlan(count, 2*time.Minute),
		},
	}
}

func fmtCheckNumber(value int) string {
	return fmt.Sprintf("%02d", value)
}
