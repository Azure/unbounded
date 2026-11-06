// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	"k8s.io/apimachinery/pkg/api/resource"
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
	"github.com/Azure/unbounded/internal/signalworkload"
)

const (
	RequestLabel               = "signal.unbounded-cloud.io/certification-request"
	ReservationLabel           = "signal.unbounded-cloud.io/reserved-by"
	RunNodeLabel               = "signal.unbounded-cloud.io/request-node"
	RunProfileLabel            = "signal.unbounded-cloud.io/profile"
	RunSuiteLabel              = "signal.unbounded-cloud.io/suite"
	RunTriggerAnnotation       = "signal.unbounded-cloud.io/trigger"
	RunAdmissionAnnotation     = "signal.unbounded-cloud.io/admission"
	RunStartIdentityAnnotation = "signal.unbounded-cloud.io/start-fingerprints"
	RunFinalizer               = "signal.unbounded-cloud.io/cleanup"
	SnapshotRunLabel           = "signal.unbounded-cloud.io/run"
	RunUIDLabel                = "signal.unbounded-cloud.io/run-uid"
	checkResultDigest          = "signal.unbounded-cloud.io/check-result-digest"
	checkResultRunUID          = "signal.unbounded-cloud.io/check-result-run-uid"
	checkResultCheckID         = "signal.unbounded-cloud.io/check-result-check-id"
	checkResultCheckOrder      = "signal.unbounded-cloud.io/check-result-check-order"
	stageResultDigest          = "signal.unbounded-cloud.io/stage-result-digest"
	stageResultRunUID          = "signal.unbounded-cloud.io/stage-result-run-uid"
	stageResultPlanDigest      = "signal.unbounded-cloud.io/stage-result-plan-digest"
	stageResultPhaseOrder      = "signal.unbounded-cloud.io/stage-result-phase-order"
	stageResultStageOrder      = "signal.unbounded-cloud.io/stage-result-stage-order"
	stageResultRunnerImages    = "signal.unbounded-cloud.io/stage-result-runner-images"
	checkResultPlanDigest      = "signal.unbounded-cloud.io/check-result-plan-digest"
	runAdmissionReady          = "ready"
	runAdmissionPending        = "pending"
	profileSuiteNameField      = "spec.suite.name"

	maxAdapterParticipants         = 8
	maxAdapterReasonBytes          = 512
	maxAdapterMeasurementsBytes    = 16 * 1024
	maxAdapterBackendOutputBytes   = 8 * 1024
	maxAdapterBackendEvidenceBytes = 24 * 1024
	maxAdapterEvidenceTextBytes    = 512
	maxAdapterEvidenceDevices      = 64
	adapterBackendEvidenceVersion  = "v2"
	maxAdapterCheckResultFields    = maxAdapterReasonBytes +
		maxAdapterMeasurementsBytes +
		maxAdapterBackendOutputBytes +
		maxAdapterBackendEvidenceBytes
	maximumCheckResultBytes = 6*maxAdapterCheckResultFields + 4*1024
)

var runnerImageDigestPattern = regexp.MustCompile(`^[^@\s]+@sha256:[0-9a-f]{64}$`)

var checkResultFieldLimits = map[string]int{
	"backend-resources": maxAdapterBackendOutputBytes,
	"backend-evidence":  maxAdapterBackendEvidenceBytes,
	"measurements":      maxAdapterMeasurementsBytes,
	"reason":            maxAdapterReasonBytes,
	"verdict":           32,
}

const (
	adapterServiceAccountName   = "project-signal-adapter"
	adapterWorkflowTemplateName = "project-signal-participant-adapter"
	maximumWorkflowSpecBytes    = 768 * 1024
	maximumStatusTextRunes      = 1024
	checkResultDataKey          = "result.json"
)

var workflowGVK = schema.GroupVersionKind{
	Group:   "argoproj.io",
	Version: "v1alpha1",
	Kind:    "Workflow",
}

type adapterBackendEvidence struct {
	Version      string                              `json:"version"`
	Identity     adapterEvidenceIdentity             `json:"identity"`
	Timing       adapterCheckTimingEvidence          `json:"timing"`
	Provenance   adapterExecutionProvenanceEvidence  `json:"provenance"`
	GroupResult  *adapterVerdictEvidence             `json:"groupResult,omitempty"`
	Complete     bool                                `json:"complete"`
	Participants []adapterParticipantBackendEvidence `json:"participants"`
	CaptureError string                              `json:"captureError,omitempty"`
}

type adapterEvidenceIdentity struct {
	RunUID     types.UID        `json:"runUID"`
	CheckID    signalv1.CheckID `json:"checkID"`
	CheckOrder int32            `json:"checkOrder"`
	PlanDigest string           `json:"planDigest,omitempty"`
}

type adapterCheckTimingEvidence struct {
	StartedAt        metav1.Time      `json:"startedAt"`
	FinishedAt       metav1.Time      `json:"finishedAt"`
	ObservedDuration metav1.Duration  `json:"observedDuration"`
	MaxObservedGap   *metav1.Duration `json:"maxObservedGap,omitempty"`
	SampleCount      int              `json:"sampleCount"`
}

type adapterExecutionProvenanceEvidence struct {
	AdapterImage string `json:"adapterImage,omitempty"`
	RunnerImage  string `json:"runnerImage"`
}

type adapterVerdictEvidence struct {
	ExecutionState string `json:"executionState"`
	Verdict        string `json:"verdict"`
	Reason         string `json:"reason"`
	Attribution    string `json:"attribution"`
}

type adapterParticipantBackendEvidence struct {
	ParticipantIndex  int                          `json:"participantIndex"`
	Verdict           adapterVerdictEvidence       `json:"verdict"`
	Start             signalv1.NodeFingerprint     `json:"start"`
	End               signalv1.NodeFingerprint     `json:"end"`
	MaxObservedGap    *metav1.Duration             `json:"maxObservedGap,omitempty"`
	VisibleDevices    []string                     `json:"visibleDevices,omitempty"`
	CoveredDevices    []string                     `json:"coveredDevices,omitempty"`
	PerDeviceCoverage bool                         `json:"perDeviceCoverage,omitempty"`
	Resource          adapterBackendObjectEvidence `json:"resource"`
	LauncherJob       *adapterJobEvidence          `json:"launcherJob,omitempty"`
	Pods              []adapterPodEvidence         `json:"pods"`
	CaptureError      string                       `json:"captureError,omitempty"`
}

type adapterBackendObjectEvidence struct {
	APIVersion      string                                  `json:"apiVersion"`
	Kind            string                                  `json:"kind"`
	Namespace       string                                  `json:"namespace,omitempty"`
	Name            string                                  `json:"name"`
	UID             types.UID                               `json:"uid"`
	Phase           string                                  `json:"phase,omitempty"`
	Reason          string                                  `json:"reason,omitempty"`
	Message         string                                  `json:"message,omitempty"`
	Conditions      []adapterConditionEvidence              `json:"conditions,omitempty"`
	ReplicaStatuses map[string]adapterReplicaStatusEvidence `json:"replicaStatuses,omitempty"`
}

type adapterReplicaStatusEvidence struct {
	Active    int64 `json:"active,omitempty"`
	Succeeded int64 `json:"succeeded,omitempty"`
	Failed    int64 `json:"failed,omitempty"`
}

type adapterConditionEvidence struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

type adapterJobEvidence struct {
	Name       string                     `json:"name"`
	UID        types.UID                  `json:"uid"`
	Conditions []adapterConditionEvidence `json:"conditions,omitempty"`
}

type adapterPodEvidence struct {
	Name       string                     `json:"name"`
	UID        types.UID                  `json:"uid"`
	Role       string                     `json:"role"`
	NodeName   string                     `json:"nodeName,omitempty"`
	NodeUID    types.UID                  `json:"nodeUID,omitempty"`
	NodeError  string                     `json:"nodeError,omitempty"`
	Phase      corev1.PodPhase            `json:"phase,omitempty"`
	Reason     string                     `json:"reason,omitempty"`
	Message    string                     `json:"message,omitempty"`
	Containers []adapterContainerEvidence `json:"containers"`
	Conditions []adapterConditionEvidence `json:"conditions,omitempty"`
}

type adapterContainerEvidence struct {
	Name         string                        `json:"name"`
	RestartCount int32                         `json:"restartCount"`
	State        adapterContainerStateEvidence `json:"state"`
	LastState    adapterContainerStateEvidence `json:"lastState"`
	CurrentLog   adapterCapturedLogEvidence    `json:"currentLog"`
	PreviousLog  adapterCapturedLogEvidence    `json:"previousLog"`
}

