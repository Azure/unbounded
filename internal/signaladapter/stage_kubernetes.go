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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

const invocationResultPrefix = "PROJECT_SIGNAL_INVOCATION_RESULT_V1 "

type KubernetesStageExecutor struct {
	Runner        *Runner
	Namespace     string
	WorkflowName  string
	ExecutionName string
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

func (e *KubernetesStageExecutor) CancelInvocation(
	ctx context.Context,
	invocation StageInvocation,
) error {
	propagation := metav1.DeletePropagationForeground
	grace := int64(0)

	var joined error

	for index := range invocation.Participants {
		name := invocationResourceName(e.ExecutionName, invocation, int32(index))

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

	for index := range invocation.Participants {
		pod, podErr := e.stagePod(workflow, invocation, int32(index))
		if podErr != nil {
			return signalv1.InvocationResultEnvelope{}, podErr
		}

		created, createErr := e.Runner.Kubernetes.CoreV1().Pods(e.Namespace).
			Create(ctx, pod, metav1.CreateOptions{})
		if createErr != nil {
			return signalv1.InvocationResultEnvelope{}, fmt.Errorf("create stage Pod: %w", createErr)
		}

		participantEnvelope, waitErr := e.waitStagePod(ctx, invocation, created)
		if waitErr != nil {
			return signalv1.InvocationResultEnvelope{}, waitErr
		}

		if len(participantEnvelope.Participants) != 1 {
			return signalv1.InvocationResultEnvelope{}, errors.New("participant Pod result must contain one participant")
		}

		participantResults = append(participantResults, participantEnvelope.Participants[0])
		for _, measurement := range participantEnvelope.Measurements {
			measurements[measurement.Name] = measurement
		}

		activitySamples += participantEnvelope.Activity.SampleCount
		if participantEnvelope.Outcome != signalv1.InvocationOutcomePassed {
			outcome = participantEnvelope.Outcome
			reason = participantEnvelope.ReasonCode
			message = participantEnvelope.Message
		}

		if deleteErr := e.deletePod(ctx, created); deleteErr != nil {
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

func (e *KubernetesStageExecutor) stagePod(
	workflow *unstructured.Unstructured,
	invocation StageInvocation,
	participantIndex int32,
) (*corev1.Pod, error) {
	participant := invocation.Participants[participantIndex]
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
		container.Args = []string{scriptInvocationWrapper(source.Key, source.Digest)}
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

	applyStageSecurity(&container, invocation.Stage.Test.Security)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: e.Namespace,
			Labels: map[string]string{
				runLabel: string(invocation.RunUID),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: workflow.GetAPIVersion(),
				Kind:       workflow.GetKind(),
				Name:       workflow.GetName(),
				UID:        workflow.GetUID(),
				Controller: &controller,
			}},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			NodeName:      participant.NodeName,
			HostNetwork:   invocation.Stage.Test.Security.HostNetwork,
			Containers:    []corev1.Container{container},
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
			logs, logErr := e.Runner.ReadPodLog(ctx, e.Namespace, pod.Name, "test", false)
			if logErr != nil {
				return signalv1.InvocationResultEnvelope{}, fmt.Errorf("read stage Pod result: %w", logErr)
			}

			envelope, parseErr := parseInvocationResultLog(logs, invocation)
			if parseErr != nil {
				return signalv1.InvocationResultEnvelope{}, parseErr
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
) (signalv1.InvocationResultEnvelope, error) {
	driver := invocation.Stage.Test.Driver.WorkflowTemplate
	if driver == nil {
		return signalv1.InvocationResultEnvelope{}, errors.New("WorkflowTemplate driver configuration is absent")
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

	templateSpec["activeDeadlineSeconds"] = int64(invocation.Stage.Schedule.InvocationTimeout.Seconds())
	templateSpec["arguments"] = map[string]any{"parameters": []any{
		map[string]any{"name": "invocation-id", "value": invocation.ID},
		map[string]any{"name": "run-uid", "value": string(invocation.RunUID)},
		map[string]any{"name": "plan-digest", "value": invocation.PlanDigest},
		map[string]any{"name": "stage-id", "value": invocation.Stage.Name},
		map[string]any{"name": "stage-order", "value": strconv.Itoa(int(invocation.StageOrder))},
		map[string]any{"name": "parameters-json", "value": parametersJSON},
		map[string]any{"name": "participants-json", "value": participantsJSON},
		map[string]any{"name": "participant-roles-json", "value": rolesJSON},
		map[string]any{"name": "resolved-inputs-json", "value": inputsJSON},
		map[string]any{"name": "result-configmap", "value": name + "-result"},
	}}

	workflow := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Workflow",
		"metadata": map[string]any{
			"name":      name,
			"namespace": e.Namespace,
			"labels": map[string]any{
				runLabel: string(invocation.RunUID),
			},
		},
		"spec": templateSpec,
	}}

	created, err := e.Runner.Dynamic.Resource(workflowGVR).Namespace(e.Namespace).
		Create(ctx, workflow, metav1.CreateOptions{})
	if err != nil {
		return signalv1.InvocationResultEnvelope{}, fmt.Errorf("create stage Workflow: %w", err)
	}

	resultName := name + "-result"

	ticker := time.NewTicker(e.Runner.pollInterval())
	defer ticker.Stop()

	for {
		result, resultErr := e.Runner.Kubernetes.CoreV1().ConfigMaps(e.Namespace).
			Get(ctx, resultName, metav1.GetOptions{})
		if resultErr == nil {
			content := result.BinaryData[resultDataKey]

			var envelope signalv1.InvocationResultEnvelope
			if err := json.Unmarshal(content, &envelope); err != nil {
				return signalv1.InvocationResultEnvelope{}, fmt.Errorf("decode WorkflowTemplate result: %w", err)
			}

			if err := e.deleteWorkflowInvocation(ctx, created, result); err != nil {
				envelope.Cleanup = signalv1.InvocationCleanupResult{
					Outcome: signalv1.CleanupOutcomeFailed,
					Message: boundedStageMessage(err.Error()),
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

func validateWorkflowTemplateIdentity(
	invocation StageInvocation,
	template *unstructured.Unstructured,
) error {
	driver := invocation.Stage.Test.Driver.WorkflowTemplate
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

	return errors.New("WorkflowTemplate stage has no complete immutable template input")
}

func (e *KubernetesStageExecutor) deleteWorkflowInvocation(
	ctx context.Context,
	workflow *unstructured.Unstructured,
	result *corev1.ConfigMap,
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

	resultUID := result.UID

	err = e.Runner.Kubernetes.CoreV1().ConfigMaps(e.Namespace).Delete(
		ctx,
		result.Name,
		metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &resultUID}},
	)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete WorkflowTemplate result: %w", err)
	}

	return nil
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
			Index:   0,
			Role:    invocation.ParticipantRole[0],
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

func scriptInvocationWrapper(key, digest string) string {
	return fmt.Sprintf(`
script=/scripts/%s
want=%s
got=$(sha256sum "$script" | awk '{print $1}')
test "sha256:$got" = "$want"
exec /bin/sh "$script"
`, key, digest)
}

func invocationResourceName(base string, invocation StageInvocation, participant int32) string {
	suffix := invocation.ID[len(invocation.ID)-8:]
	if participant >= 0 {
		return boundedDNSName(fmt.Sprintf("%s-%s-p%d", base, suffix, participant))
	}

	return boundedDNSName(fmt.Sprintf("%s-%s", base, suffix))
}

func boundedDNSName(value string) string {
	value = strings.ToLower(value)

	value = strings.Trim(value, "-.")
	if len(value) > 63 {
		value = strings.TrimRight(value[:63], "-.")
	}

	return value
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
