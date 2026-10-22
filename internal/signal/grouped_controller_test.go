// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

func TestGroupedAdmissionSnapshotsCompletePlanOnce(t *testing.T) {
	t.Parallel()

	node := readyNode("gpu-0", "example-gpu")
	node.Labels[RequestLabel] = "grouped-profile"
	profile := testProfile("grouped-profile", "example-gpu")
	profile.Spec.Checks = groupedProfileChecks()
	profile.Spec.ExecutionPolicy = signalv1.SuiteExecutionPolicy{OnFailure: signalv1.SuiteFailurePolicyStop}
	profile.Spec.Script = signalv1.ScriptReference{}
	profile.Spec.Backend = signalv1.ExecutionBackend{}
	profile.Spec.RunnerImage = ""
	profile.Spec.WorkloadPolicy = signalv1.WorkloadPolicy{}
	profile.Spec.StagePlan = nil
	profile.Spec.Parameters = nil
	profile.Spec.BackendActiveDeadline = metav1.Duration{}
	profile.Spec.MaxAttempts = 0

	source := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "scripts", Namespace: "signal"},
		Data:       map[string]string{},
	}
	for index := range profile.Spec.Checks {
		source.Data[profile.Spec.Checks[index].Script.Key] = fmt.Sprintf("check-%d\n", index)
	}

	scheme := testScheme(t)
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, profile, source).Build()
	reconciler := &AdmissionReconciler{
		Client:          kube,
		Namespace:       "signal",
		ScriptNamespace: "signal",
		Now:             func() time.Time { return time.Unix(100, 0) },
	}

	if _, err := reconciler.Reconcile(
		context.Background(),
		reconcile.Request{NamespacedName: client.ObjectKey{Name: node.Name}},
	); err != nil {
		t.Fatal(err)
	}

	var runs signalv1.CertificationRunList
	if err := kube.List(context.Background(), &runs); err != nil {
		t.Fatal(err)
	}

	if len(runs.Items) != 1 || len(runs.Items[0].Spec.Checks) != groupedCheckCount {
		t.Fatalf("runs = %#v", runs.Items)
	}

	run := &runs.Items[0]

	var snapshot corev1.ConfigMap
	if err := kube.Get(
		context.Background(),
		client.ObjectKey{Namespace: run.Namespace, Name: run.Spec.Checks[0].Script.ConfigMapName},
		&snapshot,
	); err != nil {
		t.Fatal(err)
	}

	if snapshot.Immutable == nil || !*snapshot.Immutable || len(snapshot.Data) != groupedCheckCount {
		t.Fatalf("snapshot = immutable:%v keys:%d", snapshot.Immutable, len(snapshot.Data))
	}

	for index := range run.Spec.Checks {
		check := &run.Spec.Checks[index]
		if check.Order != int32(index) ||
			check.Script.Digest != digestString(fmt.Sprintf("check-%d\n", index)) ||
			snapshot.Data[check.Script.Key] != fmt.Sprintf("check-%d\n", index) {
			t.Fatalf("check %d = %#v", index, check)
		}
	}
}

func TestGroupedAdmissionRejectsScriptDigestMismatch(t *testing.T) {
	t.Parallel()

	node := readyNode("gpu-0", "example-gpu")
	node.Labels[RequestLabel] = "grouped-profile"
	profile := testProfile("grouped-profile", "example-gpu")
	profile.Spec.Checks = groupedProfileChecks()
	profile.Spec.ExecutionPolicy = signalv1.SuiteExecutionPolicy{OnFailure: signalv1.SuiteFailurePolicyStop}

	source := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "scripts", Namespace: "signal"},
		Data:       map[string]string{},
	}
	for index := range profile.Spec.Checks {
		source.Data[profile.Spec.Checks[index].Script.Key] = fmt.Sprintf("check-%d\n", index)
	}

	source.Data[profile.Spec.Checks[8].Script.Key] = "changed\n"

	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(node, profile, source).Build()
	reconciler := &AdmissionReconciler{
		Client:          kube,
		Namespace:       "signal",
		ScriptNamespace: "signal",
	}

	_, err := reconciler.Reconcile(
		context.Background(),
		reconcile.Request{NamespacedName: client.ObjectKey{Name: node.Name}},
	)
	if err == nil || !strings.Contains(err.Error(), "source script digest for check R09") {
		t.Fatalf("Reconcile error = %v", err)
	}

	var got corev1.Node
	if getErr := kube.Get(context.Background(), client.ObjectKey{Name: node.Name}, &got); getErr != nil {
		t.Fatal(getErr)
	}

	if got.Annotations[ReservationLabel] != "" {
		t.Fatalf("reservation remained after digest rejection: %#v", got.Annotations)
	}
}

