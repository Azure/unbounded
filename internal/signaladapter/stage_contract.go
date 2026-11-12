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
	"reflect"
	"sort"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

const (
	maximumStageResultBytes           = 256 * 1024
	maximumInvocationEvidenceBytes    = 768 * 1024
	stageResultDigestAnnotation       = "signal.unbounded-cloud.io/stage-result-digest"
	stageResultRunUIDAnnotation       = "signal.unbounded-cloud.io/stage-result-run-uid"
	stageResultPlanDigestAnnotation   = "signal.unbounded-cloud.io/stage-result-plan-digest"
	stageResultPhaseOrderAnnotation   = "signal.unbounded-cloud.io/stage-result-phase-order"
	stageResultStageOrderAnnotation   = "signal.unbounded-cloud.io/stage-result-stage-order"
	stageResultRunnerImagesAnnotation = "signal.unbounded-cloud.io/stage-result-runner-images"
	stageEvidenceRunUIDLabel          = "signal.unbounded-cloud.io/evidence-run-uid"
	stageEvidencePhaseOrderLabel      = "signal.unbounded-cloud.io/evidence-phase-order"
	stageEvidenceStageOrderLabel      = "signal.unbounded-cloud.io/evidence-stage-order"
)

type StageRequest struct {
	Namespace       string
	WorkflowName    string
	RunName         string
	ExecutionName   string
	RunUID          types.UID
	PhaseIndex      int
	StageIndex      int
	ResultConfigMap string
}

func (r *Runner) ResolveStageRequest(
	ctx context.Context,
	request StageRequest,
) (StageExecutionRequest, error) {
	if r.Dynamic == nil {
		return StageExecutionRequest{}, errors.New("dynamic Kubernetes client is required")
	}

	object, err := r.Dynamic.Resource(certificationRunGVR).Namespace(request.Namespace).
		Get(ctx, request.RunName, metav1.GetOptions{})
	if err != nil {
		return StageExecutionRequest{}, fmt.Errorf("get CertificationRun: %w", err)
	}

	var run signalv1.CertificationRun
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &run); err != nil {
		return StageExecutionRequest{}, fmt.Errorf("decode CertificationRun: %w", err)
	}

	if run.UID != request.RunUID || run.Spec.ResolvedPlan == nil ||
		request.PhaseIndex < 0 || request.PhaseIndex >= len(run.Spec.ResolvedPlan.Phases) {
		return StageExecutionRequest{}, errors.New("stage request does not match the immutable Run")
	}

	phase := &run.Spec.ResolvedPlan.Phases[request.PhaseIndex]
	if request.StageIndex < 0 || request.StageIndex >= len(phase.Stages) {
		return StageExecutionRequest{}, errors.New("stage index is outside the immutable Run plan")
	}

	stage := phase.Stages[request.StageIndex].DeepCopy()

	artifacts, err := r.resolvePinnedCertificationTest(ctx, stage)
	if err != nil {
		return StageExecutionRequest{}, err
	}

	duplicate, err := duplicateQualificationStageIdentity(
		run.Spec.ResolvedPlan,
		request.PhaseIndex,
		request.StageIndex,
	)
	if err != nil {
		return StageExecutionRequest{}, fmt.Errorf("derive stage qualification identity: %w", err)
	}

	resolved := StageExecutionRequest{
		RunUID:                         run.UID,
		PlanDigest:                     run.Spec.PlanDigest,
		PhaseOrder:                     int32(request.PhaseIndex),
		StageOrder:                     int32(request.StageIndex),
		PhaseRole:                      phase.Role,
		Stage:                          *stage,
		Artifacts:                      artifacts,
		Participants:                   indexedParticipants(run.Spec.Participants),
		DuplicateQualificationIdentity: duplicate,
	}

	resume, err := r.latestInvocationEvidence(ctx, request, &run)
	if err != nil {
		return StageExecutionRequest{}, err
	}

	resolved.Resume = resume

	return resolved, nil
}

