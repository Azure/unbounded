// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

func TestBuiltinAdmissionPinsParticipantAndTestPlans(t *testing.T) {
	t.Parallel()

	node := readyNode("gpu-0", "example-gpu")
	node.Status.Capacity = corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")}
	node.Labels[RequestLabel] = "full"
	profile := testProfile("full", "example-gpu")
	profile.Spec.Workflow.Name = FullNodeCertificationWorkflow
	profile.Spec.Parameters = map[string]string{"expected-gpu-count": "8", "configured-mode": "baseline"}
	source := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "scripts", Namespace: "signal"},
		Data:       map[string]string{"payload.txt": "test payload\n"},
	}
	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(node, profile, source).Build()
	r := &AdmissionReconciler{Client: kube, Namespace: "signal", ScriptNamespace: "signal"}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKey{Name: node.Name}}); err != nil {
		t.Fatal(err)
	}

	var runs signalv1.CertificationRunList
	if err := kube.List(context.Background(), &runs); err != nil {
		t.Fatal(err)
	}

	if len(runs.Items) != 1 {
		t.Fatalf("runs = %d", len(runs.Items))
	}

	run := &runs.Items[0]
	if len(run.Spec.ParticipantPlan) != 1 || run.Spec.ParticipantPlan[0] != participantSnapshot(node) {
		t.Fatalf("participant plan = %#v", run.Spec.ParticipantPlan)
	}

	if len(run.Spec.TestPlan) != 4 {
		t.Fatalf("test plan = %#v", run.Spec.TestPlan)
	}

	for _, invocation := range run.Spec.TestPlan {
		if invocation.NodeName != node.Name || invocation.NodeUID != node.UID ||
			invocation.Parameters["configured-mode"] != "baseline" ||
			invocation.Parameters[ObservedGPUCapacityParameter] != "8" {
			t.Fatalf("invocation = %#v", invocation)
		}
	}

	copy := run.DeepCopy()

	copy.Spec.TestPlan[0].Parameters["configured-mode"] = "changed"
	if run.Spec.TestPlan[0].Parameters["configured-mode"] != "baseline" {
		t.Fatal("deepcopy aliases immutable test parameters")
	}
}

func TestBuiltinPlansOnlyExistingInvocations(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		workflow string
		ids      []string
	}{
		{GPUCertificationWorkflow, []string{"gpu-identity", "mpi-backend"}},
		{FullNodeCertificationWorkflow, []string{"preflight", "gpu-health", "mpi-rank-access", "health-after"}},
		{BurnCanaryWorkflow, []string{"gpu-burn", "cpu-stress", "memory-integrity", "storage-integrity"}},
	} {
		t.Run(tc.workflow, func(t *testing.T) {
			t.Parallel()

			run := builtinTestRun(t, tc.workflow)

			plan, strict, err := effectiveTestPlan(run)
			if err != nil || !strict {
				t.Fatalf("strict = %t, err = %v", strict, err)
			}

			var ids []string
			for _, invocation := range plan {
				ids = append(ids, invocation.ID)
			}

			if !reflect.DeepEqual(ids, tc.ids) {
				t.Fatalf("IDs = %v, want %v", ids, tc.ids)
			}
		})
	}
}

func TestGroupedPlansMapInventoryAndParametersToEachPinnedNode(t *testing.T) {
	t.Parallel()

	run := builtinTestRun(t, BurnCanaryWorkflow)
	run.Spec.Workflow.Name = RackCertificationWorkflow
	run.Spec.Parameters[NodeWorkflowTemplateParameter] = BurnCanaryWorkflow
	run.Spec.Parameters["configured-mode"] = "baseline"
	run.Spec.Participants = append(run.Spec.Participants, signalv1.Participant{NodeName: "gpu-1", NodeUID: "gpu-1-uid"})
	run.Spec.ParticipantPlan = append(run.Spec.ParticipantPlan, signalv1.ParticipantSnapshot{
		NodeName: "gpu-1", NodeUID: "gpu-1-uid", ObservedGPUCapacity: "4", ObservedGPUAllocatable: "3",
	})
	run.Spec.TestPlan = nil

	plan, strict, err := makeTestPlan(&run.Spec)
	if err != nil || !strict || len(plan) != 8 {
		t.Fatalf("plan = %#v, strict = %t, err = %v", plan, strict, err)
	}

	for _, invocation := range plan {
		capacity, allocatable := "8", "8"
		if invocation.NodeName == "gpu-1" {
			capacity, allocatable = "4", "3"
		}

		if invocation.Parameters[ObservedGPUCapacityParameter] != capacity ||
			invocation.Parameters[ObservedGPUAllocatableParameter] != allocatable ||
			invocation.Parameters["configured-mode"] != "baseline" {
			t.Fatalf("invocation parameters = %#v", invocation)
		}
	}
}

