// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package registryauth

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/stretchr/testify/require"
)

func brokerFixture(t *testing.T, handler http.Handler) string {
	t.Helper()
	// Relative Unix socket paths avoid host pathname limits while all files
	// remain inside this repository, including in an isolated worktree.
	base := filepath.Join("..", "..", "tmp")
	require.NoError(t, os.MkdirAll(base, 0o700))
	root, err := os.MkdirTemp(base, "br-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })

	listener, err := ListenBroker(root)
	require.NoError(t, err)

	server := &http.Server{Handler: handler}

	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()

	t.Cleanup(func() { require.NoError(t, server.Close()); require.ErrorIs(t, <-done, http.ErrServerClosed) })

	return filepath.Join(root, BrokerSocketPath)
}

func TestBrokerExecRoundTripUsesInfrastructureIdentity(t *testing.T) {
	p := testProvider(t, func(req *http.Request) (int, string) { return 200, `{"refresh_token":"infrastructure-refresh"}` })

	var broker Broker
	broker.Configure(Config{Registries: []string{testRegistry}}, p)

	socket := brokerFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			http.Error(w, "read failed", 500)

			return
		}

		if bytes.Contains(data, []byte("application-token")) {
			t.Error("adapter forwarded application identity material")
		}

		r.Body = io.NopCloser(bytes.NewReader(data))
		broker.ServeHTTP(w, r)
	}))
	input := `{"kind":"CredentialProviderRequest","apiVersion":"credentialprovider.kubelet.k8s.io/v1","image":"example.azurecr.io/private/image:v1","serviceAccountToken":"application-token"}`

	var output bytes.Buffer
	require.NoError(t, RunExec(t.Context(), strings.NewReader(input), &output, socket, []string{testRegistry}))

	var result ProviderResponse
	require.NoError(t, json.Unmarshal(output.Bytes(), &result))
	require.Equal(t, "infrastructure-refresh", result.Auth[testRegistry].Password)
	require.Equal(t, tokenUsername, result.Auth[testRegistry].Username)
}

func TestBrokerRejectsUnconfiguredAndMalformedRequests(t *testing.T) {
	var broker Broker

	request := httptest.NewRequest(http.MethodPost, "/credentials", strings.NewReader(`{}`))
	response := httptest.NewRecorder()
	broker.ServeHTTP(response, request)
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	broker.Configure(Config{Registries: []string{testRegistry}}, testProvider(t, func(*http.Request) (int, string) { t.Error("malformed request reached Azure"); return 500, "" }))

	for _, body := range []string{`{}`, `{"kind":"CredentialProviderRequest","apiVersion":"unsupported"}`, strings.Repeat("x", maxResponseBytes+1)} {
		response = httptest.NewRecorder()
		broker.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/credentials", strings.NewReader(body)))
		require.Equal(t, http.StatusServiceUnavailable, response.Code)
	}

	response = httptest.NewRecorder()
	broker.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/credentials", nil))
	require.Equal(t, http.StatusMethodNotAllowed, response.Code)
}

func TestExecDoesNotLeakBrokerErrorsOrAcceptBadResponses(t *testing.T) {
	input := `{"kind":"CredentialProviderRequest","apiVersion":"credentialprovider.kubelet.k8s.io/v1","image":"example.azurecr.io/repo:v1"}`

	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"failure", "private-broker-token", 503},
		{"wrong protocol", `{"kind":"Other","apiVersion":"v1","token":"private-broker-token"}`, 200},
		{"oversized", strings.Repeat("private-broker-token", maxResponseBytes), 200},
		{"wrong destination", `{"kind":"CredentialProviderResponse","apiVersion":"credentialprovider.kubelet.k8s.io/v1","auth":{"other.azurecr.io":{"password":"private-broker-token"}}}`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socket := brokerFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})) //nolint:errcheck // Test responses may be truncated when the client enforces its limit.

			var output bytes.Buffer

			err := RunExec(t.Context(), strings.NewReader(input), &output, socket, []string{testRegistry})
			require.Error(t, err)
			require.Empty(t, output.String())
			require.NotContains(t, err.Error(), "private-broker-token")
		})
	}
}

func TestBrokerRuntimeOwnershipAndPermissions(t *testing.T) {
	base := filepath.Join("..", "..", "tmp")
	require.NoError(t, os.MkdirAll(base, 0o700))
	root, err := os.MkdirTemp(base, "br-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })

	listener, err := ListenBroker(root)
	require.NoError(t, err)
	t.Cleanup(func() { closeQuietly(listener) })

	info, err := os.Stat(filepath.Join(root, BrokerSocketPath))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	info, err = os.Stat(filepath.Dir(filepath.Join(root, BrokerSocketPath)))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())

	_, err = ListenBroker(root)
	require.ErrorContains(t, err, "already listening")
	listener.(*net.UnixListener).SetUnlinkOnClose(false)
	require.NoError(t, listener.Close())

	replacement, err := ListenBroker(root)
	require.NoError(t, err, "a marked stale socket must recover after broker replacement")
	require.NoError(t, replacement.Close())
}

func TestBrokerRefusesForeignFilesAndSymlinks(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(map[bool]string{false: "foreign file", true: "symlink"}[symlink], func(t *testing.T) {
			root := t.TempDir()
			socket := filepath.Join(root, BrokerSocketPath)
			require.NoError(t, os.MkdirAll(filepath.Dir(socket), 0o700))

			if symlink {
				require.NoError(t, os.Symlink("foreign", socket))
			} else {
				require.NoError(t, os.WriteFile(socket, []byte("preserve"), 0o600))
			}

			_, err := ListenBroker(root)
			require.Error(t, err)
			_, err = os.Lstat(socket)
			require.NoError(t, err)
		})
	}
}

func TestBrokerRefusesOwnerMarkerSymlink(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Dir(filepath.Join(root, BrokerSocketPath))
	require.NoError(t, os.MkdirAll(directory, 0o700))

	target := filepath.Join(root, "foreign-owner")
	require.NoError(t, os.WriteFile(target, []byte(managedMarker), 0o600))
	require.NoError(t, os.Symlink(target, filepath.Join(directory, ".owner")))

	_, err := ListenBroker(root)
	require.ErrorContains(t, err, "symlink")
	require.NoFileExists(t, filepath.Join(root, BrokerSocketPath))
}

func TestProviderRequiresExplicitFederationWithoutIMDSFallback(t *testing.T) {
	for _, cfg := range []Config{{}, {ClientID: testClientID}, {TenantID: testTenantID}} {
		_, err := NewProvider(cfg, "/projected/token")
		require.Error(t, err)
	}

	p, err := NewProvider(Config{ClientID: testClientID, TenantID: testTenantID}, "/projected/token")
	require.NoError(t, err)
	require.IsType(t, &azidentity.WorkloadIdentityCredential{}, p.Credential)
}