type adapterContainerStateEvidence struct {
	State      string `json:"state,omitempty"`
	ExitCode   int32  `json:"exitCode,omitempty"`
	Signal     int32  `json:"signal,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Message    string `json:"message,omitempty"`
	FinishedAt string `json:"finishedAt,omitempty"`
}

type adapterCapturedLogEvidence struct {
	Status                string `json:"status"`
	Bytes                 int    `json:"bytes,omitempty"`
	CapturedBytes         int    `json:"capturedBytes,omitempty"`
	Truncated             bool   `json:"truncated,omitempty"`
	Content               string `json:"content,omitempty"`
	ResultMarkerStatus    string `json:"resultMarkerStatus,omitempty"`
	ResultDirectoryStatus string `json:"resultDirectoryStatus,omitempty"`
	Error                 string `json:"error,omitempty"`
}

type adapterPartialResult struct {
	ParticipantIndex int    `json:"participantIndex"`
	ExecutionState   string `json:"executionState"`
	Verdict          string `json:"verdict"`
	Reason           string `json:"reason"`
	Measurements     string `json:"measurements"`
	ResultLog        any    `json:"resultLog,omitempty"`
}

type adapterCheckResult struct {
	ExecutionState   string `json:"executionState"`
	Verdict          string `json:"verdict"`
	Reason           string `json:"reason"`
	Measurements     string `json:"measurements"`
	BackendResources string `json:"backend-resources"`
	BackendEvidence  string `json:"backend-evidence"`
}

func minimumAdapterBackendEvidenceBytes(participantCount int) int {
	evidence := adapterBackendEvidence{
		Version:      adapterBackendEvidenceVersion,
		Complete:     false,
		Participants: make([]adapterParticipantBackendEvidence, participantCount),
	}

	for index := range evidence.Participants {
		evidence.Participants[index] = adapterParticipantBackendEvidence{
			ParticipantIndex: index,
			Resource: adapterBackendObjectEvidence{
				APIVersion: strings.Repeat("a", 32),
				Kind:       strings.Repeat("a", 16),
				Namespace:  strings.Repeat("a", 63),
				Name:       strings.Repeat("a", 63),
				UID:        types.UID(strings.Repeat("a", 128)),
			},
			CaptureError: strings.Repeat("a", maxAdapterEvidenceTextBytes),
		}
	}

	content, err := json.Marshal(evidence)
	if err != nil {
		return maxAdapterBackendEvidenceBytes + 1
	}

	return len(content) + 64
}

func minimumAdapterPartialReasonBytes() int {
	return len("BackendSetupFailed")
}

func minimumAdapterPartialMeasurementsBytes(participantCount int) int {
	results := make([]adapterPartialResult, participantCount)
	for index := range results {
		results[index] = adapterPartialResult{
			ParticipantIndex: index,
			ExecutionState:   "Error",
			Verdict:          "Inconclusive",
			Reason:           "BackendSetupFailed",
		}
	}

	content, err := json.Marshal(map[string]any{
		"participantCount": participantCount,
		"createdBackends":  participantCount,
		"error":            strings.Repeat("\x00", maxAdapterEvidenceTextBytes),
		"results":          results,
	})
	if err != nil {
		return maxAdapterMeasurementsBytes + 1
	}

	return len(content) + 64
}

func minimumAdapterPartialBackendResourcesBytes(participantCount int) int {
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
		return maxAdapterBackendOutputBytes + 1
	}

	return len(content) + 64
}

func validateAdapterBackendEvidence(content string) error {
	if content == "" {
		return errors.New("backend evidence is required")
	}

	if len(content) > maxAdapterBackendEvidenceBytes {
		return fmt.Errorf("backend evidence exceeds %d bytes", maxAdapterBackendEvidenceBytes)
	}

	var evidence adapterBackendEvidence

	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&evidence); err != nil {
		return fmt.Errorf("decode backend evidence: %w", err)
	}

	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("backend evidence contains trailing JSON values")
		}

		return fmt.Errorf("decode trailing backend evidence value: %w", err)
	}

	if evidence.Version != adapterBackendEvidenceVersion {
		return fmt.Errorf("backend evidence version %q is invalid", evidence.Version)
	}

	if evidence.Identity.RunUID == "" || evidence.Identity.CheckID == "" || evidence.Identity.CheckOrder < 0 {
		return errors.New("backend evidence identity is incomplete")
	}

	if evidence.Identity.PlanDigest != "" && !signalv1.IsSHA256Digest(evidence.Identity.PlanDigest) {
		return errors.New("backend evidence plan digest is invalid")
	}

	if !signalv1.IsDigestPinnedImage(evidence.Provenance.RunnerImage) ||
		(evidence.Provenance.AdapterImage != "" &&
			!signalv1.IsDigestPinnedImage(evidence.Provenance.AdapterImage)) {
		return errors.New("backend evidence provenance is invalid")
	}

	if evidence.Identity.PlanDigest != "" && evidence.Provenance.AdapterImage == "" {
		return errors.New("backend evidence with a plan digest requires adapter provenance")
	}

	if evidence.Timing.StartedAt.IsZero() || evidence.Timing.FinishedAt.IsZero() ||
		evidence.Timing.FinishedAt.Before(&evidence.Timing.StartedAt) ||
		evidence.Timing.SampleCount < 1 {
		return errors.New("backend evidence timing is invalid")
	}

	if len(evidence.Participants) < 1 || len(evidence.Participants) > maxAdapterParticipants {
		return fmt.Errorf(
			"backend evidence must contain between 1 and %d participants",
			maxAdapterParticipants,
		)
	}

	for index := range evidence.Participants {
		participant := &evidence.Participants[index]
		if participant.Start.NodeUID == "" || participant.End.NodeUID == "" ||
			participant.Start.NodeUID != participant.End.NodeUID {
			return fmt.Errorf("backend evidence participant %d node identity is invalid", participant.ParticipantIndex)
		}

		if err := validateAdapterVerdictEvidence(participant.Verdict); err != nil {
			return fmt.Errorf("backend evidence participant %d verdict: %w", participant.ParticipantIndex, err)
		}

		if len(participant.VisibleDevices) > maxAdapterEvidenceDevices ||
			len(participant.CoveredDevices) > maxAdapterEvidenceDevices {
			return fmt.Errorf("backend evidence participant %d device evidence exceeds bounds", participant.ParticipantIndex)
		}
	}

	if evidence.GroupResult != nil {
		if err := validateAdapterVerdictEvidence(*evidence.GroupResult); err != nil {
			return fmt.Errorf("backend evidence group verdict: %w", err)
		}

		if evidence.GroupResult.Attribution != "group" {
			return errors.New("backend evidence group verdict attribution is invalid")
		}
	}

	return nil
}

func validateAdapterVerdictEvidence(verdict adapterVerdictEvidence) error {
	if verdict.ExecutionState == "" || verdict.Reason == "" {
		return errors.New("execution state and reason are required")
	}

	switch verdict.Verdict {
	case "Pass", "Fail", "Inconclusive":
	default:
		return errors.New("verdict is invalid")
	}

	switch verdict.Attribution {
	case "participant", "group", "group-with-participant-observation":
	default:
		return errors.New("attribution is invalid")
	}

	return nil
}

type AdmissionReconciler struct {
	client.Client
	APIReader       client.Reader
	Namespace       string
	ScriptNamespace string
	RunnerImage     string
	ControllerImage string
	AdapterImage    string
	WorkflowDigest  string
	ResolverImages  []string
	Recorder        recorder.EventRecorder
	Now             func() time.Time
	admissionMu     sync.Mutex
}

// +kubebuilder:rbac:groups=signal.unbounded-cloud.io,resources=certificationprofiles;certificationsuites;certificationtests,verbs=get;list;watch

type RunReconciler struct {
	client.Client
	APIReader         client.Reader
	Evidence          EvidenceStore
	Now               func() time.Time
	MaxConcurrentRuns int
	Recorder          recorder.EventRecorder
	ControllerImage   string
}

func (r *AdmissionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}

	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&signalv1.CertificationProfile{},
		profileSuiteNameField,
		profileSuiteIndex,
	); err != nil {
		return fmt.Errorf("index CertificationProfiles by Suite: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}, builder.WithPredicates(predicate.LabelChangedPredicate{})).
		Watches(
			&signalv1.CertificationSuite{},
			handler.EnqueueRequestsFromMapFunc(r.requestsForSuite),
		).
		Watches(
			&signalv1.CertificationTest{},
			handler.EnqueueRequestsFromMapFunc(r.requestsForTest),
		).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

func profileSuiteIndex(object client.Object) []string {
	profile, ok := object.(*signalv1.CertificationProfile)
	if !ok || profile.Spec.Suite.Name == "" {
		return nil
	}

	return []string{profile.Spec.Suite.Name}
}

func (r *AdmissionReconciler) requestsForSuite(ctx context.Context, object client.Object) []ctrl.Request {
	suite, ok := object.(*signalv1.CertificationSuite)
	if !ok {
		return nil
	}

	var profiles signalv1.CertificationProfileList
	if err := r.List(
		ctx,
		&profiles,
		client.MatchingFields{profileSuiteNameField: suite.Name},
	); err != nil {
		log.FromContext(ctx).Error(err, "failed to list CertificationProfiles for Suite", "suite", suite.Name)

		return nil
	}

	profileNames := make(map[string]struct{}, len(profiles.Items))
	for index := range profiles.Items {
		profileNames[profiles.Items[index].Name] = struct{}{}
	}

	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		log.FromContext(ctx).Error(err, "failed to list Nodes requesting Suite", "suite", suite.Name)

		return nil
	}

	requests := make([]ctrl.Request, 0)

	for index := range nodes.Items {
		node := &nodes.Items[index]
		if _, requested := profileNames[node.Labels[RequestLabel]]; requested {
			requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKey{Name: node.Name}})
		}
	}

	return requests
}

func (r *AdmissionReconciler) requestsForTest(ctx context.Context, object client.Object) []ctrl.Request {
	test, ok := object.(*signalv1.CertificationTest)
	if !ok {
		return nil
	}

	var suites signalv1.CertificationSuiteList
	if err := r.List(ctx, &suites); err != nil {
		log.FromContext(ctx).Error(err, "failed to list CertificationSuites for Test", "test", test.Name)

		return nil
	}

	requests := make([]ctrl.Request, 0)

	for index := range suites.Items {
		suite := &suites.Items[index]
		if !suiteReferencesTest(suite, test.Name) {
			continue
		}

		requests = append(requests, r.requestsForSuite(ctx, suite)...)
	}

	return uniqueRequests(requests)
}

func suiteReferencesTest(suite *signalv1.CertificationSuite, testName string) bool {
	for phaseIndex := range suite.Spec.Phases {
		for stageIndex := range suite.Spec.Phases[phaseIndex].Stages {
			if suite.Spec.Phases[phaseIndex].Stages[stageIndex].Test.Name == testName {
				return true
			}
		}
	}

	return false
}

func uniqueRequests(requests []ctrl.Request) []ctrl.Request {
	seen := make(map[types.NamespacedName]struct{}, len(requests))
	result := make([]ctrl.Request, 0, len(requests))

	for _, request := range requests {
		if _, exists := seen[request.NamespacedName]; exists {
			continue
		}

		seen[request.NamespacedName] = struct{}{}
		result = append(result, request)
	}

	return result
}

func (r *RunReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}

	if !signalv1.IsDigestPinnedImage(r.ControllerImage) {
		return errors.New("controller image must use an immutable sha256 digest")
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

	if err := ValidateRunnerImage(r.RunnerImage); err != nil {
		return err
	}

	if err := validatePlanProvenance(r.AdapterImage, r.WorkflowDigest); err != nil {
		return err
	}

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

	suite, checks, err := r.resolveSuite(ctx, &profile)
	if err != nil {
		return err
	}

	participants, err := r.selectParticipants(ctx, node, &profile, checks)
	if err != nil {
		return err
	}

	startFingerprints, err := r.captureParticipantFingerprints(ctx, participants)
	if err != nil {
		return err
	}

	for index := range checks {
		if len(participants) > maxAdapterParticipants {
			return fmt.Errorf(
				"check %s selects %d participants, maximum is %d",
				checks[index].ID,
				len(participants),
				maxAdapterParticipants,
			)
		}

		minimumEvidenceBytes := minimumAdapterBackendEvidenceBytes(len(participants))
		if maxAdapterBackendEvidenceBytes < minimumEvidenceBytes {
			return fmt.Errorf(
				"check %s fixed backend evidence limit %d cannot represent %d participants; requires at least %d bytes",
				checks[index].ID,
				maxAdapterBackendEvidenceBytes,
				len(participants),
				minimumEvidenceBytes,
			)
		}

		minimumReasonBytes := minimumAdapterPartialReasonBytes()
		if maxAdapterReasonBytes < minimumReasonBytes {
			return fmt.Errorf(
				"check %s fixed reason limit %d cannot represent mandatory failure output; requires at least %d bytes",
				checks[index].ID,
				maxAdapterReasonBytes,
				minimumReasonBytes,
			)
		}

		minimumMeasurementsBytes := minimumAdapterPartialMeasurementsBytes(len(participants))
		if maxAdapterMeasurementsBytes < minimumMeasurementsBytes {
			return fmt.Errorf(
				"check %s fixed measurements limit %d cannot represent %d participants; requires at least %d bytes",
				checks[index].ID,
				maxAdapterMeasurementsBytes,
				len(participants),
				minimumMeasurementsBytes,
			)
		}

		minimumBackendResourcesBytes := minimumAdapterPartialBackendResourcesBytes(len(participants))
		if maxAdapterBackendOutputBytes < minimumBackendResourcesBytes {
			return fmt.Errorf(
				"check %s fixed backend resource limit %d cannot represent %d participants; requires at least %d bytes",
				checks[index].ID,
				maxAdapterBackendOutputBytes,
				len(participants),
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

	var snapshot *corev1.ConfigMap

	var run *signalv1.CertificationRun

	if len(suite.Spec.Phases) > 0 {
		resolvedPlan, resolveErr := r.resolvePhasePlan(ctx, &profile, suite, participants)
		if resolveErr != nil {
			return resolveErr
		}

		run, err = newResolvedCertificationRun(
			runName,
			r.Namespace,
			node,
			&profile,
			suite,
			participants,
			resolvedPlan,
			signalv1.ExecutionPlanProvenance{
				ControllerImage:        r.ControllerImage,
				AdapterImage:           r.AdapterImage,
				WorkflowTemplateDigest: r.WorkflowDigest,
				ResolverImages:         slices.Clone(r.ResolverImages),
			},
			startFingerprints,
		)
	} else {
		sourceScripts, loadErr := r.loadScriptBundle(ctx, *suite.Spec.ScriptBundle, checks)
		if loadErr != nil {
			return loadErr
		}

		var checkSnapshots []signalv1.CertificationCheckSnapshot

		snapshot, checkSnapshots, err = r.createSnapshot(ctx, runName, sourceScripts, checks)
		if err == nil {
			run, err = newCertificationRun(
				runName,
				r.Namespace,
				node,
				&profile,
				suite,
				participants,
				snapshot,
				checkSnapshots,
				signalv1.ExecutionPlanProvenance{
					AdapterImage:           r.AdapterImage,
					WorkflowTemplateDigest: r.WorkflowDigest,
				},
				startFingerprints,
			)
		}
	}

	if err != nil {
		return fmt.Errorf("build immutable CertificationRun execution plan: %w", err)
	}

	if _, err := buildWorkflow(run); err != nil {
		var deleteErr error
		if snapshot != nil {
			deleteErr = r.Delete(context.WithoutCancel(ctx), snapshot)
		}

		return errors.Join(fmt.Errorf("validate executable plan: %w", err), client.IgnoreNotFound(deleteErr))
	}

	if err := r.Create(ctx, run); err != nil {
		var deleteErr error
		if snapshot != nil {
			deleteErr = r.Delete(context.WithoutCancel(ctx), snapshot)
		}

		return errors.Join(fmt.Errorf("create CertificationRun: %w", err), client.IgnoreNotFound(deleteErr))
	}

	cleanupFailedAdmission := func(cause error) error {
		releaseOnFailure = false
		deleteRunErr := r.Delete(context.WithoutCancel(ctx), run)

		// The Run finalizer owns rollback once the Run exists, including its snapshot.
		return errors.Join(cause, client.IgnoreNotFound(deleteRunErr))
	}

	if snapshot != nil {
		snapshot.OwnerReferences = []metav1.OwnerReference{
			*metav1.NewControllerRef(run, signalv1.GroupVersion.WithKind("CertificationRun")),
		}
		if err := r.Update(ctx, snapshot); err != nil {
			return cleanupFailedAdmission(fmt.Errorf("bind script snapshot to run: %w", err))
		}
	}

	releaseOnFailure = false

	if err := r.completeRunAdmission(ctx, run); err != nil {
		return err
	}

	if r.Recorder != nil {
		executableCount := len(run.Spec.Checks)
		if run.Spec.ResolvedPlan != nil {
			executableCount = resolvedStageCount(*run.Spec.ResolvedPlan)
		}

		r.Recorder.Eventf(run, nil, corev1.EventTypeNormal, "CertificationAdmitted", "AdmitCertification",
			"Created CertificationRun %s with %d immutable executions", run.Name, executableCount)
	}

	return nil
}

func newCertificationRun(
	runName string,
	namespace string,
	node *corev1.Node,
	profile *signalv1.CertificationProfile,
	suite *signalv1.CertificationSuite,
	participants []signalv1.Participant,
	snapshot *corev1.ConfigMap,
	checkSnapshots []signalv1.CertificationCheckSnapshot,
	provenance signalv1.ExecutionPlanProvenance,
	startFingerprints []signalv1.NodeFingerprint,
) (*signalv1.CertificationRun, error) {
	planDigest, err := signalv1.DeriveExecutionPlanDigest(
		int32(len(participants)),
		suite.Spec.ScriptBundle.Digest,
		checkSnapshots,
		provenance,
	)
	if err != nil {
		return nil, err
	}

	startIdentity, err := json.Marshal(startFingerprints)
	if err != nil {
		return nil, fmt.Errorf("encode participant start fingerprints: %w", err)
	}

	return &signalv1.CertificationRun{
		TypeMeta: metav1.TypeMeta{APIVersion: signalv1.GroupVersion.String(), Kind: "CertificationRun"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      runName,
			Namespace: namespace,
			Labels: map[string]string{
				RunNodeLabel:    node.Name,
				RunProfileLabel: profile.Name,
				RunSuiteLabel:   suite.Name,
			},
			Annotations: map[string]string{
				RunTriggerAnnotation:       "manual",
				RunAdmissionAnnotation:     runAdmissionPending,
				RunStartIdentityAnnotation: string(startIdentity),
			},
			Finalizers: []string{RunFinalizer},
		},
		Spec: signalv1.CertificationRunSpec{
			Profile: signalv1.PinnedProfile{
				Name:       profile.Name,
				UID:        profile.UID,
				Generation: profile.Generation,
			},
			Suite: signalv1.PinnedSuite{
				Name:       suite.Name,
				UID:        suite.UID,
				Generation: suite.Generation,
			},
			Participants: participants,
			ScriptBundle: &signalv1.ScriptBundleReference{
				Name:   snapshot.Name,
				UID:    snapshot.UID,
				Digest: suite.Spec.ScriptBundle.Digest,
			},
			Checks:         checkSnapshots,
			PlanDigest:     planDigest,
			PlanProvenance: &provenance,
		},
	}, nil
}

func newResolvedCertificationRun(
	runName string,
	namespace string,
	node *corev1.Node,
	profile *signalv1.CertificationProfile,
	suite *signalv1.CertificationSuite,
	participants []signalv1.Participant,
	resolvedPlan *signalv1.ResolvedCertificationPlan,
	provenance signalv1.ExecutionPlanProvenance,
	startFingerprints []signalv1.NodeFingerprint,
) (*signalv1.CertificationRun, error) {
	environment := profile.Spec.Environment.DeepCopy()

	planDigest, err := signalv1.DeriveResolvedExecutionPlanDigest(
		int32(len(participants)),
		environment,
		*resolvedPlan,
		provenance,
	)
	if err != nil {
		return nil, err
	}

	startIdentity, err := json.Marshal(startFingerprints)
	if err != nil {
		return nil, fmt.Errorf("encode participant start fingerprints: %w", err)
	}

	return &signalv1.CertificationRun{
		TypeMeta: metav1.TypeMeta{APIVersion: signalv1.GroupVersion.String(), Kind: "CertificationRun"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      runName,
			Namespace: namespace,
			Labels: map[string]string{
				RunNodeLabel:    node.Name,
				RunProfileLabel: profile.Name,
				RunSuiteLabel:   suite.Name,
			},
			Annotations: map[string]string{
				RunTriggerAnnotation:       "manual",
				RunAdmissionAnnotation:     runAdmissionPending,
				RunStartIdentityAnnotation: string(startIdentity),
			},
			Finalizers: []string{RunFinalizer},
		},
		Spec: signalv1.CertificationRunSpec{
			Profile: signalv1.PinnedProfile{
				Name:       profile.Name,
				UID:        profile.UID,
				Generation: profile.Generation,
			},
			Suite: signalv1.PinnedSuite{
				Name:       suite.Name,
				UID:        suite.UID,
				Generation: suite.Generation,
			},
			Participants:   participants,
			Environment:    environment,
			ResolvedPlan:   resolvedPlan.DeepCopy(),
			PlanDigest:     planDigest,
			PlanProvenance: provenance.DeepCopy(),
		},
	}, nil
}

func (r *AdmissionReconciler) resolvePhasePlan(
	ctx context.Context,
	profile *signalv1.CertificationProfile,
	suite *signalv1.CertificationSuite,
	participants []signalv1.Participant,
) (*signalv1.ResolvedCertificationPlan, error) {
	if len(suite.Spec.Phases) == 0 {
		return nil, errors.New("suite has no phase plan")
	}

	resolvedInputs := resolvedEnvironmentInputs(profile.Spec.Environment)
	plan := &signalv1.ResolvedCertificationPlan{
		Phases: make([]signalv1.ResolvedPhasePlan, len(suite.Spec.Phases)),
	}

	for phaseIndex := range suite.Spec.Phases {
		phase := &suite.Spec.Phases[phaseIndex]
		if len(phase.Stages) == 0 {
			return nil, fmt.Errorf("suite %s phase %s has no stages", suite.Name, phase.Name)
		}

		resolvedPhase := &plan.Phases[phaseIndex]
		resolvedPhase.Name = phase.Name
		resolvedPhase.DisplayName = phase.DisplayName
		resolvedPhase.Role = phase.Role
		resolvedPhase.Stages = make([]signalv1.ResolvedStagePlan, len(phase.Stages))

		for stageIndex := range phase.Stages {
			stage := &phase.Stages[stageIndex]

			resolvedStage, err := r.resolveStage(
				ctx,
				profile,
				participants,
				phase.Role,
				stage,
				resolvedInputs,
			)
			if err != nil {
				return nil, fmt.Errorf(
					"suite %s phase %s stage %s: %w",
					suite.Name,
					phase.Name,
					stage.Name,
					err,
				)
			}

			resolvedPhase.Stages[stageIndex] = *resolvedStage
		}
	}

	if _, err := signalv1.DeriveResolvedPlanDeadline(plan.Phases); err != nil {
		return nil, fmt.Errorf("suite %s phase deadlines: %w", suite.Name, err)
	}

	return plan, nil
}

func (r *AdmissionReconciler) resolveStage(
	ctx context.Context,
	profile *signalv1.CertificationProfile,
	participants []signalv1.Participant,
	phaseRole signalv1.CheckStageRole,
	stage *signalv1.CertificationStage,
	environmentInputs []signalv1.ResolvedInputIdentity,
) (*signalv1.ResolvedStagePlan, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}

	var test signalv1.CertificationTest
	if err := reader.Get(ctx, client.ObjectKey{Name: stage.Test.Name}, &test); err != nil {
		return nil, fmt.Errorf("get CertificationTest %s: %w", stage.Test.Name, err)
	}

	if test.UID != stage.Test.UID ||
		test.Generation != stage.Test.Generation {
		return nil, errors.New("CertificationTest identity does not match the Suite reference")
	}

	if test.Status.Trust == nil ||
		test.Status.ObservedGeneration != test.Generation ||
		test.Status.Trust.ContentDigest != stage.Test.ContentDigest {
		return nil, errors.New("CertificationTest trust decision is missing or stale")
	}

	if test.Status.Trust.State == signalv1.CertificationTestTrustRejected ||
		test.Status.Trust.State == signalv1.CertificationTestTrustRevoked {
		return nil, fmt.Errorf("CertificationTest trust state is %s", test.Status.Trust.State)
	}

	if test.Status.Trust.State != signalv1.CertificationTestTrustApproved &&
		test.Status.Trust.State != signalv1.CertificationTestTrustDiagnostic {
		return nil, fmt.Errorf("unsupported CertificationTest trust state %q", test.Status.Trust.State)
	}

	parameters, err := resolvedStageParameters(test.Spec.Parameters, stage.Parameters)
	if err != nil {
		return nil, err
	}

	if err := validateStageAssertions(test.Spec.Metrics, stage.Assertions); err != nil {
		return nil, err
	}

	roles, err := r.resolveStageParticipantRoles(
		ctx,
		profile,
		participants,
		test.Spec.Topology,
		stage.ParticipantRoles,
	)
	if err != nil {
		return nil, err
	}

	inputs := deepCopyResolvedInputs(environmentInputs)
	completeInputs := slices.IndexFunc(inputs, func(input signalv1.ResolvedInputIdentity) bool {
		return input.State != signalv1.ResolvedInputStateComplete
	}) == -1
	approved := test.Status.Trust.State == signalv1.CertificationTestTrustApproved

	parameterSchemaDigest, err := canonicalDigest(test.Spec.Parameters)
	if err != nil {
		return nil, fmt.Errorf("digest parameter schema: %w", err)
	}

	metricSchemaDigest, err := canonicalDigest(test.Spec.Metrics)
	if err != nil {
		return nil, fmt.Errorf("digest metric schema: %w", err)
	}

	securityRequirementsDigest, err := canonicalDigest(test.Spec.Security)
	if err != nil {
		return nil, fmt.Errorf("digest security requirements: %w", err)
	}

	resolved := &signalv1.ResolvedStagePlan{
		Name:        stage.Name,
		DisplayName: stage.DisplayName,
		Test: signalv1.ResolvedCertificationTest{
			Identity: signalv1.TestIdentitySnapshot{
				Name:          test.Name,
				UID:           test.UID,
				Generation:    test.Generation,
				ContentDigest: stage.Test.ContentDigest,
				Trust:         test.Status.Trust.State,
			},
			ParameterSchemaDigest:      parameterSchemaDigest,
			MetricSchemaDigest:         metricSchemaDigest,
			SecurityRequirementsDigest: securityRequirementsDigest,
			Metrics:                    append([]signalv1.TestMetricDefinition(nil), test.Spec.Metrics...),
			Topology:                   *test.Spec.Topology.DeepCopy(),
			Driver:                     *test.Spec.Driver.DeepCopy(),
			Security: signalv1.ResolvedSecurityRequirements{
				Privileged:   test.Spec.Security.Privileged,
				HostNetwork:  test.Spec.Security.HostNetwork,
				IPCLock:      test.Spec.Security.IPCLock,
				Capabilities: slices.Clone(test.Spec.Security.Capabilities),
				HostPaths:    deepCopyHostPaths(test.Spec.Security.HostPaths),
			},
		},
		Parameters:                parameters,
		ParticipantRoles:          roles,
		Schedule:                  *stage.Schedule.DeepCopy(),
		Retry:                     stage.Retry.DeepCopy(),
		Optional:                  stage.Optional,
		Assertions:                append([]signalv1.TestAssertion(nil), stage.Assertions...),
		PerDeviceCoverageInterval: stage.PerDeviceCoverageInterval.DeepCopy(),
		ResolvedInputs:            inputs,
		QualificationEligible:     approved && completeInputs,
		DiagnosticOnly:            !approved || !completeInputs,
	}

	if phaseRole != signalv1.CheckStageRoleValidation &&
		resolved.Schedule.Mode != signalv1.CheckExecutionModeRunOnce {
		return nil, errors.New("startup and terminal stages must use RunOnce mode")
	}

	return resolved, nil
}

func canonicalDigest(value any) (string, error) {
	content, err := json.Marshal(value)
	if err != nil {
		return "", err
	}

	return digestBytes(content), nil
}

func resolvedStageParameters(
	definitions []signalv1.TestParameterDefinition,
	bindings []signalv1.StageParameterBinding,
) ([]signalv1.StageParameterBinding, error) {
	bound := make(map[string]signalv1.TypedValue, len(bindings))
	for _, binding := range bindings {
		if _, exists := bound[binding.Name]; exists {
			return nil, fmt.Errorf("parameter %s is bound more than once", binding.Name)
		}

		bound[binding.Name] = *binding.Value.DeepCopy()
	}

	result := make([]signalv1.StageParameterBinding, 0, len(definitions))
	for index := range definitions {
		definition := &definitions[index]

		value, exists := bound[definition.Name]
		if !exists && definition.Default != nil {
			value = *definition.Default.DeepCopy()
			exists = true
		}

		if !exists {
			if definition.Required {
				return nil, fmt.Errorf("required parameter %s is not bound", definition.Name)
			}

			continue
		}

		if value.Type != definition.Type {
			return nil, fmt.Errorf(
				"parameter %s type %s does not match %s",
				definition.Name,
				value.Type,
				definition.Type,
			)
		}

		result = append(result, signalv1.StageParameterBinding{Name: definition.Name, Value: value})
		delete(bound, definition.Name)
	}

	if len(bound) != 0 {
		names := make([]string, 0, len(bound))
		for name := range bound {
			names = append(names, name)
		}

		sort.Strings(names)

		return nil, fmt.Errorf("unknown parameter bindings: %s", strings.Join(names, ", "))
	}

	return result, nil
}

func validateStageAssertions(
	metrics []signalv1.TestMetricDefinition,
	assertions []signalv1.TestAssertion,
) error {
	known := make(map[string]struct{}, len(metrics))
	for _, metric := range metrics {
		known[metric.Name] = struct{}{}
	}

	for _, assertion := range assertions {
		if _, exists := known[assertion.Metric]; !exists {
			return fmt.Errorf(
				"assertion %s references unknown metric %s",
				assertion.Name,
				assertion.Metric,
			)
		}
	}

	return nil
}

func resolvedEnvironmentInputs(
	environment *signalv1.CertificationEnvironmentBindings,
) []signalv1.ResolvedInputIdentity {
	if environment == nil {
		return nil
	}

	result := make([]signalv1.ResolvedInputIdentity, 0,
		len(environment.Resolvers)+len(environment.ModelCaches)+len(environment.ObjectStores))
	appendReferences := func(kind signalv1.ResolvedInputKind, values []signalv1.NamedEnvironmentReference) {
		for _, value := range values {
			result = append(result, resolvedNamedInput(kind, value))
		}
	}

	appendReferences(signalv1.ResolvedInputKindResolverOutput, environment.Resolvers)
	appendReferences(signalv1.ResolvedInputKindModel, environment.ModelCaches)
	appendReferences(signalv1.ResolvedInputKindArtifact, environment.ObjectStores)

	return result
}

func resolvedNamedInput(
	kind signalv1.ResolvedInputKind,
	value signalv1.NamedEnvironmentReference,
) signalv1.ResolvedInputIdentity {
	input := signalv1.ResolvedInputIdentity{
		Name:             value.Name,
		Kind:             kind,
		Requested:        value.Reference,
		State:            signalv1.ResolvedInputStateIncomplete,
		IncompleteReason: "environment reference is not an immutable digest identity",
	}

	digestIndex := strings.LastIndex(value.Reference, "@sha256:")
	if digestIndex >= 0 {
		digest := value.Reference[digestIndex+1:]
		if signalv1.IsSHA256Digest(digest) {
			input.State = signalv1.ResolvedInputStateComplete
			input.Resolved = value.Reference
			input.Digest = digest
			input.IncompleteReason = ""
		}
	}

	return input
}

func deepCopyResolvedInputs(values []signalv1.ResolvedInputIdentity) []signalv1.ResolvedInputIdentity {
	result := make([]signalv1.ResolvedInputIdentity, len(values))
	for index := range values {
		values[index].DeepCopyInto(&result[index])
	}

	return result
}

func deepCopyHostPaths(values []signalv1.WorkloadHostPath) []signalv1.WorkloadHostPath {
	result := make([]signalv1.WorkloadHostPath, len(values))
	for index := range values {
		values[index].DeepCopyInto(&result[index])
	}

	return result
}

func ValidateExecutionProvenance(controllerImage, adapterImage, workflowDigest string) error {
	if !signalv1.IsDigestPinnedImage(controllerImage) {
		return errors.New("controller image must use an immutable sha256 digest")
	}

	return validatePlanProvenance(adapterImage, workflowDigest)
}

func validatePlanProvenance(adapterImage, workflowDigest string) error {
	if !signalv1.IsDigestPinnedImage(adapterImage) {
		return errors.New("adapter image must use an immutable sha256 digest")
	}

	if !signalv1.IsSHA256Digest(workflowDigest) {
		return errors.New("workflow template digest must be an algorithm-qualified sha256 digest")
	}

	return nil
}

func (r *AdmissionReconciler) resolveSuite(
	ctx context.Context,
	profile *signalv1.CertificationProfile,
) (*signalv1.CertificationSuite, []signalv1.CertificationCheck, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}

	var suite signalv1.CertificationSuite
	if err := reader.Get(ctx, client.ObjectKey{Name: profile.Spec.Suite.Name}, &suite); err != nil {
		return nil, nil, fmt.Errorf(
			"get CertificationSuite %s referenced by profile %s: %w",
			profile.Spec.Suite.Name,
			profile.Name,
			err,
		)
	}

	checks := suiteChecks(&suite)
	if len(suite.Spec.Phases) > 0 {
		if len(checks) != 0 {
			return nil, nil, fmt.Errorf("suite %s configures both phases and legacy checks", suite.Name)
		}

		if err := validateSourcePhases(suite.Spec.Phases); err != nil {
			return nil, nil, fmt.Errorf("suite %s phases: %w", suite.Name, err)
		}

		return &suite, nil, nil
	}

	if _, err := validateCheckPlan(checks); err != nil {
		return nil, nil, fmt.Errorf("suite %s checks: %w", suite.Name, err)
	}

	for index := range checks {
		check := &checks[index]
		if err := signalworkload.Validate(check.WorkloadPolicy); err != nil {
			return nil, nil, fmt.Errorf("suite %s check %s workload policy: %w", suite.Name, check.ID, err)
		}
	}

	return &suite, checks, nil
}

func validateSourcePhases(phases []signalv1.CertificationPhase) error {
	if len(phases) == 0 {
		return errors.New("at least one phase is required")
	}

	phaseNames := make(map[string]struct{}, len(phases))
	stageCount := 0
	startupCount := 0
	terminalCount := 0

	for phaseIndex := range phases {
		phase := &phases[phaseIndex]
		if _, exists := phaseNames[phase.Name]; exists {
			return fmt.Errorf("phase name %s is duplicated", phase.Name)
		}

		phaseNames[phase.Name] = struct{}{}
		switch phase.Role {
		case signalv1.CheckStageRoleStartup:
			startupCount++

			if phaseIndex != 0 {
				return errors.New("startup phase must be first")
			}
		case signalv1.CheckStageRoleValidation:
		case signalv1.CheckStageRoleTerminal:
			terminalCount++

			if phaseIndex != len(phases)-1 {
				return errors.New("terminal phase must be last")
			}
		default:
			return fmt.Errorf("phase %s has invalid role %q", phase.Name, phase.Role)
		}

		if len(phase.Stages) == 0 {
			return fmt.Errorf("phase %s has no stages", phase.Name)
		}

		stageNames := make(map[string]struct{}, len(phase.Stages))

		stageCount += len(phase.Stages)
		for stageIndex := range phase.Stages {
			stage := &phase.Stages[stageIndex]
			if _, exists := stageNames[stage.Name]; exists {
				return fmt.Errorf("phase %s stage name %s is duplicated", phase.Name, stage.Name)
			}

			stageNames[stage.Name] = struct{}{}
			if err := signalv1.ValidateStageSchedule(stage.Schedule); err != nil {
				return fmt.Errorf("phase %s stage %s schedule: %w", phase.Name, stage.Name, err)
			}

			if phase.Role != signalv1.CheckStageRoleValidation &&
				stage.Schedule.Mode != signalv1.CheckExecutionModeRunOnce {
				return fmt.Errorf("phase %s non-validation stage %s must use RunOnce", phase.Name, stage.Name)
			}
		}
	}

	if startupCount > 1 || terminalCount > 1 {
		return errors.New("at most one startup and one terminal phase are allowed")
	}

	if stageCount > signalv1.MaxCertificationStages {
		return fmt.Errorf("stage count %d exceeds maximum %d", stageCount, signalv1.MaxCertificationStages)
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
	checks []signalv1.CertificationCheck,
) ([]signalv1.Participant, error) {
	count := profile.Spec.Participants.Count
	if count < 1 {
		return nil, fmt.Errorf("profile %s participant count must be positive", profile.Name)
	}

	if count > maxAdapterParticipants {
		return nil, fmt.Errorf(
			"profile %s participant count must not exceed %d",
			profile.Name,
			maxAdapterParticipants,
		)
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

	minimumAllocatable := minimumAllocatableForChecks(checks)
	if !selector.Matches(labels.Set(requestNode.Labels)) ||
		!nodeEligible(requestNode, minimumAllocatable) {
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
			!nodeEligible(candidate, minimumAllocatable) ||
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

	participants := make([]signalv1.Participant, count)
	for index := range participants {
		participants[index] = participantSnapshot(&candidates[index], int32(index))
	}

	return participants, nil
}

func (r *AdmissionReconciler) resolveStageParticipantRoles(
	ctx context.Context,
	profile *signalv1.CertificationProfile,
	participants []signalv1.Participant,
	topology signalv1.TestTopology,
	stageBindings []signalv1.StageParticipantRoleBinding,
) ([]signalv1.ResolvedStageParticipantRoleBinding, error) {
	if len(topology.Roles) == 0 {
		if len(stageBindings) != 0 {
			return nil, errors.New("participant role bindings require a role-aware CertificationTest")
		}

		return nil, nil
	}

	profileRoles := make(map[string]signalv1.ProfileParticipantRole, len(profile.Spec.ParticipantRoles))
	for _, role := range profile.Spec.ParticipantRoles {
		profileRoles[role.Role] = role
	}

	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}

	nodes := make([]corev1.Node, len(participants))
	for index := range participants {
		if err := reader.Get(
			ctx,
			client.ObjectKey{Name: participants[index].NodeName},
			&nodes[index],
		); err != nil {
			return nil, fmt.Errorf("read participant %s for role binding: %w", participants[index].NodeName, err)
		}

		if nodes[index].UID != participants[index].NodeUID {
			return nil, fmt.Errorf("participant %s identity changed during role binding", participants[index].NodeName)
		}
	}

	assigned := make(map[int32]struct{}, len(participants))
	result := make([]signalv1.ResolvedStageParticipantRoleBinding, 0, len(stageBindings))

	for _, binding := range stageBindings {
		profileRole, found := profileRoles[binding.Role]
		if !found {
			return nil, fmt.Errorf("stage role %s has no Profile binding", binding.Role)
		}

		if binding.Count > profileRole.Count {
			return nil, fmt.Errorf(
				"stage role %s requires %d participants, Profile binds %d",
				binding.Role,
				binding.Count,
				profileRole.Count,
			)
		}

		selector := labels.Everything()

		if !reflect.DeepEqual(profileRole.Selector, metav1.LabelSelector{}) {
			var err error

			selector, err = metav1.LabelSelectorAsSelector(&profileRole.Selector)
			if err != nil {
				return nil, fmt.Errorf("profile role %s selector: %w", binding.Role, err)
			}
		}

		indexes := make([]int32, 0, binding.Count)

		for index := range nodes {
			participantIndex := int32(index)
			if _, exists := assigned[participantIndex]; exists ||
				!selector.Matches(labels.Set(nodes[index].Labels)) {
				continue
			}

			assigned[participantIndex] = struct{}{}

			indexes = append(indexes, participantIndex)
			if int32(len(indexes)) == binding.Count {
				break
			}
		}

		if int32(len(indexes)) != binding.Count {
			return nil, fmt.Errorf(
				"stage role %s requires %d matching selected participants, found %d",
				binding.Role,
				binding.Count,
				len(indexes),
			)
		}

		result = append(result, signalv1.ResolvedStageParticipantRoleBinding{
			Role:               binding.Role,
			ParticipantIndexes: indexes,
		})
	}

	return result, nil
}

func (r *AdmissionReconciler) captureParticipantFingerprints(
	ctx context.Context,
	participants []signalv1.Participant,
) ([]signalv1.NodeFingerprint, error) {
	fingerprints := make([]signalv1.NodeFingerprint, len(participants))

	for index := range participants {
		var node corev1.Node
		if err := r.APIReader.Get(
			ctx,
			client.ObjectKey{Name: participants[index].NodeName},
			&node,
		); err != nil {
			return nil, fmt.Errorf("read participant %s fingerprint: %w", participants[index].NodeName, err)
		}

		if node.UID != participants[index].NodeUID {
			return nil, fmt.Errorf(
				"participant %s UID changed from %s to %s",
				participants[index].NodeName,
				participants[index].NodeUID,
				node.UID,
			)
		}

		fingerprints[index] = nodeBaseFingerprint(&node)
	}

	return fingerprints, nil
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
		Index:    index,
		NodeName: node.Name,
		NodeUID:  node.UID,
	}
}

func minimumAllocatableForChecks(checks []signalv1.CertificationCheck) corev1.ResourceList {
	result := corev1.ResourceList{}

	for index := range checks {
		resources := checks[index].WorkloadPolicy.Resources

		effectiveRequests := resources.Requests.DeepCopy()
		if effectiveRequests == nil {
			effectiveRequests = corev1.ResourceList{}
		}

		for name, limit := range resources.Limits {
			if _, exists := effectiveRequests[name]; !exists {
				effectiveRequests[name] = limit.DeepCopy()
			}
		}

		if checks[index].GPUCount > 0 {
			effectiveRequests[corev1.ResourceName("nvidia.com/gpu")] = *resource.NewQuantity(
				int64(checks[index].GPUCount),
				resource.DecimalSI,
			)
		}

		for name, quantity := range effectiveRequests {
			current, exists := result[name]
			if !exists || quantity.Cmp(current) > 0 {
				result[name] = quantity.DeepCopy()
			}
		}
	}

	return result
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

func (r *AdmissionReconciler) loadScriptBundle(
	ctx context.Context,
	scriptBundle signalv1.ScriptBundleSource,
	checks []signalv1.CertificationCheck,
) (*corev1.ConfigMap, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}

	var source corev1.ConfigMap
	if err := reader.Get(
		ctx,
		client.ObjectKey{Namespace: r.ScriptNamespace, Name: scriptBundle.Name},
		&source,
	); err != nil {
		return nil, fmt.Errorf("get source script ConfigMap: %w", err)
	}

	if source.Immutable == nil || !*source.Immutable {
		return nil, fmt.Errorf("source script ConfigMap %s must be immutable", source.Name)
	}

	if len(source.BinaryData) != 0 {
		return nil, fmt.Errorf("source script ConfigMap %s must not contain binaryData", source.Name)
	}

	derivedBundleDigest, err := signalv1.DeriveScriptBundleDigest(checks, source.Data)
	if err != nil {
		return nil, fmt.Errorf("derive source script bundle digest: %w", err)
	}

	if derivedBundleDigest != scriptBundle.Digest {
		return nil, fmt.Errorf(
			"source script ConfigMap %s digest %s does not match pinned digest %s",
			source.Name,
			derivedBundleDigest,
			scriptBundle.Digest,
		)
	}

	return &source, nil
}

func (r *AdmissionReconciler) createSnapshot(
	ctx context.Context,
	runName string,
	source *corev1.ConfigMap,
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

		content, ok := source.Data[check.ScriptKey]
		if !ok {
			return nil, nil, fmt.Errorf(
				"source script ConfigMap %s has no key %s for check %s",
				source.Name,
				check.ScriptKey,
				check.ID,
			)
		}

		digest := digestString(content)
		key := fmt.Sprintf("%02d-%s", check.Order, sanitizeTaskName(string(check.ID)))

		snapshot.Data[key] = content
		checkSnapshots[index] = signalv1.CertificationCheckSnapshot{
			ID:    check.ID,
			Order: check.Order,
			Script: signalv1.ScriptSnapshot{
				Key:    key,
				Digest: digest,
			},
			RunnerImage:           r.RunnerImage,
			Backend:               check.Backend,
			WorkloadPolicy:        *check.WorkloadPolicy.DeepCopy(),
			GPUCount:              check.GPUCount,
			BackendActiveDeadline: check.BackendActiveDeadline,
			MinDuration:           check.MinDuration.DeepCopy(),
			Continuity:            check.Continuity.DeepCopy(),
		}
	}

	if err := r.Create(ctx, snapshot); err != nil {
		return nil, nil, fmt.Errorf("create immutable script snapshot: %w", err)
	}

	return snapshot, checkSnapshots, nil
}

func ValidateRunnerImage(image string) error {
	if !runnerImageDigestPattern.MatchString(image) {
		return errors.New("runner image must use an immutable sha256 digest")
	}

	return nil
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

		checkStatuses := compactCheckStatuses(runningCheckStatuses(workflow, &run))

		if run.Status.Phase != signalv1.RunPhaseRunning ||
			run.Status.Workflow.Phase != phase ||
			!reflect.DeepEqual(run.Status.Checks, checkStatuses) {
			before := run.DeepCopy()
			run.Status.Phase = signalv1.RunPhaseRunning

			run.Status.Workflow.Phase = phase

			run.Status.Checks = checkStatuses

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
	if run.Spec.ResolvedPlan != nil {
		if run.Spec.ScriptBundle != nil || len(run.Spec.Checks) != 0 {
			return errors.New("resolved plan run contains legacy script fields")
		}

		return nil
	}

	if run.Spec.ScriptBundle == nil {
		return errors.New("legacy run is missing its script bundle")
	}

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

	derivedBundleDigest, err := signalv1.DeriveScriptSnapshotBundleDigest(run.Spec.Checks)
	if err != nil {
		return fmt.Errorf("derive immutable script bundle digest: %w", err)
	}

	if derivedBundleDigest != run.Spec.ScriptBundle.Digest {
		return errors.New("script bundle aggregate digest does not match the run")
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
		return r.finishInvalidExecutionPlan(ctx, run, err)
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

func (r *RunReconciler) finishInvalidExecutionPlan(
	ctx context.Context,
	run *signalv1.CertificationRun,
	cause error,
) (ctrl.Result, error) {
	reason := "InvalidExecutionPlan: " + cause.Error()
	evidenceRun := run.DeepCopy()
	evidenceRun.Status.Phase = signalv1.RunPhaseInconclusive
	evidenceRun.Status.Reason = reason
	evidenceRun.Status.Workflow = signalv1.WorkflowStatus{
		Name:  run.Name + "-a1",
		Phase: "Error",
	}
	evidenceRun.Status.Checks = initialCheckStatuses(run.Spec.Checks)

	for index := range evidenceRun.Status.Checks {
		evidenceRun.Status.Checks[index].Phase = signalv1.CheckPhaseInconclusive
		evidenceRun.Status.Checks[index].Verdict = "Inconclusive"
		evidenceRun.Status.Checks[index].Reason = "InvalidExecutionPlan"
	}

	startedAt := groupedStartedAt(nil, run)
	finishedAt := metav1.NewTime(r.now())

	evidence, err := r.evidenceDocument(
		ctx,
		evidenceRun,
		nil,
		"Error",
		"Inconclusive",
		reason,
		startedAt,
		finishedAt,
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
	run.Status.Checks = compactCheckStatuses(evidenceRun.Status.Checks)
	run.Status.Evidence = signalv1.EvidenceStatus{URI: uri, Digest: digest}
	run.Status.Reason = boundedStatusText(reason)

	if err := r.Status().Patch(ctx, run, client.MergeFrom(before)); err != nil {
		return ctrl.Result{}, err
	}

	if err := projectTerminal(ctx, r.Client, run, finishedAt.Time); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: time.Millisecond}, nil
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
	if run.Spec.ResolvedPlan != nil {
		return buildResolvedWorkflow(run)
	}

	checks := run.Spec.Checks

	suiteDeadline, err := validateRunCheckPlan(checks)
	if err != nil {
		return nil, err
	}

	name := run.Name + "-a1"
	tasks := make([]any, 0, len(checks))

	for index := range checks {
		check := &checks[index]

		taskName := checkTaskName(check)
		task := map[string]any{
			"name": taskName,
			"templateRef": map[string]any{
				"name":     adapterWorkflowTemplateName,
				"template": "certification",
			},
			"arguments": map[string]any{
				"parameters": checkWorkflowParameters(run, check),
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
			"activeDeadlineSeconds":        int64(suiteDeadline.Seconds()),
			"serviceAccountName":           adapterServiceAccountName,
			"automountServiceAccountToken": true,
			"entrypoint":                   "certification-suite",
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

	if err := validateWorkflowSize(workflow); err != nil {
		return nil, err
	}

	return workflow, nil
}

func buildResolvedWorkflow(run *signalv1.CertificationRun) (*unstructured.Unstructured, error) {
	if run.Spec.ResolvedPlan == nil {
		return nil, errors.New("run has no resolved plan")
	}

	deadline, err := signalv1.DeriveResolvedPlanDeadline(run.Spec.ResolvedPlan.Phases)
	if err != nil {
		return nil, err
	}

	tasks := make([]any, 0, resolvedStageCount(*run.Spec.ResolvedPlan))

	var previousPhase []struct {
		name     string
		optional bool
	}

	for phaseIndex := range run.Spec.ResolvedPlan.Phases {
		phase := &run.Spec.ResolvedPlan.Phases[phaseIndex]
		currentPhaseNames := make([]string, len(phase.Stages))

		for stageIndex := range phase.Stages {
			stage := &phase.Stages[stageIndex]
			taskName := stageTaskName(int32(phaseIndex), int32(stageIndex), stage.Name)
			currentPhaseNames[stageIndex] = taskName
			task := map[string]any{
				"name": taskName,
				"templateRef": map[string]any{
					"name":     adapterWorkflowTemplateName,
					"template": "certification-stage",
				},
				"arguments": map[string]any{
					"parameters": stageWorkflowParameters(
						run,
						int32(phaseIndex),
						int32(stageIndex),
						stage,
					),
				},
			}

			if len(previousPhase) > 0 {
				dependencies := make([]string, len(previousPhase))
				for index, previous := range previousPhase {
					if previous.optional {
						dependencies[index] = fmt.Sprintf(
							"(%s.Succeeded || %s.Failed || %s.Errored || %s.Skipped || %s.Omitted)",
							previous.name,
							previous.name,
							previous.name,
							previous.name,
							previous.name,
						)
					} else {
						dependencies[index] = previous.name + ".Succeeded"
					}
				}

				task["depends"] = strings.Join(dependencies, " && ")
			}

			tasks = append(tasks, task)
		}

		previousPhase = make([]struct {
			name     string
			optional bool
		}, len(currentPhaseNames))
		for index := range currentPhaseNames {
			previousPhase[index].name = currentPhaseNames[index]
			previousPhase[index].optional = phase.Stages[index].Optional
		}
	}

	workflow := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": workflowGVK.GroupVersion().String(),
		"kind":       workflowGVK.Kind,
		"metadata": map[string]any{
			"name":      run.Name + "-a1",
			"namespace": run.Namespace,
			"labels": map[string]any{
				SnapshotRunLabel: run.Name,
				RunUIDLabel:      string(run.UID),
			},
		},
		"spec": map[string]any{
			"activeDeadlineSeconds":        int64(deadline.Seconds()),
			"serviceAccountName":           adapterServiceAccountName,
			"automountServiceAccountToken": true,
			"entrypoint":                   "certification-suite",
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

	if err := validateWorkflowSize(workflow); err != nil {
		return nil, err
	}

	return workflow, nil
}

func stageWorkflowParameters(
	run *signalv1.CertificationRun,
	phaseIndex int32,
	stageIndex int32,
	stage *signalv1.ResolvedStagePlan,
) []any {
	return []any{
		map[string]any{"name": "run-name", "value": run.Name},
		map[string]any{
			"name":  "execution-name",
			"value": stageRunName(run.Name, phaseIndex, stageIndex, stage.Name),
		},
		map[string]any{"name": "run-uid", "value": string(run.UID)},
		map[string]any{"name": "phase-index", "value": strconv.FormatInt(int64(phaseIndex), 10)},
		map[string]any{"name": "stage-index", "value": strconv.FormatInt(int64(stageIndex), 10)},
		map[string]any{
			"name":  "result-configmap",
			"value": stageResultName(run.Name, phaseIndex, stageIndex),
		},
	}
}

func resolvedStageCount(plan signalv1.ResolvedCertificationPlan) int {
	count := 0
	for index := range plan.Phases {
		count += len(plan.Phases[index].Stages)
	}

	return count
}

func stageTaskName(phaseIndex, stageIndex int32, name string) string {
	return sanitizeTaskName(fmt.Sprintf("p%02d-s%02d-%s", phaseIndex+1, stageIndex+1, name))
}

func stageRunName(runName string, phaseIndex, stageIndex int32, name string) string {
	return boundedDNSName(
		fmt.Sprintf("%s-p%02d-s%02d-%s", runName, phaseIndex+1, stageIndex+1, sanitizeTaskName(name)),
	)
}

func stageResultName(runName string, phaseIndex, stageIndex int32) string {
	return boundedDNSName(fmt.Sprintf("%s-p%02d-s%02d-result", runName, phaseIndex+1, stageIndex+1))
}

func checkWorkflowParameters(
	run *signalv1.CertificationRun,
	check *signalv1.CertificationCheckSnapshot,
) []any {
	// Keep the Workflow bounded: the adapter resolves the immutable Run snapshot
	// and writes the full result to the named immutable ConfigMap.
	return []any{
		map[string]any{"name": "run-name", "value": run.Name},
		map[string]any{"name": "execution-name", "value": checkRunName(run.Name, check.ID)},
		map[string]any{"name": "run-uid", "value": string(run.UID)},
		map[string]any{"name": "check-index", "value": strconv.FormatInt(int64(check.Order), 10)},
		map[string]any{"name": "result-configmap", "value": checkResultName(run.Name, check.ID)},
	}
}

func validateWorkflowSize(workflow *unstructured.Unstructured) error {
	content, err := json.Marshal(workflow.Object)
	if err != nil {
		return fmt.Errorf("encode Workflow: %w", err)
	}

	if len(content) > maximumWorkflowSpecBytes {
		return fmt.Errorf(
			"workflow object is %d bytes, maximum is %d",
			len(content),
			maximumWorkflowSpecBytes,
		)
	}

	return nil
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

func (r *RunReconciler) finishGrouped(
	ctx context.Context,
	run *signalv1.CertificationRun,
	workflow *unstructured.Unstructured,
	workflowPhase string,
) (ctrl.Result, error) {
	if run.Spec.ResolvedPlan != nil {
		return r.finishResolved(ctx, run, workflow, workflowPhase)
	}

	checks, verdict, reason := r.terminalCheckStatuses(ctx, workflow, run)
	executionState := "Completed"

	if workflowPhase != "Succeeded" {
		executionState = workflowPhase
		verdict = "Inconclusive"

		if reason == "" {
			reason = "Workflow" + workflowPhase + ": " + workflowMessage(workflow)
		}
	}

	identity, enforcedVerdict, enforcedReason, err := r.qualificationEvidenceIdentity(ctx, run, checks, verdict, reason)
	if err != nil {
		return ctrl.Result{}, err
	}

	verdict = enforcedVerdict
	reason = enforcedReason

	evidenceRun := run.DeepCopy()
	evidenceRun.Status.Checks = checks
	evidenceRun.Status.Reason = reason
	evidenceRun.Status.Phase = runPhaseForVerdict(verdict)
	evidenceRun.Status.Workflow.Phase = workflowPhase
	evidenceRun.Status.Evidence.Identity = identity

	startedAt := groupedStartedAt(workflow, run)
	finishedAt := groupedFinishedAt(workflow, run)

	evidence, err := r.evidenceDocument(
		ctx,
		evidenceRun,
		nil,
		executionState,
		verdict,
		reason,
		startedAt,
		finishedAt,
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
	run.Status.Evidence = signalv1.EvidenceStatus{URI: uri, Digest: digest, Identity: identity}
	run.Status.Phase = runPhaseForVerdict(verdict)
	run.Status.Reason = boundedStatusText(reason)

	if err := r.Status().Patch(ctx, run, client.MergeFrom(before)); err != nil {
		return ctrl.Result{}, err
	}

	if err := projectTerminal(ctx, r.Client, run, finishedAt.Time); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: time.Millisecond}, nil
}

func (r *RunReconciler) finishResolved(
	ctx context.Context,
	run *signalv1.CertificationRun,
	workflow *unstructured.Unstructured,
	workflowPhase string,
) (ctrl.Result, error) {
	stages, runnerImages, verdict, reason := r.terminalStageSummaries(ctx, workflow, run)
	verdict, reason = resolvedWorkflowVerdict(workflow, workflowPhase, verdict, reason)

	identity, enforcedVerdict, enforcedReason, err := r.resolvedQualificationEvidenceIdentity(
		ctx,
		run,
		stages,
		runnerImages,
		verdict,
		reason,
	)
	if err != nil {
		return ctrl.Result{}, err
	}

	verdict = enforcedVerdict
	reason = enforcedReason
	evidenceRun := run.DeepCopy()
	evidenceRun.Status.Reason = reason
	evidenceRun.Status.Phase = runPhaseForVerdict(verdict)
	evidenceRun.Status.Workflow.Phase = workflowPhase
	evidenceRun.Status.Evidence.Identity = identity

	startedAt := groupedStartedAt(workflow, run)
	finishedAt := groupedFinishedAt(workflow, run)

	evidence, err := r.evidenceDocument(
		ctx,
		evidenceRun,
		stages,
		"Completed",
		verdict,
		reason,
		startedAt,
		finishedAt,
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
	run.Status.Evidence = signalv1.EvidenceStatus{URI: uri, Digest: digest, Identity: identity}
	run.Status.Phase = runPhaseForVerdict(verdict)
	run.Status.Reason = boundedStatusText(reason)

	if err := r.Status().Patch(ctx, run, client.MergeFrom(before)); err != nil {
		return ctrl.Result{}, err
	}

	if err := projectTerminal(ctx, r.Client, run, finishedAt.Time); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: time.Millisecond}, nil
}

func resolvedWorkflowVerdict(
	workflow *unstructured.Unstructured,
	workflowPhase string,
	verdict string,
	reason string,
) (string, string) {
	if workflowPhase != "Succeeded" && verdict != "Fail" {
		return "Inconclusive", "Workflow" + workflowPhase + ": " + workflowMessage(workflow)
	}

	return verdict, reason
}

func (r *RunReconciler) terminalStageSummaries(
	ctx context.Context,
	workflow *unstructured.Unstructured,
	run *signalv1.CertificationRun,
) ([]signalv1.StageEvidenceSummary, []string, string, string) {
	stages := make([]signalv1.StageEvidenceSummary, 0, resolvedStageCount(*run.Spec.ResolvedPlan))
	runnerImages := make([]string, 0)
	seenRunnerImages := map[string]struct{}{}
	verdict := "Pass"
	reason := "AllRequiredStagesPassed"

	for phaseIndex := range run.Spec.ResolvedPlan.Phases {
		phase := &run.Spec.ResolvedPlan.Phases[phaseIndex]
		for stageIndex := range phase.Stages {
			stage := &phase.Stages[stageIndex]

			summary, stageRunnerImages, err := r.readStageResult(
				ctx,
				workflow,
				run,
				int32(phaseIndex),
				int32(stageIndex),
				phase.Role,
				stage,
			)
			if err != nil {
				if stage.Optional {
					continue
				}

				if verdict != "Fail" {
					verdict = "Inconclusive"
					reason = fmt.Sprintf(
						"StageEvidenceInvalid: phase %s stage %s: %v",
						phase.Name,
						stage.Name,
						err,
					)
				}

				continue
			}

			stages = append(stages, *summary)

			for _, image := range stageRunnerImages {
				if _, exists := seenRunnerImages[image]; exists {
					continue
				}

				seenRunnerImages[image] = struct{}{}
				runnerImages = append(runnerImages, image)
			}

			if stage.Optional {
				continue
			}

			switch summary.Verdict {
			case signalv1.StageVerdictPassed:
			case signalv1.StageVerdictFailed:
				verdict = "Fail"
				reason = fmt.Sprintf("RequiredStageFailed: phase %s stage %s", phase.Name, stage.Name)
			default:
				if verdict != "Fail" {
					verdict = "Inconclusive"
					reason = fmt.Sprintf(
						"RequiredStageInconclusive: phase %s stage %s",
						phase.Name,
						stage.Name,
					)
				}
			}
		}
	}

	sort.Strings(runnerImages)

	return stages, runnerImages, verdict, reason
}

func (r *RunReconciler) readStageResult(
	ctx context.Context,
	workflow *unstructured.Unstructured,
	run *signalv1.CertificationRun,
	phaseIndex int32,
	stageIndex int32,
	phaseRole signalv1.CheckStageRole,
	stage *signalv1.ResolvedStagePlan,
) (*signalv1.StageEvidenceSummary, []string, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}

	var result corev1.ConfigMap

	name := stageResultName(run.Name, phaseIndex, stageIndex)
	if err := reader.Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: name}, &result); err != nil {
		return nil, nil, fmt.Errorf("read immutable stage result ConfigMap: %w", err)
	}

	if result.Immutable == nil || !*result.Immutable ||
		result.Labels[SnapshotRunLabel] != run.Name ||
		result.Annotations[stageResultRunUID] != string(run.UID) ||
		result.Annotations[stageResultPlanDigest] != run.Spec.PlanDigest ||
		result.Annotations[stageResultPhaseOrder] != strconv.FormatInt(int64(phaseIndex), 10) ||
		result.Annotations[stageResultStageOrder] != strconv.FormatInt(int64(stageIndex), 10) {
		return nil, nil, errors.New("stage result identity does not match the Run")
	}

	content, found := result.BinaryData[checkResultDataKey]
	if !found || len(content) == 0 || len(content) > maximumCheckResultBytes {
		return nil, nil, errors.New("stage result payload is missing or oversized")
	}

	if result.Annotations[stageResultDigest] != digestBytes(content) {
		return nil, nil, errors.New("stage result digest does not match its payload")
	}

	if !metav1.IsControlledBy(&result, workflow) {
		return nil, nil, errors.New("stage result is not owned by the Run Workflow")
	}

	var summary signalv1.StageEvidenceSummary
	if err := json.Unmarshal(content, &summary); err != nil {
		return nil, nil, fmt.Errorf("decode stage result: %w", err)
	}

	if err := validateStageEvidenceSummary(
		&summary,
		phaseIndex,
		stageIndex,
		phaseRole,
		stage,
		int32(len(run.Spec.Participants)),
	); err != nil {
		return nil, nil, err
	}

	runnerImages, err := stageRunnerImageProvenance(
		result.Annotations[stageResultRunnerImages],
		stage.Test.Driver,
	)
	if err != nil {
		return nil, nil, err
	}

	return &summary, runnerImages, nil
}

func stageRunnerImageProvenance(
	value string,
	driver signalv1.CertificationTestDriver,
) ([]string, error) {
	var images []string
	if err := json.Unmarshal([]byte(value), &images); err != nil {
		return nil, errors.New("stage result runner image provenance is missing or invalid")
	}

	if len(images) == 0 || len(images) > 32 {
		return nil, errors.New("stage result runner image provenance must contain between 1 and 32 images")
	}

	seen := make(map[string]struct{}, len(images))
	for _, image := range images {
		if !signalv1.IsDigestPinnedImage(image) {
			return nil, fmt.Errorf("stage result runner image %q is not digest pinned", image)
		}

		if _, exists := seen[image]; exists {
			return nil, fmt.Errorf("stage result repeats runner image %q", image)
		}

		seen[image] = struct{}{}
	}

	expected := resolvedTestRunnerImage(driver)
	if expected != "" {
		if _, found := seen[expected]; !found {
			return nil, errors.New("stage result omits the immutable planned runner image")
		}
	}

	sort.Strings(images)

	return images, nil
}

func validateStageEvidenceSummary(
	summary *signalv1.StageEvidenceSummary,
	phaseIndex int32,
	stageIndex int32,
	phaseRole signalv1.CheckStageRole,
	stage *signalv1.ResolvedStagePlan,
	participantCount int32,
) error {
	if summary.PhaseOrder != phaseIndex ||
		summary.StageOrder != stageIndex ||
		summary.Name != stage.Name ||
		summary.Role != phaseRole ||
		summary.Test != stage.Test.Identity ||
		summary.Mode != stage.Schedule.Mode ||
		summary.QualificationEligible != stage.QualificationEligible ||
		summary.DiagnosticOnly != stage.DiagnosticOnly {
		return errors.New("stage evidence identity does not match the immutable plan")
	}

	if summary.FinishedAt.Before(&summary.StartedAt) ||
		summary.ObservedCoverageWindow.Duration < 0 ||
		!signalv1.IsSHA256Digest(summary.DurableEvidenceDigest) {
		return errors.New("stage evidence timing or durable digest is invalid")
	}

	if summary.ObservedCoverageWindow.Duration > summary.FinishedAt.Sub(summary.StartedAt.Time) {
		return errors.New("stage observed coverage exceeds its execution interval")
	}

	if summary.FirstInvocationAt != nil &&
		(summary.FirstInvocationAt.Before(&summary.StartedAt) ||
			summary.FirstInvocationAt.After(summary.FinishedAt.Time)) {
		return errors.New("first invocation is outside the stage execution interval")
	}

	if summary.LastInvocationAt != nil &&
		(summary.FirstInvocationAt == nil ||
			summary.LastInvocationAt.Before(summary.FirstInvocationAt) ||
			summary.LastInvocationAt.After(summary.FinishedAt.Time)) {
		return errors.New("last invocation is outside the stage invocation interval")
	}

	if summary.InvocationCount != summary.SuccessfulInvocations+
		summary.TimedOutInvocations+
		summary.FailedInvocations+
		summary.InconclusiveInvocations+
		summary.CanceledInvocations {
		return errors.New("stage invocation outcome counts are inconsistent")
	}

	seenParticipants := make(map[int32]struct{}, len(summary.Participants))
	for index := range summary.Participants {
		participant := &summary.Participants[index]
		if participant.Index < 0 ||
			participant.Index >= participantCount ||
			participant.Role != resolvedParticipantRole(participant.Index) ||
			participant.Start.NodeUID == "" ||
			participant.Start.NodeUID != participant.End.NodeUID {
			return errors.New("stage participant evidence identity is invalid")
		}

		if _, duplicate := seenParticipants[participant.Index]; duplicate {
			return errors.New("stage participant evidence repeats an index")
		}

		seenParticipants[participant.Index] = struct{}{}

		if summary.QualificationEligible &&
			!summary.DiagnosticOnly &&
			summary.Verdict == signalv1.StageVerdictPassed {
			if err := mergeResolvedQualificationFingerprint(
				&signalv1.ParticipantEvidenceIdentity{},
				*participant,
			); err != nil {
				return fmt.Errorf(
					"stage participant %d qualification fingerprint: %w",
					participant.Index,
					err,
				)
			}
		}
	}

	if summary.QualificationEligible &&
		!summary.DiagnosticOnly &&
		summary.Verdict == signalv1.StageVerdictPassed &&
		len(summary.Participants) == 0 {
		return errors.New("passed qualification stage omits participant fingerprints")
	}

	schedule := stage.Schedule
	if schedule.Mode == signalv1.CheckExecutionModeRunOnce {
		maxAttempts := int32(1)
		if stage.Retry != nil {
			maxAttempts = stage.Retry.MaxAttempts
		}

		if summary.InvocationCount < 1 ||
			summary.InvocationCount > maxAttempts ||
			summary.ConfiguredCoverageWindow != nil ||
			summary.MaxStartGap != nil {
			return errors.New("RunOnce stage evidence has invalid invocation or coverage fields")
		}

		if summary.Verdict == signalv1.StageVerdictPassed && summary.SuccessfulInvocations != 1 {
			return errors.New("passed RunOnce stage must contain exactly one successful invocation")
		}
	} else {
		if schedule.CoverageWindow == nil ||
			summary.ConfiguredCoverageWindow == nil ||
			summary.ConfiguredCoverageWindow.Duration != schedule.CoverageWindow.Duration ||
			summary.MaxStartGap == nil {
			return errors.New("repeated stage evidence does not match its configured window")
		}

		if summary.Verdict == signalv1.StageVerdictPassed &&
			(summary.ObservedCoverageWindow.Duration < schedule.CoverageWindow.Duration ||
				schedule.MinExecutions == nil ||
				summary.SuccessfulInvocations < *schedule.MinExecutions ||
				schedule.MaxGap == nil ||
				summary.MaxStartGap == nil ||
				summary.MaxStartGap.Duration > schedule.MaxGap.Duration ||
				summary.FirstInvocationAt == nil ||
				summary.LastInvocationAt == nil) {
			return errors.New("repeated stage evidence does not prove its configured window, count, or gap")
		}
	}

	if stage.PerDeviceCoverageInterval != nil && summary.Verdict == signalv1.StageVerdictPassed {
		if schedule.CoverageWindow == nil || len(summary.DeviceIntervalCoverage) == 0 {
			return errors.New("per-device interval coverage evidence is missing")
		}

		required := int32(
			(schedule.CoverageWindow.Duration + stage.PerDeviceCoverageInterval.Duration - 1) /
				stage.PerDeviceCoverageInterval.Duration,
		)

		expectedParticipants := make(map[int32]struct{}, participantCount)
		if len(stage.ParticipantRoles) == 0 {
			for participantIndex := int32(0); participantIndex < participantCount; participantIndex++ {
				expectedParticipants[participantIndex] = struct{}{}
			}
		} else {
			for _, binding := range stage.ParticipantRoles {
				for _, participantIndex := range binding.ParticipantIndexes {
					expectedParticipants[participantIndex] = struct{}{}
				}
			}
		}

		seenDevices := make(map[string]struct{}, len(summary.DeviceIntervalCoverage))
		for _, coverage := range summary.DeviceIntervalCoverage {
			key := fmt.Sprintf("%d\x00%s", coverage.ParticipantIndex, coverage.DeviceID)
			if coverage.ParticipantIndex < 0 ||
				coverage.ParticipantIndex >= participantCount ||
				coverage.DeviceID == "" {
				return errors.New("per-device interval coverage identity is invalid")
			}

			if _, duplicate := seenDevices[key]; duplicate {
				return errors.New("per-device interval coverage repeats a device")
			}

			seenDevices[key] = struct{}{}

			delete(expectedParticipants, coverage.ParticipantIndex)

			if coverage.RequiredIntervals != required ||
				coverage.CoveredIntervals < required {
				return errors.New("per-device interval coverage is incomplete")
			}
		}

		if len(expectedParticipants) != 0 {
			return errors.New("per-device interval coverage omits a participant")
		}
	}

	expectedAssertions := make(map[string]struct{}, len(stage.Assertions))
	for _, assertion := range stage.Assertions {
		expectedAssertions[assertion.Name] = struct{}{}
	}

	for _, assertion := range summary.Assertions {
		if _, exists := expectedAssertions[assertion.Name]; !exists {
			return fmt.Errorf("stage evidence contains unknown assertion %s", assertion.Name)
		}

		if len([]rune(assertion.Message)) > 256 {
			return fmt.Errorf("stage assertion %s message is oversized", assertion.Name)
		}

		if assertion.Actual == nil &&
			(assertion.Passed || strings.TrimSpace(assertion.Message) == "") {
			return fmt.Errorf(
				"stage assertion %s without an actual value must fail with a message",
				assertion.Name,
			)
		}

		if !assertion.Passed && summary.Verdict == signalv1.StageVerdictPassed {
			return fmt.Errorf("stage passed despite failed assertion %s", assertion.Name)
		}

		delete(expectedAssertions, assertion.Name)
	}

	if len(expectedAssertions) != 0 {
		return errors.New("stage evidence is missing assertion results")
	}

	if summary.Verdict == signalv1.StageVerdictPassed && summary.SuccessfulInvocations == 0 {
		return errors.New("passed stage contains no successful invocation")
	}

	seenInvocations := make(map[string]struct{}, len(summary.InvocationEvidence))
	for _, invocation := range summary.InvocationEvidence {
		if invocation.InvocationID == "" ||
			!signalv1.IsSHA256Digest(invocation.Digest) ||
			invocation.FinishedAt.Before(&invocation.StartedAt) {
			return errors.New("stage invocation evidence reference is invalid")
		}

		if _, duplicate := seenInvocations[invocation.InvocationID]; duplicate {
			return errors.New("stage invocation evidence repeats an invocation")
		}

		seenInvocations[invocation.InvocationID] = struct{}{}
	}

	return nil
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
		RunID            string                                  `json:"runID"`
		RunName          string                                  `json:"runName"`
		Workflow         signalv1.WorkflowStatus                 `json:"workflow"`
		CheckStatus      []evidenceCheckStatus                   `json:"checkStatus"`
		ExecutionState   string                                  `json:"executionState"`
		Verdict          string                                  `json:"verdict"`
		Reason           string                                  `json:"reason"`
		Phase            signalv1.RunPhase                       `json:"phase"`
		FinishedAt       string                                  `json:"finishedAt"`
		EvidenceIdentity *signalv1.QualificationEvidenceIdentity `json:"qualificationIdentity,omitempty"`
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
	run.Status.Evidence = signalv1.EvidenceStatus{
		URI:      uri,
		Digest:   digest,
		Identity: document.EvidenceIdentity,
	}

	if document.Phase == "" {
		run.Status.Phase = runPhaseForVerdict(document.Verdict)
	} else {
		if !document.Phase.Terminal() {
			return ctrl.Result{}, false, fmt.Errorf("persisted evidence contains nonterminal phase %q", document.Phase)
		}

		run.Status.Phase = document.Phase
	}

	if err := r.Status().Patch(ctx, run, client.MergeFrom(before)); err != nil {
		return ctrl.Result{}, true, err
	}

	if err := projectTerminal(ctx, r.Client, run, finishedAt); err != nil {
		return ctrl.Result{}, true, err
	}

	return ctrl.Result{RequeueAfter: time.Millisecond}, true, nil
}

func (r *RunReconciler) persistedEvidenceFinishedAt(
	ctx context.Context,
	run *signalv1.CertificationRun,
) (time.Time, error) {
	data, uri, digest, found, err := r.Evidence.Get(ctx, evidencePath(run))
	if err != nil {
		return time.Time{}, err
	}

	if !found {
		return time.Time{}, errors.New("terminal Run is missing durable evidence")
	}

	if uri != run.Status.Evidence.URI || digest != run.Status.Evidence.Digest {
		return time.Time{}, errors.New("durable evidence identity does not match Run status")
	}

	var document struct {
		RunID      string `json:"runID"`
		RunName    string `json:"runName"`
		FinishedAt string `json:"finishedAt"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return time.Time{}, fmt.Errorf("decode persisted grouped evidence timing: %w", err)
	}

	if document.RunName != run.Name || document.RunID == "" || document.RunID != string(run.UID) {
		return time.Time{}, fmt.Errorf("persisted grouped evidence identity does not match run %s", run.Name)
	}

	finishedAt, err := time.Parse(time.RFC3339Nano, document.FinishedAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse persisted grouped evidence finishedAt: %w", err)
	}

	return finishedAt, nil
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

	return groupedStartedAt(workflow, run)
}

