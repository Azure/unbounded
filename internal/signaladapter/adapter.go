// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signaladapter

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
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
	MaxParticipants       = 64
	MaxReasonBytes        = 4 * 1024
	MaxMeasurementsBytes  = 16 * 1024
	MaxBackendOutputBytes = 16 * 1024
	resultPrefix          = "PROJECT_SIGNAL_RESULT_V2 "
	requiredTopologyKey   = "kueue.x-k8s.io/podset-required-topology"
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
	Namespace             string
	WorkflowName          string
	RunName               string
	RunUID                types.UID
	ParticipantsJSON      string
	ParticipantCount      int
	RequestNodeName       string
	LegacyParticipants    string
	ScriptConfigMap       string
	ScriptKey             string
	ScriptDigest          string
	RunnerImage           string
	BackendAPIVersion     string
	BackendKind           string
	Queue                 string
	TolerationsJSON       string
	ScriptEnvironmentJSON string
	WorkloadPolicyJSON    string
	GPUResourceCount      string
	Privileged            bool
	Attempt               int
	BackoffLimit          int64
	TTLSeconds            int64
}

type Result struct {
	ParticipantIndex int    `json:"participantIndex"`
	ExecutionState   string `json:"executionState"`
	Verdict          string `json:"verdict"`
	Reason           string `json:"reason"`
	Measurements     string `json:"measurements"`
	ExitCode         int    `json:"-"`
}

type AggregateOutput struct {
	ExecutionState   string                              `json:"executionState"`
	Verdict          string                              `json:"verdict"`
	Reason           string                              `json:"reason"`
	Measurements     string                              `json:"measurements"`
	BackendResources []signalv1.BackendResourceReference `json:"-"`
	BackendJSON      string                              `json:"backend-resources"`
}

