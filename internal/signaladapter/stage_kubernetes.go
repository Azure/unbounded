// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signaladapter

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilptr "k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

const (
	invocationResultPrefix            = "PROJECT_SIGNAL_INVOCATION_RESULT_V1 "
	maximumWorkflowArgumentParameters = 256
	argoWorkflowLabel                 = "workflows.argoproj.io/workflow"
	workflowRestartMetricName         = "restartSourceCounts"
	rawWorkflowResultArtifactName     = "rawWorkflowResult"
	maxInvocationResultArtifacts      = 16
	stageSharedMemoryVolumeName       = "shared-memory"
	stageSharedMemoryMountPath        = "/dev/shm"
	stageSharedMemorySize             = "1Gi"
	liveTelemetryVolumeName           = "live-telemetry"
	liveTelemetryMountPath            = "/live-telemetry"
	liveTelemetryRecordsPath          = liveTelemetryMountPath + "/records.jsonl"
	liveTelemetryDonePath             = liveTelemetryMountPath + "/done"
	liveTelemetryPort                 = 9464
	liveTelemetryVolumeBytes          = 256 * 1024
	liveTelemetryFinalScrapeGrace     = 10 * time.Second
	workloadPodSpecPatch              = `{"securityContext":{"fsGroup":65532,"runAsGroup":65532,"runAsNonRoot":true,"runAsUser":65532}}`
)

type workflowRestartSourceCounts struct {
	Container   int32 `json:"container"`
	WorkflowPod int32 `json:"workflowPod"`
}

type KubernetesStageExecutor struct {
	Runner                 *Runner
	Namespace              string
	WorkflowName           string
	RunName                string
	ExecutionName          string
	WorkflowServiceAccount string
}

func (e *KubernetesStageExecutor) ExecuteInvocation(
	ctx context.Context,
	invocation StageInvocation,
) (signalv1.InvocationResultEnvelope, error) {
	if e.Runner == nil || e.Runner.Dynamic == nil || e.Runner.Kubernetes == nil {
		return signalv1.InvocationResultEnvelope{}, errors.New("kubernetes stage executor requires clients")
	}

	switch invocation.Stage.Test.Driver.Type {
	case signalv1.TestDriverTypeScript, signalv1.TestDriverTypeContainer:
		return e.executePods(ctx, invocation)
	case signalv1.TestDriverTypeWorkflowTemplate:
		return e.executeWorkflowTemplate(ctx, invocation)
	default:
		return signalv1.InvocationResultEnvelope{}, fmt.Errorf(
			"unsupported stage driver %q",
			invocation.Stage.Test.Driver.Type,
		)
	}
}

func mergeWorkflowArguments(existing any, adapterParameters []any) (map[string]any, error) {
	arguments, ok := existing.(map[string]any)
	if !ok {
		arguments = map[string]any{}
	}

	parameters := []any{}
	if existingParameters, ok := arguments["parameters"].([]any); ok {
		parameters = existingParameters
	}

	if len(adapterParameters) > maximumWorkflowArgumentParameters ||
		len(parameters) > maximumWorkflowArgumentParameters-len(adapterParameters) {
		return nil, fmt.Errorf(
			"WorkflowTemplate arguments exceed the %d-parameter limit",
			maximumWorkflowArgumentParameters,
		)
	}

	byName := make(map[string]any, len(adapterParameters))
	order := make([]string, 0, len(parameters)+len(adapterParameters))

	for _, parameter := range parameters {
		typed, ok := parameter.(map[string]any)
		if !ok {
			continue
		}

		name, ok := typed["name"].(string)
		if !ok || name == "" {
			continue
		}

		if _, exists := byName[name]; !exists {
			order = append(order, name)
		}

		byName[name] = parameter
	}

	for _, parameter := range adapterParameters {
		typed, ok := parameter.(map[string]any)
		if !ok {
			continue
		}

		name, ok := typed["name"].(string)
		if !ok || name == "" {
			continue
		}

		if _, exists := byName[name]; !exists {
			order = append(order, name)
		}

		byName[name] = parameter
	}

	merged := make([]any, 0, len(order))
	for _, name := range order {
		merged = append(merged, byName[name])
	}

	arguments["parameters"] = merged

	return arguments, nil
}

func (e *KubernetesStageExecutor) CancelInvocation(
	ctx context.Context,
	invocation StageInvocation,
) error {
	propagation := metav1.DeletePropagationForeground
	grace := int64(0)

	var joined error

	for _, participant := range invocation.Participants {
		name := invocationResourceName(e.ExecutionName, invocation, participant.Index)

		err := e.Runner.Kubernetes.CoreV1().Pods(e.Namespace).Delete(
			ctx,
			name,
			metav1.DeleteOptions{
				GracePeriodSeconds: &grace,
				PropagationPolicy:  &propagation,
			},
		)
		if err != nil && !apierrors.IsNotFound(err) {
			joined = errors.Join(joined, err)
		}
	}

	name := invocationResourceName(e.ExecutionName, invocation, -1)

	err := e.Runner.Dynamic.Resource(workflowGVR).Namespace(e.Namespace).
		Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &propagation})
	if err != nil && !apierrors.IsNotFound(err) {
		joined = errors.Join(joined, err)
	}

	return joined
}

func (e *KubernetesStageExecutor) executePods(
	ctx context.Context,
	invocation StageInvocation,
) (signalv1.InvocationResultEnvelope, error) {
	workflow, err := e.Runner.Dynamic.Resource(workflowGVR).Namespace(e.Namespace).
		Get(ctx, e.WorkflowName, metav1.GetOptions{})
	if err != nil {
		return signalv1.InvocationResultEnvelope{}, fmt.Errorf("get owning Workflow: %w", err)
	}

	startedAt := e.Runner.now()
	participantResults := make([]signalv1.InvocationParticipantResult, 0, len(invocation.Participants))
	measurements := map[string]signalv1.MetricMeasurement{}
	activitySamples := int32(0)
	cleanup := signalv1.CleanupOutcomeSucceeded
	outcome := signalv1.InvocationOutcomePassed
	reason := signalv1.ResultReasonCompleted

	var message string

	createdPods := make([]*corev1.Pod, 0, len(invocation.Participants))
	for position := range invocation.Participants {
		pod, podErr := e.stagePod(workflow, invocation, position)
		if podErr != nil {
			return signalv1.InvocationResultEnvelope{}, errors.Join(
				podErr,
				e.cleanupStagePods(ctx, createdPods),
			)
		}

		created, createErr := e.Runner.Kubernetes.CoreV1().Pods(e.Namespace).
			Create(ctx, pod, metav1.CreateOptions{})
		if createErr != nil {
			createdPods = append(createdPods, e.createdStagePodAfterError(ctx, workflow, pod))

			return signalv1.InvocationResultEnvelope{}, errors.Join(
				fmt.Errorf("create stage Pod: %w", createErr),
				e.cleanupStagePods(ctx, createdPods),
			)
		}

		createdPods = append(createdPods, created)
	}

	for position, created := range createdPods {
		participantEnvelope, waitErr := e.waitStagePod(ctx, invocation, created)
		podCleanupErr := e.cleanupStagePods(ctx, []*corev1.Pod{created})
		createdPods[position] = nil

		if waitErr != nil {
			return signalv1.InvocationResultEnvelope{}, errors.Join(
				waitErr,
				podCleanupErr,
				e.cleanupStagePods(ctx, createdPods),
			)
		}

		if len(participantEnvelope.Participants) != 1 {
			return signalv1.InvocationResultEnvelope{}, errors.Join(
				errors.New("participant Pod result must contain one participant"),
				podCleanupErr,
				e.cleanupStagePods(ctx, createdPods),
			)
		}

		participantResults = append(participantResults, participantEnvelope.Participants[0])
		for _, measurement := range participantEnvelope.Measurements {
			if existing, found := measurements[measurement.Name]; found &&
				(existing.Unit != measurement.Unit ||
					!reflect.DeepEqual(existing.Value, measurement.Value)) {
				return signalv1.InvocationResultEnvelope{}, errors.Join(
					fmt.Errorf(
						"participant measurements conflict for metric %q",
						measurement.Name,
					),
					podCleanupErr,
					e.cleanupStagePods(ctx, createdPods),
					errInvocationEvidenceIntegrity,
				)
			}

			measurements[measurement.Name] = measurement
		}

		activitySamples += participantEnvelope.Activity.SampleCount
		if invocationOutcomePrecedence(participantEnvelope.Outcome) > invocationOutcomePrecedence(outcome) {
			outcome = participantEnvelope.Outcome
			reason = participantEnvelope.ReasonCode
			message = participantEnvelope.Message
		}

		if podCleanupErr != nil {
			cleanup = signalv1.CleanupOutcomeFailed

			if outcome == signalv1.InvocationOutcomePassed {
				outcome = signalv1.InvocationOutcomeInconclusive
				reason = signalv1.ResultReasonInfrastructureFailure
				message = "stage Pod cleanup failed"
			}
		}
	}

	measurementList := make([]signalv1.MetricMeasurement, 0, len(measurements))
	for _, measurement := range measurements {
		measurementList = append(measurementList, measurement)
	}

	sortMeasurements(measurementList)

	finishedAt := e.Runner.now()
	activityDigest := sha256.Sum256([]byte(
		invocation.ID + "\x00" + strconv.FormatInt(int64(activitySamples), 10),
	))

	return signalv1.InvocationResultEnvelope{
		SchemaVersion:     signalv1.InvocationResultSchemaV1,
		InvocationID:      invocation.ID,
		RunUID:            invocation.RunUID,
		StageID:           invocation.Stage.Name,
		StageOrder:        invocation.StageOrder,
		PlanDigest:        invocation.PlanDigest,
		Test:              invocation.Stage.Test.Identity,
		ResolvedInputs:    invocation.Stage.ResolvedInputs,
		StartedAt:         metav1.NewTime(startedAt),
		FinishedAt:        metav1.NewTime(finishedAt),
		MonotonicDuration: metav1.Duration{Duration: finishedAt.Sub(startedAt)},
		Outcome:           outcome,
		ReasonCode:        reason,
		Message:           boundedStageMessage(message),
		Measurements:      measurementList,
		Participants:      participantResults,
		Activity: signalv1.WorkloadActivitySummary{
			SampleCount:          activitySamples,
			SampleSequenceDigest: "sha256:" + hex.EncodeToString(activityDigest[:]),
		},
		Cleanup: signalv1.InvocationCleanupResult{Outcome: cleanup},
	}, nil
}

