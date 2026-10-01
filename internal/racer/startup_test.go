// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestConcurrentStartupSingleCreate(t *testing.T) {
	for _, failure := range []string{"none", "create denied", "create response lost", "marker response lost"} {
		t.Run(failure, func(t *testing.T) {
			r := testTopology(t)
			base := r.Client.(client.WithWatch)

			const replicas = 8

			arrived := make(chan struct{}, replicas)
			proceed := make(chan struct{})
			results := make(chan error, replicas)

			var creates, consumed atomic.Int32

			boom := errors.New(failure)
			writer := interceptor.NewClient(base, interceptor.Funcs{
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					arrived <- struct{}{}

					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-proceed:
					}

					if err := c.Update(ctx, obj, opts...); err != nil {
						return err
					}

					consumed.Add(1)

					if failure == "marker response lost" {
						return boom
					}

					return nil
				},
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					creates.Add(1)

					if failure == "create denied" {
						return boom
					}

					if err := c.Create(ctx, obj, opts...); err != nil {
						return err
					}

					if failure == "create response lost" {
						return boom
					}

					return nil
				},
			})

			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()

			for range replicas {
				go func() { results <- Assemble(r.Config, writer, base).Recover(ctx, writer) }()
			}

			for range replicas {
				select {
				case <-arrived:
				case <-ctx.Done():
					close(proceed)
					t.Fatal("startups did not reach CAS")
				}
			}

			close(proceed)

			successes := 0

			for range replicas {
				if err := <-results; err == nil {
					successes++
				}
			}

			wantCreates, wantSuccess := int32(1), 0

			switch failure {
			case "none":
				wantSuccess = replicas
			case "create response lost":
				wantSuccess = replicas - 1
			case "marker response lost":
				wantCreates = 0
			}

			if consumed.Load() != 1 || creates.Load() != wantCreates || successes != wantSuccess {
				t.Fatalf("consumed=%d creates=%d successes=%d", consumed.Load(), creates.Load(), successes)
			}
		})
	}
}

func TestStartupRecoveryNeverWrites(t *testing.T) {
	for _, state := range []string{"valid", "missing", "corrupt", "wrong binding", "mutable marker", "read denied"} {
		t.Run(state, func(t *testing.T) {
			r := initializedTopology(t)

			cm, _, err := readVersion(t.Context(), r.APIReader, r.Config)
			if err != nil {
				t.Fatal(err)
			}

			switch state {
			case "missing":
				err = r.Delete(t.Context(), cm)
			case "corrupt":
				cm.Data["sequence"] = "0"
				err = r.Update(t.Context(), cm)
			case "wrong binding":
				cm.Annotations[installationUIDAnnotation] = "foreign"
				err = r.Update(t.Context(), cm)
			case "mutable marker":
				marker, getErr := readInstallation(t.Context(), r.APIReader, r.Config, false)
				if getErr != nil {
					t.Fatal(getErr)
				}

				marker.Immutable = nil
				err = r.Update(t.Context(), marker)
			}

			if err != nil {
				t.Fatal(err)
			}

			writes := 0
			c := interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
				Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
					writes++
					return errors.New("unexpected Update")
				},
				Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
					writes++
					return errors.New("unexpected Create")
				},
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if state == "read denied" {
						return apierrors.NewForbidden(corev1.Resource("configmaps"), key.Name, errors.New("denied"))
					}

					return c.Get(ctx, key, obj, opts...)
				},
			})

			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()

			err = Assemble(r.Config, c, c).Recover(ctx, c)
			if (err == nil) != (state == "valid") || writes != 0 {
				t.Fatalf("recovery err=%v writes=%d", err, writes)
			}
		})
	}
}

func TestStartupWaitsForWinnerGap(t *testing.T) {
	r := testTopology(t)
	base := r.Client.(client.WithWatch)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	creating := make(chan struct{})
	observedGap := make(chan struct{}, 1)
	results := make(chan error, 2)

	var creates atomic.Int32

	winner := interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			creates.Add(1)
			close(creating)

			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-observedGap:
			}

			return c.Create(ctx, obj, opts...)
		},
	})

	go func() { results <- Assemble(r.Config, winner, base).Recover(ctx, winner) }()

	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-creating:
	}

	follower := interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			err := c.Get(ctx, key, obj, opts...)
			if key.Name == r.Config.VersionConfigMapName && apierrors.IsNotFound(err) {
				select {
				case observedGap <- struct{}{}:
				default:
				}
			}

			return err
		},
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			creates.Add(1)
			return errors.New("follower must not create")
		},
		Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
			return errors.New("follower must not update")
		},
	})

	go func() { results <- Assemble(r.Config, follower, follower).Recover(ctx, follower) }()

	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}

	if creates.Load() != 1 {
		t.Fatalf("Create attempts: %d", creates.Load())
	}
}
