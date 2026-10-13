// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signaldashboard

import (
	"context"
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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	signalv1 "github.com/Azure/unbounded/api/signal/v1alpha1"
	"github.com/Azure/unbounded/internal/signal"
)

const (
	defaultPageLimit = 50
	maxPageLimit     = 100
)

var (
	workflowListGVK = schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "WorkflowList"}
	mpiJobListGVK   = schema.GroupVersionKind{Group: "kubeflow.org", Version: "v2beta1", Kind: "MPIJobList"}
)

type Reader interface {
	Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error
	List(context.Context, client.ObjectList, ...client.ListOption) error
}

type PodLogReader interface {
	ReadPodLog(context.Context, string, string, string, int64, int64) ([]byte, error)
}

type Collector struct {
	Reader    Reader
	PodLogs   PodLogReader
	Namespace string
	Now       func() time.Time
}

type collected struct {
	runs      []signalv1.CertificationRun
	profiles  []signalv1.CertificationProfile
	workflows []unstructured.Unstructured
	mpiJobs   []unstructured.Unstructured
	nodes     map[string]*corev1.Node
	aliases   map[string]string
	pods      []corev1.Pod
	errors    []APIError
	refreshed time.Time
}

func (c *Collector) Collect(ctx context.Context) collected {
	result := collected{
		nodes:     map[string]*corev1.Node{},
		aliases:   map[string]string{},
		refreshed: c.now(),
	}

	var runs signalv1.CertificationRunList
	if err := c.Reader.List(ctx, &runs, client.InNamespace(c.Namespace)); err != nil {
		result.addError("CertificationRun", err)
	} else {
		result.runs = runs.Items
	}

	var profiles signalv1.CertificationProfileList
	if err := c.Reader.List(ctx, &profiles); err != nil {
		result.addError("CertificationProfile", err)
	} else {
		result.profiles = profiles.Items
	}

	workflows := &unstructured.UnstructuredList{}
	workflows.SetGroupVersionKind(workflowListGVK)

	if err := c.Reader.List(ctx, workflows, client.InNamespace(c.Namespace)); err != nil {
		result.addError("Workflow", err)
	} else {
		result.workflows = workflows.Items
	}

	mpiJobs := &unstructured.UnstructuredList{}
	mpiJobs.SetGroupVersionKind(mpiJobListGVK)

	if err := c.Reader.List(ctx, mpiJobs, client.InNamespace(c.Namespace)); err != nil {
		result.addError("MPIJob", err)
	} else {
		result.mpiJobs = mpiJobs.Items
	}

	var pods corev1.PodList
	if err := c.Reader.List(ctx, &pods, client.InNamespace(c.Namespace)); err != nil {
		result.addError("Pod", err)
	} else {
		result.pods = pods.Items
	}

	for _, name := range approvedNodeNames(result.runs, result.profiles) {
		var node corev1.Node
		if err := c.Reader.Get(ctx, client.ObjectKey{Name: name}, &node); err != nil {
			if !apierrors.IsNotFound(err) {
				result.addError("Node", fmt.Errorf("approved Node read failed: %w", err))
			}

			continue
		}

		result.nodes[name] = node.DeepCopy()
	}

	result.aliases = nodeAliases(result.nodes)

	sort.Slice(result.runs, func(i, j int) bool {
		return result.runs[i].CreationTimestamp.After(result.runs[j].CreationTimestamp.Time)
	})

	return result
}

