// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signaladapter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

func TestResolveRequestUsesImmutableRunSnapshot(t *testing.T) {
	t.Parallel()

	request := Request{
		Namespace:       "project-signal",
		WorkflowName:    "run-workflow",
		RunName:         "certification-run",
		ExecutionName:   "certification-run-r02",
		RunUID:          types.UID("run-uid"),
		CheckIndex:      1,
		ResultConfigMap: "certification-run-r02-result",
	}
	run := certificationRun()

	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(run)
	if err != nil {
		t.Fatal(err)
	}

	runner := Runner{
		Dynamic: dynamicfake.NewSimpleDynamicClient(
			runtime.NewScheme(),
			&unstructured.Unstructured{Object: content},
		),
	}

	config, check, err := runner.ResolveRequest(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}

	if check.ID != "R02" || check.Order != 1 {
		t.Fatalf("resolved check identity = %q/%d, want R02/1", check.ID, check.Order)
	}

	if config.ScriptConfigMap != run.Spec.ScriptBundle.Name ||
		config.ScriptKey != run.Spec.Checks[1].Script.Key ||
		config.ParticipantCount != len(run.Spec.Participants) ||
		config.BackendKind != string(run.Spec.Checks[1].Backend) {
		t.Fatalf("resolved config does not match the immutable run snapshot: %#v", config)
	}

	request.RunUID = "other-uid"
	if _, _, err := runner.ResolveRequest(t.Context(), request); err == nil {
		t.Fatal("ResolveRequest() accepted a mismatched run UID")
	}
}

