// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

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
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/recorder"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
)

const (
	RequestLabel         = "signal.unbounded-cloud.io/certification-request"
	ReservationLabel     = "signal.unbounded-cloud.io/reserved-by"
	RunNodeLabel         = "signal.unbounded-cloud.io/request-node"
	RunProfileLabel      = "signal.unbounded-cloud.io/profile"
	RunTriggerAnnotation = "signal.unbounded-cloud.io/trigger"
	RunFinalizer         = "signal.unbounded-cloud.io/cleanup"
	SnapshotRunLabel     = "signal.unbounded-cloud.io/run"
	DefaultMaxAttempts   = int32(1)

	ObservedGPUCapacityParameter    = "observed-gpu-capacity"
	ObservedGPUAllocatableParameter = "observed-gpu-allocatable"
)

var workflowGVK = schema.GroupVersionKind{
	Group:   "argoproj.io",
	Version: "v1alpha1",
	Kind:    "Workflow",
}

type AdmissionReconciler struct {
	client.Client
	Reader          client.Reader
	Namespace       string
	ScriptNamespace string
	Recorder        recorder.EventRecorder
	Now             func() time.Time
}

type RunReconciler struct {
	client.Client
	Reader             client.Reader
	Namespace          string
	Evidence           EvidenceStore
	Now                func() time.Time
	MaxConcurrentRuns  int
	WorkflowAPIVersion string
	Recorder           recorder.EventRecorder
}

func admissionReader(reader client.Reader, fallback client.Reader) client.Reader {
	if reader != nil {
		return reader
	}

	return fallback
}

func (r *AdmissionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Reader == nil {
		r.Reader = mgr.GetAPIReader()
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}, builder.WithPredicates(predicate.LabelChangedPredicate{})).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

func (r *RunReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Reader == nil {
		r.Reader = mgr.GetAPIReader()
	}

	workflow := &unstructured.Unstructured{}
	workflow.SetGroupVersionKind(workflowGVK)

	return ctrl.NewControllerManagedBy(mgr).
		For(&signalv1.CertificationRun{}).
		Watches(workflow, handler.EnqueueRequestForOwner(mgr.GetScheme(), mgr.GetRESTMapper(), &signalv1.CertificationRun{})).
		WithOptions(controller.Options{MaxConcurrentReconciles: max(1, r.MaxConcurrentRuns)}).
		Complete(r)
}

func (r *AdmissionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: req.Name}, &node); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	profileName := node.Labels[RequestLabel]
	if profileName == "" {
		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, r.admitNode(ctx, &node, profileName)
}

func (r *AdmissionReconciler) admitNode(ctx context.Context, node *corev1.Node, profileName string) error {
	return r.admitNodeWithTrigger(ctx, node, profileName, "manual")
}

func (r *AdmissionReconciler) admitNodeWithTrigger(
	ctx context.Context,
	node *corev1.Node,
	profileName string,
	trigger string,
) error {
	if runName := node.Annotations[ReservationLabel]; runName != "" {
		var run signalv1.CertificationRun

		err := r.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: runName}, &run)
		if err == nil {
			return nil
		}

		if !apierrors.IsNotFound(err) {
			return err
		}

		return fmt.Errorf("node %s has stale Project Signal reservation %q", node.Name, runName)
	}

	var profile signalv1.CertificationProfile
	if err := r.Get(ctx, client.ObjectKey{Name: profileName}, &profile); err != nil {
		return fmt.Errorf("get CertificationProfile %s: %w", profileName, err)
	}

	if err := validateStagePlan(profile.Spec.StagePlan); err != nil {
		return fmt.Errorf("profile %s stage plan: %w", profile.Name, err)
	}

	participants, err := r.selectParticipants(ctx, node, &profile)
	if err != nil {
		return err
	}

	runName := runName(node.Name, profile.Name, r.now())
	if err := r.reserveParticipants(ctx, participants, runName); err != nil {
		return err
	}

	releaseOnFailure := true
	defer func() {
		if releaseOnFailure {
			if err := r.releaseParticipants(context.WithoutCancel(ctx), participants, runName); err != nil {
				log.FromContext(ctx).Error(err, "failed to roll back participant reservations", "run", runName)
			}
		}
	}()

	snapshot, scriptDigest, err := r.createSnapshot(ctx, runName, &profile)
	if err != nil {
		return err
	}

	maxAttempts := profile.Spec.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = DefaultMaxAttempts
	}

	run := &signalv1.CertificationRun{
		TypeMeta: metav1.TypeMeta{APIVersion: signalv1.GroupVersion.String(), Kind: "CertificationRun"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      runName,
			Namespace: r.Namespace,
			Labels: map[string]string{
				RunNodeLabel:    node.Name,
				RunProfileLabel: profile.Name,
			},
			Annotations: map[string]string{
				RunTriggerAnnotation: trigger,
			},
			Finalizers: []string{RunFinalizer},
		},
		Spec: signalv1.CertificationRunSpec{
			Profile: signalv1.PinnedProfile{
				Name:       profile.Name,
				UID:        profile.UID,
				Generation: profile.Generation,
			},
			Participants:    participants,
			Script:          signalv1.ScriptSnapshot{ConfigMapName: snapshot.Name, Key: profile.Spec.Script.Key, Digest: scriptDigest},
			Workflow:        profile.Spec.Workflow,
			Backend:         profile.Spec.Backend,
			RunnerImage:     profile.Spec.RunnerImage,
			Purpose:         profile.Spec.Purpose,
			StagePlan:       append([]signalv1.CertificationStage(nil), profile.Spec.StagePlan...),
			Parameters:      snapshotParameters(profile.Spec.Parameters, node),
			EvidencePath:    strings.Trim(profile.Spec.EvidencePath, "/") + "/" + runName + ".json",
			Deadline:        profile.Spec.Deadline,
			MaxAttempts:     maxAttempts,
			Tolerations:     profile.Spec.Tolerations,
			RequestNodeName: node.Name,
		},
	}

	if err := r.Create(ctx, run); err != nil {
		deleteErr := r.Delete(context.WithoutCancel(ctx), snapshot)

		return errors.Join(fmt.Errorf("create CertificationRun: %w", err), client.IgnoreNotFound(deleteErr))
	}

	cleanupFailedAdmission := func(cause error) error {
		cleanupCtx := context.WithoutCancel(ctx)
		deleteRunErr := r.Delete(cleanupCtx, run, client.Preconditions{UID: &run.UID})

		var surviving signalv1.CertificationRun

		getRunErr := admissionReader(r.Reader, r.Client).Get(cleanupCtx, client.ObjectKeyFromObject(run), &surviving)
		if !apierrors.IsNotFound(getRunErr) {
			// A surviving finalizer Run still owns its inputs and cleanup barrier.
			releaseOnFailure = false

			var confirmErr error
			if getRunErr != nil {
				confirmErr = fmt.Errorf("confirm failed admission Run deletion: %w", getRunErr)
			}

			return errors.Join(cause, client.IgnoreNotFound(deleteRunErr), confirmErr)
		}

		deleteSnapshotErr := r.Delete(cleanupCtx, snapshot)

		return errors.Join(cause, client.IgnoreNotFound(deleteRunErr), client.IgnoreNotFound(deleteSnapshotErr))
	}

	snapshot.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(run, signalv1.GroupVersion.WithKind("CertificationRun"))}
	if err := r.Update(ctx, snapshot); err != nil {
		return cleanupFailedAdmission(fmt.Errorf("bind script snapshot to run: %w", err))
	}

	if err := projectPending(ctx, r.Client, participants, run.Spec.Profile, run.Spec.Purpose); err != nil {
		return cleanupFailedAdmission(fmt.Errorf("project pending certification state: %w", err))
	}

	releaseOnFailure = false

	if r.Recorder != nil {
		r.Recorder.Eventf(run, nil, corev1.EventTypeNormal, "CertificationAdmitted", "AdmitCertification",
			"Created CertificationRun %s with immutable script digest %s", run.Name, scriptDigest)
	}

	return nil
}

