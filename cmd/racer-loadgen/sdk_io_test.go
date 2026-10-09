// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/racersdk"
	"github.com/Azure/unbounded/pkg/racersdk/racersdktest"
)

type drainProbe struct {
	io.Reader
	destination io.Writer
	err         error
}

func (p *drainProbe) WriteTo(dst io.Writer) (int64, error) {
	p.destination = dst
	n, err := io.Copy(dst, p.Reader)

	return n, errors.Join(err, p.err)
}

func TestReadBodyDrainPreservesWriterTo(t *testing.T) {
	for _, ending := range []error{nil, io.ErrUnexpectedEOF} {
		t.Run(fmt.Sprint(ending), func(t *testing.T) {
			opts := pullTestOptions("http://unused")
			opts.Verify = false
			p, metrics := pullTestNew(t, &syntheticImage{}, opts)
			destination := p.devNull

			for range 2 {
				body := &drainProbe{Reader: strings.NewReader("payload"), err: ending}
				n, hash, err := p.readBody(body)
				require.ErrorIs(t, err, ending)
				require.Equal(t, int64(7), n)
				require.Empty(t, hash)
				require.Same(t, destination, body.destination)
			}

			require.Equal(t, float64(14), testutil.ToFloat64(metrics.receivedBytes))
			require.Zero(t, testutil.ToFloat64(metrics.verifiedBytes))
		})
	}
}

func TestReadBodyVerifiedUsesRead(t *testing.T) {
	p, metrics := pullTestNew(t, &syntheticImage{}, pullTestOptions("http://unused"))
	body := &drainProbe{Reader: strings.NewReader("payload")}
	n, hash, err := p.readBody(body)
	require.NoError(t, err)
	require.Equal(t, int64(7), n)
	require.Equal(t, digest.FromString("payload").String(), hash)
	require.Nil(t, body.destination, "verification must not invoke WriteTo")
	require.Equal(t, float64(7), testutil.ToFloat64(metrics.receivedBytes))
}

func TestPullerClosesDrainDestination(t *testing.T) {
	p, err := newPuller("test/image", pullTestOptions("http://unused"), pullTestMetrics())
	require.NoError(t, err)
	p.close()
	_, err = p.devNull.Write([]byte("x"))
	require.ErrorIs(t, err, os.ErrClosed)
}

func TestSDKUnverifiedDrainAcrossPages(t *testing.T) {
	const size = racersdk.PageSize + 73

	catalog, err := newBlobCatalog(t.Context(), "test/blob", "drain", 1, size)
	require.NoError(t, err)
	origin, _, err := syntheticOrigin(catalog, newMetrics(prometheus.NewRegistry()))
	require.NoError(t, err)
	client := racersdktest.NewClient(t, origin)
	opts := pullTestOptions("http://unused")
	opts.Verify = false
	p, metrics := pullTestNew(t, &syntheticImage{}, opts)
	p.acquire = udsAcquirer(client)
	response, err := p.acquire(t.Context(), "blob", catalog.batches[0].blobs[0].descriptor)
	require.NoError(t, err)
	require.IsType(t, &racersdk.Object{}, response.body)
	require.NoError(t, response.body.Close())
	require.NoError(t, p.pullBatch(t.Context(), catalog.batches[0]))
	require.Equal(t, float64(size), testutil.ToFloat64(metrics.receivedBytes))
	require.Zero(t, testutil.ToFloat64(metrics.verifiedBytes))
}

func TestSDKErrorStatus(t *testing.T) {
	for _, test := range []struct {
		err  error
		code int
	}{
		{racersdk.ErrInvalidRequest, 400},
		{racersdk.ErrUnauthorized, 401},
		{racersdk.ErrForbidden, 403},
		{racersdk.ErrNotFound, 404},
		{racersdk.ErrVersionMismatch, 412},
		{racersdk.ErrRangeNotSatisfiable, 416},
		{racersdk.ErrUnavailable, 503},
		{errors.New("protocol"), 502},
		{context.Canceled, 0},
		{context.DeadlineExceeded, 0},
		{net.ErrClosed, 0},
	} {
		t.Run(test.err.Error(), func(t *testing.T) {
			require.Equal(t, test.code, sdkErrorStatus(fmt.Errorf("wrapped: %w", test.err)))
		})
	}
}

func TestVerifyFlagEnablesHashingAndDiagnostics(t *testing.T) {
	opts, err := parseOptions([]string{"--verify", "--diagnose-integrity"}, io.Discard)
	require.NoError(t, err)
	require.True(t, opts.pull.Verify)
	require.True(t, opts.pull.DiagnoseIntegrity)
}

func TestSyntheticOriginEmptyRangesAndMissingPin(t *testing.T) {
	catalog, err := newBlobCatalog(t.Context(), "test/blob", "empty-range", 1, 73)
	require.NoError(t, err)
	origin, key, err := syntheticOrigin(catalog, newMetrics(prometheus.NewRegistry()))
	require.NoError(t, err)

	for _, request := range []racersdk.OriginRequest{
		{Request: racersdk.Request{Key: key}, Head: true},
		{Request: racersdk.Request{Key: key}, Offset: racersdk.PageSize, Length: racersdk.PageSize},
		{Request: racersdk.Request{Key: key}, Length: 0},
	} {
		metadata, body, err := origin(t.Context(), request)
		require.NoError(t, err)
		require.Equal(t, int64(73), metadata.Size)
		require.Nil(t, body)
	}

	_, body, err := origin(t.Context(), racersdk.OriginRequest{ETag: `"gone"`, Length: racersdk.PageSize})
	require.ErrorIs(t, err, racersdk.ErrVersionMismatch)
	require.Nil(t, body)
}
