// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signaldashboard

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
	"github.com/Azure/unbounded/internal/signal"
)

const (
	stagePlanAnnotation = "signal.unbounded-cloud.io/stage-plan"
	stageEventPrefix    = "PROJECT_SIGNAL_STAGE "
	maxStageLogBytes    = int64(64 * 1024)
	maxStageLogLines    = int64(500)
	maxStageLogPods     = 16
)

type stageEvent struct {
	Name    string  `json:"name"`
	Status  string  `json:"status"`
	AtEpoch float64 `json:"at_epoch"`
	Reason  string  `json:"reason,omitempty"`
	Message string  `json:"message,omitempty"`
}

func declaredStagePlan(run *signalv1.CertificationRun) ([]signalv1.CertificationStage, []string) {
	if len(run.Spec.StagePlan) > 0 {
		return append([]signalv1.CertificationStage(nil), run.Spec.StagePlan...), nil
	}

	raw := run.Annotations[stagePlanAnnotation]
	if raw == "" {
		return nil, []string{"No immutable execution stage plan was recorded for this run."}
	}

	var plan []signalv1.CertificationStage
	if err := json.Unmarshal([]byte(raw), &plan); err != nil {
		return nil, []string{"The recorded execution stage plan is invalid."}
	}

	return plan, nil
}

func parseStageEvents(content []byte) []stageEvent {
	events := []stageEvent{}
	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 4096), int(maxStageLogBytes))

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, stageEventPrefix) {
			continue
		}

		var event stageEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, stageEventPrefix)), &event); err != nil ||
			event.Name == "" || event.Status == "" || event.AtEpoch <= 0 {
			continue
		}

		event.Reason = sanitizeDisplay(event.Reason)
		event.Message = sanitizeDisplay(event.Message)
		events = append(events, event)
	}

	return events
}

func reconcileStagePlan(
	plan []signalv1.CertificationStage,
	events []stageEvent,
	terminal bool,
	now time.Time,
) ([]StageStatus, []string, string) {
	stages := make([]StageStatus, len(plan))
	index := map[string]int{}
	issues := []string{}

	for i := range plan {
		stages[i] = StageStatus{
			ID:          plan[i].ID,
			Name:        plan[i].DisplayName,
			Category:    plan[i].Category,
			Description: plan[i].Description,
			Required:    plan[i].Required,
			Conditional: plan[i].Conditional,
			Phase:       "Pending",
			Source:      "CertificationRun stage plan",
		}
		if plan[i].ExpectedDuration.Duration > 0 {
			stages[i].ExpectedDuration = plan[i].ExpectedDuration.Duration.String()
		}

		if _, duplicate := index[plan[i].ID]; duplicate {
			issues = append(issues, fmt.Sprintf("Duplicate declared stage ID %q.", plan[i].ID))
		}

		index[plan[i].ID] = i
	}

	seenStatus := map[string]map[string]bool{}
	highestStarted := -1
	failedAt := -1

	for _, event := range events {
		i, ok := index[event.Name]
		if !ok {
			issues = append(issues, fmt.Sprintf("Runtime emitted unknown stage %q.", event.Name))
			continue
		}

		status := normalizeStagePhase(event.Status)
		if seenStatus[event.Name] == nil {
			seenStatus[event.Name] = map[string]bool{}
		}

		if seenStatus[event.Name][status] {
			issues = append(issues, fmt.Sprintf("Stage %q emitted duplicate %s events.", event.Name, status))
			continue
		}

		seenStatus[event.Name][status] = true

		at := time.UnixMilli(int64(event.AtEpoch * 1000)).UTC()
		stage := &stages[i]

		switch status {
		case "Running":
			if i < highestStarted {
				issues = append(issues, fmt.Sprintf("Stage %q started out of declared order.", event.Name))
			}

			for prior := 0; prior < i; prior++ {
				if stages[prior].Required && stages[prior].Phase == "Pending" {
					issues = append(issues, fmt.Sprintf("Stage %q started before required stage %q completed.", event.Name, stages[prior].ID))
				}
			}

			highestStarted = max(highestStarted, i)
			stage.Phase = status
			stage.StartedAt = optionalTime(at)
		case "Succeeded", "Failed", "Skipped":
			if stage.StartedAt == nil {
				stage.StartedAt = optionalTime(at)
			}

			stage.Phase = status
			stage.FinishedAt = optionalTime(at)

			if status == "Failed" {
				failedAt = i
			}

			if status == "Skipped" && stage.Required && !stage.Conditional {
				issues = append(issues, fmt.Sprintf("Required stage %q was skipped.", event.Name))
			}
		default:
			issues = append(issues, fmt.Sprintf("Stage %q emitted unsupported status %q.", event.Name, event.Status))
			continue
		}

		stage.Reason = event.Reason
		stage.Message = event.Message
		stage.Duration = stageDuration(stage, now)
	}

	if failedAt >= 0 {
		for i := failedAt + 1; i < len(stages); i++ {
			if stages[i].Phase == "Pending" {
				stages[i].Phase = "Skipped"
				stages[i].Reason = "EarlierStageFailed"
				stages[i].Message = "Not run because an earlier required stage failed."
			}
		}
	}

	if terminal {
		for i := range stages {
			if stages[i].Required && (stages[i].Phase == "Pending" || stages[i].Phase == "Running") {
				issues = append(issues, fmt.Sprintf("Terminal run is missing required stage %q.", stages[i].ID))
			}
		}
	}

	current := ""

	for i := range stages {
		stages[i].Duration = stageDuration(&stages[i], now)
		if stages[i].Phase == "Running" {
			current = stages[i].ID
		}
	}

	sort.Strings(issues)

	return stages, issues, current
}

