// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package fanout

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"sort"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

const (
	maxChildNameLength = 253
	// Ten hex digits, 40 bits. When a long parent name leaves no room for the
	// machine name, the suffix alone tells 30,000 children apart.
	childNameHashSize = 10
)

func rolloutLimit(rollout *unboundedv1alpha3.MachineOperationRollout, targets int) int {
	if rollout == nil || rollout.MaxConcurrent == nil {
		return 1
	}

	limit, err := intstr.GetScaledValueFromIntOrPercent(rollout.MaxConcurrent, targets, true)
	if err != nil || limit < 1 {
		return 1
	}

	return limit
}

func continueOnFailure(rollout *unboundedv1alpha3.MachineOperationRollout) bool {
	return rollout != nil && rollout.OnFailure == unboundedv1alpha3.RolloutFailurePolicyContinue
}

// failureLimit is how many failed children stop the rollout: the first one
// under Stop, and spec.rollout.maxFailures under Continue. The limit under
// Continue is never above MaxRolloutFailures, which is what bounds the failed
// children one operation leaves behind.
func failureLimit(rollout *unboundedv1alpha3.MachineOperationRollout) int {
	if !continueOnFailure(rollout) {
		return 1
	}

	if rollout.MaxFailures != nil && *rollout.MaxFailures >= 1 {
		return min(int(*rollout.MaxFailures), unboundedv1alpha3.MaxRolloutFailures)
	}

	return unboundedv1alpha3.MaxRolloutFailures
}

// childName names the child of parent for one machine:
// <parent>-<machine>-<suffix>. The suffix is derived from the parent's UID
// and the machine name, so the name is stable for one parent and machine and
// differs from every other pair. Without it "reboot" on "worker-01" would
// take the name of an operation someone called "reboot-worker-01", and a
// parent created again under an old name would find the children of the old
// one still being garbage-collected. The readable part is cut to fit.
func childName(parent *unboundedv1alpha3.MachineOperation, machineName string) string {
	sum := sha256.Sum256([]byte(string(parent.UID) + "/" + machineName))
	suffix := hex.EncodeToString(sum[:])[:childNameHashSize]

	readable := parent.Name + "-" + machineName
	if limit := maxChildNameLength - childNameHashSize - 1; len(readable) > limit {
		// A name segment may not end in "-" or ".".
		readable = strings.TrimRight(readable[:limit], "-.")
	}

	return readable + "-" + suffix
}

type missingChild struct {
	name  string
	index int
}

func missingMachines(desired []string, children []*unboundedv1alpha3.MachineOperation) []missingChild {
	present := make(map[string]struct{}, len(children))
	for _, child := range children {
		if child.Spec.MachineRef != "" {
			present[child.Spec.MachineRef] = struct{}{}
		}
	}

	missing := make([]missingChild, 0, len(desired))

	for index, name := range desired {
		if _, exists := present[name]; exists {
			continue
		}

		missing = append(missing, missingChild{name: name, index: index})
	}

	return missing
}

// createChildren creates one child per missing machine. The first budget of
// them, in name order, are created released and the rest held. The budget is
// what the pace still allows, not a position in the list of machines: a
// Machine that appears while children are being created can sort before the
// ones already running, and must not start beyond the pace.
func (r *reconciler) createChildren(
	ctx context.Context,
	parent *unboundedv1alpha3.MachineOperation,
	batch []missingChild,
	budget int,
) ([]*unboundedv1alpha3.MachineOperation, error) {
	created := make([]*unboundedv1alpha3.MachineOperation, len(batch))

	var group errgroup.Group

	group.SetLimit(createConcurrency)

	for i, target := range batch {
		i, target := i, target

		group.Go(func() error {
			child, err := r.createChild(ctx, parent, target.name, i >= budget)
			if err != nil {
				return err
			}

			created[i] = child

			return nil
		})
	}

	if err := group.Wait(); err != nil {
		return nil, err
	}

	return created, nil
}

