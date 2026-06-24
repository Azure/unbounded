// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodestart

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/Azure/unbounded/pkg/agent/preflight"
)

func TestCheckAPIServerReachableOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/readyz", r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	results := CheckAPIServerReachable(slog.New(slog.DiscardHandler), srv.URL, nil).Check(context.Background())

	assert.Equal(t, preflight.ResultsOK(checkAPIServerReachableName, "cluster API server", "API server is reachable"), results)
}

func TestCheckAPIServerReachableInvalidEndpoint(t *testing.T) {
	results := CheckAPIServerReachable(slog.New(slog.DiscardHandler), "://bad", nil).Check(context.Background())

	assert.Equal(t, preflight.SeverityError, results[0].Severity)
	assert.Equal(t, checkAPIServerReachableName, results[0].Name)
	assert.Equal(t, "cluster API server", results[0].Target)
	assert.Equal(t, "API server endpoint is invalid", results[0].Message)
}

func TestCheckAPIServerReachableRequestFailureIsRedacted(t *testing.T) {
	const endpoint = "https://127.0.0.1:1"

	results := CheckAPIServerReachable(slog.New(slog.DiscardHandler), endpoint, nil).Check(context.Background())

	assert.Equal(t, preflight.SeverityError, results[0].Severity)
	assert.Equal(t, "API server is not reachable", results[0].Message)
	assert.NotContains(t, results[0].Message, endpoint)
}

func TestCheckAPIServerReachableServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	results := CheckAPIServerReachable(slog.New(slog.DiscardHandler), srv.URL, nil).Check(context.Background())

	assert.Equal(t, preflight.SeverityError, results[0].Severity)
	assert.Equal(t, "API server returned status 500", results[0].Message)
}