func (e *KubernetesStageExecutor) createdStagePodAfterError(
	ctx context.Context,
	workflow *unstructured.Unstructured,
	pod *corev1.Pod,
) *corev1.Pod {
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	created, err := e.Runner.Kubernetes.CoreV1().Pods(e.Namespace).
		Get(recoveryCtx, pod.Name, metav1.GetOptions{})
	if err != nil ||
		created.Labels[runUIDLabel] != pod.Labels[runUIDLabel] ||
		!podControlledByWorkflow(created, workflow) {
		return nil
	}

	return created
}

func (e *KubernetesStageExecutor) cleanupStagePods(ctx context.Context, pods []*corev1.Pod) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	var joined error

	for _, pod := range pods {
		if pod == nil {
			continue
		}

		if err := e.deletePod(cleanupCtx, pod); err != nil {
			joined = errors.Join(joined, err)
		}
	}

	return joined
}

func podControlledByWorkflow(pod *corev1.Pod, workflow *unstructured.Unstructured) bool {
	for _, owner := range pod.OwnerReferences {
		if owner.UID == workflow.GetUID() &&
			owner.Controller != nil &&
			*owner.Controller {
			return true
		}
	}

	return false
}

func invocationOutcomePrecedence(outcome signalv1.InvocationOutcome) int {
	switch outcome {
	case signalv1.InvocationOutcomeCanceled:
		return 3
	case signalv1.InvocationOutcomeFailed:
		return 2
	case signalv1.InvocationOutcomeInconclusive:
		return 1
	default:
		return 0
	}
}

func (e *KubernetesStageExecutor) stagePod(
	workflow *unstructured.Unstructured,
	invocation StageInvocation,
	position int,
) (*corev1.Pod, error) {
	participant := invocation.Participants[position]
	participantIndex := participant.Index
	role := invocation.ParticipantRole[participantIndex]
	driver := invocation.Stage.Test.Driver
	name := invocationResourceName(e.ExecutionName, invocation, participantIndex)
	controller := true

	parametersJSON, err := marshalStageJSON(invocation.Stage.Parameters)
	if err != nil {
		return nil, err
	}

	inputsJSON, err := marshalStageJSON(invocation.Stage.ResolvedInputs)
	if err != nil {
		return nil, err
	}

	container := corev1.Container{
		Name:  "test",
		Image: stageDriverImage(driver),
		Env: []corev1.EnvVar{
			{Name: "PROJECT_SIGNAL_INVOCATION_ID", Value: invocation.ID},
			{Name: "PROJECT_SIGNAL_RUN_UID", Value: string(invocation.RunUID)},
			{Name: "PROJECT_SIGNAL_PLAN_DIGEST", Value: invocation.PlanDigest},
			{Name: "PROJECT_SIGNAL_STAGE_ID", Value: invocation.Stage.Name},
			{Name: "PROJECT_SIGNAL_STAGE_ORDER", Value: strconv.Itoa(int(invocation.StageOrder))},
			{Name: "PROJECT_SIGNAL_TEST_NAME", Value: invocation.Stage.Test.Identity.Name},
			{Name: "PROJECT_SIGNAL_TEST_UID", Value: string(invocation.Stage.Test.Identity.UID)},
			{
				Name:  "PROJECT_SIGNAL_TEST_GENERATION",
				Value: strconv.FormatInt(invocation.Stage.Test.Identity.Generation, 10),
			},
			{Name: "PROJECT_SIGNAL_TEST_CONTENT_DIGEST", Value: invocation.Stage.Test.Identity.ContentDigest},
			{Name: "PROJECT_SIGNAL_TEST_TRUST", Value: string(invocation.Stage.Test.Identity.Trust)},
			{Name: "PROJECT_SIGNAL_PARTICIPANT_INDEX", Value: strconv.Itoa(int(participantIndex))},
			{Name: "PROJECT_SIGNAL_PARTICIPANT_ROLE", Value: role},
			{Name: "PROJECT_SIGNAL_NODE_UID", Value: string(participant.NodeUID)},
			{Name: "PROJECT_SIGNAL_PARAMETERS_JSON", Value: parametersJSON},
			{Name: "PROJECT_SIGNAL_RESOLVED_INPUTS_JSON", Value: inputsJSON},
			{Name: "PROJECT_SIGNAL_RESULTS_DIR", Value: "/results"},
		},
		Resources: driverResources(driver),
	}

	telemetryContract, liveTelemetry, err := metricTelemetryContract(invocation.Stage.Test.Metrics)
	if err != nil {
		return nil, err
	}

	if liveTelemetry {
		if invocation.Stage.Test.Driver.Type != signalv1.TestDriverTypeScript {
			return nil, errors.New("live metric telemetry currently requires a Script driver")
		}

		if !runnerImagePattern.MatchString(invocation.AdapterImage) {
			return nil, errors.New("live metric telemetry requires the immutable adapter image")
		}

		container.Env = append(container.Env,
			corev1.EnvVar{Name: "PROJECT_SIGNAL_METRICS_FILE", Value: liveTelemetryRecordsPath},
			corev1.EnvVar{Name: "PROJECT_SIGNAL_LIVE_METRICS_DONE", Value: liveTelemetryDonePath},
			corev1.EnvVar{
				Name:  "PROJECT_SIGNAL_LIVE_METRICS_PORT",
				Value: strconv.Itoa(liveTelemetryPort),
			},
		)
	}

	switch driver.Type {
	case signalv1.TestDriverTypeScript:
		if driver.Script == nil {
			return nil, errors.New("script driver configuration is absent")
		}

		artifact, err := invocationArtifact(invocation, driver.Script.Artifact)
		if err != nil {
			return nil, err
		}

		container.Command = []string{"/bin/sh", "-ceu"}
		container.Args = []string{scriptInvocationWrapper(driver.Script.Language, artifact.Name, artifact.Digest)}

		container.VolumeMounts = []corev1.VolumeMount{{
			Name:      "script",
			MountPath: "/scripts",
			ReadOnly:  true,
		}, {
			Name:      "results",
			MountPath: "/results",
		}}
		if liveTelemetry {
			container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
				Name:      liveTelemetryVolumeName,
				MountPath: liveTelemetryMountPath,
			})
		}
	case signalv1.TestDriverTypeContainer:
		if driver.Container == nil {
			return nil, errors.New("container driver configuration is absent")
		}

		container.Command = append([]string(nil), driver.Container.Command...)
		container.Args = append([]string(nil), driver.Container.Args...)
	}

	container.VolumeMounts = append(
		container.VolumeMounts,
		stageHostPathMounts(invocation.Stage.Test.Security.HostPaths)...,
	)
	if invocation.Stage.Test.Security.IPCLock {
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      stageSharedMemoryVolumeName,
			MountPath: stageSharedMemoryMountPath,
		})
	}

	applyStageSecurity(&container, invocation.Stage.Test.Security)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: e.Namespace,
			Labels: map[string]string{
				runLabel:    e.ExecutionName,
				runUIDLabel: string(invocation.RunUID),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         workflow.GetAPIVersion(),
				Kind:               workflow.GetKind(),
				Name:               workflow.GetName(),
				UID:                workflow.GetUID(),
				Controller:         &controller,
				BlockOwnerDeletion: &controller,
			}},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			NodeName:                     participant.NodeName,
			HostNetwork:                  invocation.Stage.Test.Security.HostNetwork,
			AutomountServiceAccountToken: utilptr.To(false),
			SecurityContext:              workloadPodSecurityContext(),
			Containers:                   []corev1.Container{container},
			Volumes:                      stageHostPathVolumes(invocation.Stage.Test.Security.HostPaths),
		},
	}
	if liveTelemetry {
		pod.Annotations = map[string]string{
			"prometheus.io/path":   "/metrics",
			"prometheus.io/port":   strconv.Itoa(liveTelemetryPort),
			"prometheus.io/scrape": "true",
		}
		pod.Spec.Containers = append(
			pod.Spec.Containers,
			liveTelemetrySidecar(
				invocation.AdapterImage,
				telemetryContract,
				e.Namespace,
				e.RunName,
				invocation,
				role,
			),
		)
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name: liveTelemetryVolumeName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					SizeLimit: resource.NewQuantity(liveTelemetryVolumeBytes, resource.BinarySI),
				},
			},
		})
	}

	if invocation.Stage.Test.Security.IPCLock {
		sizeLimit := resource.MustParse(stageSharedMemorySize)
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name: stageSharedMemoryVolumeName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					Medium:    corev1.StorageMediumMemory,
					SizeLimit: &sizeLimit,
				},
			},
		})
	}

	if driver.Type == signalv1.TestDriverTypeScript {
		artifact, err := invocationArtifact(invocation, driver.Script.Artifact)
		if err != nil {
			return nil, err
		}

		pod.Spec.InitContainers = []corev1.Container{artifactMaterializer(
			driver.Script.RunnerImage,
			artifact,
		)}
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name: "script",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		}, corev1.Volume{
			Name: "results",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		})
	}

	return pod, nil
}

func liveTelemetrySidecar(
	image string,
	contractJSON string,
	namespace string,
	runName string,
	invocation StageInvocation,
	role string,
) corev1.Container {
	return corev1.Container{
		Name:    "live-telemetry",
		Image:   image,
		Command: []string{"/project-signal-adapter"},
		Args: []string{
			"metric-proxy",
			"--contract-json=" + contractJSON,
			"--records=" + liveTelemetryRecordsPath,
			"--done=" + liveTelemetryDonePath,
			"--port=" + strconv.Itoa(liveTelemetryPort),
			"--final-grace=" + liveTelemetryFinalScrapeGrace.String(),
		},
		Env: []corev1.EnvVar{
			{Name: "PROJECT_SIGNAL_NAMESPACE", Value: namespace},
			{Name: "PROJECT_SIGNAL_RUN_NAME", Value: runName},
			{Name: "PROJECT_SIGNAL_RUN_UID", Value: string(invocation.RunUID)},
			{Name: "PROJECT_SIGNAL_STAGE_ID", Value: invocation.Stage.Name},
			{Name: "PROJECT_SIGNAL_INVOCATION_ID", Value: invocation.ID},
			{Name: "PROJECT_SIGNAL_PARTICIPANT_ROLE", Value: role},
			{
				Name: "PROJECT_SIGNAL_NODE_NAME",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"},
				},
			},
		},
		Ports: []corev1.ContainerPort{{
			Name:          "live-metrics",
			ContainerPort: liveTelemetryPort,
			Protocol:      corev1.ProtocolTCP,
		}},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10m"),
				corev1.ResourceMemory: resource.MustParse("32Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: utilptr.To(false),
			ReadOnlyRootFilesystem:   utilptr.To(true),
			Privileged:               utilptr.To(false),
			RunAsUser:                utilptr.To(workloadIdentityID),
			RunAsGroup:               utilptr.To(workloadIdentityID),
			RunAsNonRoot:             utilptr.To(true),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
			},
		},
		VolumeMounts: []corev1.VolumeMount{{
			Name:      liveTelemetryVolumeName,
			MountPath: liveTelemetryMountPath,
			ReadOnly:  true,
		}},
	}
}

