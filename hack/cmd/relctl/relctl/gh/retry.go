// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gh

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/go-github/v75/github"
)

// Transient reports whether an error is worth waiting out rather than
// reporting.
//
// For a command that polls, the distinction is the whole difference between
// riding out a bad minute and spinning for the full timeout on a credential
// that will never work. It is drawn on go-github's typed errors rather than on
// status codes, because GitHub answers a rate limit with 403 - the same code it
// uses for "you may not do that" - and only the type tells them apart.
//
// Every error this package returns wraps the original with %w, so errors.As
// reaches the cause through the annotation.
func Transient(err error) bool {
	if err == nil {
		return false
	}

	// Cancellation and deadlines come back through the same return as an API
	// failure and would otherwise look like a network blip, turning a Ctrl-C
	// into a retry loop. Checked first for that reason.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	// Both rate limits are the definition of temporary: the same request
	// succeeds once the window rolls over.
	var (
		rateLimit *github.RateLimitError
		abuse     *github.AbuseRateLimitError
	)

	if errors.As(err, &rateLimit) || errors.As(err, &abuse) {
		return true
	}

	// A definite answer from GitHub. Only a fault on their side is worth
	// waiting out: 401, 403 and 404 will say exactly the same thing in ninety
	// minutes, and retrying them turns a clear error into a timeout.
	var response *github.ErrorResponse
	if errors.As(err, &response) {
		return response.Response != nil && response.Response.StatusCode >= http.StatusInternalServerError
	}

	// Never got an answer at all: a timeout, a reset connection, DNS. Nothing
	// was learned about the request, so it is worth making again.
	return true
}
