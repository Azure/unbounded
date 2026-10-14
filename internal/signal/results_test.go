// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

func TestStrictBuiltinResultsRequireActualPlannedLeaves(t *testing.T) {
	t.Parallel()

	for _, workflowName := range []string{GPUCertificationWorkflow, FullNodeCertificationWorkflow, BurnCanaryWorkflow} {
		t.Run(workflowName, func(t *testing.T) {
			t.Parallel()

			run := builtinTestRun(t, workflowName)
			workflow := builtinWorkflow(run, "Succeeded")

			collection := collectTestResults(run, workflow, true)
			if !collection.strict || collection.verdict != "Pass" || len(collection.results) != len(run.Spec.TestPlan) {
				t.Fatalf("collection = %#v", collection)
			}

			containers := map[string]bool{}

			for _, result := range collection.results {
				if result.Verdict != "Pass" || result.ExecutionState != "Succeeded" ||
					result.NodeUID != run.Spec.Participants[0].NodeUID || result.Attempt != 1 ||
					result.ScriptDigest != run.Spec.Script.Digest || result.WorkflowUID != run.Status.Workflow.UID ||
					result.WorkflowNodeID == "" || result.WorkflowNodeName == "" || result.TaskName == "" ||
					result.Measurements == "" || !reflect.DeepEqual(result.Parameters, run.Spec.Parameters) {
					t.Fatalf("result = %#v", result)
				}

				if result.ContainerName != "" {
					if result.ContainerNodeID == "" || containers[result.ContainerNodeID] {
						t.Fatalf("container identity is missing or shared: %#v", result)
					}

					containers[result.ContainerNodeID] = true
				}
			}

			workflow.Object["status"].(map[string]any)["nodes"] = map[string]any{}

			collection = collectTestResults(run, workflow, true)
			if collection.verdict != "Inconclusive" {
				t.Fatalf("root-only outputs certified builtin: %#v", collection)
			}
		})
	}
}

