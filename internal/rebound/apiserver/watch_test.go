// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package apiserver

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternal "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/watch"
	apistorage "k8s.io/apiserver/pkg/storage"

	rebound "github.com/Azure/unbounded/api/rebound/v1alpha1"
	"github.com/Azure/unbounded/internal/rebound/ledger"
)

func TestWatchFilteredBatchResume(t *testing.T) {
	for _, replay := range []bool{false, true} {
		t.Run(fmt.Sprint("replay=", replay), func(t *testing.T) {
			s, _, ctx := fixture(t)
			created(t, s, ctx, "a", "node-a")

			options := &metainternal.ListOptions{
				ResourceVersion: "2", LabelSelector: labels.SelectorFromSet(labels.Set{"team": "blue"}),
			}
			filtered, err := s.Watch(ctx, options)
			require.NoError(t, err)

			defer filtered.Stop()

			all, err := s.Watch(ctx, &metainternal.ListOptions{ResourceVersion: "2"})
			require.NoError(t, err)

			defer all.Stop()

			_, rv, err := s.store.Update(ctx, false, func(l *ledger.Ledger) error {
				old := l.Grants()[0]
				l.Remove(old.Key(), s.clock.Now())
				next := *old
				next.Name = "b"
				next.UID = "b"
				next.Labels = map[string]string{"team": "red"}

				return l.Add(&next)
			})
			require.NoError(t, err)
			cached(t, s)

			if replay {
				filtered.Stop()
				filtered, err = s.Watch(ctx, options)
				require.NoError(t, err)

				defer filtered.Stop()
			}

			event(t, filtered, watch.Deleted, "a", rv)
			event(t, all, watch.Deleted, "a", "2")
			event(t, all, watch.Added, "b", rv)
			require.Empty(t, filtered.ResultChan())
			filtered.Stop()

			options.ResourceVersion = rv
			resumed, err := s.Watch(ctx, options)
			require.NoError(t, err)

			defer resumed.Stop()

			require.Empty(t, resumed.ResultChan(), "resuming must not replay the filtered deletion")
		})
	}
}

type watchResult struct {
	watch watch.Interface
	err   error
}

func startWaitingWatch(t *testing.T, s *storage, ctx context.Context, rv string) <-chan watchResult {
	t.Helper()

	result := make(chan watchResult, 1)

	go func() {
		w, err := s.Watch(ctx, &metainternal.ListOptions{ResourceVersion: rv})
		result <- watchResult{watch: w, err: err}
	}()

	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()

		return s.history[len(s.history)-1].changed != nil
	}, time.Second, time.Millisecond)

	return result
}

func waitingWatchResult(t *testing.T, result <-chan watchResult) watchResult {
	t.Helper()

	select {
	case got := <-result:
		if got.watch != nil {
			t.Cleanup(got.watch.Stop)
		}

		return got
	case <-time.After(time.Second):
		t.Fatal("watch did not finish waiting")
		return watchResult{}
	}
}

func TestWatchWaitForHistory(t *testing.T) {
	for _, exact := range []bool{true, false} {
		t.Run(fmt.Sprint("exact=", exact), func(t *testing.T) {
			s, _, ctx := fixture(t)
			_, err := s.Create(ctx, grant("a", "node-a"), nil, nil)
			require.NoError(t, err)
			listed, err := s.List(ctx, nil)
			require.NoError(t, err)

			list, ok := listed.(*rebound.NodeGrantList)
			require.True(t, ok)
			require.Equal(t, "2", list.ResourceVersion)

			result := startWaitingWatch(t, s, ctx, list.ResourceVersion)
			if exact {
				cached(t, s)
			}

			created(t, s, ctx, "b", "node-b")
			got := waitingWatchResult(t, result)
			require.NoError(t, got.err)

			if !exact {
				event(t, got.watch, watch.Added, "a", "1")
			}

			event(t, got.watch, watch.Added, "b", "3")
			require.Empty(t, got.watch.ResultChan())
		})
	}
}

func TestWatchHistoryBounds(t *testing.T) {
	s, _, ctx := fixture(t)
	s.history = []snapshot{{ledger: ledger.New(), rv: "0"}}
	s.observe(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: ledger.ConfigMapName, Namespace: "system", ResourceVersion: "10",
	}}, false)
	require.Len(t, s.history, 1, "the initial list must replace the synthetic seed")

	for _, rv := range []string{"1", "9", "unknown", "-1", "18446744073709551616"} {
		_, err := s.Watch(ctx, &metainternal.ListOptions{ResourceVersion: rv})
		require.True(t, apierrors.IsResourceExpired(err), "RV %q: %v", rv, err)
	}

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: ledger.ConfigMapName, Namespace: "system", ResourceVersion: "20",
	}}
	s.observe(cm, false)

	for _, rv := range []string{"10", "15", "20"} {
		w, err := s.Watch(ctx, &metainternal.ListOptions{ResourceVersion: rv})
		require.NoError(t, err)
		require.Empty(t, w.ResultChan())
		w.Stop()
	}
}

func TestWatchBetweenSnapshots(t *testing.T) {
	s, _, ctx := fixture(t)
	_, err := s.Create(ctx, grant("a", "node-a"), nil, nil)
	require.NoError(t, err)
	created(t, s, ctx, "b", "node-b")

	w, err := s.Watch(ctx, &metainternal.ListOptions{ResourceVersion: "2"})
	require.NoError(t, err)

	defer w.Stop()

	event(t, w, watch.Added, "a", "1")
	event(t, w, watch.Added, "b", "3")
	require.Empty(t, w.ResultChan())
}

func TestWatchHistoryTimeout(t *testing.T) {
	previous := watchHistoryTimeout
	watchHistoryTimeout = 20 * time.Millisecond

	t.Cleanup(func() { watchHistoryTimeout = previous })

	s, _, ctx := fixture(t)
	_, err := s.Watch(ctx, &metainternal.ListOptions{ResourceVersion: "2"})
	require.True(t, apistorage.IsTooLargeResourceVersion(err), "%v", err)
	delay, retry := apierrors.SuggestsClientDelay(err)
	require.True(t, retry)
	require.Equal(t, 1, delay)
}

func TestWatchHistoryWaitInterrupted(t *testing.T) {
	for _, closeStorage := range []bool{false, true} {
		t.Run(fmt.Sprint("close=", closeStorage), func(t *testing.T) {
			s, _, ctx := fixture(t)

			ctx, cancel := context.WithCancel(ctx)
			defer cancel()

			result := startWaitingWatch(t, s, ctx, "2")
			if closeStorage {
				s.Destroy()
				s.Destroy()
			} else {
				cancel()
			}

			got := waitingWatchResult(t, result)
			if closeStorage {
				require.True(t, apierrors.IsServiceUnavailable(got.err), "%v", got.err)
			} else {
				require.ErrorIs(t, got.err, context.Canceled)
			}
		})
	}
}
