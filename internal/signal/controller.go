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
	var profile signalv1.CertificationProfile
	if err := admissionReader(r.Reader, r.Client).Get(ctx, client.ObjectKey{Name: profileName}, &profile); err != nil {
		return fmt.Errorf("get CertificationProfile %s: %w", profileName, err)
	}

	return r.admitProfile(ctx, node, &profile, trigger)
}

func (r *AdmissionReconciler) admitProfile(
	ctx context.Context,
	node *corev1.Node,
	profile *signalv1.CertificationProfile,
	trigger string,
) (retErr error) {
	explicit := explicitParticipants(profile.Spec.Participants)
	if err := validateParticipantPolicy(profile.Spec.Participants); err != nil {
		return fmt.Errorf("profile %s participants: %w", profile.Name, err)
	}

	if explicit && len(profile.Spec.Participants.NodeNames) > 0 &&
		!containsString(profile.Spec.Participants.NodeNames, node.Name) {
		return fmt.Errorf("request node %s is not a declared member of profile %s", node.Name, profile.Name)
	}

	if runName := node.Annotations[ReservationLabel]; runName != "" {
		var run signalv1.CertificationRun

		err := admissionReader(r.Reader, r.Client).Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: runName}, &run)
		if err == nil {
			if explicit {
				participants, err := r.selectParticipants(ctx, node, profile)
				if err != nil {
					return err
				}

				if run.Spec.Profile.Name != profile.Name || !sameParticipants(run.Spec.Participants, participants) {
					return fmt.Errorf("request node %s is reserved by a different profile or member set in run %s", node.Name, runName)
				}
			}

			return nil
		}

		if !apierrors.IsNotFound(err) {
			return err
		}

		return fmt.Errorf("node %s has stale Project Signal reservation %q", node.Name, runName)
	}

	if err := validateStagePlan(profile.Spec.StagePlan); err != nil {
		return fmt.Errorf("profile %s stage plan: %w", profile.Name, err)
	}

	if _, _, err := nodeWorkflow(profile.Spec.Workflow.Name, profile.Spec.Parameters); err != nil {
		return fmt.Errorf("profile %s execution plan: %w", profile.Name, err)
	}

	participants, err := r.selectParticipants(ctx, node, profile)
	if err != nil {
		return err
	}

	if explicit {
		exists, err := r.existingExplicitRun(ctx, participants, profile.Name)
		if err != nil || exists {
			return err
		}
	}

	nameNode := node.Name
	if explicit {
		nameNode = participants[0].NodeName
	}

	runName := runName(nameNode, profile.Name, r.now())

	releaseOnFailure := !explicit
	if !explicit {
		if err := r.reserveParticipants(ctx, participants, runName); err != nil {
			return err
		}
	}

	defer func() {
		if releaseOnFailure {
			retErr = errors.Join(retErr, r.releaseParticipants(context.WithoutCancel(ctx), participants, runName))
		}
	}()

	snapshotReservation := runName
	if explicit {
		snapshotReservation = ""
	}

	participantPlan, err := r.snapshotParticipants(ctx, participants, snapshotReservation)
	if err != nil {
		return err
	}

	parameters := snapshotParameters(profile.Spec.Parameters, node)
	for _, participant := range participantPlan {
		if participant.NodeName == node.Name {
			parameters = participantParameters(parameters, participant)
		}
	}

	testPlan, _, err := makeTestPlan(&signalv1.CertificationRunSpec{
		Workflow:        profile.Spec.Workflow,
		Participants:    participants,
		ParticipantPlan: participantPlan,
		Parameters:      parameters,
	})
	if err != nil {
		return err
	}

	snapshot, scriptDigest, err := r.createSnapshot(ctx, runName, profile)
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
			ParticipantPlan: participantPlan,
			TestPlan:        testPlan,
			Script:          signalv1.ScriptSnapshot{ConfigMapName: snapshot.Name, Key: profile.Spec.Script.Key, Digest: scriptDigest},
			Workflow:        profile.Spec.Workflow,
			Backend:         profile.Spec.Backend,
			RunnerImage:     profile.Spec.RunnerImage,
			Purpose:         profile.Spec.Purpose,
			StagePlan:       append([]signalv1.CertificationStage(nil), profile.Spec.StagePlan...),
			Parameters:      parameters,
			EvidencePath:    strings.Trim(profile.Spec.EvidencePath, "/") + "/" + runName + ".json",
			Deadline:        profile.Spec.Deadline,
			MaxAttempts:     maxAttempts,
			Tolerations:     profile.Spec.Tolerations,
			RequestNodeName: node.Name,
		},
	}

	if explicit {
		admission, err := r.membershipSnapshot(ctx, participants, profile, trigger)
		if err != nil {
			return errors.Join(err, client.IgnoreNotFound(r.Delete(context.WithoutCancel(ctx), snapshot)))
		}

		run.Spec.Admission = admission
		if err := validateRunAdmission(ctx, admissionReader(r.Reader, r.Client), run, false, true, r.now()); err != nil {
			return errors.Join(err, client.IgnoreNotFound(r.Delete(context.WithoutCancel(ctx), snapshot)))
		}
	}

	if err := r.Create(ctx, run); err != nil {
		deleteErr := r.Delete(context.WithoutCancel(ctx), snapshot)

		return errors.Join(fmt.Errorf("create CertificationRun: %w", err), client.IgnoreNotFound(deleteErr))
	}

	cleanupFailedAdmission := func(cause error) error {
		deleteRunErr := r.Delete(context.WithoutCancel(ctx), run)
		if deleteRunErr != nil && !apierrors.IsNotFound(deleteRunErr) {
			// Keep the reservation if the run could not be made inert. This
			// prevents another admission from overlapping the surviving run.
			releaseOnFailure = false
		}

		var deleteSnapshotErr error
		if !explicit {
			deleteSnapshotErr = r.Delete(context.WithoutCancel(ctx), snapshot)
		}

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
	return participantParameters(parameters, participantSnapshot(node))
}