func (c *Collector) Summary(snapshot collected) Summary {
	active := ActiveCounts{}
	history := RunCounts{}

	for i := range snapshot.runs {
		switch snapshot.runs[i].Status.Phase {
		case signalv1.RunPhasePending:
			active.Pending++
		case signalv1.RunPhaseRunning:
			active.Running++
		case signalv1.RunPhaseSucceeded:
			if diagnosticRun(&snapshot.runs[i]) {
				continue
			}

			if strings.EqualFold(snapshot.runs[i].Status.Verdict, "Pass") {
				history.Passed++
			} else {
				history.Inconclusive++
			}
		case signalv1.RunPhaseFailed:
			if !diagnosticRun(&snapshot.runs[i]) {
				history.Failed++
			}
		case signalv1.RunPhaseInconclusive, signalv1.RunPhaseCancelled:
			if !diagnosticRun(&snapshot.runs[i]) {
				history.Inconclusive++
			}
		}
	}

	nodeCounts := map[string]int{}
	currentNodes := CurrentNodeCounts{}

	for _, node := range snapshot.nodes {
		key := node.Labels[signal.StateLabel] + "/" + node.Labels[signal.EligibilityLabel]
		if key == "/" {
			key = "unknown/unknown"
		}

		nodeCounts[key]++

		switch node.Labels[signal.EligibilityLabel] {
		case "production":
			currentNodes.Healthy++
		case "blocked":
			currentNodes.Blocked++
		default:
			currentNodes.Unknown++
		}
	}

	return Summary{
		Active:          active,
		CurrentNodes:    currentNodes,
		RetainedHistory: history,
		LatestOutcomes:  latestOutcomes(snapshot),
		NodeCounts:      nodeCounts,
		Controller:      componentHealth(snapshot.pods, "project-signal-controller", snapshot.errors, "CertificationRun"),
		Argo:            componentHealth(snapshot.pods, "workflow-controller", snapshot.errors, "Workflow"),
		Reasons:         reasonExplanations,
		LastRefresh:     snapshot.refreshed,
		Partial:         len(snapshot.errors) > 0,
		Errors:          snapshot.errors,
	}
}

func (c *Collector) Runs(snapshot collected, offset, limit int) RunList {
	offset, limit = pageBounds(offset, limit, len(snapshot.runs))
	items := make([]RunSummary, 0, limit)

	for i := offset; i < min(offset+limit, len(snapshot.runs)); i++ {
		items = append(items, buildRunSummary(&snapshot.runs[i], snapshot))
	}

	return RunList{
		Items:       items,
		Total:       len(snapshot.runs),
		Offset:      offset,
		Limit:       limit,
		LastRefresh: snapshot.refreshed,
		Partial:     len(snapshot.errors) > 0,
		Errors:      snapshot.errors,
	}
}

func (c *Collector) Nodes(snapshot collected) NodeList {
	items := make([]NodeSummary, 0, len(snapshot.nodes))
	profileStatus := recurringStatusByNode(snapshot.profiles)
	latestRuns := latestRunByNode(snapshot.runs)

	for name, node := range snapshot.nodes {
		profileName := firstNonempty(node.Annotations[signal.ProfileNameAnnotation], node.Labels[signal.ProfileLabel])

		status := NodeSummary{
			Alias:           snapshot.aliases[name],
			HardwareClass:   hardwareClass(node),
			Profile:         profileName,
			State:           firstNonempty(node.Labels[signal.StateLabel], "unknown"),
			Eligibility:     firstNonempty(node.Labels[signal.EligibilityLabel], "unknown"),
			ActiveRun:       node.Annotations[signal.ReservationLabel],
			LastRunAt:       optionalTime(parseTime(node.Annotations[signal.LastRunAtAnnotation])),
			NextDueAt:       optionalTime(parseTime(node.Annotations[signal.NextDueAtAnnotation])),
			RecurringReason: node.Annotations[signal.RecurringReasonAnnotation],
			Explanation:     explainReason(node.Annotations[signal.RecurringReasonAnnotation]),
		}
		if suspended, err := strconv.ParseBool(node.Annotations[signal.RecurringSuspendedAnnotation]); err == nil {
			status.RecurringSuspended = suspended
		}

		if recurring, ok := profileStatus[profileNodeKey(profileName, name)]; ok {
			status.NextDueAt = optionalTime(metaTime(recurring.NextDueAt))
			status.RecurringSuspended = recurring.Suspended
			status.RecurringReason = recurring.Reason
			status.Explanation = explainReason(recurring.Reason)
		}

		if run, ok := latestRuns[name]; ok {
			status.LastVerdict = run.Status.Verdict

			status.LastReason = sanitizeDisplay(run.Status.Reason)
			if run.Status.FinishedAt != nil {
				status.LastRunAt = optionalTime(run.Status.FinishedAt.Time)
			}
		}

		items = append(items, status)
	}

	sort.Slice(items, func(i, j int) bool { return items[i].Alias < items[j].Alias })

	return NodeList{
		Items:       items,
		LastRefresh: snapshot.refreshed,
		Partial:     len(snapshot.errors) > 0,
		Errors:      snapshot.errors,
	}
}

