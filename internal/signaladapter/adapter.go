// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signaladapter

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
	"github.com/Azure/unbounded/internal/signalworkload"
)

const (
	MaxParticipants         = 64
	MaxCheckParticipants    = 8
	MaxReasonBytes          = 512
	MaxMeasurementsBytes    = 16 * 1024
	MaxBackendOutputBytes   = 8 * 1024
	MaxBackendEvidenceBytes = 24 * 1024
	maxMPIWorkerReadyWait   = int64(30)
	maxMPIWorkerSlots       = int64(1<<31 - 1)
	mpiBackoffLimit         = int64(0)
	mpiHostfilePath         = "/tmp/project-signal-hostfile"
	mpiKnownHostsPath       = "/tmp/project-signal-known-hosts"
	resultPrefix            = "PROJECT_SIGNAL_RESULT_V2 "
)

var runnerImagePattern = regexp.MustCompile(`^[^@\s]+@sha256:[0-9a-f]{64}$`)

var reservedEnvironmentNames = map[string]struct{}{
	"BASH_ENV":        {},
	"BASHOPTS":        {},
	"CDPATH":          {},
	"ENV":             {},
	"GLOBIGNORE":      {},
	"HOST_PROC_ROOT":  {},
	"HOST_SYSFS_ROOT": {},
	"IFS":             {},
	"LD_LIBRARY_PATH": {},
	"LD_PRELOAD":      {},
	"PATH":            {},
	"PYTHONHOME":      {},
	"PYTHONPATH":      {},
	"SHELLOPTS":       {},
}

type Config struct {
	Namespace              string
	WorkflowName           string
	RunName                string
	RunUID                 types.UID
	ParticipantsJSON       string
	ParticipantCount       int
	ScriptConfigMap        string
	ScriptKey              string
	ScriptDigest           string
	RunnerImage            string
	BackendKind            string
	BackendDeadlineSeconds int64
	ScriptEnvironmentJSON  string
	WorkloadPolicyJSON     string
	GPUResourceCount       string
}

type Result struct {
	ParticipantIndex int                `json:"participantIndex"`
	ExecutionState   string             `json:"executionState"`
	Verdict          string             `json:"verdict"`
	Reason           string             `json:"reason"`
	Measurements     string             `json:"measurements"`
	ResultLog        *ResultLogMetadata `json:"resultLog,omitempty"`
	ExitCode         int                `json:"-"`
}

type ResultLogSourceMetadata struct {
	Status string `json:"status"`
	Bytes  int    `json:"bytes,omitempty"`
}

type ResultLogMetadata struct {
	Current        ResultLogSourceMetadata `json:"current"`
	Previous       ResultLogSourceMetadata `json:"previous"`
	SelectedSource string                  `json:"selectedSource,omitempty"`
}

type mpiWorkerHost struct {
	Name  string
	Slots int64
}

type AggregateOutput struct {
	ExecutionState   string                              `json:"executionState"`
	Verdict          string                              `json:"verdict"`
	Reason           string                              `json:"reason"`
	Measurements     string                              `json:"measurements"`
	BackendResources []signalv1.BackendResourceReference `json:"-"`
	BackendJSON      string                              `json:"backend-resources"`
	BackendEvidence  string                              `json:"backend-evidence"`
}

func ParseParticipants(
	participantsJSON string,
	participantCount int,
) ([]signalv1.Participant, error) {
	if participantCount < 1 || participantCount > MaxCheckParticipants {
		return nil, fmt.Errorf("participant-count must be between 1 and %d", MaxCheckParticipants)
	}

	if strings.TrimSpace(participantsJSON) == "" {
		return nil, errors.New("participants-json is required")
	}

	var participants []signalv1.Participant

	decoder := json.NewDecoder(strings.NewReader(participantsJSON))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&participants); err != nil {
		return nil, fmt.Errorf("decode participants-json: %w", err)
	}

	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("participants-json contains trailing values")
		}

		return nil, fmt.Errorf("decode trailing participants-json value: %w", err)
	}

	if len(participants) != participantCount {
		return nil, fmt.Errorf(
			"participant-count %d does not match participants-json length %d",
			participantCount,
			len(participants),
		)
	}

	names := make(map[string]struct{}, len(participants))
	uids := make(map[types.UID]struct{}, len(participants))

	for index := range participants {
		participant := &participants[index]
		participant.Index = int32(index)

		if errors := validation.IsDNS1123Subdomain(participant.NodeName); len(errors) > 0 {
			return nil, fmt.Errorf("participants-json[%d].nodeName is invalid: %s", index, strings.Join(errors, ", "))
		}

		if _, duplicate := names[participant.NodeName]; duplicate {
			return nil, fmt.Errorf("participants-json[%d].nodeName duplicates %q", index, participant.NodeName)
		}

		names[participant.NodeName] = struct{}{}

		if strings.TrimSpace(participantsJSON) == "" {
			continue
		}

		if participant.NodeUID == "" {
			return nil, fmt.Errorf("participants-json[%d].nodeUID is required", index)
		}

		if _, duplicate := uids[participant.NodeUID]; duplicate {
			return nil, fmt.Errorf("participants-json[%d].nodeUID duplicates %q", index, participant.NodeUID)
		}

		uids[participant.NodeUID] = struct{}{}
	}

	return participants, nil
}

