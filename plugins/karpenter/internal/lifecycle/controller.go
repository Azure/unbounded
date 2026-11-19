// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package lifecycle holds the plugin's own controllers. They cover what the
// upstream Karpenter controllers and the shared allocation controller leave
// open between them: fencing a host as soon as its NodeClaim terminates,
// replacing a NodeClaim whose allocation failed, and releasing an allocation
// whose NodeClaim is gone once its Node runs no workload.
package lifecycle

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	podutil "sigs.k8s.io/karpenter/pkg/utils/pod"

	"github.com/Azure/unbounded/plugins/karpenter/internal/capacity"
	"github.com/Azure/unbounded/plugins/karpenter/internal/nodeclass"
	"github.com/Azure/unbounded/plugins/karpenter/internal/provider"
)

const (
	// orphanRecheck is how often an orphan that cannot be released yet is
	// looked at again when nothing about it changes.
	orphanRecheck = 30 * time.Second

	// cordonedAtAnnotation records on a Node when this controller cordoned
	// it as an orphan's Node.
	cordonedAtAnnotation = "karpenter.unbounded-cloud.io/orphan-cordoned-at"

	// podNodeField selects the Pods bound to one Node.
	podNodeField = "spec.nodeName"

	reasonMachineDeleting = "MachineDeleting"

	eventAllocationFailed = "AllocationFailed"
	eventOrphanHeld       = "OrphanedAllocationHeld"
	eventOrphanReleased   = "OrphanedAllocationReleased"
)

// UnavailableMarker remembers that a launch from an offering failed.
type UnavailableMarker interface {
	MarkUnavailable(class types.UID, offering string)
}

// Controller reconciles the MachineAllocations whose consumer is a
// NodeClaim, together with the NodeClaims they belong to.
type Controller struct {
	// Client reads from the manager cache and makes every write.
	Client client.Client
	// Reader reads the API server directly. It confirms what the cache
	// reports missing before anything is treated as gone.
	Reader   client.Reader
	Recorder record.EventRecorder
	Marker   UnavailableMarker
	// Now is the clock; it defaults to time.Now.
	Now func() time.Time
}

// Register adds the controller to the manager.
func (c *Controller) Register(_ context.Context, mgr manager.Manager) error {
	if c.Client == nil || c.Reader == nil || c.Recorder == nil || c.Marker == nil {
		return fmt.Errorf("lifecycle controller needs a client, reader, recorder and marker")
	}

	return builder.ControllerManagedBy(mgr).
		Named("unbounded.allocation.lifecycle").
		For(provider.AllocationObject(), builder.WithPredicates(predicate.NewPredicateFuncs(karpenterAllocation))).
		Watches(&v1.NodeClaim{}, handler.EnqueueRequestsFromMapFunc(allocationForClaim)).
		Complete(c)
}

func karpenterAllocation(object client.Object) bool {
	allocation, ok := object.(*unstructured.Unstructured)
	if !ok {
		return false
	}

	group, _, err := unstructured.NestedString(allocation.Object, "spec", "consumerRef", "apiGroup")
	if err != nil {
		return false
	}

	kind, _, err := unstructured.NestedString(allocation.Object, "spec", "consumerRef", "kind")
	if err != nil {
		return false
	}

	return group == capacity.ConsumerGroup && kind == capacity.ConsumerKind
}

func allocationForClaim(_ context.Context, object client.Object) []reconcile.Request {
	claim, ok := object.(*v1.NodeClaim)
	if !ok || !managed(claim) {
		return nil
	}

	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: capacity.AllocationName(claim.UID)}}}
}

func managed(claim *v1.NodeClaim) bool {
	reference := claim.Spec.NodeClassRef

	return reference != nil && reference.Group == nodeclass.GroupVersion.Group && reference.Kind == "MachineClass"
}