func snapshotParameters(parameters map[string]string, node *corev1.Node) map[string]string {
	snapshot := make(map[string]string, len(parameters)+2)
	for key, value := range parameters {
		snapshot[key] = value
	}

	gpu := corev1.ResourceName("nvidia.com/gpu")
	capacity := node.Status.Capacity[gpu]
	allocatable := node.Status.Allocatable[gpu]
	snapshot[ObservedGPUCapacityParameter] = capacity.String()
	snapshot[ObservedGPUAllocatableParameter] = allocatable.String()

	return snapshot
}

func (r *AdmissionReconciler) selectParticipants(
	ctx context.Context,
	requestNode *corev1.Node,
	profile *signalv1.CertificationProfile,
) ([]signalv1.Participant, error) {
	count := profile.Spec.Participants.Count
	if count < 1 {
		return nil, fmt.Errorf("profile %s participant count must be positive", profile.Name)
	}

	if count > 1 {
		return nil, fmt.Errorf("profile %s requests %d participants; multi-participant execution is not implemented", profile.Name, count)
	}

	selector, err := metav1.LabelSelectorAsSelector(&profile.Spec.Participants.Selector)
	if err != nil {
		return nil, fmt.Errorf("profile %s participant selector: %w", profile.Name, err)
	}

	if !selector.Matches(labels.Set(requestNode.Labels)) || !nodeEligible(requestNode, profile.Spec.Participants.RequiredResources) {
		return nil, fmt.Errorf("request node %s does not satisfy profile %s", requestNode.Name, profile.Name)
	}

	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return nil, fmt.Errorf("list participant nodes: %w", err)
	}

	sort.Slice(nodes.Items, func(i, j int) bool { return nodes.Items[i].Name < nodes.Items[j].Name })

	selected := []signalv1.Participant{{NodeName: requestNode.Name, NodeUID: requestNode.UID}}

	for i := range nodes.Items {
		candidate := &nodes.Items[i]

		if int32(len(selected)) == count {
			break
		}

		if candidate.Name == requestNode.Name ||
			!selector.Matches(labels.Set(candidate.Labels)) ||
			!nodeEligible(candidate, profile.Spec.Participants.RequiredResources) ||
			candidate.Annotations[ReservationLabel] != "" {
			continue
		}

		selected = append(selected, signalv1.Participant{NodeName: candidate.Name, NodeUID: candidate.UID})
	}

	if int32(len(selected)) != count {
		return nil, fmt.Errorf("profile %s requires %d eligible unlocked participants, found %d", profile.Name, count, len(selected))
	}

	return selected, nil
}

func nodeEligible(node *corev1.Node, required corev1.ResourceList) bool {
	if node.Spec.Unschedulable || !nodeReady(node) {
		return false
	}

	for name, quantity := range required {
		available, ok := node.Status.Allocatable[name]
		if !ok || available.Cmp(quantity) < 0 {
			return false
		}
	}

	return true
}

func nodeReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}

	return false
}