func ValidateConfig(config Config) ([]signalv1.Participant, []corev1.Toleration, []corev1.EnvVar, error) {
	for name, value := range map[string]string{
		"namespace":        config.Namespace,
		"workflow-name":    config.WorkflowName,
		"run-name":         config.RunName,
		"run-uid":          string(config.RunUID),
		"script-configmap": config.ScriptConfigMap,
		"script-key":       config.ScriptKey,
		"script-digest":    config.ScriptDigest,
		"runner-image":     config.RunnerImage,
		"backend-kind":     config.BackendKind,
	} {
		if value == "" {
			return nil, nil, nil, fmt.Errorf("%s is required", name)
		}
	}

	for name, value := range map[string]string{
		"namespace":        config.Namespace,
		"workflow-name":    config.WorkflowName,
		"run-name":         config.RunName,
		"script-configmap": config.ScriptConfigMap,
	} {
		if errors := validation.IsDNS1123Subdomain(value); len(errors) > 0 {
			return nil, nil, nil, fmt.Errorf("%s is invalid: %s", name, strings.Join(errors, ", "))
		}
	}

	if errors := validation.IsConfigMapKey(config.ScriptKey); len(errors) > 0 {
		return nil, nil, nil, fmt.Errorf("script-key is invalid: %s", strings.Join(errors, ", "))
	}

	if !runnerImagePattern.MatchString(config.RunnerImage) {
		return nil, nil, nil, errors.New("runner-image must be an immutable sha256 digest reference")
	}

	if !signalv1.IsSHA256Digest(config.ScriptDigest) {
		return nil, nil, nil, errors.New(
			"script-digest must be an algorithm-qualified SHA-256 digest (sha256:<64 lowercase hexadecimal characters>)",
		)
	}

	if config.BackendDeadlineSeconds <= 0 {
		return nil, nil, nil, errors.New("backend-active-deadline-seconds must be positive")
	}

	switch signalv1.ExecutionBackend(config.BackendKind) {
	case signalv1.ExecutionBackendPod:
	case signalv1.ExecutionBackendMPIJob:
		if config.BackendDeadlineSeconds < 2 {
			return nil, nil, nil, errors.New(
				"backend-active-deadline-seconds must be at least 2 for MPIJob worker readiness",
			)
		}
	default:
		return nil, nil, nil, fmt.Errorf("unsupported backend %q", config.BackendKind)
	}

	if _, err := signalworkload.Decode(config.WorkloadPolicyJSON); err != nil {
		return nil, nil, nil, err
	}

	participants, err := ParseParticipants(
		config.ParticipantsJSON,
		config.ParticipantCount,
	)
	if err != nil {
		return nil, nil, nil, err
	}

	minimumEvidenceBytes := MinimumBackendEvidenceBytes(len(participants))
	if minimumEvidenceBytes > MaxBackendEvidenceBytes {
		return nil, nil, nil, fmt.Errorf(
			"fixed backend evidence limit %d cannot represent %d participants; requires at least %d bytes",
			MaxBackendEvidenceBytes,
			len(participants),
			minimumEvidenceBytes,
		)
	}

	tolerations := []corev1.Toleration{{
		Key:      "nvidia.com/gpu",
		Operator: corev1.TolerationOpExists,
		Effect:   corev1.TaintEffectNoSchedule,
	}, {
		Key:      corev1.TaintNodeUnschedulable,
		Operator: corev1.TolerationOpExists,
		Effect:   corev1.TaintEffectNoSchedule,
	}}

	var environment map[string]string
	if err := decodeJSONObject(config.ScriptEnvironmentJSON, &environment); err != nil {
		return nil, nil, nil, fmt.Errorf("decode script-environment: %w", err)
	}

	if len(environment) > 32 {
		return nil, nil, nil, errors.New("script-environment must not contain more than 32 entries")
	}

	environmentNames := make([]string, 0, len(environment))
	for name := range environment {
		environmentNames = append(environmentNames, name)
	}

	slices.Sort(environmentNames)

	env := make([]corev1.EnvVar, 0, len(environmentNames)+4)
	for _, name := range environmentNames {
		if len(name) > 253 {
			return nil, nil, nil, fmt.Errorf("script-environment name %q exceeds 253 bytes", name)
		}

		if errors := validation.IsEnvVarName(name); len(errors) > 0 {
			return nil, nil, nil, fmt.Errorf("script-environment contains invalid name %q", name)
		}

		if strings.HasPrefix(name, "PROJECT_SIGNAL_") {
			return nil, nil, nil, fmt.Errorf("script-environment contains reserved name %q", name)
		}

		if _, reserved := reservedEnvironmentNames[name]; reserved {
			return nil, nil, nil, fmt.Errorf("script-environment contains reserved name %q", name)
		}

		if len(environment[name]) > 4096 {
			return nil, nil, nil, fmt.Errorf("script-environment value for %q exceeds 4096 bytes", name)
		}

		env = append(env, corev1.EnvVar{Name: name, Value: environment[name]})
	}

	return participants, tolerations, env, nil
}

