// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gh

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-github/v75/github"
)

// apiError builds the error go-github returns for a status code.
func apiError(code int) error {
	return &github.ErrorResponse{
		Response: &http.Response{StatusCode: code},
		Message:  http.StatusText(code),
	}
}

func TestTransient(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil is not an error at all", err: nil, want: false},

		// GitHub answered, and said the fault was theirs.
		{name: "500", err: apiError(http.StatusInternalServerError), want: true},
		{name: "502", err: apiError(http.StatusBadGateway), want: true},
		{name: "503", err: apiError(http.StatusServiceUnavailable), want: true},

		// GitHub answered, and will say the same thing in ninety minutes.
		{name: "401", err: apiError(http.StatusUnauthorized), want: false},
		{name: "403", err: apiError(http.StatusForbidden), want: false},
		{name: "404", err: apiError(http.StatusNotFound), want: false},
		{name: "422", err: apiError(http.StatusUnprocessableEntity), want: false},

		// A rate limit is a 403 on the wire. Only the type tells it apart from
		// "you may not do that", which is why this is not a status check.
		{
			name: "rate limit, though it is a 403",
			err:  &github.RateLimitError{Response: &http.Response{StatusCode: http.StatusForbidden}},
			want: true,
		},
		{
			name: "secondary rate limit",
			err: &github.AbuseRateLimitError{
				Response: &http.Response{StatusCode: http.StatusForbidden},
			},
			want: true,
		},

		// Never reached GitHub, so nothing was learned about the request.
		{name: "connection refused", err: &net.OpError{Op: "dial"}, want: true},

		// The caller gave up. Retrying would turn a Ctrl-C into a loop.
		{name: "canceled", err: context.Canceled, want: false},
		{name: "deadline exceeded", err: context.DeadlineExceeded, want: false},

		// Every error this package returns is annotated, so the classification
		// has to reach through the wrapping.
		{
			name: "wrapped 503",
			err:  fmt.Errorf("list runs for release.yaml: %w", apiError(http.StatusServiceUnavailable)),
			want: true,
		},
		{
			name: "wrapped 404",
			err:  fmt.Errorf("get release v0.5.0: %w", apiError(http.StatusNotFound)),
			want: false,
		},
		{
			name: "wrapped cancellation",
			err:  fmt.Errorf("list runs for release.yaml: %w", context.Canceled),
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := Transient(tc.err); got != tc.want {
				t.Errorf("Transient(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestTransientReachesThroughTheWrappersThisPackageAdds checks the real
// annotations rather than hand-built ones.
//
// Every function here wraps its cause with %w, and Transient depends on that.
// A wrapper that ever switched to %v would still compile, still read fine, and
// would silently make a watch give up on the first 502 - so the property is
// asserted against the actual call paths.
func TestTransientReachesThroughTheWrappersThisPackageAdds(t *testing.T) {
	t.Parallel()

	// Everything 503s, so each call below fails inside its own wrapper.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)

		if _, err := w.Write([]byte(`{"message":"Service Unavailable"}`)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	client, err := New(t.Context(), Options{
		Token:   func(context.Context) (string, error) { return "t", nil },
		BaseURL: server.URL + "/",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	calls := map[string]func() error{
		"Runs": func() error {
			_, err := client.Runs(t.Context(), ListRuns{Workflow: WorkflowRelease})

			return err
		},
		"Prepares": func() error {
			_, err := client.Prepares(t.Context())

			return err
		},
		"Progress": func() error {
			_, err := client.Progress(t.Context(), "v0.5.0")

			return err
		},
		"Release": func() error {
			_, err := client.Release(t.Context(), "v0.5.0")

			return err
		},
	}

	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := call()
			if err == nil {
				t.Fatalf("%s: expected an error", name)
			}

			if !Transient(err) {
				t.Errorf("%s error is not classified as transient, so a watch "+
					"would give up on it: %v", name, err)
			}
		})
	}
}
