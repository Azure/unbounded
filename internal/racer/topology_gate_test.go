// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestTopologyAnnotationDoesNotBlockObserver(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "cancellation"} {
		t.Run(outcome, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newServingFixture(t)
				r := f.a.Topology

				var node corev1.Node
				require.NoError(t, r.Get(f.ctx, client.ObjectKey{Name: "worker"}, &node))
				node.Annotations["racer.unbounded-cloud.io/shares"] = "7"
				require.NoError(t, r.Update(f.ctx, &node))

				entered, release := make(chan struct{}), make(chan struct{})
				patchError := errors.New("patch failed")
				r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
					Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						close(entered)

						select {
						case <-ctx.Done():
							return ctx.Err()
						case <-release:
						}

						if outcome == "failure" {
							return patchError
						}

						return c.Patch(ctx, obj, patch, opts...)
					},
				})

				ctx, cancel := context.WithCancel(f.ctx)
				defer cancel()

				done := make(chan error, 1)

				go func() { _, err := r.Reconcile(ctx, ctrl.Request{}); done <- err }()

				<-entered
				require.EqualValues(t, 7, acceptedMembers(t, r)[testNodeUID].Shares)
				// Cross the original freshness deadline while the actual Node patch
				// remains blocked. The real observer must renew both accepted states.
				for range 3 {
					time.Sleep(20 * time.Second)

					observed := make(chan struct{})

					go func() { f.a.Replication.observe(f.ctx); close(observed) }()

					synctest.Wait()

					select {
					case <-observed:
					default:
						t.Fatal("observer blocked behind annotation patch")
					}

					require.NoError(t, f.a.Server.Ready(nil))
				}

				if outcome == "cancellation" {
					cancel()
				} else {
					close(release)
				}

				err := <-done

				switch outcome {
				case "success":
					require.NoError(t, err)
				case "failure":
					require.ErrorIs(t, err, patchError)
				case "cancellation":
					require.ErrorIs(t, err, context.Canceled)
				}

				gateCtx, stop := context.WithTimeout(f.ctx, time.Second)
				defer stop()

				require.NoError(t, f.a.authority.Observe(gateCtx))
			})
		})
	}
}
