// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package apiserver

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/endpoints/request"
	clienttesting "k8s.io/client-go/testing"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/Azure/unbounded/internal/rebound/ledger"
)

// requireCooldown checks that err is a handoff cooldown conflict with the
// given retryAfterSeconds hint.
func requireCooldown(t *testing.T, err error, retryAfter int32) {
	t.Helper()

	var status apierrors.APIStatus

	require.True(t, apierrors.IsConflict(err), "%v", err)
	require.ErrorAs(t, err, &status)
	require.Equal(t, retryAfter, status.Status().Details.RetryAfterSeconds, "%v", err)
}

// ledgerReleases returns the release records in the stored ledger.
func ledgerReleases(t *testing.T, s *storage) []*ledger.Release {
	t.Helper()

	return ledger.Parse(cached(t, s).Data).Releases()
}

// TestCreateAfterDelayedRemoval is a regression test for a removal write
// that commits long after the API server stamped it. The evictor can read
// the old grant until the write commits, so the cooldown must count from the
// commit, not from ReleasedAt.
func TestCreateAfterDelayedRemoval(t *testing.T) {
	s, client, ctx := fixture(t)
	c, ok := s.clock.(*clocktesting.FakeClock)
	require.True(t, ok)

	created(t, s, ctx, "first", "node-a")

	stamped := c.Now()
	delayed := false

	client.PrependReactor("update", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		if !delayed {
			delayed = true
			// The removal write takes longer than the whole cooldown.
			c.Step(ledger.HandoffCooldown + 2*time.Second)
		}

		return false, nil, nil
	})

	_, _, err := s.Delete(ctx, "first", nil, nil)
	require.NoError(t, err)
	require.True(t, delayed)

	releases := ledgerReleases(t, s)
	require.Len(t, releases, 1)
	require.Equal(t, stamped, releases[0].ReleasedAt)
	require.Equal(t, c.Now(), releases[0].ConfirmedAt, "confirmed after the removal committed")

	other := request.WithNamespace(ctx, "other")

	// A cooldown counted from ReleasedAt would have ended already.
	_, err = s.Create(other, grant("next", "node-a"), nil, nil)
	requireCooldown(t, err, int32(ledger.HandoffCooldown/time.Second))

	c.Step(ledger.HandoffCooldown - time.Second)

	_, err = s.Create(other, grant("next", "node-a"), nil, nil)
	requireCooldown(t, err, 1)

	c.Step(time.Second)
	created(t, s, other, "next", "node-a")
}

// TestCreateConfirmsUnconfirmedRelease covers a Delete whose follow-up
// confirmation write fails. The Delete still succeeds, the node stays fenced,
// and the next Create confirms the release so the node is not fenced forever.
func TestCreateConfirmsUnconfirmedRelease(t *testing.T) {
	s, client, ctx := fixture(t)
	c, ok := s.clock.(*clocktesting.FakeClock)
	require.True(t, ok)

	created(t, s, ctx, "first", "node-a")

	updates := 0

	client.PrependReactor("update", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		updates++
		if updates == 2 {
			return true, nil, apierrors.NewServiceUnavailable("ledger unavailable")
		}

		return false, nil, nil
	})

	_, _, err := s.Delete(ctx, "first", nil, nil)
	require.NoError(t, err, "a failed confirmation must not fail the delete")
	require.Equal(t, 2, updates)

	releases := ledgerReleases(t, s)
	require.Len(t, releases, 1)
	require.True(t, releases[0].ConfirmedAt.IsZero())

	// Long after the removal, even past its retention, the unconfirmed
	// release still fences the node.
	c.Step(time.Hour)

	other := request.WithNamespace(ctx, "other")
	before := cached(t, s).Data

	// A dry run is rejected without writing the confirmation.
	_, err = s.Create(other, grant("next", "node-a"), nil, &metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
	requireCooldown(t, err, int32(ledger.HandoffCooldown/time.Second))
	require.Equal(t, before, cached(t, s).Data)
	require.Equal(t, 2, updates)

	confirmedAt := c.Now()

	_, err = s.Create(other, grant("next", "node-a"), nil, nil)
	requireCooldown(t, err, int32(ledger.HandoffCooldown/time.Second))

	releases = ledgerReleases(t, s)
	require.Len(t, releases, 1)
	require.Equal(t, confirmedAt, releases[0].ConfirmedAt)

	_, ok = ledger.Parse(cached(t, s).Data).NodeGrant("node-a")
	require.False(t, ok, "the rejected create must not grant the node")

	c.Step(ledger.HandoffCooldown - time.Second)

	_, err = s.Create(other, grant("next", "node-a"), nil, nil)
	requireCooldown(t, err, 1)

	c.Step(time.Second)
	created(t, s, other, "next", "node-a")
}