func invocationArtifact(
	invocation StageInvocation,
	name string,
) (signalv1.TestArtifact, error) {
	for index := range invocation.Artifacts {
		if invocation.Artifacts[index].Name == name {
			return invocation.Artifacts[index], nil
		}
	}

	return signalv1.TestArtifact{}, fmt.Errorf("test artifact %q is unavailable", name)
}

func artifactMaterializer(image string, artifact signalv1.TestArtifact) corev1.Container {
	container := corev1.Container{
		Name:    "materialize-script",
		Image:   image,
		Command: []string{"/bin/sh", "-ceu"},
		Args: []string{`printf '%s' "$PROJECT_SIGNAL_ARTIFACT_B64" | base64 -d > "/scripts/$PROJECT_SIGNAL_ARTIFACT_NAME"
printf '%s  %s\n' "${PROJECT_SIGNAL_ARTIFACT_DIGEST#sha256:}" "/scripts/$PROJECT_SIGNAL_ARTIFACT_NAME" | sha256sum -c -
chmod 0555 "/scripts/$PROJECT_SIGNAL_ARTIFACT_NAME"`},
		Env: []corev1.EnvVar{
			{Name: "PROJECT_SIGNAL_ARTIFACT_NAME", Value: artifact.Name},
			{Name: "PROJECT_SIGNAL_ARTIFACT_DIGEST", Value: artifact.Digest},
			{
				Name:  "PROJECT_SIGNAL_ARTIFACT_B64",
				Value: base64.StdEncoding.EncodeToString([]byte(artifact.Content)),
			},
		},
		VolumeMounts: []corev1.VolumeMount{{
			Name:      "script",
			MountPath: "/scripts",
		}},
	}
	applyWorkloadIdentity(&container)

	return container
}

func stageHostPathVolumes(paths []signalv1.WorkloadHostPath) []corev1.Volume {
	volumes := make([]corev1.Volume, 0, len(paths))
	for index := range paths {
		hostPathType := workloadHostPathType(paths[index].Path)
		volumes = append(volumes, corev1.Volume{
			Name: fmt.Sprintf("hostpath-%d", index),
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: paths[index].Path,
					Type: &hostPathType,
				},
			},
		})
	}

	return volumes
}

func stageHostPathMounts(paths []signalv1.WorkloadHostPath) []corev1.VolumeMount {
	mounts := make([]corev1.VolumeMount, 0, len(paths))
	for index := range paths {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      fmt.Sprintf("hostpath-%d", index),
			MountPath: paths[index].MountPath,
			ReadOnly:  paths[index].ReadOnly,
		})
	}

	return mounts
}

