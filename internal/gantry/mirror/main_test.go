// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror_test

import (
	"os"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// TestMain installs the W3C TraceContext propagator for the whole package.
//
// Production gets the propagator from tracing.Init, which every agent runs at
// startup. These tests build handlers directly and never call Init, and
// otelhttp resolves the propagator from otel.GetTextMapPropagator, which is an
// empty composite in a bare process. Without this, an inbound traceparent is
// never extracted and the propagation assertion in tracing_test.go is vacuous.
func TestMain(m *testing.M) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	os.Exit(m.Run())
}
