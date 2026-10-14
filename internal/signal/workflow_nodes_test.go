// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestCompressedWorkflowNodesPreserveStrictBuiltinResults(t *testing.T) {
	t.Parallel()

	for _, name := range []string{GPUCertificationWorkflow, FullNodeCertificationWorkflow, BurnCanaryWorkflow} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			run := builtinTestRun(t, name)
			workflow := builtinWorkflow(run, "Succeeded")
			expected := ObserveTestResults(run, workflow)
			encoded := compressFixtureNodes(t, workflow)
			before := workflow.DeepCopy()

			results := ObserveTestResults(run, workflow)

			collection := collectTestResults(run, workflow, true)
			if !reflect.DeepEqual(results, expected) || collection.verdict != "Pass" {
				t.Fatalf("compressed results = %#v, aggregate = %#v", results, collection)
			}

			if !reflect.DeepEqual(workflow, before) {
				t.Fatal("compressed observation mutated its Workflow")
			}

			if workflowNodeStatus(workflow)["compressedNodes"] != encoded {
				t.Fatal("original compressed status marker was lost")
			}
		})
	}
}

func TestUntrustedWorkflowNodeRepresentationsCannotUseCompleteInlinePass(t *testing.T) {
	t.Parallel()

	truncated, err := compressEvidence([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		fields map[string]any
		reason string
	}{
		{"base64", map[string]any{"compressedNodes": "%%%"}, "WorkflowNodesCompressedMalformed"},
		{"gzip", map[string]any{"compressedNodes": base64.StdEncoding.EncodeToString([]byte("not gzip"))}, "WorkflowNodesCompressedMalformed"},
		{"truncated gzip", map[string]any{
			"compressedNodes": base64.StdEncoding.EncodeToString(truncated[:len(truncated)-1]),
		}, "WorkflowNodesCompressedMalformed"},
		{"JSON", map[string]any{"compressedNodes": encodedNodePayload(t, []byte(`{"broken":`))}, "WorkflowNodesCompressedMalformed"},
		{"null", map[string]any{"compressedNodes": encodedNodePayload(t, []byte(`null`))}, "WorkflowNodesCompressedMalformed"},
		{"array", map[string]any{"compressedNodes": encodedNodePayload(t, []byte(`[]`))}, "WorkflowNodesCompressedMalformed"},
		{"non-object node", map[string]any{"compressedNodes": encodedNodePayload(t, []byte(`{"node":1}`))}, "WorkflowNodesMalformed"},
		{"compressed field type", map[string]any{"compressedNodes": int64(1)}, "WorkflowNodesCompressedMalformed"},
		{"offloaded", map[string]any{"offloadNodeStatusVersion": "offload-version"}, "WorkflowNodesOffloaded"},
		{"offload before compressed", map[string]any{
			"offloadNodeStatusVersion": "offload-version", "compressedNodes": encodedNodePayload(t, []byte(`{}`)),
		}, "WorkflowNodesOffloaded"},
		{"offload before malformed compressed", map[string]any{
			"offloadNodeStatusVersion": "offload-version", "compressedNodes": "%%%",
		}, "WorkflowNodesOffloaded"},
		{"offload field type", map[string]any{"offloadNodeStatusVersion": int64(1)}, "WorkflowNodesMalformed"},
		{"inline field type", map[string]any{"nodes": []any{"not a map"}}, "WorkflowNodesMalformed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			run := builtinTestRun(t, GPUCertificationWorkflow)

			workflow := builtinWorkflow(run, "Succeeded")
			for key, value := range tc.fields {
				workflow.Object["status"].(map[string]any)[key] = value
			}

			collection := collectTestResults(run, workflow, true)
			if collection.verdict != "Inconclusive" || !strings.HasPrefix(collection.reason, tc.reason) {
				t.Fatalf("partial inline status was trusted: %#v", collection)
			}

			for _, result := range ObserveTestResults(run, workflow) {
				if result.Verdict != "Inconclusive" || result.ExecutionState != "Unknown" ||
					!strings.HasPrefix(result.Reason, tc.reason) {
					t.Fatalf("untrusted representation passed: %#v", result)
				}

				if tc.reason == "WorkflowNodesOffloaded" && result.Reason != tc.reason {
					t.Fatalf("offload reason = %q", result.Reason)
				}
			}
		})
	}
}