// Reconcile handles one allocation.
func (c *Controller) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	object := provider.AllocationObject()
	if err := c.Client.Get(ctx, request.NamespacedName, object); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	if !karpenterAllocation(object) {
		return reconcile.Result{}, nil
	}

	allocation, err := provider.DecodeAllocation(object)
	if err != nil {
		return reconcile.Result{}, err
	}

	// Only an allocation named from its consumer UID is one this plugin made.
	if allocation.Name != capacity.AllocationName(allocation.Spec.ConsumerRef.UID) {
		return reconcile.Result{}, nil
	}

	claim, err := c.consumer(ctx, allocation)
	if err != nil {
		return reconcile.Result{}, err
	}

	switch {
	case claim == nil:
		return c.reconcileOrphan(ctx, object, allocation)
	case !managed(claim) || claim.Spec.NodeClassRef.Name != allocation.Spec.ClassRef.Name:
		// The NodeClaim exists but does not launch from this allocation's
		// class, so the provider would refuse the allocation too. Leave both
		// alone rather than act on a NodeClaim this plugin does not serve.
		return reconcile.Result{}, nil
	case !claim.DeletionTimestamp.IsZero():
		return reconcile.Result{}, c.fence(ctx, object, allocation)
	default:
		return reconcile.Result{}, c.reconcileLive(ctx, allocation, claim)
	}
}

// consumer returns the allocation's NodeClaim, or nil when a read of the API
// server confirms that no NodeClaim with the consumer UID exists.
func (c *Controller) consumer(ctx context.Context, allocation *capacity.Allocation) (*v1.NodeClaim, error) {
	reference := allocation.Spec.ConsumerRef
	key := client.ObjectKey{Name: reference.Name}

	claim := new(v1.NodeClaim)

	err := c.Client.Get(ctx, key, claim)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get NodeClaim %s: %w", reference.Name, err)
	}

	if err == nil && claim.UID == reference.UID {
		return claim, nil
	}

	claim = new(v1.NodeClaim)
	if err := c.Reader.Get(ctx, key, claim); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("confirm NodeClaim %s: %w", reference.Name, err)
	}

	if claim.UID != reference.UID {
		return nil, nil
	}

	return claim, nil
}

// fence deletes the allocation. The allocation controller treats deletion as
// retirement: it fences the Machine and revokes the bootstrap token, and
// still releases nothing until spec.lifecycle is Released. Its finalizer
// keeps the object until then.
func (c *Controller) fence(ctx context.Context, object *unstructured.Unstructured, allocation *capacity.Allocation) error {
	if !allocation.DeletionTimestamp.IsZero() {
		return nil
	}

	if err := c.Client.Delete(ctx, object, client.Preconditions{UID: &allocation.UID}); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("fence allocation %s: %w", allocation.Name, err)
	}

	return nil
}

// reconcileLive deletes the NodeClaim of an allocation that can no longer
// become a usable Node. Karpenter then finalizes the NodeClaim as usual,
// which authorizes release of whatever was created, and replaces it.
func (c *Controller) reconcileLive(ctx context.Context, allocation *capacity.Allocation, claim *v1.NodeClaim) error {
	if allocation.Status.Snapshot == nil {
		// Nothing was launched. Create reports a failure before the snapshot.
		return nil
	}

	reason := unusable(allocation)
	if reason == "" {
		return nil
	}

	if allocation.Status.FailureReason != "" {
		// Whatever the reason, a launch from this offering just failed. Until
		// the memory of it expires the provider chooses other offerings, and
		// reports insufficient capacity when none is left, so a failure that
		// repeats (bad credentials, a missing image) replaces the NodeClaim at
		// most once per offering per expiry instead of in a tight loop.
		c.Marker.MarkUnavailable(allocation.Status.Snapshot.ClassUID, allocation.Status.Snapshot.Offering.Name)
	}

	c.Recorder.Eventf(claim, corev1.EventTypeWarning, eventAllocationFailed,
		"Deleting NodeClaim: MachineAllocation %s %s", allocation.Name, reason)
	log.FromContext(ctx).Info("deleting NodeClaim whose allocation cannot become a Node",
		"NodeClaim", claim.Name, "MachineAllocation", allocation.Name, "reason", reason)

	if err := c.Client.Delete(ctx, claim, client.Preconditions{UID: &claim.UID}); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("delete NodeClaim %s: %w", claim.Name, err)
	}

	failedAllocations.WithLabelValues(failureLabel(allocation)).Inc()

	return nil
}