func (r *AdmissionReconciler) reserveParticipants(ctx context.Context, participants []signalv1.Participant, runName string) error {
	reserved := make([]signalv1.Participant, 0, len(participants))
	for _, participant := range participants {
		var node corev1.Node
		if err := admissionReader(r.Reader, r.Client).Get(ctx, client.ObjectKey{Name: participant.NodeName}, &node); err != nil {
			return errors.Join(
				fmt.Errorf("get participant %s: %w", participant.NodeName, err),
				r.releaseParticipants(context.WithoutCancel(ctx), reserved, runName),
			)
		}

		if node.UID != participant.NodeUID {
			return errors.Join(
				fmt.Errorf("participant %s identity changed", participant.NodeName),
				r.releaseParticipants(context.WithoutCancel(ctx), reserved, runName),
			)
		}

		if value := node.Annotations[ReservationLabel]; value != "" {
			return errors.Join(
				fmt.Errorf("participant %s is already reserved by %s", participant.NodeName, value),
				r.releaseParticipants(context.WithoutCancel(ctx), reserved, runName),
			)
		}

		before := node.DeepCopy()
		if node.Annotations == nil {
			node.Annotations = map[string]string{}
		}

		node.Annotations[ReservationLabel] = runName
		if err := r.Patch(
			ctx,
			&node,
			client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}),
		); err != nil {
			return errors.Join(
				fmt.Errorf("reserve participant %s: %w", participant.NodeName, err),
				r.releaseParticipants(context.WithoutCancel(ctx), reserved, runName),
			)
		}

		reserved = append(reserved, participant)
	}

	return nil
}

func (r *AdmissionReconciler) releaseParticipants(ctx context.Context, participants []signalv1.Participant, runName string) error {
	var errs []error

	for _, participant := range participants {
		var node corev1.Node
		if err := admissionReader(r.Reader, r.Client).Get(ctx, client.ObjectKey{Name: participant.NodeName}, &node); err != nil {
			if !apierrors.IsNotFound(err) {
				errs = append(errs, err)
			}

			continue
		}

		if participant.NodeUID != "" && node.UID != participant.NodeUID {
			continue
		}

		if node.Annotations[ReservationLabel] != runName {
			continue
		}

		before := node.DeepCopy()
		delete(node.Annotations, ReservationLabel)

		if err := r.Patch(ctx, &node, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func (r *AdmissionReconciler) createSnapshot(
	ctx context.Context,
	runName string,
	profile *signalv1.CertificationProfile,
) (*corev1.ConfigMap, string, error) {
	var source corev1.ConfigMap
	if err := r.Get(ctx, client.ObjectKey{Namespace: r.ScriptNamespace, Name: profile.Spec.Script.Name}, &source); err != nil {
		return nil, "", fmt.Errorf("get source script ConfigMap: %w", err)
	}

	content, ok := source.Data[profile.Spec.Script.Key]
	if !ok {
		return nil, "", fmt.Errorf("source script ConfigMap %s has no key %s", source.Name, profile.Spec.Script.Key)
	}

	digest := digestString(content)
	immutable := true

	snapshot := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      runName + "-scripts",
			Namespace: r.Namespace,
			Labels:    map[string]string{SnapshotRunLabel: runName},
		},
		Immutable: &immutable,
		Data: map[string]string{
			profile.Spec.Script.Key: content,
			"sha256":                digest,
		},
	}
	if err := r.Create(ctx, snapshot); err != nil {
		return nil, "", fmt.Errorf("create immutable script snapshot: %w", err)
	}

	return snapshot, digest, nil
}

func (r *RunReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var run signalv1.CertificationRun
	if err := r.Get(ctx, req.NamespacedName, &run); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !run.Status.Phase.Terminal() || run.Status.Workflow.Name == "" {
		if err := r.recoverWorkflow(ctx, &run); err != nil {
			return ctrl.Result{}, err
		}
	}

	if !run.DeletionTimestamp.IsZero() {
		return r.reconcileDeleting(ctx, &run)
	}

	if run.Status.Phase.Terminal() {
		return r.cleanup(ctx, &run)
	}

	if run.Spec.Cancel {
		return r.cancel(ctx, &run)
	}

	if run.Status.Workflow.Name == "" {
		return r.startAttempt(ctx, &run, 1)
	}

	workflow := &unstructured.Unstructured{}
	workflow.SetGroupVersionKind(workflowGVK)

	if err := r.Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: run.Status.Workflow.Name}, workflow); err != nil {
		if apierrors.IsNotFound(err) {
			return r.retryOrFinish(ctx, &run, "Error", "WorkflowMissing", "workflow disappeared before terminal status")
		}

		return ctrl.Result{}, err
	}

	phase, _, err := unstructured.NestedString(workflow.Object, "status", "phase")
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("read Workflow phase: %w", err)
	}

	switch phase {
	case "", "Pending", "Running":
		if err := projectTesting(ctx, r.Client, &run); err != nil {
			return ctrl.Result{}, err
		}

		if run.Status.Phase != signalv1.RunPhaseRunning || run.Status.Workflow.Phase != phase {
			before := run.DeepCopy()
			run.Status.Phase = signalv1.RunPhaseRunning

			run.Status.Workflow.Phase = phase
			if run.Status.StartedAt == nil {
				now := metav1.NewTime(r.now())
				run.Status.StartedAt = &now
			}

			return ctrl.Result{}, r.Status().Patch(ctx, &run, client.MergeFrom(before))
		}

		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	case "Succeeded":
		verdict := workflowParameter(workflow, "verdict")
		reason := workflowParameter(workflow, "reason")

		if verdict == "" {
			verdict = "Inconclusive"
			reason = "workflow completed without a verdict"
		}

		return r.finish(ctx, &run, "Completed", verdict, reason)
	case "Failed", "Error":
		return r.retryOrFinish(ctx, &run, phase, "Workflow"+phase, workflowMessage(workflow))
	default:
		return ctrl.Result{}, fmt.Errorf("unsupported Workflow phase %q", phase)
	}
}