func groupedStartedAt(
	workflow *unstructured.Unstructured,
	run *signalv1.CertificationRun,
) metav1.Time {
	if workflow != nil {
		if value, found, err := unstructured.NestedString(workflow.Object, "status", "startedAt"); err == nil && found {
			if parsed, parseErr := time.Parse(time.RFC3339, value); parseErr == nil {
				return metav1.NewTime(parsed)
			}
		}

		nodes, found, err := unstructured.NestedMap(workflow.Object, "status", "nodes")
		if err == nil && found {
			var earliest time.Time

			for index := range run.Spec.Checks {
				node, count := exactCheckNode(nodes, workflow.GetName(), &run.Spec.Checks[index])
				if count != 1 {
					continue
				}

				parsed, parseErr := time.Parse(time.RFC3339, nodeString(node, "startedAt"))
				if parseErr == nil && (earliest.IsZero() || parsed.Before(earliest)) {
					earliest = parsed
				}
			}

			if !earliest.IsZero() {
				return metav1.NewTime(earliest)
			}
		}
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
		compacted[index] = signalv1.CheckStatus{
			ID:      statuses[index].ID,
			Order:   statuses[index].Order,
			Phase:   statuses[index].Phase,
			Verdict: statuses[index].Verdict,
			Reason:  boundedStatusText(statuses[index].Reason),
		}
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
	}

	return statuses
}

