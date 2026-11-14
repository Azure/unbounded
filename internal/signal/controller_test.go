// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

			checks := suiteCheckPlan(count, 2*time.Minute)

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

			if _, err := validateCheckPlan(suiteCheckPlan(count, time.Minute)); err == nil {
				t.Fatalf("validateCheckPlan() accepted %d checks", count)
			}
		})
	}
}

func TestExecutionAuthorityFailsClosed(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	immutable := true
	authorityName := "project-signal-execution-authority-aaaaaaaaaaaa-bbbbbbbbbbbb"
	valid := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      authorityName,
			Namespace: "signal-authority",
		},
		Immutable: &immutable,
		Data: map[string]string{
			"acceptanceSuitesEnabled": "true",
			"cutoverApproved":         "true",
			"sourceCommit":            strings.Repeat("a", 40),
			"staticPackageDigest":     "sha256:" + strings.Repeat("b", 64),
		},
	}
	validState := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ExecutionStateName,
			Namespace: "signal-authority",
		},
		Data: map[string]string{"hardAbort": "false"},
	}

	for _, tt := range []struct {
		name        string
		mutate      func(*corev1.ConfigMap)
		mutateState func(*corev1.ConfigMap)
		valid       bool
	}{
		{name: "valid", valid: true},
		{
			name: "cutover absent",
			mutate: func(authority *corev1.ConfigMap) {
				delete(authority.Data, "cutoverApproved")
			},
		},
		{
			name: "mutable",
			mutate: func(authority *corev1.ConfigMap) {
				authority.Immutable = nil
			},
		},
		{
			name: "different source commit",
			mutate: func(authority *corev1.ConfigMap) {
				authority.Data["sourceCommit"] = strings.Repeat("b", 40)
			},
		},
		{
			name: "different static package",
			mutate: func(authority *corev1.ConfigMap) {
				authority.Data["staticPackageDigest"] = "sha256:" + strings.Repeat("c", 64)
			},
		},
		{
			name: "state hard abort active",
			mutateState: func(state *corev1.ConfigMap) {
				state.Data["hardAbort"] = "true"
			},
		},
		{
			name: "immutable state",
			mutateState: func(state *corev1.ConfigMap) {
				immutable := true
				state.Immutable = &immutable
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			authority := valid.DeepCopy()
			if tt.mutate != nil {
				tt.mutate(authority)
			}

			state := validState.DeepCopy()
			if tt.mutateState != nil {
				tt.mutateState(state)
			}

			kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(authority, state).Build()
			reconciler := &AdmissionReconciler{
				Client:              kube,
				APIReader:           kube,
				Namespace:           "signal-system",
				AuthorityNamespace:  "signal-authority",
				AuthorityName:       authorityName,
				SourceCommit:        strings.Repeat("a", 40),
				StaticPackageDigest: "sha256:" + strings.Repeat("b", 64),
			}

			err := reconciler.requireExecutionAuthority(context.Background())
			if tt.valid && err != nil {
				t.Fatalf("requireExecutionAuthority() error = %v", err)
			}

			if !tt.valid && err == nil {
				t.Fatal("requireExecutionAuthority() accepted unsafe authority")
			}

			runReconciler := &RunReconciler{
				Client:              kube,
				APIReader:           kube,
				AuthorityNamespace:  "signal-authority",
				AuthorityName:       authorityName,
				SourceCommit:        strings.Repeat("a", 40),
				StaticPackageDigest: "sha256:" + strings.Repeat("b", 64),
			}

			runErr := runReconciler.requireExecutionAuthority(context.Background())
			if tt.valid && runErr != nil {
				t.Fatalf("RunReconciler.requireExecutionAuthority() error = %v", runErr)
			}

			if !tt.valid && runErr == nil {
				t.Fatal("RunReconciler.requireExecutionAuthority() accepted unsafe authority")
			}
		})
	}
}

func TestBuildResolvedWorkflowOrdersPhasesAndConcurrentStages(t *testing.T) {
	t.Parallel()

	run := resolvedWorkflowTestRun()

	workflow, err := buildWorkflow(run)
	if err != nil {
		t.Fatalf("buildWorkflow() error = %v", err)
	}

	templates, found, err := unstructured.NestedSlice(workflow.Object, "spec", "templates")
	if err != nil || !found || len(templates) != 1 {
		t.Fatalf("workflow templates = %#v, found %t, error %v", templates, found, err)
	}

	template := templates[0].(map[string]any)
	dag := template["dag"].(map[string]any)

	dagTasks := dag["tasks"].([]any)
	if len(dagTasks) != 3 {
		t.Fatalf("DAG task count = %d, want 3", len(dagTasks))
	}

	startup := dagTasks[0].(map[string]any)
	optional := dagTasks[1].(map[string]any)
	validation := dagTasks[2].(map[string]any)

	templateRef := startup["templateRef"].(map[string]any)
	if templateRef["name"] != adapterWorkflowTemplateName || templateRef["template"] != "certification" {
		t.Fatalf("resolved stage templateRef = %#v", templateRef)
	}

	if _, exists := startup["depends"]; exists {
		t.Fatal("first phase startup stage unexpectedly depends on another task")
	}

	if _, exists := optional["depends"]; exists {
		t.Fatal("concurrent first-phase stage unexpectedly depends on another task")
	}

	depends, ok := validation["depends"].(string)
	if !ok ||
		!strings.Contains(depends, startup["name"].(string)+".Succeeded") ||
		!strings.Contains(depends, optional["name"].(string)+".Failed") {
		t.Fatalf("validation dependency = %q", depends)
	}

	gotDeadline, found, err := unstructured.NestedInt64(workflow.Object, "spec", "activeDeadlineSeconds")
	if err != nil || !found {
		t.Fatalf("workflow deadline missing: found %t, error %v", found, err)
	}

	wantDeadline, err := signalv1.DeriveResolvedPlanDeadline(run.Spec.ResolvedPlan.Phases)
	if err != nil {
		t.Fatalf("derive expected deadline: %v", err)
	}

	if gotDeadline != int64(wantDeadline.Seconds()) {
		t.Fatalf("workflow deadline = %d, want %d", gotDeadline, int64(wantDeadline.Seconds()))
	}
}

func TestBuildResolvedWorkflowUsesExplicitStageDependencies(t *testing.T) {
	t.Parallel()

	run := resolvedWorkflowTestRun()
	run.Spec.ResolvedPlan.Phases[1].Stages[0].DependsOn = []signalv1.StageDependency{{
		Stage:     "prepare",
		Condition: signalv1.StageDependencyConditionCompleted,
	}}

	workflow, err := buildWorkflow(run)
	if err != nil {
		t.Fatalf("buildWorkflow() error = %v", err)
	}

	templates, found, err := unstructured.NestedSlice(workflow.Object, "spec", "templates")
	if err != nil || !found {
		t.Fatalf("workflow templates missing: found %t, error %v", found, err)
	}

	tasks := templates[0].(map[string]any)["dag"].(map[string]any)["tasks"].([]any)
	prepareName := tasks[0].(map[string]any)["name"].(string)
	depends := tasks[2].(map[string]any)["depends"].(string)

	for _, terminal := range []string{"Succeeded", "Failed", "Errored", "Skipped", "Omitted"} {
		if !strings.Contains(depends, prepareName+"."+terminal) {
			t.Fatalf("explicit dependency = %q, missing %s", depends, terminal)
		}
	}

	if strings.Contains(depends, tasks[1].(map[string]any)["name"].(string)) {
		t.Fatalf("explicit dependency unexpectedly includes the previous phase barrier: %q", depends)
	}
}

func TestValidateSourcePhasesStageLimit(t *testing.T) {
	t.Parallel()

	phaseWithStages := func(count int) []signalv1.CertificationPhase {
		stages := make([]signalv1.CertificationStage, count)
		for index := range stages {
			stages[index] = signalv1.CertificationStage{
				Name: fmt.Sprintf("stage-%02d", index),
				Schedule: signalv1.StageSchedule{
					Mode:              signalv1.CheckExecutionModeRunOnce,
					InvocationTimeout: metav1.Duration{Duration: time.Minute},
				},
			}
		}

		return []signalv1.CertificationPhase{{
			Name:   "validation",
			Role:   signalv1.CheckStageRoleValidation,
			Stages: stages,
		}}
	}

	if err := validateSourcePhases(phaseWithStages(signalv1.MaxCertificationStages)); err != nil {
		t.Fatalf("maximum stage plan rejected: %v", err)
	}

	if err := validateSourcePhases(phaseWithStages(signalv1.MaxCertificationStages + 1)); err == nil {
		t.Fatal("over-limit stage plan was accepted")
	}
}