func (r *RunReconciler) startAttempt(ctx context.Context, run *signalv1.CertificationRun, attempt int32) (ctrl.Result, error) {
	workflow, err := buildWorkflow(run, attempt)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.Create(ctx, workflow); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, fmt.Errorf("create Workflow attempt %d: %w", attempt, err)
		}

		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(workflowGVK)

		if getErr := r.Get(ctx, client.ObjectKeyFromObject(workflow), existing); getErr != nil {
			return ctrl.Result{}, fmt.Errorf("get existing Workflow attempt %d: %w", attempt, getErr)
		}

		if !metav1.IsControlledBy(existing, run) {
			return ctrl.Result{}, fmt.Errorf("workflow attempt %d already exists without matching CertificationRun ownership", attempt)
		}

		workflow = existing
	}

	before := run.DeepCopy()
	run.Status.Phase = signalv1.RunPhaseRunning
	run.Status.ExecutionState = "Running"

	run.Status.Workflow = signalv1.WorkflowStatus{
		Name:    workflow.GetName(),
		UID:     string(workflow.GetUID()),
		Attempt: attempt,
		Phase:   "Pending",
	}
	if run.Status.StartedAt == nil {
		now := metav1.NewTime(r.now())
		run.Status.StartedAt = &now
	}

	if err := r.Status().Patch(ctx, run, client.MergeFrom(before)); err != nil {
		return ctrl.Result{}, fmt.Errorf("record Workflow attempt: %w", err)
	}

	if r.Recorder != nil {
		r.Recorder.Eventf(run, nil, corev1.EventTypeNormal, "WorkflowStarted", "StartWorkflow",
			"Created Workflow %s for attempt %d", workflow.GetName(), attempt)
	}

	return ctrl.Result{}, nil
}

func buildWorkflow(run *signalv1.CertificationRun, attempt int32) (*unstructured.Unstructured, error) {
	participantNames := make([]string, 0, len(run.Spec.Participants))
	for _, participant := range run.Spec.Participants {
		participantNames = append(participantNames, participant.NodeName)
	}

	tolerations := uniqueTolerations(append([]corev1.Toleration{{
		Key:      "nvidia.com/gpu",
		Operator: corev1.TolerationOpExists,
		Effect:   corev1.TaintEffectNoSchedule,
	}}, run.Spec.Tolerations...))

	tolerationsJSON, err := json.Marshal(tolerations)
	if err != nil {
		return nil, fmt.Errorf("encode Workflow tolerations: %w", err)
	}

	name := fmt.Sprintf("%s-a%d", run.Name, attempt)
	workflowParameters := []any{
		map[string]any{"name": "run-name", "value": run.Name},
		map[string]any{"name": "run-uid", "value": string(run.UID)},
		map[string]any{"name": "participants", "value": strings.Join(participantNames, ",")},
		map[string]any{"name": "script-configmap", "value": run.Spec.Script.ConfigMapName},
		map[string]any{"name": "script-key", "value": run.Spec.Script.Key},
		map[string]any{"name": "script-digest", "value": run.Spec.Script.Digest},
		map[string]any{"name": "backend-api-version", "value": run.Spec.Backend.APIVersion},
		map[string]any{"name": "backend-kind", "value": run.Spec.Backend.Kind},
		map[string]any{"name": "queue", "value": run.Spec.Backend.Queue},
		map[string]any{"name": "runner-image", "value": run.Spec.RunnerImage},
		map[string]any{"name": "tolerations", "value": string(tolerationsJSON)},
		map[string]any{"name": "attempt", "value": fmt.Sprintf("%d", attempt)},
	}
	reservedParameters := map[string]struct{}{
		"run-name":            {},
		"run-uid":             {},
		"participants":        {},
		"script-configmap":    {},
		"script-key":          {},
		"script-digest":       {},
		"backend-api-version": {},
		"backend-kind":        {},
		"queue":               {},
		"runner-image":        {},
		"tolerations":         {},
		"attempt":             {},
	}

	parameterNames := make([]string, 0, len(run.Spec.Parameters))
	for parameterName := range run.Spec.Parameters {
		parameterNames = append(parameterNames, parameterName)
	}

	sort.Strings(parameterNames)

	for _, parameterName := range parameterNames {
		if _, reserved := reservedParameters[parameterName]; reserved {
			continue
		}

		workflowParameters = append(workflowParameters, map[string]any{
			"name":  parameterName,
			"value": run.Spec.Parameters[parameterName],
		})
	}

	workflow := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": workflowGVK.GroupVersion().String(),
		"kind":       workflowGVK.Kind,
		"metadata": map[string]any{
			"name":      name,
			"namespace": run.Namespace,
			"labels": map[string]any{
				SnapshotRunLabel: run.Name,
			},
		},
		"spec": map[string]any{
			"activeDeadlineSeconds": int64(run.Spec.Deadline.Seconds()),
			"ttlStrategy": map[string]any{
				"secondsAfterCompletion": int64(6 * time.Hour / time.Second),
				"secondsAfterSuccess":    int64(6 * time.Hour / time.Second),
				"secondsAfterFailure":    int64(6 * time.Hour / time.Second),
			},
			"podGC": map[string]any{
				"strategy":            "OnWorkflowCompletion",
				"deleteDelayDuration": "2h",
			},
			"workflowTemplateRef": map[string]any{
				"name": run.Spec.Workflow.Name,
			},
			"arguments": map[string]any{
				"parameters": workflowParameters,
			},
		},
	}}
	workflow.SetOwnerReferences([]metav1.OwnerReference{
		*metav1.NewControllerRef(run, signalv1.GroupVersion.WithKind("CertificationRun")),
	})

	return workflow, nil
}

func (r *RunReconciler) retryOrFinish(
	ctx context.Context,
	run *signalv1.CertificationRun,
	executionState string,
	reason string,
	message string,
) (ctrl.Result, error) {
	if run.Status.Workflow.Attempt < run.Spec.MaxAttempts {
		return r.startAttempt(ctx, run, run.Status.Workflow.Attempt+1)
	}

	return r.finish(ctx, run, executionState, "Inconclusive", reason+": "+message)
}