func (r *RunReconciler) terminalCheckStatuses(
	ctx context.Context,
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

		outputs, outputErr := r.readCheckResult(ctx, workflow, run, check)
		if outputErr != nil {
			status.Phase = signalv1.CheckPhaseInconclusive
			status.Verdict = "Inconclusive"
			status.Reason = "ResultRejected: " + outputErr.Error()
			aggregateVerdict = "Inconclusive"
			aggregateReason = "one or more check results were missing, malformed, or incomplete"

			continue
		}

		status.Verdict = outputs.Verdict
		status.Reason = outputs.Reason
		status.Measurements = outputs.Measurements
		allowPartialBackendEvidence := status.Verdict == "Inconclusive" &&
			status.Reason == "BackendSetupFailed"

		normalizedEvidence, complete, evidenceErr := groupedBackendEvidenceForParticipants(
			outputs.BackendEvidence,
			run,
			check,
			run.Spec.Participants,
			outputs.BackendResources,
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
			outputs.BackendResources,
			participantIndexes(run.Spec.Participants),
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

func (r *RunReconciler) readCheckResult(
	ctx context.Context,
	workflow *unstructured.Unstructured,
	run *signalv1.CertificationRun,
	check *signalv1.CertificationCheckSnapshot,
) (adapterCheckResult, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}

	var result corev1.ConfigMap
	if err := reader.Get(
		ctx,
		client.ObjectKey{Namespace: run.Namespace, Name: checkResultName(run.Name, check.ID)},
		&result,
	); err != nil {
		return adapterCheckResult{}, fmt.Errorf("read check result ConfigMap: %w", err)
	}

	if result.Immutable == nil || !*result.Immutable {
		return adapterCheckResult{}, errors.New("check result ConfigMap is mutable")
	}

	if !metav1.IsControlledBy(&result, workflow) {
		return adapterCheckResult{}, errors.New("check result ConfigMap is not owned by the Workflow")
	}

	if result.Labels[SnapshotRunLabel] != run.Name ||
		result.Annotations[checkResultRunUID] != string(run.UID) ||
		result.Annotations[checkResultCheckID] != string(check.ID) ||
		result.Annotations[checkResultCheckOrder] != strconv.FormatInt(int64(check.Order), 10) ||
		result.Annotations[checkResultPlanDigest] != run.Spec.PlanDigest {
		return adapterCheckResult{}, errors.New("check result ConfigMap identity does not match the run")
	}

	content, exists := result.BinaryData[checkResultDataKey]
	if !exists {
		return adapterCheckResult{}, errors.New("check result ConfigMap is missing result.json")
	}

	if len(content) > maximumCheckResultBytes {
		return adapterCheckResult{}, fmt.Errorf(
			"check result exceeds %d bytes",
			maximumCheckResultBytes,
		)
	}

	if result.Annotations[checkResultDigest] != digestBytes(content) {
		return adapterCheckResult{}, errors.New("check result ConfigMap digest does not match its content")
	}

	return decodeCheckResult(content)
}

func decodeCheckResult(content []byte) (adapterCheckResult, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(content, &fields); err != nil {
		return adapterCheckResult{}, fmt.Errorf("decode check result fields: %w", err)
	}

	for _, required := range []string{
		"executionState",
		"verdict",
		"reason",
		"measurements",
		"backend-resources",
		"backend-evidence",
	} {
		if _, exists := fields[required]; !exists {
			return adapterCheckResult{}, fmt.Errorf("required field %q is missing", required)
		}
	}

	var result adapterCheckResult

	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&result); err != nil {
		return adapterCheckResult{}, fmt.Errorf("decode check result: %w", err)
	}

	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return adapterCheckResult{}, errors.New("check result contains trailing JSON values")
		}

		return adapterCheckResult{}, fmt.Errorf("decode trailing check result value: %w", err)
	}

	values := map[string]string{
		"verdict":           result.Verdict,
		"reason":            result.Reason,
		"measurements":      result.Measurements,
		"backend-resources": result.BackendResources,
		"backend-evidence":  result.BackendEvidence,
	}
	for name, limit := range checkResultFieldLimits {
		value, exists := values[name]
		if !exists {
			continue
		}

		if len(value) > limit {
			return adapterCheckResult{}, fmt.Errorf("%s exceeds %d bytes", name, limit)
		}
	}

	return result, nil
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
	stageSummaries []signalv1.StageEvidenceSummary,
	executionState string,
	verdict string,
	reason string,
	startedAt metav1.Time,
	finishedAt metav1.Time,
) ([]byte, error) {
	if run.Spec.ResolvedPlan != nil {
		doc := map[string]any{
			"apiVersion":            "signal.unbounded-cloud.io/evidence/v1alpha1",
			"runID":                 string(run.UID),
			"runName":               run.Name,
			"profile":               run.Spec.Profile,
			"suite":                 run.Spec.Suite,
			"participants":          run.Spec.Participants,
			"resolvedPlan":          run.Spec.ResolvedPlan,
			"environment":           run.Spec.Environment,
			"planDigest":            run.Spec.PlanDigest,
			"planProvenance":        run.Spec.PlanProvenance,
			"workflow":              run.Status.Workflow,
			"executionState":        executionState,
			"verdict":               verdict,
			"reason":                reason,
			"phase":                 run.Status.Phase,
			"qualificationIdentity": run.Status.Evidence.Identity,
			"stageSummaries":        stageSummaries,
			"startedAt":             startedAt.UTC().Format(time.RFC3339Nano),
			"finishedAt":            finishedAt.UTC().Format(time.RFC3339Nano),
		}

		return json.MarshalIndent(doc, "", "  ")
	}

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
		"apiVersion":            "signal.unbounded-cloud.io/evidence/v1alpha1",
		"runID":                 string(run.UID),
		"runName":               run.Name,
		"profile":               run.Spec.Profile,
		"suite":                 run.Spec.Suite,
		"participants":          run.Spec.Participants,
		"scriptBundle":          run.Spec.ScriptBundle,
		"workflow":              run.Status.Workflow,
		"checks":                checks,
		"checkStatus":           checkStatuses,
		"scriptContents":        scriptContents,
		"executionState":        executionState,
		"verdict":               verdict,
		"reason":                reason,
		"phase":                 run.Status.Phase,
		"qualificationIdentity": run.Status.Evidence.Identity,
		"startedAt":             startedAt.UTC().Format(time.RFC3339Nano),
		"finishedAt":            finishedAt.UTC().Format(time.RFC3339Nano),
	}

	return json.MarshalIndent(doc, "", "  ")
}

