// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/cmd/centerpivot/internal/contract"
	"github.com/Azure/unbounded/cmd/centerpivot/internal/drain"
	"github.com/Azure/unbounded/cmd/centerpivot/internal/identity"
	"github.com/Azure/unbounded/cmd/centerpivot/internal/names"
	"github.com/Azure/unbounded/cmd/centerpivot/internal/omaha"
)

// MachineNodeIndex indexes Machines by the name of their Node.
const MachineNodeIndex = "centerpivot.nodeName"

// IndexMachineNode is the MachineNodeIndex function.
func IndexMachineNode(obj client.Object) []string {
	machine, ok := obj.(*unboundedv1alpha3.Machine)
	if !ok {
		return nil
	}

	return []string{machine.NodeName()}
}

// Drainer drains nodes; *drain.Drainer is the real one.
type Drainer interface {
	Check(ctx context.Context, node string, opts drain.Options) error
	Evict(ctx context.Context, node string, opts drain.Options) (int, error)
	Workloads(ctx context.Context, node string, opts drain.Options) ([]drain.Workload, error)
	Recovered(ctx context.Context, workloads []drain.Workload) (bool, error)
}

const (
	// pollInterval is how often a node waiting on its agent or its
	// workloads is looked at again. Changes to its Node are seen at once.
	pollInterval = 30 * time.Second
	// drainInterval is how often a drain asks again to evict the pods left.
	drainInterval = 10 * time.Second
)

// Reconciler takes each node with an update slot through its update, one
// step per pass, without blocking (designs/centerpivot.md, section 4.4).
type Reconciler struct {
	Client client.Client
	// Reader reads CA ConfigMaps straight from the API server, so the
	// namespace's ConfigMaps aren't cached.
	Reader    client.Reader
	Namespace string
	Nebraska  Nebraska
	Drainer   Drainer
	Clock     clock.PassiveClock
	Recorder  events.EventRecorder
}

// SetupWithManager runs the reconciler for every Node, and for the Node of
// every Machine whose state changes. One node is reconciled at a time.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("centerpivot").
		For(&corev1.Node{}).
		Watches(&unboundedv1alpha3.Machine{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, obj client.Object) []reconcile.Request {
				machine, ok := obj.(*unboundedv1alpha3.Machine)
				if !ok {
					return nil
				}

				return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: machine.NodeName()}}}
			},
		)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

// pass is one reconcile of one node.
type pass struct {
	// node is nil while the Node doesn't exist.
	node     *corev1.Node
	machine  *unboundedv1alpha3.Machine
	state    State
	settings Settings
	now      time.Time
}

// Reconcile takes one step of the node's update.
func (r *Reconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	machine, err := r.machineFor(ctx, req.Name)
	if err != nil || machine == nil {
		return reconcile.Result{}, err
	}

	node := &corev1.Node{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: req.Name}, node); err != nil {
		if !apierrors.IsNotFound(err) {
			return reconcile.Result{}, fmt.Errorf("get Node %s: %w", req.Name, err)
		}

		node = nil
	}

	settings, ok, err := r.settingsFor(ctx, siteOf(node, machine))
	if err != nil || !ok {
		return reconcile.Result{}, err
	}

	p := &pass{node: node, machine: machine, state: StateOf(machine), settings: settings, now: r.now()}

	// The Node catches up with the state first, in case the last pass ended
	// before it did.
	if err := r.syncNode(ctx, p.node, p.state); err != nil {
		return reconcile.Result{}, err
	}

	next, after, err := r.advance(ctx, p)
	if err != nil {
		return reconcile.Result{}, err
	}

	if next.Phase != p.state.Phase {
		log.FromContext(ctx).Info("Node moved to a new phase", "from", p.state.Phase, "to", next.Phase,
			"targetVersion", next.TargetVersion, "reason", next.Reason)
	}

	changed, err := differs(p.state, next)
	if err != nil {
		return reconcile.Result{}, err
	}

	// A Machine never given a slot keeps having no state.
	if changed {
		if err := writeState(ctx, r.Client, machine, next); err != nil {
			return reconcile.Result{}, err
		}
	}

	if err := r.syncNode(ctx, p.node, next); err != nil {
		return reconcile.Result{}, err
	}

	return reconcile.Result{RequeueAfter: after}, nil
}

