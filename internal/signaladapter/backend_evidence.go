// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signaladapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

const (
	backendEvidenceVersion = "v2"
	maxEvidenceLogBytes    = 512
	maxEvidenceTextBytes   = 512
	maxEvidenceSamples     = 64
	maxEvidenceDevices     = 64
	maxEvidenceDeviceBytes = 128
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
	Identity     EvidenceIdentity             `json:"identity"`
	Timing       CheckTimingEvidence          `json:"timing"`
	Provenance   ExecutionProvenanceEvidence  `json:"provenance"`
	GroupResult  *VerdictEvidence             `json:"groupResult,omitempty"`
	Complete     bool                         `json:"complete"`
	Participants []ParticipantBackendEvidence `json:"participants"`
	CaptureError string                       `json:"captureError,omitempty"`
}

type EvidenceIdentity struct {
	RunUID     types.UID        `json:"runUID"`
	CheckID    signalv1.CheckID `json:"checkID"`
	CheckOrder int32            `json:"checkOrder"`
	PlanDigest string           `json:"planDigest,omitempty"`
}

type CheckTimingEvidence struct {
	StartedAt        metav1.Time      `json:"startedAt"`
	FinishedAt       metav1.Time      `json:"finishedAt"`
	ObservedDuration metav1.Duration  `json:"observedDuration"`
	MaxObservedGap   *metav1.Duration `json:"maxObservedGap,omitempty"`
	SampleCount      int              `json:"sampleCount"`
}

type ExecutionProvenanceEvidence struct {
	AdapterImage string `json:"adapterImage,omitempty"`
	RunnerImage  string `json:"runnerImage"`
}

type VerdictEvidence struct {
	ExecutionState string `json:"executionState"`
	Verdict        string `json:"verdict"`
	Reason         string `json:"reason"`
	Attribution    string `json:"attribution"`
}