func (e *KubernetesStageExecutor) waitStagePod(
	ctx context.Context,
	invocation StageInvocation,
	pod *corev1.Pod,
) (signalv1.InvocationResultEnvelope, error) {
	ticker := time.NewTicker(e.Runner.pollInterval())
	defer ticker.Stop()

	for {
		current, err := e.Runner.Kubernetes.CoreV1().Pods(e.Namespace).
			Get(ctx, pod.Name, metav1.GetOptions{})
		if err != nil {
			return signalv1.InvocationResultEnvelope{}, fmt.Errorf("observe stage Pod: %w", err)
		}

		if current.UID != pod.UID {
			return signalv1.InvocationResultEnvelope{}, errors.New("stage Pod identity changed")
		}

		if current.Status.Phase == corev1.PodSucceeded || current.Status.Phase == corev1.PodFailed {
			logs, logErr := e.Runner.readPodLog(ctx, e.Namespace, pod.Name, "test", false)
			if logErr != nil {
				return signalv1.InvocationResultEnvelope{}, fmt.Errorf("read stage Pod result: %w", logErr)
			}

			envelope, parseErr := parseInvocationResultLog(logs, invocation)
			if parseErr != nil {
				return signalv1.InvocationResultEnvelope{}, parseErr
			}

			if current.Status.Phase == corev1.PodFailed &&
				envelope.Outcome == signalv1.InvocationOutcomePassed {
				return signalv1.InvocationResultEnvelope{},
					errors.New("failed stage Pod reported a passed invocation")
			}

			return envelope, nil
		}

		select {
		case <-ctx.Done():
			return signalv1.InvocationResultEnvelope{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (e *KubernetesStageExecutor) executeWorkflowTemplate(
	ctx context.Context,
	invocation StageInvocation,
) (envelope signalv1.InvocationResultEnvelope, returnErr error) {
	driver := invocation.Stage.Test.Driver.WorkflowTemplate
	if driver == nil {
		return signalv1.InvocationResultEnvelope{}, errors.New("WorkflowTemplate driver configuration is absent")
	}

	var workflowArtifact *signalv1.TestArtifact

	if driver.Artifact != "" {
		artifact, err := invocationArtifact(invocation, driver.Artifact)
		if err != nil {
			return signalv1.InvocationResultEnvelope{}, err
		}

		workflowArtifact = &artifact
	}

	parent, err := e.Runner.Dynamic.Resource(workflowGVR).Namespace(e.Namespace).
		Get(ctx, e.WorkflowName, metav1.GetOptions{})
	if err != nil {
		return signalv1.InvocationResultEnvelope{}, fmt.Errorf("get owning Workflow: %w", err)
	}

	templateGVR := schema.GroupVersionResource{
		Group:    "argoproj.io",
		Version:  "v1alpha1",
		Resource: "workflowtemplates",
	}

	template, err := e.Runner.Dynamic.Resource(templateGVR).Namespace(e.Namespace).
		Get(ctx, driver.Name, metav1.GetOptions{})
	if err != nil {
		return signalv1.InvocationResultEnvelope{}, fmt.Errorf("get approved WorkflowTemplate: %w", err)
	}

	if err := validateWorkflowTemplateIdentity(invocation, template); err != nil {
		return signalv1.InvocationResultEnvelope{}, err
	}

	templateSpec, found, err := unstructured.NestedMap(template.Object, "spec")
	if err != nil {
		return signalv1.InvocationResultEnvelope{}, fmt.Errorf("decode WorkflowTemplate spec: %w", err)
	}

	if !found {
		return signalv1.InvocationResultEnvelope{}, errors.New("approved WorkflowTemplate has no spec")
	}

	if err := validateWorkflowServiceAccount(templateSpec, e.WorkflowServiceAccount); err != nil {
		return signalv1.InvocationResultEnvelope{}, err
	}

	tolerations, err := e.participantTolerations(ctx, invocation.Participants)
	if err != nil {
		return signalv1.InvocationResultEnvelope{}, err
	}

	if err := applyWorkflowRuntimeIdentity(templateSpec, workflowRuntimeIdentityOptions{
		AllowRoot:   invocation.Stage.Test.Security.AllowRoot,
		Tolerations: tolerations,
	}); err != nil {
		return signalv1.InvocationResultEnvelope{}, err
	}

	podSpecPatch := []byte(workloadPodSpecPatch)
	if len(tolerations) > 0 {
		podSpecPatch, err = json.Marshal(struct {
			SecurityContext *corev1.PodSecurityContext `json:"securityContext"`
			Tolerations     []corev1.Toleration        `json:"tolerations"`
		}{
			SecurityContext: workloadPodSecurityContext(),
			Tolerations:     tolerations,
		})
		if err != nil {
			return signalv1.InvocationResultEnvelope{}, fmt.Errorf("encode WorkflowTemplate Pod spec patch: %w", err)
		}
	}

	templateSpec["podSpecPatch"] = string(podSpecPatch)

	claims := workflowPersistentVolumeClaims(templateSpec)

	locks, err := e.acquireWorkflowPersistentVolumeClaimLocks(ctx, claims, parent, invocation)
	if err != nil {
		return signalv1.InvocationResultEnvelope{}, err
	}

	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()

		if err := e.releaseWorkflowPersistentVolumeClaimLocks(cleanupCtx, locks); err != nil {
			returnErr = errors.Join(returnErr, err)
		}
	}()

	if err := e.validateWorkflowPersistentVolumeClaimsReady(ctx, claims); err != nil {
		return signalv1.InvocationResultEnvelope{}, err
	}

	name := invocationResourceName(e.ExecutionName, invocation, -1)

	parametersJSON, err := marshalStageJSON(invocation.Stage.Parameters)
	if err != nil {
		return signalv1.InvocationResultEnvelope{}, err
	}

	participantsJSON, err := marshalStageJSON(invocation.Participants)
	if err != nil {
		return signalv1.InvocationResultEnvelope{}, err
	}

	rolesJSON, err := marshalStageJSON(invocation.ParticipantRole)
	if err != nil {
		return signalv1.InvocationResultEnvelope{}, err
	}

	inputsJSON, err := marshalStageJSON(invocation.Stage.ResolvedInputs)
	if err != nil {
		return signalv1.InvocationResultEnvelope{}, err
	}

	testJSON, err := marshalStageJSON(invocation.Stage.Test)
	if err != nil {
		return signalv1.InvocationResultEnvelope{}, err
	}

	templateSpec["activeDeadlineSeconds"] = int64(invocation.Stage.Schedule.InvocationTimeout.Seconds())

	adapterParameters := []any{
		map[string]any{"name": "invocation-id", "value": invocation.ID},
		map[string]any{"name": "run-uid", "value": string(invocation.RunUID)},
		map[string]any{"name": "plan-digest", "value": invocation.PlanDigest},
		map[string]any{"name": "stage-id", "value": invocation.Stage.Name},
		map[string]any{"name": "stage-order", "value": strconv.Itoa(int(invocation.StageOrder))},
		map[string]any{"name": "test-json", "value": testJSON},
		map[string]any{"name": "parameters-json", "value": parametersJSON},
		map[string]any{"name": "participants-json", "value": participantsJSON},
		map[string]any{"name": "participant-roles-json", "value": rolesJSON},
		map[string]any{"name": "primary-participant-node-name", "value": invocation.Participants[0].NodeName},
		map[string]any{"name": "resolved-inputs-json", "value": inputsJSON},
		map[string]any{"name": "result-configmap", "value": name + "-result"},
	}
	if workflowArtifact != nil {
		adapterParameters = append(adapterParameters,
			map[string]any{"name": "script-name", "value": workflowArtifact.Name},
			map[string]any{
				"name":  "script-content-b64",
				"value": base64.StdEncoding.EncodeToString([]byte(workflowArtifact.Content)),
			},
			map[string]any{"name": "script-digest", "value": workflowArtifact.Digest},
		)
	}

	arguments, err := mergeWorkflowArguments(templateSpec["arguments"], adapterParameters)
	if err != nil {
		return signalv1.InvocationResultEnvelope{}, err
	}

	templateSpec["arguments"] = arguments

	podMetadata, _, err := unstructured.NestedMap(templateSpec, "podMetadata")
	if err != nil {
		return signalv1.InvocationResultEnvelope{}, fmt.Errorf("decode WorkflowTemplate pod metadata: %w", err)
	}

	if podMetadata == nil {
		podMetadata = map[string]any{}
	}

	podLabels, _, err := unstructured.NestedStringMap(podMetadata, "labels")
	if err != nil {
		return signalv1.InvocationResultEnvelope{}, fmt.Errorf("decode WorkflowTemplate Pod labels: %w", err)
	}

	if existing := podLabels[runUIDLabel]; existing != "" && existing != string(invocation.RunUID) {
		return signalv1.InvocationResultEnvelope{}, errors.New("approved WorkflowTemplate sets a conflicting Run UID Pod label")
	}

	if existing := podLabels[argoWorkflowLabel]; existing != "" && existing != name {
		return signalv1.InvocationResultEnvelope{}, errors.New("approved WorkflowTemplate sets a conflicting Workflow Pod label")
	}

	if podLabels == nil {
		podLabels = map[string]string{}
	}

	podLabels[runUIDLabel] = string(invocation.RunUID)
	podLabels[argoWorkflowLabel] = name

	if err := unstructured.SetNestedStringMap(podMetadata, podLabels, "labels"); err != nil {
		return signalv1.InvocationResultEnvelope{}, fmt.Errorf("set WorkflowTemplate Pod labels: %w", err)
	}

	templateSpec["podMetadata"] = podMetadata

	workflow := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Workflow",
		"metadata": map[string]any{
			"name":      name,
			"namespace": e.Namespace,
			"labels": map[string]any{
				runLabel:    e.ExecutionName,
				runUIDLabel: string(invocation.RunUID),
			},
			"ownerReferences": []any{workflowOwnerReferenceMap(parent)},
		},
		"spec": templateSpec,
	}}

	if workflowRestartMetricRequired(invocation) {
		if err := validateWorkflowRestartObservationSpec(workflow); err != nil {
			return signalv1.InvocationResultEnvelope{}, fmt.Errorf("%w: %w", errInvocationEvidenceIntegrity, err)
		}
	}

	created, err := e.Runner.Dynamic.Resource(workflowGVR).Namespace(e.Namespace).
		Create(ctx, workflow, metav1.CreateOptions{})
	if err != nil {
		return signalv1.InvocationResultEnvelope{}, fmt.Errorf("create stage Workflow: %w", err)
	}

	resultName := name + "-result"

	cleanupAttempted := false
	defer func() {
		if cleanupAttempted {
			return
		}

		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()

		if err := e.cleanupWorkflowInvocation(cleanupCtx, created, resultName); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("cleanup stage Workflow after failure: %w", err))
		}
	}()

	ticker := time.NewTicker(e.Runner.pollInterval())
	defer ticker.Stop()

	for {
		result, resultErr := e.Runner.Kubernetes.CoreV1().ConfigMaps(e.Namespace).
			Get(ctx, resultName, metav1.GetOptions{})
		if resultErr == nil {
			content := result.BinaryData[resultDataKey]

			if err := json.Unmarshal(content, &envelope); err != nil {
				return signalv1.InvocationResultEnvelope{}, fmt.Errorf("decode WorkflowTemplate result: %w", err)
			}

			if err := validateInvocationEnvelope(invocation, &envelope); err != nil {
				return signalv1.InvocationResultEnvelope{}, fmt.Errorf(
					"%w: validate raw WorkflowTemplate result: %w",
					errInvocationEvidenceIntegrity,
					err,
				)
			}

			if workflowRestartMetricRequired(invocation) {
				counts, err := e.observeWorkflowRestartSources(ctx, invocation, parent, created)
				if err != nil {
					return signalv1.InvocationResultEnvelope{}, err
				}

				if err := replaceWorkflowRestartMetric(&envelope, counts); err != nil {
					return signalv1.InvocationResultEnvelope{}, fmt.Errorf("%w: %w", errInvocationEvidenceIntegrity, err)
				}

				if err := appendRawWorkflowResultArtifact(&envelope, resultName, content); err != nil {
					return signalv1.InvocationResultEnvelope{}, fmt.Errorf("%w: %w", errInvocationEvidenceIntegrity, err)
				}
			}

			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			cleanupErr := e.cleanupWorkflowInvocation(cleanupCtx, created, resultName)

			cancel()

			cleanupAttempted = true

			if cleanupErr != nil {
				envelope.Cleanup = signalv1.InvocationCleanupResult{
					Outcome: signalv1.CleanupOutcomeFailed,
					Message: boundedStageMessage(cleanupErr.Error()),
				}
				if envelope.Outcome == signalv1.InvocationOutcomePassed {
					envelope.Outcome = signalv1.InvocationOutcomeInconclusive
					envelope.ReasonCode = signalv1.ResultReasonInfrastructureFailure
				}
			} else {
				envelope.Cleanup = signalv1.InvocationCleanupResult{Outcome: signalv1.CleanupOutcomeSucceeded}
			}

			return envelope, nil
		}

		if !apierrors.IsNotFound(resultErr) {
			return signalv1.InvocationResultEnvelope{}, resultErr
		}

		current, getErr := e.Runner.Dynamic.Resource(workflowGVR).Namespace(e.Namespace).
			Get(ctx, created.GetName(), metav1.GetOptions{})
		if getErr != nil {
			return signalv1.InvocationResultEnvelope{}, getErr
		}

		phase, found, phaseErr := unstructured.NestedString(current.Object, "status", "phase")
		if phaseErr != nil {
			return signalv1.InvocationResultEnvelope{}, fmt.Errorf("decode stage Workflow phase: %w", phaseErr)
		}

		if !found {
			phase = ""
		}

		if phase == "Failed" || phase == "Error" {
			return signalv1.InvocationResultEnvelope{}, errors.New("WorkflowTemplate invocation failed without a result")
		}

		select {
		case <-ctx.Done():
			return signalv1.InvocationResultEnvelope{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func workflowRestartMetricRequired(invocation StageInvocation) bool {
	for _, metric := range invocation.Stage.Test.Metrics {
		if metric.Name == workflowRestartMetricName {
			return true
		}
	}

	return false
}

func validateWorkflowRestartObservationSpec(workflow *unstructured.Unstructured) error {
	spec, found, err := unstructured.NestedMap(workflow.Object, "spec")
	if err != nil {
		return fmt.Errorf("decode Workflow spec for restart observation: %w", err)
	}

	if !found {
		return errors.New("workflow restart observation requires a Workflow spec")
	}

	if _, hasPodGC := spec["podGC"]; hasPodGC {
		return errors.New("workflow restart observation forbids Pod GC")
	}

	if _, hasTTLStrategy := spec["ttlStrategy"]; hasTTLStrategy {
		return errors.New("workflow restart observation forbids a TTL strategy")
	}

	entrypoint, found, err := unstructured.NestedString(spec, "entrypoint")
	if err != nil {
		return fmt.Errorf("decode Workflow entrypoint for restart observation: %w", err)
	}

	if !found || entrypoint == "" {
		return errors.New("workflow restart observation requires an entrypoint")
	}

	templates, found, err := unstructured.NestedSlice(spec, "templates")
	if err != nil {
		return fmt.Errorf("decode Workflow templates for restart observation: %w", err)
	}

	if !found || len(templates) != 1 {
		return errors.New("workflow restart observation requires exactly one Pod template")
	}

	template, ok := templates[0].(map[string]any)
	if !ok {
		return errors.New("workflow restart observation template is invalid")
	}

	name, _, err := unstructured.NestedString(template, "name")
	if err != nil {
		return fmt.Errorf("decode Workflow template name for restart observation: %w", err)
	}

	if name != entrypoint {
		return errors.New("workflow restart observation entrypoint must select the only template")
	}

	if _, hasContainer := template["container"]; !hasContainer {
		return errors.New("workflow restart observation requires a container template")
	}

	for _, unsupported := range []string{"containerSet", "dag", "data", "http", "resource", "script", "steps", "suspend"} {
		if _, hasUnsupported := template[unsupported]; hasUnsupported {
			return fmt.Errorf("workflow restart observation does not support %s templates", unsupported)
		}
	}

	retryStrategy, found, err := unstructured.NestedMap(template, "retryStrategy")
	if err != nil {
		return fmt.Errorf("decode Workflow retry strategy for restart observation: %w", err)
	}

	if !found {
		return errors.New("workflow restart observation requires an explicit zero retry limit")
	}

	limit, found := retryStrategy["limit"]
	if !found || (limit != "0" && limit != int64(0) && limit != float64(0)) {
		return errors.New("workflow restart observation requires a zero retry limit")
	}

	return nil
}

func (e *KubernetesStageExecutor) observeWorkflowRestartSources(
	ctx context.Context,
	invocation StageInvocation,
	parent *unstructured.Unstructured,
	created *unstructured.Unstructured,
) (workflowRestartSourceCounts, error) {
	current, err := e.Runner.Dynamic.Resource(workflowGVR).Namespace(e.Namespace).
		Get(ctx, created.GetName(), metav1.GetOptions{})
	if err != nil {
		return workflowRestartSourceCounts{}, fmt.Errorf("observe child Workflow restart sources: %w", err)
	}

	if current.GetUID() != created.GetUID() ||
		current.GetLabels()[runUIDLabel] != string(invocation.RunUID) ||
		!hasControllerOwner(current.GetOwnerReferences(), parent.GetUID(), parent.GetName(), "Workflow") {
		return workflowRestartSourceCounts{}, errors.New("child Workflow identity or ownership is ambiguous")
	}

	selector := fmt.Sprintf(
		"%s=%s,%s=%s",
		runUIDLabel,
		invocation.RunUID,
		argoWorkflowLabel,
		created.GetName(),
	)

	pods, err := e.Runner.Kubernetes.CoreV1().Pods(e.Namespace).
		List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return workflowRestartSourceCounts{}, fmt.Errorf("list child Workflow Pods: %w", err)
	}

	if len(pods.Items) == 0 {
		return workflowRestartSourceCounts{}, errors.New("child Workflow has no observable Pod")
	}

	counts := workflowRestartSourceCounts{WorkflowPod: int32(len(pods.Items) - 1)}
	for index := range pods.Items {
		pod := &pods.Items[index]
		if pod.Labels[runUIDLabel] != string(invocation.RunUID) ||
			pod.Labels[argoWorkflowLabel] != created.GetName() ||
			!hasControllerOwner(pod.OwnerReferences, created.GetUID(), created.GetName(), "Workflow") {
			return workflowRestartSourceCounts{}, fmt.Errorf("child Workflow Pod %q identity or ownership is ambiguous", pod.Name)
		}

		if len(pod.Status.ContainerStatuses) == 0 {
			return workflowRestartSourceCounts{}, fmt.Errorf("child Workflow Pod %q has no container status", pod.Name)
		}

		for _, status := range pod.Status.InitContainerStatuses {
			counts.Container += status.RestartCount
		}

		for _, status := range pod.Status.ContainerStatuses {
			counts.Container += status.RestartCount
		}

		for _, status := range pod.Status.EphemeralContainerStatuses {
			counts.Container += status.RestartCount
		}
	}

	return counts, nil
}

func hasControllerOwner(
	owners []metav1.OwnerReference,
	uid types.UID,
	name string,
	kind string,
) bool {
	controllerFound := false

	for _, owner := range owners {
		if owner.Controller == nil || !*owner.Controller {
			continue
		}

		if controllerFound ||
			owner.UID != uid ||
			owner.Name != name ||
			owner.Kind != kind ||
			owner.APIVersion != "argoproj.io/v1alpha1" {
			return false
		}

		controllerFound = true
	}

	return controllerFound
}

func replaceWorkflowRestartMetric(
	envelope *signalv1.InvocationResultEnvelope,
	counts workflowRestartSourceCounts,
) error {
	content, err := json.Marshal(counts)
	if err != nil {
		return fmt.Errorf("encode observed Workflow restart sources: %w", err)
	}

	measurementIndex := -1

	for index := range envelope.Measurements {
		if envelope.Measurements[index].Name != workflowRestartMetricName {
			continue
		}

		if measurementIndex >= 0 {
			return errors.New("raw WorkflowTemplate result duplicates restartSourceCounts")
		}

		measurementIndex = index
	}

	measurement := signalv1.MetricMeasurement{
		Name: workflowRestartMetricName,
		Value: signalv1.TypedValue{
			Type:        signalv1.TypedValueTypeString,
			StringValue: utilptr.To(string(content)),
		},
	}
	if measurementIndex >= 0 {
		envelope.Measurements[measurementIndex] = measurement

		return nil
	}

	if len(envelope.Measurements) >= 16 {
		return errors.New("raw WorkflowTemplate result has no room for observed restartSourceCounts")
	}

	envelope.Measurements = append(envelope.Measurements, measurement)

	return nil
}

func appendRawWorkflowResultArtifact(
	envelope *signalv1.InvocationResultEnvelope,
	resultName string,
	content []byte,
) error {
	for _, artifact := range envelope.Artifacts {
		if artifact.Name == rawWorkflowResultArtifactName {
			return errors.New("raw WorkflowTemplate result uses reserved artifact name")
		}
	}

	if len(envelope.Artifacts) >= maxInvocationResultArtifacts {
		return errors.New("raw WorkflowTemplate result has no room for platform provenance")
	}

	sum := sha256.Sum256(content)
	envelope.Artifacts = append(envelope.Artifacts, signalv1.ResultArtifactReference{
		Name:   rawWorkflowResultArtifactName,
		URI:    "urn:project-signal:workflow-result:" + resultName,
		Digest: "sha256:" + hex.EncodeToString(sum[:]),
	})

	return nil
}

func validateWorkflowServiceAccount(spec map[string]any, allowed string) error {
	if allowed == "" {
		return errors.New("approved WorkflowTemplate execution requires an allowed ServiceAccount")
	}

	serviceAccount, found, err := unstructured.NestedString(spec, "serviceAccountName")
	if err != nil {
		return fmt.Errorf("decode WorkflowTemplate ServiceAccount: %w", err)
	}

	if !found || serviceAccount != allowed {
		return fmt.Errorf("approved WorkflowTemplate must use ServiceAccount %q", allowed)
	}

	return validateWorkflowServiceAccountFields(spec, allowed)
}

func (e *KubernetesStageExecutor) validateWorkflowPersistentVolumeClaimsReady(
	ctx context.Context,
	claims []string,
) error {
	for _, claimName := range claims {
		claim, err := e.Runner.Kubernetes.CoreV1().PersistentVolumeClaims(e.Namespace).
			Get(ctx, claimName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get WorkflowTemplate PVC %s/%s: %w", e.Namespace, claimName, err)
		}

		if claim.Status.Phase != corev1.ClaimBound || claim.Spec.VolumeName == "" {
			return fmt.Errorf(
				"WorkflowTemplate PVC %s/%s is not bound to a PV",
				e.Namespace,
				claimName,
			)
		}

		pods, err := e.Runner.Kubernetes.CoreV1().Pods(e.Namespace).
			List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("list Pods before mounting PVC %s/%s: %w", e.Namespace, claimName, err)
		}

		for index := range pods.Items {
			if podReferencesPersistentVolumeClaim(&pods.Items[index], claimName) {
				return fmt.Errorf(
					"WorkflowTemplate PVC %s/%s is still referenced by Pod %s/%s",
					e.Namespace,
					claimName,
					pods.Items[index].Namespace,
					pods.Items[index].Name,
				)
			}
		}

		attachments, err := e.Runner.Kubernetes.StorageV1().VolumeAttachments().
			List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf(
				"list VolumeAttachments before mounting PVC %s/%s: %w",
				e.Namespace,
				claimName,
				err,
			)
		}

		for index := range attachments.Items {
			if volumeAttachmentReferencesPV(&attachments.Items[index], claim.Spec.VolumeName) {
				return fmt.Errorf(
					"WorkflowTemplate PVC %s/%s bound PV %s is still referenced by VolumeAttachment %s",
					e.Namespace,
					claimName,
					claim.Spec.VolumeName,
					attachments.Items[index].Name,
				)
			}
		}
	}

	return nil
}