func TestValidateSourcePhasesDependencies(t *testing.T) {
	t.Parallel()

	stage := func(name string, dependencies ...string) signalv1.CertificationStage {
		result := signalv1.CertificationStage{
			Name: name,
			Schedule: signalv1.StageSchedule{
				Mode:              signalv1.CheckExecutionModeRunOnce,
				InvocationTimeout: metav1.Duration{Duration: time.Minute},
			},
		}
		for _, dependency := range dependencies {
			result.DependsOn = append(result.DependsOn, signalv1.StageDependency{Stage: dependency})
		}

		return result
	}

	t.Run("valid DAG", func(t *testing.T) {
		t.Parallel()

		phases := []signalv1.CertificationPhase{{
			Name:   "validation",
			Role:   signalv1.CheckStageRoleValidation,
			Stages: []signalv1.CertificationStage{stage("inventory"), stage("burn", "inventory")},
		}}
		if err := validateSourcePhases(phases); err != nil {
			t.Fatalf("validateSourcePhases() error = %v", err)
		}
	})

	t.Run("missing dependency", func(t *testing.T) {
		t.Parallel()

		phases := []signalv1.CertificationPhase{{
			Name:   "validation",
			Role:   signalv1.CheckStageRoleValidation,
			Stages: []signalv1.CertificationStage{stage("burn", "inventory")},
		}}
		if err := validateSourcePhases(phases); err == nil ||
			!strings.Contains(err.Error(), "does not exist") {
			t.Fatalf("validateSourcePhases() error = %v, want missing dependency", err)
		}
	})

	t.Run("cycle", func(t *testing.T) {
		t.Parallel()

		phases := []signalv1.CertificationPhase{{
			Name: "validation",
			Role: signalv1.CheckStageRoleValidation,
			Stages: []signalv1.CertificationStage{
				stage("inventory", "burn"),
				stage("burn", "inventory"),
			},
		}}
		if err := validateSourcePhases(phases); err == nil ||
			!strings.Contains(err.Error(), "cycle") {
			t.Fatalf("validateSourcePhases() error = %v, want cycle", err)
		}
	})
}

func TestValidateStageEvidenceSummaryBoundaries(t *testing.T) {
	t.Parallel()

	minExecutions := int32(6)
	stage := signalv1.ResolvedStagePlan{
		Name: "sustained",
		Test: signalv1.ResolvedCertificationTest{
			Identity: signalv1.TestIdentitySnapshot{
				Name:          "test",
				UID:           "test-uid",
				Generation:    1,
				ContentDigest: "sha256:" + strings.Repeat("a", 64),
				Trust:         signalv1.CertificationTestTrustApproved,
			},
			Metrics: []signalv1.TestMetricDefinition{{
				Name: "throughput",
				Type: signalv1.MetricValueTypeThroughput,
				Unit: "TFLOP/s",
			}},
		},
		Schedule: signalv1.StageSchedule{
			Mode:              signalv1.CheckExecutionModePeriodic,
			InvocationTimeout: metav1.Duration{Duration: time.Minute},
			CoverageWindow:    testDurationPointer(time.Hour),
			Interval:          testDurationPointer(10 * time.Minute),
			MinExecutions:     &minExecutions,
			MaxGap:            testDurationPointer(11 * time.Minute),
		},
		QualificationEligible: true,
	}
	started := metav1.NewTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	finished := metav1.NewTime(started.Add(time.Hour))
	throughput := "800"
	summary := signalv1.StageEvidenceSummary{
		PhaseOrder:               0,
		StageOrder:               0,
		Name:                     stage.Name,
		Role:                     signalv1.CheckStageRoleValidation,
		Test:                     stage.Test.Identity,
		Mode:                     stage.Schedule.Mode,
		QualificationEligible:    true,
		Verdict:                  signalv1.StageVerdictPassed,
		StartedAt:                started,
		FinishedAt:               finished,
		ConfiguredCoverageWindow: testDurationPointer(time.Hour),
		ObservedCoverageWindow:   metav1.Duration{Duration: time.Hour},
		InvocationCount:          minExecutions,
		SuccessfulInvocations:    minExecutions,
		FirstInvocationAt:        &started,
		LastInvocationAt:         &finished,
		MaxStartGap:              testDurationPointer(10 * time.Minute),
		Measurements: []signalv1.MetricMeasurement{{
			Name: "throughput",
			Value: signalv1.TypedValue{
				Type:         signalv1.TypedValueTypeDecimal,
				DecimalValue: &throughput,
			},
			Unit: "TFLOP/s",
		}},
		Participants: []signalv1.ParticipantEvidenceIdentity{{
			Index:   0,
			Role:    signalv1.ParticipantRoleRequester,
			Verdict: signalv1.QualificationVerdictPass,
			Start: signalv1.NodeFingerprint{
				NodeUID:            "node-uid",
				DriverVersion:      "580.1",
				GPUInventoryDigest: "sha256:" + strings.Repeat("8", 64),
			},
			End: signalv1.NodeFingerprint{
				NodeUID:            "node-uid",
				DriverVersion:      "580.1",
				GPUInventoryDigest: "sha256:" + strings.Repeat("8", 64),
			},
		}},
		DurableEvidenceDigest: "sha256:" + strings.Repeat("b", 64),
	}

	if err := validateStageEvidenceSummary(
		&summary,
		0,
		0,
		signalv1.CheckStageRoleValidation,
		&stage,
		1,
	); err != nil {
		t.Fatalf("valid boundary summary rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*signalv1.StageEvidenceSummary)
	}{
		{
			name: "short window",
			mutate: func(value *signalv1.StageEvidenceSummary) {
				value.ObservedCoverageWindow.Duration--
			},
		},
		{
			name: "missing invocation",
			mutate: func(value *signalv1.StageEvidenceSummary) {
				value.InvocationCount--
				value.SuccessfulInvocations--
			},
		},
		{
			name: "excessive gap",
			mutate: func(value *signalv1.StageEvidenceSummary) {
				value.MaxStartGap = testDurationPointer(12 * time.Minute)
			},
		},
		{
			name: "measurement schema mismatch",
			mutate: func(value *signalv1.StageEvidenceSummary) {
				value.Measurements = append([]signalv1.MetricMeasurement(nil), value.Measurements...)
				value.Measurements[0].Unit = "GB/s"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			value := summary
			tt.mutate(&value)

			if err := validateStageEvidenceSummary(
				&value,
				0,
				0,
				signalv1.CheckStageRoleValidation,
				&stage,
				1,
			); err == nil {
				t.Fatal("invalid stage evidence was accepted")
			}
		})
	}
}

func TestStageRunnerImageProvenanceRequiresPinnedPlannedImage(t *testing.T) {
	t.Parallel()

	image := pinnedImage("runner", "a")
	driver := signalv1.CertificationTestDriver{
		Type:      signalv1.TestDriverTypeContainer,
		Container: &signalv1.ContainerTestDriver{Image: image},
	}

	got, err := stageRunnerImageProvenance(`["`+image+`"]`, driver)
	if err != nil {
		t.Fatalf("stageRunnerImageProvenance() error = %v", err)
	}

	if len(got) != 1 || got[0] != image {
		t.Fatalf("runner images = %#v, want %q", got, image)
	}

	for _, value := range []string{
		`[]`,
		`["runner:latest"]`,
		`["` + pinnedImage("other", "b") + `"]`,
	} {
		if _, err := stageRunnerImageProvenance(value, driver); err == nil {
			t.Fatalf("stageRunnerImageProvenance(%s) unexpectedly succeeded", value)
		}
	}
}

func TestResolveStageParticipantRolesFreezesSelectedIndexes(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("add signal scheme: %v", err)
	}

	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}

	clientNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "client-node",
			UID:    "client-node-uid",
			Labels: map[string]string{"role": "client"},
		},
	}
	serverNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "server-node",
			UID:    "server-node-uid",
			Labels: map[string]string{"role": "server"},
		},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(clientNode, serverNode).Build()
	reconciler := &AdmissionReconciler{Client: kube, APIReader: kube}
	profile := &signalv1.CertificationProfile{
		Spec: signalv1.CertificationProfileSpec{
			ParticipantRoles: []signalv1.ProfileParticipantRole{
				{
					Role:     "client",
					Count:    1,
					Selector: metav1.LabelSelector{MatchLabels: map[string]string{"role": "client"}},
				},
				{
					Role:     "server",
					Count:    1,
					Selector: metav1.LabelSelector{MatchLabels: map[string]string{"role": "server"}},
				},
			},
		},
	}
	participants := []signalv1.Participant{
		{NodeName: clientNode.Name, NodeUID: clientNode.UID},
		{NodeName: serverNode.Name, NodeUID: serverNode.UID},
	}
	topology := signalv1.TestTopology{
		MinParticipants: 2,
		MaxParticipants: 2,
		Roles: []signalv1.TestParticipantRole{
			{Name: "client", MinCount: 1, MaxCount: 1},
			{Name: "server", MinCount: 1, MaxCount: 1},
		},
	}
	bindings := []signalv1.StageParticipantRoleBinding{
		{Role: "client", Count: 1},
		{Role: "server", Count: 1},
	}

	got, err := reconciler.resolveStageParticipantRoles(
		context.Background(),
		profile,
		participants,
		topology,
		bindings,
	)
	if err != nil {
		t.Fatalf("resolveStageParticipantRoles() error = %v", err)
	}

	if len(got) != 2 ||
		!slices.Equal(got[0].ParticipantIndexes, []int32{0}) ||
		!slices.Equal(got[1].ParticipantIndexes, []int32{1}) {
		t.Fatalf("resolved role bindings = %#v", got)
	}
}