func (r *Reconciler) now() time.Time {
	// Whole seconds, without a monotonic reading, as the state stores them.
	return r.Clock.Now().UTC().Truncate(time.Second)
}

func (r *Reconciler) machineFor(ctx context.Context, nodeName string) (*unboundedv1alpha3.Machine, error) {
	var machines unboundedv1alpha3.MachineList
	if err := r.Client.List(ctx, &machines, client.MatchingFields{MachineNodeIndex: nodeName}); err != nil {
		return nil, fmt.Errorf("list the Machines of Node %s: %w", nodeName, err)
	}

	if len(machines.Items) != 1 {
		return nil, nil
	}

	return &machines.Items[0], nil
}

func (r *Reconciler) settingsFor(ctx context.Context, siteName string) (Settings, bool, error) {
	if siteName == "" {
		return Settings{}, false, nil
	}

	var site unboundedv1alpha3.Site
	if err := r.Client.Get(ctx, client.ObjectKey{Name: siteName}, &site); err != nil {
		return Settings{}, false, client.IgnoreNotFound(err)
	}

	settings, ok := SettingsFor(&site)

	return settings, ok, nil
}

// advance takes the step of the node's phase, and returns its next state, and
// when to look again if nothing changes before.
func (r *Reconciler) advance(ctx context.Context, p *pass) (State, time.Duration, error) {
	switch p.state.Phase {
	case PhaseChecking:
		return r.checking(ctx, p)
	case PhaseStaging:
		return r.staging(ctx, p)
	case PhaseDraining:
		return r.draining(ctx, p)
	case PhaseFinalizing:
		return r.finalizing(ctx, p)
	case PhaseRestarting:
		return r.restarting(p)
	case PhaseRecovering:
		return r.recovering(ctx, p)
	default:
		return p.state, 0, nil
	}
}

// checking checks that the node could be drained, then checks Nebraska for an
// update as the node. Every outcome but a grant gives the slot back.
func (r *Reconciler) checking(ctx context.Context, p *pass) (State, time.Duration, error) {
	s := p.state
	s.LastCheck = p.now

	if p.node == nil {
		return s.idle(p.now, "the Node doesn't exist"), 0, nil
	}

	check, err := r.checkFor(p)
	if err != nil {
		return s.idle(p.now, err.Error()), 0, nil
	}

	if err := r.Drainer.Check(ctx, p.node.Name, p.settings.Drain); err != nil {
		reason := "the node can't be drained now: " + err.Error()
		r.event(p, corev1.EventTypeWarning, "DrainBlocked", reason)

		return s.idle(p.now, reason), 0, nil
	}

	result, err := r.updateCheck(ctx, p, check)
	if err != nil {
		return s.idle(p.now, "the update check failed: "+err.Error()), 0, nil
	}

	switch result.Kind {
	case omaha.Granted:
		if result.Version == check.Version {
			return s.idle(p.now, ""), 0, nil
		}

		return r.grant(p, s, check, result.Version), 0, nil
	case omaha.InProgress:
		if err := r.endGrant(ctx, p, sourceOf(check)); err != nil {
			return s, 0, err
		}

		return s.idle(p.now, "ended an update Nebraska had in progress for the node"), 0, nil
	case omaha.Refused:
		return s.idle(p.now, "Nebraska refused the update check: "+result.Status), 0, nil
	default:
		return s.idle(p.now, ""), 0, nil
	}
}