func (r *AdmissionReconciler) selectParticipants(
	ctx context.Context,
	requestNode *corev1.Node,
	profile *signalv1.CertificationProfile,
) ([]signalv1.Participant, error) {
	if err := validateParticipantPolicy(profile.Spec.Participants); err != nil {
		return nil, fmt.Errorf("profile %s participants: %w", profile.Name, err)
	}

	if explicitParticipants(profile.Spec.Participants) {
		return r.selectExplicitParticipants(ctx, requestNode, profile)
	}

	count := profile.Spec.Participants.Count
	if count < 1 {
		return nil, fmt.Errorf("profile %s participant count must be positive", profile.Name)
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
	for _, participant := range participants {
		var node corev1.Node
		if err := admissionReader(r.Reader, r.Client).Get(ctx, client.ObjectKey{Name: participant.NodeName}, &node); err != nil {
			return errors.Join(
				fmt.Errorf("get participant %s: %w", participant.NodeName, err),
				r.releaseParticipants(context.WithoutCancel(ctx), participants, runName),
			)
		}

		if node.UID != participant.NodeUID {
			return errors.Join(
				fmt.Errorf("participant %s identity changed", participant.NodeName),
				r.releaseParticipants(context.WithoutCancel(ctx), participants, runName),
			)
		}

		if value := node.Annotations[ReservationLabel]; value == runName {
			continue
		} else if value != "" {
			return errors.Join(
				fmt.Errorf("participant %s is already reserved by %s", participant.NodeName, value),
				r.releaseParticipants(context.WithoutCancel(ctx), participants, runName),
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
				r.releaseParticipants(context.WithoutCancel(ctx), participants, runName),
			)
		}
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

	if run.Spec.Admission != nil && !run.Status.Phase.Terminal() {
		if err := r.recoverExplicitWorkflow(ctx, &run); err != nil {
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

	if run.Spec.Admission != nil && run.Status.Workflow.Name == "" && !run.Status.AdmissionReady {
		return r.completeExplicitAdmission(ctx, &run)
	}

	if run.Spec.Admission != nil && run.Status.Workflow.Name != "" {
		if err := validateRunAdmission(ctx, admissionReader(r.Reader, r.Client), &run, true, false, r.now()); err != nil {
			return r.rejectAdmission(ctx, &run, err)
		}
	}

	if run.Status.Workflow.Name == "" {
		return r.startAttempt(ctx, &run, 1)
	}

	workflow := &unstructured.Unstructured{}
	workflow.SetGroupVersionKind(workflowGVK)

	if err := r.Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: run.Status.Workflow.Name}, workflow); err != nil {
		if apierrors.IsNotFound(err) {
			return r.retryOrFinish(ctx, &run, nil, "Error", "WorkflowMissing", "workflow disappeared before terminal status")
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

		collection := collectTestResults(&run, workflow, false)

		results := run.Status.TestResults
		if collection.strict && attemptIndex(&run) < 0 {
			results = mergeTestResults(results, collection.results, run.Status.Workflow.Attempt)
		}

		if run.Status.Phase != signalv1.RunPhaseRunning || run.Status.Workflow.Phase != phase ||
			!reflect.DeepEqual(results, run.Status.TestResults) {
			before := run.DeepCopy()
			run.Status.Phase = signalv1.RunPhaseRunning
			run.Status.TestResults = results

			run.Status.Workflow.Phase = phase
			if run.Status.StartedAt == nil {
				now := metav1.NewTime(r.now())
				run.Status.StartedAt = &now
			}

			return ctrl.Result{}, r.Status().Patch(ctx, &run, client.MergeFrom(before))
		}

		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	case "Succeeded":
		parameter := workflowParameter
		if len(run.Spec.TestPlan) == 0 && supportedNodeWorkflow(run.Spec.Workflow.Name) {
			parameter = workflowRootParameter
		}

		verdict := parameter(workflow, "verdict")
		reason := parameter(workflow, "reason")

		if verdict == "" {
			verdict = "Inconclusive"
			reason = "workflow completed without a verdict"
		}

		return r.finish(ctx, &run, workflow, "Completed", verdict, reason)
	case "Failed", "Error":
		return r.retryOrFinish(ctx, &run, workflow, phase, "Workflow"+phase, workflowMessage(workflow))
	default:
		return ctrl.Result{}, fmt.Errorf("unsupported Workflow phase %q", phase)
	}
}

func (r *RunReconciler) startAttempt(ctx context.Context, run *signalv1.CertificationRun, attempt int32) (ctrl.Result, error) {
	if err := validateAdmissionContract(run); err != nil {
		return r.rejectAdmission(ctx, run, err)
	}

	if run.Spec.Admission != nil {
		if !run.Status.AdmissionReady {
			return r.rejectAdmission(ctx, run, fmt.Errorf("member reservations have not been admitted"))
		}

		if err := validateRunAdmission(ctx, admissionReader(r.Reader, r.Client), run, true, true, r.now()); err != nil {
			return r.rejectAdmission(ctx, run, err)
		}

		if err := r.prepareExplicitSnapshot(ctx, run); err != nil {
			return r.rejectAdmission(ctx, run, err)
		}

		var latest signalv1.CertificationRun
		if err := admissionReader(r.Reader, r.Client).Get(ctx, client.ObjectKeyFromObject(run), &latest); err != nil {
			return ctrl.Result{}, err
		}

		if latest.Spec.Cancel || !latest.DeletionTimestamp.IsZero() || latest.Status.Phase.Terminal() {
			return ctrl.Result{RequeueAfter: time.Millisecond}, nil
		}
	}

	workflow, err := buildWorkflow(run, attempt)
	if err != nil {
		return ctrl.Result{}, err
	}

	if run.Spec.Admission != nil && !run.CreationTimestamp.IsZero() {
		remaining := run.CreationTimestamp.Add(run.Spec.Deadline.Duration).Sub(r.now())

		seconds := int64((remaining + time.Second - 1) / time.Second)
		if seconds < 1 {
			return r.rejectAdmission(ctx, run, fmt.Errorf("run deadline expired before workflow creation"))
		}

		if err := unstructured.SetNestedField(workflow.Object, seconds, "spec", "activeDeadlineSeconds"); err != nil {
			return ctrl.Result{}, err
		}
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
	run.Status.Verdict = ""
	run.Status.Reason = ""
	run.Status.Evidence = signalv1.EvidenceStatus{}

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
	testPlan, _, err := effectiveTestPlan(run)
	if err != nil {
		return nil, fmt.Errorf("build Workflow test plan: %w", err)
	}

	if testPlan == nil {
		testPlan = []signalv1.TestInvocation{}
	}

	testPlanJSON, err := json.Marshal(testPlan)
	if err != nil {
		return nil, fmt.Errorf("encode Workflow test plan: %w", err)
	}

	participantPlan := run.Spec.ParticipantPlan
	if len(participantPlan) == 0 {
		participantPlan = make([]signalv1.ParticipantSnapshot, 0, len(run.Spec.Participants))
		for _, participant := range run.Spec.Participants {
			participantPlan = append(participantPlan, signalv1.ParticipantSnapshot{
				NodeName:               participant.NodeName,
				NodeUID:                participant.NodeUID,
				ObservedGPUCapacity:    run.Spec.Parameters[ObservedGPUCapacityParameter],
				ObservedGPUAllocatable: run.Spec.Parameters[ObservedGPUAllocatableParameter],
			})
		}
	}

	participantPlanJSON, err := json.Marshal(participantPlan)
	if err != nil {
		return nil, fmt.Errorf("encode Workflow participant plan: %w", err)
	}

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
		map[string]any{"name": ParticipantPlanParameter, "value": string(participantPlanJSON)},
		map[string]any{"name": TestPlanParameter, "value": string(testPlanJSON)},
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

	parameterNames := make([]string, 0, len(run.Spec.Parameters))
	for parameterName := range run.Spec.Parameters {
		parameterNames = append(parameterNames, parameterName)
	}

	sort.Strings(parameterNames)

	for _, parameterName := range parameterNames {
		if reservedWorkflowParameter(parameterName) {
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
	workflow *unstructured.Unstructured,
	executionState string,
	reason string,
	message string,
) (ctrl.Result, error) {
	collection := collectTestResults(run, workflow, true)
	if collection.strict && collection.verdict == "Fail" {
		return r.finish(ctx, run, workflow, executionState, collection.verdict, collection.reason)
	}

	if run.Status.Workflow.Attempt < run.Spec.MaxAttempts {
		verdict := "Inconclusive"
		attemptReason := reason + ": " + message

		if err := r.preserveAttempt(ctx, run, workflow, executionState, verdict, attemptReason); err != nil {
			return ctrl.Result{}, err
		}

		if requiresWorkflowCleanup(run) {
			stopped, err := r.ensureWorkflowStopped(ctx, run)
			if err != nil {
				return ctrl.Result{}, err
			}

			if !stopped {
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
		}

		return r.startAttempt(ctx, run, run.Status.Workflow.Attempt+1)
	}

	return r.finish(ctx, run, workflow, executionState, "Inconclusive", reason+": "+message)
}

func (r *RunReconciler) finish(
	ctx context.Context,
	run *signalv1.CertificationRun,
	workflow *unstructured.Unstructured,
	executionState string,
	verdict string,
	reason string,
) (ctrl.Result, error) {
	collection := collectTestResults(run, workflow, true)
	if collection.strict && (executionState == "Completed" || collection.verdict == "Fail") {
		verdict, reason = collection.verdict, collection.reason
	}

	if err := r.preserveAttempt(ctx, run, workflow, executionState, verdict, reason); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.preserveFinalEvidence(ctx, run, workflow, nil); err != nil {
		return ctrl.Result{}, err
	}

	attempt := run.Status.Attempts[attemptIndex(run)]
	before := run.DeepCopy()
	run.Status.ExecutionState = attempt.ExecutionState
	run.Status.Verdict = attempt.Verdict
	run.Status.Reason = attempt.Reason

	if executionState == "Completed" {
		run.Status.Workflow.Phase = "Succeeded"
	} else {
		run.Status.Workflow.Phase = executionState
	}

	run.Status.FinishedAt = &attempt.FinishedAt

	switch attempt.Verdict {
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

	if !requiresWorkflowCleanup(run) {
		if err := projectTerminal(ctx, r.Client, run); err != nil {
			return ctrl.Result{}, err
		}
	}

	if r.Recorder != nil {
		r.Recorder.Eventf(run, nil, corev1.EventTypeNormal, "CertificationFinished", "FinishCertification",
			"ExecutionState=%s Verdict=%s Reason=%s", attempt.ExecutionState, attempt.Verdict, attempt.Reason)
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
	workflow *unstructured.Unstructured,
	attempt signalv1.AttemptStatus,
) ([]byte, error) {
	var snapshot corev1.ConfigMap
	if err := r.Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: run.Spec.Script.ConfigMapName}, &snapshot); err != nil {
		return nil, fmt.Errorf("read script snapshot for evidence: %w", err)
	}

	nodes, nodesErr := workflowNodes(workflow)

	doc := map[string]any{
		"apiVersion":         "signal.unbounded-cloud.io/evidence/v1alpha1",
		"runID":              string(run.UID),
		"runName":            run.Name,
		"runSpec":            run.Spec,
		"profile":            run.Spec.Profile,
		"participants":       run.Spec.Participants,
		"admission":          run.Spec.Admission,
		"participantPlan":    run.Spec.ParticipantPlan,
		"testPlan":           run.Spec.TestPlan,
		"testResults":        run.Status.TestResults,
		"attempts":           run.Status.Attempts,
		"workflow":           run.Status.Workflow,
		"workflowNodes":      nodes,
		"workflowNodeStatus": workflowNodeStatus(workflow),
		"backend":            run.Spec.Backend,
		"runnerImage":        run.Spec.RunnerImage,
		"purpose":            run.Spec.Purpose,
		"stagePlan":          run.Spec.StagePlan,
		"parameters":         run.Spec.Parameters,
		"scriptSnapshot":     run.Spec.Script,
		"scriptContent":      snapshot.Data[run.Spec.Script.Key],
		"executionState":     attempt.ExecutionState,
		"verdict":            attempt.Verdict,
		"reason":             attempt.Reason,
		"startedAt":          run.Status.StartedAt,
		"finishedAt":         attempt.FinishedAt.UTC().Format(time.RFC3339Nano),
	}
	if nodesErr != nil {
		doc["workflowNodesError"] = nodesErr.Error()
	}

	return json.MarshalIndent(doc, "", "  ")
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
	attempt := run.Status.Attempts[attemptIndex(run)]

	run.Status.FinishedAt = &attempt.FinishedAt
	if err := r.Status().Patch(ctx, run, client.MergeFrom(before)); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: time.Millisecond}, nil
}

func (r *RunReconciler) cleanup(ctx context.Context, run *signalv1.CertificationRun) (ctrl.Result, error) {
	ready, err := r.strictCleanupReady(ctx, run)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !ready {
		return ctrl.Result{RequeueAfter: time.Second}, nil
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

	if run.Spec.Admission != nil {
		snapshot := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: run.Namespace, Name: run.Spec.Script.ConfigMapName}}
		if err := r.Delete(ctx, snapshot); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	before := run.DeepCopy()
	run.Finalizers = removeString(run.Finalizers, RunFinalizer)

	return ctrl.Result{}, r.Patch(ctx, run, client.MergeFrom(before))
}

func (r *RunReconciler) releaseRunReservations(ctx context.Context, run *signalv1.CertificationRun) (bool, error) {
	admission := &AdmissionReconciler{Client: r.Client, Reader: r.Reader}
	if err := admission.releaseParticipants(ctx, cleanupParticipants(run), run.Name); err != nil {
		return false, err
	}

	return true, nil
}

func (r *RunReconciler) clearRequest(ctx context.Context, run *signalv1.CertificationRun) error {
	if run.Spec.Admission != nil {
		var errs []error
		for _, participant := range cleanupParticipants(run) {
			errs = append(errs, r.clearNodeRequest(ctx, run, participant.NodeName))
		}

		return errors.Join(errs...)
	}

	return r.clearNodeRequest(ctx, run, run.Spec.RequestNodeName)
}

func (r *RunReconciler) clearNodeRequest(ctx context.Context, run *signalv1.CertificationRun, name string) error {
	var node corev1.Node
	if err := admissionReader(r.Reader, r.Client).Get(ctx, client.ObjectKey{Name: name}, &node); err != nil {
		return client.IgnoreNotFound(err)
	}

	if requestNodeUID := participantNodeUID(run, name); requestNodeUID != "" && node.UID != requestNodeUID {
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
	if run.Spec.Admission != nil || len(run.Spec.Participants) > 1 {
		return r.ensureGroupWorkflowsStopped(ctx, run)
	}

	if run.Status.Workflow.Name == "" {
		return true, nil
	}

	workflow := &unstructured.Unstructured{}
	workflow.SetGroupVersionKind(workflowGVK)
	workflow.SetNamespace(run.Namespace)
	workflow.SetName(run.Status.Workflow.Name)

	if err := r.Get(ctx, client.ObjectKeyFromObject(workflow), workflow); err != nil {
		if apierrors.IsNotFound(err) {
			return r.ownedBackendsGone(ctx, run)
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

	if err := r.Get(ctx, client.ObjectKeyFromObject(workflow), workflow); err != nil {
		if apierrors.IsNotFound(err) {
			return r.ownedBackendsGone(ctx, run)
		}

		return false, fmt.Errorf("confirm Workflow deletion: %w", err)
	}

	return false, nil
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
	return workflowResultParameter(workflow, name, true)
}

func workflowRootParameter(workflow *unstructured.Unstructured, name string) string {
	return workflowResultParameter(workflow, name, false)
}

func workflowResultParameter(workflow *unstructured.Unstructured, name string, allowTaskFallback bool) string {
	parameters, _, err := unstructured.NestedSlice(workflow.Object, "status", "outputs", "parameters")
	if err == nil {
		if value := parameterValue(parameters, name); value != "" {
			return value
		}
	}

	nodes, err := workflowNodes(workflow)
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

	if !allowTaskFallback {
		return ""
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
