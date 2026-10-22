// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signaladapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

const (
	backendEvidenceVersion = "v1"
	maxEvidenceLogBytes    = 512
	maxEvidenceTextBytes   = 512
	resultsStatusPrefix    = "PROJECT_SIGNAL_RESULTS_V1 "
)

var (
	sensitiveAssignmentPattern = regexp.MustCompile(
		`(?i)((?:token|password|passwd|secret|credential|api[_-]?key)\s*[=:]\s*)([^\s]+)`,
	)
	bearerPattern       = regexp.MustCompile(`(?i)(authorization:\s*bearer\s+)([^\s]+)`)
	urlUserPattern      = regexp.MustCompile(`(https?://)[^/@\s]+@`)
	resultsFinalPattern = regexp.MustCompile(
		`^verdict=(?:present|missing) reason=(?:present|missing) measurements=(?:present|missing)$`,
	)
)

type BackendEvidence struct {
	Version      string                       `json:"version"`
	Complete     bool                         `json:"complete"`
	Participants []ParticipantBackendEvidence `json:"participants"`
	CaptureError string                       `json:"captureError,omitempty"`
}

type ParticipantBackendEvidence struct {
	ParticipantIndex int                   `json:"participantIndex"`
	Resource         BackendObjectEvidence `json:"resource"`
	LauncherJob      *JobEvidence          `json:"launcherJob,omitempty"`
	Pods             []PodEvidence         `json:"pods"`
	CaptureError     string                `json:"captureError,omitempty"`
}

type BackendObjectEvidence struct {
	APIVersion      string                           `json:"apiVersion"`
	Kind            string                           `json:"kind"`
	Namespace       string                           `json:"namespace,omitempty"`
	Name            string                           `json:"name"`
	UID             types.UID                        `json:"uid"`
	Phase           string                           `json:"phase,omitempty"`
	Reason          string                           `json:"reason,omitempty"`
	Message         string                           `json:"message,omitempty"`
	Conditions      []ConditionEvidence              `json:"conditions,omitempty"`
	ReplicaStatuses map[string]ReplicaStatusEvidence `json:"replicaStatuses,omitempty"`
}

type ReplicaStatusEvidence struct {
	Active    int64 `json:"active,omitempty"`
	Succeeded int64 `json:"succeeded,omitempty"`
	Failed    int64 `json:"failed,omitempty"`
}

type ConditionEvidence struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

type JobEvidence struct {
	Name       string              `json:"name"`
	UID        types.UID           `json:"uid"`
	Conditions []ConditionEvidence `json:"conditions,omitempty"`
}

type PodEvidence struct {
	Name       string              `json:"name"`
	UID        types.UID           `json:"uid"`
	Role       string              `json:"role"`
	Phase      corev1.PodPhase     `json:"phase,omitempty"`
	Reason     string              `json:"reason,omitempty"`
	Message    string              `json:"message,omitempty"`
	Containers []ContainerEvidence `json:"containers"`
	Conditions []ConditionEvidence `json:"conditions,omitempty"`
}

type ContainerEvidence struct {
	Name         string                 `json:"name"`
	RestartCount int32                  `json:"restartCount"`
	State        ContainerStateEvidence `json:"state"`
	LastState    ContainerStateEvidence `json:"lastState"`
	CurrentLog   CapturedLogEvidence    `json:"currentLog"`
	PreviousLog  CapturedLogEvidence    `json:"previousLog"`
}