func RenderResources(
	config Config,
	participants []signalv1.Participant,
	tolerations []corev1.Toleration,
	environment []corev1.EnvVar,
	workflow metav1.OwnerReference,
) ([]*unstructured.Unstructured, error) {
	resources := make([]*unstructured.Unstructured, 0, len(participants))

	gpuCount, err := strconv.ParseInt(config.GPUResourceCount, 10, 64)
	if err != nil || gpuCount < 0 {
		return nil, fmt.Errorf("gpu-resource-count must be a nonnegative integer")
	}

	if config.BackendKind == "MPIJob" && gpuCount < 1 {
		return nil, errors.New("gpu-resource-count must be positive for MPIJob execution")
	}

	policy, err := signalworkload.Decode(config.WorkloadPolicyJSON)
	if err != nil {
		return nil, err
	}

	gpuResource := corev1.ResourceName("nvidia.com/gpu")
	if _, exists := policy.Resources.Requests[gpuResource]; exists {
		return nil, errors.New("workload policy must not set nvidia.com/gpu requests; use gpu-resource-count")
	}

	if _, exists := policy.Resources.Limits[gpuResource]; exists {
		return nil, errors.New("workload policy must not set nvidia.com/gpu limits; use gpu-resource-count")
	}

	for _, participant := range participants {
		var object any

		switch config.BackendKind {
		case "Pod":
			object = ordinaryPod(
				config,
				participant,
				tolerations,
				environment,
				policy,
				workflow,
				config.BackendDeadlineSeconds,
				gpuCount,
			)
		case "MPIJob":
			object, err = mpiJob(
				config,
				participant,
				tolerations,
				environment,
				policy,
				workflow,
				config.BackendDeadlineSeconds,
				gpuCount,
			)
			if err != nil {
				return nil, fmt.Errorf("render MPIJob for participant %d: %w", participant.Index, err)
			}
		default:
			return nil, fmt.Errorf("unsupported backend kind %q", config.BackendKind)
		}

		content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(object)
		if err != nil {
			return nil, fmt.Errorf("convert %s for participant %d: %w", config.BackendKind, participant.Index, err)
		}

		resources = append(resources, &unstructured.Unstructured{Object: content})
	}

	return resources, nil
}

func BackendResource(resource *unstructured.Unstructured, participantIndex int32) (signalv1.BackendResourceReference, error) {
	label := resource.GetLabels()["signal.unbounded-cloud.io/participant-index"]
	if label != strconv.FormatInt(int64(participantIndex), 10) {
		return signalv1.BackendResourceReference{}, fmt.Errorf(
			"backend resource participant label %q does not match index %d",
			label,
			participantIndex,
		)
	}

	return backendResourceIdentity(resource, participantIndex)
}

func backendResourceIdentity(
	resource *unstructured.Unstructured,
	participantIndex int32,
) (signalv1.BackendResourceReference, error) {
	if resource.GetAPIVersion() == "" ||
		resource.GetKind() == "" ||
		resource.GetName() == "" ||
		resource.GetUID() == "" {
		return signalv1.BackendResourceReference{}, errors.New("backend resource has an incomplete identity")
	}

	return signalv1.BackendResourceReference{
		Name:               resource.GetName(),
		UID:                resource.GetUID(),
		ParticipantIndexes: []int32{participantIndex},
	}, nil
}

func MinimumPartialReasonBytes() int {
	return len("BackendSetupFailed")
}

func MinimumPartialMeasurementsBytes(participantCount int) int {
	results := make([]Result, participantCount)
	for index := range results {
		results[index] = Result{
			ParticipantIndex: index,
			ExecutionState:   "Error",
			Verdict:          "Inconclusive",
			Reason:           "BackendSetupFailed",
		}
	}

	content, err := json.Marshal(map[string]any{
		"participantCount": participantCount,
		"createdBackends":  participantCount,
		"error":            strings.Repeat("\x00", maxEvidenceTextBytes),
		"results":          results,
	})
	if err != nil {
		return MaxMeasurementsBytes + 1
	}

	return len(content) + 64
}

func MinimumPartialBackendResourcesBytes(participantCount int) int {
	references := make([]signalv1.BackendResourceReference, participantCount)
	for index := range references {
		references[index] = signalv1.BackendResourceReference{
			Name:               strings.Repeat("a", 63),
			UID:                types.UID(strings.Repeat("a", 128)),
			ParticipantIndexes: []int32{int32(index)},
		}
	}

	content, err := json.Marshal(references)
	if err != nil {
		return MaxBackendOutputBytes + 1
	}

	return len(content) + 64
}

