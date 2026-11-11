// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package provider implements Karpenter over the shared capacity resources only.
package provider

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/awslabs/operatorpkg/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/Azure/unbounded/plugins/karpenter/internal/capacity"
	"github.com/Azure/unbounded/plugins/karpenter/internal/nodeclass"
)

const (
	defaultPriceCurrency  = "USD"
	defaultUnavailableTTL = 3 * time.Minute
	defaultSnapshotWait   = 3 * time.Second
	snapshotPollInterval  = 250 * time.Millisecond

	reasonAllocationPending = "AllocationPending"
)

// Options tunes the provider. The zero value of every field selects its
// default.
type Options struct {
	// PriceCurrency is the ISO 4217 currency whose offering prices are used
	// as Karpenter prices. Offerings priced in another currency, or not
	// priced, get an estimate. The default is USD.
	PriceCurrency string

	// UnavailableTTL is how long an offering whose launch failed for
	// capacity or quota is reported unavailable. The default is 3 minutes.
	UnavailableTTL time.Duration

	// SnapshotWait bounds how long Create waits for a new allocation's
	// snapshot before reporting it pending. The default is 3 seconds. A
	// negative value disables the wait.
	SnapshotWait time.Duration
}

// Provider is the Karpenter cloud provider for Unbounded capacity. It reads
// MachineClasses and reads and writes MachineAllocations, and nothing else.
type Provider struct {
	cache   client.Reader
	reader  client.Reader
	writer  client.Client
	options Options
	now     func() time.Time
	sleep   func(context.Context, time.Duration) error

	unavailableMu sync.Mutex
	unavailable   map[unavailableKey]time.Time
}

