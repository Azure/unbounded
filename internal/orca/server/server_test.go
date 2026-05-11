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
	"strings"
	"testing"

	"github.com/Azure/unbounded/internal/orca/chunk"
	"github.com/Azure/unbounded/internal/orca/config"
	"github.com/Azure/unbounded/internal/orca/origin"
)

// fakeEdgeAPI satisfies edgeFetchAPI with canned responses for unit
// tests. Only the field for the call you want to mock needs to be
// set; an unset *Func panics if the test invokes the corresponding
// method.
type fakeEdgeAPI struct {
	HeadObjectFunc func(ctx context.Context, bucket, key string) (origin.ObjectInfo, error)
	GetChunkFunc   func(ctx context.Context, k chunk.Key) (io.ReadCloser, error)
	OriginVal      origin.Origin
}

func (f *fakeEdgeAPI) HeadObject(ctx context.Context, bucket, key string) (origin.ObjectInfo, error) {
	return f.HeadObjectFunc(ctx, bucket, key)
}

func (f *fakeEdgeAPI) GetChunk(ctx context.Context, k chunk.Key) (io.ReadCloser, error) {
	return f.GetChunkFunc(ctx, k)
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
