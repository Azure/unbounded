// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// TestReportCdsubExit pins what the shutdown drain says about cdsub.Run's
// result. The ordinary results are the cancellation shutdown caused and the
// zero value read after runAgent already reported a crash; reporting either
// would put a spurious warning on every shutdown. Anything else is news.
func TestReportCdsubExit(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		wantLog bool
	}{
		{name: "nothing to report", err: nil},
		{name: "canceled by shutdown", err: context.Canceled},
		{name: "canceled, wrapped", err: fmt.Errorf("run: %w", context.Canceled)},
		{name: "deadline", err: context.DeadlineExceeded},
		{name: "an unexpected error", err: errors.New("containerd connection lost"), wantLog: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer

			reportCdsubExit(slog.New(slog.NewTextHandler(&out, nil)), tc.err)

			logged := strings.Contains(out.String(), "cdsub exited with an error while draining")
			if logged != tc.wantLog {
				t.Fatalf("logged = %v, want %v; output:\n%s", logged, tc.wantLog, out.String())
			}

			if tc.wantLog && !strings.Contains(out.String(), tc.err.Error()) {
				t.Errorf("log does not carry the error:\n%s", out.String())
			}
		})
	}
}