func workflowPersistentVolumeClaims(spec map[string]any) []string {
	claimSet := map[string]struct{}{}
	collectPersistentVolumeClaims(spec, claimSet)

	claims := make([]string, 0, len(claimSet))
	for claimName := range claimSet {
		claims = append(claims, claimName)
	}

	sort.Strings(claims)

	return claims
}

func collectPersistentVolumeClaims(value any, claims map[string]struct{}) {
	switch typed := value.(type) {
	case map[string]any:
		if source, ok := typed["persistentVolumeClaim"].(map[string]any); ok {
			if claimName, ok := source["claimName"].(string); ok && claimName != "" {
				claims[claimName] = struct{}{}
			}
		}

		for key, child := range typed {
			if key == "resource" {
				if resourceTemplate, ok := child.(map[string]any); ok {
					if manifest, ok := resourceTemplate["manifest"].(string); ok {
						resource := map[string]any{}
						if yaml.Unmarshal([]byte(manifest), &resource) == nil {
							collectPersistentVolumeClaims(resource, claims)
						}
					}
				}

				continue
			}

			collectPersistentVolumeClaims(child, claims)
		}
	case []any:
		for _, child := range typed {
			collectPersistentVolumeClaims(child, claims)
		}
	}
}

func (e *KubernetesStageExecutor) acquireWorkflowPersistentVolumeClaimLocks(
	ctx context.Context,
	claims []string,
	parent *unstructured.Unstructured,
	invocation StageInvocation,
) ([]*corev1.ConfigMap, error) {
	locks := make([]*corev1.ConfigMap, 0, len(claims))

	for _, claimName := range claims {
		lock := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      workflowPersistentVolumeClaimLockName(e.Namespace, claimName),
				Namespace: e.Namespace,
				Labels: map[string]string{
					"app.kubernetes.io/name":    "project-signal-pvc-lock",
					"app.kubernetes.io/part-of": "project-signal",
				},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion:         parent.GetAPIVersion(),
					Kind:               parent.GetKind(),
					Name:               parent.GetName(),
					UID:                parent.GetUID(),
					Controller:         utilptr.To(true),
					BlockOwnerDeletion: utilptr.To(true),
				}},
			},
			Data: map[string]string{
				"claim":       claimName,
				"invocation":  invocation.ID,
				"runUID":      string(invocation.RunUID),
				"workflowUID": string(parent.GetUID()),
			},
		}

		created, err := e.Runner.Kubernetes.CoreV1().ConfigMaps(e.Namespace).
			Create(ctx, lock, metav1.CreateOptions{})
		if err != nil {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			releaseErr := e.releaseWorkflowPersistentVolumeClaimLocks(
				cleanupCtx,
				locks,
			)

			cancel()

			return nil, errors.Join(
				fmt.Errorf(
					"acquire exclusive WorkflowTemplate PVC lock for %s/%s: %w",
					e.Namespace,
					claimName,
					err,
				),
				releaseErr,
			)
		}

		locks = append(locks, created)
	}

	return locks, nil
}

func (e *KubernetesStageExecutor) releaseWorkflowPersistentVolumeClaimLocks(
	ctx context.Context,
	locks []*corev1.ConfigMap,
) error {
	var joined error

	for index := len(locks) - 1; index >= 0; index-- {
		lockUID := locks[index].UID

		err := e.Runner.Kubernetes.CoreV1().ConfigMaps(e.Namespace).Delete(
			ctx,
			locks[index].Name,
			metav1.DeleteOptions{
				Preconditions: &metav1.Preconditions{UID: &lockUID},
			},
		)
		if err != nil && !apierrors.IsNotFound(err) {
			joined = errors.Join(
				joined,
				fmt.Errorf("release WorkflowTemplate PVC lock %s: %w", locks[index].Name, err),
			)
		}
	}

	return joined
}

