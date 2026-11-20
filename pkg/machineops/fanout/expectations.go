// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package fanout

import (
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// expectationTTL bounds how long a write of this controller is remembered
// when the cache never shows it, for example because the child was deleted
// straight away.
const expectationTTL = time.Minute

// expectations remembers what this process wrote and the cache has not shown
// yet. A reconcile often starts before the cache has caught up with the one
// before it: a child just created is not listed, and a child just released
// still looks held. Acting on that view would create the child again, or
// count too few running children and release or start more than the pace
// allows. What is remembered here is overlaid on what the cache returns.
//
// It is only ever a correction for this process's own recent writes. After a
// restart there is nothing to remember and nothing needed: the cache is
// filled before the first reconcile, later than anything an earlier process
// wrote.
type expectations struct {
	mu      sync.Mutex
	parents map[types.UID]*parentExpectations
}

type parentExpectations struct {
	// created holds children this process created, by name, until the cache
	// lists them.
	created map[string]expectedChild
	// released holds when this process released a child, by name, until the
	// cache shows it released.
	released map[string]time.Time
	// picked is the most children seen taken by an executor, and pickedAt
	// when that number last grew. See reconciler.failExpiredChildren.
	picked   int
	pickedAt time.Time
}

type expectedChild struct {
	child *unboundedv1alpha3.MachineOperation
	at    time.Time
}

func (e *expectations) parent(uid types.UID, now time.Time) *parentExpectations {
	if e.parents == nil {
		e.parents = map[types.UID]*parentExpectations{}
	}

	p, ok := e.parents[uid]
	if !ok {
		p = &parentExpectations{
			created:  map[string]expectedChild{},
			released: map[string]time.Time{},
			pickedAt: now,
		}
		e.parents[uid] = p
	}

	return p
}

// forget drops everything remembered for a parent that finished or is gone.
func (e *expectations) forget(uid types.UID) {
	e.mu.Lock()
	defer e.mu.Unlock()

	delete(e.parents, uid)
}

func (e *expectations) recordCreated(uid types.UID, children []*unboundedv1alpha3.MachineOperation, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()

	p := e.parent(uid, now)
	for _, child := range children {
		p.created[child.Name] = expectedChild{child: child, at: now}
	}
}

func (e *expectations) recordReleased(uid types.UID, name string, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.parent(uid, now).released[name] = now
}

// overlay returns the listed children corrected by what this process wrote
// and the cache has not shown: children it created are added, and children it
// released are shown released. The listed objects belong to the cache and are
// not changed; a corrected child is a copy.
func (e *expectations) overlay(
	uid types.UID,
	listed []*unboundedv1alpha3.MachineOperation,
	now time.Time,
) []*unboundedv1alpha3.MachineOperation {
	e.mu.Lock()
	defer e.mu.Unlock()

	p := e.parent(uid, now)
	if len(p.created) == 0 && len(p.released) == 0 {
		return listed
	}

	seen := make(map[string]struct{}, len(listed))
	for _, child := range listed {
		seen[child.Name] = struct{}{}
	}

	for name, expected := range p.created {
		if _, ok := seen[name]; ok || now.Sub(expected.at) > expectationTTL {
			delete(p.created, name)
			continue
		}

		listed = append(listed, expected.child)
	}

	for i, child := range listed {
		releasedAt, ok := p.released[child.Name]
		if !ok {
			continue
		}

		if !child.Spec.Suspend || now.Sub(releasedAt) > expectationTTL {
			delete(p.released, child.Name)
			continue
		}

		released := child.DeepCopy()
		released.Spec.Suspend = false

		if released.Annotations == nil {
			released.Annotations = map[string]string{}
		}

		released.Annotations[unboundedv1alpha3.MachineOperationReleasedAtAnnotation] = releasedAt.UTC().Format(time.RFC3339)
		listed[i] = released
	}

	return listed
}

// pickupProgress records how many children executors have taken so far and
// returns when that number last grew.
func (e *expectations) pickupProgress(uid types.UID, picked int, now time.Time) time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()

	p := e.parent(uid, now)
	if picked > p.picked {
		p.picked = picked
		p.pickedAt = now
	}

	return p.pickedAt
}
