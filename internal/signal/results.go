// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"fmt"
	"maps"
	"sort"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

type testCollection struct {
	results []signalv1.TestResult
	verdict string
	reason  string
	strict  bool
}

type testLeaf struct {
	id              string
	node            map[string]any
	outputs         map[string]string
	invalidOutputs  map[string]bool
	malformed       bool
	inputs          map[string]string
	invalidInputs   map[string]bool
	malformedInputs bool
}

// ObserveTestResults returns provenance-checked results for the current attempt
// without changing the run, Workflow, or evidence.
// Input identity locates live tasks but never replaces completed output provenance.
func ObserveTestResults(
	run *signalv1.CertificationRun,
	workflow *unstructured.Unstructured,
) []signalv1.TestResult {
	if run == nil {
		return nil
	}

	phase := workflowPhase(workflow)
	terminal := phase == "Succeeded" || phase == "Failed" || phase == "Error"

	return collectTestResults(run, workflow, terminal).results
}

func collectTestResults(
	run *signalv1.CertificationRun,
	workflow *unstructured.Unstructured,
	terminal bool,
) testCollection {
	plan, strict, err := effectiveTestPlan(run)

	collection := testCollection{strict: strict, verdict: "Inconclusive"}
	if !strict {
		return collection
	}

	provenanceError := workflowProvenanceError(run, workflow)
	if err != nil {
		provenanceError = "InvalidTestPlan: " + err.Error()
	}

	nodes, nodesErr := workflowNodes(workflow)
	if nodesErr != nil {
		provenanceError = nodesErr.Error()
	}

	leaves := resultLeaves(nodes)
	safety := map[string]testCollection{}

	for _, invocation := range plan {
		result := newTestResult(run, workflow, invocation, terminal)

		if provenanceError != "" {
			result.Reason = provenanceError
			if nodesErr != nil {
				result.ExecutionState = "Unknown"
			}
		} else {
			result = collectInvocation(run, result, leaves, nodes, terminal)
		}

		collection.results = append(collection.results, result)

		if invocation.ContainerName != "" {
			if _, ok := safety[invocation.NodeName]; !ok {
				safety[invocation.NodeName] = collectBurnSafety(run, invocation, leaves)
			}
		}
	}

	collection.verdict, collection.reason = aggregateTestResults(collection.results)
	if provenanceError != "" {
		collection.verdict = "Inconclusive"
		collection.reason = provenanceError

		return collection
	}

	for _, invocation := range plan {
		group, ok := safety[invocation.NodeName]
		if !ok {
			continue
		}

		if group.verdict == "Fail" && collection.verdict != "Fail" {
			collection.verdict = "Fail"
			collection.reason = invocation.NodeName + "/mixed-burn: " + group.reason
		} else if group.verdict != "Pass" && collection.verdict == "Pass" {
			collection.verdict = "Inconclusive"
			collection.reason = invocation.NodeName + "/mixed-burn: " + group.reason
		}

		delete(safety, invocation.NodeName)
	}

	if terminal && collection.verdict == "Pass" && workflowPhase(workflow) != "Succeeded" {
		collection.verdict = "Inconclusive"
		collection.reason = "Workflow execution did not succeed"
	} else if terminal && collection.verdict == "Pass" {
		if reason := workflowNodeExecutionError(nodes); reason != "" {
			collection.verdict = "Inconclusive"
			collection.reason = reason
		}
	}

	return collection
}

func newTestResult(
	run *signalv1.CertificationRun,
	workflow *unstructured.Unstructured,
	invocation signalv1.TestInvocation,
	terminal bool,
) signalv1.TestResult {
	invocation.Parameters = maps.Clone(invocation.Parameters)

	result := signalv1.TestResult{
		TestInvocation: invocation,
		WorkflowName:   run.Status.Workflow.Name,
		WorkflowUID:    run.Status.Workflow.UID,
		Attempt:        run.Status.Workflow.Attempt,
		ScriptDigest:   run.Spec.Script.Digest,
		ExecutionState: "Pending",
		Verdict:        "Inconclusive",
		Reason:         "TestNotStarted",
	}
	if workflow != nil {
		result.WorkflowName = workflow.GetName()
		result.WorkflowUID = string(workflow.GetUID())
	}

	if terminal {
		result.ExecutionState = "Omitted"
		result.Reason = "MissingTestResult"
	}

	return result
}