func TestResolvedTerminalEvidencePreservesFailureAndDiagnosticIdentity(t *testing.T) {
	t.Parallel()

	node := qualificationNode("node", "node-uid", "boot-a")
	scheme := qualificationScheme(t)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	reconciler := &RunReconciler{
		Client:          kube,
		APIReader:       kube,
		ControllerImage: pinnedImage("controller", "a"),
	}

	for _, tt := range []struct {
		name            string
		stageVerdict    signalv1.StageVerdict
		runVerdict      string
		reason          string
		diagnostic      bool
		wantParticipant signalv1.QualificationVerdict
		wantCredit      bool
	}{
		{
			name:            "failed assertion",
			stageVerdict:    signalv1.StageVerdictFailed,
			runVerdict:      "Fail",
			reason:          "RequiredStageFailed: assertion throughput failed",
			wantParticipant: signalv1.QualificationVerdictFail,
		},
		{
			name:            "diagnostic pass",
			stageVerdict:    signalv1.StageVerdictPassed,
			runVerdict:      "Pass",
			reason:          "AllRequiredStagesPassed",
			diagnostic:      true,
			wantParticipant: signalv1.QualificationVerdictPass,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			run, summary, runnerImage := resolvedTerminalTestRun(t, node, tt.stageVerdict, tt.diagnostic)

			stage := &run.Spec.ResolvedPlan.Phases[0].Stages[0]
			if err := validateStageEvidenceSummary(
				&summary,
				0,
				0,
				signalv1.CheckStageRoleValidation,
				stage,
				1,
			); err != nil {
				t.Fatalf("validateStageEvidenceSummary() error = %v", err)
			}

			if stage.Assertions[0].Expected == nil ||
				stage.Assertions[0].Expected.DecimalValue == nil ||
				*stage.Assertions[0].Expected.DecimalValue != "25.0" {
				t.Fatalf("immutable assertion threshold = %#v", stage.Assertions[0])
			}

			if tt.stageVerdict == signalv1.StageVerdictFailed &&
				(summary.Assertions[0].Actual != nil ||
					summary.Assertions[0].Passed ||
					summary.Assertions[0].Message == "") {
				t.Fatalf("missing-metric assertion evidence = %#v", summary.Assertions[0])
			}

			identity, verdict, reason, err := reconciler.resolvedQualificationEvidenceIdentity(
				context.Background(),
				run,
				[]signalv1.StageEvidenceSummary{summary},
				[]string{runnerImage},
				tt.runVerdict,
				tt.reason,
			)
			if err != nil {
				t.Fatalf("resolvedQualificationEvidenceIdentity() error = %v", err)
			}

			if identity == nil || verdict != tt.runVerdict || reason != tt.reason {
				t.Fatalf("terminal identity = %#v, verdict %q, reason %q", identity, verdict, reason)
			}

			if identity.Participants[0].Verdict != tt.wantParticipant ||
				len(identity.Stages) != 1 ||
				identity.Stages[0].Verdict != tt.stageVerdict {
				t.Fatalf("terminal identity = %#v", identity)
			}

			if got := qualificationCreditEligible(run.Spec.ResolvedPlan, identity); got != tt.wantCredit {
				t.Fatalf("qualificationCreditEligible() = %t, want %t", got, tt.wantCredit)
			}

			run.Status.Evidence.Identity = nil
			startedAt := summary.StartedAt
			finishedAt := summary.FinishedAt

			document, err := reconciler.evidenceDocument(
				context.Background(),
				run,
				[]signalv1.StageEvidenceSummary{summary},
				"Completed",
				tt.runVerdict,
				tt.reason,
				startedAt,
				finishedAt,
			)
			if err != nil {
				t.Fatalf("evidenceDocument() error = %v", err)
			}

			var decoded struct {
				StageSummaries []signalv1.StageEvidenceSummary `json:"stageSummaries"`
			}
			if err := json.Unmarshal(document, &decoded); err != nil {
				t.Fatalf("decode evidence document: %v", err)
			}

			if len(decoded.StageSummaries) != 1 ||
				decoded.StageSummaries[0].Verdict != tt.stageVerdict {
				t.Fatalf("durable stage summaries = %#v", decoded.StageSummaries)
			}
		})
	}
}

func TestResolvedQualificationEvidenceHandlesNonPassingStages(t *testing.T) {
	t.Parallel()

	node := qualificationNode("node", "node-uid", "boot-a")
	scheme := qualificationScheme(t)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	reconciler := &RunReconciler{
		Client:          kube,
		APIReader:       kube,
		ControllerImage: pinnedImage("controller", "a"),
	}

	t.Run("required failure preserves failure without success fingerprint", func(t *testing.T) {
		run, summary, runnerImage := resolvedTerminalTestRun(
			t,
			node,
			signalv1.StageVerdictFailed,
			false,
		)
		summary.Participants[0].Start.DriverVersion = ""
		summary.Participants[0].End.DriverVersion = ""
		summary.Participants[0].Start.GPUInventoryDigest = ""
		summary.Participants[0].End.GPUInventoryDigest = ""

		identity, verdict, _, err := reconciler.resolvedQualificationEvidenceIdentity(
			context.Background(),
			run,
			[]signalv1.StageEvidenceSummary{summary},
			[]string{runnerImage},
			"Fail",
			"RequiredStageFailed",
		)
		if err != nil {
			t.Fatal(err)
		}

		if identity == nil ||
			verdict != "Fail" ||
			identity.Participants[0].Verdict != signalv1.QualificationVerdictFail {
			t.Fatalf("identity = %#v, verdict = %q", identity, verdict)
		}
	})

	t.Run("optional failure does not poison participant verdict", func(t *testing.T) {
		run, summary, runnerImage := resolvedTerminalTestRun(
			t,
			node,
			signalv1.StageVerdictFailed,
			false,
		)
		run.Spec.ResolvedPlan.Phases[0].Stages[0].Optional = true

		digest, err := signalv1.DeriveResolvedExecutionPlanDigest(
			int32(len(run.Spec.Participants)),
			run.Spec.Environment,
			*run.Spec.ResolvedPlan,
			*run.Spec.PlanProvenance,
		)
		if err != nil {
			t.Fatal(err)
		}

		run.Spec.PlanDigest = digest
		summary.Participants[0].Start.DriverVersion = ""
		summary.Participants[0].End.DriverVersion = ""
		summary.Participants[0].Start.GPUInventoryDigest = ""
		summary.Participants[0].End.GPUInventoryDigest = ""

		identity, verdict, reason, err := reconciler.resolvedQualificationEvidenceIdentity(
			context.Background(),
			run,
			[]signalv1.StageEvidenceSummary{summary},
			[]string{runnerImage},
			"Pass",
			"AllRequiredStagesPassed",
		)
		if err != nil {
			t.Fatal(err)
		}

		if identity == nil ||
			verdict != "Pass" ||
			reason != "AllRequiredStagesPassed" ||
			identity.Participants[0].Verdict != signalv1.QualificationVerdictPass {
			t.Fatalf("identity = %#v, verdict = %q, reason = %q", identity, verdict, reason)
		}
	})
}

func TestValidateStageEvidenceSummaryRejectsOmittedFailedAssertion(t *testing.T) {
	t.Parallel()

	node := qualificationNode("node", "node-uid", "boot-a")
	run, summary, _ := resolvedTerminalTestRun(
		t,
		node,
		signalv1.StageVerdictFailed,
		false,
	)
	summary.Assertions = nil

	if err := validateStageEvidenceSummary(
		&summary,
		0,
		0,
		signalv1.CheckStageRoleValidation,
		&run.Spec.ResolvedPlan.Phases[0].Stages[0],
		1,
	); err == nil {
		t.Fatal("failed stage evidence without its configured assertion was accepted")
	}
}

func TestResolvedWorkflowFailurePreservesStageFailure(t *testing.T) {
	t.Parallel()

	workflow := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{"message": "required assertion failed"},
	}}

	verdict, reason := resolvedWorkflowVerdict(
		workflow,
		"Failed",
		"Fail",
		"RequiredStageFailed: assertion throughput failed",
	)
	if verdict != "Fail" || reason != "RequiredStageFailed: assertion throughput failed" {
		t.Fatalf("resolved Workflow verdict = %q, reason = %q", verdict, reason)
	}

	verdict, reason = resolvedWorkflowVerdict(workflow, "Failed", "Pass", "AllRequiredStagesPassed")
	if verdict != "Inconclusive" ||
		reason != "WorkflowFailed: required assertion failed" {
		t.Fatalf("failed Workflow without stage failure = %q, reason = %q", verdict, reason)
	}
}