func (r *reconciler) createChild(
	ctx context.Context,
	parent *unboundedv1alpha3.MachineOperation,
	machineRef string,
	suspend bool,
) (*unboundedv1alpha3.MachineOperation, error) {
	name := childName(parent, machineRef)
	child := &unboundedv1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				unboundedv1alpha3.MachineOperationParentUIDLabel: string(parent.UID),
			},
			OwnerReferences: []metav1.OwnerReference{childOwnerReference(parent)},
		},
		Spec: unboundedv1alpha3.MachineOperationSpec{
			MachineRef:    machineRef,
			Suspend:       suspend,
			OperationKind: parent.Spec.OperationKind,
			Parameters:    maps.Clone(parent.Spec.Parameters),
		},
	}

	if !suspend {
		// Created released: it joins its machine's queue, and its pickup
		// timeout starts, now.
		child.Annotations = map[string]string{
			unboundedv1alpha3.MachineOperationReleasedAtAnnotation: r.now().UTC().Format(time.RFC3339),
		}
	}

	if err := r.Writer.Create(ctx, child); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("create child MachineOperation %q: %w", name, err)
		}

		var existing unboundedv1alpha3.MachineOperation
		if err := r.apiReader().Get(ctx, client.ObjectKey{Name: name}, &existing); err != nil {
			return nil, fmt.Errorf("get existing MachineOperation %q: %w", name, err)
		}

		if metav1.IsControlledBy(&existing, parent) && existing.Spec.MachineRef == machineRef {
			return &existing, nil
		}

		return nil, fmt.Errorf("child name %q is taken by an operation that is not this target's child", name)
	}

	return child, nil
}

// releaseChildren releases up to count held children, the ones with the
// lowest machine names.
func (r *reconciler) releaseChildren(
	ctx context.Context,
	parent *unboundedv1alpha3.MachineOperation,
	children []*unboundedv1alpha3.MachineOperation,
	count int,
) error {
	if count <= 0 {
		return nil
	}

	held := make([]int, 0, len(children))

	for i, child := range children {
		if child.IsHeld() {
			held = append(held, i)
		}
	}

	sort.Slice(held, func(i, j int) bool {
		left := children[held[i]]
		right := children[held[j]]

		if left.Spec.MachineRef != right.Spec.MachineRef {
			return left.Spec.MachineRef < right.Spec.MachineRef
		}

		return left.Name < right.Name
	})

	now := r.now()
	releasedAt := now.UTC().Format(time.RFC3339)

	for _, index := range held[:min(count, len(held))] {
		updated, err := r.releaseChild(ctx, children[index], releasedAt)
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}

			return err
		}

		r.expect.recordReleased(parent.UID, updated.Name, now)
		children[index] = updated
	}

	return nil
}

func (r *reconciler) releaseChild(
	ctx context.Context,
	child *unboundedv1alpha3.MachineOperation,
	releasedAt string,
) (*unboundedv1alpha3.MachineOperation, error) {
	updated := child.DeepCopy()
	updated.Spec.Suspend = false

	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}

	updated.Annotations[unboundedv1alpha3.MachineOperationReleasedAtAnnotation] = releasedAt

	if err := r.Writer.Patch(ctx, updated, client.MergeFrom(child)); err != nil {
		return nil, fmt.Errorf("release child MachineOperation %q: %w", child.Name, err)
	}

	return updated, nil
}

// childOwnerReference makes parent the controller of a child, so deleting the
// parent garbage-collects the child. It does not block the parent's deletion:
// that would need the right to set finalizers on MachineOperations, which is
// what identifies a controller allowed to write provider state.
func childOwnerReference(parent *unboundedv1alpha3.MachineOperation) metav1.OwnerReference {
	reference := *metav1.NewControllerRef(parent, unboundedv1alpha3.GroupVersion.WithKind("MachineOperation"))
	reference.BlockOwnerDeletion = new(false)

	return reference
}
