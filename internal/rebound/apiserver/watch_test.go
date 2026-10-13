// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package apiserver

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	metainternal "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/watch"

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
