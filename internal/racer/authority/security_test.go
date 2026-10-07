// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authority

import (
	"context"
	"crypto/x509"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestSecurityPublicationReplayBeforeObservation(t *testing.T) {
	for _, scenario := range []string{"rollback", "replacement"} {
		t.Run(scenario, func(t *testing.T) {
			f := newServingFixture(t)
			a := f.a.authority
			image, err := a.Current()
			require.NoError(t, err)
			guard, stop, err := image.Admit(t.Context())
			require.NoError(t, err)

			defer stop()

			cm, record, err := readVersion(t.Context(), a.reader, a.config)
			require.NoError(t, err)

			if scenario == "rollback" {
				record.Sequence--
				record.MembershipVersion--
			} else {
				record.ContentHash = strings.Repeat("a", 64)
			}

			cm.Data = versionData(record)
			require.NoError(t, f.a.Topology.Update(t.Context(), cm))

			called := false
			_, err = a.PublishTopology(t.Context(), func(context.Context) (TopologyObservation, error) {
				called = true
				return TopologyObservation{}, errors.New("discovery unavailable")
			})
			require.ErrorIs(t, err, wire.Conflict)
			require.False(t, called, "invalid authority reached discovery")
			require.ErrorIs(t, guard.Check(t.Context()), context.Canceled)
		})
	}
}

func TestSecurityPublicationReplayBeforeCAS(t *testing.T) {
	f := newServingFixture(t)
	a := f.a.authority
	cm, previous, err := readVersion(t.Context(), a.reader, a.config)
	require.NoError(t, err)
	prepared, err := a.publications.Prepare(previous, cm.ResourceVersion, nil, nil)
	require.NoError(t, err)

	newer := previous
	newer.Sequence += 2
	require.NoError(t, a.publications.confirm(newer))
	_, err = a.publisher.CommitVersion(t.Context(), prepared)
	after, record, readErr := readVersion(t.Context(), a.reader, a.config)
	require.NoError(t, readErr)
	require.Equal(t, cm.ResourceVersion, after.ResourceVersion, "stale counter reached external CAS")
	require.Equal(t, previous, record)
	require.ErrorIs(t, err, wire.Conflict)
	require.Equal(t, newer, a.publications.observed)
}

func TestSecurityPublicationReplacementDuringObservation(t *testing.T) {
	f := newServingFixture(t)
	a := f.a.authority
	image, err := a.Current()
	require.NoError(t, err)
	guard, stop, err := image.Admit(t.Context())
	require.NoError(t, err)

	defer stop()

	_, err = a.PublishTopology(t.Context(), func(ctx context.Context) (TopologyObservation, error) {
		cm, record, err := readVersion(ctx, a.reader, a.config)
		require.NoError(t, err)

		record.ContentHash = strings.Repeat("b", 64)
		cm.Data = versionData(record)
		require.NoError(t, f.a.Topology.Update(ctx, cm))

		return f.a.Topology.observeTopology(ctx)
	})
	require.ErrorIs(t, err, wire.Conflict)
	require.ErrorIs(t, guard.Check(t.Context()), context.Canceled, "CAS mismatch concealed invalid authority")
}

func TestSecurityCredentialReplayBeforeUse(t *testing.T) {
	for _, scenario := range []string{"rollback", "replacement"} {
		for _, operation := range []string{"issue", "catalog", "rotation"} {
			t.Run(scenario+"/"+operation, func(t *testing.T) {
				f := newServingFixture(t)
				a, r := f.a.authority, f.a.Keyring
				volume := catalogVolume("cache", testNodeUID)
				require.NoError(t, r.Create(t.Context(), &volume))
				runKeys(t, r)
				_, oldBundle, oldRotation, oldMaterial := keyState(t, r)
				_, bundle, rotation, material := keyState(t, r)
				replacement := editSigningCertificate(t, material.Keys[rotation.ActiveIssuer], func(cert *x509.Certificate) {
					cert.NotBefore = cert.NotBefore.Add(-time.Second)
				})
				rebindSigning(&bundle, &rotation, material, rotation.ActiveIssuer, replacement, true)
				bundle.Generation++
				writeSigningCredentials(t, r, bundle, rotation, material)
				runKeys(t, r)

				confirmed, highWater, digest := a.trust.confirmed, a.trust.highWater, a.trust.digest
				guard, stop, err := a.AdmitTrust(t.Context())
				require.NoError(t, err)

				defer stop()

				if scenario == "replacement" {
					oldBundle.Generation = bundle.Generation
				}

				writeSigningCredentials(t, r, oldBundle, oldRotation, oldMaterial)
				before, _, _, _ := keyState(t, r)
				version, _, err := readVersion(t.Context(), a.reader, a.config)
				require.NoError(t, err)

				switch operation {
				case "issue":
					identity := pollIdentity(a.config, testNodeUID)
					identity.owner, identity.bearer = a, true
					encoded, issueErr := a.Issue(t.Context(), identity, f.request)
					err = issueErr

					require.Zero(t, len(encoded), "replayed issuer signed a fresh credential")
				case "catalog":
					_, err = a.PublishTopology(t.Context(), f.a.Topology.observeTopology)
				case "rotation":
					a.credentials.Now = func() time.Time { return oldRotation.NextRotation }
					_, err = a.ReconcileCredentials(t.Context())
				}

				after := &corev1.Secret{}
				require.NoError(t, a.reader.Get(t.Context(), client.ObjectKeyFromObject(before), after))
				require.Equal(t, before.ResourceVersion, after.ResourceVersion, "replayed credentials reached rotation CAS")
				afterVersion, _, readErr := readVersion(t.Context(), a.reader, a.config)
				require.NoError(t, readErr)
				require.Equal(t, version.ResourceVersion, afterVersion.ResourceVersion, "replayed catalog reached publication CAS")
				require.ErrorIs(t, err, wire.Conflict)
				require.ErrorIs(t, guard.Check(t.Context()), context.Canceled)
				require.Equal(t, highWater, a.trust.highWater)
				require.Equal(t, digest, a.trust.digest)
				require.Equal(t, confirmed, a.trust.confirmed)
			})
		}
	}
}

func TestSecurityCredentialValidationDoesNotInstallOrRefresh(t *testing.T) {
	for _, newer := range []bool{false, true} {
		f := newServingFixture(t)
		a, r := f.a.authority, f.a.Keyring

		_, bundle, rotation, material := keyState(t, r)
		if newer {
			bundle.Generation++
			writeSigningCredentials(t, r, bundle, rotation, material)
		}

		confirmed, highWater, digest := a.trust.confirmed, a.trust.highWater, a.trust.digest
		accepted, epoch := a.trust.bundle, a.trust.authority
		identity := pollIdentity(a.config, testNodeUID)
		identity.owner, identity.bearer = a, true
		encoded, err := a.Issue(t.Context(), identity, f.request)
		require.NoError(t, err)
		require.NotZero(t, len(encoded))
		_, err = a.PublishTopology(t.Context(), f.a.Topology.observeTopology)
		require.NoError(t, err)
		require.Equal(t, confirmed, a.trust.confirmed)
		require.Equal(t, highWater, a.trust.highWater)
		require.Equal(t, digest, a.trust.digest)
		require.Same(t, accepted, a.trust.bundle)
		require.Equal(t, epoch, a.trust.authority)
		a.trust.invalidate()
		require.NoError(t, a.trust.validateReplay(bundle))
		require.ErrorIs(t, a.TrustReady(), wire.Unavailable, "validation restored withdrawn trust")
		require.Equal(t, confirmed, a.trust.confirmed)
		require.Equal(t, highWater, a.trust.highWater)
		require.Equal(t, digest, a.trust.digest)
	}
}
