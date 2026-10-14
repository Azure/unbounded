// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

const (
	GPUCertificationWorkflow      = "project-signal-gpu-certification"
	FullNodeCertificationWorkflow = "project-signal-gpu-full-node-certification"
	BurnCanaryWorkflow            = "project-signal-gpu-burn-canary"
	RackCertificationWorkflow     = "project-signal-rack-certification"
	NodeWorkflowTemplateParameter = "node-workflow-template"
	ParticipantPlanParameter      = "participant-plan"
	TestPlanParameter             = "test-plan"
	MaxTestMeasurementBytes       = 16 * 1024
)

type testDefinition struct {
	id        string
	container string
}

func nodeWorkflow(workflow string, parameters map[string]string) (string, bool, error) {
	if workflow == RackCertificationWorkflow {
		workflow = parameters[NodeWorkflowTemplateParameter]
		if workflow == "" {
			return "", true, fmt.Errorf("rack workflow requires an explicit %s", NodeWorkflowTemplateParameter)
		}

		if !supportedNodeWorkflow(workflow) {
			return "", true, fmt.Errorf("unsupported node workflow template %q", workflow)
		}
	}

	if !supportedNodeWorkflow(workflow) {
		return "", false, nil
	}

	if err := validateRackTests(parameters["rack-tests"]); err != nil {
		return "", true, err
	}

	return workflow, true, nil
}

func supportedNodeWorkflow(workflow string) bool {
	switch workflow {
	case GPUCertificationWorkflow, FullNodeCertificationWorkflow, BurnCanaryWorkflow:
		return true
	default:
		return false
	}
}

func validateRackTests(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}

	var tests []json.RawMessage
	if !strings.HasPrefix(value, "[") || json.Unmarshal([]byte(value), &tests) != nil || len(tests) != 0 {
		return fmt.Errorf("UnsupportedRackTest: no rack or fabric execution adapter is available")
	}

	return nil
}

func workflowTests(workflow string) []testDefinition {
	switch workflow {
	case GPUCertificationWorkflow:
		return []testDefinition{{id: "gpu-identity"}, {id: "mpi-backend"}}
	case FullNodeCertificationWorkflow:
		return []testDefinition{
			{id: "preflight"},
			{id: "gpu-health"},
			{id: "mpi-rank-access"},
			{id: "health-after"},
		}
	case BurnCanaryWorkflow:
		return []testDefinition{
			{id: "gpu-burn", container: "main"},
			{id: "cpu-stress", container: "cpu-stress"},
			{id: "memory-integrity", container: "memory-integrity"},
			{id: "storage-integrity", container: "storage-integrity"},
		}
	default:
		return nil
	}
}

func participantSnapshot(node *corev1.Node) signalv1.ParticipantSnapshot {
	gpu := corev1.ResourceName("nvidia.com/gpu")
	capacity := node.Status.Capacity[gpu]
	allocatable := node.Status.Allocatable[gpu]

	return signalv1.ParticipantSnapshot{
		NodeName:               node.Name,
		NodeUID:                node.UID,
		ObservedGPUCapacity:    capacity.String(),
		ObservedGPUAllocatable: allocatable.String(),
	}
}

func (r *AdmissionReconciler) snapshotParticipants(
	ctx context.Context,
	participants []signalv1.Participant,
	runName string,
) ([]signalv1.ParticipantSnapshot, error) {
	plan := make([]signalv1.ParticipantSnapshot, 0, len(participants))

	for _, participant := range participants {
		var node corev1.Node
		if err := r.Get(ctx, client.ObjectKey{Name: participant.NodeName}, &node); err != nil {
			return nil, fmt.Errorf("snapshot participant %s: %w", participant.NodeName, err)
		}

		if node.UID != participant.NodeUID || node.Annotations[ReservationLabel] != runName {
			return nil, fmt.Errorf("participant %s identity or reservation changed before snapshot", participant.NodeName)
		}

		plan = append(plan, participantSnapshot(&node))
	}

	return plan, nil
}

func participantParameters(parameters map[string]string, participant signalv1.ParticipantSnapshot) map[string]string {
	parameters = maps.Clone(parameters)
	if parameters == nil {
		parameters = make(map[string]string, 2)
	}

	parameters[ObservedGPUCapacityParameter] = participant.ObservedGPUCapacity
	parameters[ObservedGPUAllocatableParameter] = participant.ObservedGPUAllocatable

	return parameters
}

func makeTestPlan(spec *signalv1.CertificationRunSpec) ([]signalv1.TestInvocation, bool, error) {
	workflow, strict, err := nodeWorkflow(spec.Workflow.Name, spec.Parameters)
	if err != nil || !strict {
		return nil, strict, err
	}

	inventory := make(map[string]signalv1.ParticipantSnapshot, len(spec.ParticipantPlan))
	for _, participant := range spec.ParticipantPlan {
		if _, duplicate := inventory[participant.NodeName]; duplicate {
			return nil, true, fmt.Errorf("duplicate participant snapshot %q", participant.NodeName)
		}

		inventory[participant.NodeName] = participant
	}

	if len(inventory) != 0 && len(inventory) != len(spec.Participants) {
		return nil, true, fmt.Errorf("participant snapshot membership does not match the run")
	}

	if len(spec.Participants) > 1 && len(inventory) == 0 {
		return nil, true, fmt.Errorf("grouped builtin workflows require per-participant inventory snapshots")
	}

	definitions := workflowTests(workflow)
	plan := make([]signalv1.TestInvocation, 0, len(spec.Participants)*len(definitions))
	seen := make(map[string]bool, len(spec.Participants))

	for _, participant := range spec.Participants {
		if participant.NodeName == "" || participant.NodeUID == "" || seen[participant.NodeName] {
			return nil, true, fmt.Errorf("participant names and UIDs must be nonempty and distinct")
		}

		seen[participant.NodeName] = true
		parameters := maps.Clone(spec.Parameters)

		if len(inventory) != 0 {
			snapshot, ok := inventory[participant.NodeName]
			if !ok || snapshot.NodeUID != participant.NodeUID {
				return nil, true, fmt.Errorf("participant snapshot identity does not match %s", participant.NodeName)
			}

			parameters = participantParameters(parameters, snapshot)
		}

		for key := range parameters {
			if reservedWorkflowParameter(key) {
				delete(parameters, key)
			}
		}

		for _, definition := range definitions {
			plan = append(plan, signalv1.TestInvocation{
				ID:            definition.id,
				NodeName:      participant.NodeName,
				NodeUID:       participant.NodeUID,
				ContainerName: definition.container,
				Parameters:    maps.Clone(parameters),
			})
		}
	}

	if len(plan) == 0 {
		return nil, true, fmt.Errorf("builtin workflow requires at least one participant")
	}

	return plan, true, nil
}

func effectiveTestPlan(run *signalv1.CertificationRun) ([]signalv1.TestInvocation, bool, error) {
	plan, strict, err := makeTestPlan(&run.Spec)
	if err != nil || !strict {
		return nil, strict, err
	}

	if len(run.Spec.TestPlan) != 0 && !reflect.DeepEqual(plan, run.Spec.TestPlan) {
		return plan, true, fmt.Errorf("immutable test plan does not match the builtin workflow and participant parameters")
	}

	return plan, true, nil
}

func reservedWorkflowParameter(name string) bool {
	switch name {
	case "run-name", "run-uid", "participants", "script-configmap", "script-key", "script-digest",
		"backend-api-version", "backend-kind", "queue", "runner-image", "tolerations", "attempt",
		ParticipantPlanParameter, TestPlanParameter:
		return true
	default:
		return false
	}
}
