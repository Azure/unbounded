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
	"io"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
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
	"github.com/Azure/unbounded/internal/signaladapter"
	"github.com/Azure/unbounded/internal/signalworkload"
)

const (
	RequestLabel           = "signal.unbounded-cloud.io/certification-request"
	ReservationLabel       = "signal.unbounded-cloud.io/reserved-by"
	RunNodeLabel           = "signal.unbounded-cloud.io/request-node"
	RunProfileLabel        = "signal.unbounded-cloud.io/profile"
	RunTriggerAnnotation   = "signal.unbounded-cloud.io/trigger"
	RunAdmissionAnnotation = "signal.unbounded-cloud.io/admission"
	RunFinalizer           = "signal.unbounded-cloud.io/cleanup"
	SnapshotRunLabel       = "signal.unbounded-cloud.io/run"
	runAdmissionReady      = "ready"
	runAdmissionPending    = "pending"
)

var runnerImageDigestPattern = regexp.MustCompile(`^[^@\s]+@sha256:[0-9a-f]{64}$`)

var reservedScriptEnvironmentNames = map[string]struct{}{
	"BASH_ENV":                       {},
	"BASHOPTS":                       {},
	"CDPATH":                         {},
	"ENV":                            {},
	"GLOBIGNORE":                     {},
	"HOST_PROC_ROOT":                 {},
	"HOST_SYSFS_ROOT":                {},
	"IFS":                            {},
	"LD_LIBRARY_PATH":                {},
	"LD_PRELOAD":                     {},
	"PATH":                           {},
	"PROJECT_SIGNAL_PARAMETERS_JSON": {},
	"PROJECT_SIGNAL_RESULTS_DIR":     {},
	"PYTHONHOME":                     {},
	"PYTHONPATH":                     {},
	"SHELLOPTS":                      {},
}

var workflowOutputLimits = map[string]int{
	"backend-resources": signaladapter.MaxBackendOutputBytes,
	"backend-evidence":  signaladapter.MaxBackendEvidenceBytes,
	"measurements":      signaladapter.MaxMeasurementsBytes,
	"reason":            signaladapter.MaxReasonBytes,
	"verdict":           32,
}

const (
	groupedCheckCount           = 15
	adapterServiceAccountName   = "project-signal-adapter"
	adapterWorkflowTemplateName = "project-signal-participant-adapter"
	maximumWorkflowOutputBytes  = 768 * 1024
	maximumStatusTextRunes      = 1024
)

var workflowGVK = schema.GroupVersionKind{
	Group:   "argoproj.io",
	Version: "v1alpha1",
	Kind:    "Workflow",
}

type AdmissionReconciler struct {
	client.Client
	APIReader       client.Reader
	Namespace       string
	ScriptNamespace string
	Recorder        recorder.EventRecorder
	Now             func() time.Time
	admissionMu     sync.Mutex
}

type RunReconciler struct {
	client.Client
	APIReader         client.Reader
	Evidence          EvidenceStore
	Now               func() time.Time
	MaxConcurrentRuns int
	Recorder          recorder.EventRecorder
}

func (r *AdmissionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}, builder.WithPredicates(predicate.LabelChangedPredicate{})).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

func (r *RunReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
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
		runName := node.Annotations[ReservationLabel]
		if runName == "" || node.Labels[signalv1.ReservationNodeUIDLabel] != string(node.UID) {
			return ctrl.Result{}, nil
		}

		r.admissionMu.Lock()
		defer r.admissionMu.Unlock()

		_, err := r.resumeOrReleaseReservation(ctx, runName)

		return ctrl.Result{}, err
	}

	return ctrl.Result{}, r.admitNode(ctx, &node, profileName)
}

func (r *AdmissionReconciler) admitNode(ctx context.Context, node *corev1.Node, profileName string) error {
	r.admissionMu.Lock()
	defer r.admissionMu.Unlock()

	if runName := node.Annotations[ReservationLabel]; runName != "" {
		handled, err := r.resumeOrReleaseReservation(ctx, runName)
		if err != nil || handled {
			return err
		}
	}

	var profile signalv1.CertificationProfile
	if err := r.Get(ctx, client.ObjectKey{Name: profileName}, &profile); err != nil {
		return fmt.Errorf("get CertificationProfile %s: %w", profileName, err)
	}

	checks := profileChecks(&profile)
	if err := validateCheckPlan(checks, profile.Spec.Deadline.Duration); err != nil {
		return fmt.Errorf("profile %s checks: %w", profile.Name, err)
	}

	if err := validateParticipantConstraints(&profile, checks); err != nil {
		return fmt.Errorf("profile %s participant constraints: %w", profile.Name, err)
	}

	for index := range checks {
		check := &checks[index]
		if err := signalv1.ValidateExecutionDeadlines(
			check.BackendActiveDeadline.Duration,
			profile.Spec.Deadline.Duration,
		); err != nil {
			return fmt.Errorf("profile %s check %s runtime contract: %w", profile.Name, check.ID, err)
		}

		if !runnerImageDigestPattern.MatchString(check.RunnerImage) {
			return fmt.Errorf("profile %s check %s runner image must use an immutable sha256 digest", profile.Name, check.ID)
		}

		if err := validateScriptEnvironment(check.ScriptEnvironment); err != nil {
			return fmt.Errorf("profile %s check %s script environment: %w", profile.Name, check.ID, err)
		}

		if err := signalworkload.Validate(check.WorkloadPolicy); err != nil {
			return fmt.Errorf("profile %s check %s workload policy: %w", profile.Name, check.ID, err)
		}
	}

	participants, err := r.selectParticipants(ctx, node, &profile)
	if err != nil {
		return err
	}

	for index := range checks {
		checkParticipants, _, err := participantsForCheck(participants, checks[index].ParticipantPolicy)
		if err != nil {
			return fmt.Errorf("check %s participants: %w", checks[index].ID, err)
		}

		if len(checkParticipants) > signaladapter.MaxCheckParticipants {
			return fmt.Errorf(
				"check %s selects %d participants, maximum is %d",
				checks[index].ID,
				len(checkParticipants),
				signaladapter.MaxCheckParticipants,
			)
		}

		minimumEvidenceBytes := signaladapter.MinimumBackendEvidenceBytes(len(checkParticipants))
		if signaladapter.MaxBackendEvidenceBytes < minimumEvidenceBytes {
			return fmt.Errorf(
				"check %s fixed backend evidence limit %d cannot represent %d participants; requires at least %d bytes",
				checks[index].ID,
				signaladapter.MaxBackendEvidenceBytes,
				len(checkParticipants),
				minimumEvidenceBytes,
			)
		}

		minimumReasonBytes := signaladapter.MinimumPartialReasonBytes()
		if signaladapter.MaxReasonBytes < minimumReasonBytes {
			return fmt.Errorf(
				"check %s fixed reason limit %d cannot represent mandatory failure output; requires at least %d bytes",
				checks[index].ID,
				signaladapter.MaxReasonBytes,
				minimumReasonBytes,
			)
		}

		minimumMeasurementsBytes := signaladapter.MinimumPartialMeasurementsBytes(len(checkParticipants))
		if signaladapter.MaxMeasurementsBytes < minimumMeasurementsBytes {
			return fmt.Errorf(
				"check %s fixed measurements limit %d cannot represent %d participants; requires at least %d bytes",
				checks[index].ID,
				signaladapter.MaxMeasurementsBytes,
				len(checkParticipants),
				minimumMeasurementsBytes,
			)
		}

		minimumBackendResourcesBytes := signaladapter.MinimumPartialBackendResourcesBytes(len(checkParticipants))
		if signaladapter.MaxBackendOutputBytes < minimumBackendResourcesBytes {
			return fmt.Errorf(
				"check %s fixed backend resource limit %d cannot represent %d participants; requires at least %d bytes",
				checks[index].ID,
				signaladapter.MaxBackendOutputBytes,
				len(checkParticipants),
				minimumBackendResourcesBytes,
			)
		}

		if _, err := signalworkload.Encode(checks[index].WorkloadPolicy); err != nil {
			return fmt.Errorf("check %s workload policy: %w", checks[index].ID, err)
		}

		if err := validateGPUCount(checks[index].GPUCount, checks[index].Backend); err != nil {
			return fmt.Errorf("check %s GPU count: %w", checks[index].ID, err)
		}
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

	snapshot, checkSnapshots, err := r.createSnapshot(ctx, runName, checks)
	if err != nil {
		return err
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
				RunTriggerAnnotation:   "manual",
				RunAdmissionAnnotation: runAdmissionPending,
			},
			Finalizers: []string{RunFinalizer},
		},
		Spec: signalv1.CertificationRunSpec{
			Profile: signalv1.PinnedProfile{
				Name:       profile.Name,
				UID:        profile.UID,
				Generation: profile.Generation,
			},
			Participants: participants,
			ScriptBundle: signalv1.ScriptBundleReference{
				Name: snapshot.Name,
				UID:  snapshot.UID,
			},
			Checks:   checkSnapshots,
			Deadline: profile.Spec.Deadline,
		},
	}
	if _, err := buildWorkflow(run); err != nil {
		deleteErr := r.Delete(context.WithoutCancel(ctx), snapshot)

		return errors.Join(fmt.Errorf("validate executable plan: %w", err), client.IgnoreNotFound(deleteErr))
	}

	if err := r.Create(ctx, run); err != nil {
		deleteErr := r.Delete(context.WithoutCancel(ctx), snapshot)

		return errors.Join(fmt.Errorf("create CertificationRun: %w", err), client.IgnoreNotFound(deleteErr))
	}

	cleanupFailedAdmission := func(cause error) error {
		releaseOnFailure = false
		deleteRunErr := r.Delete(context.WithoutCancel(ctx), run)

		// The Run finalizer owns rollback once the Run exists, including its snapshot.
		return errors.Join(cause, client.IgnoreNotFound(deleteRunErr))
	}

	snapshot.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(run, signalv1.GroupVersion.WithKind("CertificationRun"))}
	if err := r.Update(ctx, snapshot); err != nil {
		return cleanupFailedAdmission(fmt.Errorf("bind script snapshot to run: %w", err))
	}

	releaseOnFailure = false

	if err := r.completeRunAdmission(ctx, run); err != nil {
		return err
	}

	if r.Recorder != nil {
		r.Recorder.Eventf(run, nil, corev1.EventTypeNormal, "CertificationAdmitted", "AdmitCertification",
			"Created CertificationRun %s with %d immutable checks", run.Name, len(checkSnapshots))
	}

	return nil
}

func (r *AdmissionReconciler) resumeOrReleaseReservation(ctx context.Context, runName string) (bool, error) {
	var run signalv1.CertificationRun

	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}

	err := reader.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: runName}, &run)
	if err == nil {
		return true, r.completeRunAdmission(ctx, &run)
	}

	if !apierrors.IsNotFound(err) {
		return false, err
	}

	var snapshot corev1.ConfigMap

	err = reader.Get(
		ctx,
		client.ObjectKey{
			Namespace: r.Namespace,
			Name:      runName + "-scripts",
		},
		&snapshot,
	)
	if err == nil {
		if snapshot.Labels[SnapshotRunLabel] != runName {
			return false, fmt.Errorf(
				"orphaned snapshot %s is not bound to reservation %q",
				snapshot.Name,
				runName,
			)
		}

		if err := r.Delete(ctx, &snapshot); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("delete orphaned script snapshot: %w", err)
		}
	} else if !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("get orphaned script snapshot: %w", err)
	}

	if err := r.releaseOrphanedReservations(ctx, runName); err != nil {
		return false, fmt.Errorf("release orphaned Project Signal reservations: %w", err)
	}

	return false, nil
}

