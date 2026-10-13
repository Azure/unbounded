// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signaldashboard

import (
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

func latestOutcomes(snapshot collected) []LatestOutcome {
	latest := map[string]*signalv1.CertificationRun{}

	for i := range snapshot.runs {
		run := &snapshot.runs[i]
		if !run.Status.Phase.Terminal() || !certificationRun(run) {
			continue
		}

		for _, participant := range run.Spec.Participants {
			key := participant.NodeName + "\x00" + run.Spec.Profile.Name
			if current := latest[key]; current == nil || run.CreationTimestamp.After(current.CreationTimestamp.Time) {
				latest[key] = run
			}
		}
	}

	result := make([]LatestOutcome, 0, len(latest))
	for key, run := range latest {
		nodeName := strings.SplitN(key, "\x00", 2)[0]
		result = append(result, LatestOutcome{
			NodeAlias: firstNonempty(snapshot.aliases[nodeName], "approved-node"),
			Profile:   run.Spec.Profile.Name,
			Verdict:   run.Status.Verdict,
			Reason:    sanitizeDisplay(run.Status.Reason),
			At:        optionalTime(metaTime(run.Status.FinishedAt)),
		})
	}

	sort.Slice(result, func(i, j int) bool {
		if result[i].NodeAlias == result[j].NodeAlias {
			return result[i].Profile < result[j].Profile
		}

		return result[i].NodeAlias < result[j].NodeAlias
	})

	return result
}

func workflowPod(workflow *unstructured.Unstructured, pods []corev1.Pod) *corev1.Pod {
	if workflow == nil {
		return nil
	}

	var fallback *corev1.Pod

	for i := range pods {
		if pods[i].Labels["workflows.argoproj.io/workflow"] != workflow.GetName() &&
			!ownedByUID(&pods[i], workflow.GetUID()) {
			continue
		}

		for _, container := range pods[i].Spec.Containers {
			if container.Name != "main" {
				continue
			}

			nodeName := pods[i].Annotations["workflows.argoproj.io/node-name"]
			if nodeName == "node-health" || strings.HasSuffix(nodeName, ".node-health") {
				return &pods[i]
			}

			if fallback == nil {
				fallback = &pods[i]
			}

			break
		}
	}

	return fallback
}

func scriptStatus(run *signalv1.CertificationRun, pod *corev1.Pod) ScriptStatus {
	status := ScriptStatus{
		Key:               run.Spec.Script.Key,
		SnapshotConfigMap: sanitizeObjectName(run.Spec.Script.ConfigMapName),
		Digest:            shortDigest(run.Spec.Script.Digest),
		RunnerImageDigest: imageDigest(run.Spec.RunnerImage),
	}
	if pod == nil {
		return status
	}

	for _, container := range pod.Spec.Containers {
		if container.Name != "main" {
			continue
		}

		status.Command = append(append([]string(nil), container.Command...), container.Args...)
		for i, part := range status.Command {
			if part == "--" && i+1 < len(status.Command) {
				status.Command = append([]string(nil), status.Command[i+1:]...)
				break
			}
		}

		for i := range status.Command {
			status.Command[i] = sanitizeDisplay(status.Command[i])
		}

		break
	}

	return status
}

func sanitizeObjectName(name string) string {
	if name == "" {
		return ""
	}

	if strings.HasSuffix(name, "-scripts") {
		return "run-snapshot-[redacted]-scripts"
	}

	return sanitizeDisplay(name)
}

func imageDigest(image string) string {
	if at := strings.LastIndex(image, "@sha256:"); at >= 0 {
		return shortDigest(image[at+1:])
	}

	return ""
}

func admissionStages(run *signalv1.CertificationRun) []StageStatus {
	reservationPhase := "Succeeded"
	if run.Status.Phase == signalv1.RunPhasePending {
		reservationPhase = "Running"
	}

	return []StageStatus{
		{ID: "eligibility", Name: "Idle eligibility", Phase: "Succeeded", Source: "Project Signal policy", Message: "The approved node satisfied admission policy."},
		{ID: "reservation", Name: "Reservation", Phase: reservationPhase, Source: "Project Signal policy", Message: "Participants were selected and locked for this run."},
		{ID: "snapshot", Name: "Immutable script snapshot", Phase: "Succeeded", Source: "Project Signal policy", Message: "The script, stage plan, profile generation, and digest were pinned."},
	}
}

func finalizationStages(run *signalv1.CertificationRun) []StageStatus {
	resultPhase := "Pending"
	evidencePhase := "Pending"
	projectionPhase := "Pending"
	cleanupPhase := "Pending"

	if run.Status.Verdict != "" {
		resultPhase = "Succeeded"
	}

	if run.Status.Evidence.URI != "" {
		evidencePhase = "Succeeded"
	}

	if run.Status.Phase.Terminal() {
		projectionPhase = "Succeeded"
	}

	if run.Status.Cleanup.ReservationsReleased && run.Status.Cleanup.SnapshotDeleted {
		cleanupPhase = "Succeeded"
	} else if run.Status.Phase.Terminal() {
		cleanupPhase = "Running"
	}

	return []StageStatus{
		{ID: "typed-result", Name: "Typed result extraction", Phase: resultPhase, Source: "Project Signal finalization", Reason: run.Status.Reason},
		{ID: "evidence", Name: "Evidence write", Phase: evidencePhase, Source: "Project Signal finalization", Message: sanitizeDisplay(run.Status.Evidence.URI)},
		{ID: "node-projection", Name: "Node eligibility update", Phase: projectionPhase, Source: "Project Signal finalization"},
		{ID: "cleanup", Name: "Request and reservation cleanup", Phase: cleanupPhase, Source: "Project Signal finalization"},
		{ID: "retention", Name: "Bounded workflow retention", Phase: map[bool]string{true: "Succeeded", false: "Pending"}[run.Status.Phase.Terminal()], Source: "Argo TTL policy"},
	}
}

func argoTaskStatus(nodes []WorkflowNode) StageStatus {
	for _, node := range nodes {
		name := strings.ToLower(firstNonempty(node.DisplayName, node.Template))
		if strings.Contains(name, "node-health") {
			return StageStatus{
				ID:         "node-health",
				Name:       "node-health",
				Phase:      firstNonempty(node.Phase, "Pending"),
				Reason:     node.Reason,
				Message:    node.Message,
				StartedAt:  node.StartedAt,
				FinishedAt: node.FinishedAt,
				Source:     "Argo Workflow",
			}
		}
	}

	return StageStatus{ID: "node-health", Name: "node-health", Phase: "Pending", Source: "Argo Workflow"}
}
