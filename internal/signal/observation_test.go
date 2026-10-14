// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

func TestObserveTestResultsPreservesNativeLivePhasesWithoutMutation(t *testing.T) {
	t.Parallel()

	run := builtinTestRun(t, FullNodeCertificationWorkflow)
	run.Status.Workflow.Name = run.Name + "-a2"
	run.Status.Workflow.UID = "second-workflow-uid"
	run.Status.Workflow.Attempt = 2
	run.Status.TestResults = []signalv1.TestResult{{
		TestInvocation: *run.Spec.TestPlan[0].DeepCopy(),
		Attempt:        1, ExecutionState: "Succeeded", Verdict: "Pass",
		Evidence: signalv1.EvidenceStatus{URI: "retained-attempt-one"},
	}}
	workflow := builtinWorkflow(run, "Running")
	phases := []string{"Running", "Pending", "Running", "Pending"}

	for i, invocation := range run.Spec.TestPlan {
		node := fixtureNodes(workflow)[fixtureLeafID(invocation.NodeName, invocation.ID)].(map[string]any)
		node["phase"] = phases[i]
		delete(node, "outputs")
	}

	beforeRun, beforeWorkflow := run.DeepCopy(), workflow.DeepCopy()

	results := ObserveTestResults(run, workflow)
	if len(results) != len(phases) {
		t.Fatalf("results = %#v", results)
	}

	for i, result := range results {
		if result.ExecutionState != phases[i] || result.Verdict != "Inconclusive" ||
			result.Reason != "TestOutputsPending" || result.WorkflowNodeID == "" ||
			result.Attempt != 2 || result.WorkflowUID != run.Status.Workflow.UID ||
			result.ScriptDigest != run.Spec.Script.Digest || result.Evidence.URI != "" {
			t.Fatalf("live result = %#v", result)
		}
	}

	results[0].Parameters["configured-mode"] = "caller mutation"

	if !reflect.DeepEqual(run, beforeRun) || !reflect.DeepEqual(workflow, beforeWorkflow) {
		t.Fatal("observation or mutation of its result changed the run, retained evidence, or Workflow")
	}
}

func TestObserveTestResultsBindsConcurrentContainerPhasesBeforeOutputsExist(t *testing.T) {
	t.Parallel()

	for _, phases := range [][]string{
		{"Running", "Running", "Running", "Running"},
		{"Running", "Running", "Pending", "Succeeded"},
	} {
		t.Run(phases[2], func(t *testing.T) {
			t.Parallel()

			run := builtinTestRun(t, BurnCanaryWorkflow)
			workflow := builtinWorkflow(run, "Running")
			parent := fixtureNodes(workflow)[fixtureLeafID("gpu-0", "mixed-burn")].(map[string]any)
			parent["phase"] = "Running"
			delete(parent, "outputs")

			for i, invocation := range run.Spec.TestPlan {
				child := fixtureNodes(workflow)[fixtureContainerID(invocation.NodeName, invocation.ContainerName)].(map[string]any)
				child["phase"] = phases[i]
			}

			beforeRun, beforeWorkflow := run.DeepCopy(), workflow.DeepCopy()

			results := ObserveTestResults(run, workflow)
			if len(results) != 4 {
				t.Fatalf("results = %#v", results)
			}

			for i, result := range results {
				if result.ExecutionState != phases[i] || result.Verdict != "Inconclusive" ||
					result.Reason != "TestOutputsPending" ||
					result.ContainerNodeID != fixtureContainerID(result.NodeName, result.ContainerName) ||
					result.WorkflowNodeID != fixtureLeafID(result.NodeName, "mixed-burn") {
					t.Fatalf("container result = %#v", result)
				}
			}

			if !reflect.DeepEqual(run, beforeRun) || !reflect.DeepEqual(workflow, beforeWorkflow) {
				t.Fatal("live container observation mutated its inputs")
			}

			compressFixtureNodes(t, workflow)

			beforeCompressed := workflow.DeepCopy()
			if compressed := ObserveTestResults(run, workflow); !reflect.DeepEqual(compressed, results) {
				t.Fatalf("compressed live phases changed: %#v", compressed)
			}

			if !reflect.DeepEqual(workflow, beforeCompressed) {
				t.Fatal("compressed live observation mutated its Workflow")
			}
		})
	}
}

func TestInputIdentityNeverReplacesCompletedOutputProvenance(t *testing.T) {
	t.Parallel()

	for _, workflowName := range []string{FullNodeCertificationWorkflow, BurnCanaryWorkflow} {
		t.Run(workflowName, func(t *testing.T) {
			t.Parallel()

			for _, phase := range []string{"Pending", "Running", "Succeeded", "Failed", "Error", "Skipped", "Omitted"} {
				t.Run(phase, func(t *testing.T) {
					t.Parallel()

					run := builtinTestRun(t, workflowName)
					workflow := builtinWorkflow(run, "Succeeded")

					id := run.Spec.TestPlan[0].ID
					if workflowName == BurnCanaryWorkflow {
						id = "mixed-burn"
					}

					parent := fixtureNodes(workflow)[fixtureLeafID("gpu-0", id)].(map[string]any)
					parent["phase"] = phase
					delete(parent, "outputs")

					expectedPhase := phase
					if workflowName == BurnCanaryWorkflow {
						expectedPhase = "Succeeded"
					}

					results := ObserveTestResults(run, workflow)
					if results[0].ExecutionState != expectedPhase || results[0].Verdict != "Inconclusive" ||
						collectTestResults(run, workflow, true).verdict == "Pass" {
						t.Fatalf("input provenance certified a missing result: %#v", results)
					}
				})
			}
		})
	}
}

