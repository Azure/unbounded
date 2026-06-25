// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package oci

import (
	"errors"
	"net/http"
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

	delay, err := policy.Retry(0, nil, errors.New("lookup registry.example.test: no such host"))
	if err != nil {
		t.Fatalf("policy.Retry returned error: %v", err)
	}

	if delay != ociPullRetryDelay {
		t.Fatalf("retry delay = %v, want %v", delay, ociPullRetryDelay)
	}

	delay, err = policy.Retry(ociPullMaxAttempts-1, nil, errors.New("lookup registry.example.test: no such host"))
	if err != nil {
		t.Fatalf("policy.Retry at max attempts returned error: %v", err)
	}

	if delay >= 0 {
		t.Fatalf("retry delay at max attempts = %v, want negative", delay)
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