func TestStrictResultsRejectMissingMalformedAndWrongProvenance(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		change func(*unstructured.Unstructured, string)
	}{
		{"wrong node", func(w *unstructured.Unstructured, id string) { setTestOutput(w, id, "node-name", "other-node") }},
		{"wrong node UID", func(w *unstructured.Unstructured, id string) { setTestOutput(w, id, "node-uid", "replacement") }},
		{"missing node UID", func(w *unstructured.Unstructured, id string) { setTestOutput(w, id, "node-uid", "") }},
		{"wrong attempt", func(w *unstructured.Unstructured, id string) { setTestOutput(w, id, "attempt", "2") }},
		{"malformed attempt", func(w *unstructured.Unstructured, id string) { setTestOutput(w, id, "attempt", int64(1)) }},
		{"wrong script", func(w *unstructured.Unstructured, id string) { setTestOutput(w, id, "script-digest", "sha256:other") }},
		{"missing test identity", func(w *unstructured.Unstructured, id string) { setTestOutput(w, id, "test-id", "") }},
		{"missing verdict", func(w *unstructured.Unstructured, id string) { setTestOutput(w, id, "verdict", "") }},
		{"unknown verdict", func(w *unstructured.Unstructured, id string) { setTestOutput(w, id, "verdict", "Succeeded") }},
		{"missing reason", func(w *unstructured.Unstructured, id string) { setTestOutput(w, id, "reason", "") }},
		{"missing measurements", func(w *unstructured.Unstructured, id string) { setTestOutput(w, id, "measurements", " ") }},
		{"oversized measurements", func(w *unstructured.Unstructured, id string) {
			setTestOutput(w, id, "measurements", strings.Repeat("x", MaxTestMeasurementBytes+1))
		}},
		{"wrong scheduler node", func(w *unstructured.Unstructured, id string) {
			fixtureNodes(w)[id].(map[string]any)["hostNodeName"] = "other-node"
		}},
		{"failed leaf", func(w *unstructured.Unstructured, id string) {
			fixtureNodes(w)[id].(map[string]any)["phase"] = "Failed"
		}},
		{"errored leaf", func(w *unstructured.Unstructured, id string) { fixtureNodes(w)[id].(map[string]any)["phase"] = "Error" }},
		{"skipped leaf", func(w *unstructured.Unstructured, id string) {
			fixtureNodes(w)[id].(map[string]any)["phase"] = "Skipped"
		}},
		{"omitted leaf", func(w *unstructured.Unstructured, id string) {
			fixtureNodes(w)[id].(map[string]any)["phase"] = "Omitted"
		}},
		{"non-leaf outputs", func(w *unstructured.Unstructured, id string) { fixtureNodes(w)[id].(map[string]any)["type"] = "DAG" }},
		{"wrong workflow UID", func(w *unstructured.Unstructured, _ string) { w.SetUID("replacement-workflow") }},
		{"wrong workflow owner", func(w *unstructured.Unstructured, _ string) { w.SetOwnerReferences(nil) }},
		{"wrong workflow namespace", func(w *unstructured.Unstructured, _ string) { w.SetNamespace("other-namespace") }},
		{"wrong node ID", func(w *unstructured.Unstructured, id string) { fixtureNodes(w)[id].(map[string]any)["id"] = "other-id" }},
		{"duplicate leaf", func(w *unstructured.Unstructured, id string) {
			copy := (&unstructured.Unstructured{Object: fixtureNodes(w)[id].(map[string]any)}).DeepCopy().Object
			copy["id"] = id + "-duplicate"
			fixtureNodes(w)[id+"-duplicate"] = copy
		}},
		{"duplicate output", func(w *unstructured.Unstructured, id string) {
			node := fixtureNodes(w)[id].(map[string]any)
			outputs := node["outputs"].(map[string]any)
			outputs["parameters"] = append(outputs["parameters"].([]any), map[string]any{"name": "verdict", "value": "Pass"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			run := builtinTestRun(t, FullNodeCertificationWorkflow)
			workflow := builtinWorkflow(run, "Succeeded")
			id := fixtureLeafID("gpu-0", "preflight")
			tc.change(workflow, id)
			collection := collectTestResults(run, workflow, true)

			if collection.verdict == "Pass" || collection.results[0].Verdict == "Pass" {
				t.Fatalf("invalid result passed: %#v", collection)
			}

			if len(collection.results[0].Measurements) > MaxTestMeasurementBytes {
				t.Fatal("unbounded measurement text in status")
			}
		})
	}
}

func TestPreflightFailureRemainsFailWhenWorkloadsAreSkippedAndFinalHealthPasses(t *testing.T) {
	t.Parallel()

	run := builtinTestRun(t, FullNodeCertificationWorkflow)
	workflow := builtinWorkflow(run, "Succeeded")
	setTestOutput(workflow, fixtureLeafID("gpu-0", "preflight"), "verdict", "Fail")
	setTestOutput(workflow, fixtureLeafID("gpu-0", "preflight"), "reason", "NodeGPUCapacityMismatch")
	delete(fixtureNodes(workflow), fixtureLeafID("gpu-0", "gpu-health"))
	delete(fixtureNodes(workflow), fixtureLeafID("gpu-0", "mpi-rank-access"))

	collection := collectTestResults(run, workflow, true)
	if collection.verdict != "Fail" || !strings.Contains(collection.reason, "NodeGPUCapacityMismatch") ||
		collection.results[1].ExecutionState != "Omitted" || collection.results[3].Verdict != "Pass" {
		t.Fatalf("collection = %#v", collection)
	}
}

func TestBurnComponentsRequireDistinctContainerExecutionsAndSafety(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		change  func(*unstructured.Unstructured, string)
		verdict string
	}{
		{"CPU failure despite main pass", func(w *unstructured.Unstructured, id string) {
			setTestOutput(w, id, "cpu-stress-verdict", "Fail")
			setTestOutput(w, id, "cpu-stress-reason", "CPUComputationError")
			fixtureNodes(w)[fixtureContainerID("gpu-0", "cpu-stress")].(map[string]any)["phase"] = "Failed"
		}, "Fail"},
		{"CPU failure survives malformed GPU evidence", func(w *unstructured.Unstructured, id string) {
			setTestOutput(w, id, "cpu-stress-verdict", "Fail")
			setTestOutput(w, id, "gpu-burn-measurements", int64(1))
		}, "Fail"},
		{"failed parent preserves successful GPU sibling", func(w *unstructured.Unstructured, id string) {
			w.Object["status"].(map[string]any)["phase"] = "Error"
			fixtureNodes(w)[id].(map[string]any)["phase"] = "Failed"
			fixtureNodes(w)[fixtureContainerID("gpu-0", "cpu-stress")].(map[string]any)["phase"] = "Failed"
			setTestOutput(w, id, "cpu-stress-verdict", "Fail")
		}, "Fail"},
		{"memory failure despite main pass", func(w *unstructured.Unstructured, id string) {
			setTestOutput(w, id, "memory-integrity-verdict", "Fail")
		}, "Fail"},
		{"storage failure despite main pass", func(w *unstructured.Unstructured, id string) {
			setTestOutput(w, id, "storage-integrity-verdict", "Fail")
		}, "Fail"},
		{"safety failure despite components pass", func(w *unstructured.Unstructured, id string) {
			setTestOutput(w, id, "verdict", "Fail")
			setTestOutput(w, id, "reason", "ContinuousHealthFailure")
		}, "Fail"},
		{"safety failure survives missing safety reason", func(w *unstructured.Unstructured, id string) {
			setTestOutput(w, id, "verdict", "Fail")
			setTestOutput(w, id, "reason", "")
		}, "Fail"},
		{"malformed sibling retains GPU result", func(w *unstructured.Unstructured, id string) {
			setTestOutput(w, id, "memory-integrity-verdict", int64(1))
		}, "Inconclusive"},
		{"missing safety verdict", func(w *unstructured.Unstructured, id string) {
			setTestOutput(w, id, "verdict", "")
		}, "Inconclusive"},
		{"missing safety reason", func(w *unstructured.Unstructured, id string) {
			setTestOutput(w, id, "reason", "")
		}, "Inconclusive"},
		{"missing CPU result", func(w *unstructured.Unstructured, id string) {
			setTestOutput(w, id, "cpu-stress-verdict", "")
		}, "Inconclusive"},
		{"missing device measurements", func(w *unstructured.Unstructured, id string) {
			setTestOutput(w, id, "gpu-burn-measurements", "")
		}, "Inconclusive"},
		{"missing actual CPU container", func(w *unstructured.Unstructured, _ string) {
			delete(fixtureNodes(w), fixtureContainerID("gpu-0", "cpu-stress"))
		}, "Inconclusive"},
		{"errored actual memory container", func(w *unstructured.Unstructured, _ string) {
			fixtureNodes(w)[fixtureContainerID("gpu-0", "memory-integrity")].(map[string]any)["phase"] = "Error"
		}, "Inconclusive"},
		{"missing container relationship", func(w *unstructured.Unstructured, id string) {
			fixtureNodes(w)[id].(map[string]any)["children"] = []any{}
		}, "Inconclusive"},
		{"container output masquerades as execution", func(w *unstructured.Unstructured, _ string) {
			fixtureNodes(w)[fixtureContainerID("gpu-0", "cpu-stress")].(map[string]any)["type"] = "DAG"
		}, "Inconclusive"},
		{"wrong container boundary", func(w *unstructured.Unstructured, _ string) {
			fixtureNodes(w)[fixtureContainerID("gpu-0", "cpu-stress")].(map[string]any)["boundaryID"] = "other-pod"
		}, "Inconclusive"},
		{"display name cannot forge container identity", func(w *unstructured.Unstructured, _ string) {
			fixtureNodes(w)[fixtureContainerID("gpu-0", "cpu-stress")].(map[string]any)["name"] = "other-pod.cpu-stress"
		}, "Inconclusive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			run := builtinTestRun(t, BurnCanaryWorkflow)
			workflow := builtinWorkflow(run, "Succeeded")
			tc.change(workflow, fixtureLeafID("gpu-0", "mixed-burn"))

			collection := collectTestResults(run, workflow, true)
			if collection.verdict != tc.verdict {
				t.Fatalf("collection = %#v, want %s", collection, tc.verdict)
			}

			if (tc.name == "CPU failure despite main pass" || tc.name == "failed parent preserves successful GPU sibling") &&
				(collection.results[0].Verdict != "Pass" || collection.results[1].Verdict != "Fail") {
				t.Fatalf("sibling results were conflated: %#v", collection.results)
			}

			if tc.name == "malformed sibling retains GPU result" && collection.results[0].Verdict != "Pass" {
				t.Fatalf("malformed sibling erased the successful GPU result: %#v", collection.results)
			}
		})
	}
}

