// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

func metricWithLabels(t *testing.T, family *dto.MetricFamily, labels map[string]string) *dto.Metric {
	t.Helper()
	require.NotNil(t, family, "missing metric family for labels %v", labels)

	for _, metric := range family.GetMetric() {
		actual := make(map[string]string)
		for _, label := range metric.GetLabel() {
			actual[label.GetName()] = label.GetValue()
		}

		if len(actual) != len(labels) {
			continue
		}

		match := true

		for name, value := range labels {
			if actual[name] != value {
				match = false
			}
		}

		if match {
			return metric
		}
	}

	t.Fatalf("metric %s with labels %v not found", family.GetName(), labels)

	return nil
}

func gatherLoadgenMetrics(t *testing.T, reg *prometheus.Registry) map[string]*dto.MetricFamily {
	t.Helper()

	families, err := reg.Gather()
	require.NoError(t, err)

	result := make(map[string]*dto.MetricFamily, len(families))
	for _, family := range families {
		result[family.GetName()] = family
	}

	return result
}

func TestMetricsInstrumentStatusAndBytes(t *testing.T) {
	for _, test := range []struct {
		name    string
		handler http.HandlerFunc
		status  int
		body    string
	}{
		{"empty implicit OK", func(http.ResponseWriter, *http.Request) {}, http.StatusOK, ""},
		{"implicit OK", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "hello")
			_, _ = io.WriteString(w, " world")
		}, http.StatusOK, "hello world"},
		{"explicit error", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "unavailable")
		}, http.StatusServiceUnavailable, "unavailable"},
		{"first final status wins", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusCreated)
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, "created")
		}, http.StatusCreated, "created"},
		{"write commits OK", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "ok")
			w.WriteHeader(http.StatusInternalServerError)
		}, http.StatusOK, "ok"},
	} {
		t.Run(test.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			metrics := newMetrics(reg)
			response := httptest.NewRecorder()
			metrics.instrument(test.handler).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
			require.Equal(t, test.status, response.Code)
			require.Equal(t, test.body, response.Body.String())
			families := gatherLoadgenMetrics(t, reg)
			require.Len(t, families["racer_loadgen_origin_requests_total"].GetMetric(), 1)

			labels := map[string]string{"method": http.MethodGet, "code": strconv.Itoa(test.status)}
			require.Equal(t, float64(1), metricWithLabels(t, families["racer_loadgen_origin_requests_total"], labels).GetCounter().GetValue())
			require.Equal(t, float64(len(test.body)), metricWithLabels(t, families["racer_loadgen_origin_bytes_total"], nil).GetCounter().GetValue())
			histogram := metricWithLabels(t, families["racer_loadgen_origin_request_duration_seconds"], nil).GetHistogram()
			require.Equal(t, uint64(1), histogram.GetSampleCount())
			require.GreaterOrEqual(t, histogram.GetSampleSum(), float64(0))
			require.NotEmpty(t, histogram.GetBucket())
		})
	}
}

type partialOriginWriter struct {
	*httptest.ResponseRecorder
	limit int
	err   error
}

func (w *partialOriginWriter) Write(data []byte) (int, error) {
	n, _ := w.ResponseRecorder.Write(data[:min(len(data), w.limit)])

	return n, w.err
}

func TestMetricsInstrumentPartialWrite(t *testing.T) {
	for _, accepted := range []int{0, 3} {
		t.Run(strconv.Itoa(accepted), func(t *testing.T) {
			reg := prometheus.NewRegistry()
			metrics := newMetrics(reg)
			writeErr := errors.New("connection closed")
			writer := &partialOriginWriter{ResponseRecorder: httptest.NewRecorder(), limit: accepted, err: writeErr}
			handler := metrics.instrument(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				n, err := w.Write([]byte("longer than accepted"))
				require.Equal(t, accepted, n)
				require.ErrorIs(t, err, writeErr)
			}))
			handler.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/", nil))
			require.Equal(t, accepted, writer.Body.Len())
			require.Equal(t, float64(accepted), testutil.ToFloat64(metrics.originBytes))
			require.Equal(t, float64(1), testutil.ToFloat64(metrics.originRequests.WithLabelValues(http.MethodGet, "200")))
		})
	}
}

