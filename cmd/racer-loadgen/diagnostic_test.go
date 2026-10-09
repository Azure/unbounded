// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

type diagnosticChunks struct {
	data []byte
	step int
	err  error
}

func (r *diagnosticChunks) Read(p []byte) (int, error) {
	n := copy(p, r.data[:min(len(r.data), r.step)])

	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, r.err
	}

	return n, nil
}

func TestDiagnosticReader(t *testing.T) {
	data := make([]byte, diagnosticPageSize+1031)
	for i := range data {
		data[i] = byte(i * 31)
	}

	desc := byteDescriptor("test", data)
	p, _ := pullTestNew(t, pullTestImage(t), pullTestOptions("http://localhost"))

	for _, offset := range []int{-1, 0, 32767, 32768, diagnosticPageSize - 1, diagnosticPageSize, diagnosticPageSize + 1, len(data) - 1} {
		actual := bytes.Clone(data)
		if offset >= 0 {
			actual[offset] ^= 1
		}

		n, hash, e, err := p.readBodyDiagnostic(&diagnosticChunks{actual, 32749, io.EOF}, blobSource{desc, bytes.NewReader(data)})
		require.NoError(t, err)
		require.Equal(t, int64(len(data)), n)
		require.Equal(t, digest.FromBytes(actual).String(), hash)
		require.Equal(t, int64(offset), e.firstMismatch)
		require.Equal(t, int64(2), e.examined)

		if offset < 0 {
			require.Empty(t, e.records)
			continue
		}

		require.Len(t, e.records, 1)
		r := e.records[0]
		require.Equal(t, sha256.Sum256(actual[r.Offset:r.Offset+r.Length]), r.Actual)
		require.Equal(t, sha256.Sum256(data[r.Offset:r.Offset+r.Length]), r.Expected)
	}

	_, _, _, err := p.readBodyDiagnostic(&diagnosticChunks{[]byte{1}, 1, io.ErrUnexpectedEOF}, blobSource{desc, bytes.NewReader(data)})
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	_, _, _, err = p.readBodyDiagnostic(bytes.NewReader(data), blobSource{desc, bytes.NewReader(nil)})
	require.ErrorContains(t, err, "oracle")
}

func TestDiagnosticFetchClassification(t *testing.T) {
	img := pullTestImage(t)
	for _, mode := range []string{"good", "corrupt", "short", "long", "status", "missing", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			data := bytes.Clone(img.manifest)
			status := 200

			switch mode {
			case "corrupt":
				data[len(data)-1] ^= 1
			case "short":
				data = data[:len(data)-1]
			case "long":
				data = append(data, 0)
			case "status":
				status = 503
			}

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status); _, _ = w.Write(data) }))
			defer server.Close()

			opts := pullTestOptions(server.URL)
			opts.DiagnoseIntegrity = mode != "disabled"
			p, _ := pullTestNew(t, img, opts)
			require.NoError(t, p.configureDiagnostics(catalogFromImages([]*syntheticImage{img})))

			if mode == "missing" {
				p.expected = nil
			}

			if mode == "disabled" {
				require.Nil(t, p.expected)
			}

			err := p.fetch(t.Context(), "manifest", img.Manifest)
			if mode == "good" || mode == "disabled" {
				require.NoError(t, err)
				return
			}

			require.Error(t, err)

			want := map[string]string{"corrupt": "digest_mismatch", "short": "incomplete", "long": "size_mismatch", "status": "http_status", "missing": "other"}
			require.Equal(t, want[mode], classifyFailure(err).String())

			var failure *pullFailure
			require.ErrorAs(t, err, &failure)

			if mode == "corrupt" {
				require.NotNil(t, failure.integrity.pages)
			} else {
				require.Nil(t, failure.integrity)
			}
		})
	}
}

func TestDiagnosticFiniteTarget(t *testing.T) {
	opts := testImageOptions()
	img, err := newImage(t.Context(), catalogImageOptions(opts, 2))
	require.NoError(t, err)

	var requests atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); img.handler().ServeHTTP(w, r) }))
	defer server.Close()

	cfg := diagnosticTargetConfig{Target: server.URL, Image: opts, ImageIndices: []int{2}, Iterations: 2, LayerConcurrency: 1, StartupTimeout: "2s", PullTimeout: "2s", TotalTimeout: "5s"}
	require.NoError(t, runDiagnosticTarget(t.Context(), cfg))
	require.Equal(t, int64(2*(2+opts.Layers)), requests.Load())

	cfg.ImageIndices = []int{1}
	require.Error(t, runDiagnosticTarget(t.Context(), cfg))
	require.Equal(t, int64(2*(2+opts.Layers)+1), requests.Load())

	cfg.Iterations = 0
	require.Error(t, runDiagnosticTarget(t.Context(), cfg))

	op := pullTestOptions(server.URL)
	op.DiagnoseIntegrity, op.Verify = true, false
	_, err = newPuller(img, op, pullTestMetrics())
	require.Error(t, err)
	require.False(t, errors.Is(err, io.EOF))
}

type diagnosticPattern struct{ corrupt bool }

func (r diagnosticPattern) ReadAt(p []byte, offset int64) (int, error) {
	clear(p)

	if r.corrupt {
		for i := range p {
			if (offset+int64(i))%diagnosticPageSize == 0 {
				p[i] = 1
			}
		}
	}

	return len(p), nil
}

