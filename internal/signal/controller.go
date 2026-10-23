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
	RequestLabel         = "signal.unbounded-cloud.io/certification-request"
	ReservationLabel     = "signal.unbounded-cloud.io/reserved-by"
	RunNodeLabel         = "signal.unbounded-cloud.io/request-node"
	RunProfileLabel      = "signal.unbounded-cloud.io/profile"
	RunTriggerAnnotation = "signal.unbounded-cloud.io/trigger"
	RunFinalizer         = "signal.unbounded-cloud.io/cleanup"
	SnapshotRunLabel     = "signal.unbounded-cloud.io/run"
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
	"backend-resources": 16 * 1024,
	"backend-evidence":  signaladapter.MaxBackendEvidenceBytes,
	"cleanup-failure":   64,
	"measurements":      16 * 1024,
	"reason":            4 * 1024,
	"verdict":           32,
}

const (
	groupedCheckCount         = 15
	adapterServiceAccountName = "project-signal-adapter"
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
		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, r.admitNode(ctx, &node, profileName)
}

func (r *AdmissionReconciler) admitNode(ctx context.Context, node *corev1.Node, profileName string) error {
	r.admissionMu.Lock()
	defer r.admissionMu.Unlock()

	if runName := node.Annotations[ReservationLabel]; runName != "" {
		var run signalv1.CertificationRun

		reader := r.APIReader
		if reader == nil {
			reader = r.Client
		}

		err := reader.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: runName}, &run)
		if err == nil {
			return nil
		}

		if !apierrors.IsNotFound(err) {
			return err
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
				return fmt.Errorf(
					"orphaned snapshot %s is not bound to reservation %q",
					snapshot.Name,
					runName,
				)
			}

			if err := r.Delete(ctx, &snapshot); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete orphaned script snapshot: %w", err)
			}
		} else if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get orphaned script snapshot: %w", err)
		}

		if err := r.releaseOrphanedReservations(ctx, runName); err != nil {
			return fmt.Errorf("release orphaned Project Signal reservations: %w", err)
		}
	}

	var profile signalv1.CertificationProfile
	if err := r.Get(ctx, client.ObjectKey{Name: profileName}, &profile); err != nil {
		return fmt.Errorf("get CertificationProfile %s: %w", profileName, err)
	}

	checks := profileChecks(&profile)
	if err := validateCheckPlan(checks); err != nil {
		return fmt.Errorf("profile %s checks: %w", profile.Name, err)
	}

	if err := validateParticipantConstraints(&profile, checks); err != nil {
		return fmt.Errorf("profile %s participant constraints: %w", profile.Name, err)
	}

	for index := range checks {
		check := &checks[index]
		if err := signalv1.ValidateRuntimeContract(
			check.Script.RequiresRuntimeStage,
			check.RuntimeStage,
			check.BackendActiveDeadline.Duration,
			profile.Spec.Deadline.Duration,
		); err != nil {
			return fmt.Errorf("profile %s check %s runtime contract: %w", profile.Name, check.ID, err)
		}

		if !runnerImageDigestPattern.MatchString(check.RunnerImage) {
			return fmt.Errorf("profile %s check %s runner image must use an immutable sha256 digest", profile.Name, check.ID)
		}

		if _, err := scriptEnvironment(check.Parameters); err != nil {
			return fmt.Errorf("profile %s check %s parameters: %w", profile.Name, check.ID, err)
		}

		if err := signalworkload.Validate(check.WorkloadPolicy); err != nil {
			return fmt.Errorf("profile %s check %s workload policy: %w", profile.Name, check.ID, err)
		}

		if check.WorkloadPolicy.KueueTopology != nil && check.Backend.Queue == "" {
			return fmt.Errorf("profile %s check %s workload policy kueueTopology requires a backend queue", profile.Name, check.ID)
		}
	}

	participants, err := r.selectParticipants(ctx, node, &profile)
	if err != nil {
		return err
	}

	for index := range checks {
		if _, _, err := participantsForCheck(participants, checks[index].ParticipantPolicy); err != nil {
			return fmt.Errorf("check %s participants: %w", checks[index].ID, err)
		}

		if _, err := signalworkload.Encode(checks[index].WorkloadPolicy); err != nil {
			return fmt.Errorf("check %s workload policy: %w", checks[index].ID, err)
		}

		if _, _, err := adapterParameterValues(checks[index].Parameters, checks[index].Backend.Kind); err != nil {
			return fmt.Errorf("check %s parameters: %w", checks[index].ID, err)
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

	if len(participants) == 1 {
		var participantNode corev1.Node

		reader := r.APIReader
		if reader == nil {
			reader = r.Client
		}

		if err := reader.Get(ctx, client.ObjectKey{Name: participants[0].NodeName}, &participantNode); err != nil {
			return fmt.Errorf("read admitted participant inventory: %w", err)
		}

		gpuResource := corev1.ResourceName("nvidia.com/gpu")
		gpuCapacity := participantNode.Status.Capacity[gpuResource]
		gpuAllocatable := participantNode.Status.Allocatable[gpuResource]

		for index := range checks {
			if checks[index].Parameters == nil {
				checks[index].Parameters = map[string]string{}
			}

			checks[index].Parameters["observed-gpu-capacity"] = gpuCapacity.String()
			checks[index].Parameters["observed-gpu-allocatable"] = gpuAllocatable.String()
		}
	}

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
				RunTriggerAnnotation: "manual",
			},
			Finalizers: []string{RunFinalizer},
		},
		Spec: signalv1.CertificationRunSpec{
			Profile: signalv1.PinnedProfile{
				Name:       profile.Name,
				UID:        profile.UID,
				Generation: profile.Generation,
			},
			Participants:        participants,
			ParticipantRoles:    append([]signalv1.ParticipantRoleConstraint(nil), profile.Spec.ParticipantRoles...),
			TopologyConstraints: append([]signalv1.ParticipantTopologyConstraint(nil), profile.Spec.TopologyConstraints...),
			Checks:              checkSnapshots,
			ExecutionPolicy:     profile.Spec.ExecutionPolicy,
			Workflow:            profile.Spec.Workflow,
			Purpose:             profile.Spec.Purpose,
			EvidencePath:        strings.Trim(profile.Spec.EvidencePath, "/") + "/" + runName + ".json",
			Deadline:            profile.Spec.Deadline,
			Tolerations:         profile.Spec.Tolerations,
			RequestNodeName:     node.Name,
		},
	}
	if _, err := buildWorkflow(run, 1); err != nil {
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

	if err := projectPending(ctx, r.Client, run); err != nil {
		return cleanupFailedAdmission(fmt.Errorf("project pending certification state: %w", err))
	}

	releaseOnFailure = false

	if r.Recorder != nil {
		r.Recorder.Eventf(run, nil, corev1.EventTypeNormal, "CertificationAdmitted", "AdmitCertification",
			"Created CertificationRun %s with %d immutable checks", run.Name, len(checkSnapshots))
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

	if !selector.Matches(labels.Set(requestNode.Labels)) || !nodeEligible(requestNode, profile.Spec.Participants.RequiredResources) {
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
			!nodeEligible(candidate, profile.Spec.Participants.RequiredResources) ||
			candidate.Annotations[ReservationLabel] != "" {
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
		Capacity:    node.Status.Capacity.DeepCopy(),
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
		participant.Roles = slices.Clone(assignments[candidateIndex])
		participants = append(participants, participant)
	}

	return participants, nil
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
		if err := r.Get(ctx, client.ObjectKey{Name: participant.NodeName}, &node); err != nil {
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
				ConfigMapName:        snapshot.Name,
				Key:                  key,
				RequiresRuntimeStage: check.Script.RequiresRuntimeStage,
				Digest:               digest,
			},
			RunnerImage:           check.RunnerImage,
			Backend:               check.Backend,
			WorkloadPolicy:        *check.WorkloadPolicy.DeepCopy(),
			RuntimeStage:          copyRuntimeStage(check.RuntimeStage),
			Parameters:            maps.Clone(check.Parameters),
			BackendActiveDeadline: check.BackendActiveDeadline,
			ResultBounds:          check.ResultBounds,
			ParticipantPolicy:     check.ParticipantPolicy,
			MaxAttempts:           check.MaxAttempts,
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

	if !run.DeletionTimestamp.IsZero() {
		return r.reconcileDeleting(ctx, &run)
	}

	if run.Status.Phase.Terminal() {
		if !containsString(run.Finalizers, RunFinalizer) {
			return ctrl.Result{}, nil
		}

		return r.cleanup(ctx, &run)
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

	if run.Status.Workflow.Name == "" {
		return r.startAttempt(ctx, &run, 1)
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
		return r.recordWorkflowStarted(ctx, &run, workflow, run.Status.Workflow.Attempt)
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

func (r *RunReconciler) startAttempt(ctx context.Context, run *signalv1.CertificationRun, attempt int32) (ctrl.Result, error) {
	workflow, err := buildWorkflow(run, attempt)
	if err != nil {
		before := run.DeepCopy()
		now := metav1.NewTime(r.now())
		run.Status.Phase = signalv1.RunPhaseInconclusive
		run.Status.ExecutionState = "Error"
		run.Status.Verdict = "Inconclusive"
		run.Status.Reason = "InvalidExecutionPlan: " + err.Error()
		run.Status.FinishedAt = &now

		return ctrl.Result{RequeueAfter: time.Millisecond}, r.Status().Patch(ctx, run, client.MergeFrom(before))
	}

	before := run.DeepCopy()
	run.Status.Workflow = signalv1.WorkflowStatus{
		Name:    workflow.GetName(),
		Attempt: attempt,
		Phase:   "Creating",
	}
	run.Status.Checks = initialCheckStatuses(run.Spec.Checks)

	if err := r.Status().Patch(ctx, run, client.MergeFrom(before)); err != nil {
		return ctrl.Result{}, fmt.Errorf("record Workflow creation intent: %w", err)
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

	return r.recordWorkflowStarted(ctx, run, workflow, attempt)
}

func (r *RunReconciler) recordWorkflowStarted(
	ctx context.Context,
	run *signalv1.CertificationRun,
	workflow *unstructured.Unstructured,
	attempt int32,
) (ctrl.Result, error) {
	before := run.DeepCopy()
	run.Status.Phase = signalv1.RunPhaseRunning
	run.Status.ExecutionState = "Running"

	run.Status.Workflow = signalv1.WorkflowStatus{
		Name:    workflow.GetName(),
		UID:     string(workflow.GetUID()),
		Attempt: attempt,
		Phase:   "Pending",
	}
	run.Status.Checks = initialCheckStatuses(run.Spec.Checks)

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
	checks := run.Spec.Checks
	if err := validateRunCheckPlan(checks); err != nil {
		return nil, err
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

		environment, environmentErr := scriptEnvironment(check.Parameters)
		if environmentErr != nil {
			return nil, fmt.Errorf("check %s: %w", check.ID, environmentErr)
		}

		environmentJSON, marshalErr := json.Marshal(environment)
		if marshalErr != nil {
			return nil, fmt.Errorf("encode check %s script environment: %w", check.ID, marshalErr)
		}

		workloadPolicyJSON, encodeErr := signalworkload.Encode(check.WorkloadPolicy)
		if encodeErr != nil {
			return nil, fmt.Errorf("encode check %s workload policy: %w", check.ID, encodeErr)
		}

		gpuCount, privileged, err := adapterParameterValues(check.Parameters, check.Backend.Kind)
		if err != nil {
			return nil, fmt.Errorf("check %s parameters: %w", check.ID, err)
		}

		taskName := checkTaskName(check)
		task := map[string]any{
			"name": taskName,
			"templateRef": map[string]any{
				"name":     run.Spec.Workflow.Name,
				"template": "certification",
			},
			"arguments": map[string]any{
				"parameters": checkWorkflowParameters(
					run,
					check,
					attempt,
					checkParticipants,
					string(participantsJSON),
					string(tolerationsJSON),
					string(environmentJSON),
					workloadPolicyJSON,
					gpuCount,
					privileged,
				),
			},
		}

		if index > 0 {
			previous := checkTaskName(&checks[index-1])
			if run.Spec.ExecutionPolicy.OnFailure == signalv1.SuiteFailurePolicyContinue {
				task["depends"] = fmt.Sprintf(
					"%[1]s.Succeeded || %[1]s.Failed || %[1]s.Errored || %[1]s.Skipped || %[1]s.Omitted",
					previous,
				)
			} else {
				task["depends"] = previous + ".Succeeded"
				task["when"] = fmt.Sprintf(
					"{{tasks.%s.outputs.parameters.verdict}} == Pass",
					previous,
				)
			}
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
			"ttlStrategy": map[string]any{
				"secondsAfterCompletion": int64(6 * time.Hour / time.Second),
				"secondsAfterSuccess":    int64(6 * time.Hour / time.Second),
				"secondsAfterFailure":    int64(6 * time.Hour / time.Second),
			},
			"podGC": map[string]any{
				"strategy":            "OnWorkflowCompletion",
				"deleteDelayDuration": "2h",
			},
			"entrypoint": "certification-suite",
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
	attempt int32,
	participants []signalv1.Participant,
	participantsJSON string,
	tolerationsJSON string,
	scriptEnvironmentJSON string,
	workloadPolicyJSON string,
	gpuCount string,
	privileged string,
) []any {
	return []any{
		map[string]any{"name": "run-name", "value": checkRunName(run.Name, check.ID)},
		map[string]any{"name": "run-uid", "value": string(run.UID)},
		map[string]any{"name": "participants-json", "value": participantsJSON},
		map[string]any{"name": "participant-count", "value": fmt.Sprintf("%d", len(participants))},
		map[string]any{"name": "script-configmap", "value": check.Script.ConfigMapName},
		map[string]any{"name": "script-key", "value": check.Script.Key},
		map[string]any{"name": "script-digest", "value": check.Script.Digest},
		map[string]any{
			"name":  "script-requires-runtime-stage",
			"value": fmt.Sprintf("%t", check.Script.RequiresRuntimeStage),
		},
		map[string]any{"name": "runtime-stage-name", "value": runtimeStageName(check.RuntimeStage)},
		map[string]any{"name": "backend-api-version", "value": check.Backend.APIVersion},
		map[string]any{"name": "backend-kind", "value": check.Backend.Kind},
		map[string]any{
			"name":  "backend-active-deadline-seconds",
			"value": fmt.Sprintf("%d", int64(check.BackendActiveDeadline.Seconds())),
		},
		map[string]any{"name": "queue", "value": check.Backend.Queue},
		map[string]any{"name": "runner-image", "value": check.RunnerImage},
		map[string]any{"name": "tolerations", "value": tolerationsJSON},
		map[string]any{"name": "script-environment", "value": scriptEnvironmentJSON},
		map[string]any{"name": "workload-policy", "value": workloadPolicyJSON},
		map[string]any{"name": "mpi-payload-mode", "value": check.Parameters["mpi-payload-mode"]},
		map[string]any{"name": "expected-gpu-product-regex", "value": check.Parameters["expected-gpu-product-regex"]},
		map[string]any{"name": "expected-gpu-count", "value": check.Parameters["expected-gpu-count"]},
		map[string]any{"name": "observed-gpu-capacity", "value": check.Parameters["observed-gpu-capacity"]},
		map[string]any{"name": "observed-gpu-allocatable", "value": check.Parameters["observed-gpu-allocatable"]},
		map[string]any{"name": "backend-evidence-bytes", "value": strconv.Itoa(int(check.ResultBounds.BackendEvidenceBytes))},
		map[string]any{"name": "gpu-resource-count", "value": gpuCount},
		map[string]any{"name": "privileged", "value": privileged},
		map[string]any{"name": "attempt", "value": fmt.Sprintf("%d", attempt)},
	}
}

func adapterParameterValues(parameters map[string]string, backendKind string) (string, string, error) {
	gpuCount := parameters["gpu-resource-count"]
	if gpuCount == "" {
		gpuCount = "0"
	}

	count, err := strconv.ParseInt(gpuCount, 10, 64)
	if err != nil || count < 0 || (backendKind == "MPIJob" && count == 0) {
		return "", "", errors.New("gpu-resource-count must be nonnegative and positive for MPIJob")
	}

	privileged := parameters["privileged"]
	if privileged == "" {
		privileged = "false"
	}

	value, err := strconv.ParseBool(privileged)
	if err != nil {
		return "", "", fmt.Errorf("privileged must be a boolean: %w", err)
	}

	return strconv.FormatInt(count, 10), strconv.FormatBool(value), nil
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

func scriptEnvironment(parameters map[string]string) (map[string]string, error) {
	keys := make([]string, 0, len(parameters))
	for key := range parameters {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	environment := make(map[string]string, len(parameters))
	parameterByEnvironment := make(map[string]string, len(parameters))

	for _, key := range keys {
		environmentName := strings.ToUpper(strings.ReplaceAll(key, "-", "_"))
		if !validEnvironmentName(environmentName) {
			return nil, fmt.Errorf("profile parameter %q cannot be mapped to a script environment variable", key)
		}

		if strings.HasPrefix(environmentName, "PROJECT_SIGNAL_") {
			return nil, fmt.Errorf("profile parameter %q maps to reserved environment variable %s", key, environmentName)
		}

		if _, reserved := reservedScriptEnvironmentNames[environmentName]; reserved {
			return nil, fmt.Errorf("profile parameter %q maps to reserved environment variable %s", key, environmentName)
		}

		if previous, exists := parameterByEnvironment[environmentName]; exists {
			return nil, fmt.Errorf("profile parameters %q and %q both map to %s", previous, key, environmentName)
		}

		parameterByEnvironment[environmentName] = key
		environment[environmentName] = parameters[key]
	}

	return environment, nil
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
	evidenceRun.Status.ExecutionState = executionState
	evidenceRun.Status.Verdict = verdict
	evidenceRun.Status.Reason = reason
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

	uri, digest, err := r.Evidence.Put(ctx, run.Spec.EvidencePath, evidence)
	if err != nil {
		return ctrl.Result{}, err
	}

	before := run.DeepCopy()
	run.Status = evidenceRun.Status
	run.Status.Checks = compactCheckStatuses(checks)
	run.Status.Evidence = signalv1.EvidenceStatus{URI: uri, Digest: digest}
	run.Status.Phase = runPhaseForVerdict(verdict)

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
	data, uri, digest, found, err := r.Evidence.Get(ctx, run.Spec.EvidencePath)
	if err != nil || !found {
		return ctrl.Result{}, false, err
	}

	var document struct {
		RunID          string                  `json:"runID"`
		RunName        string                  `json:"runName"`
		Workflow       signalv1.WorkflowStatus `json:"workflow"`
		CheckStatus    []signalv1.CheckStatus  `json:"checkStatus"`
		ExecutionState string                  `json:"executionState"`
		Verdict        string                  `json:"verdict"`
		Reason         string                  `json:"reason"`
		FinishedAt     string                  `json:"finishedAt"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return ctrl.Result{}, false, fmt.Errorf("decode persisted grouped evidence: %w", err)
	}

	if document.RunName != run.Name || document.RunID == "" || document.RunID != string(run.UID) {
		return ctrl.Result{}, false, fmt.Errorf("persisted grouped evidence identity does not match run %s", run.Name)
	}

	if document.Workflow.Name != "" &&
		(document.Workflow.Name != fmt.Sprintf("%s-a1", run.Name) || document.Workflow.UID == "" ||
			(run.Status.Workflow.UID != "" && document.Workflow.UID != run.Status.Workflow.UID)) {
		return ctrl.Result{}, false, errors.New("persisted evidence Workflow identity does not match run")
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
	run.Status.Checks = compactCheckStatuses(document.CheckStatus)
	run.Status.ExecutionState = document.ExecutionState
	run.Status.Verdict = document.Verdict
	run.Status.Reason = document.Reason
	run.Status.Workflow = document.Workflow
	run.Status.Evidence = signalv1.EvidenceStatus{URI: uri, Digest: digest}
	run.Status.Phase = runPhaseForVerdict(document.Verdict)

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
			ID:         checks[index].ID,
			Order:      checks[index].Order,
			Phase:      signalv1.CheckPhasePending,
			Attempt:    1,
			RetryCount: 0,
		}
	}

	return statuses
}

func compactCheckStatuses(statuses []signalv1.CheckStatus) []signalv1.CheckStatus {
	compacted := make([]signalv1.CheckStatus, len(statuses))
	for index := range statuses {
		compacted[index] = *statuses[index].DeepCopy()
		compacted[index].BackendEvidence.Content = ""
	}

	return compacted
}

func contentDigest(content string) string {
	sum := sha256.Sum256([]byte(content))

	return fmt.Sprintf("sha256:%x", sum)
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
			statuses[index].NotRun = true
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
			statuses[index].NotRun = true
		}

		return statuses, "Inconclusive", "workflow task nodes are missing or malformed"
	}

	aggregateVerdict := "Pass"
	aggregateReason := "AllChecksPassed"
	stoppedByPolicy := false

	for index := range run.Spec.Checks {
		check := &run.Spec.Checks[index]
		status := &statuses[index]
		node, count := exactCheckNode(nodes, workflow.GetName(), check)

		if count != 1 {
			status.Phase = signalv1.CheckPhaseInconclusive
			status.Verdict = "Inconclusive"
			status.Reason = "WorkflowTaskIdentityInvalid"
			status.NotRun = true
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
			status.NotRun = true

			if run.Spec.ExecutionPolicy.OnFailure != signalv1.SuiteFailurePolicyStop || !stoppedByPolicy {
				aggregateVerdict = "Inconclusive"
				aggregateReason = "one or more checks were not run"
			}

			continue
		case "Failed", "Error":
			status.Phase = signalv1.CheckPhaseInconclusive
			status.Verdict = "Inconclusive"
			status.Reason = "WorkflowTask" + phase
			aggregateVerdict = "Inconclusive"
			aggregateReason = "one or more check tasks failed at the workflow boundary"
			stoppedByPolicy = true

			continue
		case "Succeeded":
		default:
			status.Phase = signalv1.CheckPhaseInconclusive
			status.Verdict = "Inconclusive"
			status.Reason = "WorkflowTaskIncomplete"
			status.NotRun = true
			aggregateVerdict = "Inconclusive"
			aggregateReason = "one or more check tasks were incomplete"
			stoppedByPolicy = true

			continue
		}

		outputs, outputErr := nodeWorkflowParameters(node, check.ResultBounds)
		if outputErr != nil {
			status.Phase = signalv1.CheckPhaseInconclusive
			status.Verdict = "Inconclusive"
			status.Reason = "OutputsRejected: " + outputErr.Error()
			aggregateVerdict = "Inconclusive"
			aggregateReason = "one or more check outputs were malformed or incomplete"
			stoppedByPolicy = true

			continue
		}

		status.Verdict = outputs["verdict"]
		status.Reason = outputs["reason"]
		status.Measurements = outputs["measurements"]

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
			stoppedByPolicy = true

			continue
		}

		normalizedEvidence, complete, evidenceErr := groupedBackendEvidenceForParticipants(
			outputs["backend-evidence"],
			globalIndexes,
			outputs["backend-resources"],
			check.Backend,
			run.Namespace,
		)
		if evidenceErr != nil {
			status.Phase = signalv1.CheckPhaseInconclusive
			status.Verdict = "Inconclusive"
			status.Reason = "BackendEvidenceRejected: " + evidenceErr.Error()
			aggregateVerdict = "Inconclusive"
			aggregateReason = "one or more checks had invalid backend evidence"
			stoppedByPolicy = true

			continue
		}

		status.BackendEvidence = signalv1.BackendEvidenceStatus{
			Version:  "v1",
			Complete: complete,
			Digest:   contentDigest(normalizedEvidence),
			Content:  normalizedEvidence,
		}

		resources, resourceErr := backendResourceReferencesForParticipants(
			outputs["backend-resources"],
			globalIndexes,
			run.Namespace,
		)
		if resourceErr != nil {
			status.Phase = signalv1.CheckPhaseInconclusive
			status.Verdict = "Inconclusive"
			status.Reason = "BackendResourcesRejected: " + resourceErr.Error()
			aggregateVerdict = "Inconclusive"
			aggregateReason = "one or more checks had invalid backend identities"
			stoppedByPolicy = true

			continue
		}

		status.BackendResources = resources
		status.Cleanup.Complete = outputs["cleanup-failure"] == ""
		status.Cleanup.Reason = outputs["cleanup-failure"]

		if !complete || !status.Cleanup.Complete || len(resources) == 0 {
			status.Phase = signalv1.CheckPhaseInconclusive
			status.Verdict = "Inconclusive"
			status.Reason = "EvidenceOrCleanupIncomplete"
			aggregateVerdict = "Inconclusive"
			aggregateReason = "one or more checks lacked complete evidence, backend coverage, or cleanup"
			stoppedByPolicy = true

			continue
		}

		switch status.Verdict {
		case "Pass":
			status.Phase = signalv1.CheckPhaseSucceeded
		case "Fail":
			status.Phase = signalv1.CheckPhaseFailed
			stoppedByPolicy = true

			if aggregateVerdict == "Pass" {
				aggregateVerdict = "Fail"
				aggregateReason = "OneOrMoreChecksFailed"
			}
		case "Inconclusive":
			status.Phase = signalv1.CheckPhaseInconclusive
			aggregateVerdict = "Inconclusive"
			aggregateReason = "one or more checks were inconclusive"
			stoppedByPolicy = true
		default:
			status.Phase = signalv1.CheckPhaseInconclusive
			status.Verdict = "Inconclusive"
			status.Reason = "InvalidVerdict"
			aggregateVerdict = "Inconclusive"
			aggregateReason = "one or more checks returned an invalid verdict"
			stoppedByPolicy = true
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

func nodeWorkflowParameters(
	node map[string]any,
	bounds signalv1.ResultBounds,
) (map[string]string, error) {
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
		"reason":            int(bounds.ReasonBytes),
		"measurements":      int(bounds.MeasurementsBytes),
		"backend-resources": int(bounds.BackendResourcesBytes),
		"backend-evidence":  int(bounds.BackendEvidenceBytes),
		"cleanup-failure":   workflowOutputLimits["cleanup-failure"],
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
		"cleanup-failure",
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

	if status.StartedAt != nil && status.FinishedAt != nil {
		status.Duration = metav1.Duration{Duration: status.FinishedAt.Sub(status.StartedAt.Time)}
	}
}

func nodeString(node map[string]any, key string) string {
	value, ok := node[key].(string)
	if !ok {
		return ""
	}

	return value
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
	checks := run.Spec.Checks
	if len(checks) == 0 {
		return nil, errors.New("run has no certification checks")
	}

	var snapshot corev1.ConfigMap
	if err := r.Get(
		ctx,
		client.ObjectKey{Namespace: run.Namespace, Name: checks[0].Script.ConfigMapName},
		&snapshot,
	); err != nil {
		return nil, fmt.Errorf("read script snapshot for evidence: %w", err)
	}

	scriptContents := make(map[string]string, len(checks))
	for index := range checks {
		content, exists := snapshot.Data[checks[index].Script.Key]
		if !exists {
			return nil, fmt.Errorf("script snapshot is missing check %s key %s", checks[index].ID, checks[index].Script.Key)
		}

		scriptContents[string(checks[index].ID)] = content
	}

	doc := map[string]any{
		"apiVersion":          "signal.unbounded-cloud.io/evidence/v1alpha1",
		"runID":               string(run.UID),
		"runName":             run.Name,
		"profile":             run.Spec.Profile,
		"participants":        run.Spec.Participants,
		"participantRoles":    run.Spec.ParticipantRoles,
		"topologyConstraints": run.Spec.TopologyConstraints,
		"workflow":            run.Status.Workflow,
		"checks":              checks,
		"checkStatus":         run.Status.Checks,
		"scriptContents":      scriptContents,
		"executionPolicy":     run.Spec.ExecutionPolicy,
		"purpose":             run.Spec.Purpose,
		"executionState":      executionState,
		"verdict":             verdict,
		"reason":              reason,
		"startedAt":           run.Status.StartedAt,
		"finishedAt":          run.Status.FinishedAt.UTC().Format(time.RFC3339Nano),
	}

	return json.MarshalIndent(doc, "", "  ")
}

func backendResourceReferences(
	value string,
	participantCount int,
	namespace string,
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
		if reference.APIVersion == "" || reference.Kind == "" || reference.Name == "" || reference.UID == "" {
			return nil, fmt.Errorf("backend-resources[%d] has an incomplete identity", index)
		}

		if reference.Namespace == "" {
			reference.Namespace = namespace
		}

		if reference.Namespace != namespace {
			return nil, fmt.Errorf("backend-resources[%d] namespace does not match the Run namespace", index)
		}

		identity := strings.Join([]string{
			reference.APIVersion,
			reference.Kind,
			reference.Namespace,
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

	if participantCount > 1 {
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
) ([]signalv1.BackendResourceReference, error) {
	references, err := backendResourceReferences(value, len(globalIndexes), namespace)
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
) (bool, error) {
	if err := signaladapter.ValidateBackendEvidence(content); err != nil {
		return false, err
	}

	var evidence signaladapter.BackendEvidence
	if err := json.Unmarshal([]byte(content), &evidence); err != nil {
		return false, err
	}

	if len(evidence.Participants) != participantCount {
		return false, fmt.Errorf(
			"backend-evidence contains %d participants, want %d",
			len(evidence.Participants),
			participantCount,
		)
	}

	seen := make([]bool, participantCount)
	complete := evidence.Complete && evidence.CaptureError == ""
	podUIDs := map[types.UID]struct{}{}

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

			if reference.APIVersion != participant.Resource.APIVersion ||
				reference.Kind != participant.Resource.Kind ||
				reference.Namespace != participant.Resource.Namespace ||
				reference.Name != participant.Resource.Name ||
				reference.UID != participant.Resource.UID {
				return false, fmt.Errorf("backend-evidence participant %d resource differs from backend-resources", participant.ParticipantIndex)
			}

			matches++
		}

		if matches != 1 || participant.Resource.APIVersion != backend.APIVersion ||
			participant.Resource.Kind != backend.Kind || participant.Resource.Namespace != namespace {
			return false, fmt.Errorf("backend-evidence participant %d does not match the declared backend", participant.ParticipantIndex)
		}

		complete = complete && participant.CaptureError == "" && len(participant.Pods) > 0
		launcher, worker := false, false

		for _, pod := range participant.Pods {
			if pod.UID == "" || pod.Name == "" {
				return false, errors.New("backend-evidence Pod identity is incomplete")
			}

			if _, duplicate := podUIDs[pod.UID]; duplicate {
				return false, errors.New("backend-evidence repeats a Pod UID")
			}

			podUIDs[pod.UID] = struct{}{}
			if backend.Kind == "Pod" && (pod.UID != participant.Resource.UID ||
				pod.Name != participant.Resource.Name || pod.Role != "participant" || len(participant.Pods) != 1) {
				return false, errors.New("backend-evidence Pod differs from its backend resource")
			}

			launcher = launcher || pod.Role == "launcher"
			worker = worker || pod.Role == "worker"

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

		if backend.Kind == "MPIJob" {
			complete = complete && launcher && worker && participant.LauncherJob != nil &&
				participant.LauncherJob.Name != "" && participant.LauncherJob.UID != ""
		}
	}

	return complete, nil
}

func groupedBackendEvidenceForParticipants(
	content string,
	globalIndexes []int32,
	resources string,
	backend signalv1.ExecutionBackend,
	namespace string,
) (string, bool, error) {
	references, err := backendResourceReferences(resources, len(globalIndexes), namespace)
	if err != nil {
		return "", false, err
	}

	complete, err := groupedBackendEvidenceComplete(content, len(globalIndexes), references, backend, namespace)
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

	statuses := run.Status.Checks
	if len(statuses) != len(run.Spec.Checks) {
		statuses = initialCheckStatuses(run.Spec.Checks)
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
		statuses[index].NotRun = true
	}

	run.Status.Checks = statuses

	now := metav1.NewTime(r.now())

	run.Status.FinishedAt = &now
	if err := r.Status().Patch(ctx, run, client.MergeFrom(before)); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: time.Millisecond}, nil
}

func (r *RunReconciler) cleanup(ctx context.Context, run *signalv1.CertificationRun) (ctrl.Result, error) {
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

	if err := r.clearRequest(ctx, run); err != nil {
		return ctrl.Result{}, err
	}

	released, err := r.releaseRunReservations(ctx, run)
	if err != nil {
		return ctrl.Result{}, err
	}

	deleted := false

	checks := run.Spec.Checks
	if len(checks) == 0 {
		return ctrl.Result{}, errors.New("run has no certification checks")
	}

	snapshot := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: run.Namespace, Name: checks[0].Script.ConfigMapName},
	}
	if err := r.Delete(ctx, snapshot); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	deleted = true

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

	if len(run.Spec.Checks) > 0 {
		snapshot := &corev1.ConfigMap{}

		reader := r.APIReader
		if reader == nil {
			reader = r.Client
		}

		err := reader.Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: run.Spec.Checks[0].Script.ConfigMapName}, snapshot)
		if err == nil {
			if snapshot.Labels[SnapshotRunLabel] != run.Name {
				return ctrl.Result{}, errors.New("refusing to delete script snapshot with different run identity")
			}

			uid := snapshot.UID
			if err := r.Delete(ctx, snapshot, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		} else if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
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

		before := node.DeepCopy()
		delete(node.Annotations, ReservationLabel)

		if err := r.Patch(ctx, &node, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return false, err
		}
	}

	return true, nil
}

func (r *RunReconciler) clearRequest(ctx context.Context, run *signalv1.CertificationRun) error {
	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: run.Spec.RequestNodeName}, &node); err != nil {
		return client.IgnoreNotFound(err)
	}

	if requestNodeUID := participantNodeUID(run, run.Spec.RequestNodeName); requestNodeUID != "" && node.UID != requestNodeUID {
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
			if kind.Kind != "Pod" && check.Backend.Kind != "MPIJob" {
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
		if check.Backend.Kind == "MPIJob" {
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

func participantNodeUID(run *signalv1.CertificationRun, nodeName string) types.UID {
	for _, participant := range run.Spec.Participants {
		if participant.NodeName == nodeName {
			return participant.NodeUID
		}
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
		checks[index].WorkloadPolicy = *profile.Spec.Checks[index].WorkloadPolicy.DeepCopy()
		checks[index].RuntimeStage = copyRuntimeStage(profile.Spec.Checks[index].RuntimeStage)
		checks[index].Parameters = maps.Clone(profile.Spec.Checks[index].Parameters)
		checks[index].ParticipantPolicy.RequiredRoles = slices.Clone(
			profile.Spec.Checks[index].ParticipantPolicy.RequiredRoles,
		)
		checks[index].ParticipantPolicy.ExcludedRoles = slices.Clone(
			profile.Spec.Checks[index].ParticipantPolicy.ExcludedRoles,
		)
	}

	return checks
}

func validateCheckPlan(checks []signalv1.CertificationCheck) error {
	if len(checks) != groupedCheckCount {
		return fmt.Errorf("must contain exactly %d checks", groupedCheckCount)
	}

	seen := make(map[signalv1.CheckID]struct{}, len(checks))
	for index := range checks {
		check := &checks[index]
		if check.ID == "" {
			return fmt.Errorf("check %d has an empty ID", index)
		}

		if check.Order != int32(index) {
			return fmt.Errorf("check %s order is %d, want %d", check.ID, check.Order, index)
		}

		if string(check.ID) != fmt.Sprintf("R%02d", index+1) {
			return fmt.Errorf("check order %d must use ID R%02d", index, index+1)
		}

		if _, duplicate := seen[check.ID]; duplicate {
			return fmt.Errorf("duplicate check ID %q", check.ID)
		}

		seen[check.ID] = struct{}{}

		if check.MaxAttempts != 1 {
			return fmt.Errorf("check %s maxAttempts must be exactly 1", check.ID)
		}

		if err := validateResultBounds(check.ID, check.ResultBounds); err != nil {
			return err
		}

		if err := validateCheckParticipantPolicy(check.ID, check.ParticipantPolicy); err != nil {
			return err
		}
	}

	return nil
}

func validateRunCheckPlan(checks []signalv1.CertificationCheckSnapshot) error {
	if len(checks) != groupedCheckCount {
		return fmt.Errorf("run must contain exactly %d checks", groupedCheckCount)
	}

	seen := make(map[signalv1.CheckID]struct{}, len(checks))
	for index := range checks {
		check := &checks[index]
		if check.ID == "" || check.Order != int32(index) {
			return fmt.Errorf("run check %d has invalid identity or order", index)
		}

		if string(check.ID) != fmt.Sprintf("R%02d", index+1) {
			return fmt.Errorf("run check order %d must use ID R%02d", index, index+1)
		}

		if _, duplicate := seen[check.ID]; duplicate {
			return fmt.Errorf("run contains duplicate check ID %q", check.ID)
		}

		seen[check.ID] = struct{}{}

		if check.MaxAttempts != 1 {
			return fmt.Errorf("run check %s maxAttempts must be exactly 1", check.ID)
		}

		if err := validateResultBounds(check.ID, check.ResultBounds); err != nil {
			return err
		}
	}

	return nil
}

func validateResultBounds(id signalv1.CheckID, bounds signalv1.ResultBounds) error {
	switch {
	case bounds.ReasonBytes < 1 || bounds.ReasonBytes > int32(workflowOutputLimits["reason"]):
		return fmt.Errorf("check %s reasonBytes must be between 1 and %d", id, workflowOutputLimits["reason"])
	case bounds.MeasurementsBytes < 1 || bounds.MeasurementsBytes > int32(workflowOutputLimits["measurements"]):
		return fmt.Errorf(
			"check %s measurementsBytes must be between 1 and %d",
			id,
			workflowOutputLimits["measurements"],
		)
	case bounds.BackendResourcesBytes < 1 ||
		bounds.BackendResourcesBytes > int32(workflowOutputLimits["backend-resources"]):
		return fmt.Errorf(
			"check %s backendResourcesBytes must be between 1 and %d",
			id,
			workflowOutputLimits["backend-resources"],
		)
	case bounds.BackendEvidenceBytes < 1 || bounds.BackendEvidenceBytes > 64*1024:
		return fmt.Errorf("check %s backendEvidenceBytes must be between 1 and 65536", id)
	}

	return nil
}

func validateCheckParticipantPolicy(id signalv1.CheckID, policy signalv1.CheckParticipantPolicy) error {
	required := make(map[string]struct{}, len(policy.RequiredRoles))
	for _, role := range policy.RequiredRoles {
		if role == "" {
			return fmt.Errorf("check %s contains an empty required role", id)
		}

		if _, duplicate := required[role]; duplicate {
			return fmt.Errorf("check %s repeats required role %q", id, role)
		}

		required[role] = struct{}{}
	}

	excluded := make(map[string]struct{}, len(policy.ExcludedRoles))
	for _, role := range policy.ExcludedRoles {
		if role == "" {
			return fmt.Errorf("check %s contains an empty excluded role", id)
		}

		if _, duplicate := excluded[role]; duplicate {
			return fmt.Errorf("check %s repeats excluded role %q", id, role)
		}

		if _, conflict := required[role]; conflict {
			return fmt.Errorf("check %s both requires and excludes role %q", id, role)
		}

		excluded[role] = struct{}{}
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

	if assigned > profile.Spec.Participants.Count {
		return fmt.Errorf(
			"participant roles require %d assignments but participant count is %d",
			assigned,
			profile.Spec.Participants.Count,
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
		for _, role := range append(
			slices.Clone(checks[index].ParticipantPolicy.RequiredRoles),
			checks[index].ParticipantPolicy.ExcludedRoles...,
		) {
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
	required := make(map[string]bool, len(policy.RequiredRoles))
	for _, role := range policy.RequiredRoles {
		required[role] = false
	}

	excluded := make(map[string]struct{}, len(policy.ExcludedRoles))
	for _, role := range policy.ExcludedRoles {
		excluded[role] = struct{}{}
	}

	selected := make([]signalv1.Participant, 0, len(participants))

	globalIndexes := make([]int32, 0, len(participants))
	for index := range participants {
		participant := participants[index]
		include := len(required) == 0

		for _, role := range participant.Roles {
			if _, blocked := excluded[role]; blocked {
				include = false

				break
			}

			if _, wanted := required[role]; wanted {
				required[role] = true
				include = true
			}
		}

		if !include {
			continue
		}

		globalIndexes = append(globalIndexes, participant.Index)
		participant.Index = int32(len(selected))
		selected = append(selected, participant)
	}

	if len(selected) == 0 {
		return nil, nil, errors.New("participant policy selected no participants")
	}

	for role, present := range required {
		if !present {
			return nil, nil, fmt.Errorf("required role %q has no selected participant", role)
		}
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

func copyRuntimeStage(stage *signalv1.RuntimeStage) *signalv1.RuntimeStage {
	if stage == nil {
		return nil
	}

	return &signalv1.RuntimeStage{Name: stage.Name}
}

func runtimeStageName(stage *signalv1.RuntimeStage) string {
	if stage == nil {
		return ""
	}

	return stage.Name
}

func AddToScheme(scheme *runtime.Scheme) error {
	return signalv1.AddToScheme(scheme)
}