func (r *RunReconciler) qualificationEvidenceIdentity(
	ctx context.Context,
	run *signalv1.CertificationRun,
	statuses []signalv1.CheckStatus,
	verdict string,
	reason string,
) (*signalv1.QualificationEvidenceIdentity, string, string, error) {
	if run.Spec.PlanDigest == "" || run.Spec.PlanProvenance == nil {
		return nil, verdict, reason, nil
	}

	canonical, err := signalv1.DeriveExecutionPlanDigest(
		int32(len(run.Spec.Participants)),
		run.Spec.ScriptBundle.Digest,
		run.Spec.Checks,
		*run.Spec.PlanProvenance,
	)
	if err != nil || canonical != run.Spec.PlanDigest {
		return nil, "Inconclusive", "ExecutionPlanIdentityInvalid: immutable plan provenance is invalid", nil
	}

	participants := make([]signalv1.ParticipantEvidenceIdentity, len(run.Spec.Participants))
	for index := range run.Spec.Participants {
		role := signalv1.ParticipantRoleParticipant
		if index == 0 {
			role = signalv1.ParticipantRoleRequester
		}

		participants[index] = signalv1.ParticipantEvidenceIdentity{
			Index:   int32(index),
			Role:    role,
			Verdict: signalv1.QualificationVerdictPass,
		}
	}

	summaries := make([]signalv1.CheckContinuitySummary, len(statuses))
	for index := range statuses {
		status := &statuses[index]
		if status.BackendEvidence.Content == "" {
			return nil,
				"Inconclusive",
				fmt.Sprintf("QualificationEvidenceMissing: check %s has no adapter evidence", run.Spec.Checks[index].ID),
				nil
		}

		var evidence adapterBackendEvidence
		if err := json.Unmarshal([]byte(status.BackendEvidence.Content), &evidence); err != nil {
			return nil,
				"Inconclusive",
				fmt.Sprintf("QualificationEvidenceMalformed: check %s evidence cannot be decoded", run.Spec.Checks[index].ID),
				nil
		}

		if !evidence.Complete || len(evidence.Participants) != len(participants) {
			verdict = "Inconclusive"
			reason = fmt.Sprintf("QualificationEvidenceIncomplete: check %s participant evidence is incomplete", run.Spec.Checks[index].ID)
		}

		duration := evidence.Timing.FinishedAt.Sub(evidence.Timing.StartedAt.Time)
		sameBoot := true
		perDeviceCoverage := true

		for participantIndex := range evidence.Participants {
			observed := &evidence.Participants[participantIndex]
			if observed.ParticipantIndex < 0 || observed.ParticipantIndex >= len(participants) {
				return nil, "Inconclusive", "QualificationEvidenceMalformed: participant index is invalid", nil
			}

			identity := &participants[observed.ParticipantIndex]
			if index == 0 {
				identity.Start = observed.Start
			}

			identity.End = observed.End
			identity.Verdict = combineQualificationVerdicts(
				identity.Verdict,
				qualificationVerdict(observed.Verdict.Verdict),
			)

			if observed.Start.BootID == "" ||
				observed.End.BootID == "" ||
				observed.Start.BootID != observed.End.BootID {
				sameBoot = false
			}

			perDeviceCoverage = perDeviceCoverage && observed.PerDeviceCoverage
		}

		summaries[index] = signalv1.CheckContinuitySummary{
			Order:             int32(index),
			StartedAt:         evidence.Timing.StartedAt,
			FinishedAt:        evidence.Timing.FinishedAt,
			ObservedDuration:  metav1.Duration{Duration: duration},
			MaxSampleGap:      evidence.Timing.MaxObservedGap,
			Completed:         status.Phase == signalv1.CheckPhaseSucceeded,
			SameBootID:        sameBoot,
			PerDeviceCoverage: perDeviceCoverage,
		}

		check := &run.Spec.Checks[index]
		if verdict == "Pass" && check.Continuity != nil {
			if check.Continuity.RequireSameBootID && !sameBoot {
				verdict = "Inconclusive"
				reason = fmt.Sprintf("ContinuityNotProven: check %s lacks same-boot evidence", check.ID)
			}

			if verdict == "Pass" && check.Continuity.MaxSampleGap != nil &&
				(evidence.Timing.MaxObservedGap == nil ||
					evidence.Timing.MaxObservedGap.Duration > check.Continuity.MaxSampleGap.Duration) {
				verdict = "Inconclusive"
				reason = fmt.Sprintf("ContinuityNotProven: check %s exceeds its sample-gap requirement", check.ID)
			}

			if verdict == "Pass" && check.Continuity.RequirePerDeviceCoverage && !perDeviceCoverage {
				verdict = "Inconclusive"
				reason = fmt.Sprintf("ContinuityNotProven: check %s lacks per-device coverage evidence", check.ID)
			}
		}

		for index := range run.Spec.Checks {
			if run.Spec.Checks[index].MinDuration != nil {
				return nil,
					"Inconclusive",
					"LegacyDurationSemanticsUnsupported: legacy backend lifetime cannot prove sustained execution",
					nil
			}
		}
	}

	admissionStarts, err := decodeStartFingerprints(run)
	if err != nil {
		return nil, "Inconclusive", "ParticipantIdentityMissing: " + err.Error(), nil
	}

	for index := range participants {
		if !sameBaseFingerprint(participants[index].Start, admissionStarts[index]) {
			return nil,
				"Inconclusive",
				fmt.Sprintf("ParticipantIdentityChanged: participant %d start fingerprint differs from admission", index),
				nil
		}

		var node corev1.Node
		if err := r.APIReader.Get(
			ctx,
			client.ObjectKey{Name: run.Spec.Participants[index].NodeName},
			&node,
		); err != nil {
			if !apierrors.IsNotFound(err) {
				return nil, verdict, reason, fmt.Errorf(
					"read participant %s end fingerprint: %w",
					run.Spec.Participants[index].NodeName,
					err,
				)
			}

			return nil, "Inconclusive", "ParticipantIdentityMissing: end fingerprint unavailable", nil
		}

		if !sameBaseFingerprint(participants[index].End, nodeFingerprint(&node)) {
			return nil,
				"Inconclusive",
				fmt.Sprintf("ParticipantIdentityChanged: participant %d end fingerprint is no longer current", index),
				nil
		}
	}

	runnerImages := make([]string, 0, len(run.Spec.Checks))
	seenImages := map[string]struct{}{}

	for index := range run.Spec.Checks {
		image := run.Spec.Checks[index].RunnerImage
		if _, exists := seenImages[image]; exists {
			continue
		}

		seenImages[image] = struct{}{}
		runnerImages = append(runnerImages, image)
	}

	sort.Strings(runnerImages)

	if verdict != "Pass" {
		for index := range participants {
			participants[index].Verdict = combineQualificationVerdicts(
				participants[index].Verdict,
				qualificationVerdict(verdict),
			)
		}
	}

	return &signalv1.QualificationEvidenceIdentity{
		PlanDigest:   run.Spec.PlanDigest,
		Participants: participants,
		Checks:       summaries,
		Provenance: signalv1.EvidenceProvenance{
			ControllerImage:        r.ControllerImage,
			AdapterImage:           run.Spec.PlanProvenance.AdapterImage,
			WorkflowTemplateDigest: run.Spec.PlanProvenance.WorkflowTemplateDigest,
			RunnerImages:           runnerImages,
		},
	}, verdict, reason, nil
}

