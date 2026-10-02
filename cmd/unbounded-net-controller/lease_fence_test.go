// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

const fenceTestIdentity = "controller-a"

type fakeResourceLock struct {
	resourcelock.Interface

	err error
}

func (l *fakeResourceLock) Create(context.Context, resourcelock.LeaderElectionRecord) error {
	return l.err
}

func (l *fakeResourceLock) Update(context.Context, resourcelock.LeaderElectionRecord) error {
	return l.err
}

func (l *fakeResourceLock) Identity() string {
	return fenceTestIdentity
}

func newTestLeaseFence(window time.Duration) (*leaseFence, *time.Time, *fakeResourceLock, resourcelock.Interface) {
	now := time.Unix(1000, 0)
	fence := newLeaseFence(window)
	fence.now = func() time.Time { return now }
	inner := &fakeResourceLock{}

	return fence, &now, inner, fence.wrap(inner)
}

func TestLeaseFenceNotHeldInitially(t *testing.T) {
	fence := newLeaseFence(10 * time.Second)
	if !fence.ValidUntil().IsZero() {
		t.Fatalf("ValidUntil = %v, want zero before any lease write", fence.ValidUntil())
	}
}

func TestLeaseFenceExtendsOnSuccessfulWrite(t *testing.T) {
	writes := map[string]func(resourcelock.Interface, resourcelock.LeaderElectionRecord) error{
		"create": func(l resourcelock.Interface, r resourcelock.LeaderElectionRecord) error {
			return l.Create(context.Background(), r)
		},
		"update": func(l resourcelock.Interface, r resourcelock.LeaderElectionRecord) error {
			return l.Update(context.Background(), r)
		},
	}

	for name, write := range writes {
		t.Run(name, func(t *testing.T) {
			fence, now, _, lock := newTestLeaseFence(10 * time.Second)
			start := *now

			if err := write(lock, resourcelock.LeaderElectionRecord{HolderIdentity: fenceTestIdentity}); err != nil {
				t.Fatalf("write: %v", err)
			}

			if want := start.Add(10 * time.Second); !fence.ValidUntil().Equal(want) {
				t.Fatalf("ValidUntil = %v, want %v", fence.ValidUntil(), want)
			}

			*now = now.Add(4 * time.Second)

			if err := write(lock, resourcelock.LeaderElectionRecord{HolderIdentity: fenceTestIdentity}); err != nil {
				t.Fatalf("renew: %v", err)
			}

			if want := start.Add(14 * time.Second); !fence.ValidUntil().Equal(want) {
				t.Fatalf("ValidUntil after renew = %v, want %v", fence.ValidUntil(), want)
			}
		})
	}
}

// TestLeaseFenceMeasuresFromCallStart checks that a slow write does not extend
// the fence past what the write's start time justifies.
func TestLeaseFenceMeasuresFromCallStart(t *testing.T) {
	fence := newLeaseFence(10 * time.Second)
	start := time.Unix(1000, 0)
	calls := 0
	fence.now = func() time.Time {
		calls++
		return start.Add(time.Duration(calls-1) * 7 * time.Second)
	}

	lock := fence.wrap(&fakeResourceLock{})
	if err := lock.Update(context.Background(), resourcelock.LeaderElectionRecord{HolderIdentity: fenceTestIdentity}); err != nil {
		t.Fatalf("update: %v", err)
	}

	if want := start.Add(10 * time.Second); !fence.ValidUntil().Equal(want) {
		t.Fatalf("ValidUntil = %v, want %v measured from call start", fence.ValidUntil(), want)
	}
}

func TestLeaseFenceFailedWriteDoesNotExtend(t *testing.T) {
	fence, now, inner, lock := newTestLeaseFence(10 * time.Second)
	start := *now

	if err := lock.Update(context.Background(), resourcelock.LeaderElectionRecord{HolderIdentity: fenceTestIdentity}); err != nil {
		t.Fatalf("update: %v", err)
	}

	*now = now.Add(5 * time.Second)
	inner.err = errors.New("context deadline exceeded")

	if err := lock.Update(context.Background(), resourcelock.LeaderElectionRecord{HolderIdentity: fenceTestIdentity}); !errors.Is(err, inner.err) {
		t.Fatalf("update error = %v, want %v", err, inner.err)
	}

	if want := start.Add(10 * time.Second); !fence.ValidUntil().Equal(want) {
		t.Fatalf("ValidUntil = %v, want unchanged %v after failed renewal", fence.ValidUntil(), want)
	}
}

func TestLeaseFenceClearsWhenNotHolder(t *testing.T) {
	holders := map[string]string{
		"released":     "",
		"other-holder": "controller-b",
	}

	for name, holder := range holders {
		t.Run(name, func(t *testing.T) {
			fence, _, _, lock := newTestLeaseFence(10 * time.Second)

			if err := lock.Update(context.Background(), resourcelock.LeaderElectionRecord{HolderIdentity: fenceTestIdentity}); err != nil {
				t.Fatalf("update: %v", err)
			}

			if err := lock.Update(context.Background(), resourcelock.LeaderElectionRecord{HolderIdentity: holder}); err != nil {
				t.Fatalf("update: %v", err)
			}

			if !fence.ValidUntil().IsZero() {
				t.Fatalf("ValidUntil = %v, want zero once the lease is not ours", fence.ValidUntil())
			}
		})
	}
}