func TestResolvedRunCleanupReleasesAndDeletesWithoutScriptBundle(t *testing.T) {
	t.Parallel()

	scheme := resolvedCleanupScheme(t)
	finishedAt := time.Date(2026, 11, 5, 18, 30, 0, 0, time.UTC)
	node := qualificationNode("node", "node-uid", "boot-a")
	node.Spec.Unschedulable = true
	node.Labels = map[string]string{}
	node.Labels[RequestLabel] = "profile"
	node.Labels[signalv1.ReservationNodeUIDLabel] = string(node.UID)
	node.Annotations = map[string]string{ReservationLabel: "resolved-run"}

	run := &signalv1.CertificationRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "resolved-run",
			Namespace:  "signal-system",
			UID:        "resolved-run-uid",
			Finalizers: []string{RunFinalizer},
		},
		Spec: signalv1.CertificationRunSpec{
			Profile: signalv1.PinnedProfile{Name: "profile"},
			Participants: []signalv1.Participant{{
				NodeName: node.Name,
				NodeUID:  node.UID,
			}},
			ResolvedPlan: &signalv1.ResolvedCertificationPlan{
				Phases: []signalv1.ResolvedPhasePlan{{Name: "validation"}},
			},
		},
		Status: signalv1.CertificationRunStatus{
			Phase: signalv1.RunPhaseSucceeded,
			Evidence: signalv1.EvidenceStatus{
				URI:    "kubernetes://signal-system/resolved-run-evidence/evidence.json.gz",
				Digest: "sha256:" + strings.Repeat("a", 64),
			},
		},
	}
	evidence := []byte(fmt.Sprintf(
		`{"runID":%q,"runName":%q,"finishedAt":%q}`,
		run.UID,
		run.Name,
		finishedAt.Format(time.RFC3339Nano),
	))
	store := staticEvidenceStore{
		data:   evidence,
		uri:    run.Status.Evidence.URI,
		digest: run.Status.Evidence.Digest,
	}
	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&signalv1.CertificationRun{}).
		WithObjects(node, run).
		Build()
	reconciler := &RunReconciler{Client: kube, APIReader: kube, Evidence: store}

	var current signalv1.CertificationRun
	if err := kube.Get(
		context.Background(),
		client.ObjectKey{Namespace: run.Namespace, Name: run.Name},
		&current,
	); err != nil {
		t.Fatalf("get Run before cleanup: %v", err)
	}

	if _, err := reconciler.cleanup(context.Background(), &current); err != nil {
		t.Fatalf("cleanup() error = %v", err)
	}

	if err := kube.Get(
		context.Background(),
		client.ObjectKey{Namespace: run.Namespace, Name: run.Name},
		&current,
	); err != nil {
		t.Fatalf("get Run after cleanup: %v", err)
	}

	if slices.Contains(current.Finalizers, RunFinalizer) {
		t.Fatalf("Run finalizers = %#v", current.Finalizers)
	}

	var released corev1.Node
	if err := kube.Get(context.Background(), client.ObjectKey{Name: node.Name}, &released); err != nil {
		t.Fatalf("get released Node: %v", err)
	}

	if released.Spec.Unschedulable ||
		released.Labels[RequestLabel] != "" ||
		released.Labels[signalv1.ReservationNodeUIDLabel] != "" ||
		released.Annotations[ReservationLabel] != "" {
		t.Fatalf("Node was not fully released: %#v", released)
	}

	if err := kube.Delete(context.Background(), &current); err != nil {
		t.Fatalf("delete cleaned Run: %v", err)
	}

	if err := kube.Get(
		context.Background(),
		client.ObjectKey{Namespace: run.Namespace, Name: run.Name},
		&current,
	); !apierrors.IsNotFound(err) {
		t.Fatalf("deleted Run lookup error = %v, want NotFound", err)
	}
}

func TestTerminalRunRecoversDurableEvidenceBeforeCleanup(t *testing.T) {
	t.Parallel()

	scheme := resolvedCleanupScheme(t)
	finishedAt := time.Date(2026, 11, 13, 0, 9, 53, 0, time.UTC)
	run := &signalv1.CertificationRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "terminal-run",
			Namespace:  "signal-system",
			UID:        "terminal-run-uid",
			Finalizers: []string{RunFinalizer},
		},
		Status: signalv1.CertificationRunStatus{
			Phase: signalv1.RunPhaseCancelled,
		},
	}
	evidence := []byte(fmt.Sprintf(
		`{"runID":%q,"runName":%q,"phase":"Canceled","verdict":"Inconclusive","reason":"Canceled","finishedAt":%q,"checkStatus":[]}`,
		run.UID,
		run.Name,
		finishedAt.Format(time.RFC3339Nano),
	))
	store := staticEvidenceStore{
		data:   evidence,
		uri:    "kubernetes://signal-system/terminal-run-evidence/evidence.json.gz",
		digest: "sha256:" + strings.Repeat("a", 64),
	}
	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&signalv1.CertificationRun{}).
		WithObjects(run).
		Build()
	reconciler := &RunReconciler{Client: kube, APIReader: kube, Evidence: store}

	if _, err := reconciler.Reconcile(
		context.Background(),
		ctrl.Request{NamespacedName: client.ObjectKey{
			Namespace: run.Namespace,
			Name:      run.Name,
		}},
	); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	var current signalv1.CertificationRun
	if err := kube.Get(
		context.Background(),
		client.ObjectKey{Namespace: run.Namespace, Name: run.Name},
		&current,
	); err != nil {
		t.Fatalf("get recovered Run: %v", err)
	}

	if current.Status.Evidence.URI != store.uri ||
		current.Status.Evidence.Digest != store.digest {
		t.Fatalf("recovered evidence = %#v", current.Status.Evidence)
	}

	if !slices.Contains(current.Finalizers, RunFinalizer) {
		t.Fatalf("Run finalizers = %#v, want cleanup deferred", current.Finalizers)
	}
}

func TestTerminalRunWithExistingIdentityCleansUpFromImmutableEvidence(t *testing.T) {
	t.Parallel()

	scheme := resolvedCleanupScheme(t)
	finishedAt := time.Date(2026, 11, 13, 0, 9, 53, 0, time.UTC)
	node := qualificationNode("node", "node-uid", "boot-a")
	run := &signalv1.CertificationRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "partially-recovered-run",
			Namespace:  "signal-system",
			UID:        "partially-recovered-run-uid",
			Finalizers: []string{RunFinalizer},
		},
		Spec: signalv1.CertificationRunSpec{
			Release: true,
			Participants: []signalv1.Participant{{
				NodeName: node.Name,
				NodeUID:  node.UID,
			}},
		},
		Status: signalv1.CertificationRunStatus{
			Phase: signalv1.RunPhaseCancelled,
			Evidence: signalv1.EvidenceStatus{
				Identity: &signalv1.QualificationEvidenceIdentity{
					PlanDigest: "sha256:" + strings.Repeat("b", 64),
					Participants: []signalv1.ParticipantEvidenceIdentity{{
						Index:   0,
						Role:    signalv1.ParticipantRoleRequester,
						Verdict: signalv1.QualificationVerdictInconclusive,
					}},
					Stages: []signalv1.StageEvidenceSummary{{
						Name:       "partial-stage",
						PhaseOrder: 0,
						StageOrder: 0,
						Role:       signalv1.CheckStageRoleValidation,
						Mode:       signalv1.CheckExecutionModeRunOnce,
						Verdict:    signalv1.StageVerdictPassed,
					}},
				},
			},
		},
	}
	evidence := []byte(fmt.Sprintf(
		`{"runID":%q,"runName":%q,"finishedAt":%q}`,
		run.UID,
		run.Name,
		finishedAt.Format(time.RFC3339Nano),
	))
	store := staticEvidenceStore{
		data:   evidence,
		uri:    "kubernetes://signal-system/partially-recovered-run-evidence/evidence.json.gz",
		digest: "sha256:" + strings.Repeat("a", 64),
	}
	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&signalv1.CertificationRun{}).
		WithObjects(node, run).
		Build()
	reconciler := &RunReconciler{Client: kube, APIReader: kube, Evidence: store}

	if _, err := reconciler.Reconcile(
		context.Background(),
		ctrl.Request{NamespacedName: client.ObjectKey{
			Namespace: run.Namespace,
			Name:      run.Name,
		}},
	); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	var current signalv1.CertificationRun
	if err := kube.Get(
		context.Background(),
		client.ObjectKey{Namespace: run.Namespace, Name: run.Name},
		&current,
	); err != nil {
		t.Fatalf("get cleaned Run: %v", err)
	}

	if slices.Contains(current.Finalizers, RunFinalizer) {
		t.Fatalf("Run finalizers = %#v", current.Finalizers)
	}

	if current.Status.Evidence.URI != "" ||
		current.Status.Evidence.Digest != "" ||
		current.Status.Evidence.Identity == nil {
		t.Fatalf("partial evidence status was rewritten: %#v", current.Status.Evidence)
	}
}

