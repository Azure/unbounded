// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package coldstart

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// The fast-failing chair calls are indistinguishable from timeouts in the
// metric unless the transport cause is separated out, and libp2p only reports
// it as opaque error text.
func TestChairCallOutcomeClassifiesTransportFailures(t *testing.T) {
	cases := map[string]string{
		"":                              "ok",
		"context deadline exceeded":     "deadline",
		"context canceled":              "canceled",
		"resource limit exceeded":       "resource_limit",
		"no good addresses":             "no_addresses",
		"connect: connection refused":   "refused",
		"connect: no route to host":     "no_route",
		"stream reset":                  "reset",
		"protocol not supported":        "protocol",
		"failed to dial: all attempts":  "dial",
		"unexpected EOF":                "eof",
		"something entirely unexpected": "error",
	}

	for msg, want := range cases {
		var err error

		switch msg {
		case "":
			err = nil
		case "context deadline exceeded":
			err = fmt.Errorf("please_pull: %w", context.DeadlineExceeded)
		case "context canceled":
			err = fmt.Errorf("please_pull: %w", context.Canceled)
		default:
			err = errors.New(msg)
		}

		if got := chairCallOutcome(err); got != want {
			t.Errorf("chairCallOutcome(%q) = %q, want %q", msg, got, want)
		}
	}
}