func workflowPersistentVolumeClaimLockName(namespace, claimName string) string {
	digest := sha256.Sum256([]byte(namespace + "\x00" + claimName))

	return "project-signal-pvc-lock-" + hex.EncodeToString(digest[:8])
}

func podReferencesPersistentVolumeClaim(pod *corev1.Pod, claimName string) bool {
	for index := range pod.Spec.Volumes {
		source := pod.Spec.Volumes[index].PersistentVolumeClaim
		if source != nil && source.ClaimName == claimName {
			return true
		}
	}

	return false
}

func volumeAttachmentReferencesPV(attachment *storagev1.VolumeAttachment, pvName string) bool {
	return attachment.Spec.Source.PersistentVolumeName != nil &&
		*attachment.Spec.Source.PersistentVolumeName == pvName
}

func validateWorkflowServiceAccountFields(value any, allowed string) error {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			switch key {
			case "podSpecPatch":
				return errors.New("approved WorkflowTemplate cannot use podSpecPatch")
			case argoWorkflowLabel:
				return errors.New("approved WorkflowTemplate cannot set the reserved Workflow Pod label")
			case "serviceAccountName":
				serviceAccount, ok := child.(string)
				if !ok || serviceAccount != allowed {
					return fmt.Errorf("approved WorkflowTemplate selects unauthorized ServiceAccount %q", child)
				}
			case "templateRef", "workflowTemplateRef":
				return fmt.Errorf("approved WorkflowTemplate cannot use external reference %q", key)
			case "resource":
				resourceTemplate, ok := child.(map[string]any)
				if !ok {
					return errors.New("WorkflowTemplate resource must be an object")
				}

				manifest, ok := resourceTemplate["manifest"].(string)
				if !ok {
					return errors.New("WorkflowTemplate resource manifest must be a string")
				}

				resource := map[string]any{}
				if err := yaml.Unmarshal([]byte(manifest), &resource); err != nil {
					return fmt.Errorf("decode WorkflowTemplate resource manifest: %w", err)
				}

				if err := validateWorkflowServiceAccountFields(resource, allowed); err != nil {
					return err
				}
			case "metadata":
				metadata, ok := child.(map[string]any)
				if !ok {
					break
				}

				labels, ok := metadata["labels"].(map[string]any)
				if !ok {
					break
				}

				if _, exists := labels[runUIDLabel]; exists {
					return fmt.Errorf("approved WorkflowTemplate metadata label %q is reserved", runUIDLabel)
				}
			}

			if err := validateWorkflowServiceAccountFields(child, allowed); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range typed {
			if err := validateWorkflowServiceAccountFields(child, allowed); err != nil {
				return err
			}
		}
	}

	return nil
}

type workflowRuntimeIdentityOptions struct {
	AllowRoot   bool
	Tolerations []corev1.Toleration
}

func applyWorkflowRuntimeIdentity(value any, runtimeOptions ...workflowRuntimeIdentityOptions) error {
	options := workflowRuntimeIdentityOptions{}
	if len(runtimeOptions) > 0 {
		options = runtimeOptions[0]
	}

	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			switch key {
			case "container", "script":
				container, ok := child.(map[string]any)
				if !ok {
					return fmt.Errorf("WorkflowTemplate %s must be an object", key)
				}

				if err := applyUnstructuredContainerIdentity(container, options.AllowRoot); err != nil {
					return err
				}
			case "containers", "initContainers", "sidecars", "ephemeralContainers":
				containers, ok := child.([]any)
				if !ok {
					return fmt.Errorf("WorkflowTemplate %s must be a list", key)
				}

				for index, item := range containers {
					container, ok := item.(map[string]any)
					if !ok {
						return fmt.Errorf("WorkflowTemplate %s[%d] must be an object", key, index)
					}

					if err := applyUnstructuredContainerIdentity(container, options.AllowRoot); err != nil {
						return err
					}
				}
			case "resource":
				resourceTemplate, ok := child.(map[string]any)
				if !ok {
					return errors.New("WorkflowTemplate resource must be an object")
				}

				if err := applyWorkflowResourceRuntimeIdentity(resourceTemplate, options); err != nil {
					return err
				}
			}

			if err := applyWorkflowRuntimeIdentity(child, options); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range typed {
			if err := applyWorkflowRuntimeIdentity(child, options); err != nil {
				return err
			}
		}
	}

	return nil
}

func applyWorkflowResourceRuntimeIdentity(
	resourceTemplate map[string]any,
	options workflowRuntimeIdentityOptions,
) error {
	manifest, ok := resourceTemplate["manifest"].(string)
	if !ok {
		return errors.New("WorkflowTemplate resource manifest must be a string")
	}

	resource := map[string]any{}
	if err := yaml.Unmarshal([]byte(manifest), &resource); err != nil {
		return fmt.Errorf("decode WorkflowTemplate resource manifest: %w", err)
	}

	kind, ok := resource["kind"].(string)
	if !ok || kind == "" {
		return errors.New("WorkflowTemplate resource kind must be a non-empty string")
	}

	switch kind {
	case "Pod":
		if err := applyNestedPodSpecIdentity(resource, options, "spec"); err != nil {
			return err
		}
	case "Job", "Deployment", "DaemonSet", "StatefulSet", "ReplicaSet":
		if err := applyNestedPodSpecIdentity(resource, options, "spec", "template", "spec"); err != nil {
			return err
		}
	case "CronJob":
		if err := applyNestedPodSpecIdentity(
			resource,
			options,
			"spec",
			"jobTemplate",
			"spec",
			"template",
			"spec",
		); err != nil {
			return err
		}
	case "MPIJob":
		replicas, found, err := unstructured.NestedMap(resource, "spec", "mpiReplicaSpecs")
		if err != nil || !found {
			return errors.New("WorkflowTemplate MPIJob resource has no replica specifications")
		}

		for role, replica := range replicas {
			replicaSpec, ok := replica.(map[string]any)
			if !ok {
				return fmt.Errorf("WorkflowTemplate MPIJob replica %q must be an object", role)
			}

			if err := applyNestedPodSpecIdentity(replicaSpec, options, "template", "spec"); err != nil {
				return fmt.Errorf("apply WorkflowTemplate MPIJob replica %q identity: %w", role, err)
			}
		}

		if err := unstructured.SetNestedMap(resource, replicas, "spec", "mpiReplicaSpecs"); err != nil {
			return fmt.Errorf("write WorkflowTemplate MPIJob replica identities: %w", err)
		}
	case "ConfigMap", "Secret", "Service", "PersistentVolumeClaim":
	default:
		return fmt.Errorf(
			"WorkflowTemplate resource kind %q is not approved for fixed runtime identity enforcement",
			kind,
		)
	}

	content, err := json.Marshal(resource)
	if err != nil {
		return fmt.Errorf("encode WorkflowTemplate resource manifest: %w", err)
	}

	resourceTemplate["manifest"] = string(content)

	return nil
}

func applyNestedPodSpecIdentity(
	resource map[string]any,
	options workflowRuntimeIdentityOptions,
	fields ...string,
) error {
	spec, found, err := unstructured.NestedMap(resource, fields...)
	if err != nil || !found {
		return fmt.Errorf("WorkflowTemplate resource has no Pod spec at %q", strings.Join(fields, "."))
	}

	if err := applyUnstructuredPodSpecIdentity(spec, options); err != nil {
		return err
	}

	if err := unstructured.SetNestedMap(resource, spec, fields...); err != nil {
		return fmt.Errorf("write WorkflowTemplate resource Pod spec: %w", err)
	}

	return nil
}

func applyUnstructuredPodSpecIdentity(
	spec map[string]any,
	options workflowRuntimeIdentityOptions,
) error {
	securityContext := map[string]any{}

	if value, found := spec["securityContext"]; found {
		typed, ok := value.(map[string]any)
		if !ok {
			return errors.New("WorkflowTemplate Pod securityContext must be an object")
		}

		securityContext = typed
	}

	if securityContext == nil {
		securityContext = map[string]any{}
	}

	securityContext["runAsUser"] = workloadIdentityID
	securityContext["runAsGroup"] = workloadIdentityID
	securityContext["runAsNonRoot"] = true
	securityContext["fsGroup"] = workloadIdentityID
	spec["securityContext"] = securityContext

	if len(options.Tolerations) > 0 {
		converted := make([]any, 0, len(options.Tolerations))
		for index := range options.Tolerations {
			value, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&options.Tolerations[index])
			if err != nil {
				return fmt.Errorf("convert participant toleration: %w", err)
			}

			converted = append(converted, value)
		}

		spec["tolerations"] = converted
	}

	for _, field := range []string{"initContainers", "containers", "ephemeralContainers"} {
		value, found := spec[field]
		if !found {
			continue
		}

		containers, ok := value.([]any)
		if !ok {
			return fmt.Errorf("WorkflowTemplate Pod %s must be a list", field)
		}

		for _, item := range containers {
			container, ok := item.(map[string]any)
			if !ok {
				return fmt.Errorf("WorkflowTemplate Pod %s entry must be an object", field)
			}

			if err := applyUnstructuredContainerIdentity(container, options.AllowRoot); err != nil {
				return err
			}
		}
	}

	return nil
}

func applyUnstructuredContainerIdentity(container map[string]any, allowRoot ...bool) error {
	securityContext := map[string]any{}

	if value, found := container["securityContext"]; found {
		typed, ok := value.(map[string]any)
		if !ok {
			return errors.New("WorkflowTemplate container securityContext must be an object")
		}

		securityContext = typed
	}

	if securityContext == nil {
		securityContext = map[string]any{}
	}

	if len(allowRoot) > 0 && allowRoot[0] && securityContext["runAsUser"] == int64(0) {
		if securityContext["runAsGroup"] != int64(0) ||
			securityContext["runAsNonRoot"] != false {
			return errors.New("WorkflowTemplate root container must set runAsGroup 0 and runAsNonRoot false")
		}

		container["securityContext"] = securityContext

		return nil
	}

	securityContext["runAsUser"] = workloadIdentityID
	securityContext["runAsGroup"] = workloadIdentityID
	securityContext["runAsNonRoot"] = true
	container["securityContext"] = securityContext

	return nil
}