func (r *RunReconciler) finish(
	ctx context.Context,
	run *signalv1.CertificationRun,
	executionState string,
	verdict string,
	reason string,
) (ctrl.Result, error) {
	if r.Evidence == nil {
		return ctrl.Result{}, fmt.Errorf("evidence store is required before completing a run")
	}

	evidenceRun := run.DeepCopy()
	if executionState == "Completed" {
		evidenceRun.Status.Workflow.Phase = "Succeeded"
	} else {
		evidenceRun.Status.Workflow.Phase = executionState
	}

	evidenceStatus, finishedAt, err := r.recoverAggregateEvidence(ctx, evidenceRun, executionState, verdict, reason)
	if err != nil {
		return ctrl.Result{}, err
	}

	if evidenceStatus.URI == "" {
		now := metav1.NewTime(r.now())
		finishedAt = &now
		evidenceRun.Status.FinishedAt = finishedAt

		evidence, err := r.evidenceDocument(ctx, evidenceRun, executionState, verdict, reason)
		if err != nil {
			return ctrl.Result{}, err
		}

		uri, digest, err := r.Evidence.Put(ctx, run.Spec.EvidencePath, evidence)
		if err != nil {
			return ctrl.Result{}, err
		}

		evidenceStatus = signalv1.EvidenceStatus{URI: uri, Digest: digest}
	}

	before := run.DeepCopy()
	run.Status.ExecutionState = executionState
	run.Status.Verdict = verdict
	run.Status.Reason = reason

	run.Status.Evidence = evidenceStatus
	if executionState == "Completed" {
		run.Status.Workflow.Phase = "Succeeded"
	} else {
		run.Status.Workflow.Phase = executionState
	}

	run.Status.FinishedAt = finishedAt

	switch verdict {
	case "Pass":
		run.Status.Phase = signalv1.RunPhaseSucceeded
	case "Fail", "Suspect":
		run.Status.Phase = signalv1.RunPhaseFailed
	default:
		run.Status.Phase = signalv1.RunPhaseInconclusive
	}

	if err := r.Status().Patch(ctx, run, client.MergeFrom(before)); err != nil {
		return ctrl.Result{}, err
	}

	if err := projectTerminal(ctx, r.Client, run); err != nil {
		return ctrl.Result{}, err
	}

	if r.Recorder != nil {
		r.Recorder.Eventf(run, nil, corev1.EventTypeNormal, "CertificationFinished", "FinishCertification",
			"ExecutionState=%s Verdict=%s Reason=%s", executionState, verdict, reason)
	}

	return ctrl.Result{RequeueAfter: time.Millisecond}, nil
}

func uniqueTolerations(tolerations []corev1.Toleration) []corev1.Toleration {
	unique := make([]corev1.Toleration, 0, len(tolerations))
	for _, toleration := range tolerations {
		duplicate := false

		for _, existing := range unique {
			if reflect.DeepEqual(existing, toleration) {
				duplicate = true

				break
			}
		}

		if !duplicate {
			unique = append(unique, toleration)
		}
	}

	return unique
}

func (r *RunReconciler) evidenceDocument(
	ctx context.Context,
	run *signalv1.CertificationRun,
	executionState string,
	verdict string,
	reason string,
) ([]byte, error) {
	var snapshot corev1.ConfigMap

	reader := admissionReader(r.Reader, r.Client)
	if err := reader.Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: run.Spec.Script.ConfigMapName}, &snapshot); err != nil {
		return nil, fmt.Errorf("read script snapshot for evidence: %w", err)
	}

	doc := map[string]any{
		"apiVersion":     "signal.unbounded-cloud.io/evidence/v1alpha1",
		"runID":          string(run.UID),
		"runName":        run.Name,
		"runSpec":        run.Spec,
		"profile":        run.Spec.Profile,
		"participants":   run.Spec.Participants,
		"workflow":       run.Status.Workflow,
		"backend":        run.Spec.Backend,
		"runnerImage":    run.Spec.RunnerImage,
		"purpose":        run.Spec.Purpose,
		"stagePlan":      run.Spec.StagePlan,
		"parameters":     run.Spec.Parameters,
		"scriptSnapshot": run.Spec.Script,
		"scriptContent":  snapshot.Data[run.Spec.Script.Key],
		"executionState": executionState,
		"verdict":        verdict,
		"reason":         reason,
		"startedAt":      run.Status.StartedAt,
		"finishedAt":     r.now().UTC().Format(time.RFC3339Nano),
	}
	if run.Status.FinishedAt != nil {
		doc["finishedAt"] = run.Status.FinishedAt.UTC().Format(time.RFC3339Nano)
	}

	if run.Status.Workflow.Name != "" {
		workflow := &unstructured.Unstructured{}
		workflow.SetGroupVersionKind(workflowGVK)

		if err := reader.Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: run.Status.Workflow.Name}, workflow); err != nil {
			if !apierrors.IsNotFound(err) {
				return nil, fmt.Errorf("read Workflow for evidence: %w", err)
			}
		} else {
			if !metav1.IsControlledBy(workflow, run) ||
				run.Status.Workflow.UID != "" && string(workflow.GetUID()) != run.Status.Workflow.UID {
				return nil, fmt.Errorf("refuse to preserve Workflow without matching CertificationRun ownership and UID")
			}

			nodes, _, err := unstructured.NestedMap(workflow.Object, "status", "nodes")
			if err != nil {
				doc["workflowNodesError"] = err.Error()
			}

			doc["workflowNodes"] = nodes
			doc["workflowNodeStatus"] = workflow.Object["status"]
		}
	}

	return json.MarshalIndent(doc, "", "  ")
}

