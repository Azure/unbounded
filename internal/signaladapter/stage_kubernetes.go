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
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilptr "k8s.io/utils/ptr"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

const (
	invocationResultPrefix            = "PROJECT_SIGNAL_INVOCATION_RESULT_V1 "
	maximumWorkflowArgumentParameters = 256
	argoWorkflowLabel                 = "workflows.argoproj.io/workflow"
	workflowRestartMetricName         = "restartSourceCounts"
	rawWorkflowResultArtifactName     = "rawWorkflowResult"
	maxInvocationResultArtifacts      = 16
)

type workflowRestartSourceCounts struct {
	Container   int32 `json:"container"`
	WorkflowPod int32 `json:"workflowPod"`
}

type KubernetesStageExecutor struct {
	Runner                 *Runner
	Namespace              string
	WorkflowName           string
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
		},
		Resources: driverResources(driver),
	}

	switch driver.Type {
	case signalv1.TestDriverTypeScript:
		if driver.Script == nil || driver.Script.Source.Type != signalv1.ArtifactSourceTypeConfigMap ||
			driver.Script.Source.ConfigMap == nil {
			return nil, errors.New("script runtime currently requires an immutable ConfigMap source")
		}

		source := driver.Script.Source.ConfigMap
		container.Command = []string{"/bin/sh", "-ceu"}
		container.Args = []string{scriptInvocationWrapper(driver.Script.Language, source.Key, source.Digest)}
		container.VolumeMounts = []corev1.VolumeMount{{
			Name:      "script",
			MountPath: "/scripts",
			ReadOnly:  true,
		}}
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
			Containers:                   []corev1.Container{container},
			Volumes:                      stageHostPathVolumes(invocation.Stage.Test.Security.HostPaths),
		},
	}

	if driver.Type == signalv1.TestDriverTypeScript {
		source := driver.Script.Source.ConfigMap
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name: "script",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: source.Name},
				},
			},
		})
	}

	return pod, nil
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

		for _, child := range typed {
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

	measurements, err := legacyMeasurements(legacy.Measurements)
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
		Participants: []signalv1.InvocationParticipantResult{{
			Index:   participant.Index,
			Role:    invocation.ParticipantRole[participant.Index],
			Outcome: outcome,
			Start:   signalv1.NodeFingerprint{NodeUID: participant.NodeUID},
			End:     signalv1.NodeFingerprint{NodeUID: participant.NodeUID},
		}},
		Activity: signalv1.WorkloadActivitySummary{
			SampleCount:          1,
			SampleSequenceDigest: digestText(legacy.Measurements),
		},
		Cleanup: signalv1.InvocationCleanupResult{Outcome: signalv1.CleanupOutcomeSucceeded},
	}, nil
}

func legacyMeasurements(content string) ([]signalv1.MetricMeasurement, error) {
	var values map[string]any
	if err := json.Unmarshal([]byte(content), &values); err != nil {
		return nil, fmt.Errorf("decode legacy measurements: %w", err)
	}

	result := make([]signalv1.MetricMeasurement, 0, len(values))
	for name, value := range values {
		normalized := strings.ToLower(name)
		if strings.Contains(normalized, "uuid") ||
			strings.Contains(normalized, "nodename") ||
			strings.Contains(normalized, "node_name") {
			continue
		}

		typed, ok := legacyTypedValue(value)
		if !ok {
			continue
		}

		result = append(result, signalv1.MetricMeasurement{Name: name, Value: typed})
	}

	sortMeasurements(result)

	return result, nil
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
	container.SecurityContext = &corev1.SecurityContext{
		Privileged: &security.Privileged,
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
script=/scripts/%s
want=%s
got=$(sha256sum "$script" | awk '{print $1}')
test "sha256:$got" = "$want"
exec %s "$script"
`, key, digest, interpreter)
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
	content, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode stage execution input: %w", err)
	}

	return string(content), nil
}

func digestText(value string) string {
	digest := sha256.Sum256([]byte(value))

	return "sha256:" + hex.EncodeToString(digest[:])
}