// New returns a provider that reads from cache, confirms what the cache
// does not show with reader, and writes with writer.
func New(cache, reader client.Reader, writer client.Client, options Options) (*Provider, error) {
	if cache == nil || reader == nil || writer == nil {
		return nil, fmt.Errorf("capacity cache, uncached reader, and writer are required")
	}

	if options.PriceCurrency == "" {
		options.PriceCurrency = defaultPriceCurrency
	}

	if options.UnavailableTTL == 0 {
		options.UnavailableTTL = defaultUnavailableTTL
	}

	if options.SnapshotWait == 0 {
		options.SnapshotWait = defaultSnapshotWait
	}

	return &Provider{
		cache:       cache,
		reader:      reader,
		writer:      writer,
		options:     options,
		now:         time.Now,
		sleep:       sleepContext,
		unavailable: map[unavailableKey]time.Time{},
	}, nil
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (p *Provider) Name() string {
	return "unbounded"
}

func (p *Provider) GetSupportedNodeClasses() []status.Object {
	return []status.Object{&nodeclass.MachineClass{}}
}

// RepairPolicies returns none: the shared capacity API has no
// provider-neutral source of node health conditions yet.
func (p *Provider) RepairPolicies() []cloudprovider.RepairPolicy {
	return nil
}

// Reboot is not implemented. Karpenter requests a reboot only through a
// repair policy, and RepairPolicies returns none.
func (p *Provider) Reboot(context.Context, *v1.NodeClaim, string) error {
	return cloudprovider.NewNodeRebootNotImplementedError()
}

// Create finds or creates the NodeClaim's allocation and returns the
// hydrated NodeClaim as soon as the allocation has its snapshot. It does not
// wait for the host: the snapshot already carries the provider ID, shape,
// resources and labels, and returning then makes Karpenter record the
// provider ID, so Karpenter calls Delete for every allocation that can have
// remote effects.
func (p *Provider) Create(ctx context.Context, claim *v1.NodeClaim) (*v1.NodeClaim, error) {
	if claim.UID == "" || !supportedClass(claim.Spec.NodeClassRef) {
		return nil, fmt.Errorf("NodeClaim must have a UID and reference an Unbounded MachineClass")
	}

	object, err := p.allocationForClaim(ctx, claim)
	if err != nil {
		return nil, err
	}

	if object == nil {
		object, err = p.createAllocation(ctx, claim)
		if err != nil {
			return nil, err
		}
	}

	allocation, err := p.awaitSnapshot(ctx, object)
	if err != nil {
		return nil, err
	}

	if err := ownsAllocation(claim, allocation); err != nil {
		return nil, err
	}

	if retiring(allocation) {
		return nil, fmt.Errorf("allocation %s is retiring and cannot back a NodeClaim", allocation.Name)
	}

	if allocation.Status.Snapshot != nil {
		return hydrate(allocation)
	}

	if allocation.Status.FailureReason == "" {
		return nil, cloudprovider.NewCreateError(
			fmt.Errorf("allocation %s has no snapshot yet", allocation.Name),
			reasonAllocationPending,
			"Waiting for the capacity allocation to be accepted",
		)
	}

	return nil, p.discardFailed(ctx, allocation)
}

// awaitSnapshot waits a bounded time for the allocation to get a snapshot or
// a failure, reading the API server directly, and returns the latest
// observation.
func (p *Provider) awaitSnapshot(ctx context.Context, object *unstructured.Unstructured) (*capacity.Allocation, error) {
	allocation, err := decodeAllocation(object)
	if err != nil {
		return nil, err
	}

	deadline := p.now().Add(p.options.SnapshotWait)

	if settled(allocation) || !p.now().Before(deadline) {
		return allocation, nil
	}

	for {
		if err := p.sleep(ctx, snapshotPollInterval); err != nil {
			return nil, err
		}

		latest, err := p.readAllocation(ctx, allocation.Name)
		if err != nil {
			return nil, err
		}

		if latest == nil || latest.GetUID() != allocation.UID {
			return nil, fmt.Errorf("allocation %s changed while it was pending; retry the same NodeClaim", allocation.Name)
		}

		allocation, err = decodeAllocation(latest)
		if err != nil {
			return nil, err
		}

		if settled(allocation) || !p.now().Before(deadline) {
			return allocation, nil
		}
	}
}

// settled reports whether Create has an answer for the allocation.
func settled(allocation *capacity.Allocation) bool {
	return allocation.Status.Snapshot != nil || allocation.Status.FailureReason != "" || retiring(allocation)
}

// discardFailed deletes an allocation that failed before its snapshot and
// returns the launch error for it. Such an allocation has no finalizer and
// no remote effect, and it can never succeed, so a retry of the same
// NodeClaim needs a fresh object under the same name.
func (p *Provider) discardFailed(ctx context.Context, allocation *capacity.Allocation) error {
	object := allocationObject()
	object.SetName(allocation.Name)

	if err := p.writer.Delete(ctx, object, client.Preconditions{UID: &allocation.UID}); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("remove failed allocation %s: %w", allocation.Name, err)
	}

	failure := fmt.Errorf("allocation %s failed: %s: %s",
		allocation.Name, allocation.Status.FailureReason, allocation.Status.FailureMessage)

	switch allocation.Status.FailureReason {
	case capacity.FailureInsufficientCapacity, capacity.FailureQuotaExceeded:
		return cloudprovider.NewInsufficientCapacityError(failure)
	default:
		return cloudprovider.NewCreateError(
			failure,
			allocation.Status.FailureReason,
			"Capacity allocation failed: "+allocation.Status.FailureMessage,
		)
	}
}

func (p *Provider) createAllocation(ctx context.Context, claim *v1.NodeClaim) (*unstructured.Unstructured, error) {
	class, err := p.readyClass(ctx, claim.Spec.NodeClassRef.Name)
	if err != nil {
		return nil, cloudprovider.NewNodeClassNotReadyError(err)
	}

	// The class in the cache can trail the allocation controller's view. The
	// controller decides again whether the chosen offering is launchable.
	offering, err := p.chooseOffering(claim, class)
	if err != nil {
		return nil, err
	}

	requirements := pinRequirements(offering, class.Status.Offerings)

	startupTaints := slices.Clone(claim.Spec.StartupTaints)
	if !slices.ContainsFunc(startupTaints, func(taint corev1.Taint) bool {
		return taint.MatchTaint(&v1.UnregisteredNoExecuteTaint)
	}) {
		startupTaints = append(startupTaints, v1.UnregisteredNoExecuteTaint)
	}

	allocation := capacity.Allocation{
		TypeMeta: metav1.TypeMeta{
			APIVersion: capacity.AllocationGVK.GroupVersion().String(),
			Kind:       capacity.AllocationGVK.Kind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: capacity.AllocationName(claim.UID),
		},
		Spec: capacity.AllocationSpec{
			ConsumerRef: capacity.Consumer{
				APIGroup: capacity.ConsumerGroup,
				Kind:     capacity.ConsumerKind,
				Name:     claim.Name,
				UID:      claim.UID,
			},
			ClassRef: capacity.Reference{
				Name: class.Name,
				UID:  class.UID,
			},
			Resources:    claim.Spec.Resources.Requests.DeepCopy(),
			Requirements: requirements,
			NodeTemplate: capacity.NodeTemplate{
				Labels:        maps.Clone(claim.Labels),
				Taints:        slices.Clone(claim.Spec.Taints),
				StartupTaints: startupTaints,
			},
			Lifecycle: capacity.Active,
		},
	}

	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&allocation)
	if err != nil {
		return nil, fmt.Errorf("encode allocation: %w", err)
	}

	object := &unstructured.Unstructured{Object: content}
	if err := p.writer.Create(ctx, object); err != nil {
		if apierrors.IsAlreadyExists(err) {
			existing, err := p.readAllocation(ctx, allocation.Name)
			if err != nil {
				return nil, err
			}

			if existing == nil {
				return nil, fmt.Errorf("allocation changed during creation; retry the same NodeClaim")
			}

			return existing, nil
		}

		return nil, fmt.Errorf("persist allocation: %w", err)
	}

	return object, nil
}

