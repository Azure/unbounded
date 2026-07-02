// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package oci

import (
	"context"
	"errors"
	"net"
	"net/http"
	"syscall"
	"testing"
	"time"

	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"
)

func TestConfigureOCIPullRetryUsesORASRetryClient(t *testing.T) {
	repo := &remote.Repository{}
	configureOCIPullRetry(repo)

	authClient, ok := repo.Client.(*auth.Client)
	if !ok {
		t.Fatalf("repo.Client = %T, want *auth.Client", repo.Client)
	}

	transport, ok := authClient.Client.Transport.(*retry.Transport)
	if !ok {
		t.Fatalf("auth client transport = %T, want *retry.Transport", authClient.Client.Transport)
	}

	if transport.Policy == nil {
		t.Fatal("retry transport policy is nil")
	}
}

func TestOCIPullRetryPolicyRetriesTransportErrors(t *testing.T) {
	policy := newOCIPullRetryPolicy()

	delay, err := policy.Retry(0, nil, &net.DNSError{
		Err:        "no such host",
		Name:       "registry.example.test",
		IsNotFound: true,
	})
	if err != nil {
		t.Fatalf("policy.Retry returned error: %v", err)
	}

	if delay != ociPullRetryDelay {
		t.Fatalf("retry delay = %v, want %v", delay, ociPullRetryDelay)
	}

	delay, err = policy.Retry(0, nil, &net.OpError{
		Op:  "dial",
		Err: syscall.ECONNREFUSED,
	})
	if err != nil {
		t.Fatalf("policy.Retry for connection refused returned error: %v", err)
	}

	if delay != ociPullRetryDelay {
		t.Fatalf("retry delay for connection refused = %v, want %v", delay, ociPullRetryDelay)
	}

	delay, err = policy.Retry(ociPullMaxAttempts-1, nil, &net.DNSError{
		Err:        "no such host",
		Name:       "registry.example.test",
		IsNotFound: true,
	})
	if err != nil {
		t.Fatalf("policy.Retry at max attempts returned error: %v", err)
	}

	if delay >= 0 {
		t.Fatalf("retry delay at max attempts = %v, want negative", delay)
	}
}

func TestOCIPullRetryPolicySkipsNonNetworkTransportErrors(t *testing.T) {
	policy := newOCIPullRetryPolicy()

	delay, err := policy.Retry(0, nil, errors.New("tls: failed to verify certificate"))
	if err != nil {
		t.Fatalf("policy.Retry for TLS error returned error: %v", err)
	}

	if delay >= 0 {
		t.Fatalf("retry delay for TLS error = %v, want negative", delay)
	}

	delay, err = policy.Retry(0, nil, context.Canceled)
	if err != nil {
		t.Fatalf("policy.Retry for context cancellation returned error: %v", err)
	}

	if delay >= 0 {
		t.Fatalf("retry delay for context cancellation = %v, want negative", delay)
	}
}

func TestOCIPullRetryPolicyUsesORASStatusPredicate(t *testing.T) {
	policy := newOCIPullRetryPolicy()

	delay, err := policy.Retry(0, &http.Response{StatusCode: http.StatusServiceUnavailable}, nil)
	if err != nil {
		t.Fatalf("policy.Retry for 503 returned error: %v", err)
	}

	if delay != ociPullRetryDelay {
		t.Fatalf("retry delay for 503 = %v, want %v", delay, ociPullRetryDelay)
	}

	delay, err = policy.Retry(0, &http.Response{StatusCode: http.StatusNotFound}, nil)
	if err != nil {
		t.Fatalf("policy.Retry for 404 returned error: %v", err)
	}

	if delay >= 0 {
		t.Fatalf("retry delay for 404 = %v, want negative", delay)
	}
}

func TestMaxOCIPullRetryDelay(t *testing.T) {
	if got, want := maxOCIPullRetryDelay(), 16*time.Second; got != want {
		t.Fatalf("maxOCIPullRetryDelay() = %v, want %v", got, want)
	}
}

func TestParseOCILayoutReference(t *testing.T) {
	tests := []struct {
		name       string
		image      string
		wantLayout string
		wantTag    string
		wantOK     bool
		wantErr    bool
	}{
		{
			name:       "tagged layout",
			image:      "oci-layout:///opt/unbounded/images/agent-ubuntu2404:v20260619",
			wantLayout: "/opt/unbounded/images/agent-ubuntu2404",
			wantTag:    "v20260619",
			wantOK:     true,
		},
		{
			name:       "default latest",
			image:      "oci-layout:///opt/unbounded/images/agent-ubuntu2404",
			wantLayout: "/opt/unbounded/images/agent-ubuntu2404",
			wantTag:    "latest",
			wantOK:     true,
		},
		{
			name:   "not layout",
			image:  "ghcr.io/azure/agent-ubuntu2404:v20260619",
			wantOK: false,
		},
		{
			name:    "missing source",
			image:   "oci-layout://",
			wantOK:  true,
			wantErr: true,
		},
		{
			name:    "empty tag",
			image:   "oci-layout:///opt/unbounded/images/agent-ubuntu2404:",
			wantOK:  true,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotLayout, gotTag, gotOK, err := parseOCILayoutReference(tt.image)
			if gotOK != tt.wantOK {
				t.Fatalf("ok = %v, want %v", gotOK, tt.wantOK)
			}
			if tt.wantErr {
				if err == nil {
					t.Fatal("err = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if gotLayout != tt.wantLayout {
				t.Fatalf("layout = %q, want %q", gotLayout, tt.wantLayout)
			}
			if gotTag != tt.wantTag {
				t.Fatalf("tag = %q, want %q", gotTag, tt.wantTag)
			}
		})
	}
}
