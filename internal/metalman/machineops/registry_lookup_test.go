// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/metalman/bootmedia"
	"github.com/Azure/unbounded/internal/metalman/netboot"
)

func TestBoundedLookupTimesOutAndKeepsRunning(t *testing.T) {
	t.Parallel()

	var (
		group   lookupGroup
		calls   atomic.Int32
		release = make(chan struct{})
	)

	fn := func(ctx context.Context) (string, error) {
		calls.Add(1)
		<-release

		// The detached call must not see the caller's cancellation.
		return "done", ctx.Err()
	}

	ctx, cancel := context.WithCancel(t.Context())

	start := time.Now()
	_, err := boundedLookup(ctx, &group, "k", 20*time.Millisecond, fn)
	require.ErrorIs(t, err, netboot.ErrImageUnavailable)
	require.Less(t, time.Since(start), 2*time.Second)
	cancel()

	// A second caller joins the running call instead of starting another.
	_, err = boundedLookup(t.Context(), &group, "k", 20*time.Millisecond, fn)
	require.ErrorIs(t, err, netboot.ErrImageUnavailable)

	close(release)

	got, err := boundedLookup(t.Context(), &group, "k", time.Minute, fn)
	require.NoError(t, err)
	require.Equal(t, "done", got)
	require.LessOrEqual(t, calls.Load(), int32(2))
}

func TestBoundedLookupReturnsErrors(t *testing.T) {
	t.Parallel()

	var group lookupGroup

	boom := errors.New("boom")
	_, err := boundedLookup(t.Context(), &group, "k", time.Minute, func(context.Context) (int, error) { return 0, boom })
	require.ErrorIs(t, err, boom)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = boundedLookup(ctx, &group, "k2", time.Minute, func(context.Context) (int, error) {
		time.Sleep(50 * time.Millisecond)
		return 1, nil
	})
	require.ErrorIs(t, err, context.Canceled)
}

func TestHTTPBootConfigStalledRegistryDoesNotHoldReconcile(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	r := &Reconciler{ReconcileLookupTimeout: 20 * time.Millisecond}
	r.HTTPBootURL = func(context.Context, *v1alpha3.Machine) (string, error) {
		<-release
		return "http://10.0.0.10/boot.efi", nil
	}

	machine := testBareMetalMachine("machine-1", "rack-a")
	machine.Spec.PXE.BootProtocol = v1alpha3.PXEBootProtocolHTTP
	machine.Spec.PXE.DHCPLeases = []v1alpha3.DHCPLease{httpBootLease()}

	start := time.Now()
	_, _, err := r.httpBootConfig(t.Context(), machine)
	require.ErrorIs(t, err, netboot.ErrImageUnavailable)
	require.Less(t, time.Since(start), 2*time.Second)

	close(release)

	r.ReconcileLookupTimeout = time.Minute
	url, _, err := r.httpBootConfig(t.Context(), machine)
	require.NoError(t, err)
	require.Equal(t, "http://10.0.0.10/boot.efi", url)
}

type stalledMedia struct {
	*mediaTestDriver
	release chan struct{}
}

func (s *stalledMedia) Prepare(ctx context.Context, m *v1alpha3.Machine, op *v1alpha3.MachineOperation, id string) (bootmedia.Source, error) {
	<-s.release
	return s.mediaTestDriver.Prepare(ctx, m, op, id)
}

func TestVirtualMediaStalledPrepareDoesNotHoldReconcile(t *testing.T) {
	r, d, _, op := mediaFixture(t)
	media := &stalledMedia{mediaTestDriver: d, release: make(chan struct{})}
	r.BootMedia = media
	r.ReconcileLookupTimeout = 20 * time.Millisecond

	current := mediaStep(t, r, op.Name)
	require.Equal(t, "Preparing", current.Status.Targets[0].BootMedia.Phase)

	start := time.Now()
	current = mediaStep(t, r, op.Name)
	require.Less(t, time.Since(start), 2*time.Second)
	require.Equal(t, "Preparing", current.Status.Targets[0].BootMedia.Phase)
	require.Contains(t, current.Status.Targets[0].Message, "still in progress")

	close(media.release)

	r.ReconcileLookupTimeout = time.Minute
	current = mediaStep(t, r, op.Name)
	require.Equal(t, "Staging", current.Status.Targets[0].BootMedia.Phase)
}

