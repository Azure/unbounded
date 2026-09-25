// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/metrics"
	"github.com/Azure/unbounded/internal/gantry/origin"
)

func TestOriginClientMetricLifecycles(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "body")
	}))
	defer upstream.Close()

	cfg := &config.Config{UpstreamRegistries: []config.UpstreamRegistry{{Name: "registry.example", Endpoint: upstream.URL}}}
	inst := newPhase1Metrics(metrics.New())
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	mirror, err := buildMirrorOriginClient(cfg, inst, logger)
	if err != nil {
		t.Fatal(err)
	}

	puller, success, downstreamFailure, err := buildPullOriginClient(cfg, inst, logger)
	if err != nil {
		t.Fatal(err)
	}

	ref := ifaces.OriginRef{Registry: "registry.example", Repository: "repo", Digest: digest.MustParse("sha256:0000000000000000000000000000000000000000000000000000000000000000"), Kind: ifaces.KindBlob}
	kind := ref.Kind.MetricLabel()
	pull := func(client *origin.Client) {
		t.Helper()

		body, _, err := client.Pull(t.Context(), ref)
		if err != nil {
			t.Fatal(err)
		}

		_, err = io.Copy(io.Discard, body)
		_ = body.Close()

		if err != nil {
			t.Fatal(err)
		}
	}
	pull(mirror)

	if testutil.ToFloat64(inst.originPullTotal.WithLabelValues(kind)) != 0 || testutil.ToFloat64(inst.originBytes.WithLabelValues(kind)) != 4 {
		t.Fatal("live mirror bytes entered background pull accounting")
	}

	pull(puller)

	if testutil.ToFloat64(inst.originPullTotal.WithLabelValues(kind)) != 1 || testutil.ToFloat64(inst.originPullSuccess.WithLabelValues(kind)) != 0 || testutil.ToFloat64(inst.originBytes.WithLabelValues(kind)) != 8 {
		t.Fatal("background pull must count bytes and start, but not success on close")
	}

	success(kind, 4)
	downstreamFailure(kind, "transient")

	if testutil.ToFloat64(inst.originPullSuccess.WithLabelValues(kind)) != 1 || testutil.ToFloat64(inst.originPullFailure.WithLabelValues(kind, "transient")) != 1 || testutil.ToFloat64(inst.originFailureTotal.WithLabelValues("transient")) != 0 {
		t.Fatal("downstream terminals changed origin-side failure accounting")
	}

	ref.Registry = "unconfigured.example"
	if _, _, err := mirror.Pull(t.Context(), ref); err == nil {
		t.Fatal("mirror accepted an unknown registry")
	}

	class := string(ifaces.FailureNotFound)
	if testutil.ToFloat64(inst.originPullTotal.WithLabelValues(kind)) != 1 || testutil.ToFloat64(inst.originPullFailure.WithLabelValues(kind, class)) != 0 {
		t.Fatal("live mirror failure entered background pull accounting")
	}

	if _, _, err := puller.Pull(t.Context(), ref); err == nil {
		t.Fatal("puller accepted an unknown registry")
	}

	if testutil.ToFloat64(inst.originPullTotal.WithLabelValues(kind)) != 2 || testutil.ToFloat64(inst.originPullFailure.WithLabelValues(kind, class)) != 1 || testutil.ToFloat64(inst.originFailureTotal.WithLabelValues(class)) != 1 {
		t.Fatal("origin-side failure must count both failure families")
	}
}

func TestOriginClientConstructionErrors(t *testing.T) {
	cfg := &config.Config{}
	if client, err := buildMirrorOriginClient(cfg, nil, nil); err == nil || client != nil {
		t.Fatal("mirror constructor accepted missing upstreams")
	}

	if client, success, failure, err := buildPullOriginClient(cfg, nil, nil); err == nil || client != nil || success != nil || failure != nil {
		t.Fatal("puller constructor accepted missing upstreams")
	}
}