func (r *AdmissionReconciler) completeRunAdmission(
	ctx context.Context,
	run *signalv1.CertificationRun,
) error {
	if run.Annotations[RunAdmissionAnnotation] == runAdmissionReady {
		return nil
	}

	if err := projectPending(ctx, r.Client, run); err != nil {
		return fmt.Errorf("project pending certification state: %w", err)
	}

	before := run.DeepCopy()
	if run.Annotations == nil {
		run.Annotations = map[string]string{}
	}

	run.Annotations[RunAdmissionAnnotation] = runAdmissionReady

	if err := r.Patch(
		ctx,
		run,
		client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}),
	); err != nil {
		return fmt.Errorf("mark certification admission ready: %w", err)
	}

	return nil
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

	if count > 64 {
		return nil, fmt.Errorf("profile %s participant count must not exceed 64", profile.Name)
	}

	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}

	var authoritativeRequest corev1.Node
	if err := reader.Get(ctx, client.ObjectKey{Name: requestNode.Name}, &authoritativeRequest); err != nil {
		return nil, fmt.Errorf("read request node %s: %w", requestNode.Name, err)
	}

	if requestNode.UID != "" && authoritativeRequest.UID != requestNode.UID {
		return nil, fmt.Errorf("request node %s identity changed", requestNode.Name)
	}

	requestNode = &authoritativeRequest

	selector, err := metav1.LabelSelectorAsSelector(&profile.Spec.Participants.Selector)
	if err != nil {
		return nil, fmt.Errorf("profile %s participant selector: %w", profile.Name, err)
	}

	if !selector.Matches(labels.Set(requestNode.Labels)) ||
		!nodeEligible(requestNode, profile.Spec.Participants.MinimumAllocatable) {
		return nil, fmt.Errorf("request node %s does not satisfy profile %s", requestNode.Name, profile.Name)
	}

	var nodes corev1.NodeList

	if err := reader.List(ctx, &nodes); err != nil {
		return nil, fmt.Errorf("list participant nodes: %w", err)
	}

	sort.Slice(nodes.Items, func(i, j int) bool { return nodes.Items[i].Name < nodes.Items[j].Name })

	candidates := []corev1.Node{*requestNode.DeepCopy()}

	for i := range nodes.Items {
		candidate := &nodes.Items[i]

		if candidate.Name == requestNode.Name ||
			!selector.Matches(labels.Set(candidate.Labels)) ||
			!nodeEligible(candidate, profile.Spec.Participants.MinimumAllocatable) ||
			candidate.Annotations[ReservationLabel] != "" ||
			candidate.Labels[signalv1.ReservationNodeUIDLabel] != "" {
			continue
		}

		candidates = append(candidates, *candidate.DeepCopy())
	}

	if int32(len(candidates)) < count {
		return nil, fmt.Errorf(
			"profile %s requires %d eligible unlocked participants, found %d",
			profile.Name,
			count,
			len(candidates),
		)
	}

	selected, err := selectParticipantPlan(
		candidates,
		requestNode.Name,
		count,
		profile.Spec.ParticipantRoles,
		profile.Spec.TopologyConstraints,
	)
	if err != nil {
		return nil, fmt.Errorf("profile %s participant constraints: %w", profile.Name, err)
	}

	return selected, nil
}

func (r *AdmissionReconciler) releaseOrphanedReservations(ctx context.Context, runName string) error {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}

	var nodes corev1.NodeList
	if err := reader.List(ctx, &nodes); err != nil {
		return fmt.Errorf("list Nodes reserved by %s: %w", runName, err)
	}

	participants := make([]signalv1.Participant, 0)

	for index := range nodes.Items {
		node := &nodes.Items[index]
		if node.Annotations[ReservationLabel] == runName {
			participants = append(participants, signalv1.Participant{
				NodeName: node.Name,
				NodeUID:  node.UID,
			})
		}
	}

	return r.releaseParticipants(ctx, participants, runName)
}

func participantSnapshot(node *corev1.Node, index int32) signalv1.Participant {
	return signalv1.Participant{
		Index:       index,
		NodeName:    node.Name,
		NodeUID:     node.UID,
		Allocatable: node.Status.Allocatable.DeepCopy(),
	}
}

type participantRoleSlot struct {
	name string
}

func selectParticipantPlan(
	candidates []corev1.Node,
	requestNodeName string,
	count int32,
	roles []signalv1.ParticipantRoleConstraint,
	topology []signalv1.ParticipantTopologyConstraint,
) ([]signalv1.Participant, error) {
	assignments, err := solveParticipantRoles(
		candidates,
		requestNodeName,
		count,
		roles,
		topology,
	)
	if err != nil {
		return nil, err
	}

	selectedIndexes := make([]int, 0, count)
	selected := make([]bool, len(candidates))

	selectedIndexes = append(selectedIndexes, 0)
	selected[0] = true

	for index := range candidates {
		if selected[index] || len(assignments[index]) == 0 {
			continue
		}

		selectedIndexes = append(selectedIndexes, index)
		selected[index] = true
	}

	for index := range candidates {
		if int32(len(selectedIndexes)) == count {
			break
		}

		if selected[index] {
			continue
		}

		selectedIndexes = append(selectedIndexes, index)
		selected[index] = true
	}

	if int32(len(selectedIndexes)) != count {
		return nil, fmt.Errorf("participant plan selected %d participants, want %d", len(selectedIndexes), count)
	}

	participants := make([]signalv1.Participant, 0, count)
	for _, candidateIndex := range selectedIndexes {
		participant := participantSnapshot(&candidates[candidateIndex], int32(len(participants)))
		if len(assignments[candidateIndex]) > 0 {
			participant.Role = assignments[candidateIndex][0]
		}

		participant.TopologyValues = topologyValues(&candidates[candidateIndex], topology)
		participants = append(participants, participant)
	}

	return participants, nil
}

func topologyValues(
	node *corev1.Node,
	constraints []signalv1.ParticipantTopologyConstraint,
) map[string]string {
	values := make(map[string]string)

	for index := range constraints {
		key := constraints[index].LabelKey
		if value, exists := node.Labels[key]; exists {
			values[key] = value
		}
	}

	if len(values) == 0 {
		return nil
	}

	return values
}

func solveParticipantRoles(
	candidates []corev1.Node,
	requestNodeName string,
	participantCount int32,
	roles []signalv1.ParticipantRoleConstraint,
	topology []signalv1.ParticipantTopologyConstraint,
) ([][]string, error) {
	roleNames := make(map[string]struct{}, len(roles))
	roleSelectors := make(map[string]labels.Selector, len(roles))
	slots := make([]participantRoleSlot, 0, participantCount)

	for roleIndex := range roles {
		role := &roles[roleIndex]
		if _, duplicate := roleNames[role.Name]; duplicate {
			return nil, fmt.Errorf("duplicate participant role %q", role.Name)
		}

		selector, err := metav1.LabelSelectorAsSelector(&role.Selector)
		if err != nil {
			return nil, fmt.Errorf("role %s selector: %w", role.Name, err)
		}

		roleNames[role.Name] = struct{}{}
		roleSelectors[role.Name] = selector

		for range role.Count {
			slots = append(slots, participantRoleSlot{name: role.Name})
		}
	}

	if int32(len(slots)) > participantCount {
		return nil, fmt.Errorf(
			"participant roles require %d assignments but participant count is %d",
			len(slots),
			participantCount,
		)
	}

	for index := range topology {
		if _, exists := roleNames[topology[index].Role]; !exists {
			return nil, fmt.Errorf("topology constraint references unknown role %q", topology[index].Role)
		}
	}

	assignments := make([][]string, len(candidates))
	assigned := make([]bool, len(candidates))
	slotAssignments := make([]int, len(slots))
	candidateOrders := make(map[string][]int, len(roles))

	for role, selector := range roleSelectors {
		candidateOrders[role] = roleCandidateOrder(candidates, selector, role, topology)
	}

	const maximumSearchStates = 1_000_000

	searchStates := 0
	searchExhausted := false

	var search func(int) bool

	search = func(slotIndex int) bool {
		if searchExhausted {
			return false
		}

		searchStates++
		if searchStates > maximumSearchStates {
			searchExhausted = true

			return false
		}

		if slotIndex == len(slots) {
			requestAssigned := requestNodeName == ""
			assignedCount := int32(0)

			for index := range candidates {
				if len(assignments[index]) == 0 {
					continue
				}

				assignedCount++

				if candidates[index].Name == requestNodeName {
					requestAssigned = true
				}
			}

			if !requestAssigned && assignedCount >= participantCount {
				return false
			}

			return participantTopologySatisfied(candidates, assignments, topology)
		}

		slot := &slots[slotIndex]

		firstCandidate := 0
		if slotIndex > 0 && slots[slotIndex-1].name == slot.name {
			firstCandidate = slotAssignments[slotIndex-1] + 1
		}

		order := candidateOrders[slot.name]
		for candidatePosition := firstCandidate; candidatePosition < len(order); candidatePosition++ {
			candidateIndex := order[candidatePosition]
			if assigned[candidateIndex] {
				continue
			}

			assigned[candidateIndex] = true
			assignments[candidateIndex] = []string{slot.name}
			slotAssignments[slotIndex] = candidatePosition

			roleComplete := slotIndex+1 == len(slots) || slots[slotIndex+1].name != slot.name
			if (!roleComplete || participantRoleTopologySatisfied(
				candidates,
				assignments,
				slot.name,
				topology,
			)) && search(slotIndex+1) {
				return true
			}

			assignments[candidateIndex] = nil
			assigned[candidateIndex] = false
		}

		return false
	}

	if !search(0) {
		if searchExhausted {
			return nil, fmt.Errorf("participant role search exceeded %d states", maximumSearchStates)
		}

		return nil, errors.New("eligible participants cannot satisfy role and topology constraints")
	}

	return assignments, nil
}

func roleCandidateOrder(
	candidates []corev1.Node,
	selector labels.Selector,
	role string,
	topology []signalv1.ParticipantTopologyConstraint,
) []int {
	constraints := make([]signalv1.ParticipantTopologyConstraint, 0)

	for index := range topology {
		if topology[index].Role == role {
			constraints = append(constraints, topology[index])
		}
	}

	if len(constraints) == 0 {
		order := make([]int, 0, len(candidates))
		for index := range candidates {
			if selector.Matches(labels.Set(candidates[index].Labels)) {
				order = append(order, index)
			}
		}

		return order
	}

	groups := map[string][]int{}

	for index := range candidates {
		if !selector.Matches(labels.Set(candidates[index].Labels)) {
			continue
		}

		values := make([]string, len(constraints))
		valid := true

		for constraintIndex := range constraints {
			values[constraintIndex] = candidates[index].Labels[constraints[constraintIndex].LabelKey]
			if values[constraintIndex] == "" {
				valid = false

				break
			}
		}

		if valid {
			key := strings.Join(values, "\x00")
			groups[key] = append(groups[key], index)
		}
	}

	keys := slices.Sorted(maps.Keys(groups))
	order := make([]int, 0, len(candidates))

	for offset := 0; ; offset++ {
		added := false

		for _, key := range keys {
			if offset >= len(groups[key]) {
				continue
			}

			order = append(order, groups[key][offset])
			added = true
		}

		if !added {
			return order
		}
	}
}

func participantRoleTopologySatisfied(
	candidates []corev1.Node,
	assignments [][]string,
	role string,
	topology []signalv1.ParticipantTopologyConstraint,
) bool {
	for index := range topology {
		if topology[index].Role != role {
			continue
		}

		if !participantTopologyConstraintSatisfied(candidates, assignments, &topology[index]) {
			return false
		}
	}

	return true
}

func participantTopologySatisfied(
	candidates []corev1.Node,
	assignments [][]string,
	topology []signalv1.ParticipantTopologyConstraint,
) bool {
	for constraintIndex := range topology {
		if !participantTopologyConstraintSatisfied(
			candidates,
			assignments,
			&topology[constraintIndex],
		) {
			return false
		}
	}

	return true
}

