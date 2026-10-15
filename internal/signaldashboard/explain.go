// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signaldashboard

import (
	"strings"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

var reasonExplanations = map[string]string{
	"NotDue":                 "The last successful certification is still fresh.",
	"IdleWindow":             "The node has not remained continuously idle for the profile's minimum idle window.",
	"ConcurrencyLimit":       "The profile's active-run budget is already in use.",
	"ExclusiveProfileActive": "Another mutually exclusive certification profile is active on this node.",
	"Backoff":                "A recent failure started a bounded retry backoff.",
	"CircuitOpen":            "Repeated failures suspended automatic admission until an operator reviews the node.",
	"Expired":                "The recurring policy has passed its safety expiry and will not admit new runs.",
	"WorkflowFailed":         "Argo could not complete the execution workflow.",
	"PodFailed":              "A workflow task pod failed before a typed hardware result was collected.",
	"APIFailure":             "A required Kubernetes API could not be read or updated.",
	"Healthy":                "The diagnostic completed and reported a passing hardware verdict.",
}

func explainReason(reason string) string {
	if explanation, ok := reasonExplanations[reason]; ok {
		return explanation
	}

	lower := strings.ToLower(reason)
	switch {
	case strings.Contains(lower, "idle"):
		return reasonExplanations["IdleWindow"]
	case strings.Contains(lower, "concurr"):
		return reasonExplanations["ConcurrencyLimit"]
	case strings.Contains(lower, "exclusive"):
		return reasonExplanations["ExclusiveProfileActive"]
	case strings.Contains(lower, "backoff"):
		return reasonExplanations["Backoff"]
	case strings.Contains(lower, "circuit") || strings.Contains(lower, "suspend"):
		return reasonExplanations["CircuitOpen"]
	case strings.Contains(lower, "expire"):
		return reasonExplanations["Expired"]
	case strings.Contains(lower, "workflow"):
		return reasonExplanations["WorkflowFailed"]
	case strings.Contains(lower, "pod"):
		return reasonExplanations["PodFailed"]
	case strings.Contains(lower, "api"):
		return reasonExplanations["APIFailure"]
	case reason == "":
		return "No additional reason has been reported."
	default:
		return "The controller reported this Kubernetes reason. Inspect the raw message for details."
	}
}

func outcomeType(phase signalv1.RunPhase, execution, verdict string) string {
	switch {
	case execution == "Completed" && strings.EqualFold(verdict, "Pass"):
		return "hardware-pass"
	case execution == "Completed" &&
		(strings.EqualFold(verdict, "Fail") || strings.EqualFold(verdict, "Suspect")):
		return "hardware-failure"
	case phase == signalv1.RunPhaseInconclusive:
		return "inconclusive"
	case phase == signalv1.RunPhaseFailed:
		return "infrastructure-failure"
	case phase == signalv1.RunPhasePending:
		return "pending"
	case phase == signalv1.RunPhaseRunning:
		return "running"
	default:
		return "unknown"
	}
}