func TestResolvedCleanupWaitsForAndFencesChildWorkflowsAndPods(t *testing.T) {
	t.Parallel()

	scheme := resolvedCleanupScheme(t)
	run := &signalv1.CertificationRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "resolved-run",
			Namespace: "signal-system",
			UID:       "resolved-run-uid",
		},
		Spec: signalv1.CertificationRunSpec{
			ResolvedPlan: &signalv1.ResolvedCertificationPlan{},
		},
	}
	root := cleanupWorkflow("root", run.Namespace, "root-uid", run.UID, nil)
	childOwner := metav1.OwnerReference{UID: root.GetUID(), Name: root.GetName(), Kind: "Workflow"}
	child := cleanupWorkflow("child", run.Namespace, "child-uid", run.UID, []metav1.OwnerReference{childOwner})
	podOwner := metav1.OwnerReference{UID: child.GetUID(), Name: child.GetName(), Kind: "Workflow"}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "child-pod",
			Namespace:       run.Namespace,
			UID:             "child-pod-uid",
			Labels:          map[string]string{RunUIDLabel: string(run.UID)},
			OwnerReferences: []metav1.OwnerReference{podOwner},
		},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(child, pod).Build()
	reconciler := &RunReconciler{Client: kube, APIReader: kube}

	if err := reconciler.fenceWorkflowDependents(
		context.Background(),
		kube,
		root,
		run,
	); err != nil {
		t.Fatalf("fenceWorkflowDependents() error = %v", err)
	}

	var fencedChild unstructured.Unstructured
	fencedChild.SetGroupVersionKind(workflowGVK)

	if err := kube.Get(
		context.Background(),
		client.ObjectKey{Namespace: child.GetNamespace(), Name: child.GetName()},
		&fencedChild,
	); err != nil {
		t.Fatalf("get fenced child Workflow: %v", err)
	}

	if len(fencedChild.GetOwnerReferences()) != 1 ||
		fencedChild.GetOwnerReferences()[0].BlockOwnerDeletion == nil ||
		!*fencedChild.GetOwnerReferences()[0].BlockOwnerDeletion {
		t.Fatalf("child Workflow owner fence = %#v", fencedChild.GetOwnerReferences())
	}

	var fencedPod corev1.Pod
	if err := kube.Get(
		context.Background(),
		client.ObjectKey{Namespace: pod.Namespace, Name: pod.Name},
		&fencedPod,
	); err != nil {
		t.Fatalf("get fenced Pod: %v", err)
	}

	if len(fencedPod.OwnerReferences) != 1 ||
		fencedPod.OwnerReferences[0].BlockOwnerDeletion == nil ||
		!*fencedPod.OwnerReferences[0].BlockOwnerDeletion {
		t.Fatalf("child Pod owner fence = %#v", fencedPod.OwnerReferences)
	}

	gone, err := reconciler.runWorkloadsGone(context.Background(), kube, run)
	if err != nil || gone {
		t.Fatalf("runWorkloadsGone() = %t, %v with children present", gone, err)
	}

	if err := kube.Delete(context.Background(), &fencedChild); err != nil {
		t.Fatalf("delete child Workflow: %v", err)
	}

	gone, err = reconciler.runWorkloadsGone(context.Background(), kube, run)
	if err != nil || gone {
		t.Fatalf("runWorkloadsGone() = %t, %v with child Pod present", gone, err)
	}

	if err := kube.Delete(context.Background(), &fencedPod); err != nil {
		t.Fatalf("delete child Pod: %v", err)
	}

	gone, err = reconciler.runWorkloadsGone(context.Background(), kube, run)
	if err != nil || !gone {
		t.Fatalf("runWorkloadsGone() = %t, %v after child deletion", gone, err)
	}
}

func TestResolvePhasePlanEnforcesCertificationTestTrust(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name         string
		trust        signalv1.CertificationTestTrustState
		mutableInput bool
		largeInputs  bool
		wantError    bool
		wantQualify  bool
	}{
		{name: "approved", trust: signalv1.CertificationTestTrustApproved, wantQualify: true},
		{name: "approved incomplete input", trust: signalv1.CertificationTestTrustApproved, mutableInput: true},
		{name: "oversized execution inputs", trust: signalv1.CertificationTestTrustApproved, largeInputs: true, wantError: true},
		{name: "diagnostic", trust: signalv1.CertificationTestTrustDiagnostic},
		{name: "rejected", trust: signalv1.CertificationTestTrustRejected, wantError: true},
		{name: "revoked", trust: signalv1.CertificationTestTrustRevoked, wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			scheme := runtime.NewScheme()
			if err := AddToScheme(scheme); err != nil {
				t.Fatalf("add signal scheme: %v", err)
			}

			contentDigest := "sha256:" + strings.Repeat("a", 64)

			test := &signalv1.CertificationTest{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "generic-test",
					UID:        "generic-test-uid",
					Generation: 3,
				},
				Spec: signalv1.CertificationTestSpec{
					DisplayName: "Generic test",
					Owner:       "project-signal",
					Version:     "1",
					Metrics: []signalv1.TestMetricDefinition{{
						Name:        "completed",
						DisplayName: "Completed",
						Type:        signalv1.MetricValueTypeBoolean,
						Required:    true,
					}},
					Topology: signalv1.TestTopology{MinParticipants: 1, MaxParticipants: 1},
					Driver: signalv1.CertificationTestDriver{
						Type: signalv1.TestDriverTypeContainer,
						Container: &signalv1.ContainerTestDriver{
							Image: pinnedImage("test", "a"),
						},
					},
				},
				Status: signalv1.CertificationTestStatus{
					ObservedGeneration: 3,
					Trust: &signalv1.CertificationTestTrustStatus{
						State:         tt.trust,
						ContentDigest: contentDigest,
					},
				},
			}
			if tt.largeInputs {
				for _, name := range []string{"first", "second"} {
					test.Spec.Parameters = append(test.Spec.Parameters, signalv1.TestParameterDefinition{
						Name:        name,
						DisplayName: name,
						Type:        signalv1.TypedValueTypeString,
						Required:    true,
					})
				}
			}

			kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(test).Build()
			reconciler := &AdmissionReconciler{Client: kube, APIReader: kube}

			profile := &signalv1.CertificationProfile{
				Spec: signalv1.CertificationProfileSpec{
					Participants: signalv1.ParticipantPolicy{Count: 1},
					Environment: &signalv1.CertificationEnvironmentBindings{
						Namespace: "signal-system",
					},
				},
			}
			if tt.mutableInput {
				profile.Spec.Environment.Resolvers = []signalv1.NamedEnvironmentReference{{
					Name:      "model-resolver",
					Reference: "resolver:mutable",
				}}
			}

			suite := &signalv1.CertificationSuite{
				Spec: signalv1.CertificationSuiteSpec{
					Phases: []signalv1.CertificationPhase{{
						Name: "validation",
						Role: signalv1.CheckStageRoleValidation,
						Stages: []signalv1.CertificationStage{{
							Name: "generic",
							Test: signalv1.CertificationTestReference{
								Name:          test.Name,
								UID:           test.UID,
								Generation:    test.Generation,
								ContentDigest: contentDigest,
							},
							Schedule: signalv1.StageSchedule{
								Mode:              signalv1.CheckExecutionModeRunOnce,
								InvocationTimeout: metav1.Duration{Duration: time.Minute},
							},
						}},
					}},
				},
			}

			if tt.largeInputs {
				for _, name := range []string{"first", "second"} {
					value := strings.Repeat("x", 60*1024)
					suite.Spec.Phases[0].Stages[0].Parameters = append(
						suite.Spec.Phases[0].Stages[0].Parameters,
						signalv1.StageParameterBinding{
							Name: name,
							Value: signalv1.TypedValue{
								Type:        signalv1.TypedValueTypeString,
								StringValue: &value,
							},
						},
					)
				}
			}

			participants := []signalv1.Participant{{
				NodeName: "node",
				NodeUID:  "node-uid",
			}}

			plan, err := reconciler.resolvePhasePlan(
				context.Background(),
				profile,
				suite,
				participants,
			)
			if tt.wantError {
				if err == nil {
					t.Fatal("resolvePhasePlan() unexpectedly accepted blocked trust state")
				}

				return
			}

			if err != nil {
				t.Fatalf("resolvePhasePlan() error = %v", err)
			}

			stage := plan.Phases[0].Stages[0]
			if stage.QualificationEligible != tt.wantQualify ||
				stage.DiagnosticOnly == tt.wantQualify {
				t.Fatalf(
					"resolved eligibility = qualify %t, diagnostic %t",
					stage.QualificationEligible,
					stage.DiagnosticOnly,
				)
			}
		})
	}
}