func TestCompressedWorkflowNodesBoundDecompressedSize(t *testing.T) {
	run := builtinTestRun(t, GPUCertificationWorkflow)
	workflow := builtinWorkflow(run, "Succeeded")
	status := workflow.Object["status"].(map[string]any)
	status["compressedNodes"] = encodedNodePayload(t, []byte(strings.Repeat(" ", maxEvidenceBytes+1)))

	collection := collectTestResults(run, workflow, true)
	if collection.verdict != "Inconclusive" || !strings.HasPrefix(collection.reason, "WorkflowNodesTooLarge") {
		t.Fatalf("oversized compressed status used complete inline Pass: %#v", collection)
	}

	status["compressedNodes"] = encodedNodePayload(t, []byte(`{}`+strings.Repeat(" ", maxEvidenceBytes-2)))

	nodes, err := workflowNodes(workflow)
	if err != nil || nodes == nil || len(nodes) != 0 {
		t.Fatalf("exact decompression limit: nodes = %#v, err = %v", nodes, err)
	}
}

func TestCompressedAndOffloadedLegacyNodeFallbackFailsClosed(t *testing.T) {
	t.Parallel()

	run := builtinTestRun(t, GPUCertificationWorkflow)
	workflow := builtinWorkflow(run, "Succeeded")
	delete(workflow.Object["status"].(map[string]any), "outputs")
	compressFixtureNodes(t, workflow)

	if value := workflowParameter(workflow, "verdict"); value != "Pass" {
		t.Fatalf("valid compressed node fallback = %q", value)
	}

	workflow.Object["status"].(map[string]any)["offloadNodeStatusVersion"] = "offloaded"
	if value := workflowParameter(workflow, "verdict"); value != "" {
		t.Fatalf("offloaded node fallback = %q", value)
	}

	delete(workflow.Object["status"].(map[string]any), "offloadNodeStatusVersion")

	workflow.Object["status"].(map[string]any)["compressedNodes"] = "%%%"
	if value := workflowParameter(workflow, "verdict"); value != "" {
		t.Fatalf("malformed compressed node fallback = %q", value)
	}
}

func TestEmptyNodeStatusMarkersKeepInlineBehavior(t *testing.T) {
	t.Parallel()

	run := builtinTestRun(t, GPUCertificationWorkflow)
	workflow := builtinWorkflow(run, "Succeeded")
	status := workflow.Object["status"].(map[string]any)
	status["compressedNodes"] = ""
	status["offloadNodeStatusVersion"] = ""

	if collection := collectTestResults(run, workflow, true); collection.verdict != "Pass" {
		t.Fatalf("empty representation markers invalidated inline nodes: %#v", collection)
	}

	delete(status, "nodes")
	status["phase"] = "Pending"

	for _, result := range ObserveTestResults(run, workflow) {
		if result.ExecutionState != "Pending" || result.Verdict == "Pass" {
			t.Fatalf("empty pending status = %#v", result)
		}
	}
}

func compressFixtureNodes(t *testing.T, workflow *unstructured.Unstructured) string {
	t.Helper()

	data, err := json.Marshal(fixtureNodes(workflow))
	if err != nil {
		t.Fatal(err)
	}

	encoded := encodedNodePayload(t, data)
	status := workflow.Object["status"].(map[string]any)
	status["compressedNodes"] = encoded
	delete(status, "nodes")

	return encoded
}

func encodedNodePayload(t *testing.T, data []byte) string {
	t.Helper()

	compressed, err := compressEvidence(data)
	if err != nil {
		t.Fatal(err)
	}

	return base64.StdEncoding.EncodeToString(compressed)
}
