// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package ocilayout

import (
	"context"
	"errors"
	"net"
	"net/http"
	"syscall"
	"time"

	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/Azure/unbounded/internal/ociutil"
)

const (
	ociPullMaxAttempts = 5
	ociPullRetryDelay  = 2 * time.Second
)

func newRemoteRepository(ref string) (*remote.Repository, error) {
	repo, err := remote.NewRepository(ref)
	if err != nil {
		return nil, err
	}

	ociutil.ConfigurePlainHTTP(repo)
	configureOCIPullRetry(repo)

	return repo, nil
}

func configureOCIPullRetry(repo *remote.Repository) {
	repo.Client = &auth.Client{
		Client: &http.Client{
			Transport: &retry.Transport{
				Policy: func() retry.Policy {
					return newOCIPullRetryPolicy()
				},
			},
		},
		Header: auth.DefaultClient.Header.Clone(),
		Cache:  auth.DefaultCache,
	}
}

func newOCIPullRetryPolicy() retry.Policy {
	return &retry.GenericPolicy{
		Retryable: retryOCIPullFailure,
		Backoff:   ociPullBackoff,
		MinWait:   ociPullRetryDelay,
		MaxWait:   maxOCIPullRetryDelay(),
		MaxRetry:  ociPullMaxAttempts - 1,
	}
}

func retryOCIPullFailure(resp *http.Response, err error) (bool, error) {
	if retryableOCIPullTransportError(err) {
		return true, nil
	}

	if resp == nil {
		return false, nil
	}

	return retry.DefaultPredicate(resp, nil)
}

func retryableOCIPullTransportError(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	for _, target := range []error{
		syscall.ECONNABORTED,
		syscall.ECONNREFUSED,
		syscall.ECONNRESET,
		syscall.EHOSTUNREACH,
		syscall.ENETUNREACH,
	} {
		if errors.Is(err, target) {
			return true
		}
	}

	return false
}

func ociPullBackoff(attempt int, _ *http.Response) time.Duration {
	delay := ociPullRetryDelay
	for range attempt {
		delay *= 2
	}

	return delay
}

func maxOCIPullRetryDelay() time.Duration {
	delay := ociPullRetryDelay
	for range ociPullMaxAttempts - 2 {
		delay *= 2
	}

	return delay
}