func normalizeStagePhase(value string) string {
	switch strings.ToLower(value) {
	case "running":
		return "Running"
	case "passed", "succeeded", "success":
		return "Succeeded"
	case "failed", "failure":
		return "Failed"
	case "skipped":
		return "Skipped"
	default:
		return value
	}
}

func stageDuration(stage *StageStatus, now time.Time) string {
	if stage.StartedAt == nil {
		return ""
	}

	end := now
	if stage.FinishedAt != nil {
		end = *stage.FinishedAt
	}

	if end.Before(*stage.StartedAt) {
		return ""
	}

	return end.Sub(*stage.StartedAt).Round(time.Second).String()
}

func certificationTestStages(
	run *signalv1.CertificationRun,
	workflow *unstructured.Unstructured,
	aliases map[string]string,
	now time.Time,
) ([]StageStatus, []string, string) {
	results := append([]signalv1.TestResult(nil), run.Status.TestResults...)
	if workflow != nil && !run.Status.Phase.Terminal() {
		results = append(results, signal.ObserveTestResults(run, workflow)...)
	}

	currentAttempt := max(run.Status.Workflow.Attempt, 1)
	attempts := map[int32]bool{currentAttempt: true}
	byKey := map[string]signalv1.TestResult{}
	key := func(attempt int32, nodeUID, id string) string {
		return fmt.Sprintf("%d/%s/%s", attempt, nodeUID, id)
	}

	for _, result := range results {
		if result.Attempt <= 0 {
			continue
		}

		attempts[result.Attempt] = true
		byKey[key(result.Attempt, string(result.NodeUID), result.ID)] = result
	}

	orderedAttempts := make([]int32, 0, len(attempts))
	for attempt := range attempts {
		orderedAttempts = append(orderedAttempts, attempt)
	}

	sort.Slice(orderedAttempts, func(i, j int) bool { return orderedAttempts[i] < orderedAttempts[j] })

	nodes := map[string]WorkflowNode{}
	for _, node := range extractWorkflowNodes(workflow) {
		nodes[node.ID] = node
	}

	stages := make([]StageStatus, 0, len(run.Spec.TestPlan)*len(attempts))
	issues := []string{}
	running := []string{}

	for _, attempt := range orderedAttempts {
		for i, invocation := range run.Spec.TestPlan {
			alias := firstNonempty(aliases[invocation.NodeName], "approved-node")
			stage := StageStatus{
				ID:             fmt.Sprintf("test-%d-a%d", i+1, attempt),
				Name:           sanitizeDisplay(invocation.ID),
				Category:       "test",
				Required:       true,
				Phase:          "Pending",
				NodeAlias:      alias,
				Attempt:        attempt,
				ExecutionState: "Pending",
				Parameters:     displayTestParameters(invocation.Parameters),
				Source:         "CertificationRun immutable test plan",
			}

			result, found := byKey[key(attempt, string(invocation.NodeUID), invocation.ID)]
			if found {
				stage.Phase = firstNonempty(result.ExecutionState, "Unknown")
				stage.ExecutionState = stage.Phase
				stage.Reason = sanitizeDisplay(result.Reason)

				stage.Verdict = sanitizeDisplay(result.Verdict)
				if (stage.Phase == "Pending" || stage.Phase == "Running") &&
					!run.Status.Phase.Terminal() && attempt == currentAttempt {
					stage.Verdict = ""
					stage.Reason = ""
				}

				stage.EvidenceURI = sanitizeDisplay(result.Evidence.URI)
				stage.EvidenceDigest = shortDigest(result.Evidence.Digest)

				stage.Source = "CertificationRun retained test result"
				if workflow != nil && result.Evidence.URI == "" {
					stage.Source = "Argo per-test execution"
				}

				nodeID := firstNonempty(result.ContainerNodeID, result.WorkflowNodeID)
				if node, ok := nodes[nodeID]; ok {
					stage.StartedAt, stage.FinishedAt = node.StartedAt, node.FinishedAt
					if stage.FinishedAt == nil && (run.Status.Phase.Terminal() || attempt < currentAttempt) {
						for _, finishedAttempt := range run.Status.Attempts {
							if finishedAttempt.Workflow.Attempt == attempt {
								stage.FinishedAt = optionalTime(finishedAttempt.FinishedAt.Time)

								break
							}
						}

						if stage.FinishedAt == nil && run.Status.Phase.Terminal() && attempt == currentAttempt {
							stage.FinishedAt = optionalTime(metaTime(run.Status.FinishedAt))
						}
					}

					if stage.FinishedAt != nil || (!run.Status.Phase.Terminal() && attempt == currentAttempt) {
						stage.Duration = stageDuration(&stage, now)
					}
				}
			}

			if (!found || stage.Verdict == "") &&
				(run.Status.Phase.Terminal() || attempt < currentAttempt) {
				stage.Verdict = "Inconclusive"

				stage.Reason = firstNonempty(stage.Reason, "MissingTestResult")
				if !found {
					stage.Phase = "Omitted"
					stage.ExecutionState = stage.Phase
				}

				issues = append(issues, fmt.Sprintf("Missing required result for test %q on %s, attempt %d.", invocation.ID, alias, attempt))
			}

			if stage.Phase == "Running" && !run.Status.Phase.Terminal() && attempt == currentAttempt {
				running = append(running, stage.ID)
			}

			stages = append(stages, stage)
		}
	}

	return stages, issues, strings.Join(running, ", ")
}

func displayTestParameters(parameters map[string]string) map[string]string {
	result := map[string]string{}

	for _, name := range []string{
		"expected-gpu-product-regex", "expected-gpu-count", "gpu-count", "stage-name",
		"observed-gpu-capacity", "observed-gpu-allocatable", "burn-duration-seconds",
		"sample-interval-seconds", "power-limit-tolerance-percent", "power-limit-consecutive-samples",
		"cpu-workers", "memory-mib", "storage-mib", "node-workflow-template",
	} {
		if value, ok := parameters[name]; ok {
			result[name] = sanitizeDisplay(value)
		}
	}

	return result
}