func TestMPIResourcePodRetainsParticipantIdentityWithoutRequiringExecutorAffinity(t *testing.T) {
	t.Parallel()

	run := builtinTestRun(t, GPUCertificationWorkflow)
	workflow := builtinWorkflow(run, "Succeeded")
	mpi := fixtureNodes(workflow)[fixtureLeafID("gpu-0", "mpi-backend")].(map[string]any)
	mpi["type"] = "Pod"
	mpi["hostNodeName"] = "workflow-executor-node"

	collection := collectTestResults(run, workflow, true)
	if collection.verdict != "Pass" {
		t.Fatalf("resource executor affinity was treated as participant affinity: %#v", collection)
	}

	setTestOutput(workflow, fixtureLeafID("gpu-0", "mpi-backend"), "node-uid", "wrong-target-uid")

	collection = collectTestResults(run, workflow, true)
	if collection.verdict != "Inconclusive" {
		t.Fatalf("resource target provenance was not checked: %#v", collection)
	}
}

func TestGroupedResultsStayBoundToEachParticipantAndAggregateFailures(t *testing.T) {
	t.Parallel()

	run := builtinTestRun(t, GPUCertificationWorkflow)
	run.Spec.Workflow.Name = RackCertificationWorkflow
	run.Spec.Parameters[NodeWorkflowTemplateParameter] = GPUCertificationWorkflow
	run.Spec.Participants = append(run.Spec.Participants, signalv1.Participant{NodeName: "gpu-1", NodeUID: "gpu-1-uid"})
	run.Spec.ParticipantPlan = append(run.Spec.ParticipantPlan, signalv1.ParticipantSnapshot{
		NodeName: "gpu-1", NodeUID: "gpu-1-uid", ObservedGPUCapacity: "8", ObservedGPUAllocatable: "8",
	})
	run.Spec.TestPlan = nil

	plan, _, err := makeTestPlan(&run.Spec)
	if err != nil {
		t.Fatal(err)
	}

	run.Spec.TestPlan = plan
	workflow := builtinWorkflow(run, "Succeeded")

	collection := collectTestResults(run, workflow, true)
	if collection.verdict != "Pass" || len(collection.results) != 4 {
		t.Fatalf("collection = %#v", collection)
	}

	setTestOutput(workflow, fixtureLeafID("gpu-1", "mpi-backend"), "verdict", "Fail")
	setTestOutput(workflow, fixtureLeafID("gpu-1", "mpi-backend"), "reason", "PeerGPUUnavailable")

	collection = collectTestResults(run, workflow, true)
	if collection.verdict != "Fail" || !strings.Contains(collection.reason, "gpu-1/mpi-backend") ||
		collection.results[0].Verdict != "Pass" {
		t.Fatalf("collection = %#v", collection)
	}

	setTestOutput(workflow, fixtureLeafID("gpu-1", "mpi-backend"), "node-uid", "gpu-0-uid")

	collection = collectTestResults(run, workflow, true)
	if collection.verdict != "Inconclusive" {
		t.Fatalf("wrong participant provenance was trusted: %#v", collection)
	}
}

