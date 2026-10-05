// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authority

import (
	"bytes"
	"context"
	"crypto/x509"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestIdentityAndServingHandleProvenance(t *testing.T) {
	one, two := newServingFixture(t), newServingFixture(t)

	a, b := one.a.authority, two.a.authority
	_, err := a.Issue(t.Context(), NodeIdentity{owner: a, cluster: a.config.Cluster, node: testNodeUID, expires: time.Now().Add(time.Hour)}, one.request)
	require.ErrorIs(t, err, wire.Unauthenticated, "certificate identity cannot authorize token-only issuance")

	for _, identity := range []NodeIdentity{{}, {owner: b, cluster: a.config.Cluster, node: testNodeUID, expires: time.Now().Add(time.Hour)}} {
		_, err := a.Issue(t.Context(), identity, one.request)
		require.ErrorIs(t, err, wire.Unauthenticated)
		_, err = a.Wait(t.Context(), identity, nil)
		require.ErrorIs(t, err, wire.Unauthenticated)
	}

	p, err := a.Current()
	require.NoError(t, err)
	other, stopOther, err := b.TrustContext(t.Context())
	require.NoError(t, err)

	defer stopOther()

	_, _, err = p.WriteContextWithTrust(t.Context(), other)
	require.ErrorIs(t, err, wire.Forbidden)
	_, err = p.ForBase("").WriteTo(other, io.Discard)
	require.ErrorIs(t, err, wire.Forbidden)

	var zero PublicationHandle

	_, _, err = zero.WriteContext(t.Context())
	require.ErrorIs(t, err, wire.Unavailable)
	_, err = zero.ForBase("").WriteTo(t.Context(), io.Discard)
	require.ErrorIs(t, err, wire.Forbidden)
	keyring, err := a.Keyring()
	require.NoError(t, err)
	_, err = keyring.Response().WriteTo(other, io.Discard)
	require.ErrorIs(t, err, wire.Forbidden)
	_, err = keyring.Response().WriteTo(t.Context(), io.Discard)
	require.ErrorIs(t, err, wire.Forbidden)

	var empty KeyringHandle

	_, err = empty.Response().WriteTo(other, io.Discard)
	require.ErrorIs(t, err, wire.Forbidden)
}

func TestKeyringHandleCannotBorrowRecoveredTrust(t *testing.T) {
	f := newServingFixture(t)
	a := f.a.authority
	old, err := a.Keyring()
	require.NoError(t, err)
	guard, stop, err := a.TrustContext(t.Context())
	require.NoError(t, err)

	defer stop()

	a.trust.invalidate()
	require.NoError(t, a.Observe(t.Context()))
	require.ErrorIs(t, guard.Err(), context.Canceled)
	fresh, stopFresh, err := a.TrustContext(t.Context())
	require.NoError(t, err)

	defer stopFresh()

	_, err = old.Response().WriteTo(fresh, io.Discard)
	require.ErrorIs(t, err, wire.Forbidden)
	current, err := a.Keyring()
	require.NoError(t, err)
	_, err = current.Response().WriteTo(fresh, io.Discard)
	require.NoError(t, err)
}

func TestAnnotationHintsDoNotAliasNestedHistory(t *testing.T) {
	numa := uint32(3)
	history := AcceptedMembers{testNodeUID: {Node: testNodeUID, RDMANICs: []wire.RDMANIC{{Device: "mlx5_0", Port: 1, NUMANode: &numa}}}}
	hints := cloneAccepted(history)
	*hints[testNodeUID].RDMANICs[0].NUMANode = 99
	require.EqualValues(t, 3, *history[testNodeUID].RDMANICs[0].NUMANode)
}

func TestAuthorityConstructionFreezesAuthenticationAndIssuance(t *testing.T) {
	f := newServingFixture(t)
	cfg := f.a.Topology.Config
	cfg.CertificateLifetime = 2 * time.Minute
	a := New(cfg, Dependencies{Reader: f.a.Topology.APIReader, Writer: f.a.Topology.Client})
	cfg.Cluster = ""
	cfg.CertificateLifetime = time.Second

	require.Equal(t, f.a.Topology.Config.Cluster, a.bootstrap.runtimeConfig().Cluster)
	require.Equal(t, 2*time.Minute, a.bootstrap.Issuer.runtimeConfig().CertificateLifetime)
	identity := NodeIdentity{owner: a, bearer: true, cluster: a.config.Cluster, node: testNodeUID, expires: time.Now().Add(time.Hour)}
	encoded, err := a.Issue(t.Context(), identity, f.request)
	require.NoError(t, err)
	response, err := wire.DecodeBootstrapResponse(bytes.NewReader(encoded))
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(response.CertificateChain[0])
	require.NoError(t, err)
	require.Equal(t, 3*time.Minute, leaf.NotAfter.Sub(leaf.NotBefore))
}
