// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package tracing initializes OpenTelemetry tracing for the Gantry agent so
// that an image pull can be followed end to end.
//
// containerd 2.x already instruments its own pull path and propagates the
// W3C traceparent header on registry HTTP calls (see containerd
// pkg/tracing/tracing.go and plugin/otlp.go). Gantry is the next hop on that
// path, so continuing the incoming context turns
//
//	kubelet PullImage -> containerd PullImage -> fetchLayer/unpackLayer
//	  -> Gantry mirror -> peer transfer endpoint OR upstream registry
//
// into a single trace. Without this package the trace dead-ends at Gantry.
//
// Tracing is OFF by default: nothing is exported and no collector connection
// is opened unless OTEL_TRACES_EXPORTER selects OTLP. An agent running with
// the default DaemonSet env therefore pays no collection cost.
package tracing

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// Scope is the instrumentation scope name carried by every span Gantry
// emits. One scope for the whole agent keeps a trace viewer from splitting a
// single pull across per-package scopes.
const Scope = "github.com/Azure/unbounded/internal/gantry"

// EnvExporter is the standard OTEL variable that selects the trace exporter.
// Gantry exports over OTLP/gRPC only, so any other value leaves tracing off.
// The OTLP/HTTP spelling is deliberately not accepted: silently ignoring a
// mis-set exporter is worse than staying off, and staying off is what happens
// either way.
const EnvExporter = "OTEL_TRACES_EXPORTER"

// EnvServiceName and EnvResourceAttributes are the standard OTEL variables
// that name the service in a collector's service map.
const (
	EnvServiceName        = "OTEL_SERVICE_NAME"
	EnvResourceAttributes = "OTEL_RESOURCE_ATTRIBUTES"
	// EnvBatchDelay is the standard batch-processor schedule delay, in
	// milliseconds per the OTLP environment-variable specification.
	EnvBatchDelay = "OTEL_BSP_SCHEDULE_DELAY"
)

// exporterOTLP is the only accepted value of EnvExporter.
const exporterOTLP = "otlp"

// defaultServiceName is used when the operator sets neither OTEL_SERVICE_NAME
// nor service.name inside OTEL_RESOURCE_ATTRIBUTES.
const defaultServiceName = "gantry"

// defaultBatchDelay matches the SDK's own DefaultScheduleDelay so that an
// unset OTEL_BSP_SCHEDULE_DELAY behaves identically to a stock exporter.
const defaultBatchDelay = 5 * time.Second

// ShutdownTimeout bounds the final span flush at agent exit. Detached from
// the signal context on purpose: a collector that stops responding must not
// keep the agent alive past its termination grace period.
const ShutdownTimeout = 5 * time.Second

// Shutdown flushes buffered spans and closes the exporter. It is a no-op when
// tracing was never enabled.
type Shutdown func(context.Context) error

// disable installs the no-op provider and the W3C TraceContext propagator and
// returns the inert shutdown handle. Every non-exporting path goes through
// here so the instrumentation seams always resolve to a non-recording span.
//
// The propagator is TraceContext even while disabled, and that is load-bearing
// rather than belt-and-braces. otelhttp falls back to the global propagator,
// so installing an empty one here would suppress traceparent injection and
// extraction outright. That breaks two things: the agent cannot forward an
// existing trace when a collector is configured but sampling says skip, and
// OTEL_TRACES_EXPORTER=none becomes an explicit way to wedge the propagation
// path. Keeping the propagator live decouples "am I recording?" from "do I
// carry trace context?", which is the OTel-recommended split.
func disable() Shutdown {
	otel.SetTracerProvider(noop.NewTracerProvider())
	otel.SetTextMapPropagator(propagation.TraceContext{})

	return func(context.Context) error { return nil }
}

// enabled reports whether the operator asked for OTLP export. Tracing stays
// off unless this is true, which is what keeps a default agent from dialing a
// collector that does not exist.
func enabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(EnvExporter)), exporterOTLP)
}

