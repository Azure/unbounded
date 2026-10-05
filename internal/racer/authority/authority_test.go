// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authority

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestAuthorityOperationsSharePrivateGate(t *testing.T) {
	f := newServingFixture(t)
	a := f.a.authority

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	_, err := a.PublishTopology(ctx, func(ctx context.Context) (TopologyObservation, error) {
		blocked, stop := context.WithCancel(ctx)
		stop()

		_, err := a.ReconcileCredentials(blocked)
		require.ErrorIs(t, err, context.Canceled)
		require.ErrorIs(t, a.Observe(blocked), context.Canceled)
		_, err = a.TrustPool()
		require.NoError(t, err, "failed admission must not invalidate trust")

		select {
		case <-a.gate.token:
			t.Fatal("discovery callback escaped operation gate")
		default:
		}

		return f.a.Topology.observeTopology(ctx)
	})
	require.NoError(t, err)
	require.NoError(t, a.Observe(ctx), "returned hints must not retain gate")
}

func TestAuthorityPublicationHistoryIsOperationOwned(t *testing.T) {
	f := newServingFixture(t)
	a, r := f.a.authority, f.a.Topology
	before := cloneAccepted(a.accepted)
	member := r.Accepted[testNodeUID]
	member.Shares = 999
	r.Accepted[testNodeUID] = member

	require.Equal(t, before, a.accepted, "legacy view aliases authority history")

	update, err := a.PublishTopology(t.Context(), r.observeTopology)
	require.NoError(t, err)
	delete(update.Members, testNodeUID)
	require.Equal(t, before, a.accepted, "annotation hints alias authority history")

	var node corev1.Node
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Name: "worker"}, &node))
	node.Annotations[wire.SharesAnnotation] = "7"
	require.NoError(t, r.Update(t.Context(), &node))
	base := r.Client.(client.WithWatch)
	r.Client = interceptor.NewClient(base, interceptor.Funcs{Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
		return apierrors.NewConflict(corev1.Resource("configmaps"), "version", wire.Conflict)
	}})
	a.publisher.Writer = r.Client
	_, err = a.PublishTopology(t.Context(), r.observeTopology)
	require.True(t, apierrors.IsConflict(err))
	require.Equal(t, before, a.accepted, "failed CAS advanced history")

	r.Client = base
	a.publisher.Writer = base
	_, err = a.PublishTopology(t.Context(), r.observeTopology)
	require.NoError(t, err)
	require.EqualValues(t, 7, a.accepted[testNodeUID].Shares)
}

func TestAuthorityHandlesRetainRevocationSemantics(t *testing.T) {
	f := newServingFixture(t)
	a := f.a.authority
	image, err := a.Current()
	require.NoError(t, err)
	write, stop, err := image.WriteContext(t.Context())
	require.NoError(t, err)

	defer stop()

	trust, stopTrust, err := a.TrustContext(t.Context())
	require.NoError(t, err)

	defer stopTrust()

	deadline, _ := trust.Deadline()

	require.NoError(t, a.Observe(t.Context()))

	nextDeadline, _ := trust.Deadline()
	require.Equal(t, deadline, nextDeadline)
	require.NoError(t, trust.Err())

	var node corev1.Node
	require.NoError(t, f.a.Topology.Get(t.Context(), client.ObjectKey{Name: "worker"}, &node))
	node.Annotations[wire.SharesAnnotation] = "7"
	require.NoError(t, f.a.Topology.Update(t.Context(), &node))
	_, err = a.PublishTopology(t.Context(), f.a.Topology.observeTopology)
	require.NoError(t, err)
	require.ErrorIs(t, write.Err(), context.Canceled, "replacement must synchronously revoke old image")
	require.NoError(t, trust.Err(), "publication replacement is not trust invalidation")

	secret := &corev1.Secret{}
	require.NoError(t, f.a.Topology.Get(t.Context(), client.ObjectKey{Namespace: a.config.Namespace, Name: a.config.CredentialsSecretName}, secret))
	require.NoError(t, f.a.Topology.Delete(t.Context(), secret))
	require.Error(t, a.Observe(t.Context()))
	require.ErrorIs(t, trust.Err(), context.Canceled)
}

func TestAuthorityObservationFailureDoesNotPublish(t *testing.T) {
	r := initializedTopology(t)
	a := r.authority
	boom := errors.New("discovery failed")
	_, err := a.PublishTopology(t.Context(), func(context.Context) (TopologyObservation, error) { return TopologyObservation{}, boom })
	require.ErrorIs(t, err, boom)
	_, err = a.Current()
	require.ErrorIs(t, err, wire.Unavailable)
	require.Empty(t, a.accepted)
	_, err = a.PublishTopology(t.Context(), r.observeTopology)
	require.NoError(t, err, "failed callback did not release gate")
}

func TestAuthorityBlockedOperationsHonorCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newServingFixture(t)
		a := f.a.authority

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		results := make(chan error, 4)
		_, err := a.PublishTopology(t.Context(), func(ctx context.Context) (TopologyObservation, error) {
			blocked, cancelBlocked := context.WithCancel(ctx)
			defer cancelBlocked()

			go func() { _, err := a.ReconcileCredentials(blocked); results <- err }()
			go func() { results <- a.Observe(blocked) }()
			go func() {
				_, err := a.PublishTopology(blocked, func(context.Context) (TopologyObservation, error) {
					t.Error("blocked publication entered discovery")
					return TopologyObservation{}, nil
				})
				results <- err
			}()
			go func() {
				_, err := a.Issue(blocked, NodeIdentity{owner: a, bearer: true, cluster: a.config.Cluster, node: testNodeUID, expires: time.Now().Add(time.Hour)}, f.request)
				results <- err
			}()

			synctest.Wait()

			select {
			case err := <-results:
				t.Fatalf("operation bypassed held gate: %v", err)
			default:
			}

			cancelBlocked()

			for range 4 {
				require.ErrorIs(t, <-results, context.Canceled)
			}

			return f.a.Topology.observeTopology(ctx)
		})
		require.NoError(t, err)
		require.NoError(t, a.TrustReady(), "canceled waiters invalidated accepted trust")
		require.NoError(t, a.Observe(ctx))
	})
}

func TestAuthorityConstructorCopiesConfigWithoutIO(t *testing.T) {
	cfg := testConfig(t)
	a := New(cfg, Dependencies{})
	cfg.Cluster = ""
	require.NotEqual(t, cfg.Cluster, a.config.Cluster)
	require.ErrorIs(t, a.PublicationReady(), wire.Unavailable)
	require.ErrorIs(t, a.TrustReady(), wire.Unavailable)
	require.Empty(t, a.accepted)
}
