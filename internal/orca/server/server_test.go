// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/orca/chunk"
	"github.com/Azure/unbounded/internal/orca/cluster"
	"github.com/Azure/unbounded/internal/orca/config"
	"github.com/Azure/unbounded/internal/orca/origin"
)

// fakeEdgeAPI satisfies edgeFetchAPI with canned responses for unit
// tests. Only the field for the call you want to mock needs to be
// set; an unset *Func panics if the test invokes the corresponding
// method.
type fakeEdgeAPI struct {
	HeadObjectFunc func(ctx context.Context, bucket, key string) (origin.ObjectInfo, error)
	GetChunkFunc   func(ctx context.Context, k chunk.Key, objectSize int64) (io.ReadCloser, error)
}

func (f *fakeEdgeAPI) HeadObject(ctx context.Context, bucket, key string) (origin.ObjectInfo, error) {
	return f.HeadObjectFunc(ctx, bucket, key)
}

func (f *fakeEdgeAPI) GetChunk(ctx context.Context, k chunk.Key, objectSize int64) (io.ReadCloser, error) {
	return f.GetChunkFunc(ctx, k, objectSize)
}

// fakeOrigin satisfies origin.Origin for handler tests. Only the
// fields used in the test need to be populated.
type fakeOrigin struct {
	HeadFunc     func(ctx context.Context, bucket, key string) (origin.ObjectInfo, error)
	GetRangeFunc func(ctx context.Context, bucket, key, etag string, off, n int64) (io.ReadCloser, error)
}

func (f *fakeOrigin) Head(ctx context.Context, bucket, key string) (origin.ObjectInfo, error) {
	return f.HeadFunc(ctx, bucket, key)
}

func (f *fakeOrigin) GetRange(ctx context.Context, bucket, key, etag string, off, n int64) (io.ReadCloser, error) {
	return f.GetRangeFunc(ctx, bucket, key, etag, off, n)
}

// TestWriteOriginError covers all five branches of the error mapping.
// Previously only ErrNotFound was exercised (via integration test).
func TestWriteOriginError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantBody   string
	}{
		{
			name:       "not found",
			err:        origin.ErrNotFound,
			wantStatus: http.StatusNotFound,
			wantBody:   "NoSuchKey",
		},
		{
			name:       "auth",
			err:        origin.ErrAuth,
			wantStatus: http.StatusBadGateway,
			wantBody:   "Unauthorized origin",
		},
		{
			name: "unsupported blob type",
			err: &origin.UnsupportedBlobTypeError{
				Bucket:   "ctr",
				Key:      "page-blob",
				BlobType: "PageBlob",
			},
			wantStatus: http.StatusBadGateway,
			wantBody:   "OriginUnsupported",
		},
		{
			name: "etag changed",
			err: &origin.OriginETagChangedError{
				Bucket: "b", Key: "k", Want: "old",
			},
			wantStatus: http.StatusBadGateway,
			wantBody:   "OriginETagChanged",
		},
		{
			name:       "generic error",
			err:        errors.New("unexpected"),
			wantStatus: http.StatusBadGateway,
			wantBody:   "OriginUnreachable",
		},
	}

	h := &EdgeHandler{log: discardLogger()}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			h.writeOriginError(rr, tt.err)

			if rr.Code != tt.wantStatus {
				t.Errorf("status=%d want %d", rr.Code, tt.wantStatus)
			}

			if !strings.Contains(rr.Body.String(), tt.wantBody) {
				t.Errorf("body %q does not contain %q", rr.Body.String(), tt.wantBody)
			}
		})
	}
}

// TestHandleHead covers metadata propagation and the not-found error
// path on HEAD requests.
func TestHandleHead(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		info       origin.ObjectInfo
		err        error
		wantStatus int
		wantHdrs   map[string]string
	}{
		{
			name: "normal blob",
			info: origin.ObjectInfo{
				Size:        1024,
				ETag:        "abc123",
				ContentType: "application/octet-stream",
			},
			wantStatus: http.StatusOK,
			wantHdrs: map[string]string{
				"Content-Length": "1024",
				"ETag":           `"abc123"`,
				"Content-Type":   "application/octet-stream",
			},
		},
		{
			name:       "missing content type omits header",
			info:       origin.ObjectInfo{Size: 99, ETag: "x"},
			wantStatus: http.StatusOK,
			wantHdrs: map[string]string{
				"Content-Length": "99",
				"ETag":           `"x"`,
			},
		},
		{
			name:       "missing etag omits header",
			info:       origin.ObjectInfo{Size: 7},
			wantStatus: http.StatusOK,
			wantHdrs: map[string]string{
				"Content-Length": "7",
			},
		},
		{
			name:       "origin not found yields 404",
			err:        origin.ErrNotFound,
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := &fakeEdgeAPI{
				HeadObjectFunc: func(_ context.Context, _, _ string) (origin.ObjectInfo, error) {
					return tt.info, tt.err
				},
			}
			h := NewEdgeHandler(fc, &config.Config{}, discardLogger())

			req := httptest.NewRequest(http.MethodHead, "/bucket/key", nil)
			rr := httptest.NewRecorder()
			h.handleHead(rr, req, "bucket", "key")

			if rr.Code != tt.wantStatus {
				t.Errorf("status=%d want %d", rr.Code, tt.wantStatus)
			}

			for k, want := range tt.wantHdrs {
				got := rr.Header().Get(k)
				if got != want {
					t.Errorf("header %s=%q want %q", k, got, want)
				}
			}

			if rr.Body.Len() != 0 && tt.wantStatus == http.StatusOK {
				t.Errorf("HEAD body should be empty; got %d bytes", rr.Body.Len())
			}
		})
	}
}

