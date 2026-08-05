// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodestart

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
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