func TestLiveInputsRequireMatchingParticipantAndOptionalAttemptProvenance(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		field string
		value any
	}{
		{"missing node UID", "node-uid", ""},
		{"wrong node UID", "node-uid", "replacement-uid"},
		{"malformed node UID", "node-uid", int64(1)},
		{"wrong node", "node-name", "other-node"},
		{"wrong test", "test-id", "unplanned-test"},
		{"wrong attempt", "attempt", "2"},
		{"wrong script", "script-digest", "sha256:other-script"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			for _, workflowName := range []string{GPUCertificationWorkflow, BurnCanaryWorkflow} {
				run := builtinTestRun(t, workflowName)
				workflow := builtinWorkflow(run, "Running")

				testID := "gpu-identity"
				if workflowName == BurnCanaryWorkflow {
					testID = "mixed-burn"
				}

				id := fixtureLeafID("gpu-0", testID)
				parent := fixtureNodes(workflow)[id].(map[string]any)
				parent["phase"] = "Running"
				delete(parent, "outputs")
				setTestInput(workflow, id, tc.field, tc.value)

				results := ObserveTestResults(run, workflow)
				if workflowName != BurnCanaryWorkflow {
					results = results[:1]
				}

				for _, result := range results {
					if result.Verdict == "Pass" || result.ContainerNodeID != "" || result.ExecutionState == "Running" {
						t.Fatalf("unverified inputs bound a live execution: %#v", result)
					}
				}
			}
		})
	}
}

func TestCompletedOutputsMustNotContradictDeclaredInputs(t *testing.T) {
	t.Parallel()

	run := builtinTestRun(t, FullNodeCertificationWorkflow)
	workflow := builtinWorkflow(run, "Succeeded")
	setTestInput(workflow, fixtureLeafID("gpu-0", "preflight"), "node-uid", "other-node-uid")

	result := ObserveTestResults(run, workflow)[0]
	if result.Verdict != "Inconclusive" || result.Reason != "TestInputProvenanceMismatch: node-uid" {
		t.Fatalf("contradictory input/output identity passed: %#v", result)
	}

	setTestInput(workflow, fixtureLeafID("gpu-0", "preflight"), "node-uid", "gpu-0-uid")
	setTestOutput(workflow, fixtureLeafID("gpu-0", "preflight"), "attempt", "2")

	result = ObserveTestResults(run, workflow)[0]
	if result.Verdict != "Inconclusive" || result.Reason != "TestProvenanceMismatch: attempt" {
		t.Fatalf("valid inputs replaced wrong completed output provenance: %#v", result)
	}
}

func TestLiveContainerResultsNeverInheritPodPhaseWithoutChildIdentity(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"missing child", "wrong boundary", "wrong child ID"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			run := builtinTestRun(t, BurnCanaryWorkflow)
			workflow := builtinWorkflow(run, "Running")
			parent := fixtureNodes(workflow)[fixtureLeafID("gpu-0", "mixed-burn")].(map[string]any)
			parent["phase"] = "Running"
			delete(parent, "outputs")

			for _, invocation := range run.Spec.TestPlan {
				fixtureNodes(workflow)[fixtureContainerID("gpu-0", invocation.ContainerName)].(map[string]any)["phase"] = "Running"
			}

			id := fixtureContainerID("gpu-0", "cpu-stress")

			switch mode {
			case "missing child":
				delete(fixtureNodes(workflow), id)
			case "wrong boundary":
				fixtureNodes(workflow)[id].(map[string]any)["boundaryID"] = "other-pod"
			case "wrong child ID":
				fixtureNodes(workflow)[id].(map[string]any)["id"] = "other-id"
			}

			results := ObserveTestResults(run, workflow)
			if results[1].ContainerNodeID != "" || results[1].ExecutionState == "Running" ||
				results[0].ExecutionState != "Running" || results[2].ExecutionState != "Running" ||
				results[3].ExecutionState != "Running" {
				t.Fatalf("parent phase replaced actual container identity: %#v", results)
			}
		})
	}
}

func TestObserveTestResultsHandlesNilRunAndLegacyWorkflow(t *testing.T) {
	t.Parallel()

	if results := ObserveTestResults(nil, nil); results != nil {
		t.Fatalf("nil run results = %#v", results)
	}

	run := builtinTestRun(t, GPUCertificationWorkflow)

	run.Spec.Workflow.Name = "custom-legacy-workflow"
	if results := ObserveTestResults(run, builtinWorkflow(run, "Succeeded")); results != nil {
		t.Fatalf("unknown custom workflow invented a builtin plan: %#v", results)
	}
}

func setTestInput(workflow *unstructured.Unstructured, nodeID, name string, value any) {
	node := fixtureNodes(workflow)[nodeID].(map[string]any)

	inputs := node["inputs"].(map[string]any)
	for _, raw := range inputs["parameters"].([]any) {
		parameter := raw.(map[string]any)
		if parameter["name"] == name {
			parameter["value"] = value

			return
		}
	}

	inputs["parameters"] = append(inputs["parameters"].([]any), map[string]any{"name": name, "value": value})
}
