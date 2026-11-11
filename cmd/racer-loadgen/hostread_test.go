// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/Azure/unbounded/pkg/racersdk"
)

// fakeHostReader writes a blob into the destination file like the dataplane:
// positioned writes, one page at a time, each confirmed with OnPage.
type fakeHostReader struct {
	mu       sync.Mutex
	data     []byte
	etag     string
	page     int64
	failures int   // ranges longer than one page that fail after one page
	corrupt  bool  // flip a byte in the file
	statErr  error // returned by Stat and HostRead
	calls    int
	pinned   int
	ranges   [][2]int64
}

func (f *fakeHostReader) Stat(context.Context, racersdk.Request) (racersdk.Metadata, error) {
	if f.statErr != nil {
		return racersdk.Metadata{}, f.statErr
	}

	return racersdk.Metadata{Size: int64(len(f.data)), ETag: f.etag}, nil
}

func (f *fakeHostReader) HostRead(_ context.Context, _ racersdk.Request, file *os.File, o racersdk.HostReadOptions) (racersdk.HostReadResult, error) {
	f.mu.Lock()
	f.calls++
	f.ranges = append(f.ranges, [2]int64{o.Offset, o.Length})

	if o.ETag != "" {
		f.pinned++
	}

	fail := false

	if f.failures > 0 && o.Length > f.page {
		f.failures--
		fail = true
	}
	f.mu.Unlock()

	if f.statErr != nil {
		return racersdk.HostReadResult{}, f.statErr
	}

	if o.ETag != "" && o.ETag != f.etag {
		return racersdk.HostReadResult{}, racersdk.ErrVersionMismatch
	}

	end := int64(len(f.data))
	if o.Length != 0 {
		end = o.Offset + o.Length
	}

	if end-o.Offset > o.DestinationLength {
		return racersdk.HostReadResult{}, racersdk.ErrInvalidRequest
	}

	for at := o.Offset; at < end; at += f.page {
		stop := min(at+f.page, end)

		chunk := append([]byte(nil), f.data[at:stop]...)
		if f.corrupt && at == o.Offset {
			chunk[0] ^= 0xff
		}

		if _, err := file.WriteAt(chunk, o.DestinationOffset+at-o.Offset); err != nil {
			return racersdk.HostReadResult{}, err
		}

		o.OnPage(at, stop-at)

		if fail {
			return racersdk.HostReadResult{}, io.ErrUnexpectedEOF
		}
	}

	return racersdk.HostReadResult{
		Metadata: racersdk.Metadata{Size: int64(len(f.data)), ETag: f.etag},
		Offset:   o.Offset, Length: end - o.Offset,
	}, nil
}

// hostReadTestPuller returns a puller whose acquire uses fake and a blob
// descriptor whose bytes fake serves.
func hostReadTestPuller(t *testing.T, size int64, configure func(*pullOptions)) (*puller, *fakeHostReader, *memfdPool, blobSource) {
	t.Helper()

	catalog, err := newBlobCatalog(t.Context(), "test/blobs", "host-read", 1, size)
	require.NoError(t, err)

	var blob blobSource
	for _, b := range catalog.blobs {
		blob = b
	}

	data := make([]byte, size)
	_, err = blob.data.ReadAt(data, 0)
	require.NoError(t, err)

	opts := pullTestOptions("http://127.0.0.1:1")
	if configure != nil {
		configure(&opts)
	}

	p, _ := pullTestNew(t, pullTestImage(t), opts)
	fake := &fakeHostReader{data: data, etag: `"` + blob.descriptor.Digest.String() + `"`, page: 100}
	pool := &memfdPool{}

	t.Cleanup(func() { require.NoError(t, pool.close()) })

	p.acquire = p.udsHostReadAcquirer(fake, pool)

	return p, fake, pool, blob
}

func TestHostReadPullWholeObjectVerifiesAndReusesMemfd(t *testing.T) {
	p, fake, pool, blob := hostReadTestPuller(t, 1000, nil)

	for range 3 {
		require.NoError(t, p.fetch(t.Context(), "blob", blob.descriptor))
	}

	require.Equal(t, 3, fake.calls)
	require.Zero(t, fake.pinned, "whole-object reads are unpinned")
	require.Len(t, pool.all, 1, "sequential pulls reuse one memfd")
	require.Equal(t, float64(3000), testutil.ToFloat64(p.metrics.receivedBytes))
	require.Equal(t, float64(3), testutil.ToFloat64(p.metrics.requests.WithLabelValues("blob", "success")))
}

func TestHostReadPullRangesVerifyAndResume(t *testing.T) {
	p, fake, pool, blob := hostReadTestPuller(t, 1000, func(o *pullOptions) {
		o.RangeBytes, o.RangeParallel, o.RangeRetries = 300, 3, 2
	})
	fake.failures = 2

	require.NoError(t, p.fetch(t.Context(), "blob", blob.descriptor))

	require.Equal(t, 4+2, fake.calls, "four ranges plus two resumes")
	require.Equal(t, fake.calls, fake.pinned, "ranges are pinned")
	require.Equal(t, float64(2), testutil.ToFloat64(p.metrics.rangeResumes))
	require.Equal(t, float64(1000), testutil.ToFloat64(p.metrics.receivedBytes))
	require.Len(t, pool.all, 1)
}

func TestHostReadPullDetectsCorruption(t *testing.T) {
	p, fake, _, blob := hostReadTestPuller(t, 1000, nil)
	fake.corrupt = true

	err := p.fetch(t.Context(), "blob", blob.descriptor)

	var failure *pullFailure
	require.ErrorAs(t, err, &failure)
	require.Equal(t, failureDigest, failure.reason)
}