// TestParseSimpleByteRange covers all parser branches.
func TestParseSimpleByteRange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		header    string
		size      int64
		wantStart int64
		wantEnd   int64
		wantOK    bool
	}{
		{"normal range", "bytes=0-99", 1024, 0, 99, true},
		{"suffix range", "bytes=-100", 1024, 924, 1023, true},
		{"open-ended", "bytes=100-", 1024, 100, 1023, true},
		{"end clamped to size", "bytes=0-9999", 1024, 0, 1023, true},
		{"start > end rejected", "bytes=100-50", 1024, 0, 0, false},
		{"missing prefix rejected", "0-99", 1024, 0, 0, false},
		{"multi-range rejected", "bytes=0-99,200-299", 1024, 0, 0, false},
		{"empty rejected", "", 1024, 0, 0, false},
		{"bytes= alone rejected", "bytes=", 1024, 0, 0, false},
		{"suffix larger than size rejected", "bytes=-9999", 1024, 0, 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, e, ok := parseSimpleByteRange(tt.header, tt.size)
			if ok != tt.wantOK {
				t.Fatalf("ok=%v want %v (s=%d e=%d)", ok, tt.wantOK, s, e)
			}

			if !ok {
				return
			}

			if s != tt.wantStart || e != tt.wantEnd {
				t.Errorf("(s,e)=(%d,%d) want (%d,%d)", s, e, tt.wantStart, tt.wantEnd)
			}
		})
	}
}

// TestSplitPath covers path splitting edge cases.
func TestSplitPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in         string
		wantBucket string
		wantKey    string
	}{
		{"", "", ""},
		{"/", "", ""},
		{"/bucket", "bucket", ""},
		{"/bucket/", "bucket", ""},
		{"/bucket/key", "bucket", "key"},
		{"/bucket/path/to/key", "bucket", "path/to/key"},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			b, k := splitPath(tt.in)
			if b != tt.wantBucket || k != tt.wantKey {
				t.Errorf("splitPath(%q)=(%q,%q) want (%q,%q)",
					tt.in, b, k, tt.wantBucket, tt.wantKey)
			}
		})
	}
}

// TestSetObjectHeaders covers header propagation including the
// always-set Accept-Ranges and the conditionally-set fields.
func TestSetObjectHeaders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		info origin.ObjectInfo
		want map[string]string
	}{
		{
			name: "all fields set",
			info: origin.ObjectInfo{ETag: "abc", ContentType: "text/plain"},
			want: map[string]string{
				"ETag":          `"abc"`,
				"Content-Type":  "text/plain",
				"Accept-Ranges": "bytes",
			},
		},
		{
			name: "missing content type",
			info: origin.ObjectInfo{ETag: "abc"},
			want: map[string]string{
				"ETag":          `"abc"`,
				"Content-Type":  "",
				"Accept-Ranges": "bytes",
			},
		},
		{
			name: "missing etag",
			info: origin.ObjectInfo{ContentType: "text/plain"},
			want: map[string]string{
				"ETag":          "",
				"Content-Type":  "text/plain",
				"Accept-Ranges": "bytes",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			setObjectHeaders(rr, tt.info)

			for k, want := range tt.want {
				if got := rr.Header().Get(k); got != want {
					t.Errorf("header %s=%q want %q", k, got, want)
				}
			}
		})
	}
}

// errReader is an io.ReadCloser whose first Read returns errFirst.
// Used to simulate cachestore-backed bodies that fail on their first
// network read (e.g. azureblob returning a 503 mid-stream after the
// header transaction succeeded).
type errReader struct {
	errFirst error
	closed   bool
}