func TestParticipantRoleAndTopologyConstraints(t *testing.T) {
	t.Parallel()

	service := readyNode("service", "example-gpu")
	service.Labels["signal.example/role"] = "service"
	service.Labels["topology.example/zone"] = "zone-a"
	witnessA := readyNode("witness-a", "example-gpu")
	witnessA.Labels["signal.example/role"] = "witness"
	witnessA.Labels["topology.example/zone"] = "zone-a"
	witnessB := readyNode("witness-b", "example-gpu")
	witnessB.Labels["signal.example/role"] = "witness"
	witnessB.Labels["topology.example/zone"] = "zone-b"
	witnessC := readyNode("witness-c", "example-gpu")
	witnessC.Labels["signal.example/role"] = "witness"
	witnessC.Labels["topology.example/zone"] = "zone-b"

	participants := []signalv1.Participant{
		participantSnapshot(service, 0),
		participantSnapshot(witnessA, 1),
		participantSnapshot(witnessB, 2),
		participantSnapshot(witnessC, 3),
	}
	kube := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(service, witnessA, witnessB, witnessC).
		Build()
	reconciler := &AdmissionReconciler{Client: kube}

	err := reconciler.assignParticipantRoles(
		context.Background(),
		participants,
		[]signalv1.ParticipantRoleConstraint{
			{
				Name:     "service",
				Count:    1,
				Selector: metav1.LabelSelector{MatchLabels: map[string]string{"signal.example/role": "service"}},
			},
			{
				Name:     "witness",
				Count:    3,
				Selector: metav1.LabelSelector{MatchLabels: map[string]string{"signal.example/role": "witness"}},
			},
		},
		[]signalv1.ParticipantTopologyConstraint{{
			Role:              "witness",
			LabelKey:          "topology.example/zone",
			MinDistinctValues: 2,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(participants[0].Roles, []string{"service"}) {
		t.Fatalf("service roles = %#v", participants[0].Roles)
	}

	for index := 1; index < len(participants); index++ {
		if !slices.Equal(participants[index].Roles, []string{"witness"}) {
			t.Fatalf("witness %d roles = %#v", index, participants[index].Roles)
		}
	}
}

func TestGroupedWorkflowBuildsVisibleSerialDAG(t *testing.T) {
	t.Parallel()

	run := groupedTestRun("grouped")
	run.Spec.ExecutionPolicy.OnFailure = signalv1.SuiteFailurePolicyContinue
	run.Spec.Participants = []signalv1.Participant{
		{Index: 0, NodeName: "service", NodeUID: "service-uid", Roles: []string{"service"}},
		{Index: 1, NodeName: "witness-a", NodeUID: "witness-a-uid", Roles: []string{"witness"}},
		{Index: 2, NodeName: "witness-b", NodeUID: "witness-b-uid", Roles: []string{"witness"}},
		{Index: 3, NodeName: "witness-c", NodeUID: "witness-c-uid", Roles: []string{"witness"}},
	}
	run.Spec.Checks[14].ParticipantPolicy.ExcludedRoles = []string{"service"}

	workflow, err := buildWorkflow(run, 1)
	if err != nil {
		t.Fatal(err)
	}

	tasks, found, err := unstructured.NestedSlice(
		workflow.Object,
		"spec",
		"templates",
	)
	if err != nil || !found || len(tasks) != 1 {
		t.Fatalf("templates: found=%t len=%d err=%v", found, len(tasks), err)
	}

	template := tasks[0].(map[string]any)
	dag := template["dag"].(map[string]any)

	dagTasks := dag["tasks"].([]any)
	if len(dagTasks) != groupedCheckCount {
		t.Fatalf("DAG tasks = %d, want %d", len(dagTasks), groupedCheckCount)
	}

	for index := range dagTasks {
		task := dagTasks[index].(map[string]any)
		if index > 0 {
			previous := dagTasks[index-1].(map[string]any)["name"].(string)
			want := fmt.Sprintf(
				"%[1]s.Succeeded || %[1]s.Failed || %[1]s.Errored || %[1]s.Skipped || %[1]s.Omitted",
				previous,
			)
			if task["depends"] != want {
				t.Fatalf("task %d dependency = %q, want %q", index, task["depends"], want)
			}
		}

		templateRef := task["templateRef"].(map[string]any)
		if templateRef["name"] != run.Spec.Workflow.Name || templateRef["template"] != "certification" {
			t.Fatalf("task %d templateRef = %#v", index, templateRef)
		}
	}

	last := dagTasks[14].(map[string]any)
	parameters := last["arguments"].(map[string]any)["parameters"].([]any)

	values := parameterValues(parameters)
	if values["participant-count"] != "3" {
		t.Fatalf("row 15 participant count = %q, want 3 witnesses", values["participant-count"])
	}
}

func TestGroupedWorkflowStopPolicyRequiresDiagnosticPass(t *testing.T) {
	t.Parallel()

	run := groupedTestRun("grouped-stop")
	run.Spec.ExecutionPolicy.OnFailure = signalv1.SuiteFailurePolicyStop

	workflow, err := buildWorkflow(run, 1)
	if err != nil {
		t.Fatal(err)
	}

	templates, _, _ := unstructured.NestedSlice(workflow.Object, "spec", "templates")
	tasks := templates[0].(map[string]any)["dag"].(map[string]any)["tasks"].([]any)

	for index := 1; index < len(tasks); index++ {
		previous := tasks[index-1].(map[string]any)["name"].(string)

		task := tasks[index].(map[string]any)
		if task["depends"] != previous+".Succeeded" {
			t.Fatalf("task %d dependency = %q", index, task["depends"])
		}

		wantWhen := "{{tasks." + previous + ".outputs.parameters.verdict}} == Pass"
		if task["when"] != wantWhen {
			t.Fatalf("task %d when = %q, want %q", index, task["when"], wantWhen)
		}
	}
}

func TestGroupedTerminalAggregationRequiresExactCompleteCoverage(t *testing.T) {
	t.Parallel()

	run := groupedTestRun("aggregate")
	workflow := groupedWorkflow(run, "Succeeded")

	statuses, verdict, reason := terminalCheckStatuses(workflow, run)
	if verdict != "Pass" || reason != "AllChecksPassed" {
		t.Fatalf("aggregate = %s %s", verdict, reason)
	}

	if len(statuses) != groupedCheckCount {
		t.Fatalf("statuses = %d", len(statuses))
	}

	for index := range statuses {
		if statuses[index].Phase != signalv1.CheckPhaseSucceeded ||
			!statuses[index].BackendEvidence.Complete ||
			!statuses[index].Cleanup.Complete ||
			len(statuses[index].BackendResources) != 1 {
			t.Fatalf("status %d = %#v", index, statuses[index])
		}
	}
}

func TestGroupedTerminalAggregationMapsFilteredParticipantIndexes(t *testing.T) {
	t.Parallel()

	run := groupedTestRun("participant-indexes")
	run.Spec.Participants = []signalv1.Participant{
		{Index: 0, NodeName: "service", NodeUID: "service-uid", Roles: []string{"service"}},
		{Index: 1, NodeName: "witness-a", NodeUID: "witness-a-uid", Roles: []string{"witness"}},
		{Index: 2, NodeName: "witness-b", NodeUID: "witness-b-uid", Roles: []string{"witness"}},
		{Index: 3, NodeName: "witness-c", NodeUID: "witness-c-uid", Roles: []string{"witness"}},
	}
	run.Spec.Checks[14].ParticipantPolicy.ExcludedRoles = []string{"service"}

	workflow := groupedWorkflow(run, "Succeeded")
	node := groupedNode(workflow, run, 14)
	evidence, _ := json.Marshal(map[string]any{
		"version":  "v1",
		"complete": true,
		"participants": []any{
			groupedEvidenceParticipant(run, 14, 0),
			groupedEvidenceParticipant(run, 14, 1),
			groupedEvidenceParticipant(run, 14, 2),
		},
	})
	resources, _ := json.Marshal([]signalv1.BackendResourceReference{{
		APIVersion:         "v1",
		Kind:               "Pod",
		Namespace:          run.Namespace,
		Name:               "row-15",
		UID:                "row-15-uid",
		ParticipantIndexes: []int32{0, 1, 2},
	}})

	setNodeParameter(node, "backend-evidence", string(evidence))
	setNodeParameter(node, "backend-resources", string(resources))

	statuses, _, _ := terminalCheckStatuses(workflow, run)
	status := statuses[14]

	if !slices.Equal(status.BackendResources[0].ParticipantIndexes, []int32{1, 2, 3}) {
		t.Fatalf("backend participant indexes = %#v", status.BackendResources[0].ParticipantIndexes)
	}

	var normalized struct {
		Participants []struct {
			ParticipantIndex int32 `json:"participantIndex"`
		} `json:"participants"`
	}
	if err := json.Unmarshal([]byte(status.BackendEvidence.Content), &normalized); err != nil {
		t.Fatal(err)
	}

	got := make([]int32, len(normalized.Participants))
	for index := range normalized.Participants {
		got[index] = normalized.Participants[index].ParticipantIndex
	}

	if !slices.Equal(got, []int32{1, 2, 3}) {
		t.Fatalf("evidence participant indexes = %#v", got)
	}
}

func TestGroupedTerminalAggregationRetainsFailureAfterPolicyStop(t *testing.T) {
	t.Parallel()

	run := groupedTestRun("stop-after-fail")
	run.Spec.ExecutionPolicy.OnFailure = signalv1.SuiteFailurePolicyStop
	workflow := groupedWorkflow(run, "Succeeded")
	setNodeParameter(groupedNode(workflow, run, 2), "verdict", "Fail")

	for index := 3; index < groupedCheckCount; index++ {
		groupedNode(workflow, run, index)["phase"] = "Omitted"
	}

	statuses, verdict, reason := terminalCheckStatuses(workflow, run)
	if verdict != "Fail" || reason != "OneOrMoreChecksFailed" {
		t.Fatalf("aggregate = %q %q", verdict, reason)
	}

	if statuses[2].Phase != signalv1.CheckPhaseFailed {
		t.Fatalf("failed check status = %#v", statuses[2])
	}

	for index := 3; index < groupedCheckCount; index++ {
		if statuses[index].Phase != signalv1.CheckPhaseSkipped || !statuses[index].NotRun {
			t.Fatalf("stopped check %d = %#v", index, statuses[index])
		}
	}
}

func TestCompactCheckStatusesRetainsEvidenceDigest(t *testing.T) {
	t.Parallel()

	statuses := []signalv1.CheckStatus{{
		BackendEvidence: signalv1.BackendEvidenceStatus{
			Version:  "v1",
			Complete: true,
			Digest:   contentDigest(`{"version":"v1"}`),
			Content:  `{"version":"v1"}`,
		},
	}}

	compacted := compactCheckStatuses(statuses)
	if compacted[0].BackendEvidence.Content != "" {
		t.Fatalf("compacted backend evidence retained content")
	}

	if compacted[0].BackendEvidence.Digest != statuses[0].BackendEvidence.Digest {
		t.Fatalf("compacted digest = %q, want %q", compacted[0].BackendEvidence.Digest, statuses[0].BackendEvidence.Digest)
	}

	if statuses[0].BackendEvidence.Content == "" {
		t.Fatalf("compaction mutated source status")
	}
}

func TestGroupedReconcileRecoversPublishedEvidenceBeforeCancellation(t *testing.T) {
	t.Parallel()

	scheme := testScheme(t)
	run := groupedTestRun("recover-evidence")
	run.UID = "recover-evidence-uid"
	run.Spec.Cancel = true
	run.Spec.Participants = []signalv1.Participant{{
		Index:    0,
		NodeName: "gpu-0",
		NodeUID:  "gpu-0-uid",
	}}
	run.Status.Workflow = signalv1.WorkflowStatus{Name: run.Name + "-a1", UID: "workflow-uid", Attempt: 1, Phase: "Succeeded"}

	finishedAt := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)

	statuses := initialCheckStatuses(run.Spec.Checks)
	for index := range statuses {
		statuses[index].Phase = signalv1.CheckPhaseSucceeded
		statuses[index].Verdict = "Pass"
		statuses[index].BackendEvidence = signalv1.BackendEvidenceStatus{
			Version:  "v1",
			Complete: true,
			Digest:   contentDigest(`{"version":"v1"}`),
			Content:  `{"version":"v1"}`,
		}
	}

	document, err := json.Marshal(map[string]any{
		"runID":          string(run.UID),
		"runName":        run.Name,
		"workflow":       run.Status.Workflow,
		"checkStatus":    statuses,
		"executionState": "Completed",
		"verdict":        "Pass",
		"reason":         "AllChecksPassed",
		"finishedAt":     finishedAt.Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatal(err)
	}

	store := &immutableMemoryEvidenceStore{}
	if _, _, err := store.Put(context.Background(), run.Spec.EvidencePath, document); err != nil {
		t.Fatal(err)
	}

	node := readyNode("gpu-0", "example-gpu")
	node.UID = "gpu-0-uid"
	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&signalv1.CertificationRun{}).
		WithObjects(run, node).
		Build()
	reconciler := &RunReconciler{Client: kube, Evidence: store}

	if _, err := reconciler.Reconcile(
		context.Background(),
		reconcile.Request{NamespacedName: client.ObjectKeyFromObject(run)},
	); err != nil {
		t.Fatal(err)
	}

	var got signalv1.CertificationRun
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(run), &got); err != nil {
		t.Fatal(err)
	}

	if got.Status.Phase != signalv1.RunPhaseSucceeded ||
		got.Status.Verdict != "Pass" ||
		got.Status.ExecutionState != "Completed" {
		t.Fatalf("recovered status = %#v", got.Status)
	}

	if got.Status.Checks[0].BackendEvidence.Content != "" ||
		got.Status.Checks[0].BackendEvidence.Digest == "" {
		t.Fatalf("recovered check evidence was not compacted: %#v", got.Status.Checks[0].BackendEvidence)
	}
}

func TestGroupedTerminalAggregationFailsClosed(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*unstructured.Unstructured, *signalv1.CertificationRun){
		"missing task": func(workflow *unstructured.Unstructured, run *signalv1.CertificationRun) {
			nodes := workflow.Object["status"].(map[string]any)["nodes"].(map[string]any)
			delete(nodes, string(run.Spec.Checks[3].ID))
		},
		"duplicate identity": func(workflow *unstructured.Unstructured, run *signalv1.CertificationRun) {
			nodes := workflow.Object["status"].(map[string]any)["nodes"].(map[string]any)
			nodes["duplicate"] = nodes[string(run.Spec.Checks[4].ID)]
		},
		"cleanup failure": func(workflow *unstructured.Unstructured, run *signalv1.CertificationRun) {
			node := groupedNode(workflow, run, 5)
			setNodeParameter(node, "cleanup-failure", "BackendCleanupFailed")
		},
		"malformed evidence": func(workflow *unstructured.Unstructured, run *signalv1.CertificationRun) {
			node := groupedNode(workflow, run, 6)
			setNodeParameter(node, "backend-evidence", "{")
		},
		"skipped": func(workflow *unstructured.Unstructured, run *signalv1.CertificationRun) {
			groupedNode(workflow, run, 7)["phase"] = "Omitted"
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			run := groupedTestRun("fail-closed")
			workflow := groupedWorkflow(run, "Succeeded")
			mutate(workflow, run)

			statuses, verdict, _ := terminalCheckStatuses(workflow, run)
			if verdict != "Inconclusive" {
				t.Fatalf("verdict = %q", verdict)
			}

			found := false

			for index := range statuses {
				if statuses[index].Phase == signalv1.CheckPhaseInconclusive ||
					statuses[index].Phase == signalv1.CheckPhaseSkipped {
					found = true
				}
			}

			if !found {
				t.Fatalf("statuses did not retain fail-closed check state: %#v", statuses)
			}
		})
	}
}

