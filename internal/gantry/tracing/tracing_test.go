// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package tracing

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// installRecorder replaces the global provider with an in-memory exporter and
// returns the recorder plus a restore func. Every test that asserts on spans
// must go through this so it does not leak a live provider into a sibling test.
func installRecorder(t *testing.T) (*tracetest.SpanRecorder, func()) {
	t.Helper()

	rec := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	previous := otel.GetTracerProvider()

	otel.SetTracerProvider(provider)

	return rec, func() { otel.SetTracerProvider(previous) }
}

func TestInitDisabledByDefault(t *testing.T) {
	t.Setenv(EnvExporter, "")

	shutdown, err := Init(context.Background())
	if err != nil {
		t.Fatalf("Init with exporter unset: unexpected error %v", err)
	}

	if shutdown == nil {
		t.Fatal("Init returned nil shutdown")
	}

	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("inert shutdown returned error %v", err)
	}

	if enabled() {
		t.Fatal("enabled() true with OTEL_TRACES_EXPORTER unset")
	}

	// A disabled agent must not record spans: the seams in mirror, transfer,
	// and origin depend on this being free.
	rec, restore := installRecorder(t)
	defer restore()

	_, probe := otel.GetTracerProvider().Tracer(Scope).Start(context.Background(), "probe")
	probe.End()

	if got := len(rec.Ended()); got != 1 {
		t.Fatalf("recorder provider should still record while installed, got %d spans", got)
	}

	// Re-disable and confirm the global provider records nothing.
	disable()

	_, span := Provider().Tracer(Scope).Start(context.Background(), "probe")
	if span.SpanContext().IsValid() {
		t.Fatal("disabled provider produced a valid span context")
	}

	span.End()
}

func TestInitUnknownExporterStaysOff(t *testing.T) {
	// Only "otlp" is honored. A typo or the OTLP/HTTP spelling must not
	// half-initialize the agent.
	for _, value := range []string{"none", "jaeger", "otlp-http", "otlphttp", "console", "  "} {
		t.Run(value, func(t *testing.T) {
			t.Setenv(EnvExporter, value)

			if enabled() {
				t.Fatalf("enabled() true for %q", value)
			}
		})
	}
}

func TestEnabledAcceptsOTLPCaseInsensitivelyAndTrimmed(t *testing.T) {
	for _, value := range []string{"otlp", "OTLP", "Otlp", " otlp ", "\totlp"} {
		t.Setenv(EnvExporter, value)

		if !enabled() {
			t.Fatalf("enabled() false for %q", value)
		}
	}
}

func TestServiceNamePrecedence(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		t.Setenv(EnvServiceName, "")
		t.Setenv(EnvResourceAttributes, "")

		if got := serviceName(); got != defaultServiceName {
			t.Fatalf("serviceName() = %q, want %q", got, defaultServiceName)
		}
	})

	t.Run("otel_service_name_wins", func(t *testing.T) {
		t.Setenv(EnvServiceName, "gantry-node-a")
		t.Setenv(EnvResourceAttributes, "service.name=from-attrs,deployment.environment=test")

		if got := serviceName(); got != "gantry-node-a" {
			t.Fatalf("serviceName() = %q, want OTEL_SERVICE_NAME to win", got)
		}
	})

	t.Run("resource_attributes_fallback", func(t *testing.T) {
		t.Setenv(EnvServiceName, "")
		t.Setenv(EnvResourceAttributes, "deployment.environment=test,service.name=gantry-from-attrs")

		if got := serviceName(); got != "gantry-from-attrs" {
			t.Fatalf("serviceName() = %q, want service.name from resource attributes", got)
		}
	})

	t.Run("resource_attributes_key_is_case_insensitive", func(t *testing.T) {
		t.Setenv(EnvServiceName, "")
		t.Setenv(EnvResourceAttributes, "SERVICE.Name=mixed-case")

		if got := serviceName(); got != "mixed-case" {
			t.Fatalf("serviceName() = %q, want mixed-case", got)
		}
	})

	t.Run("malformed_attributes_fall_back", func(t *testing.T) {
		t.Setenv(EnvServiceName, "")
		t.Setenv(EnvResourceAttributes, "garbage,noequals,norelation")

		if got := serviceName(); got != defaultServiceName {
			t.Fatalf("serviceName() = %q, want default on malformed input", got)
		}
	})

	t.Run("empty_otel_service_name_is_skipped", func(t *testing.T) {
		t.Setenv(EnvServiceName, "   ")
		t.Setenv(EnvResourceAttributes, "service.name=from-attrs")

		if got := serviceName(); got != "from-attrs" {
			t.Fatalf("serviceName() = %q, want whitespace-only name skipped", got)
		}
	})
}