func collectInvocation(
	run *signalv1.CertificationRun,
	result signalv1.TestResult,
	leaves []testLeaf,
	nodes map[string]any,
	terminal bool,
) signalv1.TestResult {
	testID := result.ID
	if result.ContainerName != "" {
		testID = "mixed-burn"
	}

	leaf, reason := findTestLeaf(leaves, testID, result.NodeName)
	if reason != "" {
		if terminal || reason != "MissingTestResult" {
			result.Reason = reason
		}

		return result
	}

	result.WorkflowNodeID = leaf.id
	result.WorkflowNodeName = nodeString(leaf.node, "name")
	result.TaskName = nodeString(leaf.node, "displayName")

	result.ExecutionState = nodePhase(leaf.node)
	if result.ContainerName != "" {
		result.ExecutionState = "Unknown"
	}

	outputReason := leafProvenanceError(run, result.TestInvocation, leaf, testID)

	inputReason := leafInputProvenanceError(run, result.TestInvocation, leaf, testID, true)
	if outputReason != "" && inputReason != "" {
		result.Reason = outputReason

		result.ExecutionState = "Unknown"
		if !hasOutputProvenance(leaf) {
			result.Reason = inputReason
		}

		return result
	}

	if outputReason == "" {
		if reason := leafInputProvenanceError(run, result.TestInvocation, leaf, testID, false); reason != "" {
			result.Reason = reason
			result.ExecutionState = "Unknown"

			return result
		}
	}

	prefix := ""
	if result.ContainerName != "" {
		prefix = result.ID + "-"

		result.ExecutionState = "Pending"
		if terminal {
			result.ExecutionState = "Omitted"
		}

		childID, child, reason := findContainerNode(leaf, nodes, result.ContainerName)
		if reason != "" {
			result.Reason = reason
			if reason != "MissingContainerNode" {
				result.ExecutionState = "Unknown"
			}

			return result
		}

		result.ContainerNodeID = childID
		result.ExecutionState = nodePhase(child)
	}

	if outputReason != "" {
		result.Reason = outputReason
		if (nodePhase(leaf.node) == "Pending" || nodePhase(leaf.node) == "Running") && !hasOutputProvenance(leaf) {
			result.Reason = "TestOutputsPending"
		}

		return result
	}

	result.Verdict = leaf.outputs[prefix+"verdict"]
	result.Reason = leaf.outputs[prefix+"reason"]
	measurements := leaf.outputs[prefix+"measurements"]
	result.Measurements = boundedTestText(measurements, MaxTestMeasurementBytes)

	if leaf.malformed || leaf.invalidOutputs[prefix+"verdict"] || leaf.invalidOutputs[prefix+"reason"] ||
		leaf.invalidOutputs[prefix+"measurements"] {
		invalidateTestResult(&result, "MalformedTestOutputs")
	}

	switch result.Verdict {
	case "Pass", "Fail", "Suspect", "Inconclusive":
	default:
		invalidateTestResult(&result, "MissingOrInvalidVerdict")
	}

	if strings.TrimSpace(result.Reason) == "" {
		invalidateTestResult(&result, "MissingReason")
	}

	if strings.TrimSpace(measurements) == "" {
		invalidateTestResult(&result, "MissingMeasurements")
	} else if len(measurements) > MaxTestMeasurementBytes {
		invalidateTestResult(&result, "MeasurementsTooLarge")
	}

	if result.ExecutionState != "Succeeded" {
		invalidateTestResult(&result, "Execution"+result.ExecutionState)
	}

	return result
}

func invalidateTestResult(result *signalv1.TestResult, reason string) {
	if result.Verdict != "Fail" && result.Verdict != "Suspect" {
		result.Verdict = "Inconclusive"
	}

	if result.Reason == "" {
		result.Reason = reason
	} else {
		result.Reason = reason + ": " + result.Reason
	}
}

func workflowProvenanceError(run *signalv1.CertificationRun, workflow *unstructured.Unstructured) string {
	if workflow == nil {
		return "WorkflowUnavailable"
	}

	if workflow.GetName() != run.Status.Workflow.Name || workflow.GetNamespace() != run.Namespace ||
		!metav1.IsControlledBy(workflow, run) {
		return "WorkflowOwnershipMismatch"
	}

	if run.Status.Workflow.UID != "" && string(workflow.GetUID()) != run.Status.Workflow.UID {
		return "WorkflowUIDMismatch"
	}

	return ""
}

func leafProvenanceError(
	run *signalv1.CertificationRun,
	invocation signalv1.TestInvocation,
	leaf testLeaf,
	testID string,
) string {
	if reason := leafExecutionIdentityError(invocation, leaf); reason != "" {
		return reason
	}

	expected := testProvenance(run, invocation, testID)

	for _, name := range []string{"test-id", "node-name", "node-uid", "attempt", "script-digest"} {
		if leaf.invalidOutputs[name] {
			return "MalformedTestOutputs: " + name
		}

		if leaf.outputs[name] == "" || leaf.outputs[name] != expected[name] {
			return "TestProvenanceMismatch: " + name
		}
	}

	return ""
}