// grant starts the update Nebraska granted: it asks the agent to stage it.
func (r *Reconciler) grant(p *pass, s State, check omaha.Check, version string) State {
	if version != s.TargetVersion {
		s.Attempts = 0
	}

	s.TargetVersion = version
	s.GrantedAt = p.now
	s.NodeUID = p.node.UID
	s.Workloads = nil
	s.RecoveredAt = time.Time{}
	s.KeepTaint = false
	s.Reason = ""

	r.event(p, corev1.EventTypeNormal, "UpdateGranted", fmt.Sprintf("Nebraska granted %s; staging it", version))

	return s.request(PhaseStaging, p.now,
		contract.NewRequest(contract.OperationStage, "", version, sourceOf(check)))
}

// staging waits for the agent to stage the version, then starts the drain.
func (r *Reconciler) staging(ctx context.Context, p *pass) (State, time.Duration, error) {
	s := p.state

	if p.node == nil || p.node.UID != s.NodeUID {
		return r.rebuilt(ctx, p)
	}

	status := answer(p.node, Keys.Status, s.Request)
	if status == nil || !status.Code.Final() {
		return waitForStatus(p, false), pollInterval, nil
	}

	switch status.Code {
	case contract.CodeSuccess:
		workloads, err := r.Drainer.Workloads(ctx, p.node.Name, p.settings.Drain)
		if err != nil {
			return s, 0, err
		}

		s.Phase = PhaseDraining
		s.Since = p.now
		s.Workloads = workloads

		return s, 0, nil
	case contract.CodeAlreadyAtTarget:
		s.Attempts = 0

		return s.idle(p.now, "the node already runs "+s.TargetVersion), 0, nil
	case contract.CodeInvalidRequest:
		// Nebraska's channel may have moved on since the grant.
		check, err := r.checkFor(p)
		if err != nil {
			return s.held(p.now, err.Error(), false), 0, nil
		}

		result, err := r.updateCheck(ctx, p, check)
		if err == nil && result.Kind == omaha.Granted && result.Version != s.TargetVersion && result.Version != check.Version {
			return r.grant(p, s, check, result.Version), 0, nil
		}

		return s.held(p.now, "the agent refused the stage request: "+status.Message, false), 0, nil
	default:
		return s.failed(p.now, fmt.Sprintf("staging %s failed: %s: %s", s.TargetVersion, status.Code, status.Message)), 0, nil
	}
}

// draining taints the node and evicts its pods, then asks the agent to
// finalize, if the grant still has time for the restart.
func (r *Reconciler) draining(ctx context.Context, p *pass) (State, time.Duration, error) {
	s := p.state

	if p.node == nil || p.node.UID != s.NodeUID {
		return r.rebuilt(ctx, p)
	}

	if p.now.Sub(s.Since) > p.settings.DrainDeadline {
		if err := r.endGrant(ctx, p, requestSource(s.Request)); err != nil {
			return s, 0, err
		}

		reason := "the drain missed its deadline: " + s.Reason
		r.event(p, corev1.EventTypeWarning, "DrainDeadline", reason)

		return s.failed(p.now, reason), 0, nil
	}

	if !p.settings.TimeLeftToRestart(s.GrantedAt, p.now) {
		if err := r.endGrant(ctx, p, requestSource(s.Request)); err != nil {
			return s, 0, err
		}

		return s.idle(p.now, "too little of the grant's time was left to restart"), 0, nil
	}

	left, err := r.Drainer.Evict(ctx, p.node.Name, p.settings.Drain)
	if err != nil {
		s.Reason = err.Error()

		return s, drainInterval, nil
	}

	if left > 0 {
		s.Reason = fmt.Sprintf("%d pods left to move", left)

		return s, drainInterval, nil
	}

	s.Reason = ""

	return s.request(PhaseFinalizing, p.now, contract.NewRequest(contract.OperationFinalize,
		s.Request.NodeUpdateID, s.TargetVersion, requestSource(s.Request))), 0, nil
}

