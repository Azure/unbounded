// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/smithy-go/logging"

	"github.com/Azure/unbounded/internal/racer/object"
	"github.com/Azure/unbounded/pkg/racersdk"
)

func useTestLogLimiter(t *testing.T, now func() time.Time) {
	t.Helper()

	previous := diagnosticLimiter
	diagnosticLimiter = newLogLimiter(2, time.Minute, now)

	t.Cleanup(func() { diagnosticLimiter = previous })
}

func setDiagnosticDebug(t *testing.T, debug bool) {
	t.Helper()

	previous := diagnosticDebug.Load()
	diagnosticDebug.Store(debug)

	t.Cleanup(func() { diagnosticDebug.Store(previous) })
}

func TestLogLimiter(t *testing.T) {
	now := time.Unix(0, 0)
	limiter := newLogLimiter(2, time.Minute, func() time.Time { return now })

	for i, want := range []bool{true, true, false, false} {
		if allowed, suppressed := limiter.allow("a"); allowed != want || suppressed != 0 {
			t.Fatalf("call %d = %v/%d", i, allowed, suppressed)
		}
	}

	if allowed, _ := limiter.allow("b"); !allowed {
		t.Fatal("keys share a budget")
	}

	now = now.Add(time.Minute)

	if allowed, suppressed := limiter.allow("a"); !allowed || suppressed != 2 {
		t.Fatalf("after window = %v/%d, want true/2", allowed, suppressed)
	}

	if allowed, suppressed := limiter.allow("a"); !allowed || suppressed != 0 {
		t.Fatalf("suppressed count repeated: %v/%d", allowed, suppressed)
	}
}

func TestOriginExpectedOutcomesLevel(t *testing.T) {
	useTestLogLimiter(t, time.Now)

	for _, cause := range []error{racersdk.ErrNotFound, racersdk.ErrForbidden, racersdk.ErrVersionMismatch, context.Canceled, racersdk.ErrInvalidRequest} {
		for _, debug := range []bool{false, true} {
			t.Run(fmt.Sprintf("%v/%v", cause, debug), func(t *testing.T) {
				output := captureDiagnosticLog(t)
				setDiagnosticDebug(t, debug)

				origin := diagnoseOrigin(func(context.Context, racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
					return racersdk.Metadata{}, nil, fmt.Errorf("%s: %w", diagnosticSecret, cause)
				})
				_, _, _ = origin(context.Background(), racersdk.OriginRequest{}) //nolint:errcheck // Only the log matters.

				got := output.String()
				if !debug && got != "" {
					t.Fatalf("expected outcome logged at info: %q", got)
				}

				if debug && (!strings.Contains(got, "level=debug") || strings.Contains(got, "level=warn")) {
					t.Fatalf("expected outcome not logged at debug: %q", got)
				}

				if strings.Contains(got, diagnosticSecret) {
					t.Fatal("unsafe diagnostic")
				}
			})
		}
	}
}

func TestOutcomeRateLimit(t *testing.T) {
	now := time.Unix(0, 0)

	useTestLogLimiter(t, func() time.Time { return now })
	setDiagnosticDebug(t, false)

	output := captureDiagnosticLog(t)
	failure := object.SidecarFailure{Method: "GET", Status: 503, Bucket: "b", Err: racersdk.ErrUnavailable}

	for range 5 {
		logSidecarFailure(failure)
	}

	logSidecarFailure(object.SidecarFailure{Method: "GET", Status: 502, Bucket: "b", Err: object.ErrMetadataMismatch})

	if got := strings.Count(output.String(), "category=unavailable"); got != 2 {
		t.Fatalf("unavailable lines = %d, want 2: %q", got, output.String())
	}

	if !strings.Contains(output.String(), "category=mismatch") {
		t.Fatal("other category was rate limited")
	}

	now = now.Add(time.Minute)

	logSidecarFailure(failure)

	if !strings.Contains(output.String(), "category=unavailable bucket=\"b\" suppressed=3 level=warn") {
		t.Fatalf("missing suppressed summary: %q", output.String())
	}
}

func TestSidecarVersionMismatchIsDebug(t *testing.T) {
	useTestLogLimiter(t, time.Now)
	setDiagnosticDebug(t, false)

	output := captureDiagnosticLog(t)

	logSidecarFailure(object.SidecarFailure{Method: "GET", Status: 503, Bucket: "b", Err: racersdk.ErrVersionMismatch})

	if output.String() != "" {
		t.Fatalf("version mismatch logged at info: %q", output.String())
	}
}

func TestAWSLogger(t *testing.T) {
	for _, debug := range []bool{false, true} {
		t.Run(fmt.Sprint(debug), func(t *testing.T) {
			output := captureDiagnosticLog(t)
			logger := awsLogger(debug)

			logger.Logf(logging.Warn, "retry %s", "quota\nlevel=forged")
			logger.Logf(logging.Debug, "checksum skipped")
			logger.Logf(logging.Classification("OTHER"), "ignored")

			got := output.String()
			if !strings.Contains(got, `event=aws_sdk level=warn message="retry quota\nlevel=forged"`) {
				t.Fatalf("warning missing or unquoted: %q", got)
			}

			if strings.Contains(got, "checksum skipped") != debug || strings.Contains(got, "ignored") {
				t.Fatalf("debug=%v filtering wrong: %q", debug, got)
			}
		})
	}
}

func TestAWSLogMode(t *testing.T) {
	if awsLogMode(false) != 0 {
		t.Fatal("info level enabled SDK client logs")
	}

	mode := awsLogMode(true)
	if !mode.IsRetries() || mode.IsRequest() || mode.IsRequestWithBody() || mode.IsResponse() || mode.IsResponseWithBody() || mode.IsSigning() {
		t.Fatalf("debug log mode %v exposes requests or signing", mode)
	}
}

func TestOriginAWSConfigLogging(t *testing.T) {
	for _, level := range []string{logLevelInfo, logLevelDebug} {
		cfg, err := originAWSConfig(t.Context(), options{region: "us-east-1", requestTimeout: time.Second, logLevel: level})
		if err != nil {
			t.Fatal(err)
		}

		if cfg.Logger == nil || cfg.ClientLogMode != awsLogMode(level == logLevelDebug) {
			t.Fatalf("%s: logger %v mode %v", level, cfg.Logger, cfg.ClientLogMode)
		}
	}
}