func TestResolveWorkflowTemplateInputPinsLiveSemanticIdentity(t *testing.T) {
	t.Parallel()

	template := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "WorkflowTemplate",
		"metadata": map[string]any{
			"name":            "vllm-canary",
			"namespace":       "signal-system",
			"resourceVersion": "42",
		},
		"spec": map[string]any{
			"entrypoint": "canary",
			"templates": []any{
				map[string]any{"name": "canary", "container": map[string]any{"image": pinnedImage("vllm", "a")}},
			},
		},
	}}
	template.SetGroupVersionKind(workflowTemplateGVK)

	digest, err := workflowTemplateSemanticDigest(template)
	if err != nil {
		t.Fatalf("workflowTemplateSemanticDigest() error = %v", err)
	}

	kube := fake.NewClientBuilder().WithObjects(template).Build()
	reconciler := &AdmissionReconciler{
		Client:    kube,
		APIReader: kube,
		Namespace: "signal-system",
	}
	driver := &signalv1.WorkflowTemplateTestDriver{Name: "vllm-canary", Digest: digest}

	input, err := reconciler.resolveWorkflowTemplateInput(context.Background(), driver)
	if err != nil {
		t.Fatalf("resolveWorkflowTemplateInput() error = %v", err)
	}

	if input.Kind != signalv1.ResolvedInputKindWorkflowTemplate ||
		input.State != signalv1.ResolvedInputStateComplete ||
		input.Resolved != "vllm-canary@42" ||
		input.Digest != digest {
		t.Fatalf("resolved WorkflowTemplate input = %#v", input)
	}

	driver.Digest = "sha256:" + strings.Repeat("f", 64)
	if _, err := reconciler.resolveWorkflowTemplateInput(context.Background(), driver); err == nil {
		t.Fatal("resolveWorkflowTemplateInput() accepted a semantic digest mismatch")
	}
}

func resolvedWorkflowTestRun() *signalv1.CertificationRun {
	runOnce := signalv1.StageSchedule{
		Mode:              signalv1.CheckExecutionModeRunOnce,
		InvocationTimeout: metav1.Duration{Duration: 2 * time.Minute},
	}
	stage := func(name string, optional bool) signalv1.ResolvedStagePlan {
		return signalv1.ResolvedStagePlan{
			Name:     name,
			Optional: optional,
			Schedule: runOnce,
		}
	}

	return &signalv1.CertificationRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "resolved-run",
			Namespace: "signal-system",
			UID:       "resolved-run-uid",
		},
		Spec: signalv1.CertificationRunSpec{
			ResolvedPlan: &signalv1.ResolvedCertificationPlan{
				Phases: []signalv1.ResolvedPhasePlan{
					{
						Name: "startup",
						Role: signalv1.CheckStageRoleStartup,
						Stages: []signalv1.ResolvedStagePlan{
							stage("prepare", false),
							stage("optional-observer", true),
						},
					},
					{
						Name:   "validation",
						Role:   signalv1.CheckStageRoleValidation,
						Stages: []signalv1.ResolvedStagePlan{stage("validate", false)},
					},
				},
			},
		},
	}
}

func TestRunningRunStatusesTracksResolvedStages(t *testing.T) {
	t.Parallel()

	run := resolvedWorkflowTestRun()
	workflow := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{
			"nodes": map[string]any{},
		},
	}}
	workflow.SetName(run.Name + "-a1")

	nodes := workflow.Object["status"].(map[string]any)["nodes"].(map[string]any)
	nodes[workflow.GetName()+"."+stageTaskName(0, 0, "prepare")] = map[string]any{
		"phase": "Succeeded",
	}
	nodes[workflow.GetName()+"."+stageTaskName(0, 1, "optional-observer")] = map[string]any{
		"phase": "Running",
	}

	statuses := runningRunStatuses(workflow, run)

	want := []signalv1.CheckPhase{
		signalv1.CheckPhaseSucceeded,
		signalv1.CheckPhaseRunning,
		signalv1.CheckPhasePending,
	}
	if len(statuses) != len(want) {
		t.Fatalf("status count = %d, want %d", len(statuses), len(want))
	}

	for index := range want {
		if statuses[index].Phase != want[index] {
			t.Fatalf("status %d phase = %q, want %q", index, statuses[index].Phase, want[index])
		}
	}
}

func TestCanceledRunStatusesPreservesCompletedResolvedStages(t *testing.T) {
	t.Parallel()

	run := resolvedWorkflowTestRun()
	workflow := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{
			"nodes": map[string]any{},
		},
	}}
	workflow.SetName(run.Name + "-a1")

	nodes := workflow.Object["status"].(map[string]any)["nodes"].(map[string]any)
	nodes[workflow.GetName()+"."+stageTaskName(0, 0, "prepare")] = map[string]any{
		"phase": "Succeeded",
	}
	nodes[workflow.GetName()+"."+stageTaskName(0, 1, "optional-observer")] = map[string]any{
		"phase": "Running",
	}

	statuses := canceledRunStatuses(workflow, run)
	if statuses[0].Phase != signalv1.CheckPhaseSucceeded {
		t.Fatalf("completed stage phase = %q, want %q", statuses[0].Phase, signalv1.CheckPhaseSucceeded)
	}

	if statuses[1].Phase != signalv1.CheckPhaseInconclusive ||
		statuses[1].Reason != "Canceled" {
		t.Fatalf("active canceled stage = %#v", statuses[1])
	}

	if statuses[2].Phase != signalv1.CheckPhaseSkipped ||
		statuses[2].Reason != "Canceled" {
		t.Fatalf("pending canceled stage = %#v", statuses[2])
	}
}

func TestTerminalResolvedRunStatusesUsesStageEvidence(t *testing.T) {
	t.Parallel()

	run := resolvedWorkflowTestRun()
	workflow := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{
			"nodes": map[string]any{},
		},
	}}
	workflow.SetName(run.Name + "-a1")

	nodes := workflow.Object["status"].(map[string]any)["nodes"].(map[string]any)
	nodes[workflow.GetName()+"."+stageTaskName(0, 0, "prepare")] = map[string]any{
		"phase": "Running",
	}
	nodes[workflow.GetName()+"."+stageTaskName(0, 1, "optional-observer")] = map[string]any{
		"phase": "Running",
	}

	statuses := terminalResolvedRunStatuses(
		workflow,
		run,
		[]signalv1.StageEvidenceSummary{
			{PhaseOrder: 0, StageOrder: 0, Verdict: signalv1.StageVerdictPassed},
			{PhaseOrder: 0, StageOrder: 1, Verdict: signalv1.StageVerdictFailed},
			{PhaseOrder: 99, StageOrder: 99, Verdict: signalv1.StageVerdictPassed},
		},
	)

	want := []signalv1.CheckPhase{
		signalv1.CheckPhaseSucceeded,
		signalv1.CheckPhaseFailed,
		signalv1.CheckPhasePending,
	}
	for index := range want {
		if statuses[index].Phase != want[index] {
			t.Fatalf("status %d phase = %q, want %q", index, statuses[index].Phase, want[index])
		}
	}
}

func testDurationPointer(value time.Duration) *metav1.Duration {
	return &metav1.Duration{Duration: value}
}

func TestCompletedStageSummariesReadsImmutableResultBeforeWorkflowTermination(t *testing.T) {
	t.Parallel()

	node := qualificationNode("node", "node-uid", "boot-a")
	run, summary, runnerImage := resolvedTerminalTestRun(
		t,
		node,
		signalv1.StageVerdictPassed,
		false,
	)
	run.Namespace = "signal-system"

	throughput := "30.0"
	summary.Measurements = []signalv1.MetricMeasurement{{
		Name: "throughput",
		Value: signalv1.TypedValue{
			Type:         signalv1.TypedValueTypeDecimal,
			DecimalValue: &throughput,
		},
	}}

	content, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}

	immutable := true
	controller := true
	workflow := &unstructured.Unstructured{}
	workflow.SetGroupVersionKind(workflowGVK)
	workflow.SetName("active-workflow")
	workflow.SetNamespace(run.Namespace)
	workflow.SetUID("workflow-uid")

	result := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      stageResultName(run.Name, 0, 0),
			Namespace: run.Namespace,
			Labels: map[string]string{
				SnapshotRunLabel: run.Name,
			},
			Annotations: map[string]string{
				stageResultRunUID:       string(run.UID),
				stageResultPlanDigest:   run.Spec.PlanDigest,
				stageResultPhaseOrder:   "0",
				stageResultStageOrder:   "0",
				stageResultDigest:       digestBytes(content),
				stageResultRunnerImages: `["` + runnerImage + `"]`,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: workflow.GetAPIVersion(),
				Kind:       workflow.GetKind(),
				Name:       workflow.GetName(),
				UID:        workflow.GetUID(),
				Controller: &controller,
			}},
		},
		Immutable:  &immutable,
		BinaryData: map[string][]byte{checkResultDataKey: content},
	}

	stage := run.Spec.ResolvedPlan.Phases[0].Stages[0]
	test := &signalv1.CertificationTest{
		ObjectMeta: metav1.ObjectMeta{
			Name:       stage.Test.Identity.Name,
			UID:        stage.Test.Identity.UID,
			Generation: stage.Test.Identity.Generation,
		},
		Spec: signalv1.CertificationTestSpec{
			Metrics:  stage.Test.Metrics,
			Topology: stage.Test.Topology,
			Driver:   stage.Test.Driver,
		},
		Status: signalv1.CertificationTestStatus{
			ObservedGeneration: stage.Test.Identity.Generation,
			Trust: &signalv1.CertificationTestTrustStatus{
				State:         stage.Test.Identity.Trust,
				ContentDigest: stage.Test.Identity.ContentDigest,
			},
		},
	}

	scheme := qualificationScheme(t)
	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node, result, test).
		Build()
	reconciler := &RunReconciler{Client: kube, APIReader: kube}

	stages, images, err := reconciler.completedStageSummaries(
		t.Context(),
		workflow,
		run,
	)
	if err != nil {
		t.Fatalf("completedStageSummaries() error = %v", err)
	}

	if len(stages) != 1 ||
		len(stages[0].Measurements) != 1 ||
		stages[0].Measurements[0].Name != "throughput" ||
		!slices.Equal(images, []string{runnerImage}) {
		t.Fatalf("stages = %#v, images = %#v", stages, images)
	}
}