func participantTopologyConstraintSatisfied(
	candidates []corev1.Node,
	assignments [][]string,
	constraint *signalv1.ParticipantTopologyConstraint,
) bool {
	values := map[string]struct{}{}

	for participantIndex := range candidates {
		if !slices.Contains(assignments[participantIndex], constraint.Role) {
			continue
		}

		value := candidates[participantIndex].Labels[constraint.LabelKey]
		if value == "" {
			return false
		}

		values[value] = struct{}{}
	}

	return int32(len(values)) >= constraint.MinDistinctValues
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
		if err := r.Get(ctx, client.ObjectKey{Name: participant.NodeName}, &node); err != nil {
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

		if node.Spec.Unschedulable {
			return errors.Join(
				fmt.Errorf("participant %s became unschedulable before reservation", participant.NodeName),
				r.releaseParticipants(context.WithoutCancel(ctx), reserved, runName),
			)
		}

		if value := node.Annotations[ReservationLabel]; value != "" {
			return errors.Join(
				fmt.Errorf("participant %s is already reserved by %s", participant.NodeName, value),
				r.releaseParticipants(context.WithoutCancel(ctx), reserved, runName),
			)
		}

		if value := node.Labels[signalv1.ReservationNodeUIDLabel]; value != "" {
			return errors.Join(
				fmt.Errorf("participant %s has stale reservation identity %s", participant.NodeName, value),
				r.releaseParticipants(context.WithoutCancel(ctx), reserved, runName),
			)
		}

		before := node.DeepCopy()
		if node.Annotations == nil {
			node.Annotations = map[string]string{}
		}

		if node.Labels == nil {
			node.Labels = map[string]string{}
		}

		node.Annotations[ReservationLabel] = runName
		node.Labels[signalv1.ReservationNodeUIDLabel] = string(participant.NodeUID)
		node.Spec.Unschedulable = true

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
		if err := r.Get(ctx, client.ObjectKey{Name: participant.NodeName}, &node); err != nil {
			if !apierrors.IsNotFound(err) {
				errs = append(errs, err)
			}

			continue
		}

		if participant.NodeUID != "" && node.UID != participant.NodeUID {
			errs = append(errs, fmt.Errorf(
				"participant %s identity changed from %s to %s",
				participant.NodeName,
				participant.NodeUID,
				node.UID,
			))

			continue
		}

		if node.Annotations[ReservationLabel] != runName {
			continue
		}

		if value := node.Labels[signalv1.ReservationNodeUIDLabel]; value != string(participant.NodeUID) {
			errs = append(errs, fmt.Errorf(
				"participant %s reservation identity changed from %s to %s",
				participant.NodeName,
				participant.NodeUID,
				value,
			))

			continue
		}

		before := node.DeepCopy()
		node.Spec.Unschedulable = false
		delete(node.Annotations, ReservationLabel)
		delete(node.Labels, signalv1.ReservationNodeUIDLabel)

		if err := r.Patch(ctx, &node, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func (r *AdmissionReconciler) createSnapshot(
	ctx context.Context,
	runName string,
	checks []signalv1.CertificationCheck,
) (*corev1.ConfigMap, []signalv1.CertificationCheckSnapshot, error) {
	immutable := true

	snapshot := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      runName + "-scripts",
			Namespace: r.Namespace,
			Labels:    map[string]string{SnapshotRunLabel: runName},
		},
		Immutable: &immutable,
		Data:      map[string]string{},
	}

	checkSnapshots := make([]signalv1.CertificationCheckSnapshot, len(checks))
	for index := range checks {
		check := &checks[index]

		var source corev1.ConfigMap
		if err := r.Get(
			ctx,
			client.ObjectKey{Namespace: r.ScriptNamespace, Name: check.Script.Name},
			&source,
		); err != nil {
			return nil, nil, fmt.Errorf("get source script ConfigMap for check %s: %w", check.ID, err)
		}

		content, ok := source.Data[check.Script.Key]
		if !ok {
			return nil, nil, fmt.Errorf(
				"source script ConfigMap %s has no key %s for check %s",
				source.Name,
				check.Script.Key,
				check.ID,
			)
		}

		digest := digestString(content)
		if check.Script.Digest != "" && digest != check.Script.Digest {
			return nil, nil, fmt.Errorf(
				"source script digest for check %s is %s, want %s",
				check.ID,
				digest,
				check.Script.Digest,
			)
		}

		key := fmt.Sprintf("%02d-%s", check.Order, sanitizeTaskName(string(check.ID)))

		snapshot.Data[key] = content
		checkSnapshots[index] = signalv1.CertificationCheckSnapshot{
			ID:    check.ID,
			Order: check.Order,
			Script: signalv1.ScriptSnapshot{
				Key:    key,
				Digest: digest,
			},
			RunnerImage:           check.RunnerImage,
			Backend:               check.Backend,
			WorkloadPolicy:        *check.WorkloadPolicy.DeepCopy(),
			GPUCount:              check.GPUCount,
			ScriptEnvironment:     maps.Clone(check.ScriptEnvironment),
			BackendActiveDeadline: check.BackendActiveDeadline,
			ParticipantPolicy:     check.ParticipantPolicy,
		}
	}

	if err := r.Create(ctx, snapshot); err != nil {
		return nil, nil, fmt.Errorf("create immutable script snapshot: %w", err)
	}

	return snapshot, checkSnapshots, nil
}

func (r *RunReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var run signalv1.CertificationRun
	if err := r.Get(ctx, req.NamespacedName, &run); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	hydrateRunDerivedFields(&run)

	if !run.DeletionTimestamp.IsZero() {
		return r.reconcileDeleting(ctx, &run)
	}

	if run.Status.Phase.Terminal() {
		if !containsString(run.Finalizers, RunFinalizer) {
			return ctrl.Result{}, nil
		}

		return r.cleanup(ctx, &run)
	}

	if err := r.verifyScriptBundle(ctx, &run); err != nil {
		return ctrl.Result{}, err
	}

	if run.Status.Evidence.URI == "" {
		result, recovered, err := r.resumeGroupedEvidence(ctx, &run)
		if err != nil || recovered {
			return result, err
		}
	}

	if run.Spec.Cancel {
		return r.cancel(ctx, &run)
	}

	participantsOwned, err := r.ensureParticipantsCordoned(ctx, &run)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !participantsOwned {
		return r.finishParticipantOwnershipLoss(ctx, &run)
	}

	if run.Annotations[RunAdmissionAnnotation] != runAdmissionReady {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	if run.Status.Workflow.Name == "" {
		return r.startWorkflow(ctx, &run)
	}

	workflow := &unstructured.Unstructured{}
	workflow.SetGroupVersionKind(workflowGVK)

	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}

	if err := reader.Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: run.Status.Workflow.Name}, workflow); err != nil {
		if apierrors.IsNotFound(err) {
			workflow.SetName(run.Status.Workflow.Name)
			workflow.Object["status"] = map[string]any{
				"phase":   "Error",
				"message": "workflow disappeared before terminal status",
			}

			return r.finishGrouped(ctx, &run, workflow, "Error")
		}

		return ctrl.Result{}, err
	}

	if !metav1.IsControlledBy(workflow, &run) {
		return ctrl.Result{}, fmt.Errorf(
			"workflow %s identity or ownership changed; refusing concurrent retry",
			workflow.GetName(),
		)
	}

	if run.Status.Workflow.UID == "" {
		return r.recordWorkflowStarted(ctx, &run, workflow)
	}

	if string(workflow.GetUID()) != run.Status.Workflow.UID {
		return ctrl.Result{}, fmt.Errorf(
			"workflow %s identity or ownership changed; refusing concurrent retry",
			workflow.GetName(),
		)
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

		checkStatuses := runningCheckStatuses(workflow, &run)

		if run.Status.Phase != signalv1.RunPhaseRunning ||
			run.Status.Workflow.Phase != phase ||
			!reflect.DeepEqual(run.Status.Checks, checkStatuses) {
			before := run.DeepCopy()
			run.Status.Phase = signalv1.RunPhaseRunning

			run.Status.Workflow.Phase = phase

			run.Status.Checks = checkStatuses
			if run.Status.StartedAt == nil {
				now := metav1.NewTime(r.now())
				run.Status.StartedAt = &now
			}

			return ctrl.Result{}, r.Status().Patch(ctx, &run, client.MergeFrom(before))
		}

		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	case "Succeeded":
		return r.finishGrouped(ctx, &run, workflow, phase)
	case "Failed", "Error":
		return r.finishGrouped(ctx, &run, workflow, phase)
	default:
		return ctrl.Result{}, fmt.Errorf("unsupported Workflow phase %q", phase)
	}
}

func (r *RunReconciler) verifyScriptBundle(ctx context.Context, run *signalv1.CertificationRun) error {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}

	var snapshot corev1.ConfigMap
	if err := reader.Get(
		ctx,
		client.ObjectKey{Namespace: run.Namespace, Name: run.Spec.ScriptBundle.Name},
		&snapshot,
	); err != nil {
		return fmt.Errorf("read immutable script bundle: %w", err)
	}

	if snapshot.UID != run.Spec.ScriptBundle.UID || snapshot.Labels[SnapshotRunLabel] != run.Name {
		return errors.New("script bundle identity does not match the run")
	}

	if snapshot.Immutable == nil || !*snapshot.Immutable {
		return errors.New("script bundle is not immutable")
	}

	for index := range run.Spec.Checks {
		check := &run.Spec.Checks[index]
		content, found := snapshot.Data[check.Script.Key]

		if !found {
			return fmt.Errorf("script bundle is missing check %s key %s", check.ID, check.Script.Key)
		}

		if digestString(content) != check.Script.Digest {
			return fmt.Errorf("script bundle digest does not match check %s", check.ID)
		}
	}

	return nil
}

func hydrateRunDerivedFields(run *signalv1.CertificationRun) {
	for index := range run.Spec.Participants {
		run.Spec.Participants[index].Index = int32(index)
	}

	for index := range run.Spec.Checks {
		run.Spec.Checks[index].ID = signalv1.CheckID(fmt.Sprintf("R%02d", index+1))
		run.Spec.Checks[index].Order = int32(index)
	}

	if len(run.Status.Checks) == len(run.Spec.Checks) {
		for index := range run.Status.Checks {
			run.Status.Checks[index].ID = run.Spec.Checks[index].ID
			run.Status.Checks[index].Order = run.Spec.Checks[index].Order
		}
	}
}

func (r *RunReconciler) ensureParticipantsCordoned(
	ctx context.Context,
	run *signalv1.CertificationRun,
) (bool, error) {
	allOwned := true

	for _, participant := range run.Spec.Participants {
		var node corev1.Node
		if err := r.Get(ctx, client.ObjectKey{Name: participant.NodeName}, &node); err != nil {
			if apierrors.IsNotFound(err) {
				allOwned = false

				continue
			}

			return false, fmt.Errorf("get cordoned participant %s: %w", participant.NodeName, err)
		}

		if node.UID != participant.NodeUID {
			allOwned = false

			continue
		}

		if node.Annotations[ReservationLabel] != run.Name ||
			node.Labels[signalv1.ReservationNodeUIDLabel] != string(participant.NodeUID) {
			allOwned = false

			continue
		}

		if node.Spec.Unschedulable {
			continue
		}

		before := node.DeepCopy()
		node.Spec.Unschedulable = true

		if err := r.Patch(
			ctx,
			&node,
			client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}),
		); err != nil {
			return false, fmt.Errorf("restore participant %s cordon: %w", participant.NodeName, err)
		}
	}

	status := metav1.ConditionTrue
	reason := "ParticipantsCordoned"
	message := "All participants are reserved and cordoned by this Run"

	if !allOwned {
		status = metav1.ConditionFalse
		reason = "ParticipantOwnershipLost"
		message = "One or more snapshotted participants are missing, replaced, or no longer owned by this Run"
	}

	if err := r.patchRunCondition(
		ctx,
		run,
		signalv1.RunConditionCordoned,
		status,
		reason,
		message,
	); err != nil {
		return false, err
	}

	return allOwned, nil
}

func (r *RunReconciler) finishParticipantOwnershipLoss(
	ctx context.Context,
	run *signalv1.CertificationRun,
) (ctrl.Result, error) {
	workflow, err := r.workflowForCancellation(ctx, run)
	if err != nil {
		return ctrl.Result{}, err
	}

	if workflow == nil {
		workflow = &unstructured.Unstructured{}
		workflow.SetGroupVersionKind(workflowGVK)
		workflow.SetNamespace(run.Namespace)
		workflow.SetName(fmt.Sprintf("%s-a1", run.Name))
	} else {
		workflow = workflow.DeepCopy()
	}

	if err := unstructured.SetNestedField(workflow.Object, "Error", "status", "phase"); err != nil {
		return ctrl.Result{}, err
	}

	if err := unstructured.SetNestedField(
		workflow.Object,
		"participant identity or reservation ownership changed",
		"status",
		"message",
	); err != nil {
		return ctrl.Result{}, err
	}

	return r.finishGrouped(ctx, run, workflow, "Error")
}

func (r *RunReconciler) patchRunCondition(
	ctx context.Context,
	run *signalv1.CertificationRun,
	conditionType string,
	status metav1.ConditionStatus,
	reason string,
	message string,
) error {
	before := run.DeepCopy()
	run.Status.ObservedGeneration = run.Generation
	apimeta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		ObservedGeneration: run.Generation,
		Reason:             reason,
		Message:            message,
	})

	if reflect.DeepEqual(before.Status, run.Status) {
		return nil
	}

	return r.Status().Patch(ctx, run, client.MergeFrom(before))
}