func (r *Runner) resolvePinnedCertificationTest(
	ctx context.Context,
	stage *signalv1.ResolvedStagePlan,
) ([]signalv1.TestArtifact, error) {
	testGVR := schema.GroupVersionResource{
		Group:    signalv1.GroupVersion.Group,
		Version:  signalv1.GroupVersion.Version,
		Resource: "certificationtests",
	}

	object, err := r.Dynamic.Resource(testGVR).Get(ctx, stage.Test.Identity.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get pinned CertificationTest: %w", err)
	}

	var test signalv1.CertificationTest
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &test); err != nil {
		return nil, fmt.Errorf("decode pinned CertificationTest: %w", err)
	}

	identity := stage.Test.Identity
	if test.UID != identity.UID ||
		test.Generation != identity.Generation ||
		test.Status.ObservedGeneration != identity.Generation ||
		test.Status.Trust == nil ||
		test.Status.Trust.State != identity.Trust ||
		test.Status.Trust.ContentDigest != identity.ContentDigest {
		return nil, errors.New("CertificationTest no longer matches the immutable Run identity")
	}

	if err := signalv1.ValidateTestArtifacts(test.Spec); err != nil {
		return nil, fmt.Errorf("CertificationTest artifacts are invalid: %w", err)
	}

	parameterDigest, err := digestJSON(test.Spec.Parameters)
	if err != nil {
		return nil, fmt.Errorf("digest CertificationTest parameters: %w", err)
	}

	metricDigest, err := digestJSON(test.Spec.Metrics)
	if err != nil {
		return nil, fmt.Errorf("digest CertificationTest metrics: %w", err)
	}

	securityDigest, err := digestJSON(test.Spec.Security)
	if err != nil {
		return nil, fmt.Errorf("digest CertificationTest security: %w", err)
	}

	if parameterDigest != stage.Test.ParameterSchemaDigest ||
		metricDigest != stage.Test.MetricSchemaDigest ||
		securityDigest != stage.Test.SecurityRequirementsDigest ||
		!reflect.DeepEqual(test.Spec.Metrics, stage.Test.Metrics) ||
		!reflect.DeepEqual(test.Spec.Topology, stage.Test.Topology) ||
		!reflect.DeepEqual(test.Spec.Driver, stage.Test.Driver) {
		return nil, errors.New("CertificationTest contract no longer matches the immutable Run")
	}

	return append([]signalv1.TestArtifact(nil), test.Spec.Artifacts...), nil
}

func digestJSON(value any) (string, error) {
	content, err := json.Marshal(value)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(content)

	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func indexedParticipants(participants []signalv1.Participant) []signalv1.Participant {
	indexed := append([]signalv1.Participant(nil), participants...)
	for index := range indexed {
		indexed[index].Index = int32(index)
	}

	return indexed
}

func (r *Runner) latestInvocationEvidence(
	ctx context.Context,
	request StageRequest,
	run *signalv1.CertificationRun,
) ([]signalv1.InvocationResultEnvelope, error) {
	candidates := make(map[string]*corev1.ConfigMap)
	baseName := request.ResultConfigMap + "-evidence"

	base, err := r.Kubernetes.CoreV1().ConfigMaps(request.Namespace).
		Get(ctx, baseName, metav1.GetOptions{})
	if err == nil {
		candidates[base.Name] = base
	} else if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("read persisted invocation evidence: %w", err)
	}

	selector := fmt.Sprintf(
		"%s=%s,%s=%d,%s=%d",
		stageEvidenceRunUIDLabel,
		run.UID,
		stageEvidencePhaseOrderLabel,
		request.PhaseIndex,
		stageEvidenceStageOrderLabel,
		request.StageIndex,
	)

	list, err := r.Kubernetes.CoreV1().ConfigMaps(request.Namespace).
		List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list persisted invocation evidence: %w", err)
	}

	for index := range list.Items {
		candidate := &list.Items[index]
		candidates[candidate.Name] = candidate
	}

	versions := make([][]signalv1.InvocationResultEnvelope, 0, len(candidates))

	for _, candidate := range candidates {
		envelopes, err := decodeInvocationEvidence(candidate, request, run)
		if err != nil {
			return nil, err
		}

		versions = append(versions, envelopes)
	}

	sort.Slice(versions, func(left, right int) bool {
		return len(versions[left]) < len(versions[right])
	})

	for index := 1; index < len(versions); index++ {
		previous := versions[index-1]

		current := versions[index]
		if len(previous) == len(current) {
			if !reflect.DeepEqual(previous, current) {
				return nil, errors.New("persisted invocation evidence versions conflict")
			}

			continue
		}

		if !reflect.DeepEqual(previous, current[:len(previous)]) {
			return nil, errors.New("persisted invocation evidence is not append-only")
		}
	}

	if len(versions) == 0 {
		return nil, nil
	}

	return versions[len(versions)-1], nil
}