func TestPersistResultCreatesControllerContractConfigMap(t *testing.T) {
	t.Parallel()

	request := Request{
		Namespace:       "project-signal",
		WorkflowName:    "run-workflow",
		RunName:         "certification-run",
		ExecutionName:   "certification-run-r02",
		RunUID:          types.UID("run-uid"),
		CheckIndex:      1,
		ResultConfigMap: "certification-run-r02-result",
	}
	workflow := &unstructured.Unstructured{}
	workflow.SetAPIVersion("argoproj.io/v1alpha1")
	workflow.SetKind("Workflow")
	workflow.SetNamespace(request.Namespace)
	workflow.SetName(request.WorkflowName)
	workflow.SetUID(types.UID("workflow-uid"))

	run := certificationRun()

	runContent, err := runtime.DefaultUnstructuredConverter.ToUnstructured(run)
	if err != nil {
		t.Fatal(err)
	}

	kubernetesClient := kubernetesfake.NewSimpleClientset()
	runner := Runner{
		Dynamic: dynamicfake.NewSimpleDynamicClient(
			runtime.NewScheme(),
			workflow,
			&unstructured.Unstructured{Object: runContent},
		),
		Kubernetes: kubernetesClient,
	}
	check := *run.Spec.Checks[1].DeepCopy()
	check.ID = "R02"
	check.Order = 1
	output := AggregateOutput{
		ExecutionState:  "Succeeded",
		Verdict:         "Pass",
		Reason:          "AllParticipantsPassed",
		Measurements:    "{}",
		BackendJSON:     "[]",
		BackendEvidence: validBackendEvidence(t, run, check),
	}

	if err := runner.PersistResult(t.Context(), request, check, output); err != nil {
		t.Fatal(err)
	}

	result, err := kubernetesClient.CoreV1().ConfigMaps(request.Namespace).
		Get(t.Context(), request.ResultConfigMap, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if result.Immutable == nil || !*result.Immutable {
		t.Fatal("result ConfigMap is not immutable")
	}

	if len(result.OwnerReferences) != 1 ||
		result.OwnerReferences[0].UID != workflow.GetUID() ||
		result.OwnerReferences[0].Controller == nil ||
		!*result.OwnerReferences[0].Controller {
		t.Fatalf("unexpected result owner references: %#v", result.OwnerReferences)
	}

	content := result.BinaryData[resultDataKey]
	if len(content) == 0 {
		t.Fatal("result ConfigMap is missing result.json")
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(content, &fields); err != nil {
		t.Fatal(err)
	}

	for _, field := range []string{
		"executionState",
		"verdict",
		"reason",
		"measurements",
		"backend-resources",
		"backend-evidence",
	} {
		if _, exists := fields[field]; !exists {
			t.Fatalf("result.json is missing %q", field)
		}
	}

	if len(fields) != 6 {
		t.Fatalf("result.json has %d fields, want 6", len(fields))
	}

	digest := sha256.Sum256(content)
	if result.Annotations[resultDigestAnnotation] != "sha256:"+hex.EncodeToString(digest[:]) ||
		result.Annotations[resultRunUIDAnnotation] != string(request.RunUID) ||
		result.Annotations[resultCheckIDAnnotation] != string(check.ID) ||
		result.Annotations[resultCheckOrderAnnotation] != "1" ||
		result.Annotations[resultPlanDigestAnnotation] != run.Spec.PlanDigest ||
		result.Labels[runLabel] != request.RunName {
		t.Fatalf("unexpected result identity metadata: labels=%v annotations=%v", result.Labels, result.Annotations)
	}
}

func TestPersistInvocationEvidenceAppendsImmutableVersions(t *testing.T) {
	t.Parallel()

	request := StageRequest{
		Namespace:       "signal-system",
		WorkflowName:    "run-workflow",
		RunName:         "certification-run",
		ExecutionName:   "certification-run-stage",
		RunUID:          types.UID("run-uid"),
		PhaseIndex:      1,
		StageIndex:      2,
		ResultConfigMap: "certification-run-stage-result",
	}
	execution := StageExecutionRequest{
		RunUID:     request.RunUID,
		PlanDigest: testStageDigest("plan"),
		PhaseOrder: int32(request.PhaseIndex),
		StageOrder: int32(request.StageIndex),
	}
	runner := Runner{
		Dynamic: dynamicfake.NewSimpleDynamicClient(
			runtime.NewScheme(),
			testWorkflow(request.WorkflowName, "workflow-uid"),
		),
		Kubernetes: kubernetesfake.NewSimpleClientset(),
	}
	first := signalv1.InvocationResultEnvelope{InvocationID: "first"}
	second := signalv1.InvocationResultEnvelope{InvocationID: "second"}

	if err := runner.PersistInvocationEvidence(t.Context(), request, execution, []signalv1.InvocationResultEnvelope{
		first,
	}); err != nil {
		t.Fatalf("persist first invocation evidence: %v", err)
	}

	if err := runner.PersistInvocationEvidence(t.Context(), request, execution, []signalv1.InvocationResultEnvelope{
		first,
		second,
	}); err != nil {
		t.Fatalf("append invocation evidence: %v", err)
	}

	run := &signalv1.CertificationRun{
		ObjectMeta: metav1.ObjectMeta{UID: request.RunUID},
		Spec: signalv1.CertificationRunSpec{
			PlanDigest: execution.PlanDigest,
		},
	}

	resume, err := runner.latestInvocationEvidence(t.Context(), request, run)
	if err != nil {
		t.Fatalf("latestInvocationEvidence() error = %v", err)
	}

	if len(resume) != 2 || resume[0].InvocationID != "first" || resume[1].InvocationID != "second" {
		t.Fatalf("resumed invocation evidence = %#v", resume)
	}

	configMaps, err := runner.Kubernetes.CoreV1().ConfigMaps(request.Namespace).
		List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if len(configMaps.Items) != 2 {
		t.Fatalf("invocation evidence ConfigMaps = %d, want 2", len(configMaps.Items))
	}

	for index := range configMaps.Items {
		if configMaps.Items[index].Immutable == nil || !*configMaps.Items[index].Immutable {
			t.Fatalf("invocation evidence ConfigMap is mutable: %#v", configMaps.Items[index])
		}
	}
}

func TestIndexedParticipantsRestoresSerializedPositions(t *testing.T) {
	t.Parallel()

	participants := indexedParticipants([]signalv1.Participant{
		{NodeName: "worker-0", NodeUID: "node-uid-0"},
		{NodeName: "worker-1", NodeUID: "node-uid-1"},
	})

	if participants[0].Index != 0 || participants[1].Index != 1 {
		t.Fatalf("indexed participants = %#v", participants)
	}
}

func certificationRun() *signalv1.CertificationRun {
	return &signalv1.CertificationRun{
		TypeMeta: metav1.TypeMeta{
			APIVersion: signalv1.GroupVersion.String(),
			Kind:       "CertificationRun",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "certification-run",
			Namespace: "project-signal",
			UID:       types.UID("run-uid"),
		},
		Spec: signalv1.CertificationRunSpec{
			Participants: []signalv1.Participant{{
				NodeName: "worker-0",
				NodeUID:  types.UID("node-uid"),
			}},
			ScriptBundle: &signalv1.ScriptBundleReference{
				Name:   "script-snapshot",
				UID:    types.UID("script-uid"),
				Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			},
			PlanDigest: "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
			PlanProvenance: &signalv1.ExecutionPlanProvenance{
				AdapterImage:           "example.invalid/adapter@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				WorkflowTemplateDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			},
			Checks: []signalv1.CertificationCheckSnapshot{{
				Script: signalv1.ScriptSnapshot{
					Key:    "00-r01",
					Digest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
				},
				RunnerImage:           "example.invalid/runner@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
				Backend:               signalv1.ExecutionBackendPod,
				BackendActiveDeadline: metav1.Duration{Duration: time.Minute},
			}, {
				Script: signalv1.ScriptSnapshot{
					Key:    "01-r02",
					Digest: "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
				},
				RunnerImage:           "example.invalid/runner@sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
				Backend:               signalv1.ExecutionBackendPod,
				BackendActiveDeadline: metav1.Duration{Duration: 2 * time.Minute},
			}},
		},
	}
}

func validBackendEvidence(
	t *testing.T,
	run *signalv1.CertificationRun,
	check signalv1.CertificationCheckSnapshot,
) string {
	t.Helper()

	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	finished := started.Add(time.Minute)
	evidence := BackendEvidence{
		Version: backendEvidenceVersion,
		Identity: EvidenceIdentity{
			RunUID:     run.UID,
			CheckID:    check.ID,
			CheckOrder: check.Order,
			PlanDigest: run.Spec.PlanDigest,
		},
		Timing: CheckTimingEvidence{
			StartedAt:        metav1.NewTime(started),
			FinishedAt:       metav1.NewTime(finished),
			ObservedDuration: metav1.Duration{Duration: time.Minute},
			SampleCount:      2,
		},
		Provenance: ExecutionProvenanceEvidence{
			AdapterImage: run.Spec.PlanProvenance.AdapterImage,
			RunnerImage:  check.RunnerImage,
		},
		Complete: true,
		Participants: []ParticipantBackendEvidence{{
			ParticipantIndex: 0,
			Verdict: VerdictEvidence{
				ExecutionState: "Succeeded",
				Verdict:        "Pass",
				Reason:         "Passed",
				Attribution:    "participant",
			},
			Start: signalv1.NodeFingerprint{NodeUID: run.Spec.Participants[0].NodeUID, BootID: "boot-a"},
			End:   signalv1.NodeFingerprint{NodeUID: run.Spec.Participants[0].NodeUID, BootID: "boot-a"},
			Resource: BackendObjectEvidence{
				APIVersion: "v1",
				Kind:       "Pod",
				Namespace:  run.Namespace,
				Name:       "backend",
				UID:        "backend-uid",
			},
			Pods: []PodEvidence{},
		}},
	}

	content, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}

	return string(content)
}