func TestBatchDelay(t *testing.T) {
	t.Run("unset_uses_default", func(t *testing.T) {
		t.Setenv(EnvBatchDelay, "")

		if got := batchDelay(); got != defaultBatchDelay {
			t.Fatalf("batchDelay() = %v, want %v", got, defaultBatchDelay)
		}
	})

	t.Run("milliseconds_are_honored", func(t *testing.T) {
		t.Setenv(EnvBatchDelay, "250")

		if got := batchDelay(); got.Milliseconds() != 250 {
			t.Fatalf("batchDelay() = %v, want 250ms", got)
		}
	})

	// Zero and garbage must not produce a zero-interval batcher, which would
	// spin on export.
	for _, value := range []string{"0", "-1", "abc", "1.5", ""} {
		t.Run("invalid_"+value, func(t *testing.T) {
			t.Setenv(EnvBatchDelay, value)

			if got := batchDelay(); got != defaultBatchDelay {
				t.Fatalf("batchDelay(%q) = %v, want %v", value, got, defaultBatchDelay)
			}
		})
	}
}

func TestMarkErrorNilIsNoop(t *testing.T) {
	rec, restore := installRecorder(t)
	defer restore()

	ctx, span := Tracer().Start(context.Background(), "op")
	MarkError(ctx, nil)
	span.End()

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}

	if spans[0].Status().Code.String() == "Error" {
		t.Fatal("nil error must not set Error status")
	}
}

func TestMarkErrorRecordsStatus(t *testing.T) {
	rec, restore := installRecorder(t)
	defer restore()

	ctx, span := Tracer().Start(context.Background(), "op")
	MarkError(ctx, context.DeadlineExceeded)
	span.End()

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}

	if got := spans[0].Status().Code.String(); got != "Error" {
		t.Fatalf("status code = %q, want Error", got)
	}

	if len(spans[0].Events()) == 0 {
		t.Fatal("MarkError must record an exception event")
	}
}

func TestMarkErrorSafeWhenDisabled(t *testing.T) {
	disable()

	// Must not panic on a no-op span with no span in context at all.
	MarkError(context.Background(), context.Canceled)
}

func TestAttributesProduceExpectedKeys(t *testing.T) {
	rec, restore := installRecorder(t)
	defer restore()

	_, span := Tracer().Start(context.Background(), "op")
	span.SetAttributes(
		Digest("sha256:abc"),
		MediaType("application/vnd.oci.image.layer.v1.tar"),
		Bytes(4096),
		Cache(true),
		Source(SourceUpstream),
		PeerID("12D3Koo"),
		Registry("docker.io"),
		Repository("library/nginx"),
		Kind("blob"),
		StatusCode(200),
	)
	span.End()

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}

	want := map[string]string{
		AttrDigest:      "sha256:abc",
		AttrMediaType:   "application/vnd.oci.image.layer.v1.tar",
		AttrBytes:       "4096",
		AttrCacheResult: CacheHit,
		AttrSource:      SourceUpstream,
		AttrPeerID:      "12D3Koo",
		AttrRegistry:    "docker.io",
		AttrRepository:  "library/nginx",
		AttrKind:        "blob",
		AttrStatusCode:  "200",
	}

	got := map[string]string{}
	for _, kv := range spans[0].Attributes() {
		got[string(kv.Key)] = kv.Value.String()
	}

	for key, expect := range want {
		if got[key] != expect {
			t.Errorf("attribute %s = %q, want %q", key, got[key], expect)
		}
	}
}

func TestCacheAttributeDistinguishesHitFromMiss(t *testing.T) {
	if Cache(true).Value.AsString() != CacheHit {
		t.Fatal("Cache(true) must emit the hit label")
	}

	if Cache(false).Value.AsString() != CacheMiss {
		t.Fatal("Cache(false) must emit the miss label")
	}
}

// TestResourceMergeKeepsServiceName guards the schema-URL conflict. The pinned
// SDK's default resource is tagged semconv v1.43.0; merging an override tagged
// with any other schema version fails with ErrSchemaURLConflict, and the error
// path falls back to a bare Default(), silently losing the service name so
// spans land as unknown_service:<binary>. A schemaless override merges cleanly.
func TestResourceMergeKeepsServiceName(t *testing.T) {
	t.Setenv(EnvServiceName, "gantry-node-7")

	res, err := sdkresource.Merge(
		sdkresource.Default(),
		sdkresource.NewWithAttributes("", semconv.ServiceName(serviceName())),
	)
	if err != nil {
		t.Fatalf("merge schemaless service-name override: %v", err)
	}

	var found string

	for _, kv := range res.Attributes() {
		if string(kv.Key) == "service.name" {
			found = kv.Value.AsString()
		}
	}

	if found != "gantry-node-7" {
		t.Fatalf("service.name = %q, want gantry-node-7", found)
	}
}

// TestResourceMergeRejectsMismatchedSchema documents WHY the override above is
// schemaless. If this ever stops failing, the SDK changed its default schema
// handling and the schemaless workaround can be revisited.
func TestResourceMergeRejectsMismatchedSchema(t *testing.T) {
	_, err := sdkresource.Merge(
		sdkresource.Default(),
		sdkresource.NewWithAttributes(semconv.SchemaURL, semconv.ServiceName(defaultServiceName)),
	)
	if err == nil {
		t.Skip("SDK default resource no longer conflicts with the v1.26.0 schema; revisit the schemaless override")
	}
}