func TestBoundedLookupRetainsResultFinishedBetweenCalls(t *testing.T) {
	t.Parallel()

	var (
		group   lookupGroup
		calls   atomic.Int32
		release = make(chan struct{})
		done    = make(chan struct{})
	)

	fn := func(context.Context) (string, error) {
		calls.Add(1)
		<-release

		return "iso", nil
	}

	_, err := boundedLookup(t.Context(), &group, "k", 10*time.Millisecond, fn)
	require.ErrorIs(t, err, netboot.ErrImageUnavailable)

	// Let the flight finish while no caller is waiting.
	go func() {
		defer close(done)

		for {
			if _, ok := retained(&group, "k"); ok {
				return
			}

			time.Sleep(time.Millisecond)
		}
	}()

	close(release)
	<-done

	// The next caller consumes the result even with a tiny timeout.
	got, err := boundedLookup(t.Context(), &group, "k", time.Nanosecond, fn)
	require.NoError(t, err)
	require.Equal(t, "iso", got)
	require.Equal(t, int32(1), calls.Load())

	_, ok := retained(&group, "k")
	require.False(t, ok, "a consumed result must not be retained")
}

func TestLookupGroupDropsRetainedResults(t *testing.T) {
	t.Parallel()

	now := time.Unix(1000, 0)
	group := lookupGroup{now: func() time.Time { return now }}

	group.store("forgotten", "a")
	group.forget("forgotten")
	_, ok := group.take("forgotten")
	require.False(t, ok)

	group.store("expired", "b")

	now = now.Add(retainedLookupTTL)
	_, ok = group.take("expired")
	require.False(t, ok)

	// Storing prunes other expired entries so abandoned keys cannot leak.
	group.store("stale", "c")

	now = now.Add(retainedLookupTTL)

	group.store("fresh", "d")
	_, ok = retained(&group, "stale")
	require.False(t, ok)

	v, ok := group.take("fresh")
	require.True(t, ok)
	require.Equal(t, "d", v)
}

func retained(g *lookupGroup, key string) (any, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	v, ok := g.done[key]

	return v.val, ok
}

type countingMedia struct {
	*mediaTestDriver
	release  chan struct{}
	prepares atomic.Int32
}

func (c *countingMedia) Prepare(ctx context.Context, m *v1alpha3.Machine, op *v1alpha3.MachineOperation, id string) (bootmedia.Source, error) {
	c.prepares.Add(1)
	<-c.release

	return c.mediaTestDriver.Prepare(ctx, m, op, id)
}

func TestVirtualMediaConsumesPrepareFinishedBetweenReconciles(t *testing.T) {
	r, d, _, op := mediaFixture(t)
	media := &countingMedia{mediaTestDriver: d, release: make(chan struct{})}
	r.BootMedia = media
	r.ReconcileLookupTimeout = 20 * time.Millisecond

	current := mediaStep(t, r, op.Name)
	require.Equal(t, "Preparing", current.Status.Targets[0].BootMedia.Phase)

	current = mediaStep(t, r, op.Name)
	require.Equal(t, "Preparing", current.Status.Targets[0].BootMedia.Phase)

	key := mediaLookupKey(current.Status.Targets[0].BootMedia.ID)

	close(media.release)
	require.Eventually(t, func() bool {
		_, ok := retained(&r.lookups, key)
		return ok
	}, 5*time.Second, time.Millisecond)

	// The retained result advances the target without a second Prepare,
	// even with a timeout too short for any new registry work.
	r.ReconcileLookupTimeout = time.Nanosecond
	current = mediaStep(t, r, op.Name)
	require.Equal(t, "Staging", current.Status.Targets[0].BootMedia.Phase)
	require.Equal(t, int32(1), media.prepares.Load())

	_, ok := retained(&r.lookups, key)
	require.False(t, ok)
}

func TestVirtualMediaReleaseDropsRetainedPrepare(t *testing.T) {
	r, _, _, op := mediaFixture(t)
	for range 3 {
		mediaStep(t, r, op.Name)
	}

	var current v1alpha3.MachineOperation
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(op), &current))
	key := mediaLookupKey(current.Status.Targets[0].BootMedia.ID)
	current.Status.Targets[0].BootMedia.ImageWritten = true
	require.NoError(t, r.Status().Update(t.Context(), &current))
	mediaStep(t, r, op.Name)
	mediaStep(t, r, op.Name)
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(op), &current))
	current.Status.Targets[0].BootMedia.RebootRequestedAt = ptrTo(fixedNow())
	require.NoError(t, r.Status().Update(t.Context(), &current))

	// A late detached Prepare left a result behind.
	r.lookups.store(key, bootmedia.Source{Name: "late"})
	mediaStep(t, r, op.Name)
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(op), &current))
	require.True(t, current.Status.Targets[0].BootMedia.Released)

	_, ok := retained(&r.lookups, key)
	require.False(t, ok)
}