func Aggregate(
	participantCount int,
	backendResources []signalv1.BackendResourceReference,
	results []Result,
) (AggregateOutput, error) {
	if participantCount < 1 || participantCount > MaxParticipants {
		return AggregateOutput{}, fmt.Errorf("participant-count must be between 1 and %d", MaxParticipants)
	}

	if err := validateBackendCoverage(participantCount, backendResources); err != nil {
		return AggregateOutput{}, err
	}

	if len(results) != participantCount {
		return AggregateOutput{}, fmt.Errorf("results must contain exactly %d entries", participantCount)
	}

	ordered := make([]Result, participantCount)
	seen := make([]bool, participantCount)
	executionFailed := false
	executionFailureReason := ""
	diagnosticFailed := false
	allPassed := true

	for index, result := range results {
		if result.ParticipantIndex < 0 || result.ParticipantIndex >= participantCount {
			return AggregateOutput{}, fmt.Errorf("results[%d].participantIndex is out of range", index)
		}

		if seen[result.ParticipantIndex] {
			return AggregateOutput{}, fmt.Errorf("results repeats participant index %d", result.ParticipantIndex)
		}

		seen[result.ParticipantIndex] = true

		if result.ExecutionState == "" || result.Reason == "" {
			return AggregateOutput{}, fmt.Errorf("results[%d] requires executionState and reason", index)
		}

		if len(result.Reason) > MaxReasonBytes {
			return AggregateOutput{}, fmt.Errorf("results[%d].reason exceeds %d bytes", index, MaxReasonBytes)
		}

		switch result.Verdict {
		case "Pass":
		case "Fail":
			diagnosticFailed = true
			allPassed = false
		case "Inconclusive":
			allPassed = false
		default:
			return AggregateOutput{}, fmt.Errorf("results[%d].verdict is invalid", index)
		}

		if result.ExecutionState != "Succeeded" {
			executionFailed = true

			if executionFailureReason == "" {
				executionFailureReason = result.Reason
			}
		}

		if len(result.Measurements) > MaxMeasurementsBytes {
			return AggregateOutput{}, fmt.Errorf("results[%d].measurements exceeds %d bytes", index, MaxMeasurementsBytes)
		}

		ordered[result.ParticipantIndex] = result
	}

	for index, present := range seen {
		if !present {
			return AggregateOutput{}, fmt.Errorf("results does not cover participant index %d", index)
		}
	}

	backendJSON, err := json.Marshal(backendResources)
	if err != nil {
		return AggregateOutput{}, fmt.Errorf("encode backend-resources: %w", err)
	}

	if len(backendJSON) > MaxBackendOutputBytes {
		return AggregateOutput{}, fmt.Errorf("backend-resources exceeds %d bytes", MaxBackendOutputBytes)
	}

	measurementsJSON, err := json.Marshal(map[string]any{
		"participantCount": participantCount,
		"results":          ordered,
	})
	if err != nil {
		return AggregateOutput{}, fmt.Errorf("encode measurements: %w", err)
	}

	if len(measurementsJSON) > MaxMeasurementsBytes {
		return AggregateOutput{}, fmt.Errorf("aggregate measurements exceeds %d bytes", MaxMeasurementsBytes)
	}

	output := AggregateOutput{
		ExecutionState:   "Succeeded",
		Verdict:          "Inconclusive",
		Reason:           "ParticipantCertificationInconclusive",
		Measurements:     string(measurementsJSON),
		BackendResources: backendResources,
		BackendJSON:      string(backendJSON),
	}

	switch {
	case executionFailed:
		output.ExecutionState = "Error"
		output.Reason = executionFailureReason
	case diagnosticFailed:
		output.Verdict = "Fail"
		output.Reason = "ParticipantCertificationFailed"
	case allPassed:
		output.Verdict = "Pass"
		output.Reason = "AllParticipantsPassed"
	}

	return output, nil
}

func ParseResultLog(logContent string, participantIndex int) (Result, error) {
	lines := strings.Split(logContent, "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		line := lines[index]
		if !strings.HasPrefix(line, resultPrefix) {
			continue
		}

		fields := strings.Fields(strings.TrimPrefix(line, resultPrefix))
		if len(fields) != 4 {
			return Result{}, errors.New("result record must contain verdict, reason, measurements, and exit code")
		}

		values := make([]string, 3)

		for fieldIndex, field := range fields[:3] {
			if field == "-" {
				continue
			}

			decoded, err := base64.StdEncoding.DecodeString(field)
			if err != nil {
				return Result{}, fmt.Errorf("decode result field %d: %w", fieldIndex, err)
			}

			values[fieldIndex] = string(decoded)
		}

		exitCode, err := strconv.Atoi(fields[3])
		if err != nil || exitCode < 0 || exitCode > 255 {
			return Result{}, errors.New("result exit code must be an integer between 0 and 255")
		}

		result := Result{
			ParticipantIndex: participantIndex,
			ExecutionState:   "Succeeded",
			Verdict:          values[0],
			Reason:           values[1],
			Measurements:     values[2],
			ExitCode:         exitCode,
		}
		if err := validateResult(result); err != nil {
			return Result{}, err
		}

		return result, nil
	}

	return Result{}, errors.New("workload log does not contain a Project Signal result record")
}

func validateResult(result Result) error {
	if result.Reason == "" {
		return errors.New("result reason must not be empty")
	}

	if len(result.Reason) > MaxReasonBytes {
		return fmt.Errorf("result reason exceeds %d bytes", MaxReasonBytes)
	}

	if len(result.Measurements) > MaxMeasurementsBytes {
		return fmt.Errorf("result measurements exceeds %d bytes", MaxMeasurementsBytes)
	}

	var expectedExitCode int

	switch result.Verdict {
	case "Pass":
		expectedExitCode = 0
	case "Fail":
		expectedExitCode = 1
	case "Inconclusive":
		expectedExitCode = 2
	default:
		return fmt.Errorf("result verdict %q is invalid", result.Verdict)
	}

	if result.ExitCode != expectedExitCode {
		return fmt.Errorf(
			"result verdict %s requires exit code %d, got %d",
			result.Verdict,
			expectedExitCode,
			result.ExitCode,
		)
	}

	return nil
}