func (r *RunReconciler) resolvedQualificationEvidenceIdentity(
	ctx context.Context,
	run *signalv1.CertificationRun,
	stages []signalv1.StageEvidenceSummary,
	runnerImages []string,
	verdict string,
	reason string,
) (*signalv1.QualificationEvidenceIdentity, string, string, error) {
	if run.Spec.PlanDigest == "" ||
		run.Spec.PlanProvenance == nil ||
		run.Spec.ResolvedPlan == nil {
		return nil, verdict, reason, nil
	}

	canonical, err := signalv1.DeriveResolvedExecutionPlanDigest(
		int32(len(run.Spec.Participants)),
		run.Spec.Environment,
		*run.Spec.ResolvedPlan,
		*run.Spec.PlanProvenance,
	)
	if err != nil || canonical != run.Spec.PlanDigest {
		return nil, "Inconclusive", "ExecutionPlanIdentityInvalid: immutable resolved plan provenance is invalid", nil
	}

	if len(stages) == 0 {
		return nil, verdict, reason, nil
	}

	starts, err := decodeStartFingerprints(run)
	if err != nil {
		return nil, "Inconclusive", "ParticipantIdentityMissing: " + err.Error(), nil
	}

	participants := make([]signalv1.ParticipantEvidenceIdentity, len(run.Spec.Participants))
	for index := range run.Spec.Participants {
		var node corev1.Node
		if err := r.APIReader.Get(
			ctx,
			client.ObjectKey{Name: run.Spec.Participants[index].NodeName},
			&node,
		); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, "Inconclusive", "ParticipantIdentityMissing: end fingerprint unavailable", nil
			}

			return nil, verdict, reason, fmt.Errorf(
				"read participant %s end fingerprint: %w",
				run.Spec.Participants[index].NodeName,
				err,
			)
		}

		if node.UID != run.Spec.Participants[index].NodeUID {
			return nil, "Inconclusive", "ParticipantIdentityChanged: node UID changed during execution", nil
		}

		participants[index] = signalv1.ParticipantEvidenceIdentity{
			Index:   int32(index),
			Role:    resolvedParticipantRole(int32(index)),
			Verdict: "",
			Start:   starts[index],
			End:     nodeBaseFingerprint(&node),
		}
	}

	if len(runnerImages) == 0 {
		return nil, verdict, reason, nil
	}

	observedParticipants := make([]bool, len(participants))
	hasQualificationStage := false

	for stageIndex := range stages {
		stage := &stages[stageIndex]
		if !stage.QualificationEligible || stage.DiagnosticOnly {
			continue
		}

		if stage.PhaseOrder < 0 ||
			int(stage.PhaseOrder) >= len(run.Spec.ResolvedPlan.Phases) ||
			stage.StageOrder < 0 ||
			int(stage.StageOrder) >= len(run.Spec.ResolvedPlan.Phases[stage.PhaseOrder].Stages) {
			return nil,
				"Inconclusive",
				"QualificationEvidenceMalformed: stage order is outside the resolved plan",
				nil
		}

		if run.Spec.ResolvedPlan.Phases[stage.PhaseOrder].Stages[stage.StageOrder].Optional {
			continue
		}

		hasQualificationStage = true

		for participantIndex := range stage.Participants {
			observed := &stage.Participants[participantIndex]
			if observed.Index < 0 || int(observed.Index) >= len(participants) {
				return nil,
					"Inconclusive",
					"QualificationEvidenceMalformed: stage participant index is invalid",
					nil
			}

			participant := &participants[observed.Index]
			if !sameBaseFingerprint(observed.Start, participant.Start) ||
				!sameBaseFingerprint(observed.End, participant.End) {
				return nil,
					"Inconclusive",
					fmt.Sprintf(
						"ParticipantIdentityChanged: participant %d stage fingerprint differs from the Run",
						observed.Index,
					),
					nil
			}

			if stage.Verdict == signalv1.StageVerdictPassed {
				if err := mergeResolvedQualificationFingerprint(participant, *observed); err != nil {
					return nil,
						"Inconclusive",
						fmt.Sprintf(
							"QualificationEvidenceMalformed: participant %d fingerprint: %v",
							observed.Index,
							err,
						),
						nil
				}

				observedParticipants[observed.Index] = true
			}

			participant.Verdict = combineQualificationVerdicts(
				participant.Verdict,
				observed.Verdict,
			)
		}
	}

	resolverImages := append([]string(nil), run.Spec.PlanProvenance.ResolverImages...)
	sort.Strings(resolverImages)

	for index := range participants {
		if verdict == "Pass" &&
			hasQualificationStage &&
			(!observedParticipants[index] ||
				participants[index].Start.DriverVersion == "" ||
				participants[index].Start.GPUInventoryDigest == "") {
			participants[index].Verdict = signalv1.QualificationVerdictInconclusive
			verdict = "Inconclusive"
			reason = fmt.Sprintf(
				"QualificationEvidenceMissing: participant %d has no qualification fingerprint",
				index,
			)
		} else if !hasQualificationStage {
			participants[index].Verdict = qualificationVerdict(verdict)
		}

		if verdict != "Pass" {
			participants[index].Verdict = combineQualificationVerdicts(
				participants[index].Verdict,
				qualificationVerdict(verdict),
			)
		}
	}

	return &signalv1.QualificationEvidenceIdentity{
		PlanDigest:   run.Spec.PlanDigest,
		Participants: participants,
		Stages:       stages,
		Provenance: signalv1.EvidenceProvenance{
			ControllerImage:        run.Spec.PlanProvenance.ControllerImage,
			AdapterImage:           run.Spec.PlanProvenance.AdapterImage,
			WorkflowTemplateDigest: run.Spec.PlanProvenance.WorkflowTemplateDigest,
			RunnerImages:           runnerImages,
			ResolverImages:         resolverImages,
		},
	}, verdict, reason, nil
}