func resolvedTerminalTestRun(
	t *testing.T,
	node *corev1.Node,
	stageVerdict signalv1.StageVerdict,
	diagnostic bool,
) (*signalv1.CertificationRun, signalv1.StageEvidenceSummary, string) {
	t.Helper()

	runnerImage := pinnedImage("runner", "b")
	minimumThroughput := "25.0"
	stage := signalv1.ResolvedStagePlan{
		Name: "validation",
		Test: signalv1.ResolvedCertificationTest{
			Identity: signalv1.TestIdentitySnapshot{
				Name:          "test",
				UID:           "test-uid",
				Generation:    1,
				ContentDigest: "sha256:" + strings.Repeat("c", 64),
				Trust:         signalv1.CertificationTestTrustApproved,
			},
			ParameterSchemaDigest:      "sha256:" + strings.Repeat("1", 64),
			MetricSchemaDigest:         "sha256:" + strings.Repeat("2", 64),
			SecurityRequirementsDigest: "sha256:" + strings.Repeat("3", 64),
			Metrics: []signalv1.TestMetricDefinition{{
				Name:        "throughput",
				DisplayName: "Throughput",
				Type:        signalv1.MetricValueTypeThroughput,
				Required:    true,
			}},
			Driver: signalv1.CertificationTestDriver{
				Type:      signalv1.TestDriverTypeContainer,
				Container: &signalv1.ContainerTestDriver{Image: runnerImage},
			},
			Topology: signalv1.TestTopology{
				MinParticipants: 1,
				MaxParticipants: 1,
			},
		},
		Schedule: signalv1.StageSchedule{
			Mode:              signalv1.CheckExecutionModeRunOnce,
			InvocationTimeout: metav1.Duration{Duration: time.Minute},
		},
		Assertions: []signalv1.TestAssertion{{
			Name:     "throughput",
			Metric:   "throughput",
			Operator: signalv1.AssertionOperatorMinimum,
			Expected: &signalv1.TypedValue{
				Type:         signalv1.TypedValueTypeDecimal,
				DecimalValue: &minimumThroughput,
			},
		}},
		QualificationEligible: !diagnostic,
		DiagnosticOnly:        diagnostic,
	}
	plan := &signalv1.ResolvedCertificationPlan{
		Phases: []signalv1.ResolvedPhasePlan{{
			Name:   "validation",
			Role:   signalv1.CheckStageRoleValidation,
			Stages: []signalv1.ResolvedStagePlan{stage},
		}},
	}
	provenance := signalv1.ExecutionPlanProvenance{
		ControllerImage:        pinnedImage("controller", "a"),
		AdapterImage:           pinnedImage("adapter", "d"),
		WorkflowTemplateDigest: "sha256:" + strings.Repeat("e", 64),
	}

	digest, err := signalv1.DeriveResolvedExecutionPlanDigest(1, nil, *plan, provenance)
	if err != nil {
		t.Fatalf("derive resolved plan digest: %v", err)
	}

	fingerprint := nodeFingerprint(node)
	stageFingerprint := fingerprint
	stageFingerprint.DriverVersion = "580.1"
	stageFingerprint.GPUInventoryDigest = "sha256:" + strings.Repeat("8", 64)
	startedAt := metav1.NewTime(time.Date(2026, 11, 5, 17, 0, 0, 0, time.UTC))
	finishedAt := metav1.NewTime(startedAt.Add(time.Minute))
	assertionPassed := stageVerdict == signalv1.StageVerdictPassed
	actualThroughput := "30.0"
	actual := &signalv1.TypedValue{
		Type:         signalv1.TypedValueTypeDecimal,
		DecimalValue: &actualThroughput,
	}
	message := ""

	if !assertionPassed {
		actual = nil
		message = "required throughput metric was not produced"
	}

	summary := signalv1.StageEvidenceSummary{
		PhaseOrder:             0,
		StageOrder:             0,
		Name:                   stage.Name,
		Role:                   signalv1.CheckStageRoleValidation,
		Test:                   stage.Test.Identity,
		Mode:                   stage.Schedule.Mode,
		Verdict:                stageVerdict,
		StartedAt:              startedAt,
		FinishedAt:             finishedAt,
		ObservedCoverageWindow: metav1.Duration{Duration: time.Minute},
		InvocationCount:        1,
		SuccessfulInvocations:  1,
		Assertions: []signalv1.AssertionResult{{
			Name:    "throughput",
			Passed:  assertionPassed,
			Actual:  actual,
			Message: message,
		}},
		DurableEvidenceDigest: "sha256:" + strings.Repeat("f", 64),
		QualificationEligible: !diagnostic,
		DiagnosticOnly:        diagnostic,
	}
	if !diagnostic {
		participantVerdict := signalv1.QualificationVerdictPass
		if stageVerdict == signalv1.StageVerdictFailed {
			participantVerdict = signalv1.QualificationVerdictFail
		}

		summary.Participants = []signalv1.ParticipantEvidenceIdentity{{
			Index:   0,
			Role:    signalv1.ParticipantRoleRequester,
			Verdict: participantVerdict,
			Start:   stageFingerprint,
			End:     stageFingerprint,
		}}
	}

	if stageVerdict == signalv1.StageVerdictFailed {
		summary.SuccessfulInvocations = 0
		summary.FailedInvocations = 1
	}

	startIdentity, err := json.Marshal([]signalv1.NodeFingerprint{fingerprint})
	if err != nil {
		t.Fatalf("encode start identity: %v", err)
	}

	run := &signalv1.CertificationRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "resolved-terminal",
			Namespace: "signal-system",
			UID:       "resolved-terminal-uid",
			Annotations: map[string]string{
				RunStartIdentityAnnotation: string(startIdentity),
			},
		},
		Spec: signalv1.CertificationRunSpec{
			Profile: signalv1.PinnedProfile{Name: "profile", UID: "profile-uid", Generation: 1},
			Suite:   signalv1.PinnedSuite{Name: "suite", UID: "suite-uid", Generation: 1},
			Participants: []signalv1.Participant{{
				NodeName: node.Name,
				NodeUID:  node.UID,
			}},
			ResolvedPlan:   plan,
			PlanDigest:     digest,
			PlanProvenance: &provenance,
		},
	}

	return run, summary, runnerImage
}

type staticEvidenceStore struct {
	data   []byte
	uri    string
	digest string
}

func (s staticEvidenceStore) Put(context.Context, string, []byte) (string, string, error) {
	return s.uri, s.digest, nil
}

func (s staticEvidenceStore) Get(context.Context, string) ([]byte, string, string, bool, error) {
	return s.data, s.uri, s.digest, true, nil
}

func resolvedCleanupScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	if err := signalv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	scheme.AddKnownTypeWithName(workflowGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{
			Group:   workflowGVK.Group,
			Version: workflowGVK.Version,
			Kind:    workflowGVK.Kind + "List",
		},
		&unstructured.UnstructuredList{},
	)

	return scheme
}

