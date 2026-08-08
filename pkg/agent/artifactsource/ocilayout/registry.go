// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package ocilayout

import (
	"context"
	"net/http"
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
	if ociutil.RetryableNetworkError(err) {
		return true, nil
	}

	if resp == nil {
		return false, nil
	}

	return retry.DefaultPredicate(resp, nil)
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

func retryOCIPullOperation(
	ctx context.Context,
	pull func() error,
	wait func(context.Context, time.Duration) error,
) error {
	delay := ociPullRetryDelay

	for attempt := 1; attempt <= ociPullMaxAttempts; attempt++ {
		if err := context.Cause(ctx); err != nil {
			return err
		}

		err := pull()
		if err == nil {
			return nil
		}

		if !ociutil.RetryableNetworkError(err) || attempt == ociPullMaxAttempts {
			return err
		}

		if err := wait(ctx, delay); err != nil {
			return err
		}

		delay *= 2
	}

	return nil
}

func waitForOCIPullRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}
