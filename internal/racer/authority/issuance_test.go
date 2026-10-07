// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authority

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestIssuanceReadCancellationPreservesTrust(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		for _, boundary := range []string{"marker", "version", "credentials"} {
			for _, successfulRead := range []bool{false, true} {
				t.Run(boundary+"/"+map[bool]string{false: "cancel", true: "deadline"}[deadline]+"/"+map[bool]string{false: "failed read", true: "successful read"}[successfulRead], func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						f := newServingFixture(t)
						a := f.a.authority
						identity := pollIdentity(a.config, testNodeUID)
						identity.owner, identity.bearer = a, true
						confirmed, epoch, bundle := a.trust.confirmed, a.trust.authority, a.trust.bundle
						publicationConfirmed := a.publications.confirmed

						ctx, cancel := context.WithCancel(t.Context())
						defer cancel()

						want := context.Canceled

						if deadline {
							var stop context.CancelFunc

							ctx, stop = context.WithTimeout(ctx, time.Second)
							defer stop()

							want = context.DeadlineExceeded
						}

						target := map[string]string{"marker": a.config.InstallationConfigMapName, "version": a.config.VersionConfigMapName, "credentials": a.config.CredentialsSecretName}[boundary]
						reads := 0
						a.bootstrap.Issuer.APIReader = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{
							Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
								err := c.Get(ctx, key, obj, opts...)
								if key.Name != target {
									return err
								}

								reads++

								if deadline {
									time.Sleep(time.Second)
								} else {
									cancel()
								}

								if !successfulRead {
									return ctx.Err()
								}

								return err
							},
						})
						encoded, err := a.Issue(ctx, identity, f.request)
						require.ErrorIs(t, err, want)
						require.Empty(t, encoded)
						require.Equal(t, 1, reads)
						require.NoError(t, a.TrustReady())
						require.NoError(t, a.PublicationReady())
						require.Equal(t, confirmed, a.trust.confirmed, "issuance must not refresh trust")
						require.Equal(t, publicationConfirmed, a.publications.confirmed)
						require.Same(t, bundle, a.trust.bundle)
						require.Equal(t, epoch, a.trust.authority)
						require.NoError(t, epoch.Err())

						gateCtx, stop := context.WithTimeout(t.Context(), time.Second)
						defer stop()

						require.NoError(t, a.gate.Acquire(gateCtx), "issuance must release its gate")
						a.gate.Release()
					})
				})
			}
		}
	}
}

func TestIssuanceInvalidEvidenceWithCanceledRequestRevokesTrust(t *testing.T) {
	for _, boundary := range []string{"marker", "version", "credentials"} {
		t.Run(boundary, func(t *testing.T) {
			f := newServingFixture(t)
			a := f.a.authority
			identity := pollIdentity(a.config, testNodeUID)
			identity.owner, identity.bearer = a, true
			epoch := a.trust.authority

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			target := map[string]string{"marker": a.config.InstallationConfigMapName, "version": a.config.VersionConfigMapName, "credentials": a.config.CredentialsSecretName}[boundary]
			a.bootstrap.Issuer.APIReader = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					err := c.Get(ctx, key, obj, opts...)
					if key.Name == target {
						switch obj := obj.(type) {
						case *corev1.ConfigMap:
							obj.Data = nil
						case *corev1.Secret:
							obj.Data = nil
						}

						cancel()
					}

					return err
				},
			})
			encoded, err := a.Issue(ctx, identity, f.request)
			require.Error(t, err)
			require.NotErrorIs(t, err, context.Canceled, "invalid evidence must not be replaced by request cancellation")
			require.Empty(t, encoded)
			require.ErrorIs(t, a.TrustReady(), wire.Unavailable)
			require.ErrorIs(t, epoch.Err(), context.Canceled)
		})
	}
}

func TestIssuanceStormRetainsAuthoritativeReads(t *testing.T) {
	f := newServingFixture(t)
	a := f.a.authority
	identity := pollIdentity(a.config, testNodeUID)
	identity.owner, identity.bearer = a, true
	confirmed := a.trust.confirmed

	var reads atomic.Int64

	a.bootstrap.Issuer.APIReader = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			reads.Add(1)
			return c.Get(ctx, key, obj, opts...)
		},
	})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	const workers, requests = 8, 8

	errors := make(chan error, workers*requests)
	start := time.Now()

	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for range requests {
				_, err := a.Issue(ctx, identity, f.request)
				errors <- err
			}
		})
	}

	wg.Wait()
	close(errors)

	for err := range errors {
		require.NoError(t, err)
	}

	require.EqualValues(t, workers*requests*3, reads.Load(), "every issuance must read marker, version, and credentials")
	require.Equal(t, confirmed, a.trust.confirmed)
	t.Logf("%d issuances, %d workers, %d authoritative reads, elapsed %s (in-memory API fixture)", workers*requests, workers, reads.Load(), time.Since(start))
}