func cleanupWorkflow(
	name string,
	namespace string,
	uid types.UID,
	runUID types.UID,
	owners []metav1.OwnerReference,
) *unstructured.Unstructured {
	workflow := &unstructured.Unstructured{}
	workflow.SetGroupVersionKind(workflowGVK)
	workflow.SetName(name)
	workflow.SetNamespace(namespace)
	workflow.SetUID(uid)
	workflow.SetLabels(map[string]string{RunUIDLabel: string(runUID)})
	workflow.SetOwnerReferences(owners)

	return workflow
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

func TestSuiteChecksAssignsPositionalIdentity(t *testing.T) {
	t.Parallel()

	suite := &signalv1.CertificationSuite{
		Spec: signalv1.CertificationSuiteSpec{
			Checks: suiteCheckPlan(signalv1.MaxCertificationChecks, time.Minute),
		},
	}

	checks := suiteChecks(suite)

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

func TestResolveSuite(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := signalv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add signal scheme: %v", err)
	}

	profile := &signalv1.CertificationProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "profile"},
		Spec: signalv1.CertificationProfileSpec{
			Suite: signalv1.CertificationSuiteReference{Name: "suite"},
		},
	}
	suite := &signalv1.CertificationSuite{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "suite",
			UID:        types.UID("suite-uid"),
			Generation: 7,
		},
		Spec: signalv1.CertificationSuiteSpec{
			Checks: suiteCheckPlan(16, time.Minute),
		},
	}

	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(suite).Build()
	reconciler := &AdmissionReconciler{Client: kube, APIReader: kube}

	resolved, checks, err := reconciler.resolveSuite(context.Background(), profile)
	if err != nil {
		t.Fatalf("resolveSuite() error = %v", err)
	}

	if resolved.UID != suite.UID || resolved.Generation != suite.Generation {
		t.Fatalf(
			"resolved Suite identity = %s/%d, want %s/%d",
			resolved.UID,
			resolved.Generation,
			suite.UID,
			suite.Generation,
		)
	}

	if len(checks) != 16 || checks[15].ID != "R16" || checks[15].Order != 15 {
		t.Fatalf("resolved checks do not preserve positional identity: %#v", checks)
	}

	missingProfile := profile.DeepCopy()
	missingProfile.Spec.Suite.Name = "missing"

	if _, _, err := reconciler.resolveSuite(context.Background(), missingProfile); err == nil {
		t.Fatal("resolveSuite() accepted a missing Suite")
	}

	invalidSuite := suite.DeepCopy()
	invalidSuite.Name = "invalid"
	invalidSuite.Spec.Checks = nil

	invalidClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(invalidSuite).Build()
	invalidReconciler := &AdmissionReconciler{Client: invalidClient, APIReader: invalidClient}
	invalidProfile := profile.DeepCopy()
	invalidProfile.Spec.Suite.Name = invalidSuite.Name

	if _, _, err := invalidReconciler.resolveSuite(context.Background(), invalidProfile); err == nil {
		t.Fatal("resolveSuite() accepted an empty Suite plan")
	}
}

func TestSuiteWatchRequestsProfileNodes(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}

	if err := signalv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add signal scheme: %v", err)
	}

	suite := &signalv1.CertificationSuite{ObjectMeta: metav1.ObjectMeta{Name: "suite-a"}}
	profile := &signalv1.CertificationProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "profile-a"},
		Spec: signalv1.CertificationProfileSpec{
			Suite: signalv1.CertificationSuiteReference{Name: suite.Name},
		},
	}
	otherProfile := &signalv1.CertificationProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "profile-b"},
		Spec: signalv1.CertificationProfileSpec{
			Suite: signalv1.CertificationSuiteReference{Name: "suite-b"},
		},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-a",
			Labels: map[string]string{RequestLabel: profile.Name},
		},
	}
	otherNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-b",
			Labels: map[string]string{RequestLabel: otherProfile.Name},
		},
	}

	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(profile, otherProfile, node, otherNode).
		WithIndex(&signalv1.CertificationProfile{}, profileSuiteNameField, profileSuiteIndex).
		Build()
	reconciler := &AdmissionReconciler{Client: kube}

	requests := reconciler.requestsForSuite(context.Background(), suite)
	if len(requests) != 1 || requests[0].Name != node.Name {
		t.Fatalf("requestsForSuite() = %#v, want Node %s", requests, node.Name)
	}
}

func TestProfileWatchRequestsProfileNodes(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}

	if err := signalv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add signal scheme: %v", err)
	}

	profile := &signalv1.CertificationProfile{ObjectMeta: metav1.ObjectMeta{Name: "profile-a"}}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-a",
			Labels: map[string]string{RequestLabel: profile.Name},
		},
	}
	otherNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-b",
			Labels: map[string]string{RequestLabel: "profile-b"},
		},
	}

	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node, otherNode).
		Build()
	reconciler := &AdmissionReconciler{Client: kube}

	requests := reconciler.requestsForProfile(context.Background(), profile)
	if len(requests) != 1 || requests[0].Name != node.Name {
		t.Fatalf("requestsForProfile() = %#v, want Node %s", requests, node.Name)
	}
}

func TestSuiteSnapshotAndRunPinning(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}

	if err := signalv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add signal scheme: %v", err)
	}

	checks := suiteCheckPlan(2, time.Minute)

	checks[0].ScriptKey = "first.sh"
	checks[1].ScriptKey = "second.sh"
	scripts := map[string]string{
		"first.sh":  "echo first",
		"second.sh": "echo second",
	}

	bundleDigest, err := signalv1.DeriveScriptBundleDigest(checks, scripts)
	if err != nil {
		t.Fatalf("derive script bundle digest: %v", err)
	}

	suite := &signalv1.CertificationSuite{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "suite",
			UID:        types.UID("suite-uid"),
			Generation: 4,
		},
		Spec: signalv1.CertificationSuiteSpec{
			ScriptBundle: &signalv1.ScriptBundleSource{
				Name:   "suite-scripts",
				Digest: bundleDigest,
			},
			Checks: checks,
		},
	}
	profile := &signalv1.CertificationProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "profile",
			UID:        types.UID("profile-uid"),
			Generation: 3,
		},
		Spec: signalv1.CertificationProfileSpec{
			Participants: signalv1.ParticipantPolicy{Count: 1},
			Suite:        signalv1.CertificationSuiteReference{Name: suite.Name},
		},
	}
	immutable := true
	source := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: suite.Spec.ScriptBundle.Name, Namespace: "scripts"},
		Immutable:  &immutable,
		Data:       scripts,
	}

	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(suite, source).Build()
	reconciler := &AdmissionReconciler{
		Client:          kube,
		APIReader:       kube,
		Namespace:       "project-signal",
		ScriptNamespace: source.Namespace,
		RunnerImage:     "registry.invalid/runner@sha256:" + strings.Repeat("a", 64),
	}

	resolvedSuite, resolvedChecks, err := reconciler.resolveSuite(context.Background(), profile)
	if err != nil {
		t.Fatalf("resolveSuite() error = %v", err)
	}

	sourceScripts, err := reconciler.loadScriptBundle(
		context.Background(),
		*resolvedSuite.Spec.ScriptBundle,
		resolvedChecks,
	)
	if err != nil {
		t.Fatalf("loadScriptBundle() error = %v", err)
	}

	snapshot, checkSnapshots, err := reconciler.createSnapshot(
		context.Background(),
		"run",
		sourceScripts,
		resolvedChecks,
	)
	if err != nil {
		t.Fatalf("createSnapshot() error = %v", err)
	}

	snapshot.UID = types.UID("snapshot-uid")

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node"}}
	participants := []signalv1.Participant{{NodeName: node.Name, NodeUID: types.UID("node-uid")}}

	run, err := newCertificationRun(
		"run",
		reconciler.Namespace,
		node,
		profile,
		resolvedSuite,
		participants,
		snapshot,
		checkSnapshots,
		signalv1.ExecutionPlanProvenance{
			AdapterImage:           "registry.invalid/adapter@sha256:" + strings.Repeat("b", 64),
			WorkflowTemplateDigest: "sha256:" + strings.Repeat("c", 64),
		},
		[]signalv1.NodeFingerprint{{NodeUID: types.UID("node-uid")}},
	)
	if err != nil {
		t.Fatalf("newCertificationRun() error = %v", err)
	}

	if run.Spec.Suite.Name != suite.Name ||
		run.Spec.Suite.UID != suite.UID ||
		run.Spec.Suite.Generation != suite.Generation {
		t.Fatalf("Run Suite pin = %#v, want %s/%s/%d", run.Spec.Suite, suite.Name, suite.UID, suite.Generation)
	}

	if run.Spec.ScriptBundle.Digest != bundleDigest ||
		len(run.Spec.Checks) != len(checks) ||
		run.Spec.Checks[0].Script.Digest != digestString(scripts["first.sh"]) ||
		run.Spec.Checks[1].Script.Digest != digestString(scripts["second.sh"]) {
		t.Fatalf("Run did not retain resolved script/check snapshots: %#v", run.Spec)
	}

	if run.Spec.PlanDigest == "" || run.Spec.PlanProvenance == nil {
		t.Fatalf("Run omitted immutable execution plan identity: %#v", run.Spec)
	}

	suite.Generation++

	suite.Spec.Checks = nil
	if run.Spec.Suite.Generation != 4 || len(run.Spec.Checks) != 2 {
		t.Fatal("Run snapshots changed after the source Suite mutated")
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
				checkResultPlanDigest: run.Spec.PlanDigest,
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

	wrongPlan := result.DeepCopy()
	wrongPlan.Annotations[checkResultPlanDigest] = "sha256:" + strings.Repeat("f", 64)
	planClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(wrongPlan).Build()
	planReconciler := &RunReconciler{Client: planClient, APIReader: planClient}

	if _, err := planReconciler.readCheckResult(context.Background(), workflow, run, check); err == nil {
		t.Fatal("readCheckResult() accepted a mismatched plan digest")
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

func suiteCheckPlan(count int, deadline time.Duration) []signalv1.CertificationCheck {
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
