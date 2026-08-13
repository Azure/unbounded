// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodestart

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

func TestCheckAPIServerReachableOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/readyz", r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	results := CheckAPIServerReachable(slog.New(slog.DiscardHandler), apiServerPreflightConfig(srv.URL)).Check(context.Background())

	assert.Equal(t, preflight.ResultsOK(checkAPIServerReachableName, "cluster API server", "API server is reachable"), results)
}

func TestCheckAPIServerReachableInvalidEndpoint(t *testing.T) {
	results := CheckAPIServerReachable(slog.New(slog.DiscardHandler), apiServerPreflightConfig("://bad")).Check(context.Background())

	assert.Equal(t, preflight.SeverityError, results[0].Severity)
	assert.Equal(t, checkAPIServerReachableName, results[0].Name)
	assert.Equal(t, "cluster API server", results[0].Target)
	assert.Equal(t, "API server endpoint is invalid", results[0].Message)
}

func TestCheckAPIServerReachableRequestFailureIncludesEndpointAndNetworkError(t *testing.T) {
	const endpoint = "https://api.example.com:6443"

	networkErr := errors.New("DNS resolution failed")
	checker := apiServerReachableChecker{
		config: apiServerPreflightConfig(endpoint),
		httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, networkErr
		})},
	}

	results := checker.Check(context.Background())

	assert.Equal(t, preflight.SeverityError, results[0].Severity)
	assert.Equal(t, `API server "https://api.example.com:6443" is not reachable: DNS resolution failed`, results[0].Message)
}

func TestCheckAPIServerReachableServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	results := CheckAPIServerReachable(slog.New(slog.DiscardHandler), apiServerPreflightConfig(srv.URL)).Check(context.Background())

	assert.Equal(t, preflight.SeverityError, results[0].Severity)
	assert.Equal(t, "API server returned status 500", results[0].Message)
}

func TestCheckAPIServerReachableDoesNotRequireBootstrapCredential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cfg := apiServerPreflightConfig(srv.URL)
	cfg.Kubelet.Auth.BootstrapToken = ""

	results := CheckAPIServerReachable(slog.New(slog.DiscardHandler), cfg).Check(context.Background())

	assert.Equal(t, preflight.SeverityOK, results[0].Severity)
}

func TestCheckAPIServerReachableInvalidCA(t *testing.T) {
	cfg := apiServerPreflightConfig("https://api.example.com:443")
	cfg.Cluster.CaCertBase64 = "not-base64"

	results := CheckAPIServerReachable(slog.New(slog.DiscardHandler), cfg).Check(context.Background())

	assert.Equal(t, preflight.SeverityError, results[0].Severity)
	assert.Contains(t, results[0].Message, "cluster CA data is invalid")
}

func apiServerPreflightConfig(apiServer string) config.AgentConfig {
	return config.AgentConfig{
		Cluster: config.AgentClusterConfig{
			CaCertBase64: "Y2E=",
		},
		Kubelet: config.AgentKubeletConfig{
			ApiServer: apiServer,
			Auth: config.KubeletAuthInfo{
				BootstrapToken: "abc123.secret456",
			},
		},
	}
}