func leafExecutionIdentityError(invocation signalv1.TestInvocation, leaf testLeaf) string {
	if id, ok := leaf.node["id"].(string); ok && id != leaf.id {
		return "WorkflowNodeIdentityMismatch"
	}

	if name, ok := leaf.node["name"].(string); !ok || name == "" {
		return "WorkflowNodeIdentityMismatch"
	}

	// Argo resource templates also use Pod nodes, but their executor can run
	// away from the participant targeted by the MPI backend.
	if leaf.node["type"] == "Pod" && invocation.ID != "mpi-backend" && invocation.ID != "mpi-rank-access" {
		if host, ok := leaf.node["hostNodeName"].(string); ok && host != "" && host != invocation.NodeName {
			return "TestProvenanceMismatch: hostNodeName"
		}
	}

	return ""
}

func leafInputProvenanceError(
	run *signalv1.CertificationRun,
	invocation signalv1.TestInvocation,
	leaf testLeaf,
	testID string,
	required bool,
) string {
	if reason := leafExecutionIdentityError(invocation, leaf); reason != "" {
		return reason
	}

	if required && leaf.malformedInputs {
		return "MalformedTestInputs"
	}

	expected := testProvenance(run, invocation, testID)

	for _, name := range []string{"test-id", "node-name", "node-uid", "attempt", "script-digest"} {
		value, found := leaf.inputs[name]

		identityRequired := required && (name == "test-id" || name == "node-name" || name == "node-uid")
		if leaf.invalidInputs[name] || identityRequired && !found || found && value != expected[name] {
			return "TestInputProvenanceMismatch: " + name
		}
	}

	return ""
}

func testProvenance(
	run *signalv1.CertificationRun,
	invocation signalv1.TestInvocation,
	testID string,
) map[string]string {
	return map[string]string{
		"test-id":       testID,
		"node-name":     invocation.NodeName,
		"node-uid":      string(invocation.NodeUID),
		"attempt":       strconv.FormatInt(int64(run.Status.Workflow.Attempt), 10),
		"script-digest": run.Spec.Script.Digest,
	}
}

func hasOutputProvenance(leaf testLeaf) bool {
	for _, name := range []string{"test-id", "node-name", "node-uid", "attempt", "script-digest"} {
		if _, found := leaf.outputs[name]; found || leaf.invalidOutputs[name] {
			return true
		}
	}

	return false
}

func resultLeaves(nodes map[string]any) []testLeaf {
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}

	sort.Strings(ids)

	leaves := make([]testLeaf, 0, len(ids))
	for _, id := range ids {
		node, ok := nodes[id].(map[string]any)
		if !ok || node["type"] != "Pod" && node["type"] != "Resource" {
			continue
		}

		outputs, invalid, malformed := parseTestNodeOutputs(node)
		inputs, invalidInputs, malformedInputs := parseNodeParameters(node, "inputs")
		leaves = append(leaves, testLeaf{
			id: id, node: node, outputs: outputs, invalidOutputs: invalid, malformed: malformed,
			inputs: inputs, invalidInputs: invalidInputs, malformedInputs: malformedInputs,
		})
	}

	return leaves
}

func testNodeOutputs(node map[string]any) (map[string]string, bool) {
	outputs, invalid, malformed := parseTestNodeOutputs(node)

	return outputs, malformed || len(invalid) != 0
}

func parseTestNodeOutputs(node map[string]any) (map[string]string, map[string]bool, bool) {
	return parseNodeParameters(node, "outputs")
}

func parseNodeParameters(node map[string]any, field string) (map[string]string, map[string]bool, bool) {
	outputs := map[string]string{}
	invalid := map[string]bool{}

	parameters, found, err := unstructured.NestedSlice(node, field, "parameters")
	if err != nil || !found {
		return outputs, invalid, true
	}

	malformed := false

	for _, raw := range parameters {
		parameter, ok := raw.(map[string]any)
		if !ok {
			malformed = true

			continue
		}

		name, nameOK := parameter["name"].(string)
		if !nameOK || name == "" {
			malformed = true

			continue
		}

		value, valueOK := parameter["value"].(string)
		if _, duplicate := outputs[name]; duplicate || !valueOK {
			invalid[name] = true

			continue
		}

		outputs[name] = value
	}

	return outputs, invalid, malformed
}

func findTestLeaf(leaves []testLeaf, testID, nodeName string) (testLeaf, string) {
	var matches []testLeaf

	for _, leaf := range leaves {
		if leaf.outputs["test-id"] == testID && leaf.outputs["node-name"] == nodeName ||
			leaf.inputs["test-id"] == testID && leaf.inputs["node-name"] == nodeName {
			matches = append(matches, leaf)
		}
	}

	switch len(matches) {
	case 0:
		return testLeaf{}, "MissingTestResult"
	case 1:
		return matches[0], ""
	default:
		return testLeaf{}, "DuplicateTestResult"
	}
}