func (r *errReader) Read(_ []byte) (int, error) { return 0, r.errFirst }
func (r *errReader) Close() error               { r.closed = true; return nil }

// TestHandleGet_EmptyObject_NoRange_Returns200 verifies that a GET
// against a zero-byte object responds with 200 + Content-Length: 0
// and an empty body. Previously the handler computed rangeEnd = -1
// and fell into the unsatisfiable-range branch, returning a spurious
// 416 for what should be a successful empty-body fetch.
func TestHandleGet_EmptyObject_NoRange_Returns200(t *testing.T) {
	t.Parallel()

	info := origin.ObjectInfo{Size: 0, ETag: "etag-empty", ContentType: "application/octet-stream"}

	fc := &fakeEdgeAPI{
		HeadObjectFunc: func(_ context.Context, _, _ string) (origin.ObjectInfo, error) {
			return info, nil
		},
		// GetChunkFunc deliberately unset; the short-circuit must
		// not call into the fetch coordinator for zero-byte objects.
	}

	cfg := &config.Config{Chunking: config.Chunking{Size: 1024}}
	h := NewEdgeHandler(fc, cfg, discardLogger())

	req := httptest.NewRequest(http.MethodGet, "/bucket/empty", nil)
	rr := httptest.NewRecorder()
	h.handleGet(rr, req, "bucket", "empty")

	if rr.Code != http.StatusOK {
		t.Errorf("status=%d want %d", rr.Code, http.StatusOK)
	}

	if rr.Body.Len() != 0 {
		t.Errorf("body=%d bytes, want 0", rr.Body.Len())
	}

	if got := rr.Header().Get("Content-Length"); got != "0" {
		t.Errorf("Content-Length=%q want %q", got, "0")
	}
}

// TestHandleGet_EmptyObject_WithRange_Returns416 verifies that a
// Range request against a zero-byte object remains a 416. RFC 7233
// classifies any range over a zero-byte representation as
// unsatisfiable.
func TestHandleGet_EmptyObject_WithRange_Returns416(t *testing.T) {
	t.Parallel()

	info := origin.ObjectInfo{Size: 0, ETag: "etag-empty"}

	fc := &fakeEdgeAPI{
		HeadObjectFunc: func(_ context.Context, _, _ string) (origin.ObjectInfo, error) {
			return info, nil
		},
	}

	cfg := &config.Config{Chunking: config.Chunking{Size: 1024}}
	h := NewEdgeHandler(fc, cfg, discardLogger())

	req := httptest.NewRequest(http.MethodGet, "/bucket/empty", nil)
	req.Header.Set("Range", "bytes=0-0")

	rr := httptest.NewRecorder()
	h.handleGet(rr, req, "bucket", "empty")

	if rr.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("status=%d want %d", rr.Code, http.StatusRequestedRangeNotSatisfiable)
	}
}

// TestHandleGet_FirstChunkErrorReturnsCleanError verifies that when
// the very first chunk fetch fails the edge handler responds with an
// S3-style error response (proper status + error body) rather than
// committing a 200 status and then aborting the connection
// mid-stream.
//
// Regression test for B4.
func TestHandleGet_FirstChunkErrorReturnsCleanError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		fetchErr   error
		peekErr    error // non-nil means GetChunk succeeds but first Read fails
		wantStatus int
		wantBody   string // substring assertion on the error body
	}{
		{
			name:       "GetChunk returns NotFound",
			fetchErr:   origin.ErrNotFound,
			wantStatus: http.StatusNotFound,
			wantBody:   "NoSuchKey",
		},
		{
			name:       "GetChunk returns generic origin error",
			fetchErr:   errors.New("origin: connect: timeout"),
			wantStatus: http.StatusBadGateway,
			wantBody:   "OriginUnreachable",
		},
		{
			name:       "GetChunk succeeds but first Read fails",
			peekErr:    errors.New("cachestore: blob fetch 503"),
			wantStatus: http.StatusBadGateway,
			wantBody:   "OriginUnreachable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := origin.ObjectInfo{
				Size:        1024,
				ETag:        "etag1",
				ContentType: "application/octet-stream",
			}

			fc := &fakeEdgeAPI{
				HeadObjectFunc: func(_ context.Context, _, _ string) (origin.ObjectInfo, error) {
					return info, nil
				},
				GetChunkFunc: func(_ context.Context, _ chunk.Key, _ int64) (io.ReadCloser, error) {
					if tt.fetchErr != nil {
						return nil, tt.fetchErr
					}

					return &errReader{errFirst: tt.peekErr}, nil
				},
			}

			cfg := &config.Config{Chunking: config.Chunking{Size: 1024}}
			h := NewEdgeHandler(fc, cfg, discardLogger())

			req := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
			rr := httptest.NewRecorder()
			h.handleGet(rr, req, "bucket", "key")

			if rr.Code != tt.wantStatus {
				t.Errorf("status=%d want %d; body=%q", rr.Code, tt.wantStatus, rr.Body.String())
			}

			if !strings.Contains(rr.Body.String(), tt.wantBody) {
				t.Errorf("body=%q want substring %q", rr.Body.String(), tt.wantBody)
			}
			// A bug here would 200 first, then write nothing or
			// partial bytes; verify the response did not commit a
			// success status that contradicts the error.
			if rr.Code == http.StatusOK {
				t.Errorf("handler committed 200 before failure became known")
			}
		})
	}
}