func (e *KubernetesStageExecutor) participantTolerations(
	ctx context.Context,
	participants []signalv1.Participant,
) ([]corev1.Toleration, error) {
	byIdentity := map[string]corev1.Toleration{}

	for _, participant := range participants {
		node, err := e.Runner.Kubernetes.CoreV1().Nodes().
			Get(ctx, participant.NodeName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}

			return nil, fmt.Errorf("get participant node %s tolerations: %w", participant.NodeName, err)
		}

		if node.UID != participant.NodeUID {
			return nil, fmt.Errorf("participant node %s identity changed", participant.NodeName)
		}

		for _, taint := range node.Spec.Taints {
			if taint.Effect != corev1.TaintEffectNoSchedule &&
				taint.Effect != corev1.TaintEffectNoExecute {
				continue
			}

			toleration := corev1.Toleration{
				Key:      taint.Key,
				Operator: corev1.TolerationOpEqual,
				Value:    taint.Value,
				Effect:   taint.Effect,
			}
			identity := toleration.Key + "\x00" + toleration.Value + "\x00" + string(toleration.Effect)
			byIdentity[identity] = toleration
		}
	}

	identities := make([]string, 0, len(byIdentity))
	for identity := range byIdentity {
		identities = append(identities, identity)
	}

	sort.Strings(identities)

	tolerations := make([]corev1.Toleration, 0, len(identities))
	for _, identity := range identities {
		tolerations = append(tolerations, byIdentity[identity])
	}

	return tolerations, nil
}

func validateWorkflowTemplateIdentity(
	invocation StageInvocation,
	template *unstructured.Unstructured,
) error {
	driver := invocation.Stage.Test.Driver.WorkflowTemplate
	if driver == nil || template.GetName() != driver.Name {
		return errors.New("live WorkflowTemplate identity does not match the approved driver")
	}

	digest, err := workflowTemplateSpecDigest(template)
	if err != nil {
		return err
	}

	if digest != driver.Digest {
		return errors.New("live WorkflowTemplate digest does not match the approved driver")
	}

	for _, input := range invocation.Stage.ResolvedInputs {
		if input.Kind != signalv1.ResolvedInputKindWorkflowTemplate ||
			input.State != signalv1.ResolvedInputStateComplete {
			continue
		}

		if input.Digest != driver.Digest {
			return errors.New("resolved WorkflowTemplate digest does not match the approved driver")
		}

		expected := template.GetName() + "@" + template.GetResourceVersion()
		if input.Resolved != expected {
			return errors.New("live WorkflowTemplate identity does not match the immutable resolved input")
		}

		return nil
	}

	return nil
}

func workflowTemplateSpecDigest(template *unstructured.Unstructured) (string, error) {
	spec, found, err := unstructured.NestedFieldNoCopy(template.Object, "spec")
	if err != nil {
		return "", fmt.Errorf("decode WorkflowTemplate spec: %w", err)
	}

	if !found {
		return "", errors.New("approved WorkflowTemplate has no spec")
	}

	content, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("encode WorkflowTemplate spec: %w", err)
	}

	digest := sha256.Sum256(content)

	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func (e *KubernetesStageExecutor) cleanupWorkflowInvocation(
	ctx context.Context,
	workflow *unstructured.Unstructured,
	resultName string,
) error {
	propagation := metav1.DeletePropagationForeground
	workflowUID := workflow.GetUID()

	err := e.Runner.Dynamic.Resource(workflowGVR).Namespace(e.Namespace).Delete(
		ctx,
		workflow.GetName(),
		metav1.DeleteOptions{
			Preconditions:     &metav1.Preconditions{UID: &workflowUID},
			PropagationPolicy: &propagation,
		},
	)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete WorkflowTemplate invocation: %w", err)
	}

	ticker := time.NewTicker(e.Runner.pollInterval())
	defer ticker.Stop()

	absentObserved := false

	for {
		_, workflowErr := e.Runner.Dynamic.Resource(workflowGVR).Namespace(e.Namespace).
			Get(ctx, workflow.GetName(), metav1.GetOptions{})

		result, resultErr := e.Runner.Kubernetes.CoreV1().ConfigMaps(e.Namespace).
			Get(ctx, resultName, metav1.GetOptions{})
		if apierrors.IsNotFound(workflowErr) && apierrors.IsNotFound(resultErr) {
			if absentObserved {
				return nil
			}

			absentObserved = true
		} else {
			absentObserved = false
		}

		if workflowErr != nil && !apierrors.IsNotFound(workflowErr) {
			return fmt.Errorf("verify WorkflowTemplate invocation cleanup: %w", workflowErr)
		}

		if resultErr != nil && !apierrors.IsNotFound(resultErr) {
			return fmt.Errorf("verify WorkflowTemplate result cleanup: %w", resultErr)
		}

		if resultErr == nil {
			resultUID := result.UID

			err = e.Runner.Kubernetes.CoreV1().ConfigMaps(e.Namespace).Delete(
				ctx,
				result.Name,
				metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &resultUID}},
			)
			if err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete WorkflowTemplate result: %w", err)
			}
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for WorkflowTemplate cleanup: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func workflowOwnerReferenceMap(workflow *unstructured.Unstructured) map[string]any {
	return map[string]any{
		"apiVersion":         workflow.GetAPIVersion(),
		"kind":               workflow.GetKind(),
		"name":               workflow.GetName(),
		"uid":                string(workflow.GetUID()),
		"controller":         true,
		"blockOwnerDeletion": true,
	}
}

func parseInvocationResultLog(
	content string,
	invocation StageInvocation,
) (signalv1.InvocationResultEnvelope, error) {
	for _, line := range strings.Split(content, "\n") {
		if !strings.HasPrefix(line, invocationResultPrefix) {
			continue
		}

		var envelope signalv1.InvocationResultEnvelope

		decoder := json.NewDecoder(strings.NewReader(strings.TrimPrefix(line, invocationResultPrefix)))
		decoder.DisallowUnknownFields()

		if err := decoder.Decode(&envelope); err != nil {
			return signalv1.InvocationResultEnvelope{}, fmt.Errorf("decode invocation result: %w", err)
		}

		return envelope, nil
	}

	legacy, err := ParseResultLog(content, int(invocation.Participants[0].Index))
	if err != nil {
		return signalv1.InvocationResultEnvelope{}, errors.New("stage output has no canonical invocation result")
	}

	return convertLegacyInvocationResult(invocation, legacy)
}

func convertLegacyInvocationResult(
	invocation StageInvocation,
	legacy Result,
) (signalv1.InvocationResultEnvelope, error) {
	if len(invocation.Participants) != 1 {
		return signalv1.InvocationResultEnvelope{}, errors.New("legacy result conversion supports one participant")
	}

	measurements, err := legacyMeasurements(legacy.Measurements, invocation.Stage.Test.Metrics)
	if err != nil {
		return signalv1.InvocationResultEnvelope{}, err
	}

	outcome := signalv1.InvocationOutcomeInconclusive
	reason := signalv1.ResultReasonInvalidResult

	switch legacy.Verdict {
	case "Pass":
		outcome = signalv1.InvocationOutcomePassed
		reason = signalv1.ResultReasonCompleted
	case "Fail":
		outcome = signalv1.InvocationOutcomeFailed
		reason = signalv1.ResultReasonAssertionFailed
	}

	participant := invocation.Participants[0]
	participantResult := signalv1.InvocationParticipantResult{
		Index:   participant.Index,
		Role:    invocation.ParticipantRole[participant.Index],
		Outcome: outcome,
		Start:   signalv1.NodeFingerprint{NodeUID: participant.NodeUID},
		End:     signalv1.NodeFingerprint{NodeUID: participant.NodeUID},
	}

	qualification, qualificationErr := decodeQualificationMeasurements(
		legacy.Measurements,
		int(participant.Index),
		string(signalv1.ExecutionBackendPod),
	)
	if qualificationErr != nil {
		return signalv1.InvocationResultEnvelope{}, qualificationErr
	}

	if qualification != nil {
		participantResult.Start.DriverVersion = qualification.DriverVersion
		participantResult.End.DriverVersion = qualification.DriverVersion
		participantResult.Start.GPUInventoryDigest = qualification.GPUInventoryDigest
		participantResult.End.GPUInventoryDigest = qualification.GPUInventoryDigest
		participantResult.VisibleDevices = append([]string(nil), qualification.VisibleDevices...)
		participantResult.CoveredDevices = append([]string(nil), qualification.CoveredDevices...)
	}

	startedAt := legacy.StartedAt
	finishedAt := legacy.FinishedAt

	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}

	if finishedAt.Before(startedAt) {
		finishedAt = startedAt
	}

	return signalv1.InvocationResultEnvelope{
		SchemaVersion:     signalv1.InvocationResultSchemaV1,
		InvocationID:      invocation.ID,
		RunUID:            invocation.RunUID,
		StageID:           invocation.Stage.Name,
		StageOrder:        invocation.StageOrder,
		PlanDigest:        invocation.PlanDigest,
		Test:              invocation.Stage.Test.Identity,
		ResolvedInputs:    invocation.Stage.ResolvedInputs,
		StartedAt:         metav1.NewTime(startedAt),
		FinishedAt:        metav1.NewTime(finishedAt),
		MonotonicDuration: metav1.Duration{Duration: finishedAt.Sub(startedAt)},
		Outcome:           outcome,
		ReasonCode:        reason,
		Message:           boundedStageMessage(legacy.Reason),
		Measurements:      measurements,
		Participants:      []signalv1.InvocationParticipantResult{participantResult},
		Activity: signalv1.WorkloadActivitySummary{
			SampleCount:          1,
			SampleSequenceDigest: digestText(legacy.Measurements),
		},
		Cleanup: signalv1.InvocationCleanupResult{Outcome: signalv1.CleanupOutcomeSucceeded},
	}, nil
}

func legacyMeasurements(
	content string,
	definitions []signalv1.TestMetricDefinition,
) ([]signalv1.MetricMeasurement, error) {
	var values map[string]any
	if err := json.Unmarshal([]byte(content), &values); err != nil {
		return nil, fmt.Errorf("decode legacy measurements: %w", err)
	}

	declared := make(map[string]signalv1.TestMetricDefinition, len(definitions))
	for _, definition := range definitions {
		declared[definition.Name] = definition
	}

	result := make([]signalv1.MetricMeasurement, 0, len(values))
	for name, value := range values {
		normalized := strings.ToLower(name)
		if strings.Contains(normalized, "uuid") ||
			strings.Contains(normalized, "nodename") ||
			strings.Contains(normalized, "node_name") {
			continue
		}

		definition, exists := declared[name]
		typed, ok := legacyTypedValue(value)
		unit := ""

		if exists {
			typed, ok = legacyTypedMetricValue(value, definition.Type)
			unit = definition.Unit
		}

		if !ok {
			continue
		}

		result = append(result, signalv1.MetricMeasurement{
			Name:  name,
			Value: typed,
			Unit:  unit,
		})
	}

	sortMeasurements(result)

	return result, nil
}