func findContainerNode(leaf testLeaf, nodes map[string]any, name string) (string, map[string]any, string) {
	children, ok := leaf.node["children"].([]any)
	if !ok {
		return "", nil, "MissingContainerNode"
	}

	parentName := nodeString(leaf.node, "name")
	expectedName := parentName + "." + name

	var (
		foundID string
		found   map[string]any
	)

	for _, raw := range children {
		id, ok := raw.(string)
		if !ok {
			continue
		}

		child, ok := nodes[id].(map[string]any)
		if !ok || child["type"] != "Container" {
			continue
		}

		nodeName := nodeString(child, "name")
		if nodeName != expectedName {
			continue
		}

		if boundary := nodeString(child, "boundaryID"); boundary != leaf.id {
			return "", nil, "ContainerBoundaryMismatch"
		}

		if actualID, ok := child["id"].(string); !ok || actualID != id {
			return "", nil, "ContainerNodeIdentityMismatch"
		}

		if found != nil {
			return "", nil, "DuplicateContainerNode"
		}

		foundID, found = id, child
	}

	if found == nil {
		return "", nil, "MissingContainerNode"
	}

	return foundID, found, ""
}

func collectBurnSafety(
	run *signalv1.CertificationRun,
	invocation signalv1.TestInvocation,
	leaves []testLeaf,
) testCollection {
	safety := testCollection{verdict: "Inconclusive"}

	leaf, reason := findTestLeaf(leaves, "mixed-burn", invocation.NodeName)
	if reason != "" {
		safety.reason = reason

		return safety
	}

	if reason := leafProvenanceError(run, invocation, leaf, "mixed-burn"); reason != "" {
		safety.reason = reason

		return safety
	}

	safety.reason = leaf.outputs["reason"]
	switch leaf.outputs["verdict"] {
	case "Fail", "Suspect":
		safety.verdict = "Fail"
	case "Pass":
		if nodePhase(leaf.node) == "Succeeded" {
			safety.verdict = "Pass"
		}
	case "Inconclusive":
	default:
		safety.reason = "MissingOrInvalidSafetyVerdict"
	}

	if leaf.malformed || leaf.invalidOutputs["verdict"] || leaf.invalidOutputs["reason"] {
		if safety.verdict != "Fail" {
			safety.verdict = "Inconclusive"
		}

		safety.reason = "MalformedSafetyOutputs: " + safety.reason
	}

	if strings.TrimSpace(leaf.outputs["reason"]) == "" {
		if safety.verdict != "Fail" {
			safety.verdict = "Inconclusive"
		}

		safety.reason = "MissingSafetyReason"
	}

	return safety
}

func aggregateTestResults(results []signalv1.TestResult) (string, string) {
	verdict := "Pass"
	reason := "AllPlannedTestsPassed"

	if len(results) == 0 {
		return "Inconclusive", "MissingTestPlan"
	}

	for _, result := range results {
		switch result.Verdict {
		case "Fail", "Suspect":
			return "Fail", fmt.Sprintf("%s/%s: %s", result.NodeName, result.ID, result.Reason)
		case "Pass":
		default:
			if verdict == "Pass" {
				verdict = "Inconclusive"
				reason = fmt.Sprintf("%s/%s: %s", result.NodeName, result.ID, result.Reason)
			}
		}
	}

	return verdict, reason
}

func mergeTestResults(existing, current []signalv1.TestResult, attempt int32) []signalv1.TestResult {
	if len(existing) == 0 && len(current) == 0 {
		return nil
	}

	merged := make([]signalv1.TestResult, 0, len(existing)+len(current))
	for _, result := range existing {
		if result.Attempt != attempt {
			merged = append(merged, result)
		}
	}

	return append(merged, current...)
}

func nodePhase(node map[string]any) string {
	phase := nodeString(node, "phase")
	if phase == "" {
		return "Unknown"
	}

	return phase
}

func nodeString(node map[string]any, key string) string {
	value, ok := node[key].(string)
	if !ok {
		return ""
	}

	return value
}

func workflowNodeExecutionError(nodes map[string]any) string {
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}

	sort.Strings(ids)

	for _, id := range ids {
		node, ok := nodes[id].(map[string]any)
		if !ok {
			return "MalformedWorkflowNode: " + id
		}

		switch phase := nodePhase(node); phase {
		case "Succeeded", "Skipped", "Omitted":
		default:
			return "WorkflowNodeExecution" + phase + ": " + id
		}
	}

	return ""
}

func workflowPhase(workflow *unstructured.Unstructured) string {
	if workflow == nil {
		return ""
	}

	phase, _, err := unstructured.NestedString(workflow.Object, "status", "phase")
	if err != nil {
		return ""
	}

	return phase
}

func boundedTestText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}

	return strings.ToValidUTF8(value[:limit], "")
}