// TestCreateConfirmationWriteFails checks that Create reports the write error
// if it cannot store the confirmation, and leaves the release unconfirmed.
func TestCreateConfirmationWriteFails(t *testing.T) {
	s, client, ctx := fixture(t)

	created(t, s, ctx, "first", "node-a")

	// Fail every write after the removal, including the follow-up
	// confirmation.
	removed := false

	client.PrependReactor("update", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if removed {
			return true, nil, apierrors.NewServiceUnavailable("ledger unavailable")
		}

		update, ok := action.(clienttesting.UpdateAction)
		require.True(t, ok)

		cm, ok := update.GetObject().(*corev1.ConfigMap)
		require.True(t, ok)

		_, granted := cm.Data["node-a"]
		removed = !granted

		return false, nil, nil
	})

	_, _, err := s.Delete(ctx, "first", nil, nil)
	require.NoError(t, err)

	_, err = s.Create(request.WithNamespace(ctx, "other"), grant("next", "node-a"), nil, nil)
	require.True(t, apierrors.IsServiceUnavailable(err), "%v", err)

	releases := ledgerReleases(t, s)
	require.Len(t, releases, 1)
	require.True(t, releases[0].ConfirmedAt.IsZero())
}

// TestCreateAfterExpiredGrant checks that a grant that expired fences its
// nodes for HandoffCooldown from its expiry, with no extra wait for a
// confirmation write.
func TestCreateAfterExpiredGrant(t *testing.T) {
	s, _, ctx := fixture(t)
	c, ok := s.clock.(*clocktesting.FakeClock)
	require.True(t, ok)

	g := created(t, s, ctx, "first", "node-a")
	expiresAt := g.Status.ExpiresAt.Time
	other := request.WithNamespace(ctx, "other")

	c.SetTime(expiresAt.Add(ledger.HandoffCooldown - time.Second))

	// Create reaps the expired grant itself. Its release is confirmed at
	// expiry, so the rejection writes nothing and the wait ends on time.
	_, err := s.Create(other, grant("next", "node-a"), nil, nil)
	requireCooldown(t, err, 1)
	require.Empty(t, ledgerReleases(t, s))

	c.SetTime(expiresAt.Add(ledger.HandoffCooldown))
	created(t, s, other, "next", "node-a")
}

// TestDeleteCollectionConfirmsReleases checks that DeleteCollection confirms
// the releases it writes, and that a dry run writes nothing.
func TestDeleteCollectionConfirmsReleases(t *testing.T) {
	s, _, ctx := fixture(t)

	created(t, s, ctx, "first", "node-a")
	created(t, s, ctx, "second", "node-b")

	before := cached(t, s).Data

	_, err := s.DeleteCollection(ctx, nil, &metav1.DeleteOptions{DryRun: []string{metav1.DryRunAll}}, nil)
	require.NoError(t, err)
	require.Equal(t, before, cached(t, s).Data)

	_, err = s.DeleteCollection(ctx, nil, nil, nil)
	require.NoError(t, err)

	releases := ledgerReleases(t, s)
	require.Len(t, releases, 2)

	for _, r := range releases {
		require.Equal(t, s.clock.Now(), r.ConfirmedAt)
	}
}