func (r *RunReconciler) startWorkflow(ctx context.Context, run *signalv1.CertificationRun) (ctrl.Result, error) {
	workflow, err := buildWorkflow(run)
	if err != nil {
		before := run.DeepCopy()
		now := metav1.NewTime(r.now())
		run.Status.Phase = signalv1.RunPhaseInconclusive
		run.Status.Reason = boundedStatusText("InvalidExecutionPlan: " + err.Error())
		run.Status.FinishedAt = &now

		return ctrl.Result{RequeueAfter: time.Millisecond}, r.Status().Patch(ctx, run, client.MergeFrom(before))
	}

	before := run.DeepCopy()
	run.Status.Workflow = signalv1.WorkflowStatus{
		Name:  workflow.GetName(),
		Phase: "Creating",
	}
	run.Status.Checks = initialCheckStatuses(run.Spec.Checks)

	if err := r.Status().Patch(ctx, run, client.MergeFrom(before)); err != nil {
		return ctrl.Result{}, fmt.Errorf("record Workflow creation intent: %w", err)
	}

	if err := r.Create(ctx, workflow); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, fmt.Errorf("create Workflow: %w", err)
		}

		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(workflowGVK)

		if getErr := r.Get(ctx, client.ObjectKeyFromObject(workflow), existing); getErr != nil {
			return ctrl.Result{}, fmt.Errorf("get existing Workflow: %w", getErr)
		}

		if !metav1.IsControlledBy(existing, run) {
			return ctrl.Result{}, errors.New("workflow already exists without matching CertificationRun ownership")
		}

		workflow = existing
	}

	return r.recordWorkflowStarted(ctx, run, workflow)
}

func (r *RunReconciler) recordWorkflowStarted(
	ctx context.Context,
	run *signalv1.CertificationRun,
	workflow *unstructured.Unstructured,
) (ctrl.Result, error) {
	before := run.DeepCopy()
	run.Status.Phase = signalv1.RunPhaseRunning

	run.Status.Workflow = signalv1.WorkflowStatus{
		Name:  workflow.GetName(),
		UID:   string(workflow.GetUID()),
		Phase: "Pending",
	}
	run.Status.Checks = initialCheckStatuses(run.Spec.Checks)

	if run.Status.StartedAt == nil {
		now := metav1.NewTime(r.now())
		run.Status.StartedAt = &now
	}

	if err := r.Status().Patch(ctx, run, client.MergeFrom(before)); err != nil {
		return ctrl.Result{}, fmt.Errorf("record Workflow identity: %w", err)
	}

	if r.Recorder != nil {
		r.Recorder.Eventf(run, nil, corev1.EventTypeNormal, "WorkflowStarted", "StartWorkflow",
			"Created Workflow %s", workflow.GetName())
	}

	return ctrl.Result{}, nil
}

func buildWorkflow(run *signalv1.CertificationRun) (*unstructured.Unstructured, error) {
	checks := run.Spec.Checks
	if err := validateRunCheckPlan(checks, run.Spec.Deadline.Duration); err != nil {
		return nil, err
	}

	name := run.Name + "-a1"
	tasks := make([]any, 0, len(checks))

	for index := range checks {
		check := &checks[index]

		checkParticipants, _, participantErr := participantsForCheck(
			run.Spec.Participants,
			check.ParticipantPolicy,
		)
		if participantErr != nil {
			return nil, fmt.Errorf("check %s participants: %w", check.ID, participantErr)
		}

		participantsJSON, marshalErr := json.Marshal(checkParticipants)
		if marshalErr != nil {
			return nil, fmt.Errorf("encode check %s participants: %w", check.ID, marshalErr)
		}

		environmentJSON, marshalErr := json.Marshal(check.ScriptEnvironment)
		if marshalErr != nil {
			return nil, fmt.Errorf("encode check %s script environment: %w", check.ID, marshalErr)
		}

		workloadPolicyJSON, encodeErr := signalworkload.Encode(check.WorkloadPolicy)
		if encodeErr != nil {
			return nil, fmt.Errorf("encode check %s workload policy: %w", check.ID, encodeErr)
		}

		taskName := checkTaskName(check)
		task := map[string]any{
			"name": taskName,
			"templateRef": map[string]any{
				"name":     adapterWorkflowTemplateName,
				"template": "certification",
			},
			"arguments": map[string]any{
				"parameters": checkWorkflowParameters(
					run,
					check,
					checkParticipants,
					string(participantsJSON),
					string(environmentJSON),
					workloadPolicyJSON,
					strconv.FormatInt(int64(check.GPUCount), 10),
				),
			},
		}

		if index > 0 {
			previous := checkTaskName(&checks[index-1])
			task["depends"] = previous + ".Succeeded"
		}

		tasks = append(tasks, task)
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
			"serviceAccountName":    adapterServiceAccountName,
			"entrypoint":            "certification-suite",
			"templates": []any{
				map[string]any{
					"name": "certification-suite",
					"dag": map[string]any{
						"failFast": false,
						"tasks":    tasks,
					},
				},
			},
		},
	}}
	workflow.SetOwnerReferences([]metav1.OwnerReference{
		*metav1.NewControllerRef(run, signalv1.GroupVersion.WithKind("CertificationRun")),
	})

	return workflow, nil
}

func checkWorkflowParameters(
	run *signalv1.CertificationRun,
	check *signalv1.CertificationCheckSnapshot,
	participants []signalv1.Participant,
	participantsJSON string,
	scriptEnvironmentJSON string,
	workloadPolicyJSON string,
	gpuCount string,
) []any {
	return []any{
		map[string]any{"name": "run-name", "value": checkRunName(run.Name, check.ID)},
		map[string]any{"name": "run-uid", "value": string(run.UID)},
		map[string]any{"name": "participants-json", "value": participantsJSON},
		map[string]any{"name": "participant-count", "value": fmt.Sprintf("%d", len(participants))},
		map[string]any{"name": "script-configmap", "value": run.Spec.ScriptBundle.Name},
		map[string]any{"name": "script-key", "value": check.Script.Key},
		map[string]any{"name": "script-digest", "value": check.Script.Digest},
		map[string]any{"name": "backend-kind", "value": string(check.Backend)},
		map[string]any{
			"name":  "backend-active-deadline-seconds",
			"value": fmt.Sprintf("%d", int64(check.BackendActiveDeadline.Seconds())),
		},
		map[string]any{"name": "runner-image", "value": check.RunnerImage},
		map[string]any{"name": "script-environment", "value": scriptEnvironmentJSON},
		map[string]any{"name": "workload-policy", "value": workloadPolicyJSON},
		map[string]any{"name": "gpu-resource-count", "value": gpuCount},
	}
}

func validateGPUCount(gpuCount int32, backend signalv1.ExecutionBackend) error {
	if backend != signalv1.ExecutionBackendPod && backend != signalv1.ExecutionBackendMPIJob {
		return fmt.Errorf("unsupported backend %q", backend)
	}

	if gpuCount < 0 || (backend == signalv1.ExecutionBackendMPIJob && gpuCount == 0) {
		return errors.New("must be nonnegative and positive for MPIJob")
	}

	return nil
}