func resultCommand(arguments ...string) []string {
	return resultCommandWithPrelude("", arguments...)
}

func resultCommandWithPrelude(prelude string, arguments ...string) []string {
	if strings.TrimSpace(prelude) == "" {
		prelude = ":"
	}

	quotedArguments := make([]string, len(arguments))
	for index, argument := range arguments {
		quotedArguments[index] = shellQuote(argument)
	}

	return []string{
		"/bin/bash",
		"-c",
		fmt.Sprintf(`
set -u
mkdir -p /results
chmod 0755 /results
rm -f /results/verdict /results/reason /results/measurements.txt
printf 'PROJECT_SIGNAL_RESULTS_V1 prepared\n'
set +e
script_path="/scripts/${PROJECT_SIGNAL_SCRIPT_KEY}"
expected_digest="${PROJECT_SIGNAL_SCRIPT_DIGEST#sha256:}"
actual_digest=""
verification_reason=""
if command -v sha256sum >/dev/null 2>&1; then
  actual_digest="$(sha256sum "${script_path}" | cut -d ' ' -f 1)"
elif command -v shasum >/dev/null 2>&1; then
  actual_digest="$(shasum -a 256 "${script_path}" | cut -d ' ' -f 1)"
else
  verification_reason="ScriptDigestVerificationUnavailable"
fi
if [[ -z "${verification_reason}" && "${actual_digest}" != "${expected_digest}" ]]; then
  verification_reason="ScriptDigestMismatch"
fi
if [[ -n "${verification_reason}" ]]; then
  printf 'Inconclusive' > /results/verdict
  printf '%%s' "${verification_reason}" > /results/reason
  status=125
else
  %s
  prelude_status=$?
  if [[ "${prelude_status}" -eq 0 ]]; then
    /bin/bash "${script_path}" %s
    status=$?
  else
    status="${prelude_status}"
  fi
fi
set -e
canonicalize_result_file() {
  if [[ -f "$1" ]]; then
    local value
    value="$(<"$1")"
    printf '%%s' "${value}" > "$1"
  fi
}
canonicalize_result_file /results/verdict
canonicalize_result_file /results/reason
canonicalize_result_file /results/measurements.txt
encode_result_file() {
  if [[ -f "$1" ]]; then
    base64 < "$1" | tr -d '\n'
  else
    printf '%%s' -
  fi
}
verdict="$(encode_result_file /results/verdict)"
reason="$(encode_result_file /results/reason)"
measurements="$(encode_result_file /results/measurements.txt)"
[[ -n "${verdict}" ]] || verdict=-
[[ -n "${reason}" ]] || reason=-
[[ -n "${measurements}" ]] || measurements=-
printf 'PROJECT_SIGNAL_RESULTS_V1 verdict=%%s reason=%%s measurements=%%s\n' \
  "$([[ -f /results/verdict ]] && printf present || printf missing)" \
  "$([[ -f /results/reason ]] && printf present || printf missing)" \
  "$([[ -f /results/measurements.txt ]] && printf present || printf missing)"
printf '%s%%s %%s %%s %%d\n' "${verdict}" "${reason}" "${measurements}" "$status"
exit "$status"
`, prelude, strings.Join(quotedArguments, " "), resultPrefix),
	}
}

func mpiWorkerReadinessTimeout(backendDeadlineSeconds int64) int64 {
	timeout := backendDeadlineSeconds - 1
	if timeout > maxMPIWorkerReadyWait {
		timeout = maxMPIWorkerReadyWait
	}

	return timeout
}

func mpiWorkerHosts(namespace, jobName string, workerCount, slots int64) ([]mpiWorkerHost, error) {
	if workerCount < 1 || workerCount > MaxParticipants {
		return nil, fmt.Errorf("MPI worker count must be between 1 and %d", MaxParticipants)
	}

	if slots < 1 || slots > maxMPIWorkerSlots {
		return nil, fmt.Errorf("MPI worker slots must be between 1 and %d", maxMPIWorkerSlots)
	}

	for field, value := range map[string]string{
		"namespace": namespace,
		"job name":  jobName,
	} {
		if errors := validation.IsDNS1123Label(value); len(errors) > 0 {
			return nil, fmt.Errorf("MPI %s %q is invalid: %s", field, value, strings.Join(errors, ", "))
		}
	}

	hosts := make([]mpiWorkerHost, workerCount)
	for index := range workerCount {
		workerName := fmt.Sprintf("%s-worker-%d", jobName, index)
		if errors := validation.IsDNS1123Label(workerName); len(errors) > 0 {
			return nil, fmt.Errorf(
				"generated MPI worker name %q is invalid: %s",
				workerName,
				strings.Join(errors, ", "),
			)
		}

		name := fmt.Sprintf(
			"%s.%s.%s.svc",
			workerName,
			jobName,
			namespace,
		)
		if errors := validation.IsDNS1123Subdomain(name); len(errors) > 0 {
			return nil, fmt.Errorf("generated MPI worker host %q is invalid: %s", name, strings.Join(errors, ", "))
		}

		hosts[index] = mpiWorkerHost{Name: name, Slots: slots}
	}

	return hosts, nil
}