func TestBuiltinPlanRejectsInvalidProvenanceAndUnsupportedRackTests(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		change func(*signalv1.CertificationRun)
	}{
		{"wrong snapshot UID", func(run *signalv1.CertificationRun) { run.Spec.ParticipantPlan[0].NodeUID = "replacement" }},
		{"duplicate snapshot", func(run *signalv1.CertificationRun) {
			run.Spec.ParticipantPlan = append(run.Spec.ParticipantPlan, run.Spec.ParticipantPlan[0])
		}},
		{"missing node UID", func(run *signalv1.CertificationRun) { run.Spec.Participants[0].NodeUID = "" }},
		{"missing group node template", func(run *signalv1.CertificationRun) { run.Spec.Workflow.Name = RackCertificationWorkflow }},
		{"unsupported group node template", func(run *signalv1.CertificationRun) {
			run.Spec.Workflow.Name = RackCertificationWorkflow
			run.Spec.Parameters[NodeWorkflowTemplateParameter] = "custom"
		}},
		{"unsupported rack test", func(run *signalv1.CertificationRun) { run.Spec.Parameters["rack-tests"] = `["fabric"]` }},
		{"malformed rack test", func(run *signalv1.CertificationRun) { run.Spec.Parameters["rack-tests"] = "null" }},
		{"changed invocation", func(run *signalv1.CertificationRun) { run.Spec.TestPlan[0].ID = "invented-test" }},
		{"changed parameters", func(run *signalv1.CertificationRun) { run.Spec.TestPlan[0].Parameters["configured-mode"] = "changed" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			run := builtinTestRun(t, FullNodeCertificationWorkflow)
			tc.change(run)

			if _, strict, err := effectiveTestPlan(run); !strict || err == nil {
				t.Fatalf("strict = %t, err = %v", strict, err)
			}
		})
	}

	for _, value := range []string{"", "[]", " [ ] "} {
		if err := validateRackTests(value); err != nil {
			t.Fatalf("empty rack tests %q: %v", value, err)
		}
	}
}

func TestRackTemplateRetainsMembershipHoldAndRejectsUnsupportedRackTests(t *testing.T) {
	t.Parallel()

	for _, nodeTemplate := range []string{GPUCertificationWorkflow, FullNodeCertificationWorkflow, BurnCanaryWorkflow} {
		t.Run(nodeTemplate, func(t *testing.T) {
			t.Parallel()

			request := readyNode("gpu-0", "example-gpu")
			peer := readyNode("gpu-1", "example-gpu")
			profile := testProfile("rack-held", "example-gpu")
			profile.Spec.Workflow.Name = RackCertificationWorkflow
			profile.Spec.Participants.Count = 2
			profile.Spec.Parameters = map[string]string{
				NodeWorkflowTemplateParameter: nodeTemplate,
				"rack-tests":                  "[]",
			}
			kube := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(request, peer).Build()
			r := &AdmissionReconciler{Client: kube}

			if _, err := r.selectParticipants(t.Context(), request, profile); err == nil ||
				!strings.Contains(err.Error(), "multi-participant execution is not implemented") {
				t.Fatalf("rack template bypassed the membership hold: %v", err)
			}

			for _, value := range []string{`["fabric"]`, "null", `{"test":"fabric"}`} {
				profile.Spec.Parameters["rack-tests"] = value
				if _, strict, err := nodeWorkflow(RackCertificationWorkflow, profile.Spec.Parameters); !strict ||
					err == nil || !strings.HasPrefix(err.Error(), "UnsupportedRackTest:") {
					t.Fatalf("rack-tests %q: strict = %t, err = %v", value, strict, err)
				}
			}
		})
	}
}