func legacyTypedMetricValue(
	value any,
	metricType signalv1.MetricValueType,
) (signalv1.TypedValue, bool) {
	switch metricType {
	case signalv1.MetricValueTypeBoolean:
		typed, ok := value.(bool)
		if !ok {
			return signalv1.TypedValue{}, false
		}

		return signalv1.TypedValue{
			Type:         signalv1.TypedValueTypeBoolean,
			BooleanValue: &typed,
		}, true
	case signalv1.MetricValueTypeStatus:
		typed, ok := value.(string)
		if !ok {
			return signalv1.TypedValue{}, false
		}

		return signalv1.TypedValue{
			Type:        signalv1.TypedValueTypeString,
			StringValue: &typed,
		}, true
	case signalv1.MetricValueTypeCount:
		number, ok := value.(float64)
		if !ok || number != float64(int64(number)) {
			return signalv1.TypedValue{}, false
		}

		integer := int64(number)

		return signalv1.TypedValue{
			Type:         signalv1.TypedValueTypeInteger,
			IntegerValue: &integer,
		}, true
	case signalv1.MetricValueTypeGauge,
		signalv1.MetricValueTypeRatio,
		signalv1.MetricValueTypeThroughput,
		signalv1.MetricValueTypeIOPS,
		signalv1.MetricValueTypeBandwidth,
		signalv1.MetricValueTypeLatency:
		number, ok := value.(float64)
		if !ok || math.IsNaN(number) || math.IsInf(number, 0) {
			return signalv1.TypedValue{}, false
		}

		decimal := strconv.FormatFloat(number, 'f', -1, 64)

		return signalv1.TypedValue{
			Type:         signalv1.TypedValueTypeDecimal,
			DecimalValue: &decimal,
		}, true
	default:
		return legacyTypedValue(value)
	}
}

func legacyTypedValue(value any) (signalv1.TypedValue, bool) {
	switch typed := value.(type) {
	case bool:
		return signalv1.TypedValue{Type: signalv1.TypedValueTypeBoolean, BooleanValue: &typed}, true
	case string:
		return signalv1.TypedValue{Type: signalv1.TypedValueTypeString, StringValue: &typed}, true
	case float64:
		if typed == float64(int64(typed)) {
			integer := int64(typed)
			return signalv1.TypedValue{Type: signalv1.TypedValueTypeInteger, IntegerValue: &integer}, true
		}

		decimal := strconv.FormatFloat(typed, 'f', -1, 64)

		return signalv1.TypedValue{Type: signalv1.TypedValueTypeDecimal, DecimalValue: &decimal}, true
	default:
		return signalv1.TypedValue{}, false
	}
}

func stageDriverImage(driver signalv1.CertificationTestDriver) string {
	switch driver.Type {
	case signalv1.TestDriverTypeScript:
		if driver.Script != nil {
			return driver.Script.RunnerImage
		}
	case signalv1.TestDriverTypeContainer:
		if driver.Container != nil {
			return driver.Container.Image
		}
	}

	return ""
}

func driverResources(driver signalv1.CertificationTestDriver) corev1.ResourceRequirements {
	if driver.Script != nil {
		return *driver.Script.Resources.DeepCopy()
	}

	if driver.Container != nil {
		return *driver.Container.Resources.DeepCopy()
	}

	return corev1.ResourceRequirements{}
}

func applyStageSecurity(container *corev1.Container, security signalv1.ResolvedSecurityRequirements) {
	runAsUser := workloadIdentityID
	runAsGroup := workloadIdentityID
	runAsNonRoot := true
	if security.AllowRoot {
		runAsUser = 0
		runAsGroup = 0
		runAsNonRoot = false
	}

	container.SecurityContext = &corev1.SecurityContext{
		Privileged:   &security.Privileged,
		RunAsUser:    utilptr.To(runAsUser),
		RunAsGroup:   utilptr.To(runAsGroup),
		RunAsNonRoot: utilptr.To(runAsNonRoot),
	}
	if security.IPCLock || len(security.Capabilities) > 0 {
		container.SecurityContext.Capabilities = &corev1.Capabilities{}
		if security.IPCLock {
			container.SecurityContext.Capabilities.Add = append(
				container.SecurityContext.Capabilities.Add,
				corev1.Capability("IPC_LOCK"),
			)
		}

		for _, capability := range security.Capabilities {
			container.SecurityContext.Capabilities.Add = append(
				container.SecurityContext.Capabilities.Add,
				corev1.Capability(capability),
			)
		}
	}
}

func scriptInvocationWrapper(language signalv1.ScriptLanguage, key, digest string) string {
	interpreter := "/bin/sh"
	if language == signalv1.ScriptLanguagePython {
		interpreter = "python3"
	}

	return fmt.Sprintf(`
set +e
results_dir="${PROJECT_SIGNAL_RESULTS_DIR:-/results}"
mkdir -p "$results_dir"
rm -f "$results_dir/verdict" "$results_dir/reason" "$results_dir/measurements.json"
if [ -n "${PROJECT_SIGNAL_LIVE_METRICS_DONE:-}" ]; then
  trap ': > "$PROJECT_SIGNAL_LIVE_METRICS_DONE" || true' EXIT
fi
script=/scripts/%s
want=%s
got=$(sha256sum "$script" | awk '{print $1}')
if [ "sha256:$got" != "$want" ]; then
  printf 'Inconclusive' > "$results_dir/verdict"
  printf 'ScriptDigestMismatch' > "$results_dir/reason"
  status=2
else
  %s "$script"
  status=$?
fi

if [ -n "${PROJECT_SIGNAL_LIVE_METRICS_DONE:-}" ]; then
  : > "$PROJECT_SIGNAL_LIVE_METRICS_DONE"
  if ! python3 - "$results_dir/measurements.json" <<'PY_METRICS'
import json
import os
import pathlib
import sys
import time
import urllib.error
import urllib.request

measurements_path = pathlib.Path(sys.argv[1])
measurements = {}
if measurements_path.exists():
    measurements = json.loads(measurements_path.read_text(encoding="utf-8"))
    if not isinstance(measurements, dict):
        raise SystemExit("measurements must be an object")
final = None
for _ in range(30):
    try:
        port = os.environ["PROJECT_SIGNAL_LIVE_METRICS_PORT"]
        with urllib.request.urlopen("http://127.0.0.1:" + port + "/final", timeout=2) as response:
            if response.status == 202:
                time.sleep(1)
                continue
            final = json.load(response)
            break
    except urllib.error.HTTPError as error:
        raise SystemExit("metric contract violation") from error
    except OSError:
        time.sleep(1)
if final is None:
    raise SystemExit("metric telemetry timeout")
if not isinstance(final, dict):
    raise SystemExit("final metric telemetry must be an object")
if set(measurements) & set(final):
    raise SystemExit("trusted metric telemetry collides with workload measurements")
measurements.update(final)
measurements_path.write_text(
    json.dumps(measurements, sort_keys=True, separators=(",", ":")),
    encoding="utf-8",
)
PY_METRICS
  then
    printf 'Inconclusive' > "$results_dir/verdict"
    printf 'MetricContractViolation' > "$results_dir/reason"
    status=2
  fi
fi

if [ -f "$results_dir/verdict" ] || [ -f "$results_dir/reason" ] || [ -f "$results_dir/measurements.json" ]; then
  verdict="$(cat "$results_dir/verdict" 2>/dev/null || true)"
  reason="$(cat "$results_dir/reason" 2>/dev/null || true)"
  expected_status=2
  case "$verdict" in
    Pass) expected_status=0 ;;
    Fail) expected_status=1 ;;
    Inconclusive) expected_status=2 ;;
    *)
      verdict=Inconclusive
      reason=InvalidResultVerdict
      expected_status=2
      ;;
  esac

  if [ -z "$reason" ]; then
    verdict=Inconclusive
    reason=MissingResultReason
    expected_status=2
  elif [ "$status" -ne "$expected_status" ]; then
    verdict=Inconclusive
    reason=ResultExitCodeMismatch
    expected_status=2
  fi

  encode_file() {
    if [ -f "$1" ]; then
      base64 < "$1" | tr -d '\n'
    else
      printf '%%s' -
    fi
  }

  printf '%s%%s %%s %%s %%d\n' \
    "$(printf '%%s' "$verdict" | base64 | tr -d '\n')" \
    "$(printf '%%s' "$reason" | base64 | tr -d '\n')" \
    "$(encode_file "$results_dir/measurements.json")" \
    "$expected_status"
  exit "$expected_status"
fi

exit "$status"
`, key, digest, interpreter, resultPrefix)
}

func invocationResourceName(base string, invocation StageInvocation, participant int32) string {
	base = strings.Trim(strings.ToLower(base), "-.")

	suffix := "-" + invocation.ID[len(invocation.ID)-8:]
	if participant >= 0 {
		suffix += fmt.Sprintf("-p%d", participant)
	}

	return resourceNameWithSuffix(base, suffix, 63)
}

func (e *KubernetesStageExecutor) deletePod(ctx context.Context, pod *corev1.Pod) error {
	grace := int64(0)
	uid := pod.UID

	err := e.Runner.Kubernetes.CoreV1().Pods(e.Namespace).Delete(
		ctx,
		pod.Name,
		metav1.DeleteOptions{
			GracePeriodSeconds: &grace,
			Preconditions:      &metav1.Preconditions{UID: &uid},
		},
	)
	if apierrors.IsNotFound(err) {
		return nil
	}

	return err
}

func sortMeasurements(values []signalv1.MetricMeasurement) {
	sort.Slice(values, func(left, right int) bool {
		return values[left].Name < values[right].Name
	})
}

func boundedStageMessage(value string) string {
	if len(value) > 1024 {
		return value[:1024]
	}

	return value
}

func marshalStageJSON(value any) (string, error) {
	content, err := signalv1.MarshalStageExecutionInput(value)
	if err != nil {
		return "", err
	}

	return string(content), nil
}

func digestText(value string) string {
	digest := sha256.Sum256([]byte(value))

	return "sha256:" + hex.EncodeToString(digest[:])
}