func mpiLauncherPrelude(hostfile string, hosts []mpiWorkerHost, timeoutSeconds int64) string {
	hostfileLines := make([]string, len(hosts))

	hostArguments := make([]string, len(hosts))
	for index, host := range hosts {
		hostfileLines[index] = fmt.Sprintf("%s slots=%d", host.Name, host.Slots)
		hostArguments[index] = shellQuote(host.Name)
	}

	return fmt.Sprintf(
		`
umask 077
printf '%%s\n' %s > %s
export OMPI_MCA_orte_default_hostfile=%s
wait_for_mpi_workers() {
  local hostfile=%s
  local timeout_seconds=%d
  local deadline=$((SECONDS + timeout_seconds))
  local host all_ready last_unready remaining
  local -a hosts=(%s)
  while (( SECONDS < deadline )); do
    all_ready=1
    for host in "${hosts[@]}"; do
      remaining=$((deadline - SECONDS))
      if (( remaining < 1 )) || ! timeout "${remaining}s" ssh \
        -o BatchMode=yes \
        -o ConnectTimeout=2 \
        -o ConnectionAttempts=1 \
        -o StrictHostKeyChecking=accept-new \
        -o UserKnownHostsFile=%s \
        -- "${host}" /bin/true </dev/null >/dev/null; then
        all_ready=0
        last_unready="${host}"
        break
      fi
    done
    if (( all_ready == 1 )); then
      return 0
    fi
    sleep 1
  done
  printf 'Project Signal MPI readiness timed out after %%ds waiting for every worker in %%s to resolve and accept SSH/22; last unready host=%%s\n' \
    "${timeout_seconds}" "${hostfile}" "${last_unready:-unknown}" >&2
  return 124
}
wait_for_mpi_workers
`,
		strings.Join(quotedShellArguments(hostfileLines), " "),
		shellQuote(hostfile),
		shellQuote(hostfile),
		shellQuote(hostfile),
		timeoutSeconds,
		strings.Join(hostArguments, " "),
		shellQuote(mpiKnownHostsPath),
	)
}

func ordinaryPod(
	config Config,
	participant signalv1.Participant,
	tolerations []corev1.Toleration,
	environment []corev1.EnvVar,
	policy signalv1.WorkloadPolicy,
	workflow metav1.OwnerReference,
	backendDeadlineSeconds int64,
	gpuCount int64,
) *corev1.Pod {
	labels := resourceLabels(config, participant)

	env := workloadEnvironment(config, participant, environment)
	container := workloadContainer(
		config,
		policy,
		corev1.Container{
			Name:    "main",
			Image:   config.RunnerImage,
			Command: resultCommand(),
			Env:     env,
		},
		gpuCount,
	)

	pod := &corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			Name:            resourceName(config.RunName, participant.Index),
			Namespace:       config.Namespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{workflow},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			Affinity:                     participantNodeAffinity(participant.NodeName, participant.NodeUID),
			Tolerations:                  tolerations,
			ActiveDeadlineSeconds:        &backendDeadlineSeconds,
			AutomountServiceAccountToken: ptr(false),
			Containers:                   []corev1.Container{container},
			Volumes:                      workloadVolumes(config.ScriptConfigMap, policy),
		},
	}
	applyPodPolicy(&pod.Spec, policy)

	return pod
}

func mpiJob(
	config Config,
	participant signalv1.Participant,
	tolerations []corev1.Toleration,
	environment []corev1.EnvVar,
	policy signalv1.WorkloadPolicy,
	workflow metav1.OwnerReference,
	backendDeadlineSeconds int64,
	gpuCount int64,
) (*unstructured.Unstructured, error) {
	labels := resourceLabels(config, participant)

	jobName := resourceNameWithLimit(config.RunName, participant.Index, 63-len("-worker-0"))
	launcherEnv := workloadEnvironment(config, participant, environment)
	workerEnv := workloadEnvironment(config, participant, environment)
	readinessTimeout := mpiWorkerReadinessTimeout(backendDeadlineSeconds)

	workerHosts, err := mpiWorkerHosts(config.Namespace, jobName, 1, gpuCount)
	if err != nil {
		return nil, err
	}

	worker := workloadContainer(
		config,
		policy,
		corev1.Container{
			Name:    "worker",
			Image:   config.RunnerImage,
			Command: resultCommand("mpi-worker"),
			Env:     workerEnv,
		},
		gpuCount,
	)

	object := map[string]any{
		"apiVersion": "kubeflow.org/v2beta1",
		"kind":       "MPIJob",
		"metadata": map[string]any{
			"name":            jobName,
			"namespace":       config.Namespace,
			"labels":          stringMapAny(labels),
			"ownerReferences": []any{ownerReferenceMap(workflow)},
		},
		"spec": map[string]any{
			"launcherCreationPolicy": "WaitForWorkersReady",
			"mpiImplementation":      "OpenMPI",
			"runLauncherAsWorker":    false,
			"slotsPerWorker":         gpuCount,
			"runPolicy": map[string]any{
				"activeDeadlineSeconds": backendDeadlineSeconds,
				"backoffLimit":          mpiBackoffLimit,
				"cleanPodPolicy":        "Running",
			},
			"mpiReplicaSpecs": map[string]any{
				"Launcher": map[string]any{
					"replicas":      int64(1),
					"restartPolicy": string(corev1.RestartPolicyNever),
					"template": podTemplate(
						labels,
						"",
						"",
						tolerations,
						corev1.Container{
							Name:  "launcher",
							Image: config.RunnerImage,
							Command: resultCommandWithPrelude(
								mpiLauncherPrelude(mpiHostfilePath, workerHosts, readinessTimeout),
								"mpi-launcher",
								strconv.FormatInt(gpuCount, 10),
							),
							Env:          launcherEnv,
							VolumeMounts: resultVolumeMounts(),
						},
						config.ScriptConfigMap,
					),
				},
				"Worker": map[string]any{
					"replicas":      int64(1),
					"restartPolicy": string(corev1.RestartPolicyNever),
					"template": podTemplate(
						labels,
						participant.NodeName,
						participant.NodeUID,
						tolerations,
						worker,
						config.ScriptConfigMap,
						policy,
					),
				},
			},
		},
	}

	return &unstructured.Unstructured{Object: object}, nil
}