// unusable returns why the allocation can no longer become a usable Node for
// a NodeClaim that is not terminating, or "" when it still can.
func unusable(allocation *capacity.Allocation) string {
	status := allocation.Status

	if status.FailureReason != "" {
		return fmt.Sprintf("failed: %s: %s", status.FailureReason, status.FailureMessage)
	}

	if !allocation.DeletionTimestamp.IsZero() {
		return "is being deleted"
	}

	if allocation.Spec.Lifecycle == capacity.Released {
		return "is released"
	}

	if fenced := apimeta.FindStatusCondition(status.Conditions, capacity.ConditionFenced); fenced != nil && fenced.Status == "True" {
		return fmt.Sprintf("is fenced: %s", fenced.Reason)
	}

	if status.Snapshot != nil && apimeta.IsStatusConditionTrue(status.Conditions, capacity.ConditionAllocated) {
		// Compare with the offering frozen in the snapshot, not with labels:
		// a zone label can be this plugin's default for a zoneless offering.
		// Providers differ in the case they report names in.
		planned := status.Snapshot.Offering

		if !strings.EqualFold(status.InstanceType, planned.InstanceType) {
			return fmt.Sprintf("launched instance type %s, not the planned %s", status.InstanceType, planned.InstanceType)
		}

		if planned.Zone != "" && !strings.EqualFold(status.Zone, planned.Zone) {
			return fmt.Sprintf("launched in zone %s, not the planned %s", status.Zone, planned.Zone)
		}
	}

	return ""
}

func failureLabel(allocation *capacity.Allocation) string {
	if allocation.Status.FailureReason != "" {
		return allocation.Status.FailureReason
	}

	fenced := apimeta.FindStatusCondition(allocation.Status.Conditions, capacity.ConditionFenced)
	if fenced != nil && fenced.Reason == reasonMachineDeleting {
		return reasonMachineDeleting
	}

	return "Other"
}

// reconcileOrphan handles an allocation whose NodeClaim is gone, so nobody
// will call Delete for it. It fences the allocation first, then authorizes
// release only when no Node exists for it or the Node runs no workload:
// without a consumer nothing has drained the Node, and this controller never
// evicts workloads.
func (c *Controller) reconcileOrphan(
	ctx context.Context,
	object *unstructured.Unstructured,
	allocation *capacity.Allocation,
) (reconcile.Result, error) {
	if allocation.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, c.fence(ctx, object, allocation)
	}

	if allocation.Spec.Lifecycle == capacity.Released {
		// Release is authorized; the allocation controller finishes it. No
		// NodeClaim is left to remove the Node of the host it deletes.
		return reconcile.Result{}, c.removeNodes(ctx, allocation)
	}

	if !apimeta.IsStatusConditionTrue(allocation.Status.Conditions, capacity.ConditionFenced) {
		// The fence revokes the bootstrap token, so a host that has not
		// joined cannot join after the Node check below.
		return reconcile.Result{RequeueAfter: orphanRecheck}, nil
	}

	nodes, err := c.nodesFor(ctx, allocation)
	if err != nil {
		return reconcile.Result{}, err
	}

	if len(nodes) == 0 {
		return reconcile.Result{}, c.releaseOrphan(ctx, object,
			"NodeClaim %s is gone and no Node is registered for this allocation; release authorized",
			allocation.Spec.ConsumerRef.Name)
	}

	// Nothing new may land on a Node that no NodeClaim accounts for. Pods are
	// counted only once the cordon has settled, so one the scheduler placed
	// just before it is seen.
	settled, err := c.cordon(ctx, nodes)
	if err != nil || !settled {
		return reconcile.Result{RequeueAfter: orphanRecheck}, err
	}

	names := nodeNames(nodes)

	workloads, err := c.workloads(ctx, nodes)
	if err != nil {
		return reconcile.Result{}, err
	}

	if len(workloads) > 0 {
		orphans.WithLabelValues("held").Inc()
		c.Recorder.Eventf(object, corev1.EventTypeWarning, eventOrphanHeld,
			"NodeClaim %s is gone and cordoned Node %s still runs %d workload Pod(s), such as %s; the host is released once they are gone, or drain and delete the Node, or set spec.lifecycle to Released",
			allocation.Spec.ConsumerRef.Name, names, len(workloads), workloads[0])

		return reconcile.Result{RequeueAfter: orphanRecheck}, nil
	}

	return reconcile.Result{}, c.releaseOrphan(ctx, object,
		"NodeClaim %s is gone and Node %s runs no workload Pods; release authorized",
		allocation.Spec.ConsumerRef.Name, names)
}

func (c *Controller) releaseOrphan(ctx context.Context, object *unstructured.Unstructured, message string, arguments ...any) error {
	if err := provider.AuthorizeRelease(ctx, c.Client, object); err != nil {
		return err
	}

	orphans.WithLabelValues("released").Inc()
	c.Recorder.Eventf(object, corev1.EventTypeNormal, eventOrphanReleased, message, arguments...)

	return nil
}

