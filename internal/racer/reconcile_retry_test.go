// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestReconcilerDependencyCancellationRetries(t *testing.T) {
	for _, controller := range []string{"topology", "keyring"} {
		for _, stage := range []string{"read", "write", "completion read"} {
			if controller == "topology" && stage == "completion read" {
				continue
			}

			for _, dependencyErr := range []error{context.DeadlineExceeded, context.Canceled} {
				for _, cancelParent := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/%v/parent=%v", controller, stage, dependencyErr, cancelParent), func(t *testing.T) {
						topology := initializedTopology(t)
						app := Assemble(topology.Config, topology.Client, topology.APIReader)

						var target reconcile.Reconciler = app.Topology
						if controller == "keyring" {
							runKeys(t, app.Keyring)
							_, _, state, _ := keyState(t, app.Keyring)
							app.Keyring.Now = func() time.Time { return state.NextRotation }
							target = app.Keyring
						}

						ctx, cancel := context.WithCancel(t.Context())
						defer cancel()

						injected, reads := false, 0
						fail := func() error {
							injected = true

							if cancelParent {
								cancel()
							}

							return fmt.Errorf("dependency: %w", dependencyErr)
						}
						base := topology.Client.(client.WithWatch)
						wrapped := interceptor.NewClient(base, interceptor.Funcs{
							Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
								if _, ok := obj.(*corev1.ConfigMap); ok && key.Name == topology.Config.VersionConfigMapName {
									reads++
									if stage == "read" || stage == "completion read" && reads == 2 {
										return fail()
									}
								}

								return c.Get(ctx, key, obj, opts...)
							},
							Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
								if stage == "write" {
									return fail()
								}

								return c.Update(ctx, obj, opts...)
							},
						})
						app.Topology.Client, app.Topology.APIReader = wrapped, wrapped
						app.Keyring.Client, app.Keyring.APIReader = wrapped, wrapped

						_, err := target.Reconcile(ctx, ctrl.Request{})

						want := dependencyErr
						if cancelParent {
							want = context.Canceled
						}

						if !injected || !errors.Is(err, want) || errors.Is(err, reconcile.TerminalError(nil)) != cancelParent {
							t.Fatalf("injected=%v, error=%v, parent canceled=%v", injected, err, cancelParent)
						}

						// A fresh reconcile succeeds without waiting for another watch event.
						app.Topology.Client, app.Topology.APIReader = base, base
						app.Keyring.Client, app.Keyring.APIReader = base, base

						if _, err := target.Reconcile(t.Context(), ctrl.Request{}); err != nil {
							t.Fatalf("retry failed: %v", err)
						}
					})
				}
			}
		}
	}
}