func podTemplate(
	labels map[string]string,
	nodeName string,
	nodeUID types.UID,
	tolerations []corev1.Toleration,
	container corev1.Container,
	scriptConfigMap string,
	policies ...signalv1.WorkloadPolicy,
) map[string]any {
	policy := signalv1.WorkloadPolicy{}
	if len(policies) > 0 {
		policy = policies[0]
	}

	template := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: labels,
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			Affinity:                     participantNodeAffinity(nodeName, nodeUID),
			Tolerations:                  tolerations,
			AutomountServiceAccountToken: ptr(false),
			Containers:                   []corev1.Container{container},
			Volumes:                      workloadVolumes(scriptConfigMap, policy),
		},
	}
	applyPodPolicy(&template.Spec, policy)

	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&template)
	if err != nil {
		panic(err)
	}

	return content
}

func participantNodeAffinity(nodeName string, nodeUID types.UID) *corev1.Affinity {
	if nodeName == "" {
		return nil
	}

	return &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchFields: []corev1.NodeSelectorRequirement{{
						Key:      "metadata.name",
						Operator: corev1.NodeSelectorOpIn,
						Values:   []string{nodeName},
					}},
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key:      signalv1.ReservationNodeUIDLabel,
						Operator: corev1.NodeSelectorOpIn,
						Values:   []string{string(nodeUID)},
					}},
				}},
			},
		},
	}
}

func applyPodPolicy(spec *corev1.PodSpec, policy signalv1.WorkloadPolicy) {
	if !policy.HostNetwork {
		return
	}

	spec.HostNetwork = true
	spec.DNSPolicy = corev1.DNSClusterFirstWithHostNet
}

func workloadEnvironment(
	config Config,
	participant signalv1.Participant,
	environment []corev1.EnvVar,
) []corev1.EnvVar {
	result := slices.Clone(environment)

	result = append(
		result,
		corev1.EnvVar{Name: "PROJECT_SIGNAL_RUN_NAME", Value: config.RunName},
		corev1.EnvVar{Name: "PROJECT_SIGNAL_RUN_UID", Value: string(config.RunUID)},
		corev1.EnvVar{Name: "PROJECT_SIGNAL_PARTICIPANT_INDEX", Value: strconv.FormatInt(int64(participant.Index), 10)},
		corev1.EnvVar{Name: "PROJECT_SIGNAL_NODE_NAME", Value: participant.NodeName},
		corev1.EnvVar{Name: "PROJECT_SIGNAL_NODE_UID", Value: string(participant.NodeUID)},
		corev1.EnvVar{Name: "PROJECT_SIGNAL_SCRIPT_KEY", Value: config.ScriptKey},
		corev1.EnvVar{Name: "PROJECT_SIGNAL_SCRIPT_DIGEST", Value: config.ScriptDigest},
		corev1.EnvVar{Name: "PROJECT_SIGNAL_RESULTS_DIR", Value: "/results"},
	)

	return result
}

func resultVolumes(scriptConfigMap string) []corev1.Volume {
	return []corev1.Volume{
		{
			Name: "scripts",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: scriptConfigMap},
					DefaultMode:          ptr(int32(0o555)),
				},
			},
		},
		{Name: "results", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
}

func workloadContainer(
	config Config,
	policy signalv1.WorkloadPolicy,
	container corev1.Container,
	gpuCount int64,
) corev1.Container {
	container.Resources = *policy.Resources.DeepCopy()
	if gpuCount > 0 {
		if container.Resources.Requests == nil {
			container.Resources.Requests = corev1.ResourceList{}
		}

		if container.Resources.Limits == nil {
			container.Resources.Limits = corev1.ResourceList{}
		}

		gpu := *resourceQuantity(gpuCount)
		container.Resources.Requests[corev1.ResourceName("nvidia.com/gpu")] = gpu
		container.Resources.Limits[corev1.ResourceName("nvidia.com/gpu")] = gpu
	}

	if policy.Privileged || len(policy.Capabilities) > 0 {
		container.SecurityContext = &corev1.SecurityContext{
			Privileged: ptr(policy.Privileged),
		}
		if len(policy.Capabilities) > 0 {
			container.SecurityContext.Capabilities = &corev1.Capabilities{
				Add: slices.Clone(policy.Capabilities),
			}
		}
	}

	container.WorkingDir = policy.WorkingDir
	container.VolumeMounts = append(resultVolumeMounts(), workloadVolumeMounts(policy)...)

	return container
}