func decodeInvocationEvidence(
	evidence *corev1.ConfigMap,
	request StageRequest,
	run *signalv1.CertificationRun,
) ([]signalv1.InvocationResultEnvelope, error) {
	if evidence.Annotations[stageResultRunUIDAnnotation] != string(run.UID) ||
		evidence.Annotations[stageResultPlanDigestAnnotation] != run.Spec.PlanDigest ||
		evidence.Annotations[stageResultPhaseOrderAnnotation] != strconv.Itoa(request.PhaseIndex) ||
		evidence.Annotations[stageResultStageOrderAnnotation] != strconv.Itoa(request.StageIndex) {
		return nil, errors.New("persisted invocation evidence identity mismatch")
	}

	var envelopes []signalv1.InvocationResultEnvelope
	if err := json.Unmarshal(evidence.BinaryData[resultDataKey], &envelopes); err != nil {
		return nil, fmt.Errorf("decode persisted invocation evidence: %w", err)
	}

	return envelopes, nil
}

func (r *Runner) PersistStageResult(
	ctx context.Context,
	request StageRequest,
	execution StageExecutionRequest,
	summary signalv1.StageEvidenceSummary,
) error {
	if r.Dynamic == nil || r.Kubernetes == nil {
		return errors.New("kubernetes clients are required")
	}

	runObject, err := r.Dynamic.Resource(certificationRunGVR).Namespace(request.Namespace).
		Get(ctx, request.RunName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("re-read CertificationRun before stage result publication: %w", err)
	}

	var run signalv1.CertificationRun
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(runObject.Object, &run); err != nil {
		return fmt.Errorf("decode CertificationRun before stage result publication: %w", err)
	}

	if run.UID != request.RunUID ||
		run.Spec.PlanDigest != execution.PlanDigest ||
		run.Spec.ResolvedPlan == nil ||
		request.PhaseIndex < 0 ||
		request.PhaseIndex >= len(run.Spec.ResolvedPlan.Phases) ||
		request.StageIndex < 0 ||
		request.StageIndex >= len(run.Spec.ResolvedPlan.Phases[request.PhaseIndex].Stages) {
		return errors.New("CertificationRun stage identity changed before result publication")
	}

	planned := &run.Spec.ResolvedPlan.Phases[request.PhaseIndex].Stages[request.StageIndex]
	if summary.Name != planned.Name ||
		summary.PhaseOrder != int32(request.PhaseIndex) ||
		summary.StageOrder != int32(request.StageIndex) ||
		summary.Test != planned.Test.Identity {
		return errors.New("stage summary identity does not match the immutable Run")
	}

	content, err := json.Marshal(summary)
	if err != nil {
		return fmt.Errorf("encode stage result: %w", err)
	}

	if len(content) > maximumStageResultBytes {
		return errors.New("stage result exceeds the bounded ConfigMap payload")
	}

	workflow, err := r.Dynamic.Resource(workflowGVR).Namespace(request.Namespace).
		Get(ctx, request.WorkflowName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get owning Workflow: %w", err)
	}

	runnerImages, err := r.stageRunnerImages(ctx, request.Namespace, execution.Stage)
	if err != nil {
		return err
	}

	runnerImagesJSON, err := json.Marshal(runnerImages)
	if err != nil {
		return fmt.Errorf("encode runner image provenance: %w", err)
	}

	controller := true
	immutable := true
	digest := sha256.Sum256(content)
	result := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      request.ResultConfigMap,
			Namespace: request.Namespace,
			Labels: map[string]string{
				runLabel: request.RunName,
			},
			Annotations: map[string]string{
				stageResultDigestAnnotation:       "sha256:" + hex.EncodeToString(digest[:]),
				stageResultRunUIDAnnotation:       string(request.RunUID),
				stageResultPlanDigestAnnotation:   execution.PlanDigest,
				stageResultPhaseOrderAnnotation:   strconv.Itoa(request.PhaseIndex),
				stageResultStageOrderAnnotation:   strconv.Itoa(request.StageIndex),
				stageResultRunnerImagesAnnotation: string(runnerImagesJSON),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: workflow.GetAPIVersion(),
				Kind:       workflow.GetKind(),
				Name:       workflow.GetName(),
				UID:        workflow.GetUID(),
				Controller: &controller,
			}},
		},
		Immutable: &immutable,
		BinaryData: map[string][]byte{
			resultDataKey: content,
		},
	}

	if _, err := r.Kubernetes.CoreV1().ConfigMaps(request.Namespace).
		Create(ctx, result, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create stage result ConfigMap: %w", err)
	}

	return nil
}