func ParseParticipants(
	participantsJSON string,
	participantCount int,
	requestNodeName string,
	legacyParticipants string,
) ([]signalv1.Participant, error) {
	if participantCount < 1 || participantCount > MaxParticipants {
		return nil, fmt.Errorf("participant-count must be between 1 and %d", MaxParticipants)
	}

	var participants []signalv1.Participant

	if strings.TrimSpace(participantsJSON) != "" {
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
	} else {
		nodeName := requestNodeName
		if nodeName == "" {
			names := strings.Split(legacyParticipants, ",")
			if len(names) > 0 {
				nodeName = strings.TrimSpace(names[0])
			}
		}

		if nodeName == "" {
			return nil, errors.New("participants-json or a compatibility Node name is required")
		}

		participants = []signalv1.Participant{{Index: 0, NodeName: nodeName}}
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
		if participant.Index != int32(index) {
			return nil, fmt.Errorf("participants-json[%d].index must equal %d", index, index)
		}

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

	switch {
	case config.BackendKind == "Pod" && config.BackendAPIVersion == "v1":
	case config.BackendKind == "MPIJob" && config.BackendAPIVersion == "kubeflow.org/v2beta1":
		if config.Queue == "" {
			return nil, nil, nil, errors.New("queue is required for MPIJob execution")
		}
	default:
		return nil, nil, nil, fmt.Errorf(
			"unsupported backend %s %s",
			config.BackendAPIVersion,
			config.BackendKind,
		)
	}

	if config.Queue != "" {
		if errors := validation.IsDNS1123Subdomain(config.Queue); len(errors) > 0 {
			return nil, nil, nil, fmt.Errorf("queue is invalid: %s", strings.Join(errors, ", "))
		}
	}

	if config.BackoffLimit < 0 {
		return nil, nil, nil, errors.New("backoff-limit must not be negative")
	}

	if config.TTLSeconds < 0 {
		return nil, nil, nil, errors.New("ttl-seconds-after-finished must not be negative")
	}

	if config.Attempt < 1 {
		return nil, nil, nil, errors.New("attempt must be positive")
	}

	policy, err := signalworkload.Decode(config.WorkloadPolicyJSON)
	if err != nil {
		return nil, nil, nil, err
	}

	if policy.KueueTopology != nil && config.Queue == "" {
		return nil, nil, nil, errors.New("workload policy kueueTopology requires a queue")
	}

	participants, err := ParseParticipants(
		config.ParticipantsJSON,
		config.ParticipantCount,
		config.RequestNodeName,
		config.LegacyParticipants,
	)
	if err != nil {
		return nil, nil, nil, err
	}

	var tolerations []corev1.Toleration
	if err := decodeJSONObject(config.TolerationsJSON, &tolerations); err != nil {
		return nil, nil, nil, fmt.Errorf("decode tolerations: %w", err)
	}

	var environment map[string]string
	if err := decodeJSONObject(config.ScriptEnvironmentJSON, &environment); err != nil {
		return nil, nil, nil, fmt.Errorf("decode script-environment: %w", err)
	}

	if value := environment["BACKEND_BACKOFF_LIMIT"]; value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed < 0 {
			return nil, nil, nil, errors.New("BACKEND_BACKOFF_LIMIT must be a nonnegative integer")
		}

		config.BackoffLimit = parsed
	}

	if value := environment["BACKEND_TTL_SECONDS_AFTER_FINISHED"]; value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed < 0 {
			return nil, nil, nil, errors.New("BACKEND_TTL_SECONDS_AFTER_FINISHED must be a nonnegative integer")
		}

		config.TTLSeconds = parsed
	}

	environmentNames := make([]string, 0, len(environment))
	for name := range environment {
		environmentNames = append(environmentNames, name)
	}

	slices.Sort(environmentNames)

	env := make([]corev1.EnvVar, 0, len(environmentNames)+4)
	for _, name := range environmentNames {
		if errors := validation.IsEnvVarName(name); len(errors) > 0 {
			return nil, nil, nil, fmt.Errorf("script-environment contains invalid name %q", name)
		}

		if strings.HasPrefix(name, "PROJECT_SIGNAL_") {
			return nil, nil, nil, fmt.Errorf("script-environment contains reserved name %q", name)
		}

		if _, reserved := reservedEnvironmentNames[name]; reserved {
			return nil, nil, nil, fmt.Errorf("script-environment contains reserved name %q", name)
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
	activeDeadlineSeconds int64,
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

	var policyEnvironment map[string]string
	if err := decodeJSONObject(config.ScriptEnvironmentJSON, &policyEnvironment); err != nil {
		return nil, fmt.Errorf("decode adapter policy: %w", err)
	}

	if value := policyEnvironment["BACKEND_BACKOFF_LIMIT"]; value != "" {
		config.BackoffLimit, err = strconv.ParseInt(value, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse BACKEND_BACKOFF_LIMIT: %w", err)
		}
	}

	if value := policyEnvironment["BACKEND_TTL_SECONDS_AFTER_FINISHED"]; value != "" {
		config.TTLSeconds, err = strconv.ParseInt(value, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse BACKEND_TTL_SECONDS_AFTER_FINISHED: %w", err)
		}
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
				activeDeadlineSeconds,
				gpuCount,
			)
		case "MPIJob":
			object = mpiJob(
				config,
				participant,
				tolerations,
				environment,
				policy,
				workflow,
				activeDeadlineSeconds,
				gpuCount,
			)
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

	if resource.GetAPIVersion() == "" ||
		resource.GetKind() == "" ||
		resource.GetName() == "" ||
		resource.GetUID() == "" {
		return signalv1.BackendResourceReference{}, errors.New("backend resource has an incomplete identity")
	}

	return signalv1.BackendResourceReference{
		APIVersion:         resource.GetAPIVersion(),
		Kind:               resource.GetKind(),
		Namespace:          resource.GetNamespace(),
		Name:               resource.GetName(),
		UID:                resource.GetUID(),
		ParticipantIndexes: []int32{participantIndex},
	}, nil
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
		output.Reason = "ParticipantCoverageIncomplete"
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
set +e
/bin/bash "/scripts/${PROJECT_SIGNAL_SCRIPT_KEY}" %s
status=$?
set -e
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
printf '%s%%s %%s %%s %%d\n' "${verdict}" "${reason}" "${measurements}" "$status"
exit "$status"
`, strings.Join(quotedArguments, " "), resultPrefix),
	}
}

func ordinaryPod(
	config Config,
	participant signalv1.Participant,
	tolerations []corev1.Toleration,
	environment []corev1.EnvVar,
	policy signalv1.WorkloadPolicy,
	workflow metav1.OwnerReference,
	activeDeadlineSeconds int64,
	gpuCount int64,
) *corev1.Pod {
	labels := resourceLabels(config, participant)
	if config.Queue != "" {
		labels = withLabel(labels, "kueue.x-k8s.io/queue-name", config.Queue)
	}

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
			Annotations:     workloadAnnotations(policy),
			OwnerReferences: []metav1.OwnerReference{workflow},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			NodeSelector:                 map[string]string{corev1.LabelHostname: participant.NodeName},
			Tolerations:                  tolerations,
			ActiveDeadlineSeconds:        &activeDeadlineSeconds,
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
	activeDeadlineSeconds int64,
	gpuCount int64,
) *unstructured.Unstructured {
	labels := resourceLabels(config, participant)
	jobName := resourceName(config.RunName, participant.Index)
	launcherEnv := workloadEnvironment(config, participant, environment)
	workerEnv := workloadEnvironment(config, participant, environment)
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
			"labels":          stringMapAny(withLabel(labels, "kueue.x-k8s.io/queue-name", config.Queue)),
			"ownerReferences": []any{ownerReferenceMap(workflow)},
		},
		"spec": map[string]any{
			"launcherCreationPolicy": "WaitForWorkersReady",
			"mpiImplementation":      "OpenMPI",
			"runLauncherAsWorker":    false,
			"slotsPerWorker":         gpuCount,
			"runPolicy": map[string]any{
				"activeDeadlineSeconds":   activeDeadlineSeconds,
				"backoffLimit":            config.BackoffLimit,
				"cleanPodPolicy":          "All",
				"ttlSecondsAfterFinished": config.TTLSeconds,
			},
			"mpiReplicaSpecs": map[string]any{
				"Launcher": map[string]any{
					"replicas": int64(1),
					"template": podTemplate(
						labels,
						nil,
						nil,
						corev1.Container{
							Name:         "launcher",
							Image:        config.RunnerImage,
							Command:      resultCommand("mpi-launcher", strconv.FormatInt(gpuCount, 10)),
							Env:          launcherEnv,
							VolumeMounts: resultVolumeMounts(),
						},
						config.ScriptConfigMap,
					),
				},
				"Worker": map[string]any{
					"replicas": int64(1),
					"template": podTemplate(
						labels,
						map[string]string{corev1.LabelHostname: participant.NodeName},
						tolerations,
						worker,
						config.ScriptConfigMap,
						policy,
					),
				},
			},
		},
	}

	return &unstructured.Unstructured{Object: object}
}

func podTemplate(
	labels map[string]string,
	nodeSelector map[string]string,
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
			Labels:      labels,
			Annotations: workloadAnnotations(policy),
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			NodeSelector:                 nodeSelector,
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

func workloadAnnotations(policy signalv1.WorkloadPolicy) map[string]string {
	if policy.KueueTopology == nil {
		return nil
	}

	return map[string]string{requiredTopologyKey: policy.KueueTopology.RequiredLevel}
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

	return append(
		result,
		corev1.EnvVar{Name: "PROJECT_SIGNAL_RUN_NAME", Value: config.RunName},
		corev1.EnvVar{Name: "PROJECT_SIGNAL_RUN_UID", Value: string(config.RunUID)},
		corev1.EnvVar{Name: "PROJECT_SIGNAL_PARTICIPANT_INDEX", Value: strconv.FormatInt(int64(participant.Index), 10)},
		corev1.EnvVar{Name: "PROJECT_SIGNAL_NODE_NAME", Value: participant.NodeName},
		corev1.EnvVar{Name: "PROJECT_SIGNAL_NODE_UID", Value: string(participant.NodeUID)},
		corev1.EnvVar{Name: "PROJECT_SIGNAL_SCRIPT_KEY", Value: config.ScriptKey},
		corev1.EnvVar{Name: "PROJECT_SIGNAL_SCRIPT_DIGEST", Value: config.ScriptDigest},
		corev1.EnvVar{Name: "PROJECT_SIGNAL_ATTEMPT", Value: strconv.Itoa(config.Attempt)},
		corev1.EnvVar{Name: "PROJECT_SIGNAL_RESULTS_DIR", Value: "/results"},
	)
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

	if policy.SecurityContext != nil {
		container.SecurityContext = policy.SecurityContext.DeepCopy()
	} else if config.Privileged {
		container.SecurityContext = &corev1.SecurityContext{Privileged: ptr(true)}
	}

	container.WorkingDir = policy.WorkingDir
	container.VolumeMounts = append(resultVolumeMounts(), workloadVolumeMounts(policy)...)

	return container
}

func workloadVolumes(scriptConfigMap string, policy signalv1.WorkloadPolicy) []corev1.Volume {
	volumes := resultVolumes(scriptConfigMap)
	if policy.Results != nil {
		volumes[1].EmptyDir = policy.Results.DeepCopy()
	}

	for index := range policy.Volumes {
		volume := &policy.Volumes[index]
		volumes = append(volumes, corev1.Volume{
			Name: volume.Name,
			VolumeSource: corev1.VolumeSource{
				HostPath:              volume.HostPath.DeepCopy(),
				EmptyDir:              volume.EmptyDir.DeepCopy(),
				PersistentVolumeClaim: volume.PersistentVolumeClaim.DeepCopy(),
			},
		})
	}

	return volumes
}

func workloadVolumeMounts(policy signalv1.WorkloadPolicy) []corev1.VolumeMount {
	mounts := make([]corev1.VolumeMount, 0, len(policy.Volumes))
	for index := range policy.Volumes {
		volume := &policy.Volumes[index]
		mounts = append(mounts, corev1.VolumeMount{
			Name:      volume.Name,
			MountPath: volume.MountPath,
			ReadOnly:  volume.ReadOnly,
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
		if resource.APIVersion == "" || resource.Kind == "" || resource.Name == "" || resource.UID == "" {
			return fmt.Errorf("backend-resources[%d] has an incomplete identity", index)
		}

		identity := strings.Join([]string{
			resource.APIVersion,
			resource.Kind,
			resource.Namespace,
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
	suffix := fmt.Sprintf("-p%d", participantIndex)

	maximumRunLength := 63 - len(suffix)
	if len(runName) > maximumRunLength {
		runName = strings.TrimRight(runName[:maximumRunLength], "-.")
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

func withLabel(values map[string]string, key, value string) map[string]string {
	result := maps.Clone(values)
	result[key] = value

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

func ptr[T any](value T) *T {
	return &value
}