func workloadVolumes(scriptConfigMap string, policy signalv1.WorkloadPolicy) []corev1.Volume {
	volumes := resultVolumes(scriptConfigMap)

	for index := range policy.HostPaths {
		hostPath := &policy.HostPaths[index]
		hostPathType := corev1.HostPathDirectory
		volumes = append(volumes, corev1.Volume{
			Name: signalworkload.HostPathVolumeName(index),
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: hostPath.Path,
					Type: &hostPathType,
				},
			},
		})
	}

	return volumes
}

func workloadVolumeMounts(policy signalv1.WorkloadPolicy) []corev1.VolumeMount {
	mounts := make([]corev1.VolumeMount, 0, len(policy.HostPaths))
	for index := range policy.HostPaths {
		hostPath := &policy.HostPaths[index]
		mounts = append(mounts, corev1.VolumeMount{
			Name:      signalworkload.HostPathVolumeName(index),
			MountPath: hostPath.MountPath,
			ReadOnly:  true,
		})
	}

	return mounts
}

func resultVolumeMounts() []corev1.VolumeMount {
	return []corev1.VolumeMount{
		{Name: "scripts", MountPath: "/scripts", ReadOnly: true},
		{Name: "results", MountPath: "/results"},
	}
}

func validateBackendCoverage(
	participantCount int,
	resources []signalv1.BackendResourceReference,
) error {
	if len(resources) != participantCount {
		return fmt.Errorf("backend-resources must contain exactly %d entries", participantCount)
	}

	identities := make(map[string]struct{}, len(resources))
	covered := make([]bool, participantCount)

	for index, resource := range resources {
		if resource.Name == "" || resource.UID == "" {
			return fmt.Errorf("backend-resources[%d] has an incomplete identity", index)
		}

		identity := strings.Join([]string{
			resource.Name,
			string(resource.UID),
		}, "\x00")
		if _, duplicate := identities[identity]; duplicate {
			return fmt.Errorf("backend-resources[%d] duplicates a workload identity", index)
		}

		identities[identity] = struct{}{}

		if len(resource.ParticipantIndexes) != 1 {
			return fmt.Errorf("backend-resources[%d] must identify exactly one participant", index)
		}

		participantIndex := resource.ParticipantIndexes[0]
		if participantIndex < 0 || participantIndex >= int32(participantCount) {
			return fmt.Errorf("backend-resources[%d] participant index %d is out of range", index, participantIndex)
		}

		if covered[participantIndex] {
			return fmt.Errorf("backend-resources[%d] repeats participant index %d", index, participantIndex)
		}

		covered[participantIndex] = true
	}

	for index, present := range covered {
		if !present {
			return fmt.Errorf("backend-resources does not cover participant index %d", index)
		}
	}

	return nil
}

func decodeJSONObject[T any](content string, destination *T) error {
	if strings.TrimSpace(content) == "" {
		content = "[]"
		if _, ok := any(destination).(*map[string]string); ok {
			content = "{}"
		}
	}

	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(destination); err != nil {
		return err
	}

	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("contains trailing JSON values")
		}

		return err
	}

	return nil
}

func resourceLabels(config Config, participant signalv1.Participant) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":                      "project-signal-adapter",
		"signal.unbounded-cloud.io/run":               config.RunName,
		"signal.unbounded-cloud.io/participant-index": strconv.FormatInt(int64(participant.Index), 10),
	}
}

func resourceName(runName string, participantIndex int32) string {
	return resourceNameWithLimit(runName, participantIndex, 63)
}

func resourceNameWithLimit(runName string, participantIndex int32, maximumLength int) string {
	suffix := fmt.Sprintf("-p%d", participantIndex)

	maximumRunLength := maximumLength - len(suffix)
	if len(runName) > maximumRunLength {
		digest := fmt.Sprintf("%x", sha256.Sum256([]byte(runName)))[:8]
		prefixLength := maximumRunLength - len(digest) - 1
		runName = strings.TrimRight(runName[:prefixLength], "-.") + "-" + digest
	}

	return runName + suffix
}

func stringMapAny(values map[string]string) map[string]any {
	result := make(map[string]any, len(values))
	for key, value := range values {
		result[key] = value
	}

	return result
}

func ownerReferenceMap(reference metav1.OwnerReference) map[string]any {
	return map[string]any{
		"apiVersion": reference.APIVersion,
		"kind":       reference.Kind,
		"name":       reference.Name,
		"uid":        string(reference.UID),
		"controller": reference.Controller,
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func quotedShellArguments(values []string) []string {
	quoted := make([]string, len(values))
	for index, value := range values {
		quoted[index] = shellQuote(value)
	}

	return quoted
}

func ptr[T any](value T) *T {
	return &value
}