func (c *Collector) Run(ctx context.Context, snapshot collected, name string) (RunDetail, error) {
	for i := range snapshot.runs {
		if snapshot.runs[i].Name != name {
			continue
		}

		return c.buildRunDetail(ctx, &snapshot.runs[i], snapshot), nil
	}

	return RunDetail{}, apierrors.NewNotFound(signalv1.GroupVersion.WithResource("certificationruns").GroupResource(), name)
}

func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}

	return time.Now().UTC()
}

func (c *collected) addError(resource string, err error) {
	c.errors = append(c.errors, APIError{Resource: resource, Message: sanitizeDisplay(err.Error())})
}

func approvedNodeNames(runs []signalv1.CertificationRun, profiles []signalv1.CertificationProfile) []string {
	names := map[string]struct{}{}

	for i := range runs {
		for _, participant := range runs[i].Spec.Participants {
			if participant.NodeName != "" {
				names[participant.NodeName] = struct{}{}
			}
		}
	}

	for i := range profiles {
		if profiles[i].Spec.Recurring == nil {
			continue
		}

		for _, name := range profiles[i].Spec.Recurring.NodeNames {
			if name != "" && !strings.HasPrefix(name, "REPLACE_WITH_") {
				names[name] = struct{}{}
			}
		}
	}

	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}

	sort.Strings(result)

	return result
}

func nodeAliases(nodes map[string]*corev1.Node) map[string]string {
	names := make([]string, 0, len(nodes))
	for name := range nodes {
		names = append(names, name)
	}

	sort.Slice(names, func(i, j int) bool {
		left, right := hardwareClass(nodes[names[i]]), hardwareClass(nodes[names[j]])
		if left == right {
			return names[i] < names[j]
		}

		return left < right
	})

	counts := map[string]int{}
	aliases := map[string]string{}

	for _, name := range names {
		class := strings.ToLower(safeAliasPart(hardwareClass(nodes[name])))
		counts[class]++
		aliases[name] = fmt.Sprintf("%s-node-%d", class, counts[class])
	}

	return aliases
}

func safeAliasPart(value string) string {
	var builder strings.Builder

	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			builder.WriteRune(r)
		case builder.Len() > 0:
			builder.WriteByte('-')
		}
	}

	result := strings.Trim(builder.String(), "-")
	if result == "" {
		return "gpu"
	}

	return result
}

func hardwareClass(node *corev1.Node) string {
	product := node.Labels["nvidia.com/gpu.product"]
	if product == "" {
		product = node.Labels["node.kubernetes.io/instance-type"]
	}

	if product == "" {
		return "gpu"
	}

	upper := strings.ToUpper(product)
	for _, class := range []string{"GB200", "H200", "H100", "A100"} {
		if strings.Contains(upper, class) {
			return class
		}
	}

	return "gpu"
}

func buildRunSummary(run *signalv1.CertificationRun, snapshot collected) RunSummary {
	workflow := workflowForRun(run, snapshot.workflows)
	mpiJob := mpiJobForRun(run, workflow, snapshot.mpiJobs)
	nextDue := nextDueForRun(run, snapshot.profiles)
	aliases := make([]string, 0, len(run.Spec.Participants))
	hardware := "gpu"

	for _, participant := range run.Spec.Participants {
		aliases = append(aliases, firstNonempty(snapshot.aliases[participant.NodeName], "approved-node"))
		if node := snapshot.nodes[participant.NodeName]; node != nil {
			hardware = hardwareClass(node)
		}
	}

	started := metaTime(run.Status.StartedAt)
	finished := metaTime(run.Status.FinishedAt)
	purpose, scope := runPurpose(run)
	classification, superseded := runClassification(run, snapshot.runs)

	timingLabel := "elapsed"
	if run.Status.Phase.Terminal() {
		timingLabel = "completed in"
	}

	return RunSummary{
		Name:           run.Name,
		Profile:        run.Spec.Profile.Name,
		HardwareClass:  hardware,
		NodeAliases:    aliases,
		Phase:          string(run.Status.Phase),
		Execution:      run.Status.ExecutionState,
		Verdict:        run.Status.Verdict,
		Reason:         sanitizeDisplay(run.Status.Reason),
		Explanation:    explainReason(run.Status.Reason),
		OutcomeType:    outcomeType(run.Status.Phase, run.Status.ExecutionState, run.Status.Verdict),
		StartedAt:      optionalTime(started),
		FinishedAt:     optionalTime(finished),
		Duration:       runDuration(started, finished, snapshot.refreshed),
		NextDueAt:      optionalTime(nextDue),
		WorkflowName:   objectName(workflow),
		WorkflowPhase:  nestedString(workflow, "status", "phase"),
		MPIJobName:     objectName(mpiJob),
		MPIJobPhase:    mpiPhase(mpiJob),
		ScriptDigest:   shortDigest(run.Spec.Script.Digest),
		Purpose:        purpose,
		Scope:          scope,
		Classification: classification,
		Superseded:     superseded,
		TimingLabel:    timingLabel,
		Trigger:        runTrigger(run, snapshot.profiles),
		Attempt:        run.Status.Workflow.Attempt,
	}
}