// finalizing waits for the agent to set the node to start from the new
// version, and to restart it.
func (r *Reconciler) finalizing(ctx context.Context, p *pass) (State, time.Duration, error) {
	s := p.state

	// The node may restart before its finalize status is seen.
	if commit := answer(p.node, Keys.CommitStatus, s.Request); commit != nil {
		s.Phase = PhaseRestarting
		s.Since = p.now

		return s, 0, nil
	}

	if p.node == nil || p.node.UID != s.NodeUID {
		// A node that restarted into the new version before its Node was
		// rebuilt reports its commit on the new Node.
		if p.node == nil || p.now.Sub(s.RequestedAt) < p.settings.StatusDeadline {
			return s, pollInterval, nil
		}

		return r.rebuilt(ctx, p)
	}

	status := answer(p.node, Keys.Status, s.Request)
	if status == nil || !status.Code.Final() {
		return waitForStatus(p, true), pollInterval, nil
	}

	switch status.Code {
	case contract.CodeSuccess:
		s.Phase = PhaseRestarting
		s.Since = p.now

		return s, 0, nil
	case contract.CodeNotStaged:
		return s.request(PhaseStaging, p.now, contract.NewRequest(contract.OperationStage, "",
			s.TargetVersion, requestSource(s.Request))), 0, nil
	case contract.CodeOperationFailed:
		return s.failed(p.now, fmt.Sprintf("finalizing %s failed: %s", s.TargetVersion, status.Message)), 0, nil
	default:
		// After AgentInternalError, the node may start from the new version
		// at its next restart, so it stays tainted.
		return s.held(p.now, fmt.Sprintf("finalizing %s failed: %s: %s", s.TargetVersion, status.Code, status.Message), true), 0, nil
	}
}

// restarting waits for the node to come back committed, running the new
// version, and Ready.
func (r *Reconciler) restarting(p *pass) (State, time.Duration, error) {
	s := p.state

	var missing []string

	if p.node == nil {
		missing = append(missing, "the Node")
	} else {
		commit := answer(p.node, Keys.CommitStatus, s.Request)

		switch {
		case commit == nil || !commit.Code.Final():
			missing = append(missing, "the commit status")
		case commit.Code == contract.CodeTargetBootFailed:
			return s.held(p.now, fmt.Sprintf("the node didn't boot %s, and went back to its old version: %s", s.TargetVersion, commit.Message), false), 0, nil
		case commit.Code != contract.CodeSuccess:
			return s.held(p.now, fmt.Sprintf("committing %s failed: %s: %s", s.TargetVersion, commit.Code, commit.Message), false), 0, nil
		}

		if info, err := agentInfoOf(p.node); err != nil || info.CurrentVersion != s.TargetVersion {
			missing = append(missing, "agent-info showing "+s.TargetVersion)
		}

		if !ready(p.node) {
			missing = append(missing, "Ready")
		}
	}

	if len(missing) == 0 {
		r.event(p, corev1.EventTypeNormal, "Updated", "the node runs "+s.TargetVersion)

		s.Phase = PhaseRecovering
		s.Since = p.now
		s.Reason = ""

		return s, 0, nil
	}

	if p.now.Sub(s.Since) > p.settings.RestartDeadline {
		return s.held(p.now, "the node didn't come back in time, without "+strings.Join(missing, ", "), false), 0, nil
	}

	return s, pollInterval, nil
}

// recovering waits for the workloads the drain moved to recover, and then
// the settle time, before giving the slot back.
func (r *Reconciler) recovering(ctx context.Context, p *pass) (State, time.Duration, error) {
	s := p.state

	recovered, err := r.Drainer.Recovered(ctx, s.Workloads)
	if err != nil {
		return s, 0, err
	}

	if !recovered {
		if p.now.Sub(s.Since) > p.settings.RecoveryDeadline {
			return s.held(p.now, "the workloads moved off the node didn't recover in time", false), 0, nil
		}

		return s, pollInterval, nil
	}

	if s.RecoveredAt.IsZero() {
		s.RecoveredAt = p.now
	}

	if wait := p.settings.SettleTime - p.now.Sub(s.RecoveredAt); wait > 0 {
		return s, wait, nil
	}

	s.Attempts = 0

	return s.idle(p.now, "updated to "+s.TargetVersion), 0, nil
}