func TestRunningAndInterruptedResultsNeverInferPassFromUnfinishedExecution(t *testing.T) {
	t.Parallel()

	run := builtinTestRun(t, BurnCanaryWorkflow)
	workflow := builtinWorkflow(run, "Running")
	fixtureNodes(workflow)[fixtureContainerID("gpu-0", "cpu-stress")].(map[string]any)["phase"] = "Running"

	collection := collectTestResults(run, workflow, false)
	if collection.results[1].ExecutionState != "Running" || collection.results[1].Verdict == "Pass" {
		t.Fatalf("collection = %#v", collection)
	}

	delete(fixtureNodes(workflow), fixtureLeafID("gpu-0", "mixed-burn"))

	collection = collectTestResults(run, workflow, false)
	if collection.results[0].ExecutionState != "Pending" {
		t.Fatalf("pending result = %#v", collection.results[0])
	}

	collection = collectTestResults(run, workflow, true)
	if collection.verdict != "Inconclusive" || collection.results[0].ExecutionState != "Omitted" {
		t.Fatalf("interrupted result = %#v", collection)
	}
}

func builtinWorkflow(run *signalv1.CertificationRun, phase string) *unstructured.Unstructured {
	workflow := workflowObject(run.Namespace, run.Status.Workflow.Name, phase, "Pass", "RootClaimsPass")
	workflow.SetUID(types.UID(run.Status.Workflow.UID))
	workflow.SetOwnerReferences([]metav1.OwnerReference{*metav1.NewControllerRef(run, signalv1.GroupVersion.WithKind("CertificationRun"))})

	nodes := map[string]any{}

	for _, invocation := range run.Spec.TestPlan {
		id := invocation.ID
		if invocation.ContainerName != "" {
			id = "mixed-burn"
		}

		nodeID := fixtureLeafID(invocation.NodeName, id)
		if _, exists := nodes[nodeID]; !exists {
			node := map[string]any{
				"id":           nodeID,
				"name":         workflow.GetName() + "." + nodeID,
				"displayName":  id,
				"type":         "Pod",
				"phase":        "Succeeded",
				"hostNodeName": invocation.NodeName,
				"inputs": map[string]any{"parameters": []any{
					map[string]any{"name": "gpu-count", "value": "8"},
					map[string]any{"name": "test-id", "value": id},
					map[string]any{"name": "node-name", "value": invocation.NodeName},
					map[string]any{"name": "node-uid", "value": string(invocation.NodeUID)},
				}},
				"outputs": map[string]any{"parameters": []any{
					map[string]any{"name": "test-id", "value": id},
					map[string]any{"name": "node-name", "value": invocation.NodeName},
					map[string]any{"name": "node-uid", "value": string(invocation.NodeUID)},
					map[string]any{"name": "attempt", "value": fmt.Sprintf("%d", run.Status.Workflow.Attempt)},
					map[string]any{"name": "script-digest", "value": run.Spec.Script.Digest},
					map[string]any{"name": "verdict", "value": "Pass"},
					map[string]any{"name": "reason", "value": "Healthy"},
					map[string]any{"name": "measurements", "value": "GPU-uuid PCI:0000:01:00.0 baseline measurements"},
					map[string]any{"name": "device-inventory", "value": `[{"uuid":"GPU-uuid","pci":"0000:01:00.0"}]`},
				}},
			}
			if strings.HasPrefix(id, "mpi-") {
				node["type"] = "Resource"
				node["hostNodeName"] = "workflow-executor-node"
			}

			nodes[nodeID] = node
		}

		if invocation.ContainerName != "" {
			node := nodes[nodeID].(map[string]any)
			childID := fixtureContainerID(invocation.NodeName, invocation.ContainerName)

			nodes[childID] = map[string]any{
				"id":          childID,
				"name":        node["name"].(string) + "." + invocation.ContainerName,
				"displayName": invocation.ContainerName,
				"boundaryID":  nodeID,
				"type":        "Container",
				"phase":       "Succeeded",
			}
			if _, ok := node["children"]; !ok {
				node["children"] = []any{}
			}

			node["children"] = append(node["children"].([]any), childID)

			outputs := node["outputs"].(map[string]any)
			for name, value := range map[string]string{
				"verdict":      "Pass",
				"reason":       "Healthy",
				"measurements": "GPU-uuid PCI:0000:01:00.0 " + invocation.ID + " measurements",
			} {
				outputs["parameters"] = append(outputs["parameters"].([]any), map[string]any{
					"name": invocation.ID + "-" + name, "value": value,
				})
			}
		}
	}

	workflow.Object["status"].(map[string]any)["nodes"] = nodes

	return workflow
}

func fixtureLeafID(nodeName, testID string) string {
	return nodeName + "-" + testID
}

func fixtureContainerID(nodeName, containerName string) string {
	return nodeName + "-container-" + containerName
}

func fixtureNodes(workflow *unstructured.Unstructured) map[string]any {
	return workflow.Object["status"].(map[string]any)["nodes"].(map[string]any)
}

func setTestOutput(workflow *unstructured.Unstructured, nodeID, name string, value any) {
	node := fixtureNodes(workflow)[nodeID].(map[string]any)

	parameters := node["outputs"].(map[string]any)["parameters"].([]any)
	for _, raw := range parameters {
		parameter := raw.(map[string]any)
		if parameter["name"] == name {
			parameter["value"] = value

			return
		}
	}

	panic("fixture output not found: " + name)
}