func (r *RunReconciler) recoverAggregateEvidence(
	ctx context.Context,
	run *signalv1.CertificationRun,
	executionState string,
	verdict string,
	reason string,
) (signalv1.EvidenceStatus, *metav1.Time, error) {
	reader, ok := r.Evidence.(EvidenceReader)
	if !ok {
		return signalv1.EvidenceStatus{}, nil, nil
	}

	data, uri, digest, err := reader.Get(ctx, run.Spec.EvidencePath)
	if apierrors.IsNotFound(err) {
		return signalv1.EvidenceStatus{}, nil, nil
	}

	if err != nil {
		return signalv1.EvidenceStatus{}, nil, fmt.Errorf("read existing run evidence: %w", err)
	}

	var checkpoint struct {
		APIVersion     string                         `json:"apiVersion"`
		RunID          string                         `json:"runID"`
		RunName        string                         `json:"runName"`
		RunSpec        *signalv1.CertificationRunSpec `json:"runSpec"`
		Workflow       signalv1.WorkflowStatus        `json:"workflow"`
		ExecutionState string                         `json:"executionState"`
		Verdict        string                         `json:"verdict"`
		Reason         string                         `json:"reason"`
		FinishedAt     string                         `json:"finishedAt"`
	}
	if err := json.Unmarshal(data, &checkpoint); err != nil {
		return signalv1.EvidenceStatus{}, nil, fmt.Errorf("decode existing run evidence: %w", err)
	}

	if checkpoint.RunSpec == nil {
		return signalv1.EvidenceStatus{}, nil, fmt.Errorf("existing evidence is missing the immutable run spec")
	}

	currentSpec := run.Spec
	// Cancellation is mutable; the admitted execution inputs are not.
	currentSpec.Cancel = false
	checkpoint.RunSpec.Cancel = false

	expectedSpec, err := json.Marshal(currentSpec)
	if err != nil {
		return signalv1.EvidenceStatus{}, nil, fmt.Errorf("encode current immutable run spec: %w", err)
	}

	recordedSpec, err := json.Marshal(checkpoint.RunSpec)
	if err != nil {
		return signalv1.EvidenceStatus{}, nil, fmt.Errorf("encode recorded immutable run spec: %w", err)
	}

	reasonMatches := checkpoint.Reason == reason || executionState == "Interrupted" &&
		(checkpoint.Reason == "canceled by operator" || checkpoint.Reason == "run deleted by operator")
	// Workflow status may advance after immutable evidence is written.
	if checkpoint.APIVersion != "signal.unbounded-cloud.io/evidence/v1alpha1" ||
		checkpoint.RunID != string(run.UID) || checkpoint.RunName != run.Name ||
		string(recordedSpec) != string(expectedSpec) ||
		checkpoint.Workflow.Name != run.Status.Workflow.Name || checkpoint.Workflow.UID != run.Status.Workflow.UID ||
		checkpoint.Workflow.Attempt != run.Status.Workflow.Attempt ||
		checkpoint.ExecutionState != executionState || checkpoint.Verdict != verdict || !reasonMatches {
		return signalv1.EvidenceStatus{}, nil, fmt.Errorf("existing evidence does not match the immutable run and Workflow outcome")
	}

	finishedAt, err := time.Parse(time.RFC3339Nano, checkpoint.FinishedAt)
	if err != nil {
		return signalv1.EvidenceStatus{}, nil, fmt.Errorf("decode recorded evidence completion time: %w", err)
	}

	finished := metav1.NewTime(finishedAt)

	return signalv1.EvidenceStatus{URI: uri, Digest: digest}, &finished, nil
}

func (r *RunReconciler) preserveInterrupted(ctx context.Context, run *signalv1.CertificationRun) error {
	if run.Status.Evidence.URI != "" {
		return nil
	}

	if r.Evidence == nil {
		return fmt.Errorf("evidence store is required before interrupting a run")
	}

	reason := "canceled by operator"
	if !run.DeletionTimestamp.IsZero() {
		reason = "run deleted by operator"
	}

	evidence, finishedAt, err := r.recoverAggregateEvidence(ctx, run, "Interrupted", "Inconclusive", reason)
	if err != nil {
		return err
	}

	if evidence.URI != "" {
		before := run.DeepCopy()
		run.Status.Evidence = evidence
		run.Status.FinishedAt = finishedAt

		return r.Status().Patch(ctx, run, client.MergeFrom(before))
	}

	if run.Status.FinishedAt == nil {
		before := run.DeepCopy()
		now := metav1.NewTime(r.now())
		run.Status.FinishedAt = &now

		if err := r.Status().Patch(ctx, run, client.MergeFrom(before)); err != nil {
			return fmt.Errorf("record interrupted outcome before evidence: %w", err)
		}
	}

	data, err := r.evidenceDocument(ctx, run, "Interrupted", "Inconclusive", reason)
	if err != nil {
		return err
	}

	uri, digest, err := r.Evidence.Put(ctx, run.Spec.EvidencePath, data)
	if err != nil {
		return fmt.Errorf("preserve interrupted evidence: %w", err)
	}

	before := run.DeepCopy()
	run.Status.Evidence = signalv1.EvidenceStatus{URI: uri, Digest: digest}

	return r.Status().Patch(ctx, run, client.MergeFrom(before))
}

func (r *RunReconciler) cancel(ctx context.Context, run *signalv1.CertificationRun) (ctrl.Result, error) {
	if err := r.preserveInterrupted(ctx, run); err != nil {
		return ctrl.Result{}, err
	}

	stopped, err := r.ensureWorkflowStopped(ctx, run)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !stopped {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	before := run.DeepCopy()
	run.Status.Phase = signalv1.RunPhaseCancelled
	run.Status.ExecutionState = "Interrupted"
	run.Status.Verdict = "Inconclusive"

	run.Status.Reason = "canceled by operator"
	if err := r.Status().Patch(ctx, run, client.MergeFrom(before)); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: time.Millisecond}, nil
}

