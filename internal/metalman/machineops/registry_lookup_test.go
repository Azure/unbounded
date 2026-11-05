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
	"golang.org/x/sync/singleflight"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/metalman/bootmedia"
	"github.com/Azure/unbounded/internal/metalman/netboot"
)

func TestBoundedLookupTimesOutAndKeepsRunning(t *testing.T) {
	t.Parallel()

	var (
		group   singleflight.Group
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

	var group singleflight.Group

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
