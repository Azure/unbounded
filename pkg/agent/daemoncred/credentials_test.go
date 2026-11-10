// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func storeControllerCertificateForTest(
	t *testing.T,
	opts ControllerCertificateOptions,
	certPEM []byte,
	keyPEM []byte,
) {
	t.Helper()

	store, err := newCertificateStore(opts)
	require.NoError(t, err)
	_, err = store.Update(certPEM, keyPEM)
	require.NoError(t, err)
}

func testControllerCertificateOptions(t *testing.T) ControllerCertificateOptions {
	t.Helper()

	return ControllerCertificateOptions{
		Name:          "test-daemon-controller",
		SignerName:    DefaultControllerCertificateSignerName,
		DaemonGroup:   testDaemonGroup,
		CredentialDir: t.TempDir(),
	}
}

const testDaemonGroup = "test-daemon-group"

func testCertificate(t *testing.T, commonName string) ([]byte, []byte) {
	t.Helper()

	return testCertificateValidUntil(t, commonName, time.Now().Add(time.Hour))
}

func testCertificateValidUntil(t *testing.T, commonName string, notAfter time.Time) ([]byte, []byte) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject: pkix.Name{
			CommonName: commonName,
		},
		NotBefore: notAfter.Add(-2 * time.Hour),
		NotAfter:  notAfter,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func TestStoredRESTConfig(t *testing.T) {
	validCert, validKey := testCertificate(t, "valid")
	expiredCert, expiredKey := testCertificateValidUntil(t, "expired", time.Now().Add(-time.Minute))

	for _, tc := range []struct {
		name      string
		store     func(t *testing.T, opts ControllerCertificateOptions)
		wantFound bool
		wantErr   bool
	}{
		{
			name: "stored valid certificate",
			store: func(t *testing.T, opts ControllerCertificateOptions) {
				storeControllerCertificateForTest(t, opts, validCert, validKey)
			},
			wantFound: true,
		},
		{
			name:  "empty directory",
			store: func(*testing.T, ControllerCertificateOptions) {},
		},
		{
			name: "missing directory",
			store: func(t *testing.T, opts ControllerCertificateOptions) {
				require.NoError(t, os.Remove(opts.CredentialDir))
			},
		},
		{
			name: "expired certificate",
			store: func(t *testing.T, opts ControllerCertificateOptions) {
				storeControllerCertificateForTest(t, opts, expiredCert, expiredKey)
			},
		},
		{
			name: "corrupt certificate file",
			store: func(t *testing.T, opts ControllerCertificateOptions) {
				path := filepath.Join(opts.CredentialDir, controllerCertificateStorePrefix+"-current.pem")
				require.NoError(t, os.WriteFile(path, []byte("not a certificate"), 0o600))
			},
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				t.Errorf("unexpected request to the API server: %s %s", r.Method, r.URL.Path)
			}))
			defer server.Close()

			opts := testControllerCertificateOptions(t)
			tc.store(t, opts)

			base := &rest.Config{
				Host:            server.URL,
				BearerToken:     "bootstrap-token",
				TLSClientConfig: rest.TLSClientConfig{CAData: validCert},
			}

			cfg, found, err := StoredRESTConfig(base, "node-a", opts)
			if tc.wantErr {
				require.Error(t, err)
				assert.False(t, found)
				assert.Nil(t, cfg)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.wantFound, found)

			if !tc.wantFound {
				assert.Nil(t, cfg)

				return
			}

			require.NotNil(t, cfg)
			assert.Equal(t, server.URL, cfg.Host)
			assert.Empty(t, cfg.BearerToken)
			assert.NotNil(t, cfg.Transport)
		})
	}
}

func TestStoredRESTConfigValidatesOptions(t *testing.T) {
	opts := testControllerCertificateOptions(t)
	opts.DaemonGroup = ""

	_, found, err := StoredRESTConfig(&rest.Config{Host: "https://example.test"}, "node-a", opts)
	require.ErrorContains(t, err, "daemon group is required")
	assert.False(t, found)
}