func (p *Provider) readyClass(ctx context.Context, name string) (*nodeclass.MachineClass, error) {
	class := new(nodeclass.MachineClass)
	if err := p.cache.Get(ctx, client.ObjectKey{Name: name}, class); err != nil {
		return nil, fmt.Errorf("get MachineClass %s: %w", name, err)
	}

	if class.UID == "" || class.Status.ResolvedRevision == "" || !class.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("MachineClass %s is unresolved or terminating", name)
	}

	if !class.StatusConditions().Get(status.ConditionReady).IsTrue() {
		return nil, fmt.Errorf("MachineClass %s is not ready for its current generation", name)
	}

	return class, nil
}

func supportedClass(reference *v1.NodeClassReference) bool {
	return reference != nil &&
		reference.Group == nodeclass.GroupVersion.Group &&
		reference.Kind == "MachineClass"
}

// ownsAllocation reports an error unless the allocation is the NodeClaim's
// own: named from its UID, consumed by exactly this NodeClaim incarnation,
// and made from the class the NodeClaim names. The rest of the allocation
// spec is immutable, so it is what this NodeClaim asked for when it was
// created.
func ownsAllocation(claim *v1.NodeClaim, allocation *capacity.Allocation) error {
	consumer := allocation.Spec.ConsumerRef
	if consumer.APIGroup != capacity.ConsumerGroup || consumer.Kind != capacity.ConsumerKind ||
		consumer.Name != claim.Name || consumer.UID != claim.UID ||
		allocation.Name != capacity.AllocationName(claim.UID) {
		return fmt.Errorf("allocation %s is not owned by this NodeClaim UID", allocation.Name)
	}

	if claim.Spec.NodeClassRef == nil || allocation.Spec.ClassRef.Name != claim.Spec.NodeClassRef.Name ||
		allocation.Spec.ClassRef.UID == "" {
		return fmt.Errorf("allocation %s is not from the NodeClaim's MachineClass", allocation.Name)
	}

	return nil
}

// retiring reports whether the allocation can no longer back a new launch.
func retiring(allocation *capacity.Allocation) bool {
	return allocation.Spec.Lifecycle == capacity.Released ||
		!allocation.DeletionTimestamp.IsZero() ||
		conditionTrue(allocation, capacity.ConditionFenced)
}

type unavailableKey struct {
	class    types.UID
	offering string
}

// MarkUnavailable records that a launch from the class's offering failed for
// capacity or quota, so the offering is reported unavailable for a while.
func (p *Provider) MarkUnavailable(class types.UID, offering string) {
	if class == "" || offering == "" {
		return
	}

	p.unavailableMu.Lock()
	defer p.unavailableMu.Unlock()

	p.unavailable[unavailableKey{class: class, offering: offering}] = p.now().Add(p.options.UnavailableTTL)
}

func (p *Provider) recentlyFailed(class types.UID, offering string) bool {
	p.unavailableMu.Lock()
	defer p.unavailableMu.Unlock()

	key := unavailableKey{class: class, offering: offering}

	until, found := p.unavailable[key]
	if !found {
		return false
	}

	if !until.After(p.now()) {
		delete(p.unavailable, key)
		return false
	}

	return true
}