func (r *RunReconciler) cleanup(ctx context.Context, run *signalv1.CertificationRun) (ctrl.Result, error) {
	if run.Status.Phase == signalv1.RunPhaseCancelled || run.Status.ExecutionState == "Interrupted" {
		stopped, err := r.ensureWorkflowStopped(ctx, run)
		if err != nil {
			return ctrl.Result{}, err
		}

		if !stopped {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
	}

	if err := projectTerminal(ctx, r.Client, run); err != nil {
		return ctrl.Result{}, err
	}

	released, err := r.releaseRunReservations(ctx, run)
	if err != nil {
		return ctrl.Result{}, err
	}

	deleted := false

	snapshot := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: run.Namespace, Name: run.Spec.Script.ConfigMapName}}
	if err := r.Delete(ctx, snapshot); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	deleted = true

	if err := r.clearRequest(ctx, run); err != nil {
		return ctrl.Result{}, err
	}

	statusBefore := run.DeepCopy()

	run.Status.Cleanup = signalv1.CleanupStatus{ReservationsReleased: released, SnapshotDeleted: deleted}
	if err := r.Status().Patch(ctx, run, client.MergeFrom(statusBefore)); err != nil {
		return ctrl.Result{}, err
	}

	before := run.DeepCopy()

	run.Finalizers = removeString(run.Finalizers, RunFinalizer)
	if err := r.Patch(ctx, run, client.MergeFrom(before)); err != nil {
		return ctrl.Result{}, err
	}

	if r.Recorder != nil {
		r.Recorder.Eventf(run, nil, corev1.EventTypeNormal, "CertificationCleaned", "CleanCertification",
			"Released participant reservations, cleared the request label, and deleted the script snapshot")
	}

	return ctrl.Result{}, nil
}

func (r *RunReconciler) reconcileDeleting(ctx context.Context, run *signalv1.CertificationRun) (ctrl.Result, error) {
	if !containsString(run.Finalizers, RunFinalizer) {
		return ctrl.Result{}, nil
	}

	if run.Status.Workflow.Name != "" && run.Status.Evidence.URI == "" {
		if err := r.preserveInterrupted(ctx, run); err != nil {
			return ctrl.Result{}, err
		}
	}

	stopped, err := r.ensureWorkflowStopped(ctx, run)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !stopped {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	if _, err := r.releaseRunReservations(ctx, run); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.clearRequest(ctx, run); err != nil {
		return ctrl.Result{}, err
	}

	snapshot := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: run.Namespace, Name: run.Spec.Script.ConfigMapName}}
	if err := r.Delete(ctx, snapshot); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	before := run.DeepCopy()
	run.Finalizers = removeString(run.Finalizers, RunFinalizer)

	return ctrl.Result{}, r.Patch(ctx, run, client.MergeFrom(before))
}

func (r *RunReconciler) releaseRunReservations(ctx context.Context, run *signalv1.CertificationRun) (bool, error) {
	admission := &AdmissionReconciler{Client: r.Client, Reader: r.Reader}
	if err := admission.releaseParticipants(ctx, run.Spec.Participants, run.Name); err != nil {
		return false, err
	}

	return true, nil
}

func (r *RunReconciler) clearRequest(ctx context.Context, run *signalv1.CertificationRun) error {
	var node corev1.Node
	if err := admissionReader(r.Reader, r.Client).Get(ctx, client.ObjectKey{Name: run.Spec.RequestNodeName}, &node); err != nil {
		return client.IgnoreNotFound(err)
	}

	if requestNodeUID := participantNodeUID(run, run.Spec.RequestNodeName); requestNodeUID != "" && node.UID != requestNodeUID {
		return nil
	}

	if owner := node.Annotations[ReservationLabel]; owner != "" && owner != run.Name {
		return nil
	}

	if node.Labels[RequestLabel] != run.Spec.Profile.Name {
		return nil
	}

	before := node.DeepCopy()
	delete(node.Labels, RequestLabel)

	return r.Patch(ctx, &node, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

func (r *RunReconciler) ensureWorkflowStopped(ctx context.Context, run *signalv1.CertificationRun) (bool, error) {
	if run.Status.Workflow.Name == "" {
		if err := r.recoverWorkflow(ctx, run); err != nil {
			return false, err
		}

		if run.Status.Workflow.Name == "" {
			return true, nil
		}
	}

	workflow := &unstructured.Unstructured{}
	workflow.SetGroupVersionKind(workflowGVK)
	workflow.SetNamespace(run.Namespace)
	workflow.SetName(run.Status.Workflow.Name)

	reader := admissionReader(r.Reader, r.Client)
	if err := reader.Get(ctx, client.ObjectKeyFromObject(workflow), workflow); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}

		return false, fmt.Errorf("read Workflow before stopping it: %w", err)
	}

	if !metav1.IsControlledBy(workflow, run) ||
		run.Status.Workflow.UID != "" && string(workflow.GetUID()) != run.Status.Workflow.UID {
		return false, fmt.Errorf("refuse to delete Workflow without matching CertificationRun ownership and UID")
	}

	propagation := metav1.DeletePropagationForeground

	uid := workflow.GetUID()
	if err := r.Delete(ctx, workflow, client.PropagationPolicy(propagation), client.Preconditions{UID: &uid}); err != nil &&
		!apierrors.IsNotFound(err) {
		return false, fmt.Errorf("delete Workflow before releasing participants: %w", err)
	}

	if err := reader.Get(ctx, client.ObjectKeyFromObject(workflow), workflow); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}

		return false, fmt.Errorf("confirm Workflow deletion: %w", err)
	}

	return false, nil
}

