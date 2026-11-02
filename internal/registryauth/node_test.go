// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package registryauth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestRestartLeaseUsesOnlyPrecreatedLease(t *testing.T) {
	client := fake.NewClientset(&coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: RestartLeaseName, Namespace: "system"}})
	called := false
	err := withRestartLease(t.Context(), client, NodeOptions{Namespace: "system", Identity: "pod-uid"}, func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}

		lease, err := client.CoordinationV1().Leases("system").Get(ctx, RestartLeaseName, metav1.GetOptions{})
		if err != nil {
			return err
		}

		if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != "pod-uid" {
			return errors.New("restart callback ran without holding the Lease")
		}

		called = true

		return nil
	})
	require.NoError(t, err)
	require.True(t, called)

	for _, action := range client.Actions() {
		require.Contains(t, []string{"get", "update"}, action.GetVerb())
		require.Equal(t, "leases", action.GetResource().Resource)
	}
}

func TestRestartLeaseWaitsForCanceledHostOperation(t *testing.T) {
	client := fake.NewClientset(&coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: RestartLeaseName, Namespace: "system"}})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	started, cleanedUp := make(chan struct{}), make(chan struct{})
	go func() {
		<-started
		cancel()
	}()

	err := withRestartLease(ctx, client, NodeOptions{Namespace: "system", Identity: "pod-uid"}, func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		time.Sleep(10 * time.Millisecond)
		close(cleanedUp)

		return ctx.Err()
	})
	require.Error(t, err)

	select {
	case <-cleanedUp:
	default:
		t.Fatal("restart coordination returned while the canceled host operation was still running")
	}
}

func TestRestartLeaseCancellationDoesNotMutateHost(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	client := fake.NewClientset()
	called := false
	err := withRestartLease(ctx, client, NodeOptions{Namespace: "system", Identity: "pod-uid"}, func(context.Context) error { called = true; return nil })
	require.Error(t, err)
	require.False(t, called)
}