func TestWorkflowPlansAreReservedAndKeepLegacyParticipants(t *testing.T) {
	t.Parallel()

	run := builtinTestRun(t, FullNodeCertificationWorkflow)
	run.Spec.Parameters[ParticipantPlanParameter] = "untrusted"
	run.Spec.Parameters[TestPlanParameter] = "untrusted"
	run.Spec.TestPlan = nil

	workflow, err := buildWorkflow(run, 1)
	if err != nil {
		t.Fatal(err)
	}

	var participants []signalv1.ParticipantSnapshot
	if err := json.Unmarshal([]byte(workflowArgument(workflow, ParticipantPlanParameter)), &participants); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(participants, run.Spec.ParticipantPlan) {
		t.Fatalf("participants = %#v", participants)
	}

	var tests []signalv1.TestInvocation
	if err := json.Unmarshal([]byte(workflowArgument(workflow, TestPlanParameter)), &tests); err != nil {
		t.Fatal(err)
	}

	if len(tests) != 4 || tests[0].Parameters[ParticipantPlanParameter] != "" ||
		tests[0].Parameters[TestPlanParameter] != "" || workflowArgument(workflow, "participants") != "gpu-0" {
		t.Fatalf("test plan = %#v, participants = %q", tests, workflowArgument(workflow, "participants"))
	}
}

func TestParticipantSnapshotAndReservationRollbackPreserveOtherOwners(t *testing.T) {
	t.Parallel()

	first := readyNode("gpu-0", "example-gpu")
	second := readyNode("gpu-1", "example-gpu")
	second.Annotations = map[string]string{ReservationLabel: "other-run"}
	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(first, second).Build()
	r := &AdmissionReconciler{Client: kube}
	participants := []signalv1.Participant{{NodeName: first.Name, NodeUID: first.UID}, {NodeName: second.Name, NodeUID: second.UID}}

	if err := r.reserveParticipants(context.Background(), participants, "group-run"); err == nil {
		t.Fatal("reservation unexpectedly succeeded")
	}

	for _, participant := range participants {
		var node corev1.Node
		if err := kube.Get(context.Background(), client.ObjectKey{Name: participant.NodeName}, &node); err != nil {
			t.Fatal(err)
		}

		expected := ""
		if node.Name == second.Name {
			expected = "other-run"
		}

		if node.Annotations[ReservationLabel] != expected {
			t.Fatalf("reservation for %s = %q", node.Name, node.Annotations[ReservationLabel])
		}
	}

	if _, err := r.snapshotParticipants(context.Background(), participants, "group-run"); err == nil ||
		!strings.Contains(err.Error(), "reservation changed") {
		t.Fatalf("snapshot unowned participants: %v", err)
	}
}

func builtinTestRun(t *testing.T, workflow string) *signalv1.CertificationRun {
	t.Helper()

	run := testRun("builtin-run")
	run.Spec.Workflow.Name = workflow
	run.Spec.Parameters = map[string]string{
		"configured-mode":               "baseline",
		ObservedGPUCapacityParameter:    "8",
		ObservedGPUAllocatableParameter: "8",
	}
	run.Spec.ParticipantPlan = []signalv1.ParticipantSnapshot{{
		NodeName: "gpu-0", NodeUID: "gpu-0-uid", ObservedGPUCapacity: "8", ObservedGPUAllocatable: "8",
	}}

	plan, _, err := makeTestPlan(&run.Spec)
	if err != nil {
		t.Fatal(err)
	}

	run.Spec.TestPlan = plan
	run.Status.Workflow = signalv1.WorkflowStatus{Name: run.Name + "-a1", UID: "workflow-uid", Attempt: 1, Phase: "Running"}

	return run
}

func runSnapshot(run *signalv1.CertificationRun) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: run.Spec.Script.ConfigMapName, Namespace: run.Namespace},
		Data:       map[string]string{run.Spec.Script.Key: "test payload\n"},
	}
}