func (r *RunReconciler) recoverWorkflow(ctx context.Context, run *signalv1.CertificationRun) error {
	workflows := &unstructured.UnstructuredList{}
	workflows.SetGroupVersionKind(workflowGVK.GroupVersion().WithKind("WorkflowList"))

	if err := admissionReader(r.Reader, r.Client).List(ctx, workflows, client.InNamespace(run.Namespace)); err != nil {
		return fmt.Errorf("recover Workflow identity: %w", err)
	}

	latest := run.Status.Workflow

	for index := range workflows.Items {
		workflow := &workflows.Items[index]
		if !metav1.IsControlledBy(workflow, run) {
			continue
		}

		if workflow.GetName() == run.Status.Workflow.Name && run.Status.Workflow.UID != "" &&
			string(workflow.GetUID()) != run.Status.Workflow.UID {
			return fmt.Errorf("workflow attempt %d UID changed", run.Status.Workflow.Attempt)
		}

		suffix, found := strings.CutPrefix(workflow.GetName(), run.Name+"-a")
		if !found {
			if workflow.GetName() == run.Status.Workflow.Name {
				continue
			}

			return fmt.Errorf("owned workflow %s has an unrecognized attempt identity", workflow.GetName())
		}

		attempt, err := strconv.ParseInt(suffix, 10, 32)
		if err != nil || attempt < 1 || attempt > int64(run.Spec.MaxAttempts) {
			if workflow.GetName() == run.Status.Workflow.Name {
				continue
			}

			return fmt.Errorf("owned workflow %s has an invalid attempt identity", workflow.GetName())
		}

		if int32(attempt) < latest.Attempt {
			continue
		}

		if int32(attempt) == latest.Attempt && latest.UID != "" && latest.UID != string(workflow.GetUID()) {
			return fmt.Errorf("workflow attempt %d UID changed", attempt)
		}

		phase, _, err := unstructured.NestedString(workflow.Object, "status", "phase")
		if err != nil {
			return fmt.Errorf("recover Workflow attempt %d phase: %w", attempt, err)
		}

		latest = signalv1.WorkflowStatus{
			Name: workflow.GetName(), UID: string(workflow.GetUID()), Attempt: int32(attempt), Phase: phase,
		}
	}

	if latest.Name == "" || latest == run.Status.Workflow {
		return nil
	}

	before := run.DeepCopy()
	run.Status.Workflow = latest

	return r.Status().Patch(ctx, run, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

func participantNodeUID(run *signalv1.CertificationRun, nodeName string) types.UID {
	for _, participant := range run.Spec.Participants {
		if participant.NodeName == nodeName {
			return participant.NodeUID
		}
	}

	return ""
}

func workflowParameter(workflow *unstructured.Unstructured, name string) string {
	parameters, _, err := unstructured.NestedSlice(workflow.Object, "status", "outputs", "parameters")
	if err == nil {
		if value := parameterValue(parameters, name); value != "" {
			return value
		}
	}

	nodes, _, err := unstructured.NestedMap(workflow.Object, "status", "nodes")
	if err != nil {
		return ""
	}

	if value := workflowNodeParameter(nodes[workflow.GetName()], name); value != "" {
		return value
	}

	for _, raw := range nodes {
		node, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		displayName, hasDisplayName := node["displayName"].(string)

		nodeName, hasNodeName := node["name"].(string)
		if (hasDisplayName && displayName == workflow.GetName()) ||
			(hasNodeName && nodeName == workflow.GetName()) {
			if value := workflowNodeParameter(node, name); value != "" {
				return value
			}
		}
	}

	fallback := ""

	for _, raw := range nodes {
		node, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		if value := workflowNodeParameter(node, name); value != "" {
			if fallback == "" {
				fallback = value
			} else if fallback != value {
				return ""
			}
		}
	}

	return fallback
}

func workflowNodeParameter(raw any, name string) string {
	node, ok := raw.(map[string]any)
	if !ok {
		return ""
	}

	outputs, ok := node["outputs"].(map[string]any)
	if !ok {
		return ""
	}

	parameters, ok := outputs["parameters"].([]any)
	if !ok {
		return ""
	}

	return parameterValue(parameters, name)
}

func parameterValue(parameters []any, name string) string {
	for _, raw := range parameters {
		parameter, ok := raw.(map[string]any)
		if !ok || parameter["name"] != name {
			continue
		}

		value, ok := parameter["value"].(string)
		if !ok {
			return ""
		}

		return value
	}

	return ""
}

func workflowMessage(workflow *unstructured.Unstructured) string {
	message, _, err := unstructured.NestedString(workflow.Object, "status", "message")
	if err != nil {
		return "workflow message is malformed"
	}

	if message == "" {
		return "workflow did not report a message"
	}

	return message
}

func digestString(content string) string {
	sum := sha256.Sum256([]byte(content))

	return "sha256:" + hex.EncodeToString(sum[:])
}

func runName(node, profile string, now time.Time) string {
	base := strings.ToLower(node + "-" + profile)
	base = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}

		return '-'
	}, base)

	base = strings.Trim(base, "-")
	if len(base) > 40 {
		base = strings.TrimRight(base[:40], "-")
	}

	return fmt.Sprintf("%s-%x-%s", base, now.Unix(), rand.String(5))
}

func (r *AdmissionReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}

	return time.Now()
}

func (r *RunReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}

	return time.Now()
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}

	return false
}

func removeString(values []string, remove string) []string {
	result := values[:0]
	for _, value := range values {
		if value != remove {
			result = append(result, value)
		}
	}

	return result
}

func validateStagePlan(plan []signalv1.CertificationStage) error {
	seen := map[string]struct{}{}

	for i := range plan {
		stage := &plan[i]
		if stage.ID == "" {
			return fmt.Errorf("stage %d has an empty ID", i)
		}

		if stage.DisplayName == "" {
			return fmt.Errorf("stage %q has an empty display name", stage.ID)
		}

		if _, ok := seen[stage.ID]; ok {
			return fmt.Errorf("duplicate stage ID %q", stage.ID)
		}

		seen[stage.ID] = struct{}{}
	}

	return nil
}

func AddToScheme(scheme *runtime.Scheme) error {
	return signalv1.AddToScheme(scheme)
}
