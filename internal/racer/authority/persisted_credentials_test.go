// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authority

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestValidatePersistedCredentialsDoesNotRequireSigningReadiness(t *testing.T) {
	for _, scenario := range []string{"longer lifetime", "expired active", "expired preparation"} {
		t.Run(scenario, func(t *testing.T) {
			r, now := testKeyring(t)
			r.Config.CertificateLifetime = 2 * time.Minute
			r.Config.Rotation = RotationPolicy{Interval: 5 * time.Minute, PrepareFor: 20 * time.Second, RetainFor: 2 * time.Minute}
			runKeys(t, r)

			_, _, rotation, _ := keyState(t, r)
			if scenario == "expired preparation" {
				*now = rotation.NextRotation

				runKeys(t, r)
			}

			cfg := r.Config
			if scenario == "longer lifetime" {
				cfg.CertificateLifetime = wire.CertificateLifetime
				cfg.Rotation = RotationPolicy{Interval: 24 * time.Hour, PrepareFor: time.Hour, RetainFor: 48 * time.Hour}
			} else {
				*now = now.Add(30 * 24 * time.Hour)
			}

			before, _, _, _ := keyState(t, r)
			_, err := loadSigning(t.Context(), r.APIReader, cfg, *now)
			require.ErrorIs(t, err, wire.Unavailable)
			a := New(cfg, Dependencies{Reader: r.APIReader, Writer: rejectWrites(t, r.Client.(client.WithWatch))})
			require.NoError(t, a.ValidatePersistedCredentials(t.Context()))
			require.ErrorIs(t, a.TrustReady(), wire.Unavailable)
			require.ErrorIs(t, a.PublicationReady(), wire.Unavailable)
			require.Zero(t, a.trust.highWater)
			require.True(t, a.trust.confirmed.IsZero())
			after, _, _, _ := keyState(t, r)
			require.Equal(t, before, after)
		})
	}
}

func TestValidatePersistedCredentialsRejectsDamagedState(t *testing.T) {
	for _, scenario := range []string{"missing", "replacement", "binding", "issuer.json", "bundle.json", "rotation.json", "claim", "version"} {
		t.Run(scenario, func(t *testing.T) {
			r, _ := testKeyring(t)
			runKeys(t, r)
			secret, _, _, _ := keyState(t, r)
			version, _, err := readVersion(t.Context(), r.APIReader, r.Config)
			require.NoError(t, err)

			switch scenario {
			case "missing", "replacement":
				require.NoError(t, r.Delete(t.Context(), secret))

				if scenario == "replacement" {
					secret.UID, secret.ResourceVersion = "", ""
					require.NoError(t, r.Create(t.Context(), secret))
				}
			case "claim", "version":
				if scenario == "claim" {
					delete(version.Annotations, credentialClaim)
				} else {
					version.Data["sequence"] = "0"
				}

				require.NoError(t, r.Update(t.Context(), version))
			default:
				if scenario == "binding" {
					secret.Annotations[installationUIDAnnotation] = "foreign"
				} else {
					delete(secret.Data, scenario)
				}

				require.NoError(t, r.Update(t.Context(), secret))
			}

			a := New(r.Config, Dependencies{Reader: r.APIReader, Writer: rejectWrites(t, r.Client.(client.WithWatch))})
			require.Error(t, a.ValidatePersistedCredentials(t.Context()))
			require.ErrorIs(t, a.TrustReady(), wire.Unavailable)
		})
	}
}

func TestValidatePersistedCredentialsCancellation(t *testing.T) {
	r, _ := testKeyring(t)
	runKeys(t, r)
	a := New(r.Config, Dependencies{Reader: r.APIReader})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, a.ValidatePersistedCredentials(ctx), context.Canceled)
}