func TestMetricsInstrumentRegistryHeadRangeAndErrors(t *testing.T) {
	img, err := newImage(t.Context(), imageOptions{Repository: "test/image", Layers: 1, LayerBytes: 1024, Seed: "metrics"})
	require.NoError(t, err)

	reg := prometheus.NewRegistry()
	metrics := newMetrics(reg)
	handler := metrics.instrument(img.handler())
	path := "/v2/test/image/blobs/" + img.Layers[0].Digest.String()

	var totalBytes int

	for _, test := range []struct {
		method string
		path   string
		ranges string
		status int
	}{
		{http.MethodGet, path, "", http.StatusOK},
		{http.MethodGet, path, "bytes=7-15", http.StatusPartialContent},
		{http.MethodHead, path, "", http.StatusOK},
		{http.MethodHead, "/v2/test/image/blobs/missing", "", http.StatusNotFound},
		{http.MethodGet, "/v2/test/image/blobs/missing", "", http.StatusNotFound},
		{http.MethodPost, path, "", http.StatusMethodNotAllowed},
	} {
		request := httptest.NewRequest(test.method, test.path, nil)
		request.Header.Set("Range", test.ranges)

		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		require.Equal(t, test.status, response.Code)

		if test.method == http.MethodHead {
			require.Empty(t, response.Body.Bytes())
		} else if test.status == http.StatusPartialContent {
			require.Equal(t, 9, response.Body.Len())
		} else if test.status == http.StatusOK {
			require.Equal(t, img.Layers[0].Size, int64(response.Body.Len()))
		} else {
			require.Positive(t, response.Body.Len())
		}

		totalBytes += response.Body.Len()
		require.Equal(t, float64(totalBytes), testutil.ToFloat64(metrics.originBytes))
	}

	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(`
# HELP racer_loadgen_origin_requests_total Synthetic origin HTTP requests.
# TYPE racer_loadgen_origin_requests_total counter
racer_loadgen_origin_requests_total{code="200",method="GET"} 1
racer_loadgen_origin_requests_total{code="206",method="GET"} 1
racer_loadgen_origin_requests_total{code="404",method="GET"} 1
racer_loadgen_origin_requests_total{code="200",method="HEAD"} 1
racer_loadgen_origin_requests_total{code="404",method="HEAD"} 1
racer_loadgen_origin_requests_total{code="405",method="other"} 1
`), "racer_loadgen_origin_requests_total"))
	families := gatherLoadgenMetrics(t, reg)
	require.Equal(t, uint64(6), metricWithLabels(t, families["racer_loadgen_origin_request_duration_seconds"], nil).GetHistogram().GetSampleCount())
}

func TestMetricsInstrumentBoundedMethodLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := newMetrics(reg)
	handler := metrics.instrument(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	methods := []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions, http.MethodPatch, http.MethodConnect, http.MethodTrace, "get"}
	for index := range 100 {
		methods = append(methods, fmt.Sprintf("CUSTOM%d", index))
	}

	for index, method := range methods {
		request := httptest.NewRequest(method, fmt.Sprintf("/arbitrary/%d?unbounded=%d", index, index), nil)
		handler.ServeHTTP(httptest.NewRecorder(), request)
	}

	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(fmt.Sprintf(`
# HELP racer_loadgen_origin_requests_total Synthetic origin HTTP requests.
# TYPE racer_loadgen_origin_requests_total counter
racer_loadgen_origin_requests_total{code="204",method="GET"} 1
racer_loadgen_origin_requests_total{code="204",method="HEAD"} 1
racer_loadgen_origin_requests_total{code="204",method="other"} %d
`, len(methods)-2)), "racer_loadgen_origin_requests_total"))
	require.Zero(t, testutil.ToFloat64(metrics.originBytes))
}

func TestOriginResponseUnwrap(t *testing.T) {
	underlying := httptest.NewRecorder()
	response := &originResponse{ResponseWriter: underlying}
	require.Same(t, underlying, response.Unwrap())
	require.NoError(t, http.NewResponseController(response).Flush())
	require.True(t, underlying.Flushed)
}