// rebuilt ends an update whose Node was rebuilt before it restarted. The
// update starts again at a later check. While the Node doesn't exist, it
// waits for it to return.
func (r *Reconciler) rebuilt(ctx context.Context, p *pass) (State, time.Duration, error) {
	if p.node == nil {
		return p.state, pollInterval, nil
	}

	if err := r.endGrant(ctx, p, requestSource(p.state.Request)); err != nil {
		return p.state, 0, err
	}

	return p.state.idle(p.now, "the Node was rebuilt during the update"), 0, nil
}

// waitForStatus waits for the agent's status until the status deadline, then
// writes the request again, and holds the node after maxAttempts writes.
func waitForStatus(p *pass, keepTaint bool) State {
	s := p.state
	if p.now.Sub(s.RequestedAt) < p.settings.StatusDeadline {
		return s
	}

	if s.Writes >= maxAttempts {
		return s.held(p.now, fmt.Sprintf("the agent didn't answer the %s request, written %d times", s.Request.Operation, s.Writes), keepTaint)
	}

	s.Writes++
	s.RequestedAt = p.now

	return s
}

// endGrant ends the node's open grant from source in Nebraska: a stage
// request for a version other than the one it runs makes the agent tell
// Nebraska the update in progress failed (designs/centerpivot.md, section
// 4.7).
func (r *Reconciler) endGrant(ctx context.Context, p *pass, source contract.Source) error {
	if p.node == nil {
		return nil
	}

	var current string
	if info, err := agentInfoOf(p.node); err == nil {
		current = info.CurrentVersion
	}

	version := p.state.TargetVersion
	if version == "" || version == current {
		running, err := semver.StrictNewVersion(current)
		if err != nil {
			return fmt.Errorf("end the grant of Node %s: no version other than the one it runs: %w", p.node.Name, err)
		}

		version = running.IncPatch().String()
	}

	request := contract.NewRequest(contract.OperationStage, "", version, source)
	if err := r.writeRequest(ctx, p.node, request); err != nil {
		return err
	}

	r.event(p, corev1.EventTypeNormal, "UpdateEnded", "ended the node's update in Nebraska")

	return nil
}

// checkFor returns the update check to send as the node, for its app.
func (r *Reconciler) checkFor(p *pass) (omaha.Check, error) {
	info, err := agentInfoOf(p.node)
	if err != nil {
		return omaha.Check{}, err
	}

	app, err := p.settings.AppFor(p.node.Labels)
	if err != nil {
		return omaha.Check{}, err
	}

	track := p.node.Labels[names.TrackLabel]
	if track == "" {
		track = app.DefaultTrack
	}

	if track == "" {
		return omaha.Check{}, fmt.Errorf("the node has no track, and app %s no default track", app.Name)
	}

	arch, err := omaha.Arch(p.node.Labels[corev1.LabelArchStable])
	if err != nil {
		return omaha.Check{}, err
	}

	return omaha.Check{
		Server:    p.settings.Server,
		AppID:     app.AppID,
		Track:     track,
		MachineID: info.MachineID,
		Version:   info.CurrentVersion,
		Arch:      arch,
	}, nil
}

func (r *Reconciler) updateCheck(ctx context.Context, p *pass, check omaha.Check) (omaha.Result, error) {
	var ca string

	if p.settings.CAConfigMap != "" {
		var configMap corev1.ConfigMap
		if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: p.settings.CAConfigMap}, &configMap); err != nil {
			return omaha.Result{}, fmt.Errorf("get CA ConfigMap %s: %w", p.settings.CAConfigMap, err)
		}

		ca = configMap.Data["ca.crt"]
	}

	return r.Nebraska.UpdateCheck(ctx, ca, check)
}

// syncNode gives the Node the taint and request its state calls for. It
// never removes a request: the agent's statuses refer to it.
func (r *Reconciler) syncNode(ctx context.Context, node *corev1.Node, state State) error {
	if node == nil {
		return nil
	}

	patched := node.DeepCopy()
	setTaint(patched, state.Tainted())

	if request := state.RequestToWrite(); request != nil {
		value, err := request.Marshal()
		if err != nil {
			return err
		}

		if patched.Annotations == nil {
			patched.Annotations = map[string]string{}
		}

		patched.Annotations[Keys.Request] = value
	}

	return r.patchNode(ctx, node, patched)
}

