// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestStartupDeadlineCancelsAPI(t *testing.T) {
	for _, operation := range []string{"initial marker", "initial version", "update", "create", "final marker", "final version"} {
		for _, boundary := range []string{"application", "earlier caller", "caller cancellation"} {
			t.Run(operation+"/"+boundary, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					r := testTopology(t)

					parent, cancel := context.WithCancel(t.Context())
					defer cancel()

					wantDuration := 30 * time.Second
					wantErr := context.DeadlineExceeded

					switch boundary {
					case "earlier caller":
						var stop context.CancelFunc

						parent, stop = context.WithTimeout(parent, time.Second)
						defer stop()

						wantDuration = time.Second
					case "caller cancellation":
						time.AfterFunc(time.Second, cancel)
						wantDuration = time.Second
						wantErr = context.Canceled
					}

					blocked := false
					block := func(ctx context.Context) error {
						blocked = true

						<-ctx.Done()

						return ctx.Err()
					}
					created := false
					c := interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
						Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
							stage := "initial "
							if created {
								stage = "final "
							}

							if key.Name == r.Config.InstallationConfigMapName {
								stage += "marker"
							} else {
								stage += "version"
							}

							if operation == stage {
								return block(ctx)
							}
							// Spending budget before later calls catches per-call resets.
							time.Sleep(100 * time.Millisecond)

							return c.Get(ctx, key, obj, opts...)
						},
						Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
							if operation == "update" {
								return block(ctx)
							}

							return c.Update(ctx, obj, opts...)
						},
						Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
							if operation == "create" {
								return block(ctx)
							}

							created = true

							return c.Create(ctx, obj, opts...)
						},
					})

					start := time.Now()

					err := Assemble(r.Config, c, c).Recover(parent, c)
					if !blocked || !errors.Is(err, wantErr) || time.Since(start) != wantDuration {
						t.Fatalf("blocked=%v error=%v elapsed=%v; want %v after %v", blocked, err, time.Since(start), wantErr, wantDuration)
					}

					if boundary == "application" && parent.Err() != nil {
						t.Fatalf("recovery canceled parent: %v", parent.Err())
					}
				})
			})
		}
	}
}

func TestStartupDeadlineSuccess(t *testing.T) {
	for _, installed := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "installed"}[installed], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := testTopology(t)
				if installed {
					if err := ensureInstalled(t.Context(), r.Client, r.APIReader, r.Config); err != nil {
						t.Fatal(err)
					}
				}

				var recovery []context.Context

				reader := interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if len(recovery) == 0 {
							recovery = append(recovery, ctx)

							deadline, ok := ctx.Deadline()
							if !ok || time.Until(deadline) != 30*time.Second {
								t.Fatalf("startup deadline=%v present=%v", deadline, ok)
							}
						}

						return c.Get(ctx, key, obj, opts...)
					},
				})

				a := Assemble(r.Config, r.Client, reader)
				if err := a.Recover(t.Context(), r.Client); err != nil {
					t.Fatal(err)
				}

				if len(recovery) == 0 || !errors.Is(recovery[0].Err(), context.Canceled) {
					t.Fatal("recovery context not released on success")
				}

				if t.Context().Err() != nil || a.Server.Ready(nil) == nil {
					t.Fatal("recovery canceled caller or granted serving authority")
				}
			})
		})
	}
}

func TestStartupDeadlineAllowsCompetingInstallerWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := testTopology(t)

		c := interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
			Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				return errors.New("winner stopped before version creation")
			},
		})
		if err := ensureInstalled(t.Context(), c, c, r.Config); err == nil {
			t.Fatal("expected incomplete installation")
		}

		first := true
		reader := interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if first {
					first = false

					time.Sleep(2 * time.Second)
				}

				return c.Get(ctx, key, obj, opts...)
			},
		})
		start := time.Now()

		err := Assemble(r.Config, r.Client, reader).Recover(t.Context(), r.Client)
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != 7*time.Second {
			t.Fatalf("competing installer wait: error=%v elapsed=%v", err, time.Since(start))
		}
	})
}
