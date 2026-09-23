// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nodestart

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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

// TestLocalDNSNetworkUnitExecutesTheResolvedHelper pins the agreement between
// the unit and the script it runs.
//
// The helper is written under the installation prefix and the unit is the only
// thing that executes it. When the unit carried a fixed path, a host with a
// prefix got a unit pointing into a directory the script was never written to,
// and the failure only appears when systemd runs the unit during node start.
func TestLocalDNSNetworkUnitExecutesTheResolvedHelper(t *testing.T) {
	t.Parallel()

	const helper = "/opt/unbounded/libexec/unbounded-localdns-network"

	var unit bytes.Buffer
	require.NoError(t, assetsTemplate.ExecuteTemplate(&unit, "unbounded-localdns-network.service", map[string]string{
		"MachineName":       "kube1",
		"NodeListenerIP":    "169.254.10.10",
		"ClusterListenerIP": "169.254.10.11",
		"NetworkHelper":     helper,
	}))

	rendered := unit.String()
	require.Contains(t, rendered, "ExecStart="+helper)
	require.NotContains(t, rendered, "/usr/local/libexec",
		"the unit must not carry a path from the default prefix")
	require.NotContains(t, rendered, "{{", "template must be fully resolved")
}