func (r *Runner) PersistInvocationEvidence(
	ctx context.Context,
	request StageRequest,
	execution StageExecutionRequest,
	envelopes []signalv1.InvocationResultEnvelope,
) error {
	if r.Dynamic == nil || r.Kubernetes == nil {
		return errors.New("kubernetes clients are required")
	}

	content, err := json.Marshal(envelopes)
	if err != nil {
		return fmt.Errorf("encode invocation evidence: %w", err)
	}

	if len(content) > maximumInvocationEvidenceBytes {
		return errors.New("invocation evidence exceeds the bounded ConfigMap payload")
	}

	workflow, err := r.Dynamic.Resource(workflowGVR).Namespace(request.Namespace).
		Get(ctx, request.WorkflowName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get owning Workflow: %w", err)
	}

	controller := true
	immutable := true
	digest := sha256.Sum256(content)

	name := request.ResultConfigMap + "-evidence"
	if len(envelopes) > 1 {
		name += "-" + strconv.Itoa(len(envelopes))
	}

	result := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: request.Namespace,
			Labels: map[string]string{
				stageEvidenceRunUIDLabel:     string(execution.RunUID),
				stageEvidencePhaseOrderLabel: strconv.Itoa(int(execution.PhaseOrder)),
				stageEvidenceStageOrderLabel: strconv.Itoa(int(execution.StageOrder)),
			},
			Annotations: map[string]string{
				stageResultRunUIDAnnotation:     string(execution.RunUID),
				stageResultPlanDigestAnnotation: execution.PlanDigest,
				stageResultPhaseOrderAnnotation: strconv.Itoa(int(execution.PhaseOrder)),
				stageResultStageOrderAnnotation: strconv.Itoa(int(execution.StageOrder)),
				stageResultDigestAnnotation:     "sha256:" + hex.EncodeToString(digest[:]),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: workflow.GetAPIVersion(),
				Kind:       workflow.GetKind(),
				Name:       workflow.GetName(),
				UID:        workflow.GetUID(),
				Controller: &controller,
			}},
		},
		Immutable:  &immutable,
		BinaryData: map[string][]byte{resultDataKey: content},
	}

	_, err = r.Kubernetes.CoreV1().ConfigMaps(request.Namespace).Create(
		ctx,
		result,
		metav1.CreateOptions{},
	)
	if apierrors.IsAlreadyExists(err) {
		existing, getErr := r.Kubernetes.CoreV1().ConfigMaps(request.Namespace).
			Get(ctx, result.Name, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("read existing invocation evidence ConfigMap: %w", getErr)
		}

		if existing.Annotations[stageResultRunUIDAnnotation] == string(execution.RunUID) &&
			existing.Annotations[stageResultPlanDigestAnnotation] == execution.PlanDigest &&
			string(existing.BinaryData[resultDataKey]) == string(content) {
			return nil
		}

		return errors.New("existing invocation evidence ConfigMap does not match this execution")
	}

	if err != nil {
		return fmt.Errorf("create immutable invocation evidence ConfigMap: %w", err)
	}

	return nil
}