func mergeResolvedQualificationFingerprint(
	current *signalv1.ParticipantEvidenceIdentity,
	observed signalv1.ParticipantEvidenceIdentity,
) error {
	if observed.Start.DriverVersion == "" ||
		observed.End.DriverVersion == "" ||
		observed.Start.DriverVersion != observed.End.DriverVersion {
		return errors.New("driver version is missing or mismatched")
	}

	if !signalv1.IsSHA256Digest(observed.Start.GPUInventoryDigest) ||
		!signalv1.IsSHA256Digest(observed.End.GPUInventoryDigest) ||
		observed.Start.GPUInventoryDigest != observed.End.GPUInventoryDigest {
		return errors.New("GPU inventory digest is missing, mismatched, or invalid")
	}

	if current.Start.DriverVersion != "" &&
		current.Start.DriverVersion != observed.Start.DriverVersion {
		return errors.New("driver version disagrees across qualification stages")
	}

	if current.Start.GPUInventoryDigest != "" &&
		current.Start.GPUInventoryDigest != observed.Start.GPUInventoryDigest {
		return errors.New("GPU inventory digest disagrees across qualification stages")
	}

	current.Start.DriverVersion = observed.Start.DriverVersion
	current.End.DriverVersion = observed.End.DriverVersion
	current.Start.GPUInventoryDigest = observed.Start.GPUInventoryDigest
	current.End.GPUInventoryDigest = observed.End.GPUInventoryDigest

	return nil
}

func resolvedTestRunnerImage(driver signalv1.CertificationTestDriver) string {
	switch {
	case driver.Script != nil:
		return driver.Script.RunnerImage
	case driver.Container != nil:
		return driver.Container.Image
	default:
		return ""
	}
}

