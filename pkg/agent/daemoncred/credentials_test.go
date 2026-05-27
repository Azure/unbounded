// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemoncred

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
)

func TestNewRESTConfigProviderUsesStoredCertificate(t *testing.T) {
	opts := testControllerCertificateOptions(t)
	certPEM, keyPEM := testCertificate(t, "first")
	storeControllerCertificateForTest(t, opts, certPEM, keyPEM)

	base := &rest.Config{
		Host:        "https://example.test",
		BearerToken: "bootstrap-token",
		TLSClientConfig: rest.TLSClientConfig{
			CAData: certPEM,
		},
	}

	provider, err := NewRESTConfigProvider(context.Background(), base, "node-a", opts)
	require.NoError(t, err)
	cfg := provider.RESTConfig()
	assert.Equal(t, base.Host, cfg.Host)
	assert.Empty(t, cfg.BearerToken)
	assert.NotNil(t, cfg.Transport)
	assert.Empty(t, cfg.CertFile)
	assert.Empty(t, cfg.KeyFile)
	assert.Empty(t, cfg.CAData)
}

func TestCertificateStore(t *testing.T) {
	opts := testControllerCertificateOptions(t)
	certPEM, keyPEM := testCertificate(t, "first")
	storeControllerCertificateForTest(t, opts, certPEM, keyPEM)

	store, err := newCertificateStore(opts)
	require.NoError(t, err)
	cert, err := store.Current()
	require.NoError(t, err)
	assert.Equal(t, "first", cert.Leaf.Subject.CommonName)
	assert.FileExists(t, store.CurrentPath())
}

func TestValidateRejectsReservedDaemonGroup(t *testing.T) {
	for _, group := range []string{"system:masters", "masters", "cluster-admin"} {
		t.Run(group, func(t *testing.T) {
			opts := testControllerCertificateOptions(t)
			opts.DaemonGroup = group

			err := opts.validate()
			require.ErrorContains(t, err, "reserved or privileged")
		})
	}
}

func storeControllerCertificateForTest(t *testing.T, opts ControllerCertificateOptions, certPEM []byte, keyPEM []byte) {
	t.Helper()

	store, err := newCertificateStore(opts)
	require.NoError(t, err)
	_, err = store.Update(certPEM, keyPEM)
	require.NoError(t, err)
}

func testControllerCertificateOptions(t *testing.T) ControllerCertificateOptions {
	t.Helper()

	return DefaultControllerCertificateOptions(t.TempDir())
}

func testCertificate(t *testing.T, commonName string) ([]byte, []byte) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject: pkix.Name{
			CommonName: commonName,
		},
		NotBefore: time.Now().Add(-time.Minute),
		NotAfter:  time.Now().Add(time.Hour),
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}