func (c *Collector) buildRunDetail(ctx context.Context, run *signalv1.CertificationRun, snapshot collected) RunDetail {
	workflow := workflowForRun(run, snapshot.workflows)
	mpiJob := mpiJobForRun(run, workflow, snapshot.mpiJobs)
	summary := buildRunSummary(run, snapshot)
	participants := make([]ParticipantStatus, 0, len(run.Spec.Participants))

	for _, participant := range run.Spec.Participants {
		node := snapshot.nodes[participant.NodeName]

		item := ParticipantStatus{Alias: firstNonempty(snapshot.aliases[participant.NodeName], "approved-node")}
		if node != nil {
			item.State = node.Labels[signal.StateLabel]
			item.Eligibility = node.Labels[signal.EligibilityLabel]
			item.Reserved = node.Annotations[signal.ReservationLabel] == run.Name
		}

		participants = append(participants, item)
	}

	workflowNodes := extractWorkflowNodes(workflow)
	plan, planIssues := declaredStagePlan(run)
	events := []stageEvent{}
	logError := ""

	pod := workflowPod(workflow, snapshot.pods)
	if pod != nil && c.PodLogs != nil {
		content, err := c.PodLogs.ReadPodLog(ctx, c.Namespace, pod.Name, "main", maxStageLogLines, maxStageLogBytes)
		if err != nil {
			logError = "Structured stage events are temporarily unavailable."
		} else {
			events = parseStageEvents(content)
		}
	}

	executionPlan, eventIssues, currentStage := reconcileStagePlan(plan, events, run.Status.Phase.Terminal(), snapshot.refreshed)

	planIssues = append(planIssues, eventIssues...)
	if logError != "" {
		planIssues = append(planIssues, logError)
	}

	argoTask := argoTaskStatus(workflowNodes)

	return RunDetail{
		RunSummary:    summary,
		Admission:     admissionStages(run),
		ExecutionPlan: executionPlan,
		Finalization:  finalizationStages(run),
		PlanIssues:    planIssues,
		CurrentStage:  currentStage,
		Script:        scriptStatus(run, pod),
		ArgoTask:      argoTask,
		WorkflowNodes: workflowNodes,
		Outputs:       workflowOutputs(workflow),
		MPIJob:        extractMPIJobStatus(mpiJob),
		Participants:  participants,
		Cleanup: CleanupStatus{
			ReservationsReleased: run.Status.Cleanup.ReservationsReleased,
			SnapshotDeleted:      run.Status.Cleanup.SnapshotDeleted,
		},
		Partial: len(snapshot.errors) > 0,
		Errors:  snapshot.errors,
	}
}

func workflowForRun(run *signalv1.CertificationRun, workflows []unstructured.Unstructured) *unstructured.Unstructured {
	for i := range workflows {
		if run.Status.Workflow.UID != "" && string(workflows[i].GetUID()) == run.Status.Workflow.UID {
			return &workflows[i]
		}
	}

	for i := range workflows {
		if ownedByUID(&workflows[i], run.UID) {
			return &workflows[i]
		}
	}

	return nil
}

func mpiJobForRun(
	run *signalv1.CertificationRun,
	workflow *unstructured.Unstructured,
	mpiJobs []unstructured.Unstructured,
) *unstructured.Unstructured {
	if workflow != nil {
		for i := range mpiJobs {
			if ownedByUID(&mpiJobs[i], workflow.GetUID()) {
				return &mpiJobs[i]
			}
		}
	}

	for i := range mpiJobs {
		if mpiJobs[i].GetLabels()[signal.SnapshotRunLabel] == run.Name {
			return &mpiJobs[i]
		}
	}

	return nil
}