func validEnvironmentName(value string) bool {
	if value == "" || (value[0] != '_' && (value[0] < 'A' || value[0] > 'Z')) {
		return false
	}

	for index := 1; index < len(value); index++ {
		character := value[index]
		if (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') &&
			character != '_' {
			return false
		}
	}

	return true
}

func validateScriptEnvironment(environment map[string]string) error {
	if len(environment) > 32 {
		return errors.New("must not contain more than 32 entries")
	}

	keys := make([]string, 0, len(environment))
	for key := range environment {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	for _, name := range keys {
		if len(name) > 253 {
			return fmt.Errorf("name %q exceeds 253 bytes", name)
		}

		if !validEnvironmentName(name) {
			return fmt.Errorf("name %q is invalid", name)
		}

		if strings.HasPrefix(name, "PROJECT_SIGNAL_") {
			return fmt.Errorf("name %q is reserved", name)
		}

		if _, reserved := reservedScriptEnvironmentNames[name]; reserved {
			return fmt.Errorf("name %q is reserved", name)
		}

		if len(environment[name]) > 4096 {
			return fmt.Errorf("value for %q exceeds 4096 bytes", name)
		}
	}

	return nil
}

func (r *RunReconciler) finishGrouped(
	ctx context.Context,
	run *signalv1.CertificationRun,
	workflow *unstructured.Unstructured,
	workflowPhase string,
) (ctrl.Result, error) {
	checks, verdict, reason := terminalCheckStatuses(workflow, run)
	executionState := "Completed"

	if workflowPhase != "Succeeded" {
		executionState = workflowPhase
		verdict = "Inconclusive"

		if reason == "" {
			reason = "Workflow" + workflowPhase + ": " + workflowMessage(workflow)
		}
	}

	evidenceRun := run.DeepCopy()
	evidenceRun.Status.Checks = checks
	evidenceRun.Status.Reason = reason
	evidenceRun.Status.Phase = runPhaseForVerdict(verdict)
	evidenceRun.Status.Workflow.Phase = workflowPhase

	finishedAt := groupedFinishedAt(workflow, run)
	evidenceRun.Status.FinishedAt = &finishedAt

	evidence, err := r.evidenceDocument(
		ctx,
		evidenceRun,
		executionState,
		verdict,
		reason,
	)
	if err != nil {
		return ctrl.Result{}, err
	}

	uri, digest, err := r.Evidence.Put(ctx, evidencePath(run), evidence)
	if err != nil {
		return ctrl.Result{}, err
	}

	before := run.DeepCopy()
	run.Status = evidenceRun.Status
	run.Status.Checks = compactCheckStatuses(checks)
	run.Status.Evidence = signalv1.EvidenceStatus{URI: uri, Digest: digest}
	run.Status.Phase = runPhaseForVerdict(verdict)
	run.Status.Reason = boundedStatusText(reason)

	if err := r.Status().Patch(ctx, run, client.MergeFrom(before)); err != nil {
		return ctrl.Result{}, err
	}

	if err := projectTerminal(ctx, r.Client, run); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: time.Millisecond}, nil
}

func (r *RunReconciler) resumeGroupedEvidence(
	ctx context.Context,
	run *signalv1.CertificationRun,
) (ctrl.Result, bool, error) {
	data, uri, digest, found, err := r.Evidence.Get(ctx, evidencePath(run))
	if err != nil || !found {
		return ctrl.Result{}, false, err
	}

	var document struct {
		RunID          string                  `json:"runID"`
		RunName        string                  `json:"runName"`
		Workflow       signalv1.WorkflowStatus `json:"workflow"`
		CheckStatus    []evidenceCheckStatus   `json:"checkStatus"`
		ExecutionState string                  `json:"executionState"`
		Verdict        string                  `json:"verdict"`
		Reason         string                  `json:"reason"`
		Phase          signalv1.RunPhase       `json:"phase"`
		FinishedAt     string                  `json:"finishedAt"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return ctrl.Result{}, false, fmt.Errorf("decode persisted grouped evidence: %w", err)
	}

	if document.RunName != run.Name || document.RunID == "" || document.RunID != string(run.UID) {
		return ctrl.Result{}, false, fmt.Errorf("persisted grouped evidence identity does not match run %s", run.Name)
	}

	if document.Workflow.Name != "" {
		if document.Workflow.Name != fmt.Sprintf("%s-a1", run.Name) ||
			(run.Status.Workflow.UID != "" && document.Workflow.UID != run.Status.Workflow.UID) {
			return ctrl.Result{}, false, errors.New("persisted evidence Workflow identity does not match run")
		}

		neverObserved := document.Workflow.UID == "" &&
			((document.Phase == signalv1.RunPhaseInconclusive && document.Workflow.Phase == "Error") ||
				(document.Phase == signalv1.RunPhaseCancelled && document.Workflow.Phase == "Canceled"))
		if document.Workflow.UID == "" && !neverObserved {
			return ctrl.Result{}, false, errors.New("persisted evidence is missing the observed Workflow UID")
		}
	}

	if len(document.CheckStatus) != len(run.Spec.Checks) {
		return ctrl.Result{}, false, fmt.Errorf(
			"persisted grouped evidence contains %d check statuses, want %d",
			len(document.CheckStatus),
			len(run.Spec.Checks),
		)
	}

	for index := range run.Spec.Checks {
		if document.CheckStatus[index].ID != run.Spec.Checks[index].ID ||
			document.CheckStatus[index].Order != run.Spec.Checks[index].Order {
			return ctrl.Result{}, false, fmt.Errorf(
				"persisted grouped evidence check %d identity does not match run",
				index,
			)
		}
	}

	finishedAt, err := time.Parse(time.RFC3339Nano, document.FinishedAt)
	if err != nil {
		return ctrl.Result{}, false, fmt.Errorf("parse persisted grouped evidence finishedAt: %w", err)
	}

	before := run.DeepCopy()
	run.Status.Checks = compactEvidenceCheckStatuses(document.CheckStatus)
	run.Status.Reason = boundedStatusText(document.Reason)
	run.Status.Workflow = document.Workflow
	run.Status.Evidence = signalv1.EvidenceStatus{URI: uri, Digest: digest}

	if document.Phase == "" {
		run.Status.Phase = runPhaseForVerdict(document.Verdict)
	} else {
		if !document.Phase.Terminal() {
			return ctrl.Result{}, false, fmt.Errorf("persisted evidence contains nonterminal phase %q", document.Phase)
		}

		run.Status.Phase = document.Phase
	}

	value := metav1.NewTime(finishedAt)
	run.Status.FinishedAt = &value

	if err := r.Status().Patch(ctx, run, client.MergeFrom(before)); err != nil {
		return ctrl.Result{}, true, err
	}

	if err := projectTerminal(ctx, r.Client, run); err != nil {
		return ctrl.Result{}, true, err
	}

	return ctrl.Result{RequeueAfter: time.Millisecond}, true, nil
}

func runPhaseForVerdict(verdict string) signalv1.RunPhase {
	switch verdict {
	case "Pass":
		return signalv1.RunPhaseSucceeded
	case "Fail":
		return signalv1.RunPhaseFailed
	default:
		return signalv1.RunPhaseInconclusive
	}
}

func groupedFinishedAt(
	workflow *unstructured.Unstructured,
	run *signalv1.CertificationRun,
) metav1.Time {
	if value, found, err := unstructured.NestedString(workflow.Object, "status", "finishedAt"); err == nil && found {
		if parsed, parseErr := time.Parse(time.RFC3339, value); parseErr == nil {
			return metav1.NewTime(parsed)
		}
	}

	nodes, found, err := unstructured.NestedMap(workflow.Object, "status", "nodes")
	if err == nil && found {
		var latest time.Time

		for index := range run.Spec.Checks {
			node, count := exactCheckNode(nodes, workflow.GetName(), &run.Spec.Checks[index])
			if count != 1 {
				continue
			}

			parsed, parseErr := time.Parse(time.RFC3339, nodeString(node, "finishedAt"))
			if parseErr == nil && parsed.After(latest) {
				latest = parsed
			}
		}

		if !latest.IsZero() {
			return metav1.NewTime(latest)
		}
	}

	if run.Status.StartedAt != nil {
		return *run.Status.StartedAt.DeepCopy()
	}

	if !run.CreationTimestamp.IsZero() {
		return run.CreationTimestamp
	}

	return metav1.NewTime(time.Unix(0, 0).UTC())
}

func initialCheckStatuses(checks []signalv1.CertificationCheckSnapshot) []signalv1.CheckStatus {
	statuses := make([]signalv1.CheckStatus, len(checks))
	for index := range checks {
		statuses[index] = signalv1.CheckStatus{
			ID:    checks[index].ID,
			Order: checks[index].Order,
			Phase: signalv1.CheckPhasePending,
		}
	}

	return statuses
}

func compactCheckStatuses(statuses []signalv1.CheckStatus) []signalv1.CheckStatus {
	compacted := make([]signalv1.CheckStatus, len(statuses))
	for index := range statuses {
		compacted[index] = *statuses[index].DeepCopy()
		compacted[index].BackendEvidence.Content = ""
		compacted[index].Reason = boundedStatusText(compacted[index].Reason)
	}

	return compacted
}

func boundedStatusText(value string) string {
	runes := []rune(value)
	if len(runes) <= maximumStatusTextRunes {
		return value
	}

	return string(runes[:maximumStatusTextRunes])
}

func contentDigest(content string) string {
	sum := sha256.Sum256([]byte(content))

	return fmt.Sprintf("sha256:%x", sum)
}

func evidencePath(run *signalv1.CertificationRun) string {
	return run.Name + ".json"
}

func runningCheckStatuses(
	workflow *unstructured.Unstructured,
	run *signalv1.CertificationRun,
) []signalv1.CheckStatus {
	statuses := initialCheckStatuses(run.Spec.Checks)

	nodes, found, err := unstructured.NestedMap(workflow.Object, "status", "nodes")
	if err != nil || !found {
		return statuses
	}

	for index := range run.Spec.Checks {
		node, count := exactCheckNode(nodes, workflow.GetName(), &run.Spec.Checks[index])
		if count != 1 {
			continue
		}

		statuses[index].WorkflowNodeName = nodeString(node, "name")
		phase := nodeString(node, "phase")

		switch phase {
		case "Running":
			statuses[index].Phase = signalv1.CheckPhaseRunning
		case "Succeeded":
			statuses[index].Phase = signalv1.CheckPhaseSucceeded
		case "Failed", "Error":
			statuses[index].Phase = signalv1.CheckPhaseInconclusive
		case "Omitted", "Skipped":
			statuses[index].Phase = signalv1.CheckPhaseSkipped
		}

		setCheckTimes(&statuses[index], node)
	}

	return statuses
}

func terminalCheckStatuses(
	workflow *unstructured.Unstructured,
	run *signalv1.CertificationRun,
) ([]signalv1.CheckStatus, string, string) {
	statuses := initialCheckStatuses(run.Spec.Checks)

	nodes, found, err := unstructured.NestedMap(workflow.Object, "status", "nodes")
	if err != nil || !found {
		for index := range statuses {
			statuses[index].Phase = signalv1.CheckPhaseInconclusive
			statuses[index].Verdict = "Inconclusive"
			statuses[index].Reason = "WorkflowNodesUnavailable"
		}

		return statuses, "Inconclusive", "workflow task nodes are missing or malformed"
	}

	aggregateVerdict := "Pass"
	aggregateReason := "AllChecksPassed"

	for index := range run.Spec.Checks {
		check := &run.Spec.Checks[index]
		status := &statuses[index]
		node, count := exactCheckNode(nodes, workflow.GetName(), check)

		if count != 1 {
			status.Phase = signalv1.CheckPhaseInconclusive
			status.Verdict = "Inconclusive"
			status.Reason = "WorkflowTaskIdentityInvalid"
			aggregateVerdict = "Inconclusive"
			aggregateReason = "workflow did not contain exactly one task node for every check"

			continue
		}

		status.WorkflowNodeName = nodeString(node, "name")
		setCheckTimes(status, node)

		phase := nodeString(node, "phase")
		switch phase {
		case "Omitted", "Skipped":
			status.Phase = signalv1.CheckPhaseSkipped
			status.Verdict = "Inconclusive"
			status.Reason = "NotRun"

			aggregateVerdict = "Inconclusive"
			aggregateReason = "one or more checks were not run"

			continue
		case "Failed", "Error":
			status.Phase = signalv1.CheckPhaseInconclusive
			status.Verdict = "Inconclusive"
			status.Reason = "WorkflowTask" + phase
			aggregateVerdict = "Inconclusive"
			aggregateReason = "one or more check tasks failed at the workflow boundary"

			continue
		case "Succeeded":
		default:
			status.Phase = signalv1.CheckPhaseInconclusive
			status.Verdict = "Inconclusive"
			status.Reason = "WorkflowTaskIncomplete"
			aggregateVerdict = "Inconclusive"
			aggregateReason = "one or more check tasks were incomplete"

			continue
		}

		outputs, outputErr := nodeWorkflowParameters(node)
		if outputErr != nil {
			status.Phase = signalv1.CheckPhaseInconclusive
			status.Verdict = "Inconclusive"
			status.Reason = "OutputsRejected: " + outputErr.Error()
			aggregateVerdict = "Inconclusive"
			aggregateReason = "one or more check outputs were malformed or incomplete"

			continue
		}

		status.Verdict = outputs["verdict"]
		status.Reason = outputs["reason"]
		status.Measurements = outputs["measurements"]
		allowPartialBackendEvidence := status.Verdict == "Inconclusive" &&
			status.Reason == "BackendSetupFailed"

		_, globalIndexes, participantErr := participantsForCheck(
			run.Spec.Participants,
			check.ParticipantPolicy,
		)
		if participantErr != nil {
			status.Phase = signalv1.CheckPhaseInconclusive
			status.Verdict = "Inconclusive"
			status.Reason = "ParticipantPolicyInvalid: " + participantErr.Error()
			aggregateVerdict = "Inconclusive"
			aggregateReason = "one or more checks had invalid participant policy"

			continue
		}

		normalizedEvidence, complete, evidenceErr := groupedBackendEvidenceForParticipants(
			outputs["backend-evidence"],
			globalIndexes,
			outputs["backend-resources"],
			check.Backend,
			run.Namespace,
			allowPartialBackendEvidence,
		)
		if evidenceErr != nil {
			status.Phase = signalv1.CheckPhaseInconclusive
			status.Verdict = "Inconclusive"
			status.Reason = "BackendEvidenceRejected: " + evidenceErr.Error()
			aggregateVerdict = "Inconclusive"
			aggregateReason = "one or more checks had invalid backend evidence"

			continue
		}

		status.BackendEvidence = signalv1.BackendEvidenceStatus{
			Complete: complete,
			Digest:   contentDigest(normalizedEvidence),
			Content:  normalizedEvidence,
		}

		resources, resourceErr := backendResourceReferencesForParticipants(
			outputs["backend-resources"],
			globalIndexes,
			run.Namespace,
			allowPartialBackendEvidence,
		)
		if resourceErr != nil {
			status.Phase = signalv1.CheckPhaseInconclusive
			status.Verdict = "Inconclusive"
			status.Reason = "BackendResourcesRejected: " + resourceErr.Error()
			aggregateVerdict = "Inconclusive"
			aggregateReason = "one or more checks had invalid backend identities"

			continue
		}

		status.BackendResources = resources
		status.Cleanup.Complete = false
		status.Cleanup.Reason = "DeferredToController"

		if !complete || len(resources) == 0 {
			status.Phase = signalv1.CheckPhaseInconclusive
			status.Verdict = "Inconclusive"
			status.Reason = "EvidenceOrCleanupIncomplete"
			aggregateVerdict = "Inconclusive"
			aggregateReason = "one or more checks lacked complete evidence or backend coverage"

			continue
		}

		switch status.Verdict {
		case "Pass":
			status.Phase = signalv1.CheckPhaseSucceeded
		case "Fail":
			status.Phase = signalv1.CheckPhaseFailed

			if aggregateVerdict == "Pass" {
				aggregateVerdict = "Fail"
				aggregateReason = "OneOrMoreChecksFailed"
			}
		case "Inconclusive":
			status.Phase = signalv1.CheckPhaseInconclusive
			aggregateVerdict = "Inconclusive"
			aggregateReason = "one or more checks were inconclusive"
		default:
			status.Phase = signalv1.CheckPhaseInconclusive
			status.Verdict = "Inconclusive"
			status.Reason = "InvalidVerdict"
			aggregateVerdict = "Inconclusive"
			aggregateReason = "one or more checks returned an invalid verdict"
		}
	}

	return statuses, aggregateVerdict, aggregateReason
}

func exactCheckNode(
	nodes map[string]any,
	workflowName string,
	check *signalv1.CertificationCheckSnapshot,
) (map[string]any, int) {
	expected := workflowName + "." + checkTaskName(check)

	var match map[string]any

	count := 0

	for _, raw := range nodes {
		node, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		name, ok := node["name"].(string)
		if ok && name == expected {
			match = node
			count++
		}
	}

	return match, count
}

func nodeWorkflowParameters(node map[string]any) (map[string]string, error) {
	outputs, ok := node["outputs"].(map[string]any)
	if !ok {
		return nil, errors.New("task outputs are missing or malformed")
	}

	parameters, ok := outputs["parameters"].([]any)
	if !ok {
		return nil, errors.New("task output parameters are missing or malformed")
	}

	limits := map[string]int{
		"verdict":           workflowOutputLimits["verdict"],
		"reason":            workflowOutputLimits["reason"],
		"measurements":      workflowOutputLimits["measurements"],
		"backend-resources": workflowOutputLimits["backend-resources"],
		"backend-evidence":  workflowOutputLimits["backend-evidence"],
	}

	decoded, err := decodeWorkflowParametersWithLimits(parameters, limits)
	if err != nil {
		return nil, err
	}

	for _, required := range []string{
		"verdict",
		"reason",
		"measurements",
		"backend-resources",
		"backend-evidence",
	} {
		if _, exists := decoded[required]; !exists {
			return nil, fmt.Errorf("required parameter %q is missing", required)
		}
	}

	return decoded, nil
}

func setCheckTimes(status *signalv1.CheckStatus, node map[string]any) {
	started := nodeString(node, "startedAt")
	finished := nodeString(node, "finishedAt")

	if parsed, err := time.Parse(time.RFC3339, started); err == nil {
		value := metav1.NewTime(parsed)
		status.StartedAt = &value
	}

	if parsed, err := time.Parse(time.RFC3339, finished); err == nil {
		value := metav1.NewTime(parsed)
		status.FinishedAt = &value
	}
}

func nodeString(node map[string]any, key string) string {
	value, ok := node[key].(string)
	if !ok {
		return ""
	}

	return value
}

type evidenceBackendStatus struct {
	Complete bool            `json:"complete"`
	Digest   string          `json:"digest,omitempty"`
	Document json.RawMessage `json:"document,omitempty"`
}

type evidenceCleanupStatus struct {
	Complete bool   `json:"complete"`
	Reason   string `json:"reason,omitempty"`
}

type evidenceCheckStatus struct {
	ID               signalv1.CheckID                    `json:"id"`
	Order            int32                               `json:"order"`
	WorkflowNodeName string                              `json:"workflowNodeName,omitempty"`
	Phase            signalv1.CheckPhase                 `json:"phase"`
	Verdict          string                              `json:"verdict"`
	Reason           string                              `json:"reason,omitempty"`
	Measurements     string                              `json:"measurements,omitempty"`
	BackendResources []signalv1.BackendResourceReference `json:"backendResources,omitempty"`
	BackendEvidence  evidenceBackendStatus               `json:"backendEvidence"`
	Cleanup          evidenceCleanupStatus               `json:"cleanup"`
	StartedAt        *metav1.Time                        `json:"startedAt,omitempty"`
	FinishedAt       *metav1.Time                        `json:"finishedAt,omitempty"`
}

func evidenceCheckStatuses(statuses []signalv1.CheckStatus) ([]evidenceCheckStatus, error) {
	result := make([]evidenceCheckStatus, len(statuses))

	for index := range statuses {
		status := &statuses[index]

		var document json.RawMessage

		if status.BackendEvidence.Content != "" {
			document = json.RawMessage(status.BackendEvidence.Content)
			if !json.Valid(document) {
				return nil, fmt.Errorf("check %s backend evidence is not valid JSON", status.ID)
			}
		}

		result[index] = evidenceCheckStatus{
			ID:               status.ID,
			Order:            status.Order,
			WorkflowNodeName: status.WorkflowNodeName,
			Phase:            status.Phase,
			Verdict:          status.Verdict,
			Reason:           status.Reason,
			Measurements:     status.Measurements,
			BackendResources: status.BackendResources,
			BackendEvidence: evidenceBackendStatus{
				Complete: status.BackendEvidence.Complete,
				Digest:   status.BackendEvidence.Digest,
				Document: document,
			},
			Cleanup: evidenceCleanupStatus{
				Complete: status.Cleanup.Complete,
				Reason:   status.Cleanup.Reason,
			},
			StartedAt:  status.StartedAt,
			FinishedAt: status.FinishedAt,
		}
	}

	return result, nil
}

func compactEvidenceCheckStatuses(statuses []evidenceCheckStatus) []signalv1.CheckStatus {
	result := make([]signalv1.CheckStatus, len(statuses))
	for index := range statuses {
		status := &statuses[index]
		result[index] = signalv1.CheckStatus{
			ID:               status.ID,
			Order:            status.Order,
			WorkflowNodeName: status.WorkflowNodeName,
			Phase:            status.Phase,
			Verdict:          status.Verdict,
			Reason:           boundedStatusText(status.Reason),
			Measurements:     status.Measurements,
			BackendResources: status.BackendResources,
			BackendEvidence: signalv1.BackendEvidenceStatus{
				Complete: status.BackendEvidence.Complete,
				Digest:   status.BackendEvidence.Digest,
			},
			StartedAt:  status.StartedAt,
			FinishedAt: status.FinishedAt,
			Cleanup: signalv1.CheckCleanupStatus{
				Complete: status.Cleanup.Complete,
				Reason:   status.Cleanup.Reason,
			},
		}
	}

	return result
}

func (r *RunReconciler) evidenceDocument(
	ctx context.Context,
	run *signalv1.CertificationRun,
	executionState string,
	verdict string,
	reason string,
) ([]byte, error) {
	checks := run.Spec.Checks
	if len(checks) == 0 {
		return nil, errors.New("run has no certification checks")
	}

	var snapshot corev1.ConfigMap
	if err := r.Get(
		ctx,
		client.ObjectKey{Namespace: run.Namespace, Name: run.Spec.ScriptBundle.Name},
		&snapshot,
	); err != nil {
		return nil, fmt.Errorf("read script snapshot for evidence: %w", err)
	}

	if snapshot.UID != run.Spec.ScriptBundle.UID || snapshot.Labels[SnapshotRunLabel] != run.Name {
		return nil, errors.New("script bundle identity does not match the run")
	}

	scriptContents := make(map[string]string, len(checks))
	for index := range checks {
		content, exists := snapshot.Data[checks[index].Script.Key]
		if !exists {
			return nil, fmt.Errorf("script snapshot is missing check %s key %s", checks[index].ID, checks[index].Script.Key)
		}

		scriptContents[string(checks[index].ID)] = content
	}

	checkStatuses, err := evidenceCheckStatuses(run.Status.Checks)
	if err != nil {
		return nil, err
	}

	doc := map[string]any{
		"apiVersion":     "signal.unbounded-cloud.io/evidence/v1alpha1",
		"runID":          string(run.UID),
		"runName":        run.Name,
		"profile":        run.Spec.Profile,
		"participants":   run.Spec.Participants,
		"scriptBundle":   run.Spec.ScriptBundle,
		"workflow":       run.Status.Workflow,
		"checks":         checks,
		"checkStatus":    checkStatuses,
		"scriptContents": scriptContents,
		"executionState": executionState,
		"verdict":        verdict,
		"reason":         reason,
		"phase":          run.Status.Phase,
		"startedAt":      run.Status.StartedAt,
		"finishedAt":     run.Status.FinishedAt.UTC().Format(time.RFC3339Nano),
	}

	return json.MarshalIndent(doc, "", "  ")
}

func backendResourceReferences(
	value string,
	participantCount int,
	_ string,
	allowPartial bool,
) ([]signalv1.BackendResourceReference, error) {
	if participantCount < 1 || participantCount > 64 {
		return nil, fmt.Errorf("participant count must be between 1 and 64")
	}

	if value == "" {
		return nil, nil
	}

	var references []signalv1.BackendResourceReference

	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&references); err != nil {
		return nil, fmt.Errorf("decode backend-resources: %w", err)
	}

	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("backend-resources contains trailing JSON values")
	}

	if len(references) == 0 || len(references) > 64 {
		return nil, fmt.Errorf("backend-resources must contain between 1 and 64 entries")
	}

	identities := make(map[string]struct{}, len(references))
	covered := make([]bool, participantCount)

	for index := range references {
		reference := &references[index]
		if reference.Name == "" || reference.UID == "" {
			return nil, fmt.Errorf("backend-resources[%d] has an incomplete identity", index)
		}

		identity := strings.Join([]string{
			reference.Name,
			string(reference.UID),
		}, "\x00")
		if _, exists := identities[identity]; exists {
			return nil, fmt.Errorf("backend-resources[%d] duplicates a workload identity", index)
		}

		identities[identity] = struct{}{}

		if len(reference.ParticipantIndexes) == 0 {
			return nil, fmt.Errorf("backend-resources[%d] does not identify a participant", index)
		}

		seenIndexes := make(map[int32]struct{}, len(reference.ParticipantIndexes))
		for _, participantIndex := range reference.ParticipantIndexes {
			if participantIndex < 0 || int(participantIndex) >= participantCount {
				return nil, fmt.Errorf(
					"backend-resources[%d] participant index %d is out of range",
					index,
					participantIndex,
				)
			}

			if _, exists := seenIndexes[participantIndex]; exists {
				return nil, fmt.Errorf(
					"backend-resources[%d] repeats participant index %d",
					index,
					participantIndex,
				)
			}

			seenIndexes[participantIndex] = struct{}{}
			covered[participantIndex] = true
		}
	}

	if !allowPartial && participantCount > 1 {
		for index, isCovered := range covered {
			if !isCovered {
				return nil, fmt.Errorf("backend-resources does not cover participant index %d", index)
			}
		}
	}

	return references, nil
}

func backendResourceReferencesForParticipants(
	value string,
	globalIndexes []int32,
	namespace string,
	allowPartial bool,
) ([]signalv1.BackendResourceReference, error) {
	references, err := backendResourceReferences(value, len(globalIndexes), namespace, allowPartial)
	if err != nil {
		return nil, err
	}

	for referenceIndex := range references {
		for participantIndex := range references[referenceIndex].ParticipantIndexes {
			localIndex := references[referenceIndex].ParticipantIndexes[participantIndex]
			references[referenceIndex].ParticipantIndexes[participantIndex] = globalIndexes[localIndex]
		}
	}

	return references, nil
}

func decodeWorkflowParametersWithLimits(parameters []any, limits map[string]int) (map[string]string, error) {
	outputs := make(map[string]string, len(parameters))
	for _, raw := range parameters {
		parameter, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("parameter entry is not an object")
		}

		name, nameOK := parameter["name"].(string)
		value, valueOK := parameter["value"].(string)

		if !nameOK || !valueOK {
			return nil, fmt.Errorf("parameter name and value must be strings")
		}

		limit, allowed := limits[name]
		if !allowed {
			return nil, fmt.Errorf("unexpected parameter %q", name)
		}

		if len(value) > limit {
			return nil, fmt.Errorf("parameter %q exceeds %d bytes", name, limit)
		}

		if _, duplicate := outputs[name]; duplicate {
			return nil, fmt.Errorf("duplicate parameter %q", name)
		}

		outputs[name] = value
	}

	return outputs, nil
}

func groupedBackendEvidenceComplete(
	content string,
	participantCount int,
	references []signalv1.BackendResourceReference,
	backend signalv1.ExecutionBackend,
	namespace string,
	allowPartial bool,
) (bool, error) {
	if err := signaladapter.ValidateBackendEvidence(content); err != nil {
		return false, err
	}

	var evidence signaladapter.BackendEvidence
	if err := json.Unmarshal([]byte(content), &evidence); err != nil {
		return false, err
	}

	if len(evidence.Participants) > participantCount || (!allowPartial && len(evidence.Participants) != participantCount) {
		return false, fmt.Errorf(
			"backend-evidence contains %d participants, want at most %d",
			len(evidence.Participants),
			participantCount,
		)
	}

	seen := make([]bool, participantCount)
	complete := !allowPartial && evidence.Complete && evidence.CaptureError == ""
	podUIDs := map[types.UID]struct{}{}

	expectedAPIVersion, expectedKind, err := backendIdentity(backend)
	if err != nil {
		return false, err
	}

	for index := range evidence.Participants {
		participant := &evidence.Participants[index]
		if participant.ParticipantIndex < 0 || participant.ParticipantIndex >= participantCount {
			return false, fmt.Errorf(
				"backend-evidence participant index %d is out of range",
				participant.ParticipantIndex,
			)
		}

		if seen[participant.ParticipantIndex] {
			return false, fmt.Errorf(
				"backend-evidence repeats participant index %d",
				participant.ParticipantIndex,
			)
		}

		seen[participant.ParticipantIndex] = true

		if participant.Resource.APIVersion == "" ||
			participant.Resource.Kind == "" ||
			participant.Resource.Name == "" ||
			participant.Resource.UID == "" {
			return false, fmt.Errorf(
				"backend-evidence participant %d has incomplete resource identity",
				participant.ParticipantIndex,
			)
		}

		matches := 0

		for _, reference := range references {
			if !slices.Contains(reference.ParticipantIndexes, int32(participant.ParticipantIndex)) {
				continue
			}

			if reference.Name != participant.Resource.Name ||
				reference.UID != participant.Resource.UID {
				return false, fmt.Errorf("backend-evidence participant %d resource differs from backend-resources", participant.ParticipantIndex)
			}

			matches++
		}

		if matches != 1 || participant.Resource.APIVersion != expectedAPIVersion ||
			participant.Resource.Kind != expectedKind || participant.Resource.Namespace != namespace {
			return false, fmt.Errorf("backend-evidence participant %d does not match the declared backend", participant.ParticipantIndex)
		}

		complete = complete && participant.CaptureError == "" && len(participant.Pods) > 0
		launcher := false

		for _, pod := range participant.Pods {
			if pod.UID == "" || pod.Name == "" {
				return false, errors.New("backend-evidence Pod identity is incomplete")
			}

			if _, duplicate := podUIDs[pod.UID]; duplicate {
				return false, errors.New("backend-evidence repeats a Pod UID")
			}

			podUIDs[pod.UID] = struct{}{}
			if backend == signalv1.ExecutionBackendPod && (pod.UID != participant.Resource.UID ||
				pod.Name != participant.Resource.Name || pod.Role != "participant" || len(participant.Pods) != 1) {
				return false, errors.New("backend-evidence Pod differs from its backend resource")
			}

			launcher = launcher || pod.Role == "launcher"

			complete = complete && pod.Phase != "" && len(pod.Containers) > 0
			for _, container := range pod.Containers {
				complete = complete && container.Name != "" && container.State.State != "" &&
					container.CurrentLog.Status == "captured" && container.CurrentLog.Error == "" &&
					(container.PreviousLog.Status == "captured" || container.PreviousLog.Status == "not-applicable") &&
					container.PreviousLog.Error == ""
				if container.RestartCount > 0 || container.LastState.State == "terminated" {
					complete = complete && container.PreviousLog.Status == "captured"
				}
			}
		}

		if backend == signalv1.ExecutionBackendMPIJob {
			complete = complete && launcher && participant.LauncherJob != nil &&
				participant.LauncherJob.Name != "" && participant.LauncherJob.UID != ""
		}
	}

	return complete, nil
}

func backendIdentity(backend signalv1.ExecutionBackend) (string, string, error) {
	switch backend {
	case signalv1.ExecutionBackendPod:
		return "v1", "Pod", nil
	case signalv1.ExecutionBackendMPIJob:
		return "kubeflow.org/v2beta1", "MPIJob", nil
	default:
		return "", "", fmt.Errorf("unsupported backend %q", backend)
	}
}

func groupedBackendEvidenceForParticipants(
	content string,
	globalIndexes []int32,
	resources string,
	backend signalv1.ExecutionBackend,
	namespace string,
	allowPartial bool,
) (string, bool, error) {
	references, err := backendResourceReferences(resources, len(globalIndexes), namespace, allowPartial)
	if err != nil {
		return "", false, err
	}

	complete, err := groupedBackendEvidenceComplete(
		content,
		len(globalIndexes),
		references,
		backend,
		namespace,
		allowPartial,
	)
	if err != nil {
		return "", false, err
	}

	var evidence map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &evidence); err != nil {
		return "", false, fmt.Errorf("decode backend-evidence for participant mapping: %w", err)
	}

	var participants []map[string]json.RawMessage
	if err := json.Unmarshal(evidence["participants"], &participants); err != nil {
		return "", false, fmt.Errorf("decode backend-evidence participants for mapping: %w", err)
	}

	for index := range participants {
		var localIndex int
		if err := json.Unmarshal(participants[index]["participantIndex"], &localIndex); err != nil {
			return "", false, fmt.Errorf("decode backend-evidence participant index: %w", err)
		}

		mapped, err := json.Marshal(globalIndexes[localIndex])
		if err != nil {
			return "", false, fmt.Errorf("encode mapped backend-evidence participant index: %w", err)
		}

		participants[index]["participantIndex"] = mapped
	}

	mappedParticipants, err := json.Marshal(participants)
	if err != nil {
		return "", false, fmt.Errorf("encode mapped backend-evidence participants: %w", err)
	}

	evidence["participants"] = mappedParticipants

	normalized, err := json.Marshal(evidence)
	if err != nil {
		return "", false, fmt.Errorf("encode mapped backend-evidence: %w", err)
	}

	return string(normalized), complete, nil
}

func (r *RunReconciler) cancel(ctx context.Context, run *signalv1.CertificationRun) (ctrl.Result, error) {
	evidenceRun := run.DeepCopy()
	evidenceRun.Status.Phase = signalv1.RunPhaseCancelled
	evidenceRun.Status.Reason = "canceled by operator"

	if !run.DeletionTimestamp.IsZero() {
		evidenceRun.Status.Reason = "canceled by deletion"
	}

	evidenceRun.Status.Workflow.Phase = "Canceled"

	statuses := initialCheckStatuses(run.Spec.Checks)

	workflow, err := r.workflowForCancellation(ctx, run)
	if err != nil {
		return ctrl.Result{}, err
	}

	if workflow != nil {
		evidenceRun.Status.Workflow.Name = workflow.GetName()
		evidenceRun.Status.Workflow.UID = string(workflow.GetUID())

		if phase, found, phaseErr := unstructured.NestedString(workflow.Object, "status", "phase"); phaseErr != nil {
			return ctrl.Result{}, fmt.Errorf("read Workflow phase during cancellation: %w", phaseErr)
		} else if found {
			evidenceRun.Status.Workflow.Phase = phase
		}

		completed, _, _ := terminalCheckStatuses(workflow, run)
		nodes, found, nodesErr := unstructured.NestedMap(workflow.Object, "status", "nodes")

		if nodesErr != nil {
			return ctrl.Result{}, fmt.Errorf("read Workflow nodes during cancellation: %w", nodesErr)
		}

		if found {
			for index := range run.Spec.Checks {
				node, count := exactCheckNode(nodes, workflow.GetName(), &run.Spec.Checks[index])

				if count == 1 && nodeString(node, "phase") == "Succeeded" {
					statuses[index] = completed[index]
				}
			}
		}
	}

	for index := range statuses {
		if statuses[index].Phase == signalv1.CheckPhaseSucceeded ||
			statuses[index].Phase == signalv1.CheckPhaseFailed ||
			statuses[index].Phase == signalv1.CheckPhaseInconclusive {
			continue
		}

		statuses[index].Phase = signalv1.CheckPhaseSkipped
		statuses[index].Verdict = "Inconclusive"
		statuses[index].Reason = "Canceled"
	}

	evidenceRun.Status.Checks = statuses

	now := metav1.NewTime(r.now())
	evidenceRun.Status.FinishedAt = &now

	evidence, err := r.evidenceDocument(
		ctx,
		evidenceRun,
		"Interrupted",
		"Inconclusive",
		evidenceRun.Status.Reason,
	)
	if err != nil {
		return ctrl.Result{}, err
	}

	uri, digest, err := r.Evidence.Put(ctx, evidencePath(run), evidence)
	if err != nil {
		return ctrl.Result{}, err
	}

	before := run.DeepCopy()
	run.Status = evidenceRun.Status
	run.Status.Evidence = signalv1.EvidenceStatus{URI: uri, Digest: digest}

	if err := r.Status().Patch(ctx, run, client.MergeFrom(before)); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: time.Millisecond}, nil
}

func (r *RunReconciler) workflowForCancellation(
	ctx context.Context,
	run *signalv1.CertificationRun,
) (*unstructured.Unstructured, error) {
	name := run.Status.Workflow.Name
	if name == "" {
		name = fmt.Sprintf("%s-a1", run.Name)
	}

	workflow := &unstructured.Unstructured{}
	workflow.SetGroupVersionKind(workflowGVK)

	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}

	err := reader.Get(
		ctx,
		client.ObjectKey{Namespace: run.Namespace, Name: name},
		workflow,
	)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	if !metav1.IsControlledBy(workflow, run) ||
		(run.Status.Workflow.UID != "" && string(workflow.GetUID()) != run.Status.Workflow.UID) {
		return nil, errors.New("workflow identity changed during cancellation")
	}

	return workflow, nil
}

func (r *RunReconciler) cleanup(ctx context.Context, run *signalv1.CertificationRun) (ctrl.Result, error) {
	holdParticipants := run.Status.Phase != signalv1.RunPhaseSucceeded && !run.Spec.Release
	if holdParticipants {
		if _, err := r.ensureParticipantsCordoned(ctx, run); err != nil {
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

	if err := projectTerminal(ctx, r.Client, run); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.deleteScriptBundle(ctx, run); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.patchRunCondition(
		ctx,
		run,
		signalv1.RunConditionCleanup,
		metav1.ConditionTrue,
		"WorkloadsDeleted",
		"Workflow, backend descendants, and script bundle are deleted",
	); err != nil {
		return ctrl.Result{}, err
	}

	if holdParticipants {
		if err := r.patchRunCondition(
			ctx,
			run,
			signalv1.RunConditionReleased,
			metav1.ConditionFalse,
			"AwaitingExplicitRelease",
			"Participants remain reserved and cordoned until spec.release is true",
		); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if err := r.clearRequest(ctx, run); err != nil {
		return ctrl.Result{}, err
	}

	if _, err := r.releaseRunReservations(ctx, run); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.patchRunCondition(
		ctx,
		run,
		signalv1.RunConditionReleased,
		metav1.ConditionTrue,
		"ParticipantsReleased",
		"Participant reservations and Signal-owned cordons are released",
	); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.patchRunCondition(
		ctx,
		run,
		signalv1.RunConditionCordoned,
		metav1.ConditionFalse,
		"ParticipantsReleased",
		"Signal no longer owns participant cordons",
	); err != nil {
		return ctrl.Result{}, err
	}

	before := run.DeepCopy()

	run.Finalizers = removeString(run.Finalizers, RunFinalizer)
	if err := r.Patch(ctx, run, client.MergeFrom(before)); err != nil {
		return ctrl.Result{}, err
	}

	if r.Recorder != nil {
		r.Recorder.Eventf(run, nil, corev1.EventTypeNormal, "CertificationCleaned", "CleanCertification",
			"Deleted certification workloads and released participant reservations and cordons")
	}

	return ctrl.Result{}, nil
}

func (r *RunReconciler) deleteScriptBundle(ctx context.Context, run *signalv1.CertificationRun) error {
	snapshot := &corev1.ConfigMap{}

	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}

	err := reader.Get(
		ctx,
		client.ObjectKey{Namespace: run.Namespace, Name: run.Spec.ScriptBundle.Name},
		snapshot,
	)
	if apierrors.IsNotFound(err) {
		return nil
	}

	if err != nil {
		return err
	}

	if snapshot.UID != run.Spec.ScriptBundle.UID || snapshot.Labels[SnapshotRunLabel] != run.Name {
		return errors.New("refusing to delete script bundle with changed identity")
	}

	uid := snapshot.UID

	return client.IgnoreNotFound(r.Delete(ctx, snapshot, client.Preconditions{UID: &uid}))
}

func (r *RunReconciler) reconcileDeleting(ctx context.Context, run *signalv1.CertificationRun) (ctrl.Result, error) {
	if !containsString(run.Finalizers, RunFinalizer) {
		return ctrl.Result{}, nil
	}

	if !run.Status.Phase.Terminal() && run.Status.Evidence.URI == "" {
		result, recovered, err := r.resumeGroupedEvidence(ctx, run)
		if err != nil || recovered {
			return result, err
		}
	}

	if !run.Status.Phase.Terminal() && run.Status.Workflow.Name != "" {
		return r.cancel(ctx, run)
	}

	stopped, err := r.ensureWorkflowStopped(ctx, run)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !stopped {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	if err := r.clearRequest(ctx, run); err != nil {
		return ctrl.Result{}, err
	}

	if _, err := r.releaseRunReservations(ctx, run); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.deleteScriptBundle(ctx, run); err != nil {
		return ctrl.Result{}, err
	}

	before := run.DeepCopy()
	run.Finalizers = removeString(run.Finalizers, RunFinalizer)

	return ctrl.Result{}, r.Patch(ctx, run, client.MergeFrom(before))
}

func (r *RunReconciler) releaseRunReservations(ctx context.Context, run *signalv1.CertificationRun) (bool, error) {
	for _, participant := range run.Spec.Participants {
		var node corev1.Node
		if err := r.Get(ctx, client.ObjectKey{Name: participant.NodeName}, &node); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}

			return false, err
		}

		if participant.NodeUID != "" && node.UID != participant.NodeUID {
			continue
		}

		if node.Annotations[ReservationLabel] != run.Name {
			continue
		}

		if node.Labels[signalv1.ReservationNodeUIDLabel] != string(participant.NodeUID) {
			continue
		}

		before := node.DeepCopy()
		node.Spec.Unschedulable = false
		delete(node.Annotations, ReservationLabel)
		delete(node.Labels, signalv1.ReservationNodeUIDLabel)

		if err := r.Patch(ctx, &node, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return false, err
		}
	}

	return true, nil
}

func (r *RunReconciler) clearRequest(ctx context.Context, run *signalv1.CertificationRun) error {
	if len(run.Spec.Participants) == 0 {
		return errors.New("run has no requesting participant")
	}

	requestNode := run.Spec.Participants[0]

	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: requestNode.NodeName}, &node); err != nil {
		return client.IgnoreNotFound(err)
	}

	if node.UID != requestNode.NodeUID {
		return nil
	}

	if node.Labels[RequestLabel] != run.Spec.Profile.Name || node.Annotations[ReservationLabel] != run.Name {
		return nil
	}

	before := node.DeepCopy()
	delete(node.Labels, RequestLabel)

	return r.Patch(ctx, &node, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

func (r *RunReconciler) ensureWorkflowStopped(ctx context.Context, run *signalv1.CertificationRun) (bool, error) {
	name := run.Status.Workflow.Name
	if name == "" {
		name = fmt.Sprintf("%s-a1", run.Name)
	}

	workflow := &unstructured.Unstructured{}
	workflow.SetGroupVersionKind(workflowGVK)

	key := client.ObjectKey{Namespace: run.Namespace, Name: name}
	reader := r.APIReader

	if reader == nil {
		reader = r.Client
	}

	if err := reader.Get(ctx, key, workflow); err != nil {
		if apierrors.IsNotFound(err) {
			return r.runWorkloadsGone(ctx, reader, run)
		}

		return false, fmt.Errorf("get Workflow before deletion: %w", err)
	}

	if (run.Status.Workflow.UID != "" && string(workflow.GetUID()) != run.Status.Workflow.UID) ||
		!metav1.IsControlledBy(workflow, run) {
		return false, fmt.Errorf("refusing to delete Workflow with changed identity or ownership")
	}

	if err := r.fenceWorkflowDependents(ctx, reader, workflow, run); err != nil {
		return false, err
	}

	propagation := metav1.DeletePropagationForeground
	uid := workflow.GetUID()

	if err := r.Delete(
		ctx,
		workflow,
		client.PropagationPolicy(propagation),
		client.Preconditions{UID: &uid},
	); err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("delete Workflow before releasing participants: %w", err)
	}

	if err := reader.Get(ctx, key, workflow); err != nil {
		if apierrors.IsNotFound(err) {
			return r.runWorkloadsGone(ctx, reader, run)
		}

		return false, fmt.Errorf("confirm Workflow deletion: %w", err)
	}

	return false, nil
}

func (r *RunReconciler) runWorkloadsGone(ctx context.Context, reader client.Reader, run *signalv1.CertificationRun) (bool, error) {
	for _, check := range run.Spec.Checks {
		for _, kind := range []schema.GroupVersionKind{
			corev1.SchemeGroupVersion.WithKind("Pod"),
			{Group: "batch", Version: "v1", Kind: "Job"},
			{Group: "kubeflow.org", Version: "v2beta1", Kind: "MPIJob"},
		} {
			if kind.Kind != "Pod" && check.Backend != signalv1.ExecutionBackendMPIJob {
				continue
			}

			list := &unstructured.UnstructuredList{}
			list.SetGroupVersionKind(kind.GroupVersion().WithKind(kind.Kind + "List"))

			if err := reader.List(ctx, list, client.InNamespace(run.Namespace),
				client.MatchingLabels{SnapshotRunLabel: checkRunName(run.Name, check.ID)}); err != nil {
				return false, fmt.Errorf("confirm %s cleanup: %w", kind.Kind, err)
			}

			if len(list.Items) > 0 {
				return false, nil
			}
		}
	}

	return true, nil
}

func (r *RunReconciler) fenceWorkflowDependents(
	ctx context.Context,
	reader client.Reader,
	workflow *unstructured.Unstructured,
	run *signalv1.CertificationRun,
) error {
	owners := map[types.UID]struct{}{workflow.GetUID(): {}}
	kinds := []schema.GroupVersionKind{}

	for _, check := range run.Spec.Checks {
		if check.Backend == signalv1.ExecutionBackendMPIJob {
			kinds = append(kinds, schema.GroupVersionKind{Group: "kubeflow.org", Version: "v2beta1", Kind: "MPIJob"})

			break
		}
	}

	kinds = append(
		kinds,
		schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"},
		corev1.SchemeGroupVersion.WithKind("Pod"),
	)

	// Fence every level before deleting the Workflow so foreground GC also waits
	// for existing backend descendants whose original owner reference did not block.
	for _, kind := range kinds {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(kind.GroupVersion().WithKind(kind.Kind + "List"))

		if err := reader.List(ctx, list, client.InNamespace(run.Namespace)); err != nil {
			return fmt.Errorf("list %s before Workflow cleanup: %w", kind.Kind, err)
		}

		for index := range list.Items {
			object := &list.Items[index]
			references := object.GetOwnerReferences()
			owned := false
			changed := false

			for ownerIndex := range references {
				if _, found := owners[references[ownerIndex].UID]; !found {
					continue
				}

				owned = true

				if references[ownerIndex].BlockOwnerDeletion == nil || !*references[ownerIndex].BlockOwnerDeletion {
					block := true
					references[ownerIndex].BlockOwnerDeletion = &block
					changed = true
				}
			}

			if !owned {
				continue
			}

			owners[object.GetUID()] = struct{}{}
			if changed {
				before := object.DeepCopy()
				object.SetOwnerReferences(references)

				if err := r.Patch(ctx, object, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
					return fmt.Errorf("fence %s %s before Workflow cleanup: %w", kind.Kind, object.GetName(), err)
				}
			}
		}
	}

	return nil
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
	return digestBytes([]byte(content))
}

func digestBytes(content []byte) string {
	sum := sha256.Sum256(content)

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

func profileChecks(profile *signalv1.CertificationProfile) []signalv1.CertificationCheck {
	checks := make([]signalv1.CertificationCheck, len(profile.Spec.Checks))
	for index := range profile.Spec.Checks {
		checks[index] = profile.Spec.Checks[index]
		checks[index].ID = signalv1.CheckID(fmt.Sprintf("R%02d", index+1))
		checks[index].Order = int32(index)
		checks[index].WorkloadPolicy = *profile.Spec.Checks[index].WorkloadPolicy.DeepCopy()
		checks[index].ScriptEnvironment = maps.Clone(profile.Spec.Checks[index].ScriptEnvironment)
		checks[index].ParticipantPolicy.Roles = slices.Clone(profile.Spec.Checks[index].ParticipantPolicy.Roles)
	}

	return checks
}

func validateCheckPlan(checks []signalv1.CertificationCheck, deadline time.Duration) error {
	if len(checks) != groupedCheckCount {
		return fmt.Errorf("must contain exactly %d checks", groupedCheckCount)
	}

	for index := range checks {
		check := &checks[index]
		if err := validateCheckParticipantPolicy(check.ID, check.ParticipantPolicy); err != nil {
			return err
		}

		deadlines := make([]time.Duration, len(checks))
		for index := range checks {
			deadlines[index] = checks[index].BackendActiveDeadline.Duration
		}

		if err := signalv1.ValidateSuiteDeadlines(deadlines, deadline); err != nil {
			return err
		}
	}

	if fixedWorkflowOutputBytes() > maximumWorkflowOutputBytes {
		return fmt.Errorf(
			"fixed aggregate check output budget is %d bytes, maximum is %d",
			fixedWorkflowOutputBytes(),
			maximumWorkflowOutputBytes,
		)
	}

	return nil
}

func validateRunCheckPlan(checks []signalv1.CertificationCheckSnapshot, deadline time.Duration) error {
	if len(checks) != groupedCheckCount {
		return fmt.Errorf("run must contain exactly %d checks", groupedCheckCount)
	}

	for index := range checks {
		check := &checks[index]
		check.ID = signalv1.CheckID(fmt.Sprintf("R%02d", index+1))
		check.Order = int32(index)
	}

	deadlines := make([]time.Duration, len(checks))
	for index := range checks {
		deadlines[index] = checks[index].BackendActiveDeadline.Duration
	}

	if err := signalv1.ValidateSuiteDeadlines(deadlines, deadline); err != nil {
		return err
	}

	if fixedWorkflowOutputBytes() > maximumWorkflowOutputBytes {
		return fmt.Errorf(
			"fixed run aggregate check output budget is %d bytes, maximum is %d",
			fixedWorkflowOutputBytes(),
			maximumWorkflowOutputBytes,
		)
	}

	return nil
}

func fixedWorkflowOutputBytes() int64 {
	perCheck := int64(
		workflowOutputLimits["verdict"] +
			workflowOutputLimits["reason"] +
			workflowOutputLimits["measurements"] +
			workflowOutputLimits["backend-resources"] +
			workflowOutputLimits["backend-evidence"],
	)

	return groupedCheckCount * perCheck
}

func validateCheckParticipantPolicy(id signalv1.CheckID, policy signalv1.CheckParticipantPolicy) error {
	roles := make(map[string]struct{}, len(policy.Roles))
	for _, role := range policy.Roles {
		if role == "" {
			return fmt.Errorf("check %s contains an empty participant role", id)
		}

		if _, duplicate := roles[role]; duplicate {
			return fmt.Errorf("check %s repeats participant role %q", id, role)
		}

		roles[role] = struct{}{}
	}

	return nil
}

func validateParticipantConstraints(
	profile *signalv1.CertificationProfile,
	checks []signalv1.CertificationCheck,
) error {
	roles := make(map[string]struct{}, len(profile.Spec.ParticipantRoles))

	var assigned int32

	for index := range profile.Spec.ParticipantRoles {
		role := &profile.Spec.ParticipantRoles[index]
		if role.Name == "" || role.Count < 1 {
			return fmt.Errorf("participant role %d has invalid name or count", index)
		}

		if _, duplicate := roles[role.Name]; duplicate {
			return fmt.Errorf("duplicate participant role %q", role.Name)
		}

		if _, err := metav1.LabelSelectorAsSelector(&role.Selector); err != nil {
			return fmt.Errorf("participant role %s selector: %w", role.Name, err)
		}

		roles[role.Name] = struct{}{}
		assigned += role.Count
	}

	if len(profile.Spec.ParticipantRoles) > 0 && assigned != profile.Spec.Participants.Count {
		return fmt.Errorf(
			"participant roles require exactly %d assignments, got %d",
			profile.Spec.Participants.Count,
			assigned,
		)
	}

	for index := range profile.Spec.TopologyConstraints {
		constraint := &profile.Spec.TopologyConstraints[index]
		if _, exists := roles[constraint.Role]; !exists {
			return fmt.Errorf("topology constraint references unknown role %q", constraint.Role)
		}

		if constraint.LabelKey == "" || constraint.MinDistinctValues < 1 {
			return fmt.Errorf("topology constraint %d has invalid label key or minimum", index)
		}
	}

	for index := range checks {
		for _, role := range checks[index].ParticipantPolicy.Roles {
			if _, exists := roles[role]; !exists {
				return fmt.Errorf("check %s references unknown participant role %q", checks[index].ID, role)
			}
		}
	}

	return nil
}

func participantsForCheck(
	participants []signalv1.Participant,
	policy signalv1.CheckParticipantPolicy,
) ([]signalv1.Participant, []int32, error) {
	included := make(map[string]struct{}, len(policy.Roles))
	for _, role := range policy.Roles {
		included[role] = struct{}{}
	}

	selected := make([]signalv1.Participant, 0, len(participants))

	globalIndexes := make([]int32, 0, len(participants))
	for index := range participants {
		participant := participants[index]
		_, include := included[participant.Role]
		include = len(included) == 0 || include

		if !include {
			continue
		}

		globalIndexes = append(globalIndexes, int32(index))
		participant.Index = int32(len(selected))
		selected = append(selected, participant)
	}

	if len(selected) == 0 {
		return nil, nil, errors.New("participant policy selected no participants")
	}

	return selected, globalIndexes, nil
}

func sanitizeTaskName(value string) string {
	value = strings.ToLower(value)
	value = strings.Map(func(character rune) rune {
		if character >= 'a' && character <= 'z' ||
			character >= '0' && character <= '9' ||
			character == '-' {
			return character
		}

		return '-'
	}, value)

	value = strings.Trim(value, "-")
	if value == "" {
		value = "check"
	}

	if len(value) > 48 {
		value = strings.TrimRight(value[:48], "-")
	}

	return value
}

func checkTaskName(check *signalv1.CertificationCheckSnapshot) string {
	return fmt.Sprintf("check-%02d-%s", check.Order, sanitizeTaskName(string(check.ID)))
}

func checkRunName(runName string, checkID signalv1.CheckID) string {
	suffix := sanitizeTaskName(string(checkID))

	candidate := runName + "-" + suffix
	if len(candidate) <= 63 {
		return candidate
	}

	sum := sha256.Sum256([]byte(candidate))
	hash := hex.EncodeToString(sum[:4])
	prefixLength := 63 - len(hash) - 1

	return strings.TrimRight(candidate[:prefixLength], "-") + "-" + hash
}

func AddToScheme(scheme *runtime.Scheme) error {
	return signalv1.AddToScheme(scheme)
}