func resolvedParticipantRole(participantIndex int32) signalv1.ParticipantRole {
	if participantIndex == 0 {
		return signalv1.ParticipantRoleRequester
	}

	return signalv1.ParticipantRoleParticipant
}

func decodeStartFingerprints(run *signalv1.CertificationRun) ([]signalv1.NodeFingerprint, error) {
	value := run.Annotations[RunStartIdentityAnnotation]
	if value == "" {
		return nil, errors.New("start fingerprints are absent")
	}

	var fingerprints []signalv1.NodeFingerprint
	if err := json.Unmarshal([]byte(value), &fingerprints); err != nil {
		return nil, errors.New("start fingerprints are invalid")
	}

	if len(fingerprints) != len(run.Spec.Participants) {
		return nil, errors.New("start fingerprint count does not match participants")
	}

	for index := range fingerprints {
		if fingerprints[index].NodeUID != run.Spec.Participants[index].NodeUID {
			return nil, errors.New("start fingerprint identity does not match participants")
		}
	}

	return fingerprints, nil
}

func qualificationVerdict(verdict string) signalv1.QualificationVerdict {
	switch verdict {
	case "Pass":
		return signalv1.QualificationVerdictPass
	case "Fail":
		return signalv1.QualificationVerdictFail
	default:
		return signalv1.QualificationVerdictInconclusive
	}
}

func combineQualificationVerdicts(
	current signalv1.QualificationVerdict,
	observed signalv1.QualificationVerdict,
) signalv1.QualificationVerdict {
	if current == signalv1.QualificationVerdictFail || observed == signalv1.QualificationVerdictFail {
		return signalv1.QualificationVerdictFail
	}

	if current == signalv1.QualificationVerdictInconclusive ||
		observed == signalv1.QualificationVerdictInconclusive {
		return signalv1.QualificationVerdictInconclusive
	}

	return signalv1.QualificationVerdictPass
}

func sameBaseFingerprint(left, right signalv1.NodeFingerprint) bool {
	return left.NodeUID != "" &&
		left.NodeUID == right.NodeUID &&
		(left.BootID == "" || left.BootID == right.BootID) &&
		(left.KernelVersion == "" || left.KernelVersion == right.KernelVersion) &&
		(left.OSImage == "" || left.OSImage == right.OSImage) &&
		(left.KubeletVersion == "" || left.KubeletVersion == right.KubeletVersion) &&
		(left.SystemUUID == "" || left.SystemUUID == right.SystemUUID)
}

func nodeFingerprint(node *corev1.Node) signalv1.NodeFingerprint {
	fingerprint := nodeBaseFingerprint(node)
	fingerprint.DriverVersion = node.Annotations[DriverVersionAnnotation]
	fingerprint.GPUInventoryDigest = node.Annotations[GPUInventoryDigestAnnotation]

	return fingerprint
}

func nodeBaseFingerprint(node *corev1.Node) signalv1.NodeFingerprint {
	return signalv1.NodeFingerprint{
		NodeUID:        node.UID,
		BootID:         node.Status.NodeInfo.BootID,
		KernelVersion:  node.Status.NodeInfo.KernelVersion,
		OSImage:        node.Status.NodeInfo.OSImage,
		KubeletVersion: node.Status.NodeInfo.KubeletVersion,
		SystemUUID:     node.Status.NodeInfo.SystemUUID,
	}
}

func backendResourceReferences(
	value string,
	participantCount int,
	_ string,
	allowPartial bool,
) ([]signalv1.BackendResourceReference, error) {
	if participantCount < 1 || participantCount > maxAdapterParticipants {
		return nil, fmt.Errorf(
			"participant count must be between 1 and %d",
			maxAdapterParticipants,
		)
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

	if len(references) == 0 || len(references) > maxAdapterParticipants {
		return nil, fmt.Errorf(
			"backend-resources must contain between 1 and %d entries",
			maxAdapterParticipants,
		)
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
			if covered[participantIndex] {
				return nil, fmt.Errorf(
					"backend-resources[%d] repeats participant index %d across workload identities",
					index,
					participantIndex,
				)
			}

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

func groupedBackendEvidenceComplete(
	content string,
	run *signalv1.CertificationRun,
	check *signalv1.CertificationCheckSnapshot,
	participants []signalv1.Participant,
	references []signalv1.BackendResourceReference,
	backend signalv1.ExecutionBackend,
	namespace string,
	allowPartial bool,
) (bool, error) {
	if err := validateAdapterBackendEvidence(content); err != nil {
		return false, err
	}

	participantCount := len(participants)

	var evidence adapterBackendEvidence
	if err := json.Unmarshal([]byte(content), &evidence); err != nil {
		return false, err
	}

	if run == nil || check == nil ||
		evidence.Identity.RunUID != run.UID ||
		evidence.Identity.CheckID != check.ID ||
		evidence.Identity.CheckOrder != check.Order ||
		evidence.Identity.PlanDigest != run.Spec.PlanDigest {
		return false, errors.New("backend-evidence identity does not match the immutable Run check")
	}

	if evidence.Provenance.RunnerImage != check.RunnerImage {
		return false, errors.New("backend-evidence runner image differs from the immutable check")
	}

	if run.Spec.PlanProvenance != nil &&
		evidence.Provenance.AdapterImage != run.Spec.PlanProvenance.AdapterImage {
		return false, errors.New("backend-evidence adapter image differs from the immutable plan")
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
	launcherPods := 0
	launcherJobs := 0

	expectedAPIVersion, expectedKind, err := backendIdentity(backend)
	if err != nil {
		return false, err
	}

	switch backend {
	case signalv1.ExecutionBackendPod:
		if evidence.GroupResult != nil {
			return false, errors.New("backend-evidence Pod result must not contain a group verdict")
		}
	case signalv1.ExecutionBackendMPIJob:
		if evidence.GroupResult == nil {
			return false, errors.New("backend-evidence MPI result requires a group verdict")
		}
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

		expectedAttribution := "participant"
		if backend == signalv1.ExecutionBackendMPIJob {
			expectedAttribution = "group-with-participant-observation"
		}

		if participant.Verdict.Attribution != expectedAttribution {
			return false, fmt.Errorf(
				"backend-evidence participant %d verdict attribution is invalid for %s",
				participant.ParticipantIndex,
				backend,
			)
		}

		expectedParticipant := participants[participant.ParticipantIndex]
		if participant.Start.NodeUID != expectedParticipant.NodeUID ||
			participant.End.NodeUID != expectedParticipant.NodeUID {
			return false, fmt.Errorf(
				"backend-evidence participant %d fingerprint differs from the immutable participant",
				participant.ParticipantIndex,
			)
		}

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
		workerPods := 0
		workerHealthy := false

		for _, pod := range participant.Pods {
			if pod.UID == "" || pod.Name == "" || pod.NodeName == "" || pod.NodeUID == "" {
				return false, errors.New("backend-evidence Pod identity is incomplete")
			}

			if _, duplicate := podUIDs[pod.UID]; duplicate {
				return false, errors.New("backend-evidence repeats a Pod UID")
			}

			podUIDs[pod.UID] = struct{}{}

			switch backend {
			case signalv1.ExecutionBackendPod:
				if pod.UID != participant.Resource.UID ||
					pod.Name != participant.Resource.Name ||
					pod.Role != "participant" ||
					len(participant.Pods) != 1 ||
					pod.NodeName != participants[participant.ParticipantIndex].NodeName ||
					pod.NodeUID != participants[participant.ParticipantIndex].NodeUID {
					return false, errors.New("backend-evidence Pod differs from its backend resource or immutable participant")
				}
			case signalv1.ExecutionBackendMPIJob:
				switch pod.Role {
				case "launcher":
					launcherPods++
				case "worker":
					workerPods++

					if pod.NodeName != participants[participant.ParticipantIndex].NodeName ||
						pod.NodeUID != participants[participant.ParticipantIndex].NodeUID {
						return false, fmt.Errorf(
							"backend-evidence participant %d worker differs from the immutable participant Node",
							participant.ParticipantIndex,
						)
					}

					workerHealthy = pod.Phase == corev1.PodSucceeded && len(pod.Containers) > 0
					for _, container := range pod.Containers {
						workerHealthy = workerHealthy &&
							container.State.State == "terminated" &&
							container.State.ExitCode == 0
					}
				default:
					return false, fmt.Errorf("backend-evidence MPI Pod role %q is invalid", pod.Role)
				}
			}

			complete = complete && pod.Phase != "" && len(pod.Containers) > 0
			complete = complete && pod.NodeError == ""

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
			complete = complete && workerPods == 1

			if participant.Verdict.Verdict == "Pass" && !workerHealthy {
				return false, fmt.Errorf(
					"backend-evidence participant %d pass verdict lacks healthy worker evidence",
					participant.ParticipantIndex,
				)
			}

			if participant.LauncherJob != nil {
				if participant.LauncherJob.Name == "" || participant.LauncherJob.UID == "" {
					return false, errors.New("backend-evidence MPI launcher Job identity is incomplete")
				}

				launcherJobs++
			}
		}
	}

	if !allowPartial {
		for index, present := range seen {
			if !present {
				return false, fmt.Errorf("backend-evidence does not cover participant index %d", index)
			}
		}
	}

	if backend == signalv1.ExecutionBackendMPIJob {
		complete = complete && launcherPods == 1 && launcherJobs == 1
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
	run *signalv1.CertificationRun,
	check *signalv1.CertificationCheckSnapshot,
	participants []signalv1.Participant,
	resources string,
	backend signalv1.ExecutionBackend,
	namespace string,
	allowPartial bool,
) (string, bool, error) {
	globalIndexes := participantIndexes(participants)

	references, err := backendResourceReferences(resources, len(globalIndexes), namespace, allowPartial)
	if err != nil {
		return "", false, err
	}

	complete, err := groupedBackendEvidenceComplete(
		content,
		run,
		check,
		participants,
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

	var evidenceParticipants []map[string]json.RawMessage
	if err := json.Unmarshal(evidence["participants"], &evidenceParticipants); err != nil {
		return "", false, fmt.Errorf("decode backend-evidence participants for mapping: %w", err)
	}

	for index := range evidenceParticipants {
		var localIndex int
		if err := json.Unmarshal(evidenceParticipants[index]["participantIndex"], &localIndex); err != nil {
			return "", false, fmt.Errorf("decode backend-evidence participant index: %w", err)
		}

		mapped, err := json.Marshal(globalIndexes[localIndex])
		if err != nil {
			return "", false, fmt.Errorf("encode mapped backend-evidence participant index: %w", err)
		}

		evidenceParticipants[index]["participantIndex"] = mapped
	}

	mappedParticipants, err := json.Marshal(evidenceParticipants)
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

		completed, _, _ := r.terminalCheckStatuses(ctx, workflow, run)

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
	startedAt := groupedStartedAt(workflow, run)

	evidence, err := r.evidenceDocument(
		ctx,
		evidenceRun,
		nil,
		"Interrupted",
		"Inconclusive",
		evidenceRun.Status.Reason,
		startedAt,
		now,
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

	finishedAt, err := r.persistedEvidenceFinishedAt(ctx, run)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := projectTerminal(ctx, r.Client, run, finishedAt); err != nil {
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
	if run.Spec.ScriptBundle == nil {
		return nil
	}

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
	if run.Spec.ResolvedPlan != nil {
		for _, kind := range []schema.GroupVersionKind{
			workflowGVK,
			corev1.SchemeGroupVersion.WithKind("Pod"),
		} {
			list := &unstructured.UnstructuredList{}
			list.SetGroupVersionKind(kind.GroupVersion().WithKind(kind.Kind + "List"))

			if err := reader.List(
				ctx,
				list,
				client.InNamespace(run.Namespace),
				client.MatchingLabels{RunUIDLabel: string(run.UID)},
			); err != nil {
				return false, fmt.Errorf("confirm resolved %s cleanup: %w", kind.Kind, err)
			}

			if len(list.Items) > 0 {
				return false, nil
			}
		}

		return true, nil
	}

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

	if run.Spec.ResolvedPlan != nil {
		kinds = append(kinds, workflowGVK)
	} else {
		for _, check := range run.Spec.Checks {
			if check.Backend == signalv1.ExecutionBackendMPIJob {
				kinds = append(kinds, schema.GroupVersionKind{Group: "kubeflow.org", Version: "v2beta1", Kind: "MPIJob"})

				break
			}
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

		listOptions := []client.ListOption{client.InNamespace(run.Namespace)}
		if run.Spec.ResolvedPlan != nil && (kind == workflowGVK || kind == corev1.SchemeGroupVersion.WithKind("Pod")) {
			listOptions = append(listOptions, client.MatchingLabels{RunUIDLabel: string(run.UID)})
		}

		if err := reader.List(ctx, list, listOptions...); err != nil {
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

func suiteChecks(suite *signalv1.CertificationSuite) []signalv1.CertificationCheck {
	checks := make([]signalv1.CertificationCheck, len(suite.Spec.Checks))
	for index := range suite.Spec.Checks {
		checks[index] = suite.Spec.Checks[index]
		checks[index].ID = signalv1.CheckID(fmt.Sprintf("R%02d", index+1))
		checks[index].Order = int32(index)
		checks[index].WorkloadPolicy = *suite.Spec.Checks[index].WorkloadPolicy.DeepCopy()
	}

	return checks
}

func validateCheckPlan(checks []signalv1.CertificationCheck) (time.Duration, error) {
	if err := validateCheckCount(len(checks)); err != nil {
		return 0, err
	}

	deadlines := make([]time.Duration, len(checks))
	for index := range checks {
		deadlines[index] = checks[index].BackendActiveDeadline.Duration
	}

	suiteDeadline, err := signalv1.DeriveSuiteDeadline(deadlines)
	if err != nil {
		return 0, err
	}

	for index := range checks {
		if err := signalv1.ValidateExecutionDeadlines(deadlines[index], suiteDeadline); err != nil {
			return 0, fmt.Errorf("check %s runtime contract: %w", checks[index].ID, err)
		}
	}

	return suiteDeadline, nil
}

func validateRunCheckPlan(checks []signalv1.CertificationCheckSnapshot) (time.Duration, error) {
	if err := validateCheckCount(len(checks)); err != nil {
		return 0, fmt.Errorf("run %w", err)
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

	suiteDeadline, err := signalv1.DeriveSuiteDeadline(deadlines)
	if err != nil {
		return 0, err
	}

	for index := range checks {
		if err := signalv1.ValidateExecutionDeadlines(deadlines[index], suiteDeadline); err != nil {
			return 0, fmt.Errorf("check %s runtime contract: %w", checks[index].ID, err)
		}
	}

	return suiteDeadline, nil
}

func validateCheckCount(count int) error {
	if count < signalv1.MinCertificationChecks || count > signalv1.MaxCertificationChecks {
		return fmt.Errorf(
			"must contain between %d and %d checks",
			signalv1.MinCertificationChecks,
			signalv1.MaxCertificationChecks,
		)
	}

	return nil
}

func participantIndexes(participants []signalv1.Participant) []int32 {
	indexes := make([]int32, len(participants))
	for index := range participants {
		indexes[index] = int32(index)
	}

	return indexes
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

	return boundedDNSName(runName + "-" + suffix)
}

func checkResultName(runName string, checkID signalv1.CheckID) string {
	return boundedDNSName(runName + "-" + sanitizeTaskName(string(checkID)) + "-result")
}

func boundedDNSName(candidate string) string {
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