func ownedByUID(object metav1.Object, uid types.UID) bool {
	if uid == "" {
		return false
	}

	for _, owner := range object.GetOwnerReferences() {
		if owner.UID == uid {
			return true
		}
	}

	return false
}

func extractWorkflowNodes(workflow *unstructured.Unstructured) []WorkflowNode {
	if workflow == nil {
		return nil
	}

	nodes, found, err := unstructured.NestedMap(workflow.Object, "status", "nodes")
	if err != nil || !found {
		return nil
	}

	result := make([]WorkflowNode, 0, len(nodes))

	for id, raw := range nodes {
		node, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		result = append(result, WorkflowNode{
			ID:          id,
			Name:        sanitizeDisplay(stringValue(node["name"])),
			DisplayName: sanitizeDisplay(stringValue(node["displayName"])),
			Template:    sanitizeDisplay(stringValue(node["templateName"])),
			Type:        sanitizeDisplay(stringValue(node["type"])),
			Phase:       sanitizeDisplay(stringValue(node["phase"])),
			Reason:      sanitizeDisplay(stringValue(node["reason"])),
			Message:     sanitizeDisplay(stringValue(node["message"])),
			StartedAt:   optionalTime(parseTime(stringValue(node["startedAt"]))),
			FinishedAt:  optionalTime(parseTime(stringValue(node["finishedAt"]))),
		})
	}

	sort.Slice(result, func(i, j int) bool {
		left := timeValue(result[i].StartedAt)

		right := timeValue(result[j].StartedAt)
		if left.Equal(right) {
			return result[i].DisplayName < result[j].DisplayName
		}

		return left.Before(right)
	})

	return result
}

func workflowOutputs(workflow *unstructured.Unstructured) map[string]string {
	if workflow == nil {
		return nil
	}

	parameters, found, err := unstructured.NestedSlice(workflow.Object, "status", "outputs", "parameters")
	if err != nil || !found {
		return nil
	}

	outputs := map[string]string{}

	for _, raw := range parameters {
		parameter, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		name := sanitizeDisplay(stringValue(parameter["name"]))

		value := sanitizeDisplay(stringValue(parameter["value"]))
		if name != "" {
			outputs[name] = value
		}
	}

	return outputs
}

func extractMPIJobStatus(job *unstructured.Unstructured) *MPIJobStatus {
	if job == nil {
		return nil
	}

	result := &MPIJobStatus{
		Name:            job.GetName(),
		Phase:           mpiPhase(job),
		ReplicaStatuses: map[string]ReplicaState{},
	}

	conditions, found, err := unstructured.NestedSlice(job.Object, "status", "conditions")
	if err != nil || !found {
		conditions = nil
	}

	for _, raw := range conditions {
		condition, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		result.Conditions = append(result.Conditions, MPIJobCondition{
			Type:               sanitizeDisplay(stringValue(condition["type"])),
			Status:             sanitizeDisplay(stringValue(condition["status"])),
			Reason:             sanitizeDisplay(stringValue(condition["reason"])),
			Message:            sanitizeDisplay(stringValue(condition["message"])),
			LastTransitionTime: optionalTime(parseTime(stringValue(condition["lastTransitionTime"]))),
		})
	}

	statuses, found, err := unstructured.NestedMap(job.Object, "status", "replicaStatuses")
	if err != nil || !found {
		statuses = nil
	}

	for name, raw := range statuses {
		status, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		result.ReplicaStatuses[name] = ReplicaState{
			Active:    intValue(status["active"]),
			Succeeded: intValue(status["succeeded"]),
			Failed:    intValue(status["failed"]),
		}
	}

	return result
}

func mpiPhase(job *unstructured.Unstructured) string {
	if job == nil {
		return ""
	}

	conditions, found, err := unstructured.NestedSlice(job.Object, "status", "conditions")
	if err != nil || !found {
		return ""
	}

	for i := len(conditions) - 1; i >= 0; i-- {
		condition, ok := conditions[i].(map[string]any)
		if ok && strings.EqualFold(stringValue(condition["status"]), "True") {
			return stringValue(condition["type"])
		}
	}

	return ""
}

