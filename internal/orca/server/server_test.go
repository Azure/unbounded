// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package server

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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
	OriginVal      origin.Origin
}

func (f *fakeEdgeAPI) HeadObject(ctx context.Context, bucket, key string) (origin.ObjectInfo, error) {
	return f.HeadObjectFunc(ctx, bucket, key)
}

func (f *fakeEdgeAPI) GetChunk(ctx context.Context, k chunk.Key, objectSize int64) (io.ReadCloser, error) {
	return f.GetChunkFunc(ctx, k, objectSize)
}

func (f *fakeEdgeAPI) Origin() origin.Origin { return f.OriginVal }

// fakeOrigin satisfies origin.Origin for handler tests. Only the
// fields used in the test need to be populated.
type fakeOrigin struct {
	HeadFunc     func(ctx context.Context, bucket, key string) (origin.ObjectInfo, error)
	GetRangeFunc func(ctx context.Context, bucket, key, etag string, off, n int64) (io.ReadCloser, error)
	ListFunc     func(ctx context.Context, bucket, prefix, marker string, max int) (origin.ListResult, error)
}

func (f *fakeOrigin) Head(ctx context.Context, bucket, key string) (origin.ObjectInfo, error) {
	return f.HeadFunc(ctx, bucket, key)
}

func (f *fakeOrigin) GetRange(ctx context.Context, bucket, key, etag string, off, n int64) (io.ReadCloser, error) {
	return f.GetRangeFunc(ctx, bucket, key, etag, off, n)
}

func (f *fakeOrigin) List(ctx context.Context, bucket, prefix, marker string, max int) (origin.ListResult, error) {
	return f.ListFunc(ctx, bucket, prefix, marker, max)
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

// TestHandleList covers the XML pass-through, prefix propagation,
// truncation, and empty-list handling.
func TestHandleList(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		prefix      string
		listResult  origin.ListResult
		listErr     error
		wantStatus  int
		wantKeys    []string
		wantTrunc   bool
		wantNextTok string
	}{
		{
			name:   "normal list",
			prefix: "alpha/",
			listResult: origin.ListResult{
				Entries: []origin.ObjectEntry{
					{Key: "alpha/one", Size: 3, ETag: "e1"},
					{Key: "alpha/two", Size: 5, ETag: "e2"},
				},
			},
			wantStatus: http.StatusOK,
			wantKeys:   []string{"alpha/one", "alpha/two"},
		},
		{
			name:       "empty list",
			prefix:     "missing/",
			listResult: origin.ListResult{},
			wantStatus: http.StatusOK,
			wantKeys:   nil,
		},
		{
			name: "truncated list",
			listResult: origin.ListResult{
				Entries:     []origin.ObjectEntry{{Key: "k1"}},
				IsTruncated: true,
				NextMarker:  "next-page",
			},
			wantStatus:  http.StatusOK,
			wantKeys:    []string{"k1"},
			wantTrunc:   true,
			wantNextTok: "next-page",
		},
		{
			name:       "origin error yields 502",
			listErr:    errors.New("upstream broken"),
			wantStatus: http.StatusBadGateway,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			or := &fakeOrigin{
				ListFunc: func(_ context.Context, bucket, prefix, _ string, _ int) (origin.ListResult, error) {
					if bucket != "b" {
						t.Errorf("bucket=%q want %q", bucket, "b")
					}

					if prefix != tt.prefix {
						t.Errorf("prefix=%q want %q", prefix, tt.prefix)
					}

					return tt.listResult, tt.listErr
				},
			}
			fc := &fakeEdgeAPI{OriginVal: or}
			h := NewEdgeHandler(fc, &config.Config{}, discardLogger())

			req := httptest.NewRequest(http.MethodGet,
				"/b/?list-type=2&prefix="+tt.prefix, nil)
			rr := httptest.NewRecorder()
			h.handleList(rr, req, "b")

			if rr.Code != tt.wantStatus {
				t.Errorf("status=%d want %d body=%s", rr.Code, tt.wantStatus, rr.Body.String())
			}

			if tt.wantStatus != http.StatusOK {
				return
			}

			var got struct {
				XMLName     xml.Name `xml:"ListBucketResult"`
				Name        string   `xml:"Name"`
				Prefix      string   `xml:"Prefix"`
				KeyCount    int      `xml:"KeyCount"`
				IsTruncated bool     `xml:"IsTruncated"`
				NextMarker  string   `xml:"NextContinuationToken"`
				Contents    []struct {
					Key string `xml:"Key"`
				} `xml:"Contents"`
			}
			if err := xml.Unmarshal(rr.Body.Bytes(), &got); err != nil {
				t.Fatalf("xml decode: %v body=%s", err, rr.Body.String())
			}

			if got.Name != "b" {
				t.Errorf("Name=%q want %q", got.Name, "b")
			}

			if got.Prefix != tt.prefix {
				t.Errorf("Prefix=%q want %q", got.Prefix, tt.prefix)
			}

			if got.KeyCount != len(tt.wantKeys) {
				t.Errorf("KeyCount=%d want %d", got.KeyCount, len(tt.wantKeys))
			}

			if got.IsTruncated != tt.wantTrunc {
				t.Errorf("IsTruncated=%v want %v", got.IsTruncated, tt.wantTrunc)
			}

			if got.NextMarker != tt.wantNextTok {
				t.Errorf("NextMarker=%q want %q", got.NextMarker, tt.wantNextTok)
			}

			gotKeys := make([]string, 0, len(got.Contents))
			for _, c := range got.Contents {
				gotKeys = append(gotKeys, c.Key)
			}

			if !equalStrings(gotKeys, tt.wantKeys) {
				t.Errorf("keys=%v want %v", gotKeys, tt.wantKeys)
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

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
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