type ParticipantBackendEvidence struct {
	ParticipantIndex  int                      `json:"participantIndex"`
	Verdict           VerdictEvidence          `json:"verdict"`
	Start             signalv1.NodeFingerprint `json:"start"`
	End               signalv1.NodeFingerprint `json:"end"`
	MaxObservedGap    *metav1.Duration         `json:"maxObservedGap,omitempty"`
	VisibleDevices    []string                 `json:"visibleDevices,omitempty"`
	CoveredDevices    []string                 `json:"coveredDevices,omitempty"`
	PerDeviceCoverage bool                     `json:"perDeviceCoverage,omitempty"`
	Resource          BackendObjectEvidence    `json:"resource"`
	LauncherJob       *JobEvidence             `json:"launcherJob,omitempty"`
	Pods              []PodEvidence            `json:"pods"`
	CaptureError      string                   `json:"captureError,omitempty"`
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
	NodeName   string              `json:"nodeName,omitempty"`
	NodeUID    types.UID           `json:"nodeUID,omitempty"`
	NodeError  string              `json:"nodeError,omitempty"`
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

func MinimumBackendEvidenceBytes(participantCount int) int {
	evidence := BackendEvidence{
		Version:      backendEvidenceVersion,
		Complete:     false,
		Participants: make([]ParticipantBackendEvidence, participantCount),
	}

	for index := range evidence.Participants {
		evidence.Participants[index] = ParticipantBackendEvidence{
			ParticipantIndex: index,
			Resource: BackendObjectEvidence{
				APIVersion: strings.Repeat("a", 32),
				Kind:       strings.Repeat("a", 16),
				Namespace:  strings.Repeat("a", 63),
				Name:       strings.Repeat("a", 63),
				UID:        types.UID(strings.Repeat("a", 128)),
			},
			CaptureError: strings.Repeat("a", maxEvidenceTextBytes),
		}
	}

	content, err := json.Marshal(evidence)
	if err != nil {
		return MaxBackendEvidenceBytes + 1
	}

	return len(content) + 64
}

func (r *Runner) captureBackendEvidence(
	ctx context.Context,
	config Config,
	participants []signalv1.Participant,
	backends []createdResource,
	results []Result,
	starts []signalv1.NodeFingerprint,
	ends []signalv1.NodeFingerprint,
) (string, bool, error) {
	evidence := BackendEvidence{
		Version: backendEvidenceVersion,
		Identity: EvidenceIdentity{
			RunUID:     config.RunUID,
			CheckID:    config.CheckID,
			CheckOrder: config.CheckOrder,
			PlanDigest: config.PlanDigest,
		},
		Provenance: ExecutionProvenanceEvidence{
			AdapterImage: config.AdapterImage,
			RunnerImage:  config.RunnerImage,
		},
		Complete:     true,
		Participants: make([]ParticipantBackendEvidence, 0, len(participants)),
	}

	for index := range backends {
		backend := backends[index]
		switch backend.resource.GetKind() {
		case "Pod":
			if len(backend.participantIndexes) != 1 {
				return "", false, errors.New("pod backend evidence requires exactly one participant index")
			}

			participantIndex := backend.participantIndexes[0]
			if participantIndex < 0 || int(participantIndex) >= len(participants) {
				return "", false, fmt.Errorf("pod backend participant index %d is out of range", participantIndex)
			}

			participant, complete := r.capturePodBackendEvidence(
				ctx,
				config,
				participants[participantIndex],
				backend,
			)
			evidence.Participants = append(evidence.Participants, participant)
			evidence.Complete = evidence.Complete && complete
		case "MPIJob":
			captured, complete := r.captureMPIBackendEvidence(ctx, config, participants, backend)
			evidence.Participants = append(evidence.Participants, captured...)
			evidence.Complete = evidence.Complete && complete
		default:
			return "", false, fmt.Errorf("unsupported backend kind %q", backend.resource.GetKind())
		}
	}

	slices.SortFunc(evidence.Participants, func(left, right ParticipantBackendEvidence) int {
		return left.ParticipantIndex - right.ParticipantIndex
	})

	applyQualificationEvidence(&evidence, config, results, starts, ends)

	content, complete, err := marshalBackendEvidence(evidence, MaxBackendEvidenceBytes)
	if err != nil {
		return "", false, err
	}

	return string(content), complete, nil
}

type qualificationMeasurement struct {
	DriverVersion  string                                `json:"driverVersion,omitempty"`
	VisibleDevices []string                              `json:"visibleDevices,omitempty"`
	CoveredDevices []string                              `json:"coveredDevices,omitempty"`
	Participants   []participantQualificationMeasurement `json:"participants,omitempty"`
}

type participantQualificationMeasurement struct {
	ParticipantIndex int      `json:"participantIndex"`
	DriverVersion    string   `json:"driverVersion,omitempty"`
	VisibleDevices   []string `json:"visibleDevices,omitempty"`
	CoveredDevices   []string `json:"coveredDevices,omitempty"`
}

func applyQualificationEvidence(
	evidence *BackendEvidence,
	config Config,
	results []Result,
	starts []signalv1.NodeFingerprint,
	ends []signalv1.NodeFingerprint,
) {
	resultByParticipant := make(map[int]Result, len(results))
	for index := range results {
		resultByParticipant[results[index].ParticipantIndex] = results[index]
	}

	var earliest time.Time
	var latest time.Time
	var largestGap time.Duration
	totalSamples := 0

	for index := range evidence.Participants {
		participant := &evidence.Participants[index]
		result, found := resultByParticipant[participant.ParticipantIndex]
		if !found {
			participant.Verdict = inconclusiveVerdict("ParticipantResultMissing", qualificationAttribution(config))
			evidence.Complete = false

			continue
		}

		if participant.ParticipantIndex >= 0 && participant.ParticipantIndex < len(starts) {
			participant.Start = starts[participant.ParticipantIndex]
		}
		if participant.ParticipantIndex >= 0 && participant.ParticipantIndex < len(ends) {
			participant.End = ends[participant.ParticipantIndex]
		}

		if earliest.IsZero() || (!result.StartedAt.IsZero() && result.StartedAt.Before(earliest)) {
			earliest = result.StartedAt
		}
		if result.FinishedAt.After(latest) {
			latest = result.FinishedAt
		}

		samples := boundedSampleTimes(result)
		totalSamples += len(samples)
		gap := maximumSampleGap(samples)
		if gap > largestGap {
			largestGap = gap
		}
		if len(samples) > 1 {
			value := metav1.Duration{Duration: gap}
			participant.MaxObservedGap = &value
		}

		participant.Verdict = VerdictEvidence{
			ExecutionState: result.ExecutionState,
			Verdict:        result.Verdict,
			Reason:         result.Reason,
			Attribution:    qualificationAttribution(config),
		}

		measurements, measurementErr := decodeQualificationMeasurements(
			result.Measurements,
			participant.ParticipantIndex,
			config.BackendKind,
		)
		if measurementErr == nil && measurements != nil {
			participant.VisibleDevices = measurements.VisibleDevices
			participant.CoveredDevices = measurements.CoveredDevices
			if measurements.DriverVersion != "" {
				participant.Start.DriverVersion = measurements.DriverVersion
				participant.End.DriverVersion = measurements.DriverVersion
			}
			if len(measurements.VisibleDevices) > 0 {
				digest := deviceInventoryDigest(measurements.VisibleDevices)
				participant.Start.GPUInventoryDigest = digest
				participant.End.GPUInventoryDigest = digest
			}
			participant.PerDeviceCoverage = sameStringSet(
				measurements.VisibleDevices,
				measurements.CoveredDevices,
			)
		}

		enforceQualificationRequirements(participant, config, result, measurementErr)
	}

	if config.BackendKind == string(signalv1.ExecutionBackendMPIJob) && len(results) > 0 {
		evidence.GroupResult = &VerdictEvidence{
			ExecutionState: results[0].ExecutionState,
			Verdict:        results[0].Verdict,
			Reason:         results[0].Reason,
			Attribution:    "group",
		}
	}

	if !earliest.IsZero() && !latest.IsZero() && !latest.Before(earliest) {
		evidence.Timing = CheckTimingEvidence{
			StartedAt:        metav1.NewTime(earliest),
			FinishedAt:       metav1.NewTime(latest),
			ObservedDuration: metav1.Duration{Duration: latest.Sub(earliest)},
			SampleCount:      totalSamples,
		}
		if totalSamples > len(evidence.Participants) {
			value := metav1.Duration{Duration: largestGap}
			evidence.Timing.MaxObservedGap = &value
		}
	}
}

func qualificationAttribution(config Config) string {
	if config.BackendKind == string(signalv1.ExecutionBackendMPIJob) {
		return "group-with-participant-observation"
	}

	return "participant"
}

func enforceQualificationRequirements(
	participant *ParticipantBackendEvidence,
	config Config,
	result Result,
	measurementErr error,
) {
	if config.PlanDigest == "" || config.AdapterImage == "" {
		participant.Verdict = inconclusiveVerdict("QualificationIdentityUnavailable", participant.Verdict.Attribution)

		return
	}

	if participant.CaptureError != "" || participant.Start.NodeUID == "" || participant.End.NodeUID == "" {
		participant.Verdict = inconclusiveVerdict("ParticipantEvidenceIncomplete", participant.Verdict.Attribution)

		return
	}

	if config.MinDuration != nil && result.FinishedAt.Sub(result.StartedAt) < config.MinDuration.Duration {
		participant.Verdict = inconclusiveVerdict("MinimumDurationNotMet", participant.Verdict.Attribution)

		return
	}

	if config.Continuity == nil {
		return
	}

	if config.Continuity.RequireSameBootID &&
		(participant.Start.BootID == "" || participant.End.BootID == "" ||
			participant.Start.BootID != participant.End.BootID) {
		participant.Verdict = inconclusiveVerdict("SameBootNotProven", participant.Verdict.Attribution)

		return
	}

	if config.Continuity.MaxSampleGap != nil {
		if participant.MaxObservedGap == nil ||
			participant.MaxObservedGap.Duration > config.Continuity.MaxSampleGap.Duration {
			participant.Verdict = inconclusiveVerdict("SampleContinuityNotProven", participant.Verdict.Attribution)

			return
		}
	}

	if config.Continuity.RequirePerDeviceCoverage {
		if measurementErr != nil || len(participant.VisibleDevices) == 0 || !participant.PerDeviceCoverage {
			participant.Verdict = inconclusiveVerdict("PerDeviceCoverageNotProven", participant.Verdict.Attribution)
		}
	}
}

func inconclusiveVerdict(reason, attribution string) VerdictEvidence {
	return VerdictEvidence{
		ExecutionState: "Succeeded",
		Verdict:        "Inconclusive",
		Reason:         reason,
		Attribution:    attribution,
	}
}

func boundedSampleTimes(result Result) []time.Time {
	samples := make([]time.Time, 0, len(result.SampleTimes)+2)
	if !result.StartedAt.IsZero() {
		samples = append(samples, result.StartedAt)
	}
	samples = append(samples, result.SampleTimes...)
	if !result.FinishedAt.IsZero() {
		samples = append(samples, result.FinishedAt)
	}
	sort.Slice(samples, func(left, right int) bool {
		return samples[left].Before(samples[right])
	})
	samples = slices.Compact(samples)
	if len(samples) > maxEvidenceSamples {
		step := float64(len(samples)-1) / float64(maxEvidenceSamples-1)
		bounded := make([]time.Time, 0, maxEvidenceSamples)
		for index := 0; index < maxEvidenceSamples; index++ {
			bounded = append(bounded, samples[int(float64(index)*step)])
		}
		samples = bounded
	}

	return samples
}

func maximumSampleGap(samples []time.Time) time.Duration {
	var maximum time.Duration
	for index := 1; index < len(samples); index++ {
		if gap := samples[index].Sub(samples[index-1]); gap > maximum {
			maximum = gap
		}
	}

	return maximum
}

func decodeQualificationMeasurements(
	content string,
	participantIndex int,
	backendKind string,
) (*qualificationMeasurement, error) {
	if strings.TrimSpace(content) == "" {
		return nil, nil
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &envelope); err != nil {
		return nil, fmt.Errorf("decode qualification measurements: %w", err)
	}
	raw, exists := envelope["qualification"]
	if !exists {
		return nil, nil
	}

	var measurement qualificationMeasurement
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&measurement); err != nil {
		return nil, fmt.Errorf("decode qualification measurements: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("qualification measurements contain trailing JSON values")
	}

	if backendKind == string(signalv1.ExecutionBackendMPIJob) {
		var selected *participantQualificationMeasurement
		seen := map[int]struct{}{}
		for index := range measurement.Participants {
			participant := &measurement.Participants[index]
			if participant.ParticipantIndex < 0 || participant.ParticipantIndex >= MaxParticipants {
				return nil, errors.New("qualification participant index is invalid")
			}
			if _, duplicate := seen[participant.ParticipantIndex]; duplicate {
				return nil, errors.New("qualification measurements repeat a participant index")
			}
			seen[participant.ParticipantIndex] = struct{}{}
			if participant.ParticipantIndex == participantIndex {
				selected = participant
			}
		}
		if selected == nil {
			return nil, errors.New("qualification measurements do not identify the MPI participant")
		}
		returnMeasurement := &qualificationMeasurement{
			DriverVersion:  selected.DriverVersion,
			VisibleDevices: selected.VisibleDevices,
			CoveredDevices: selected.CoveredDevices,
		}
		measurement = *returnMeasurement
	} else if len(measurement.Participants) != 0 {
		return nil, errors.New("Pod qualification measurements must not contain participant groups")
	}

	for name, devices := range map[string][]string{
		"visibleDevices": measurement.VisibleDevices,
		"coveredDevices": measurement.CoveredDevices,
	} {
		if len(devices) > maxEvidenceDevices {
			return nil, fmt.Errorf("%s exceeds %d entries", name, maxEvidenceDevices)
		}
		for _, device := range devices {
			if device == "" || len(device) > maxEvidenceDeviceBytes {
				return nil, fmt.Errorf("%s contains an invalid device identity", name)
			}
		}
	}
	if len(measurement.DriverVersion) > 128 {
		return nil, errors.New("driverVersion exceeds 128 bytes")
	}

	measurement.VisibleDevices = normalizedStringSet(measurement.VisibleDevices)
	measurement.CoveredDevices = normalizedStringSet(measurement.CoveredDevices)

	return &measurement, nil
}

func normalizedStringSet(values []string) []string {
	normalized := slices.Clone(values)
	sort.Strings(normalized)

	return slices.Compact(normalized)
}

func sameStringSet(left, right []string) bool {
	return slices.Equal(normalizedStringSet(left), normalizedStringSet(right))
}

func deviceInventoryDigest(devices []string) string {
	sum := sha256.Sum256([]byte(strings.Join(normalizedStringSet(devices), "\x00")))

	return "sha256:" + hex.EncodeToString(sum[:])
}

func marshalBackendEvidence(evidence BackendEvidence, limit int) ([]byte, bool, error) {
	if limit == 0 {
		limit = MaxBackendEvidenceBytes
	}

	if limit < 1 || limit > MaxBackendEvidenceBytes {
		return nil, false, fmt.Errorf("backend evidence limit must be between 1 and %d bytes", MaxBackendEvidenceBytes)
	}

	evidence.CaptureError = boundedRedactedText(evidence.CaptureError, maxEvidenceTextBytes)
	for index := range evidence.Participants {
		evidence.Participants[index].CaptureError = boundedRedactedText(
			evidence.Participants[index].CaptureError,
			maxEvidenceTextBytes,
		)
	}

	content, err := json.Marshal(evidence)
	if err != nil {
		return nil, false, fmt.Errorf("encode backend evidence: %w", err)
	}

	if len(content) <= limit {
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

	if len(content) <= limit {
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

	if len(content) > limit {
		return nil, false, fmt.Errorf("reduced backend evidence exceeds %d bytes", limit)
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

func markBackendEvidenceIncomplete(content, captureError string, limit int) (string, error) {
	var evidence BackendEvidence

	if err := json.Unmarshal([]byte(content), &evidence); err != nil {
		return "", fmt.Errorf("decode backend evidence after persistence failure: %w", err)
	}

	evidence.Complete = false
	evidence.CaptureError = boundedRedactedText(captureError, maxEvidenceTextBytes)

	bounded, _, err := marshalBackendEvidence(evidence, limit)
	if err != nil {
		return "", err
	}

	return string(bounded), nil
}

func (r *Runner) capturePodBackendEvidence(
	ctx context.Context,
	config Config,
	expected signalv1.Participant,
	backend createdResource,
) (ParticipantBackendEvidence, bool) {
	participant := ParticipantBackendEvidence{
		ParticipantIndex: int(expected.Index),
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
	} else if object.GetUID() != backend.resource.GetUID() {
		participant.CaptureError = "backend object UID changed before evidence capture"
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

	pod, podErr := r.Kubernetes.CoreV1().Pods(config.Namespace).
		Get(ctx, backend.resource.GetName(), metav1.GetOptions{})
	if podErr != nil {
		participant.CaptureError = joinCaptureError(participant.CaptureError, "get participant Pod: "+podErr.Error())

		return participant, false
	}

	if pod.UID != backend.resource.GetUID() {
		participant.CaptureError = joinCaptureError(participant.CaptureError, "participant Pod UID changed before evidence capture")

		return participant, false
	}

	podEvidence, podComplete := r.capturePodEvidence(ctx, config.Namespace, pod, "participant")
	participant.Pods = []PodEvidence{podEvidence}
	complete = complete && podComplete

	if podEvidence.NodeName != expected.NodeName || podEvidence.NodeUID != expected.NodeUID {
		participant.CaptureError = joinCaptureError(
			participant.CaptureError,
			"participant Pod ran on a different Node identity than the immutable participant",
		)
		complete = false
	}

	return participant, complete
}

func (r *Runner) captureMPIBackendEvidence(
	ctx context.Context,
	config Config,
	participants []signalv1.Participant,
	backend createdResource,
) ([]ParticipantBackendEvidence, bool) {
	evidence := make([]ParticipantBackendEvidence, len(backend.participantIndexes))
	complete := len(backend.participantIndexes) == len(participants)

	resource := BackendObjectEvidence{
		APIVersion: backend.resource.GetAPIVersion(),
		Kind:       backend.resource.GetKind(),
		Namespace:  backend.resource.GetNamespace(),
		Name:       backend.resource.GetName(),
		UID:        backend.resource.GetUID(),
	}

	object, err := r.Dynamic.Resource(backend.gvr).Namespace(config.Namespace).
		Get(ctx, backend.resource.GetName(), metav1.GetOptions{})
	objectCaptureError := ""

	if err != nil {
		complete = false
		objectCaptureError = "get backend object: " + err.Error()
	} else if object.GetUID() != backend.resource.GetUID() {
		complete = false
		objectCaptureError = "backend object UID changed before evidence capture"
	} else {
		projected, projectErr := projectBackendObject(object)
		if projectErr != nil {
			complete = false
			objectCaptureError = "project backend object: " + projectErr.Error()
		} else {
			resource = projected
		}
	}

	for index, participantIndex := range backend.participantIndexes {
		evidence[index] = ParticipantBackendEvidence{
			ParticipantIndex: int(participantIndex),
			Resource:         resource,
		}
		if objectCaptureError != "" {
			evidence[index].CaptureError = boundedRedactedText(objectCaptureError, maxEvidenceTextBytes)
		}
	}

	var launcherJob *JobEvidence

	jobs, jobErr := r.Kubernetes.BatchV1().Jobs(config.Namespace).List(ctx, metav1.ListOptions{})
	jobUIDs := map[types.UID]struct{}{}
	launcherJobCount := 0

	if jobErr != nil {
		complete = false
	} else {
		for index := range jobs.Items {
			job := &jobs.Items[index]
			if !ownedBy(job.OwnerReferences, backend.resource.GetUID()) {
				continue
			}

			launcherJobCount++
			jobUIDs[job.UID] = struct{}{}
			projected := projectJob(job)
			launcherJob = &projected
		}
	}

	var launcherPod *corev1.Pod

	pods, podErr := r.Kubernetes.CoreV1().Pods(config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: runLabelSelector(config.ExecutionName),
	})
	if podErr != nil {
		complete = false
	}

	workerPods := make(map[string]*corev1.Pod, len(participants))
	launcherPodCount := 0
	workerPodCount := 0

	if podErr == nil {
		for index := range pods.Items {
			pod := &pods.Items[index]
			switch mpiPodRole(pod, backend.resource.GetUID(), jobUIDs) {
			case "launcher":
				launcherPodCount++
				launcherPod = pod
			case "worker":
				workerPodCount++

				if _, duplicate := workerPods[pod.Spec.NodeName]; duplicate {
					complete = false
				}

				workerPods[pod.Spec.NodeName] = pod
			}
		}
	}

	if launcherJobCount != 1 || launcherPodCount != 1 || workerPodCount != len(participants) {
		complete = false
	}

	if len(evidence) > 0 {
		if jobErr != nil {
			evidence[0].CaptureError = joinCaptureError(
				evidence[0].CaptureError,
				"list launcher Jobs: "+jobErr.Error(),
			)
		} else if launcherJobCount != 1 {
			evidence[0].CaptureError = joinCaptureError(
				evidence[0].CaptureError,
				fmt.Sprintf("expected exactly one MPI launcher Job, found %d", launcherJobCount),
			)
		} else {
			evidence[0].LauncherJob = launcherJob
		}

		if podErr != nil {
			evidence[0].CaptureError = joinCaptureError(
				evidence[0].CaptureError,
				"list MPI Pods: "+podErr.Error(),
			)
		} else if launcherPodCount != 1 {
			evidence[0].CaptureError = joinCaptureError(
				evidence[0].CaptureError,
				fmt.Sprintf("expected exactly one MPI launcher Pod, found %d", launcherPodCount),
			)
		} else {
			podEvidence, podComplete := r.capturePodEvidence(ctx, config.Namespace, launcherPod, "launcher")
			evidence[0].Pods = append(evidence[0].Pods, podEvidence)
			complete = complete && podComplete
		}
	}

	for evidenceIndex, participantIndex := range backend.participantIndexes {
		if participantIndex < 0 || int(participantIndex) >= len(participants) {
			evidence[evidenceIndex].CaptureError = joinCaptureError(
				evidence[evidenceIndex].CaptureError,
				fmt.Sprintf("participant index %d is out of range", participantIndex),
			)
			complete = false

			continue
		}

		expected := participants[participantIndex]

		pod := workerPods[expected.NodeName]
		if pod == nil {
			evidence[evidenceIndex].CaptureError = joinCaptureError(
				evidence[evidenceIndex].CaptureError,
				"MPI worker Pod was not found on the immutable participant Node",
			)
			complete = false

			continue
		}

		podEvidence, podComplete := r.capturePodEvidence(ctx, config.Namespace, pod, "worker")
		evidence[evidenceIndex].Pods = append(evidence[evidenceIndex].Pods, podEvidence)
		complete = complete && podComplete

		if podEvidence.NodeUID != expected.NodeUID {
			evidence[evidenceIndex].CaptureError = joinCaptureError(
				evidence[evidenceIndex].CaptureError,
				"MPI worker ran on a different Node UID than the immutable participant",
			)
			complete = false
		}
	}

	if workerPodCount != len(participants) && len(evidence) > 0 {
		evidence[0].CaptureError = joinCaptureError(
			evidence[0].CaptureError,
			fmt.Sprintf("expected %d MPI worker Pods, found %d", len(participants), workerPodCount),
		)
	}

	return evidence, complete
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
		NodeName:   pod.Spec.NodeName,
		Phase:      pod.Status.Phase,
		Reason:     boundedRedactedText(pod.Status.Reason, maxEvidenceTextBytes),
		Message:    boundedRedactedText(pod.Status.Message, maxEvidenceTextBytes),
		Conditions: projectPodConditions(pod.Status.Conditions),
	}
	complete := true

	if pod.Spec.NodeName == "" {
		evidence.NodeError = "Pod is not assigned to a Node"
		complete = false
	} else {
		node, err := r.Kubernetes.CoreV1().Nodes().Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
		if err != nil {
			evidence.NodeError = boundedRedactedText("get Pod Node: "+err.Error(), maxEvidenceTextBytes)
			complete = false
		} else {
			evidence.NodeUID = node.UID
		}
	}

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
	begin := strings.Index(content, "BEGIN ")
	end := strings.Index(content, "END ")
	inPrivateKey := end >= 0 && (begin < 0 || end < begin) && strings.Contains(content[end:], "PRIVATE KEY")

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

func runLabelSelector(runName string) string {
	return "signal.unbounded-cloud.io/run=" + runName
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

	if evidence.Identity.RunUID == "" || evidence.Identity.CheckID == "" || evidence.Identity.CheckOrder < 0 {
		return errors.New("backend evidence identity is incomplete")
	}
	if evidence.Identity.PlanDigest != "" && !signalv1.IsSHA256Digest(evidence.Identity.PlanDigest) {
		return errors.New("backend evidence plan digest is invalid")
	}
	if evidence.Provenance.RunnerImage == "" ||
		!runnerImagePattern.MatchString(evidence.Provenance.RunnerImage) {
		return errors.New("backend evidence runner image is not digest pinned")
	}
	if evidence.Provenance.AdapterImage != "" &&
		!runnerImagePattern.MatchString(evidence.Provenance.AdapterImage) {
		return errors.New("backend evidence adapter image is not digest pinned")
	}
	if evidence.Identity.PlanDigest != "" && evidence.Provenance.AdapterImage == "" {
		return errors.New("backend evidence with a plan digest requires adapter provenance")
	}
	if evidence.Timing.StartedAt.IsZero() || evidence.Timing.FinishedAt.IsZero() ||
		evidence.Timing.FinishedAt.Before(&evidence.Timing.StartedAt) {
		return errors.New("backend evidence timing is incomplete")
	}
	if evidence.Timing.SampleCount < 1 || evidence.Timing.SampleCount > MaxParticipants*(maxEvidenceSamples+2) {
		return errors.New("backend evidence sample count is invalid")
	}

	if len(evidence.Participants) < 1 || len(evidence.Participants) > MaxParticipants {
		return fmt.Errorf(
			"backend evidence must contain between 1 and %d participants",
			MaxParticipants,
		)
	}

	seen := make(map[int]struct{}, len(evidence.Participants))
	for index := range evidence.Participants {
		participant := &evidence.Participants[index]
		if participant.ParticipantIndex < 0 || participant.ParticipantIndex >= MaxParticipants {
			return fmt.Errorf("backend evidence participant %d is out of range", participant.ParticipantIndex)
		}
		if _, exists := seen[participant.ParticipantIndex]; exists {
			return fmt.Errorf("backend evidence repeats participant %d", participant.ParticipantIndex)
		}
		seen[participant.ParticipantIndex] = struct{}{}
		if participant.Start.NodeUID == "" || participant.End.NodeUID == "" ||
			participant.Start.NodeUID != participant.End.NodeUID {
			return fmt.Errorf("backend evidence participant %d has invalid node identity", participant.ParticipantIndex)
		}
		if err := validateVerdictEvidence(participant.Verdict); err != nil {
			return fmt.Errorf("backend evidence participant %d verdict: %w", participant.ParticipantIndex, err)
		}
		if len(participant.VisibleDevices) > maxEvidenceDevices ||
			len(participant.CoveredDevices) > maxEvidenceDevices {
			return fmt.Errorf("backend evidence participant %d device evidence exceeds bounds", participant.ParticipantIndex)
		}
	}
	if evidence.GroupResult != nil {
		if err := validateVerdictEvidence(*evidence.GroupResult); err != nil {
			return fmt.Errorf("backend evidence group verdict: %w", err)
		}
		if evidence.GroupResult.Attribution != "group" {
			return errors.New("backend evidence group verdict attribution is invalid")
		}
	}

	return nil
}

func ValidateBackendEvidenceBinding(
	content string,
	runUID types.UID,
	checkID signalv1.CheckID,
	checkOrder int32,
	planDigest string,
) error {
	var evidence BackendEvidence
	if err := json.Unmarshal([]byte(content), &evidence); err != nil {
		return fmt.Errorf("decode backend evidence binding: %w", err)
	}

	if evidence.Identity.RunUID != runUID ||
		evidence.Identity.CheckID != checkID ||
		evidence.Identity.CheckOrder != checkOrder ||
		evidence.Identity.PlanDigest != planDigest {
		return errors.New("backend evidence identity does not match the immutable Run check")
	}

	return nil
}

func validateVerdictEvidence(verdict VerdictEvidence) error {
	if verdict.ExecutionState == "" || verdict.Reason == "" {
		return errors.New("execution state and reason are required")
	}
	if len(verdict.Reason) > MaxReasonBytes {
		return fmt.Errorf("reason exceeds %d bytes", MaxReasonBytes)
	}
	switch verdict.Verdict {
	case "Pass", "Fail", "Inconclusive":
	default:
		return errors.New("verdict is invalid")
	}
	switch verdict.Attribution {
	case "participant", "group", "group-with-participant-observation":
	default:
		return errors.New("attribution is invalid")
	}

	return nil
}