// Init configures the global tracer provider and the W3C TraceContext
// propagator from the environment, and returns the shutdown handle.
//
// When OTEL_TRACES_EXPORTER does not select OTLP, Init installs a no-op
// tracer provider and a no-op shutdown and returns a nil error. In that state
// every StartSpan in the agent returns a non-recording span, so the
// instrumentation at the mirror, transfer, and origin seams costs an interface
// call and nothing else.
//
// Endpoint, headers, TLS, and compression all come from the standard
// OTEL_EXPORTER_OTLP_* variables, which otlptracegrpc reads on its own, so an
// operator configures Gantry the same way they configure any other
// OTel-instrumented process.
func Init(ctx context.Context) (Shutdown, error) {
	if !enabled() {
		return disable(), nil
	}

	exporter, err := otlptracegrpc.New(ctx)
	if err != nil {
		disable()

		return nil, fmt.Errorf("tracing: otlp grpc exporter: %w", err)
	}

	// The service-name override must be schemaless. sdkresource.Default() in the
	// pinned SDK carries semconv v1.43.0, and Merge rejects resources whose
	// schema URLs differ with ErrSchemaURLConflict. Tagging this override with
	// any other schema version therefore did not merely lose the version on
	// this one attribute: Merge failed, and the fallback to a bare Default()
	// dropped the override entirely, so spans exported as
	// unknown_service:<binary> unless the operator happened to set
	// OTEL_SERVICE_NAME. A schemaless resource merges into the default and
	// inherits v1.43.0, which is the behavior wanted here.
	res, err := sdkresource.Merge(
		sdkresource.Default(),
		sdkresource.NewWithAttributes("", semconv.ServiceName(serviceName())),
	)
	if err != nil {
		// A malformed resource must not stop the agent from serving pulls.
		// Default() always exists, so the worst case is a span attributed to
		// the SDK's own unknown_service identity rather than "gantry".
		res = sdkresource.Default()
	}

	// ParentBased keeps the sampling decision containerd already made, so a
	// trace that containerd chose not to sample does not get restarted here.
	// A root span with no remote parent (a request that arrived with no
	// traceparent) is still sampled, so the agent stays traceable standalone.
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter, sdktrace.WithBatchTimeout(batchDelay())),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
	)

	otel.SetTracerProvider(provider)
	// W3C TraceContext is the only propagator Gantry needs: it is the format
	// containerd sends, and the only one a peer or a registry has to
	// understand to stay on the same trace.
	otel.SetTextMapPropagator(propagation.TraceContext{})

	shutdown := provider.Shutdown

	return func(ctx context.Context) error {
		if err := shutdown(ctx); err != nil {
			return fmt.Errorf("tracing: shutdown: %w", err)
		}

		return nil
	}, nil
}

// Provider returns the global tracer provider, for handing to otelhttp so the
// HTTP wrappers emit into the same provider as the hand-started spans.
//
// Resolved lazily for the same reason as Tracer: otelhttp captures the
// provider at wrapper-construction time, so a provider snapshot taken before
// Init would pin the no-op provider for the whole agent.
func Provider() trace.TracerProvider {
	return otel.GetTracerProvider()
}

// SetProviderForTest installs provider as the global tracer provider and
// returns the previous one for restoration. Exported only so tests in other
// packages can assert on recorded spans; production code must call Init.
func SetProviderForTest(provider trace.TracerProvider) trace.TracerProvider {
	previous := otel.GetTracerProvider()

	otel.SetTracerProvider(provider)

	return previous
}

// Tracer returns the agent's tracer from the global provider.
//
// Call sites resolve it lazily on purpose: the global provider only becomes
// meaningful once Init has run, and a Tracer captured before Init would pin
// the no-op provider for the whole process lifetime.
func Tracer() trace.Tracer {
	return otel.Tracer(Scope)
}

// SpanFromContext returns the span active in ctx, for sub-package code that
// needs to attach attributes but does not own the span lifecycle. Returns a
// non-recording span when tracing is disabled or no span is active, so calls
// are always safe.
func SpanFromContext(ctx context.Context) trace.Span {
	return trace.SpanFromContext(ctx)
}

// ServiceName returns the resolved OTLP service.name, for logging at startup
// so an operator can confirm which identity this agent reports to the
// collector's service map.
func ServiceName() string {
	return serviceName()
}

// MarkError records err on the span active in ctx and sets error status, so a
// collector shows the failed hop instead of a silently truncated one. Safe to
// call on a no-op span and with a nil error.
func MarkError(ctx context.Context, err error) {
	if err == nil {
		return
	}

	span := trace.SpanFromContext(ctx)
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

// serviceName resolves the OTLP service.name attribute. OTEL_SERVICE_NAME
// wins, then service.name inside OTEL_RESOURCE_ATTRIBUTES, then the fallback.
func serviceName() string {
	if name := strings.TrimSpace(os.Getenv(EnvServiceName)); name != "" {
		return name
	}

	if name := serviceFromResourceAttributes(os.Getenv(EnvResourceAttributes)); name != "" {
		return name
	}

	return defaultServiceName
}

// serviceFromResourceAttributes pulls service.name out of the standard
// OTEL_RESOURCE_ATTRIBUTES list format (comma-separated key=value pairs).
func serviceFromResourceAttributes(raw string) string {
	for _, pair := range strings.Split(raw, ",") {
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}

		if strings.EqualFold(strings.TrimSpace(key), "service.name") {
			return strings.TrimSpace(value)
		}
	}

	return ""
}

// batchDelay honors OTEL_BSP_SCHEDULE_DELAY, which the OTLP environment
// specification defines in milliseconds. Unset, zero, or unparseable falls
// back to the SDK default.
func batchDelay() time.Duration {
	ms, err := strconv.Atoi(strings.TrimSpace(os.Getenv(EnvBatchDelay)))
	if err != nil || ms <= 0 {
		return defaultBatchDelay
	}

	return time.Duration(ms) * time.Millisecond
}