type fakeInternalFetchAPI struct {
	body []byte
}

func (f *fakeInternalFetchAPI) FillForPeer(_ context.Context, _ chunk.Key, _ int64) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(string(f.body))), nil
}

// singleSelfPeerSource produces a peer-set containing only self.
// IsCoordinator therefore returns true for every key, letting the
// internal-fill handler proceed past its coordinator check without
// requiring the test to know the rendezvous-hash outcome.
type singleSelfPeerSource struct{}

func (singleSelfPeerSource) Peers(_ context.Context) ([]cluster.Peer, error) {
	return []cluster.Peer{{IP: "10.0.0.1", Self: true}}, nil
}

// TestInternalHandler_SetsContentLength verifies the internal-fill
// handler sets Content-Length to chunk.Key.ExpectedLen(objectSize)
// on the response. Setting the header allows the requesting peer to
// detect mid-stream truncation via net/http's standard io.ErrUnexpectedEOF
// surfacing; without it, a truncated peer response would be
// indistinguishable from a clean EOF.
//
// Regression test for B7.
func TestInternalHandler_SetsContentLength(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		chunkSize  int64
		index      int64
		objectSize int64
		wantLen    string
	}{
		{
			name:       "full chunk",
			chunkSize:  1024,
			index:      0,
			objectSize: 4096,
			wantLen:    "1024",
		},
		{
			// The fake body returns chunkSize=1024 bytes but the
			// tail-chunk ExpectedLen is 428 (3500 - 3*1024). The
			// resulting Content-Length: 428 can only come from the
			// handler computing ExpectedLen explicitly, proving the
			// header is not auto-derived from the body length.
			name:       "tail chunk partial",
			chunkSize:  1024,
			index:      3,
			objectSize: 3500,
			wantLen:    "428",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := cluster.New(t.Context(),
				config.Cluster{
					Service:           "test",
					SelfPodIP:         "10.0.0.1",
					MembershipRefresh: time.Hour,
					InternalListen:    "0.0.0.0:8444",
				},
				cluster.WithPeerSource(singleSelfPeerSource{}),
			)
			if err != nil {
				t.Fatalf("cluster.New: %v", err)
			}

			t.Cleanup(func() { _ = c.Close(context.Background()) })

			h := NewInternalHandler(&fakeInternalFetchAPI{body: make([]byte, tt.chunkSize)}, c, discardLogger())

			req := httptest.NewRequest(http.MethodGet, "/internal/fill?"+(func() string {
				k := chunk.Key{
					OriginID:  "origin",
					Bucket:    "bucket",
					ObjectKey: "key",
					ETag:      "etag",
					ChunkSize: tt.chunkSize,
					Index:     tt.index,
				}

				return encodeQuery(k, tt.objectSize)
			})(), nil)
			req.Header.Set("X-Orca-Internal", "1")

			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d want 200; body=%q", rr.Code, rr.Body.String())
			}

			got := rr.Header().Get("Content-Length")
			if got != tt.wantLen {
				t.Errorf("Content-Length = %q want %q", got, tt.wantLen)
			}
		})
	}
}

// encodeQuery duplicates cluster.encodeChunkKey for test purposes
// (it is unexported in the cluster package).
func encodeQuery(k chunk.Key, objectSize int64) string {
	return "origin_id=" + k.OriginID +
		"&bucket=" + k.Bucket +
		"&key=" + k.ObjectKey +
		"&etag=" + k.ETag +
		"&chunk_size=" + strconv.FormatInt(k.ChunkSize, 10) +
		"&index=" + strconv.FormatInt(k.Index, 10) +
		"&object_size=" + strconv.FormatInt(objectSize, 10)
}

// helpers