func (r *Reconciler) writeRequest(ctx context.Context, node *corev1.Node, request contract.Request) error {
	value, err := request.Marshal()
	if err != nil {
		return err
	}

	patched := node.DeepCopy()
	if patched.Annotations == nil {
		patched.Annotations = map[string]string{}
	}

	patched.Annotations[Keys.Request] = value

	return r.patchNode(ctx, node, patched)
}

// patchNode writes the changes from node to patched, if any. The patch
// carries the Node's resourceVersion.
func (r *Reconciler) patchNode(ctx context.Context, node, patched *corev1.Node) error {
	if patched.Annotations[Keys.Request] == node.Annotations[Keys.Request] && hasTaint(patched) == hasTaint(node) {
		return nil
	}

	if err := r.Client.Patch(ctx, patched, client.MergeFromWithOptions(node, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("patch Node %s: %w", node.Name, err)
	}

	*node = *patched

	return nil
}

func (r *Reconciler) event(p *pass, eventType, reason, note string) {
	if r.Recorder == nil {
		return
	}

	var regarding runtime.Object = p.machine
	if p.node != nil {
		regarding = p.node
	}

	r.Recorder.Eventf(regarding, nil, eventType, reason, "Update", "%s", note)
}

// answer returns the node's status under key if it answers the request.
func answer(node *corev1.Node, key string, request *contract.Request) *contract.Status {
	if node == nil || request == nil {
		return nil
	}

	value, ok := node.Annotations[key]
	if !ok {
		return nil
	}

	status, err := contract.ParseStatus(value)
	if err != nil || !status.Answers(*request) {
		return nil
	}

	return &status
}

// agentInfoOf returns the node's agent-info, which must have a version.
func agentInfoOf(node *corev1.Node) (identity.AgentInfo, error) {
	value, ok := node.Annotations[names.AgentInfoAnnotation]
	if !ok {
		return identity.AgentInfo{}, errors.New("the node has no agent-info")
	}

	info, err := identity.ParseAgentInfo(value)
	if err != nil {
		return identity.AgentInfo{}, err
	}

	if info.CurrentVersion == "" {
		return info, errors.New("the node's agent-info has no version")
	}

	return info, nil
}

// sourceOf returns the source a request for the check's grant names.
func sourceOf(check omaha.Check) contract.Source {
	return contract.Source{Server: check.Server, AppID: check.AppID, Track: check.Track}
}

func requestSource(request *contract.Request) contract.Source {
	return contract.Source{Server: request.Server, AppID: request.AppID, Track: request.Track}
}

// siteOf returns the node's Site, from the Node, or from the Machine while
// the Node doesn't exist.
func siteOf(node *corev1.Node, machine *unboundedv1alpha3.Machine) string {
	if node != nil && node.Labels[unboundedv1alpha3.MachineSiteLabelKey] != "" {
		return node.Labels[unboundedv1alpha3.MachineSiteLabelKey]
	}

	return machine.Labels[unboundedv1alpha3.MachineSiteLabelKey]
}

func ready(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}

	return false
}

func hasTaint(node *corev1.Node) bool {
	for _, taint := range node.Spec.Taints {
		if taint.Key == names.UpdatingTaint {
			return true
		}
	}

	return false
}

// setTaint adds or removes the updating taint, leaving every other taint.
func setTaint(node *corev1.Node, tainted bool) {
	if hasTaint(node) == tainted {
		return
	}

	if tainted {
		node.Spec.Taints = append(node.Spec.Taints, corev1.Taint{Key: names.UpdatingTaint, Effect: corev1.TaintEffectNoSchedule})

		return
	}

	taints := node.Spec.Taints[:0:0]
	for _, taint := range node.Spec.Taints {
		if taint.Key != names.UpdatingTaint {
			taints = append(taints, taint)
		}
	}

	node.Spec.Taints = taints
}