type ContainerStateEvidence struct {
	State      string `json:"state,omitempty"`
	ExitCode   int32  `json:"exitCode,omitempty"`
	Signal     int32  `json:"signal,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Message    string `json:"message,omitempty"`
	FinishedAt string `json:"finishedAt,omitempty"`
}

type CapturedLogEvidence struct {
	Status                string `json:"status"`
	Bytes                 int    `json:"bytes,omitempty"`
	CapturedBytes         int    `json:"capturedBytes,omitempty"`
	Truncated             bool   `json:"truncated,omitempty"`
	Content               string `json:"content,omitempty"`
	ResultMarkerStatus    string `json:"resultMarkerStatus,omitempty"`
	ResultDirectoryStatus string `json:"resultDirectoryStatus,omitempty"`
	Error                 string `json:"error,omitempty"`
}

func (r *Runner) captureBackendEvidence(
	ctx context.Context,
	config Config,
	backends []createdResource,
) (string, bool, error) {
	evidence := BackendEvidence{
		Version:      backendEvidenceVersion,
		Complete:     true,
		Participants: make([]ParticipantBackendEvidence, len(backends)),
	}

	for index := range backends {
		participant, complete := r.captureParticipantEvidence(ctx, config, backends[index])
		evidence.Participants[index] = participant
		evidence.Complete = evidence.Complete && complete
	}

	content, complete, err := marshalBackendEvidence(evidence)
	if err != nil {
		return "", false, err
	}

	return string(content), complete, nil
}

func marshalBackendEvidence(evidence BackendEvidence) ([]byte, bool, error) {
	content, err := json.Marshal(evidence)
	if err != nil {
		return nil, false, fmt.Errorf("encode backend evidence: %w", err)
	}

	if len(content) <= MaxBackendEvidenceBytes {
		return content, evidence.Complete, nil
	}

	evidence.Complete = false
	for index := range evidence.Participants {
		participant := &evidence.Participants[index]

		participant.CaptureError = "container log content omitted because backend evidence exceeded the size limit"
		for podIndex := range participant.Pods {
			pod := &participant.Pods[podIndex]
			for containerIndex := range pod.Containers {
				container := &pod.Containers[containerIndex]
				omitLogContent(&container.CurrentLog)
				omitLogContent(&container.PreviousLog)
			}
		}
	}

	content, err = json.Marshal(evidence)
	if err != nil {
		return nil, false, fmt.Errorf("encode bounded backend evidence: %w", err)
	}

	if len(content) <= MaxBackendEvidenceBytes {
		return content, false, nil
	}

	for index := range evidence.Participants {
		participant := &evidence.Participants[index]
		participant.Resource = BackendObjectEvidence{
			APIVersion: participant.Resource.APIVersion,
			Kind:       participant.Resource.Kind,
			Namespace:  participant.Resource.Namespace,
			Name:       participant.Resource.Name,
			UID:        participant.Resource.UID,
		}
		participant.LauncherJob = nil
		participant.Pods = nil
		participant.CaptureError = "backend evidence projection reduced because it exceeded the size limit"
	}

	content, err = json.Marshal(evidence)
	if err != nil {
		return nil, false, fmt.Errorf("encode reduced backend evidence: %w", err)
	}

	if len(content) > MaxBackendEvidenceBytes {
		return nil, false, fmt.Errorf("reduced backend evidence exceeds %d bytes", MaxBackendEvidenceBytes)
	}

	return content, false, nil
}

func omitLogContent(logEvidence *CapturedLogEvidence) {
	if logEvidence.Content == "" {
		return
	}

	logEvidence.Status = "unavailable"
	logEvidence.Content = ""
	logEvidence.CapturedBytes = 0
	logEvidence.Error = "content omitted because backend evidence exceeded the size limit"
}

func markBackendEvidenceIncomplete(content, captureError string) (string, error) {
	var evidence BackendEvidence

	if err := json.Unmarshal([]byte(content), &evidence); err != nil {
		return "", fmt.Errorf("decode backend evidence after persistence failure: %w", err)
	}

	evidence.Complete = false
	evidence.CaptureError = boundedRedactedText(captureError, maxEvidenceTextBytes)

	bounded, _, err := marshalBackendEvidence(evidence)
	if err != nil {
		return "", err
	}

	return string(bounded), nil
}

func (r *Runner) captureParticipantEvidence(
	ctx context.Context,
	config Config,
	backend createdResource,
) (ParticipantBackendEvidence, bool) {
	participant := ParticipantBackendEvidence{
		ParticipantIndex: int(backend.participantIndex),
		Resource: BackendObjectEvidence{
			APIVersion: backend.resource.GetAPIVersion(),
			Kind:       backend.resource.GetKind(),
			Namespace:  backend.resource.GetNamespace(),
			Name:       backend.resource.GetName(),
			UID:        backend.resource.GetUID(),
		},
	}
	complete := true

	object, err := r.Dynamic.Resource(backend.gvr).Namespace(config.Namespace).
		Get(ctx, backend.resource.GetName(), metav1.GetOptions{})
	if err != nil {
		participant.CaptureError = boundedRedactedText("get backend object: "+err.Error(), maxEvidenceTextBytes)
		complete = false
	} else {
		projected, projectErr := projectBackendObject(object)
		if projectErr != nil {
			participant.CaptureError = joinCaptureError(
				participant.CaptureError,
				"project backend object: "+projectErr.Error(),
			)
			complete = false
		} else {
			participant.Resource = projected
		}
	}

	switch backend.resource.GetKind() {
	case "Pod":
		pod, podErr := r.Kubernetes.CoreV1().Pods(config.Namespace).
			Get(ctx, backend.resource.GetName(), metav1.GetOptions{})
		if podErr != nil {
			participant.CaptureError = joinCaptureError(participant.CaptureError, "get participant Pod: "+podErr.Error())

			return participant, false
		}

		podEvidence, podComplete := r.capturePodEvidence(ctx, config.Namespace, pod, "participant")
		participant.Pods = []PodEvidence{podEvidence}
		complete = complete && podComplete
	case "MPIJob":
		jobs, jobErr := r.Kubernetes.BatchV1().Jobs(config.Namespace).List(ctx, metav1.ListOptions{})
		if jobErr != nil {
			participant.CaptureError = joinCaptureError(participant.CaptureError, "list launcher Jobs: "+jobErr.Error())
			complete = false
		}

		jobUIDs := map[types.UID]struct{}{}

		if jobErr == nil {
			for index := range jobs.Items {
				job := &jobs.Items[index]
				if !ownedBy(job.OwnerReferences, backend.resource.GetUID()) {
					continue
				}

				jobUIDs[job.UID] = struct{}{}
				projected := projectJob(job)
				participant.LauncherJob = &projected
			}
		}

		pods, podErr := r.Kubernetes.CoreV1().Pods(config.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: participantLabelSelector(config.RunName, backend.participantIndex),
		})
		if podErr != nil {
			participant.CaptureError = joinCaptureError(participant.CaptureError, "list MPI Pods: "+podErr.Error())

			return participant, false
		}

		for index := range pods.Items {
			pod := &pods.Items[index]

			role := mpiPodRole(pod, backend.resource.GetUID(), jobUIDs)
			if role == "" {
				continue
			}

			podEvidence, podComplete := r.capturePodEvidence(ctx, config.Namespace, pod, role)
			participant.Pods = append(participant.Pods, podEvidence)
			complete = complete && podComplete
		}

		slices.SortFunc(participant.Pods, func(left, right PodEvidence) int {
			return strings.Compare(left.Name, right.Name)
		})

		if participant.LauncherJob == nil {
			participant.CaptureError = joinCaptureError(participant.CaptureError, "MPI launcher Job was not found")
			complete = false
		}

		if !hasPodRole(participant.Pods, "launcher") || !hasPodRole(participant.Pods, "worker") {
			participant.CaptureError = joinCaptureError(
				participant.CaptureError,
				"MPI launcher and worker Pods were not both found",
			)
			complete = false
		}
	default:
		participant.CaptureError = joinCaptureError(
			participant.CaptureError,
			fmt.Sprintf("unsupported backend kind %q", backend.resource.GetKind()),
		)
		complete = false
	}

	return participant, complete
}

func (r *Runner) capturePodEvidence(
	ctx context.Context,
	namespace string,
	pod *corev1.Pod,
	role string,
) (PodEvidence, bool) {
	evidence := PodEvidence{
		Name:       pod.Name,
		UID:        pod.UID,
		Role:       role,
		Phase:      pod.Status.Phase,
		Reason:     boundedRedactedText(pod.Status.Reason, maxEvidenceTextBytes),
		Message:    boundedRedactedText(pod.Status.Message, maxEvidenceTextBytes),
		Conditions: projectPodConditions(pod.Status.Conditions),
	}
	complete := true

	statuses := append(slices.Clone(pod.Status.InitContainerStatuses), pod.Status.ContainerStatuses...)
	for _, status := range statuses {
		if !capturedContainer(role, status.Name) {
			continue
		}

		container := ContainerEvidence{
			Name:         status.Name,
			RestartCount: status.RestartCount,
			State:        projectContainerState(status.State),
			LastState:    projectContainerState(status.LastTerminationState),
		}

		current, currentErr := r.readPodLog(ctx, namespace, pod.Name, status.Name, false)

		container.CurrentLog = capturedLog(current, currentErr)
		if currentErr != nil {
			complete = false
		}

		if status.RestartCount > 0 || status.LastTerminationState.Terminated != nil {
			previous, previousErr := r.readPodLog(ctx, namespace, pod.Name, status.Name, true)

			container.PreviousLog = capturedLog(previous, previousErr)
			if previousErr != nil {
				complete = false
			}
		} else {
			container.PreviousLog = CapturedLogEvidence{Status: "not-applicable"}
		}

		evidence.Containers = append(evidence.Containers, container)
	}

	if len(evidence.Containers) == 0 {
		complete = false
	}

	return evidence, complete
}

func projectBackendObject(object *unstructured.Unstructured) (BackendObjectEvidence, error) {
	evidence := BackendObjectEvidence{
		APIVersion: object.GetAPIVersion(),
		Kind:       object.GetKind(),
		Namespace:  object.GetNamespace(),
		Name:       object.GetName(),
		UID:        object.GetUID(),
	}

	var err error

	evidence.Phase, _, err = unstructured.NestedString(object.Object, "status", "phase")
	if err != nil {
		return evidence, fmt.Errorf("read status.phase: %w", err)
	}

	evidence.Reason, _, err = unstructured.NestedString(object.Object, "status", "reason")
	if err != nil {
		return evidence, fmt.Errorf("read status.reason: %w", err)
	}

	evidence.Message, _, err = unstructured.NestedString(object.Object, "status", "message")
	if err != nil {
		return evidence, fmt.Errorf("read status.message: %w", err)
	}

	evidence.Reason = boundedRedactedText(evidence.Reason, maxEvidenceTextBytes)
	evidence.Message = boundedRedactedText(evidence.Message, maxEvidenceTextBytes)

	evidence.Conditions, err = projectUnstructuredConditions(object)
	if err != nil {
		return evidence, err
	}

	statuses, found, err := unstructured.NestedMap(object.Object, "status", "replicaStatuses")
	if err != nil {
		return evidence, fmt.Errorf("read status.replicaStatuses: %w", err)
	}

	if found {
		evidence.ReplicaStatuses = make(map[string]ReplicaStatusEvidence, len(statuses))
		for name := range statuses {
			active, countErr := nestedReplicaCount(statuses, name, "active")
			if countErr != nil {
				return evidence, countErr
			}

			succeeded, countErr := nestedReplicaCount(statuses, name, "succeeded")
			if countErr != nil {
				return evidence, countErr
			}

			failed, countErr := nestedReplicaCount(statuses, name, "failed")
			if countErr != nil {
				return evidence, countErr
			}

			evidence.ReplicaStatuses[name] = ReplicaStatusEvidence{
				Active:    active,
				Succeeded: succeeded,
				Failed:    failed,
			}
		}
	}

	return evidence, nil
}

func nestedReplicaCount(statuses map[string]any, replica, field string) (int64, error) {
	value, _, err := unstructured.NestedInt64(statuses, replica, field)
	if err != nil {
		return 0, fmt.Errorf("read status.replicaStatuses.%s.%s: %w", replica, field, err)
	}

	return value, nil
}

func projectUnstructuredConditions(object *unstructured.Unstructured) ([]ConditionEvidence, error) {
	conditions, found, err := unstructured.NestedSlice(object.Object, "status", "conditions")
	if err != nil {
		return nil, fmt.Errorf("read status.conditions: %w", err)
	}

	if !found {
		return nil, nil
	}

	projected := make([]ConditionEvidence, 0, len(conditions))
	for _, raw := range conditions {
		condition, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		projected = append(projected, ConditionEvidence{
			Type:    stringValue(condition["type"]),
			Status:  stringValue(condition["status"]),
			Reason:  boundedRedactedText(stringValue(condition["reason"]), maxEvidenceTextBytes),
			Message: boundedRedactedText(stringValue(condition["message"]), maxEvidenceTextBytes),
		})
	}

	return projected, nil
}

func projectJob(job *batchv1.Job) JobEvidence {
	conditions := make([]ConditionEvidence, 0, len(job.Status.Conditions))
	for _, condition := range job.Status.Conditions {
		conditions = append(conditions, ConditionEvidence{
			Type:    string(condition.Type),
			Status:  string(condition.Status),
			Reason:  boundedRedactedText(condition.Reason, maxEvidenceTextBytes),
			Message: boundedRedactedText(condition.Message, maxEvidenceTextBytes),
		})
	}

	return JobEvidence{Name: job.Name, UID: job.UID, Conditions: conditions}
}

func projectPodConditions(conditions []corev1.PodCondition) []ConditionEvidence {
	projected := make([]ConditionEvidence, 0, len(conditions))
	for _, condition := range conditions {
		projected = append(projected, ConditionEvidence{
			Type:    string(condition.Type),
			Status:  string(condition.Status),
			Reason:  boundedRedactedText(condition.Reason, maxEvidenceTextBytes),
			Message: boundedRedactedText(condition.Message, maxEvidenceTextBytes),
		})
	}

	return projected
}

func projectContainerState(state corev1.ContainerState) ContainerStateEvidence {
	switch {
	case state.Terminated != nil:
		return ContainerStateEvidence{
			State:      "terminated",
			ExitCode:   state.Terminated.ExitCode,
			Signal:     state.Terminated.Signal,
			Reason:     boundedRedactedText(state.Terminated.Reason, maxEvidenceTextBytes),
			Message:    boundedRedactedText(state.Terminated.Message, maxEvidenceTextBytes),
			FinishedAt: state.Terminated.FinishedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
		}
	case state.Waiting != nil:
		return ContainerStateEvidence{
			State:   "waiting",
			Reason:  boundedRedactedText(state.Waiting.Reason, maxEvidenceTextBytes),
			Message: boundedRedactedText(state.Waiting.Message, maxEvidenceTextBytes),
		}
	case state.Running != nil:
		return ContainerStateEvidence{State: "running"}
	default:
		return ContainerStateEvidence{}
	}
}

func capturedLog(content string, err error) CapturedLogEvidence {
	if err != nil {
		return CapturedLogEvidence{
			Status: "unavailable",
			Error:  boundedRedactedText(err.Error(), maxEvidenceTextBytes),
		}
	}

	redacted := redactLog(content)

	truncated := len(redacted) > maxEvidenceLogBytes
	if truncated {
		redacted = redacted[len(redacted)-maxEvidenceLogBytes:]
	}

	return CapturedLogEvidence{
		Status:                "captured",
		Bytes:                 len(content),
		CapturedBytes:         len(redacted),
		Truncated:             truncated,
		Content:               redacted,
		ResultMarkerStatus:    resultLogSourceMetadata(content, nil).Status,
		ResultDirectoryStatus: resultDirectoryStatus(content),
	}
}

func redactLog(content string) string {
	lines := strings.Split(content, "\n")
	inPrivateKey := false

	for index, line := range lines {
		if strings.Contains(line, "BEGIN ") && strings.Contains(line, "PRIVATE KEY") {
			inPrivateKey = true
			lines[index] = "[REDACTED PRIVATE KEY]"

			continue
		}

		if inPrivateKey {
			lines[index] = "[REDACTED PRIVATE KEY]"

			if strings.Contains(line, "END ") && strings.Contains(line, "PRIVATE KEY") {
				inPrivateKey = false
			}

			continue
		}

		if strings.HasPrefix(line, resultPrefix) {
			lines[index] = resultPrefix + "[REDACTED]"

			continue
		}

		line = bearerPattern.ReplaceAllString(line, `${1}[REDACTED]`)
		line = sensitiveAssignmentPattern.ReplaceAllString(line, `${1}[REDACTED]`)
		lines[index] = urlUserPattern.ReplaceAllString(line, `${1}[REDACTED]@`)
	}

	return strings.Join(lines, "\n")
}

func resultDirectoryStatus(content string) string {
	status := "missing"

	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, resultsStatusPrefix) {
			value := strings.TrimPrefix(line, resultsStatusPrefix)
			switch {
			case value == "prepared", resultsFinalPattern.MatchString(value):
				status = value
			default:
				status = "invalid"
			}
		}
	}

	return status
}

func boundedRedactedText(value string, limit int) string {
	value = redactLog(value)
	if len(value) <= limit {
		return value
	}

	return value[:limit]
}

func participantLabelSelector(runName string, participantIndex int32) string {
	return strings.Join([]string{
		"signal.unbounded-cloud.io/run=" + runName,
		"signal.unbounded-cloud.io/participant-index=" + strconv.FormatInt(int64(participantIndex), 10),
	}, ",")
}

func mpiPodRole(pod *corev1.Pod, mpiJobUID types.UID, jobUIDs map[types.UID]struct{}) string {
	if ownedBy(pod.OwnerReferences, mpiJobUID) && hasContainer(pod, "worker") {
		return "worker"
	}

	for _, reference := range pod.OwnerReferences {
		if _, exists := jobUIDs[reference.UID]; exists && hasContainer(pod, "launcher") {
			return "launcher"
		}
	}

	return ""
}

func hasContainer(pod *corev1.Pod, name string) bool {
	return slices.ContainsFunc(pod.Spec.Containers, func(container corev1.Container) bool {
		return container.Name == name
	})
}

func capturedContainer(role, name string) bool {
	switch role {
	case "participant":
		return name == "main"
	case "launcher":
		return name == "launcher"
	case "worker":
		return name == "worker"
	default:
		return false
	}
}

func hasPodRole(pods []PodEvidence, role string) bool {
	return slices.ContainsFunc(pods, func(pod PodEvidence) bool {
		return pod.Role == role
	})
}

func joinCaptureError(current, next string) string {
	next = boundedRedactedText(next, maxEvidenceTextBytes)
	if current == "" {
		return next
	}

	return boundedRedactedText(current+"; "+next, maxEvidenceTextBytes)
}

func stringValue(value any) string {
	text, ok := value.(string)
	if !ok {
		return ""
	}

	return text
}

func recordEvidenceCaptureFailure(output *AggregateOutput) {
	output.ExecutionState = "Error"
	output.Verdict = "Inconclusive"

	if output.Reason == "" || output.Reason == "AllParticipantsPassed" ||
		output.Reason == "ParticipantCertificationFailed" ||
		output.Reason == "ParticipantCertificationInconclusive" {
		output.Reason = "BackendEvidenceCaptureFailed"
	}
}

func ValidateBackendEvidence(content string) error {
	if content == "" {
		return errors.New("backend evidence is required")
	}

	if len(content) > MaxBackendEvidenceBytes {
		return fmt.Errorf("backend evidence exceeds %d bytes", MaxBackendEvidenceBytes)
	}

	var evidence BackendEvidence

	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&evidence); err != nil {
		return fmt.Errorf("decode backend evidence: %w", err)
	}

	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("backend evidence contains trailing JSON values")
		}

		return fmt.Errorf("decode trailing backend evidence value: %w", err)
	}

	if evidence.Version != backendEvidenceVersion {
		return fmt.Errorf("backend evidence version %q is invalid", evidence.Version)
	}

	if len(evidence.Participants) < 1 || len(evidence.Participants) > MaxParticipants {
		return fmt.Errorf(
			"backend evidence must contain between 1 and %d participants",
			MaxParticipants,
		)
	}

	return nil
}
