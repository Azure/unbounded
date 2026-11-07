// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nodestart

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestLocalDNSReady(t *testing.T) {
	t.Parallel()

	var requests int

	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++

		if request.URL.Path != "/ready" {
			t.Fatalf("request path = %q, want /ready", request.URL.Path)
		}

		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(strings.NewReader("OK")),
		}, nil
	})}

	if err := localDNSReady(context.Background(), client, []string{"169.254.10.10", "169.254.10.11"}); err != nil {
		t.Fatalf("localDNSReady() error = %v", err)
	}

	if requests != 2 {
		t.Fatalf("request count = %d, want 2", requests)
	}
}

func TestLocalDNSReadyRejectsFailureStatus(t *testing.T) {
	t.Parallel()

	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Status:     "503 Service Unavailable",
			Body:       io.NopCloser(strings.NewReader("not ready")),
		}, nil
	})}

	err := localDNSReady(context.Background(), client, []string{"169.254.10.10"})
	if err == nil || !strings.Contains(err.Error(), "503 Service Unavailable") {
		t.Fatalf("localDNSReady() error = %v", err)
	}
}

// Restarting the network unit restarts the node units that require it, so it
// is restarted only when its files changed.
func TestSetupLocalDNSNetworkRestartsOnlyWhenChanged(t *testing.T) {
	t.Parallel()

	gs := &goalstates.NodeStart{LocalDNS: goalstates.LocalDNS{Enabled: true}}

	for _, changed := range []bool{false, true} {
		var calls []string

		task := &setupLocalDNSNetwork{
			log:       slog.New(slog.DiscardHandler),
			goalState: gs,
			write:     func(*goalstates.NodeStart) (bool, error) { return changed, nil },
			systemctl: func(_ context.Context, args ...string) error {
				calls = append(calls, strings.Join(args, " "))
				return nil
			},
		}

		require.NoError(t, task.Do(context.Background()))

		if changed {
			require.Equal(t, []string{"daemon-reload", "restart " + goalstates.LocalDNSNetworkUnit}, calls)
		} else {
			require.Empty(t, calls, "unchanged files restart nothing")
		}
	}

	failing := &setupLocalDNSNetwork{
		log:       slog.New(slog.DiscardHandler),
		goalState: gs,
		write:     func(*goalstates.NodeStart) (bool, error) { return false, errors.New("read-only /etc") },
	}
	require.ErrorContains(t, failing.Do(context.Background()), "read-only /etc")
}