func componentHealth(pods []corev1.Pod, component string, apiErrors []APIError, resource string) HealthStatus {
	for _, apiErr := range apiErrors {
		if apiErr.Resource == resource || apiErr.Resource == "Pod" {
			return HealthStatus{Status: "unknown", Message: "Required Kubernetes data is unavailable."}
		}
	}

	found := false

	for i := range pods {
		if pods[i].Labels["app.kubernetes.io/name"] != component &&
			pods[i].Labels["app"] != component {
			continue
		}

		found = true

		if podReady(&pods[i]) {
			return HealthStatus{Status: "healthy"}
		}
	}

	if found {
		return HealthStatus{Status: "degraded", Message: "Matching pods are not Ready."}
	}

	return HealthStatus{Status: "unknown", Message: "No matching component pod was visible."}
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}

	return false
}

func recurringStatusByNode(profiles []signalv1.CertificationProfile) map[string]signalv1.RecurringNodeStatus {
	result := map[string]signalv1.RecurringNodeStatus{}

	for i := range profiles {
		for _, status := range profiles[i].Status.Nodes {
			result[profileNodeKey(profiles[i].Name, status.NodeName)] = status
		}
	}

	return result
}

func profileNodeKey(profile, node string) string {
	return profile + "\x00" + node
}

func latestRunByNode(runs []signalv1.CertificationRun) map[string]*signalv1.CertificationRun {
	result := map[string]*signalv1.CertificationRun{}

	for i := range runs {
		if !runs[i].Status.Phase.Terminal() || !certificationRun(&runs[i]) {
			continue
		}

		for _, participant := range runs[i].Spec.Participants {
			current := result[participant.NodeName]
			if current == nil || current.CreationTimestamp.Before(&runs[i].CreationTimestamp) {
				result[participant.NodeName] = &runs[i]
			}
		}
	}

	return result
}

func nextDueForRun(run *signalv1.CertificationRun, profiles []signalv1.CertificationProfile) time.Time {
	for i := range profiles {
		if profiles[i].Name != run.Spec.Profile.Name {
			continue
		}

		for _, status := range profiles[i].Status.Nodes {
			for _, participant := range run.Spec.Participants {
				if status.NodeName == participant.NodeName {
					return metaTime(status.NextDueAt)
				}
			}
		}
	}

	return time.Time{}
}

func runTrigger(run *signalv1.CertificationRun, profiles []signalv1.CertificationProfile) string {
	for _, owner := range run.OwnerReferences {
		if owner.Kind == "CertificationProfile" {
			return "recurring"
		}
	}

	if run.Annotations[signal.RunTriggerAnnotation] == "recurring" {
		return "recurring"
	}

	for i := range profiles {
		if profiles[i].Name != run.Spec.Profile.Name {
			continue
		}

		for _, status := range profiles[i].Status.Nodes {
			if run.Name != "" && status.LastSuccessfulRun == run.Name {
				return "recurring"
			}
		}
	}

	return "manual"
}

func pageBounds(offset, limit, total int) (int, int) {
	if offset < 0 {
		offset = 0
	}

	if offset > total {
		offset = total
	}

	if limit <= 0 {
		limit = defaultPageLimit
	}

	if limit > maxPageLimit {
		limit = maxPageLimit
	}

	return offset, limit
}

func runDuration(started, finished, now time.Time) string {
	if started.IsZero() {
		return ""
	}

	if finished.IsZero() {
		finished = now
	}

	return finished.Sub(started).Round(time.Second).String()
}

func objectName(object *unstructured.Unstructured) string {
	if object == nil {
		return ""
	}

	return object.GetName()
}

func nestedString(object *unstructured.Unstructured, fields ...string) string {
	if object == nil {
		return ""
	}

	value, found, err := unstructured.NestedString(object.Object, fields...)
	if err != nil || !found {
		return ""
	}

	return sanitizeDisplay(value)
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case fmt.Stringer:
		return typed.String()
	default:
		return ""
	}
}

func intValue(value any) int64 {
	switch typed := value.(type) {
	case int64:
		return typed
	case int32:
		return int64(typed)
	case int:
		return int64(typed)
	case float64:
		return int64(typed)
	default:
		return 0
	}
}

func parseTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}

	return parsed
}

func metaTime(value *metav1.Time) time.Time {
	if value == nil {
		return time.Time{}
	}

	return value.Time
}

func optionalTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}

	value = value.UTC()

	return &value
}

func timeValue(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}

	return *value
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}

	return ""
}

func IsNotFound(err error) bool {
	return apierrors.IsNotFound(err)
}