func TestDiagnosticPageCapAndLogging(t *testing.T) {
	p, _ := pullTestNew(t, pullTestImage(t), pullTestOptions("http://localhost"))
	desc := byteDescriptor("test", nil)
	desc.Size = 9 * diagnosticPageSize
	_, _, evidence, err := p.readBodyDiagnostic(io.NewSectionReader(diagnosticPattern{true}, 0, desc.Size), blobSource{desc, diagnosticPattern{}})
	require.NoError(t, err)
	require.Equal(t, int64(9), evidence.examined)
	require.Equal(t, int64(9), evidence.mismatching)
	require.Len(t, evidence.records, 8)

	reg := prometheus.NewRegistry()
	p.metrics = newMetrics(reg)

	var output bytes.Buffer

	logger := slog.New(slog.NewJSONHandler(&output, nil))
	failure := &pullFailure{
		err: errors.New("secret payload URL"), reason: failureDigest, kind: "layer", status: 200,
		integrity: &integrityEvidence{expectedDigest: desc.Digest.String(), actualDigest: desc.Digest.String(), pages: evidence},
	}
	now := time.Now()
	p.reportPullFailure(failure, now, logger)

	first := output.String()

	p.reportPullFailure(failure, now, logger)
	require.Equal(t, first, output.String())
	require.NotContains(t, first, "secret")
	require.Less(t, len(first), 4096)

	var record struct {
		Pages struct {
			Records []struct {
				Actual   string `json:"actual_digest"`
				Expected string `json:"expected_digest"`
			} `json:"records"`
			Omitted int `json:"omitted_mismatching_pages"`
		} `json:"pages"`
	}
	require.NoError(t, json.Unmarshal([]byte(first), &record))
	require.Len(t, record.Pages.Records, 8)
	require.Equal(t, 1, record.Pages.Omitted)
	require.NotEqual(t, record.Pages.Records[0].Actual, record.Pages.Records[0].Expected)
	require.Equal(t, "sha256:", record.Pages.Records[0].Actual[:7])
	family := gatherLoadgenMetrics(t, reg)["racer_loadgen_pull_failures_total"]
	require.Len(t, family.Metric, 1)
	require.Len(t, family.Metric[0].Label, 1)
	// The logging boundary caps records even for incorrectly constructed evidence.
	evidence.records = append(evidence.records, evidence.records...)

	output.Reset()
	p.reportPullFailure(failure, now.Add(failureLogInterval), logger)
	require.NoError(t, json.Unmarshal(output.Bytes(), &record))
	require.Len(t, record.Pages.Records, 8)
}

func TestDiagnosticCancellationAndOracle(t *testing.T) {
	img := pullTestImage(t)

	for _, canceled := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(img.manifest[:1])
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}))
		opts := pullTestOptions(server.URL)
		opts.DiagnoseIntegrity = true
		opts.Timeout = 20 * time.Millisecond
		p, metrics := pullTestNew(t, img, opts)
		require.NoError(t, p.configureDiagnostics(catalogFromImages([]*syntheticImage{img})))

		ctx, cancel := context.WithCancel(t.Context())
		if canceled {
			cancel()
		}

		err := p.pull(ctx)

		cancel()

		if canceled {
			require.ErrorIs(t, err, context.Canceled)
		} else {
			require.ErrorIs(t, err, context.DeadlineExceeded)
		}

		require.Zero(t, testutil.ToFloat64(metrics.verifiedBytes))
		require.Zero(t, testutil.ToFloat64(metrics.inFlight))
		server.Close()
	}

	server := httptest.NewServer(img.handler())
	defer server.Close()

	opts := pullTestOptions(server.URL)
	opts.DiagnoseIntegrity = true
	p, _ := pullTestNew(t, img, opts)
	require.Error(t, p.configureDiagnostics(nil))
	require.NoError(t, p.configureDiagnostics(catalogFromImages([]*syntheticImage{img})))
	blob := p.expected[img.Manifest.Digest]
	blob.data = bytes.NewReader(nil)
	p.expected[img.Manifest.Digest] = blob
	err := p.pull(t.Context())
	require.ErrorIs(t, err, errDiagnosticOracle)
	require.Equal(t, "other", classifyFailure(err).String())
}

func TestDiagnosticTruncatedAndEmpty(t *testing.T) {
	p, metrics := pullTestNew(t, pullTestImage(t), pullTestOptions("http://localhost"))

	for _, size := range []int64{0, diagnosticPageSize} {
		desc := byteDescriptor("test", nil)
		desc.Size = size
		n, _, e, err := p.readBodyDiagnostic(io.NewSectionReader(diagnosticPattern{}, 0, size), blobSource{desc, diagnosticPattern{}})
		require.NoError(t, err)
		require.Equal(t, size, n)
		require.Equal(t, size/diagnosticPageSize, e.examined)
		require.Equal(t, int64(-1), e.firstMismatch)
	}

	desc := byteDescriptor("test", []byte{0, 0})
	n, _, e, err := p.readBodyDiagnostic(&diagnosticChunks{[]byte{1}, 1, io.EOF}, blobSource{desc, diagnosticPattern{}})
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
	require.Zero(t, e.examined, "short final page must not be finalized")
	require.Empty(t, e.records)
	require.Equal(t, float64(diagnosticPageSize+1), testutil.ToFloat64(metrics.receivedBytes))

	parsed, err := parseOptions([]string{"--diagnose-integrity"}, io.Discard)
	require.NoError(t, err)
	require.True(t, parsed.pull.DiagnoseIntegrity)
}

func TestDiagnosticTargetDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()

	cfg := diagnosticTargetConfig{Target: server.URL, Image: testImageOptions(), ImageIndices: []int{0}, Iterations: 2, LayerConcurrency: 1, StartupTimeout: "1s", PullTimeout: "1s", TotalTimeout: "20ms"}
	require.ErrorIs(t, runDiagnosticTarget(t.Context(), cfg), context.DeadlineExceeded)

	for _, indices := range [][]int{nil, {-1}, {512}} {
		cfg.ImageIndices = indices
		require.Error(t, runDiagnosticTarget(t.Context(), cfg))
	}
}