func duplicateQualificationStageIdentity(
	plan *signalv1.ResolvedCertificationPlan,
	phaseIndex int,
	stageIndex int,
) (bool, error) {
	stage := &plan.Phases[phaseIndex].Stages[stageIndex]
	if !stage.QualificationEligible {
		return false, nil
	}

	identity, err := driverIdentity(stage.Test.Driver)
	if err != nil {
		return false, err
	}

	matches := 0

	for phase := range plan.Phases {
		for candidate := range plan.Phases[phase].Stages {
			other := &plan.Phases[phase].Stages[candidate]

			otherIdentity, err := driverIdentity(other.Test.Driver)
			if err != nil {
				return false, err
			}

			if other.QualificationEligible &&
				other.Test.Identity == stage.Test.Identity &&
				otherIdentity == identity {
				matches++
			}
		}
	}

	return matches > 1, nil
}

func driverIdentity(driver signalv1.CertificationTestDriver) (string, error) {
	content, err := json.Marshal(driver)
	if err != nil {
		return "", fmt.Errorf("encode stage driver identity: %w", err)
	}

	digest := sha256.Sum256(content)

	return hex.EncodeToString(digest[:]), nil
}

func (r *Runner) stageRunnerImages(
	ctx context.Context,
	namespace string,
	stage signalv1.ResolvedStagePlan,
) ([]string, error) {
	images := make([]string, 0, 2)

	switch stage.Test.Driver.Type {
	case signalv1.TestDriverTypeScript:
		if stage.Test.Driver.Script != nil {
			images = append(images, stage.Test.Driver.Script.RunnerImage)
		}
	case signalv1.TestDriverTypeContainer:
		if stage.Test.Driver.Container != nil {
			images = append(images, stage.Test.Driver.Container.Image)
		}
	case signalv1.TestDriverTypeWorkflowTemplate:
		driver := stage.Test.Driver.WorkflowTemplate
		if driver == nil {
			return nil, errors.New("WorkflowTemplate driver configuration is absent")
		}

		templateGVR := schema.GroupVersionResource{
			Group:    "argoproj.io",
			Version:  "v1alpha1",
			Resource: "workflowtemplates",
		}

		template, err := r.Dynamic.Resource(templateGVR).Namespace(namespace).
			Get(ctx, driver.Name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get approved WorkflowTemplate provenance: %w", err)
		}

		if err := validateWorkflowTemplateIdentity(StageInvocation{Stage: stage}, template); err != nil {
			return nil, err
		}

		spec, found, err := unstructured.NestedFieldNoCopy(template.Object, "spec")
		if err != nil {
			return nil, fmt.Errorf("decode WorkflowTemplate provenance: %w", err)
		}

		if !found {
			return nil, errors.New("approved WorkflowTemplate has no spec")
		}

		images = workflowTemplateImages(spec)
		for _, image := range images {
			if !signalv1.IsDigestPinnedImage(image) {
				return nil, fmt.Errorf("WorkflowTemplate image %q is not digest pinned", image)
			}
		}
	}

	sort.Strings(images)

	return slicesCompact(images), nil
}

func workflowTemplateImages(value any) []string {
	images := make([]string, 0, 4)

	var walk func(any)

	walk = func(current any) {
		switch typed := current.(type) {
		case map[string]any:
			for key, child := range typed {
				if key == "image" {
					if image, ok := child.(string); ok {
						images = append(images, image)
					}

					continue
				}

				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}

	walk(value)

	return slicesCompact(images)
}

func slicesCompact(values []string) []string {
	if len(values) == 0 {
		return values
	}

	result := values[:1]
	for _, value := range values[1:] {
		if value != result[len(result)-1] {
			result = append(result, value)
		}
	}

	return result
}