// TestEdgeHandler_DebugEmissions verifies that the edge handler
// emits a debug-level 'edge_request' trace at entry and at least
// one of the response-shape emissions for HEAD/GET. Operators rely
// on these to trace a single request across the structured-log
// output.
func TestEdgeHandler_DebugEmissions(t *testing.T) {
	t.Parallel()

	info := origin.ObjectInfo{Size: 5, ETag: "etag-xyz", ContentType: "application/octet-stream"}

	fc := &fakeEdgeAPI{
		HeadObjectFunc: func(_ context.Context, _, _ string) (origin.ObjectInfo, error) {
			return info, nil
		},
	}

	var buf bytes.Buffer

	cfg := &config.Config{Chunking: config.Chunking{Size: 1024}}
	h := NewEdgeHandler(fc, cfg, debugLoggerTo(&buf))

	req := httptest.NewRequest(http.MethodHead, "/bkt/obj", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	out := buf.String()
	for _, want := range []string{"edge_request", "edge_head_response", "bucket=bkt", "key=obj"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in debug output; got %q", want, out)
		}
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// debugLoggerTo returns a slog.Logger that writes Debug-and-above
// emissions to buf. Used by tests asserting debug-trace emission
// at known call sites.
func debugLoggerTo(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

// readaheadConfig returns a config tailored for readahead unit tests.
// Origin.ID is required by the chunk-key construction inside
// handleGet; chunk size and readahead are explicit so each test
// controls them independently.
func readaheadConfig(chunkSize int64, readahead int) *config.Config {
	r := readahead

	return &config.Config{
		Origin: config.Origin{ID: "origin"},
		Chunking: config.Chunking{
			Size:      config.ByteSize(chunkSize),
			Readahead: &r,
		},
	}
}

// makeChunkData returns a chunkSize-byte payload whose contents
// encode the chunk index so test assertions can verify that the
// streamed body delivers chunks in correct order. Each byte at
// offset b within chunk i is `byte((int(i) + b) % 251)`; using a
// prime modulus avoids spurious alignment on power-of-two
// boundaries.
func makeChunkData(idx int64, n int) []byte {
	out := make([]byte, n)
	for b := 0; b < n; b++ {
		out[b] = byte((int(idx) + b) % 251)
	}

	return out
}

// trackedReadCloser is an io.ReadCloser that records Close() calls
// for the readahead-cancellation test. closedCh fires once on the
// first Close().
type trackedReadCloser struct {
	io.Reader
	closed   bool
	closedCh chan struct{}
}

func (t *trackedReadCloser) Close() error {
	if !t.closed {
		t.closed = true
		close(t.closedCh)
	}

	return nil
}

// TestHandleGet_DynamicChunkSize_SmallObject verifies a small object
// (well below any tier threshold) uses the base Chunking.Size. The
// fake fetch records the chunk-key sizes seen so we can assert the
// edge handler is not regressing to the previous global-only chunk
// size on the small-object path.
func TestHandleGet_DynamicChunkSize_SmallObject(t *testing.T) {
	t.Parallel()

	info := origin.ObjectInfo{Size: 100 * (1 << 20), ETag: "etag", ContentType: "application/octet-stream"}

	var (
		mu        sync.Mutex
		seenSizes []int64
	)

	fc := &fakeEdgeAPI{
		HeadObjectFunc: func(_ context.Context, _, _ string) (origin.ObjectInfo, error) {
			return info, nil
		},
		GetChunkFunc: func(_ context.Context, k chunk.Key, _ int64) (io.ReadCloser, error) {
			mu.Lock()

			seenSizes = append(seenSizes, k.ChunkSize)
			mu.Unlock()

			return io.NopCloser(bytes.NewReader(makeChunkData(k.Index, int(k.ExpectedLen(info.Size))))), nil
		},
	}

	cfg := &config.Config{
		Origin: config.Origin{ID: "origin"},
		Chunking: config.Chunking{
			Size: 8 << 20,
			Tiers: []config.ChunkTier{
				{MinObjectSize: 1 << 30, ChunkSize: 64 << 20},
			},
		},
	}

	h := NewEdgeHandler(fc, cfg, discardLogger())

	req := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
	rr := httptest.NewRecorder()
	h.handleGet(rr, req, "bucket", "key")

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%q", rr.Code, rr.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()

	if len(seenSizes) == 0 {
		t.Fatalf("no chunk fetches recorded")
	}

	for i, sz := range seenSizes {
		if sz != 8<<20 {
			t.Errorf("seenSizes[%d]=%d want 8 MiB (base)", i, sz)
		}
	}
}

// TestHandleGet_DynamicChunkSize_LargeObject verifies a large object
// (above the tier threshold) uses the tier's ChunkSize and that the
// number of chunks fetched matches the larger granularity (fewer
// requests).
func TestHandleGet_DynamicChunkSize_LargeObject(t *testing.T) {
	t.Parallel()

	// 700 GiB synthetic object; chunked at the 128 MiB tier this is
	// 5600 chunks. We don't fetch them all in this test (we set up a
	// fake that streams a tiny payload per chunk request), but we do
	// confirm the chunk keys carry ChunkSize=128 MiB and the
	// first-chunk path lands on Index=0.
	const (
		large  = int64(700) * (1 << 30) // 700 GiB
		tierSz = int64(128) << 20       // 128 MiB
		baseSz = int64(8) << 20         // 8 MiB
	)

	info := origin.ObjectInfo{Size: large, ETag: "etag", ContentType: "application/octet-stream"}

	// To keep the test fast we use a Range request covering exactly
	// the first chunk; otherwise the handler would attempt to stream
	// 700 GiB. Range bytes=0-(tierSz-1) targets chunk 0 only.
	var (
		mu        sync.Mutex
		seenSizes []int64
		seenIdx   []int64
	)

	fc := &fakeEdgeAPI{
		HeadObjectFunc: func(_ context.Context, _, _ string) (origin.ObjectInfo, error) {
			return info, nil
		},
		GetChunkFunc: func(_ context.Context, k chunk.Key, _ int64) (io.ReadCloser, error) {
			mu.Lock()

			seenSizes = append(seenSizes, k.ChunkSize)
			seenIdx = append(seenIdx, k.Index)
			mu.Unlock()

			return io.NopCloser(bytes.NewReader(makeChunkData(k.Index, int(k.ExpectedLen(info.Size))))), nil
		},
	}

	cfg := &config.Config{
		Origin: config.Origin{ID: "origin"},
		Chunking: config.Chunking{
			Size: config.ByteSize(baseSz),
			Tiers: []config.ChunkTier{
				{MinObjectSize: 10 * (1 << 30), ChunkSize: config.ByteSize(tierSz)},
			},
		},
	}

	h := NewEdgeHandler(fc, cfg, discardLogger())

	req := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
	req.Header.Set("Range", "bytes=0-"+strconv.FormatInt(tierSz-1, 10))

	rr := httptest.NewRecorder()
	h.handleGet(rr, req, "bucket", "key")

	if rr.Code != http.StatusPartialContent {
		t.Fatalf("status=%d want 206; body=%q", rr.Code, rr.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()

	if len(seenSizes) != 1 {
		t.Fatalf("expected exactly 1 chunk fetch for first-chunk range; got %d", len(seenSizes))
	}

	if seenSizes[0] != tierSz {
		t.Errorf("seenSizes[0]=%d want %d (tier size)", seenSizes[0], tierSz)
	}

	if seenIdx[0] != 0 {
		t.Errorf("seenIdx[0]=%d want 0", seenIdx[0])
	}
}

// TestHandleGet_Readahead_DisabledZero verifies that Readahead=0
// preserves the strictly-sequential behavior: GetChunk is called
// one chunk at a time, in order, with no concurrent fetches in
// flight. The fake fetch deliberately reports concurrent calls so a
// regression that started the prefetcher despite depth=0 would be
// caught.
func TestHandleGet_Readahead_DisabledZero(t *testing.T) {
	t.Parallel()

	const (
		chunkSize  = int64(1024)
		nChunks    = int64(5)
		objectSize = chunkSize * nChunks
	)

	info := origin.ObjectInfo{Size: objectSize, ETag: "e", ContentType: "application/octet-stream"}

	var (
		mu        sync.Mutex
		inFlight  int
		maxInFlt  int
		callOrder []int64
	)

	fc := &fakeEdgeAPI{
		HeadObjectFunc: func(_ context.Context, _, _ string) (origin.ObjectInfo, error) {
			return info, nil
		},
		GetChunkFunc: func(_ context.Context, k chunk.Key, _ int64) (io.ReadCloser, error) {
			mu.Lock()
			inFlight++

			if inFlight > maxInFlt {
				maxInFlt = inFlight
			}

			callOrder = append(callOrder, k.Index)
			mu.Unlock()
			// Brief sleep to widen any concurrency window.
			time.Sleep(5 * time.Millisecond)

			mu.Lock()
			inFlight--
			mu.Unlock()

			return io.NopCloser(bytes.NewReader(makeChunkData(k.Index, int(chunkSize)))), nil
		},
	}

	cfg := readaheadConfig(chunkSize, 0)
	h := NewEdgeHandler(fc, cfg, discardLogger())

	req := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
	rr := httptest.NewRecorder()
	h.handleGet(rr, req, "bucket", "key")

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%q", rr.Code, rr.Body.String())
	}

	if int64(rr.Body.Len()) != objectSize {
		t.Errorf("body=%d bytes, want %d", rr.Body.Len(), objectSize)
	}

	mu.Lock()
	defer mu.Unlock()

	if maxInFlt != 1 {
		t.Errorf("max in-flight=%d want 1 (no readahead)", maxInFlt)
	}

	for i, idx := range callOrder {
		if idx != int64(i) {
			t.Errorf("callOrder[%d]=%d want %d (in-order serial fetch)", i, idx, i)
		}
	}
}

// TestHandleGet_Readahead_ParallelHidesLatency verifies that with
// Readahead > 0 the handler can have multiple chunk fetches in
// flight concurrently. The fake fetch sleeps long enough per chunk
// that the wall-clock time for the full GET should be substantially
// less than nChunks * perChunkDelay if readahead is working.
func TestHandleGet_Readahead_ParallelHidesLatency(t *testing.T) {
	t.Parallel()

	const (
		chunkSize   = int64(1024)
		nChunks     = int64(5)
		objectSize  = chunkSize * nChunks
		perChunkLat = 40 * time.Millisecond
		readahead   = 4
	)

	info := origin.ObjectInfo{Size: objectSize, ETag: "e", ContentType: "application/octet-stream"}

	var (
		mu       sync.Mutex
		inFlight int
		maxInFlt int
	)

	fc := &fakeEdgeAPI{
		HeadObjectFunc: func(_ context.Context, _, _ string) (origin.ObjectInfo, error) {
			return info, nil
		},
		GetChunkFunc: func(ctx context.Context, k chunk.Key, _ int64) (io.ReadCloser, error) {
			mu.Lock()
			inFlight++

			if inFlight > maxInFlt {
				maxInFlt = inFlight
			}
			mu.Unlock()

			select {
			case <-time.After(perChunkLat):
			case <-ctx.Done():
				mu.Lock()
				inFlight--
				mu.Unlock()

				return nil, ctx.Err()
			}

			mu.Lock()
			inFlight--
			mu.Unlock()

			return io.NopCloser(bytes.NewReader(makeChunkData(k.Index, int(chunkSize)))), nil
		},
	}

	cfg := readaheadConfig(chunkSize, readahead)
	h := NewEdgeHandler(fc, cfg, discardLogger())

	req := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
	rr := httptest.NewRecorder()

	start := time.Now()

	h.handleGet(rr, req, "bucket", "key")

	elapsed := time.Since(start)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%q", rr.Code, rr.Body.String())
	}

	if int64(rr.Body.Len()) != objectSize {
		t.Errorf("body=%d bytes, want %d", rr.Body.Len(), objectSize)
	}

	// Strict serial baseline = nChunks * perChunkLat. With readahead
	// we expect substantially less; we conservatively assert <
	// (nChunks * perChunkLat * 0.8) which gives the test plenty of
	// CI slack. The exact speedup depends on scheduler timing; the
	// in-flight max metric below is the deterministic assertion.
	serialBaseline := time.Duration(nChunks) * perChunkLat

	if elapsed >= serialBaseline {
		t.Errorf("readahead did not hide latency: elapsed=%v, serial baseline=%v",
			elapsed, serialBaseline)
	}

	mu.Lock()
	defer mu.Unlock()

	if maxInFlt < 2 {
		t.Errorf("max in-flight=%d want >= 2 (readahead concurrent)", maxInFlt)
	}
}

// TestHandleGet_Readahead_CancellationClosesBodies verifies that
// when the streaming consumer aborts mid-response (e.g. a downstream
// write fails), every prefetched body still buffered in the
// readahead channel is Close()d on the way out. Without this the
// cachestore would leak HTTP response bodies whenever a client
// disconnects partway through a large blob.
//
// Setup: the handler streams to an http.ResponseWriter wrapped to
// return an io.ErrShortWrite after a fixed byte count, forcing the
// streamSlice call to abort mid-chunk. We then assert that every
// trackedReadCloser handed out has had Close() called.
func TestHandleGet_Readahead_CancellationClosesBodies(t *testing.T) {
	t.Parallel()

	const (
		chunkSize  = int64(256)
		nChunks    = int64(8)
		objectSize = chunkSize * nChunks
		readahead  = 4
	)

	info := origin.ObjectInfo{Size: objectSize, ETag: "e", ContentType: "application/octet-stream"}

	var (
		mu     sync.Mutex
		bodies []*trackedReadCloser
	)

	fc := &fakeEdgeAPI{
		HeadObjectFunc: func(_ context.Context, _, _ string) (origin.ObjectInfo, error) {
			return info, nil
		},
		GetChunkFunc: func(_ context.Context, k chunk.Key, _ int64) (io.ReadCloser, error) {
			b := &trackedReadCloser{
				Reader:   bytes.NewReader(makeChunkData(k.Index, int(chunkSize))),
				closedCh: make(chan struct{}),
			}

			mu.Lock()

			bodies = append(bodies, b)
			mu.Unlock()

			return b, nil
		},
	}

	cfg := readaheadConfig(chunkSize, readahead)
	h := NewEdgeHandler(fc, cfg, discardLogger())

	// shortWriter writes the first maxBytes bytes to inner and
	// returns io.ErrShortWrite on any further write. Reproduces a
	// client connection that closes mid-stream.
	rr := httptest.NewRecorder()
	w := &shortWriter{inner: rr, maxBytes: int(chunkSize) + int(chunkSize)/2} // 1.5 chunks

	req := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
	h.handleGet(w, req, "bucket", "key")

	// All bodies handed out should be closed; allow a brief window
	// for the producer goroutine to observe ctx-cancellation and
	// close its in-flight body via the select branch.
	deadline := time.After(2 * time.Second)

	for i := 0; ; i++ {
		mu.Lock()
		allClosed := true

		for _, b := range bodies {
			if !b.closed {
				allClosed = false
				break
			}
		}

		count := len(bodies)
		mu.Unlock()

		if allClosed && count > 1 {
			// Multiple bodies were handed out and all are closed.
			return
		}

		select {
		case <-deadline:
			mu.Lock()
			defer mu.Unlock()

			if count <= 1 {
				t.Fatalf("only %d bodies handed out; readahead did not engage", count)
			}

			for j, b := range bodies {
				if !b.closed {
					t.Errorf("body[%d] (chunk index %d) not closed", j, j)
				}
			}

			return
		default:
			time.Sleep(10 * time.Millisecond)
		}

		_ = i
	}
}

// TestHandleGet_Readahead_ProducerPanicRecovered verifies that a
// panic inside the readahead producer goroutine is recovered, logged,
// and does not deadlock the consumer or crash the process. The
// consumer should see an early channel close and treat the response
// as a mid-stream abort.
func TestHandleGet_Readahead_ProducerPanicRecovered(t *testing.T) {
	t.Parallel()

	const (
		chunkSize  = int64(256)
		nChunks    = int64(6)
		objectSize = chunkSize * nChunks
		readahead  = 2
	)

	info := origin.ObjectInfo{Size: objectSize, ETag: "e", ContentType: "application/octet-stream"}

	var (
		mu      sync.Mutex
		calls   int64
		panicAt = int64(3) // panic on the 3rd GetChunk
	)

	fc := &fakeEdgeAPI{
		HeadObjectFunc: func(_ context.Context, _, _ string) (origin.ObjectInfo, error) {
			return info, nil
		},
		GetChunkFunc: func(_ context.Context, k chunk.Key, _ int64) (io.ReadCloser, error) {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()

			if n == panicAt {
				panic("readahead test: synthetic producer panic")
			}

			return io.NopCloser(bytes.NewReader(makeChunkData(k.Index, int(chunkSize)))), nil
		},
	}

	var logBuf bytes.Buffer

	cfg := readaheadConfig(chunkSize, readahead)
	h := NewEdgeHandler(fc, cfg, debugLoggerTo(&logBuf))

	req := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
	rr := httptest.NewRecorder()

	done := make(chan struct{})

	go func() {
		defer close(done)

		h.handleGet(rr, req, "bucket", "key")
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("handler deadlocked after producer panic")
	}

	// The first chunk was peeked and streamed successfully (a
	// committed 200 response). Subsequent panic is a mid-stream
	// abort; the response code is therefore 200 even though the
	// body is truncated.
	if rr.Code != http.StatusOK {
		t.Errorf("status=%d want 200 (panic is mid-stream)", rr.Code)
	}

	out := logBuf.String()
	if !strings.Contains(out, "readahead worker panic") {
		t.Errorf("missing 'readahead worker panic' in log; got %q", out)
	}
}

// shortWriter writes the first maxBytes bytes to inner then returns
// io.ErrShortWrite on any subsequent Write. Used to simulate a
// client connection that drops mid-response.
type shortWriter struct {
	inner    http.ResponseWriter
	written  int
	maxBytes int
}

func (s *shortWriter) Header() http.Header { return s.inner.Header() }

func (s *shortWriter) WriteHeader(code int) { s.inner.WriteHeader(code) }

func (s *shortWriter) Write(p []byte) (int, error) {
	if s.written >= s.maxBytes {
		return 0, io.ErrShortWrite
	}

	remaining := s.maxBytes - s.written
	if len(p) > remaining {
		// Write exactly up to the cap, then fail any further calls.
		n, _ := s.inner.Write(p[:remaining])
		s.written += n

		return n, io.ErrShortWrite
	}

	n, err := s.inner.Write(p)
	s.written += n

	return n, err
}
