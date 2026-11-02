// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package transfer

import (
	"os"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// TestMain installs the W3C TraceContext propagator for the whole package.
//
// Production code gets the propagator from tracing.Init, which every agent
// runs at startup. Tests construct servers and clients directly and never
// call Init, and otelhttp resolves the propagator from the global default
// (otel.GetTextMapPropagator), which is an empty composite in a bare process.
// Without this, traceparent is neither injected nor extracted and the
// propagation assertions in tracing_test.go would be vacuous.
func TestMain(m *testing.M) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	os.Exit(m.Run())
}
