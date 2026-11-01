// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

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

const Scope = "github.com/Azure/unbounded/internal/gantry"

const EnvExporter = "OTEL_TRACES_EXPORTER"

const (
	EnvServiceName        = "OTEL_SERVICE_NAME"
	EnvResourceAttributes = "OTEL_RESOURCE_ATTRIBUTES"

	EnvBatchDelay = "OTEL_BSP_SCHEDULE_DELAY"
)

const exporterOTLP = "otlp"

const defaultServiceName = "gantry"

const defaultBatchDelay = 5 * time.Second

const ShutdownTimeout = 5 * time.Second

type Shutdown func(context.Context) error

func disable() Shutdown {
	otel.SetTracerProvider(noop.NewTracerProvider())
	otel.SetTextMapPropagator(propagation.TraceContext{})

	return func(context.Context) error { return nil }
}

func enabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(EnvExporter)), exporterOTLP)
}

func Init(ctx context.Context) (Shutdown, error) {
	if !enabled() {
		return disable(), nil
	}

	exporter, err := otlptracegrpc.New(ctx)
	if err != nil {
		disable()

		return nil, fmt.Errorf("tracing: otlp grpc exporter: %w", err)
	}

	res, err := sdkresource.Merge(
		sdkresource.Default(),
		sdkresource.NewWithAttributes("", semconv.ServiceName(serviceName())),
	)
	if err != nil {

		res = sdkresource.Default()
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter, sdktrace.WithBatchTimeout(batchDelay())),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
	)

	otel.SetTracerProvider(provider)

	otel.SetTextMapPropagator(propagation.TraceContext{})

	shutdown := provider.Shutdown

	return func(ctx context.Context) error {
		if err := shutdown(ctx); err != nil {
			return fmt.Errorf("tracing: shutdown: %w", err)
		}

		return nil
	}, nil
}

func Provider() trace.TracerProvider {
	return otel.GetTracerProvider()
}

func SetProviderForTest(provider trace.TracerProvider) trace.TracerProvider {
	previous := otel.GetTracerProvider()

	otel.SetTracerProvider(provider)

	return previous
}

func Tracer() trace.Tracer {
	return otel.Tracer(Scope)
}

func SpanFromContext(ctx context.Context) trace.Span {
	return trace.SpanFromContext(ctx)
}

func ServiceName() string {
	return serviceName()
}

func MarkError(ctx context.Context, err error) {
	if err == nil {
		return
	}

	span := trace.SpanFromContext(ctx)
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

func serviceName() string {
	if name := strings.TrimSpace(os.Getenv(EnvServiceName)); name != "" {
		return name
	}

	if name := serviceFromResourceAttributes(os.Getenv(EnvResourceAttributes)); name != "" {
		return name
	}

	return defaultServiceName
}

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

func batchDelay() time.Duration {
	ms, err := strconv.Atoi(strings.TrimSpace(os.Getenv(EnvBatchDelay)))
	if err != nil || ms <= 0 {
		return defaultBatchDelay
	}

	return time.Duration(ms) * time.Millisecond
}