func TestSelectParticipantPlanFindsSatisfiableTopology(t *testing.T) {
	t.Parallel()

	candidates := []corev1.Node{
		participantNode("service", "service", "zone-a"),
		participantNode("witness-a", "witness", "zone-a"),
		participantNode("witness-b", "witness", "zone-a"),
		participantNode("witness-c", "witness", "zone-a"),
		participantNode("witness-d", "witness", "zone-b"),
	}

	participants, err := selectParticipantPlan(
		candidates,
		"service",
		4,
		[]signalv1.ParticipantRoleConstraint{
			{
				Name:     "service",
				Count:    1,
				Selector: metav1.LabelSelector{MatchLabels: map[string]string{"signal.example/role": "service"}},
			},
			{
				Name:     "witness",
				Count:    3,
				Selector: metav1.LabelSelector{MatchLabels: map[string]string{"signal.example/role": "witness"}},
			},
		},
		[]signalv1.ParticipantTopologyConstraint{{
			Role:              "witness",
			LabelKey:          "topology.example/zone",
			MinDistinctValues: 2,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}

	names := make([]string, len(participants))
	for index := range participants {
		names[index] = participants[index].NodeName
	}

	if !slices.Contains(names, "witness-d") {
		t.Fatalf("selected participants = %#v, missing satisfiable zone-b witness", names)
	}
}

func TestSelectParticipantPlanPrioritizesDistinctTopologyDomains(t *testing.T) {
	t.Parallel()

	candidates := []corev1.Node{participantNode("service", "service", "zone-a")}
	for index := 1; index < 63; index++ {
		candidates = append(candidates, participantNode(
			fmt.Sprintf("witness-%02d", index),
			"witness",
			"zone-a",
		))
	}

	candidates = append(candidates, participantNode("witness-63", "witness", "zone-b"))

	participants, err := selectParticipantPlan(
		candidates,
		"service",
		7,
		[]signalv1.ParticipantRoleConstraint{
			{
				Name:     "service",
				Count:    1,
				Selector: metav1.LabelSelector{MatchLabels: map[string]string{"signal.example/role": "service"}},
			},
			{
				Name:     "witness",
				Count:    6,
				Selector: metav1.LabelSelector{MatchLabels: map[string]string{"signal.example/role": "witness"}},
			},
		},
		[]signalv1.ParticipantTopologyConstraint{{
			Role:              "witness",
			LabelKey:          "topology.example/zone",
			MinDistinctValues: 2,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}

	names := make([]string, len(participants))
	for index := range participants {
		names[index] = participants[index].NodeName
	}

	if !slices.Contains(names, "witness-63") {
		t.Fatalf("selected participants = %#v, missing late distinct domain", names)
	}
}

func groupedTestRun(name string) *signalv1.CertificationRun {
	run := testRun(name)
	run.Spec.Script = signalv1.LegacyScriptSnapshot{}
	run.Spec.Backend = signalv1.ExecutionBackend{}
	run.Spec.RunnerImage = ""
	run.Spec.WorkloadPolicy = signalv1.WorkloadPolicy{}
	run.Spec.Parameters = nil
	run.Spec.BackendActiveDeadline = metav1.Duration{}
	run.Spec.MaxAttempts = 0
	run.Spec.ExecutionPolicy = signalv1.SuiteExecutionPolicy{OnFailure: signalv1.SuiteFailurePolicyStop}
	run.Spec.Checks = make([]signalv1.CertificationCheckSnapshot, groupedCheckCount)

	for index := range run.Spec.Checks {
		run.Spec.Checks[index] = signalv1.CertificationCheckSnapshot{
			ID:    signalv1.CheckID(fmt.Sprintf("R%02d", index+1)),
			Order: int32(index),
			Script: signalv1.ScriptSnapshot{
				ConfigMapName: name + "-scripts",
				Key:           fmt.Sprintf("%02d-check", index),
				Digest:        digestString(fmt.Sprintf("check-%d\n", index)),
			},
			RunnerImage:           "runner@sha256:" + stringsRepeat("a", 64),
			Backend:               signalv1.ExecutionBackend{APIVersion: "v1", Kind: "Pod"},
			BackendActiveDeadline: metav1.Duration{Duration: 9 * time.Minute},
			ResultBounds: signalv1.ResultBounds{
				ReasonBytes:           4096,
				MeasurementsBytes:     16384,
				BackendResourcesBytes: 16384,
				BackendEvidenceBytes:  65536,
			},
			MaxAttempts: 1,
		}
	}

	return run
}

func groupedProfileChecks() []signalv1.CertificationCheck {
	checks := make([]signalv1.CertificationCheck, groupedCheckCount)
	for index := range checks {
		checks[index] = signalv1.CertificationCheck{
			ID:    signalv1.CheckID(fmt.Sprintf("R%02d", index+1)),
			Order: int32(index),
			Script: signalv1.ScriptReference{
				Name:   "scripts",
				Key:    fmt.Sprintf("check-%02d.sh", index+1),
				Digest: digestString(fmt.Sprintf("check-%d\n", index)),
			},
			RunnerImage:           "runner@sha256:" + stringsRepeat("a", 64),
			Backend:               signalv1.ExecutionBackend{APIVersion: "v1", Kind: "Pod"},
			BackendActiveDeadline: metav1.Duration{Duration: 9 * time.Minute},
			ResultBounds: signalv1.ResultBounds{
				ReasonBytes:           4096,
				MeasurementsBytes:     16384,
				BackendResourcesBytes: 16384,
				BackendEvidenceBytes:  65536,
			},
			MaxAttempts: 1,
		}
	}

	return checks
}

func groupedWorkflow(run *signalv1.CertificationRun, phase string) *unstructured.Unstructured {
	nodes := make(map[string]any, len(run.Spec.Checks))

	for index := range run.Spec.Checks {
		check := &run.Spec.Checks[index]
		taskName := checkTaskName(check)
		evidence, _ := json.Marshal(map[string]any{
			"version":  "v1",
			"complete": true,
			"participants": []any{map[string]any{
				"participantIndex": 0,
				"resource": map[string]any{
					"apiVersion": "v1",
					"kind":       "Pod",
					"namespace":  run.Namespace,
					"name":       taskName,
					"uid":        "uid-" + taskName,
				},
				"pods": []any{},
			}},
		})
		resources, _ := json.Marshal([]signalv1.BackendResourceReference{{
			APIVersion:         "v1",
			Kind:               "Pod",
			Namespace:          run.Namespace,
			Name:               taskName,
			UID:                types.UID("uid-" + taskName),
			ParticipantIndexes: []int32{0},
		}})
		nodes[string(check.ID)] = map[string]any{
			"name":       run.Name + "-a1." + taskName,
			"phase":      "Succeeded",
			"startedAt":  "2026-10-22T10:00:00Z",
			"finishedAt": "2026-10-22T10:00:01Z",
			"outputs": map[string]any{"parameters": []any{
				map[string]any{"name": "verdict", "value": "Pass"},
				map[string]any{"name": "reason", "value": "Healthy"},
				map[string]any{"name": "measurements", "value": "{}"},
				map[string]any{"name": "backend-resources", "value": string(resources)},
				map[string]any{"name": "backend-evidence", "value": string(evidence)},
				map[string]any{"name": "cleanup-failure", "value": ""},
			}},
		}
	}

	return &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": run.Name + "-a1"},
		"status": map[string]any{
			"phase": phase,
			"nodes": nodes,
		},
	}}
}

func groupedNode(
	workflow *unstructured.Unstructured,
	run *signalv1.CertificationRun,
	index int,
) map[string]any {
	return workflow.Object["status"].(map[string]any)["nodes"].(map[string]any)[string(run.Spec.Checks[index].ID)].(map[string]any)
}

func groupedEvidenceParticipant(
	run *signalv1.CertificationRun,
	checkIndex int,
	participantIndex int,
) map[string]any {
	taskName := checkTaskName(&run.Spec.Checks[checkIndex])

	return map[string]any{
		"participantIndex": participantIndex,
		"resource": map[string]any{
			"apiVersion": "v1",
			"kind":       "Pod",
			"namespace":  run.Namespace,
			"name":       fmt.Sprintf("%s-p%d", taskName, participantIndex),
			"uid":        fmt.Sprintf("uid-%s-p%d", taskName, participantIndex),
		},
		"pods": []any{},
	}
}

func participantNode(name, role, zone string) corev1.Node {
	return corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: name,
		UID:  types.UID(name + "-uid"),
		Labels: map[string]string{
			"signal.example/role":   role,
			"topology.example/zone": zone,
		},
	}}
}

func setNodeParameter(node map[string]any, name, value string) {
	parameters := node["outputs"].(map[string]any)["parameters"].([]any)
	for _, raw := range parameters {
		parameter := raw.(map[string]any)
		if parameter["name"] == name {
			parameter["value"] = value

			return
		}
	}
}

func parameterValues(parameters []any) map[string]string {
	values := make(map[string]string, len(parameters))
	for _, raw := range parameters {
		parameter := raw.(map[string]any)
		values[parameter["name"].(string)] = parameter["value"].(string)
	}

	return values
}

func stringsRepeat(value string, count int) string {
	result := ""
	for range count {
		result += value
	}

	return result
}