func TestHostReadPullWithoutVerifySkipsHash(t *testing.T) {
	p, fake, _, blob := hostReadTestPuller(t, 1000, func(o *pullOptions) { o.Verify = false })
	fake.corrupt = true

	require.NoError(t, p.fetch(t.Context(), "blob", blob.descriptor))
	require.Equal(t, float64(1000), testutil.ToFloat64(p.metrics.receivedBytes))
}

func TestHostReadPullErrors(t *testing.T) {
	t.Run("host-read fails", func(t *testing.T) {
		p, fake, pool, blob := hostReadTestPuller(t, 1000, nil)
		fake.statErr = racersdk.ErrNotFound

		require.ErrorIs(t, p.fetch(t.Context(), "blob", blob.descriptor), racersdk.ErrNotFound)
		require.Len(t, pool.free, 1, "the memfd returns to the pool on failure")
	})

	t.Run("stat fails", func(t *testing.T) {
		p, fake, pool, blob := hostReadTestPuller(t, 1000, func(o *pullOptions) { o.RangeBytes = 300 })
		fake.statErr = racersdk.ErrUnavailable

		require.ErrorIs(t, p.fetch(t.Context(), "blob", blob.descriptor), racersdk.ErrUnavailable)
		require.Empty(t, pool.all, "no memfd is taken before Stat succeeds")
	})

	t.Run("retries exhausted", func(t *testing.T) {
		p, fake, _, blob := hostReadTestPuller(t, 1000, func(o *pullOptions) { o.RangeBytes = 300 })
		fake.failures = 1

		require.ErrorIs(t, p.fetch(t.Context(), "blob", blob.descriptor), io.ErrUnexpectedEOF)
	})

	t.Run("object larger than descriptor", func(t *testing.T) {
		p, fake, _, blob := hostReadTestPuller(t, 1000, nil)
		fake.data = append(fake.data, 0)

		var failure *pullFailure
		require.ErrorAs(t, p.fetch(t.Context(), "blob", blob.descriptor), &failure)
	})
}

func TestMemfdPool(t *testing.T) {
	pool := &memfdPool{}

	m, err := pool.get(10)
	require.NoError(t, err)
	require.EqualValues(t, 10, m.size)

	_, err = m.file.WriteAt([]byte("hello"), 2)
	require.NoError(t, err)

	view, err := m.view(7)
	require.NoError(t, err)
	require.Equal(t, "\x00\x00hello", string(view))

	_, err = m.view(11)
	require.Error(t, err)

	pool.put(m)

	again, err := pool.get(4096)
	require.NoError(t, err)
	require.Same(t, m, again, "a free memfd is reused")
	require.EqualValues(t, 4096, again.size)
	require.Nil(t, again.mapping, "growing drops the stale mapping")

	seals, err := unix.FcntlInt(again.file.Fd(), unix.F_GET_SEALS, 0)
	require.NoError(t, err)
	require.NotZero(t, seals&unix.F_SEAL_SHRINK, "sealed against shrinking")
	require.Zero(t, seals&unix.F_SEAL_WRITE, "still writable")
	require.Error(t, again.file.Truncate(1), "cannot shrink")

	other, err := pool.get(1)
	require.NoError(t, err)
	require.NotSame(t, m, other, "a busy memfd is not shared")

	pool.put(again)
	pool.put(other)
	require.NoError(t, pool.close())

	_, err = pool.get(1)
	require.Error(t, err)
}

func TestValidateDelivery(t *testing.T) {
	for name, tc := range map[string]struct {
		opts pullOptions
		ok   bool
	}{
		"default":            {pullOptions{Backend: "gantry"}, true},
		"stream":             {pullOptions{Backend: "gantry", Delivery: deliveryStream}, true},
		"host-read uds":      {pullOptions{Backend: "uds", Delivery: deliveryHostRead}, true},
		"host-read gantry":   {pullOptions{Backend: "gantry", Delivery: deliveryHostRead}, false},
		"host-read diagnose": {pullOptions{Backend: "uds", Delivery: deliveryHostRead, Verify: true, DiagnoseIntegrity: true}, false},
		"unknown":            {pullOptions{Backend: "uds", Delivery: "splice"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			err := validateDelivery(tc.opts)
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestParseOptionsDelivery(t *testing.T) {
	opts, err := parseOptions([]string{"--backend=uds", "--cache=blobs", "--delivery=host-read", "--range-bytes=1048576", "--verify=true"}, io.Discard)
	require.NoError(t, err, "host-read can verify ranges")
	require.Equal(t, deliveryHostRead, opts.pull.Delivery)

	_, err = parseOptions([]string{"--backend=uds", "--cache=blobs", "--range-bytes=1048576", "--verify=true"}, io.Discard)
	require.Error(t, err, "stream ranges cannot verify")

	_, err = parseOptions([]string{"--delivery=host-read"}, io.Discard)
	require.Error(t, err, "host-read needs uds")

	_, err = parseOptions([]string{"--backend=uds", "--cache=blobs", "--delivery=bogus"}, io.Discard)
	require.Error(t, err)

	_, err = newPuller(pullTestImage(t), pullOptions{Backend: "gantry", Delivery: deliveryHostRead}, pullTestMetrics())
	require.True(t, err != nil && !errors.Is(err, os.ErrNotExist))
}