func nodeNames(nodes []*corev1.Node) string {
	names := make([]string, 0, len(nodes))
	for _, node := range nodes {
		names = append(names, node.Name)
	}

	return strings.Join(names, ", ")
}

func (c *Controller) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}

	return time.Now()
}

// cordon makes every Node unschedulable and records when. It reports whether
// all of them have been cordoned for at least orphanRecheck. A Node somebody
// else cordoned, or uncordoned again, starts its own wait here.
func (c *Controller) cordon(ctx context.Context, nodes []*corev1.Node) (bool, error) {
	now := c.now()
	settled := true

	for _, node := range nodes {
		since, err := time.Parse(time.RFC3339, node.Annotations[cordonedAtAnnotation])
		if err == nil && node.Spec.Unschedulable {
			settled = settled && !now.Before(since.Add(orphanRecheck))

			continue
		}

		settled = false
		base := node.DeepCopy()
		node.Spec.Unschedulable = true

		if node.Annotations == nil {
			node.Annotations = map[string]string{}
		}

		node.Annotations[cordonedAtAnnotation] = now.UTC().Format(time.RFC3339)

		if err := c.Client.Patch(ctx, node, client.MergeFrom(base)); client.IgnoreNotFound(err) != nil {
			return false, fmt.Errorf("cordon Node %s: %w", node.Name, err)
		}
	}

	return settled, nil
}

// workloads returns the Pods on the Nodes that releasing their host would
// interrupt, as namespace/name in a stable order. DaemonSet and static Pods
// exist only because the Node does, and a finished Pod has nothing left to
// interrupt. It reads the API server, because a Pod the cache has not seen
// yet must still block release.
func (c *Controller) workloads(ctx context.Context, nodes []*corev1.Node) ([]string, error) {
	var workloads []string

	for _, node := range nodes {
		var pods corev1.PodList
		if err := c.Reader.List(ctx, &pods, client.MatchingFields{podNodeField: node.Name}); err != nil {
			return nil, fmt.Errorf("list Pods on Node %s: %w", node.Name, err)
		}

		for index := range pods.Items {
			pod := &pods.Items[index]
			if podutil.IsOwnedByDaemonSet(pod) || podutil.IsOwnedByNode(pod) || podutil.IsTerminal(pod) {
				continue
			}

			workloads = append(workloads, pod.Namespace+"/"+pod.Name)
		}
	}

	slices.Sort(workloads)

	return workloads, nil
}

// removeNodes deletes every Node that exists for the allocation.
func (c *Controller) removeNodes(ctx context.Context, allocation *capacity.Allocation) error {
	nodes, err := c.nodesFor(ctx, allocation)
	if err != nil {
		return err
	}

	for _, node := range nodes {
		err := c.Client.Delete(ctx, node, client.Preconditions{UID: &node.UID})
		if client.IgnoreNotFound(err) != nil && !apierrors.IsConflict(err) {
			return fmt.Errorf("delete Node %s: %w", node.Name, err)
		}
	}

	return nil
}

// nodesFor returns every Node that exists for the allocation: the one its
// nodeRef names and any registered with its provider ID. All of them run on
// the allocation's host, so all of them decide whether it can be released.
// It reads the API server, because a Node the cache has not seen yet must
// still block release. The allocation's nodeRef is set once and never
// cleared, so it is only a hint: the Node it names counts only while it
// exists as the same object.
func (c *Controller) nodesFor(ctx context.Context, allocation *capacity.Allocation) ([]*corev1.Node, error) {
	providerID := capacity.ProviderIDPrefix + string(allocation.UID)

	var (
		found    []*corev1.Node
		recorded string
	)

	if reference := allocation.Status.NodeRef; reference != nil && reference.Name != "" {
		node := new(corev1.Node)

		err := c.Reader.Get(ctx, client.ObjectKey{Name: reference.Name}, node)
		if client.IgnoreNotFound(err) != nil {
			return nil, fmt.Errorf("get Node %s: %w", reference.Name, err)
		}

		if err == nil && (reference.UID == "" || node.UID == reference.UID || node.Spec.ProviderID == providerID) {
			found = append(found, node)
			recorded = node.Name
		}
	}

	var nodes corev1.NodeList
	if err := c.Reader.List(ctx, &nodes); err != nil {
		return nil, fmt.Errorf("list Nodes: %w", err)
	}

	for index := range nodes.Items {
		if node := &nodes.Items[index]; node.Spec.ProviderID == providerID && node.Name != recorded {
			found = append(found, node)
		}
	}

	return found, nil
}
