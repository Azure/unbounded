// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package object

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/Azure/unbounded/pkg/racersdk"
)

func sidecarTestMetadata(t *testing.T, size int, tag string) racersdk.Metadata {
	t.Helper()

	return racersdk.Metadata{Size: int64(size), ETag: tag, ExpiresAt: time.Unix(0, 0), ContentType: "text/plain"}
}

func sidecarTestHandler(t *testing.T, origin racersdk.Origin) (http.Handler, *racersdk.Client) {
	t.Helper()

	client := testSDKClient(t, origin)

	h, err := NewSidecar(client, SidecarConfig{Namespace: "test", Buckets: []string{"bucket"}})
	if err != nil {
		t.Fatal(err)
	}

	return h, client
}

func sidecarTestOrigin(t *testing.T, data, tag string, calls *atomic.Int32) racersdk.Origin {
	t.Helper()
	m := sidecarTestMetadata(t, len(data), tag)

	return func(_ context.Context, r racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		if calls != nil {
			calls.Add(1)
		}

		if r.Authorization != "" {
			t.Error("incoming authorization was forwarded")
		}

		if r.Head {
			return m, nil, nil
		}

		if r.ETag != m.ETag {
			t.Error("read was not pinned to Stat metadata")
		}

		if r.Offset >= m.Size {
			return m, nil, nil
		}

		return m, io.NopCloser(strings.NewReader(data[r.Offset:min(r.Offset+r.Length, m.Size)])), nil
	}
}

func sidecarTestRequest(h http.Handler, method, target string, headers http.Header) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	if headers != nil {
		r.Header = headers
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	return w
}

func TestSidecarReads(t *testing.T) {
	var calls atomic.Int32

	h, _ := sidecarTestHandler(t, sidecarTestOrigin(t, "0123456789", `"opaque,tag\\value"`, &calls))
	for _, tc := range []struct {
		name, method, rangeValue, body, contentRange string
		status                                       int
	}{
		{"full", "GET", "", "0123456789", "", 200},
		{"head", "HEAD", "", "", "", 200},
		{"bounded", "GET", "bytes=2-5", "2345", "bytes 2-5/10", 206},
		{"open", "GET", "bytes=7-", "789", "bytes 7-9/10", 206},
		{"suffix", "GET", "bytes=-3", "789", "bytes 7-9/10", 206},
		{"clamped", "GET", "bytes=8-999", "89", "bytes 8-9/10", 206},
		{"large suffix", "GET", "bytes=-999", "0123456789", "bytes 0-9/10", 206},
		{"one byte", "GET", "bytes=0-0", "0", "bytes 0-0/10", 206},
		{"head range", "HEAD", "bytes=2-5", "", "bytes 2-5/10", 206},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := calls.Load()

			headers := http.Header{"Authorization": {"AWS4-HMAC-SHA256 incoming-secret"}}
			if tc.rangeValue != "" {
				headers.Set("Range", tc.rangeValue)
			}

			w := sidecarTestRequest(h, tc.method, "/bucket/key?versionId=version%2B1", headers)
			if w.Code != tc.status || w.Body.String() != tc.body {
				t.Fatalf("response: %d %q", w.Code, w.Body.String())
			}

			if w.Header().Get("ETag") != `"opaque,tag\\value"` || w.Header().Get("Content-Range") != tc.contentRange || w.Header().Get("Content-Type") != "text/plain" || w.Header().Get("Accept-Ranges") != "bytes" || w.Header().Get("x-amz-version-id") != "version+1" {
				t.Fatalf("headers: %v", w.Header())
			}

			if tc.method == "HEAD" {
				if calls.Load()-before != 1 {
					t.Fatal("HEAD opened a stream")
				}
			} else if w.Header().Get("Content-Length") != strconv.Itoa(len(tc.body)) {
				t.Fatal("wrong content length")
			}
		})
	}
}

func TestSidecarConditions(t *testing.T) {
	var calls atomic.Int32

	tag := `"a,b\c"`

	h, _ := sidecarTestHandler(t, sidecarTestOrigin(t, "data", tag, &calls))
	for _, tc := range []struct {
		name, match, none string
		status            int
	}{
		{"match", tag, "", 200},
		{"match list", `"other", ` + tag, "", 200},
		{"match wildcard", "*", "", 200},
		{"match absent", `"other"`, "", 412},
		{"weak match fails", "W/" + tag, "", 412},
		{"none", "", tag, 304},
		{"none weak", "", "W/" + tag, 304},
		{"none list", "", `"other", W/` + tag, 304},
		{"none wildcard", "", "*", 304},
		{"none absent", "", `"other"`, 200},
		{"match precedence", `"other"`, "*", 412},
		{"both", "*", tag, 304},
		{"unquoted", "bare", "", 400},
		{"unterminated", `"x`, "", 400},
		{"bad list", tag + " suffix", "", 400},
		{"wildcard list", "*, " + tag, "", 400},
		{"space in tag", `"a b"`, "", 400},
		{"empty list", ",,", "", 400},
		{"empty tag valid", `""`, "", 412},
		{"empty list elements", ", " + tag + ",,", "", 200},
	} {
		for _, method := range []string{"GET", "HEAD"} {
			t.Run(tc.name+method, func(t *testing.T) {
				before := calls.Load()

				headers := http.Header{}
				if tc.match != "" {
					headers.Set("If-Match", tc.match)
				}

				if tc.none != "" {
					headers.Set("If-None-Match", tc.none)
				}

				w := sidecarTestRequest(h, method, "/bucket/key", headers)
				if w.Code != tc.status {
					t.Fatalf("status %d want %d: %s", w.Code, tc.status, w.Body.String())
				}

				if method == "HEAD" || tc.status == 304 {
					if w.Body.Len() != 0 {
						t.Fatal("unexpected response body")
					}
				}

				if tc.status == 304 && w.Header().Get("ETag") != tag {
					t.Fatal("missing 304 ETag")
				}

				if tc.status != 200 && calls.Load()-before > 1 {
					t.Fatal("condition opened a stream")
				}
			})
		}
	}

	w := sidecarTestRequest(h, "GET", "/bucket/key", http.Header{"If-Match": {`"other"`, tag}})
	if w.Code != 200 {
		t.Fatal("multiple header lines were not treated as a list")
	}

	w = sidecarTestRequest(h, "GET", "/bucket/key", http.Header{"If-None-Match": {""}})
	if w.Code != 400 {
		t.Fatal("empty condition accepted")
	}
}

func TestSidecarRejectedRequests(t *testing.T) {
	var calls atomic.Int32

	h, _ := sidecarTestHandler(t, sidecarTestOrigin(t, "data", `"v"`, &calls))
	for _, tc := range []struct {
		method, target string
		headers        http.Header
		status         int
	}{
		{"PUT", "/bucket/key", nil, 405},
		{"POST", "/bucket/key", nil, 405},
		{"DELETE", "/bucket/key", nil, 405},
		{"OPTIONS", "/bucket/key", nil, 405},
		{"GET", "/", nil, 400},
		{"GET", "/bucket", nil, 400},
		{"GET", "/bucket/", nil, 400},
		{"GET", "/bucket%2Fkey", nil, 400},
		{"GET", "/other/key", nil, 403},
		{"GET", "/bucket/key?list-type=2", nil, 501},
		{"GET", "/bucket/key?acl", nil, 400},
		{"GET", "/bucket/key?partNumber=1", nil, 501},
		{"GET", "/bucket/key?response-content-type=text/plain", nil, 501},
		{"GET", "/bucket/key?X-Amz-Signature=secret", nil, 501},
		{"GET", "/bucket/key?versionId=", nil, 400},
		{"GET", "/bucket/key?versionId=1&versionId=2", nil, 400},
		{"GET", "/bucket/key?versionId=%zz", nil, 400},
		{"GET", "/bucket/key?versionId=a;b", nil, 400},
		{"GET", "/bucket/key?versionId=a%0D%0AX-Injected%3Ayes", nil, 400},
		{"GET", "/bucket/key?versionId=a%00", nil, 400},
		{"GET", "/bucket/key?versionId=a%7F", nil, 400},
		{"GET", "/bucket/key?versionId=%FF", nil, 400},
		{"GET", "/bucket/" + strings.Repeat("k", 1025), nil, 400},
		{"GET", "/bucket/%FF", nil, 400},
		{"GET", "/bucket/key?x-id=ListObjects", nil, 501},
		{"GET", "/bucket/key?x-id=HeadObject", nil, 501},
		{"HEAD", "/bucket/key?x-id=GetObject", nil, 501},
		{"GET", "/bucket/key", http.Header{"If-Modified-Since": {"Tue, 06 Oct 2026 00:00:00 GMT"}}, 501},
		{"GET", "/bucket/key", http.Header{"If-Unmodified-Since": {""}}, 501},
		{"GET", "/bucket/key", http.Header{"If-Range": {`"v"`}}, 501},
	} {
		t.Run(tc.method+tc.target+strconv.Itoa(tc.status), func(t *testing.T) {
			before := calls.Load()

			w := sidecarTestRequest(h, tc.method, tc.target, tc.headers)
			if w.Code != tc.status {
				t.Fatalf("status %d want %d", w.Code, tc.status)
			}

			if calls.Load() != before {
				t.Fatal("invalid request reached Racer")
			}

			if tc.status == 405 && w.Header().Get("Allow") != "GET, HEAD" {
				t.Fatal("missing Allow")
			}

			if tc.method == "HEAD" {
				if w.Body.Len() != 0 {
					t.Fatal("HEAD error body")
				}

				return
			}

			var parsed struct {
				XMLName xml.Name
				Code    string
				Message string
			}
			if err := xml.Unmarshal(w.Body.Bytes(), &parsed); err != nil || parsed.XMLName.Local != "Error" || parsed.Code == "" || parsed.Message == "" {
				t.Fatalf("invalid error: %s", w.Body.String())
			}

			if strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "bucket") {
				t.Fatal("error leaked request data")
			}
		})
	}
}

func TestSidecarInvalidRanges(t *testing.T) {
	h, _ := sidecarTestHandler(t, sidecarTestOrigin(t, "data", `"v"`, nil))
	for _, tc := range []struct {
		value  string
		status int
	}{
		{"bytes=4-", 416},
		{"bytes=-0", 416},
		{"bytes=999-1000", 416},
		{"bytes=2-1", 400},
		{"bytes=0-1,2-3", 400},
		{"items=0-1", 400},
		{"bytes=", 400},
		{"bytes=-", 400},
		{"bytes=+1-2", 400},
		{"bytes=0-+1", 400},
		{"bytes=0-1-2", 400},
		{"bytes= 0-1", 400},
		{"bytes=18446744073709551616-", 400},
		{"bytes=-bad", 400},
		{"bytes=-18446744073709551616", 400},
		{"", 400},
	} {
		t.Run(tc.value, func(t *testing.T) {
			w := sidecarTestRequest(h, "GET", "/bucket/key", http.Header{"Range": {tc.value}})
			if w.Code != tc.status {
				t.Fatalf("status %d want %d", w.Code, tc.status)
			}

			if tc.status == 416 && w.Header().Get("Content-Range") != "bytes */4" {
				t.Fatal("missing unsatisfied range size")
			}
		})
	}

	w := sidecarTestRequest(h, "GET", "/bucket/key", http.Header{"Range": {"bytes=0-1", "bytes=2-3"}})
	if w.Code != 400 {
		t.Fatal("multiple ranges accepted")
	}
}

func TestSidecarExactIdentity(t *testing.T) {
	for _, key := range []string{"a//b/../c", "/leading", "./relative", "a%2Fb", "a+b c", "雪/☃", "trailing/", "a?b#c", "a\x7fb"} {
		t.Run(key, func(t *testing.T) {
			want, err := NewRequest("test", "bucket", key, "version+/%")
			if err != nil {
				t.Fatal(err)
			}

			origin := sidecarTestOrigin(t, "body", `"v"`, nil)
			h, _ := sidecarTestHandler(t, func(ctx context.Context, r racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
				if r.Key != want.Key || r.Metadata != want.Metadata {
					t.Error("exact identity changed")
				}

				return origin(ctx, r)
			})

			w := sidecarTestRequest(h, "GET", "/bucket/"+url.PathEscape(key)+"?versionId="+url.QueryEscape("version+/%"), nil)
			if w.Code != 200 || w.Body.String() != "body" {
				t.Fatalf("response: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestSidecarSDKFailures(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{racersdk.ErrInvalidRequest, 400, "InvalidArgument"},
		{racersdk.ErrUnauthorized, 403, "AccessDenied"},
		{racersdk.ErrForbidden, 403, "AccessDenied"},
		{racersdk.ErrNotFound, 404, "NoSuchKey"},
		{racersdk.ErrVersionMismatch, 412, "PreconditionFailed"},
		{errInvalidS3Response, 502, "BadGateway"},
		{racersdk.ErrUnavailable, 503, "ServiceUnavailable"},
		{context.Canceled, 503, "ServiceUnavailable"},
		{context.DeadlineExceeded, 503, "ServiceUnavailable"},
	} {
		for _, method := range []string{"GET", "HEAD"} {
			t.Run(tc.err.Error()+method, func(t *testing.T) {
				h, _ := sidecarTestHandler(t, func(context.Context, racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
					return racersdk.Metadata{}, nil, fmt.Errorf("private-upstream-secret: %w", tc.err)
				})

				w := sidecarTestRequest(h, method, "/bucket/key", nil)
				if w.Code != tc.status {
					t.Fatalf("status %d want %d", w.Code, tc.status)
				}

				if method == "HEAD" {
					if w.Body.Len() != 0 {
						t.Fatal("HEAD error body")
					}

					return
				}

				if !strings.Contains(w.Body.String(), "<Code>"+tc.code+"</Code>") || strings.Contains(w.Body.String(), "private") {
					t.Fatalf("error: %s", w.Body.String())
				}
			})
		}
	}
}

func TestSidecarPinnedVersionFailure(t *testing.T) {
	m := sidecarTestMetadata(t, 4, `"old"`)
	h, _ := sidecarTestHandler(t, func(_ context.Context, r racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		if r.Head {
			return m, nil, nil
		}

		if r.ETag != m.ETag {
			t.Error("missing Stat pin")
		}

		return racersdk.Metadata{}, nil, racersdk.ErrVersionMismatch
	})

	w := sidecarTestRequest(h, "GET", "/bucket/key", nil)
	if w.Code != 412 {
		t.Fatalf("unavailable pin status %d", w.Code)
	}
}

func TestSidecarRejectsChangedPinnedMetadata(t *testing.T) {
	for _, field := range []string{"size", "empty-size", "content-type"} {
		t.Run(field, func(t *testing.T) {
			m := sidecarTestMetadata(t, 4, `"same"`)
			if field == "empty-size" {
				m.Size = 0
			}

			var calls atomic.Int32

			h, _ := sidecarTestHandler(t, func(_ context.Context, r racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
				if calls.Add(1) == 1 {
					return m, nil, nil
				}

				changed := m
				if field == "content-type" {
					changed.ContentType = "application/octet-stream"
				} else {
					changed.Size++
				}

				if r.Head {
					return changed, nil, nil
				}

				return changed, io.NopCloser(strings.NewReader(strings.Repeat("x", int(changed.Size)))), nil
			})

			w := sidecarTestRequest(h, "GET", "/bucket/key", nil)
			if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "<Code>BadGateway</Code>") {
				t.Fatalf("changed metadata committed object: %d %q", w.Code, w.Body.String())
			}
		})
	}
}

func TestSidecarEmptyObject(t *testing.T) {
	h, _ := sidecarTestHandler(t, sidecarTestOrigin(t, "", `"empty"`, nil))
	for _, method := range []string{"GET", "HEAD"} {
		w := sidecarTestRequest(h, method, "/bucket/key", nil)
		if w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Length") != "0" {
			t.Fatalf("empty: %d %v %q", w.Code, w.Header(), w.Body.String())
		}

		w = sidecarTestRequest(h, method, "/bucket/key", http.Header{"Range": {"bytes=0-"}})
		if w.Code != 416 || w.Header().Get("Content-Range") != "bytes */0" {
			t.Fatalf("empty range: %d", w.Code)
		}
	}
}

func TestSidecarLateFailureAborts(t *testing.T) {
	m := sidecarTestMetadata(t, 1024*1024, `"v"`)
	h, _ := sidecarTestHandler(t, func(_ context.Context, r racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		if r.Head {
			return m, nil, nil
		}

		return m, io.NopCloser(strings.NewReader(strings.Repeat("x", 512*1024))), nil
	})
	t.Run("handler abort", func(t *testing.T) {
		defer func() {
			if got := recover(); got != http.ErrAbortHandler {
				t.Errorf("panic = %v", got)
			}
		}()

		sidecarTestRequest(h, "GET", "/bucket/key", nil)
		t.Error("failed stream returned normally")
	})

	server := httptest.NewServer(h)
	t.Cleanup(server.Close)

	req, err := http.NewRequestWithContext(t.Context(), "GET", server.URL+"/bucket/key", nil)
	if err != nil {
		t.Fatal(err)
	}

	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	data, err := io.ReadAll(res.Body)
	if err == nil || len(data) == int(m.Size) || strings.Contains(string(data), "<Error>") {
		t.Fatalf("late failure became success or XML: bytes=%d err=%v", len(data), err)
	}
}

func TestSidecarAWSClient(t *testing.T) {
	getRequest, err := NewRequest("test", "bucket", "a//b/../c", "")
	if err != nil {
		t.Fatal(err)
	}

	headRequest, err := NewRequest("test", "bucket", "key", "")
	if err != nil {
		t.Fatal(err)
	}

	missingRequest, err := NewRequest("test", "bucket", "missing", "")
	if err != nil {
		t.Fatal(err)
	}

	origin := sidecarTestOrigin(t, "0123456789", `"v"`, nil)
	h, _ := sidecarTestHandler(t, func(ctx context.Context, r racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		if r.Key == missingRequest.Key {
			return racersdk.Metadata{}, nil, racersdk.ErrNotFound
		}

		if r.Key != getRequest.Key && r.Key != headRequest.Key {
			t.Error("signed SDK key identity changed")
		}

		return origin(ctx, r)
	})

	var signed atomic.Bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
			signed.Store(true)
		}

		h.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	client := testS3Client(server)

	res, err := client.GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String("bucket"), Key: aws.String("a//b/../c"), Range: aws.String("bytes=2-4"), IfMatch: aws.String(`"v"`)})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	data, err := io.ReadAll(res.Body)
	if err != nil || string(data) != "234" || aws.ToString(res.ContentRange) != "bytes 2-4/10" {
		t.Fatalf("SDK response %q %v", data, err)
	}

	head, err := client.HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: aws.String("bucket"), Key: aws.String("key")})
	if err != nil || aws.ToInt64(head.ContentLength) != 10 {
		t.Fatalf("SDK HEAD: %v", err)
	}

	if !signed.Load() {
		t.Fatal("test did not send signed SDK requests")
	}

	_, err = client.GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String("bucket"), Key: aws.String("missing")})

	var apiError smithy.APIError
	if !errors.As(err, &apiError) || apiError.ErrorCode() != "NoSuchKey" {
		t.Fatalf("SDK error mapping: %v", err)
	}

	_, err = client.GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String("bucket"), Key: aws.String("key"), ChecksumMode: "ENABLED"})
	if !errors.As(err, &apiError) || apiError.ErrorCode() != "NotImplemented" {
		t.Fatalf("SDK checksum request was not rejected: %v", err)
	}
}

func TestSidecarConfigAndClosedClient(t *testing.T) {
	h, client := sidecarTestHandler(t, sidecarTestOrigin(t, "data", `"v"`, nil))
	for _, tc := range []struct {
		config  SidecarConfig
		message string
	}{
		{SidecarConfig{}, "racer-object: invalid sidecar namespace"},
		{SidecarConfig{Namespace: "test", Buckets: []string{""}}, "racer-object: invalid sidecar bucket"},
	} {
		if _, err := NewSidecar(client, tc.config); err == nil || err.Error() != tc.message {
			t.Fatalf("invalid config error: %v, want %q", err, tc.message)
		}
	}

	if _, err := NewSidecar(nil, SidecarConfig{Namespace: "test"}); err == nil || err.Error() != "racer-object: missing sidecar client" {
		t.Fatalf("nil client error: %v", err)
	}

	if err := client.Close(); err != nil {
		t.Fatal(err)
	}

	w := sidecarTestRequest(h, "GET", "/bucket/key", nil)
	if w.Code != 503 {
		t.Fatalf("closed client: %d", w.Code)
	}
}

func TestSidecarCrossPageRange(t *testing.T) {
	data := strings.Repeat("0123456789abcdef", int(racersdk.PageSize)/16+2)
	h, _ := sidecarTestHandler(t, sidecarTestOrigin(t, data, `"pages"`, nil))
	first := int(racersdk.PageSize) - 4

	w := sidecarTestRequest(h, "GET", "/bucket/key", http.Header{"Range": {"bytes=" + strconv.Itoa(first) + "-" + strconv.Itoa(first+10)}})
	if w.Code != 206 || w.Body.String() != data[first:first+11] {
		t.Fatalf("cross-page response: %d %q", w.Code, w.Body.String())
	}
}

func TestSidecarEmptyReadFailure(t *testing.T) {
	m := sidecarTestMetadata(t, 0, `"empty"`)

	var calls atomic.Int32

	h, _ := sidecarTestHandler(t, func(context.Context, racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		if calls.Add(1) == 1 {
			return m, nil, nil
		}

		return racersdk.Metadata{}, nil, racersdk.ErrUnavailable
	})

	w := sidecarTestRequest(h, "GET", "/bucket/key", nil)
	if calls.Load() < 2 || w.Code != 503 || !strings.Contains(w.Body.String(), "ServiceUnavailable") {
		t.Fatalf("empty read failure became success: %d %s", w.Code, w.Body.String())
	}
}

func TestSidecarCancellation(t *testing.T) {
	entered := make(chan struct{})
	m := sidecarTestMetadata(t, 10, `"v"`)
	h, _ := sidecarTestHandler(t, func(ctx context.Context, r racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		if r.Head {
			return m, nil, nil
		}

		close(entered)
		<-ctx.Done()

		return racersdk.Metadata{}, nil, ctx.Err()
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	r := httptest.NewRequestWithContext(ctx, "GET", "/bucket/key", nil)
	done := make(chan struct{})

	go func() { defer close(done); h.ServeHTTP(httptest.NewRecorder(), r) }()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("origin not entered")
	}

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not stop after cancellation")
	}
}

func TestSidecarAllowlistSnapshot(t *testing.T) {
	_, client := sidecarTestHandler(t, sidecarTestOrigin(t, "data", `"v"`, nil))
	config := SidecarConfig{Namespace: "test", Buckets: []string{"bucket"}}

	h, err := NewSidecar(client, config)
	if err != nil {
		t.Fatal(err)
	}

	config.Buckets[0] = "other"

	w := sidecarTestRequest(h, "HEAD", "/bucket/key", nil)
	if w.Code != 200 {
		t.Fatal("caller mutation changed allowlist")
	}

	h, err = NewSidecar(client, SidecarConfig{Namespace: "test"})
	if err != nil {
		t.Fatal(err)
	}

	w = sidecarTestRequest(h, "HEAD", "/other/key", nil)
	if w.Code != 200 {
		t.Fatal("empty allowlist rejected bucket")
	}
}

func TestSidecarUnsupportedSemanticHeaders(t *testing.T) {
	var calls atomic.Int32

	h, _ := sidecarTestHandler(t, sidecarTestOrigin(t, "data", `"v"`, &calls))

	for _, name := range []string{
		"x-amz-expected-bucket-owner",
		"x-amz-server-side-encryption-customer-algorithm",
		"x-amz-server-side-encryption-customer-key",
		"x-amz-server-side-encryption-customer-key-MD5",
		"x-amz-server-side-encryption",
		"x-amz-checksum-mode",
		"x-amz-request-payer",
		"x-amz-copy-source",
		"x-amz-meta-custom",
		"x-amz-unknown-operation",
		"If-Modified-Since", "If-Unmodified-Since", "If-Range",
	} {
		for _, method := range []string{"GET", "HEAD"} {
			for _, value := range []string{"private-semantic-value", ""} {
				t.Run(name+method+value, func(t *testing.T) {
					for _, spelling := range []string{name, http.CanonicalHeaderKey(name), strings.ToUpper(name)} {
						w := sidecarTestRequest(h, method, "/bucket/key", http.Header{spelling: {value}})
						if w.Code != 501 {
							t.Fatalf("%s status %d", spelling, w.Code)
						}

						if method == "HEAD" {
							if w.Body.Len() != 0 {
								t.Fatal("HEAD error body")
							}
						} else if !strings.Contains(w.Body.String(), "<Code>NotImplemented</Code>") || strings.Contains(w.Body.String(), "private-semantic-value") {
							t.Fatalf("unsafe or incorrect XML: %s", w.Body.String())
						}
					}
				})
			}
		}
	}

	if calls.Load() != 0 {
		t.Fatal("unsupported headers reached Racer")
	}
}

func TestSidecarAuthAndPlumbingHeaders(t *testing.T) {
	h, _ := sidecarTestHandler(t, sidecarTestOrigin(t, "data", `"v"`, nil))

	headers := http.Header{
		"Authorization":         {"AWS4-HMAC-SHA256 ignored"},
		"X-Amz-Date":            {"20261006T140000Z"},
		"X-Amz-Content-Sha256":  {"UNSIGNED-PAYLOAD"},
		"X-Amz-Security-Token":  {"ignored-session-token"},
		"X-Amz-Region-Set":      {"us-east-1"},
		"X-Amz-S3session-Token": {"ignored-express-token"},
		"X-Amz-User-Agent":      {"aws-sdk-test"},
		"Amz-Sdk-Invocation-Id": {"test-invocation"},
		"Amz-Sdk-Request":       {"attempt=1; max=1"},
		"User-Agent":            {"aws-sdk-test"},
		"Accept-Encoding":       {"identity"},
	}
	for _, method := range []string{"GET", "HEAD"} {
		w := sidecarTestRequest(h, method, "/bucket/key", headers)
		if w.Code != 200 {
			t.Fatalf("normal SDK headers rejected: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestSidecarFailureWireFormat(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{
		{400, "InvalidArgument"},
		{403, "AccessDenied"},
		{404, "NoSuchKey"},
		{405, "MethodNotAllowed"},
		{412, "PreconditionFailed"},
		{416, "InvalidRange"},
		{431, "RequestHeaderSectionTooLarge"},
		{500, "InternalError"},
		{501, "NotImplemented"},
		{502, "BadGateway"},
		{503, "ServiceUnavailable"},
	} {
		for _, method := range []string{"GET", "HEAD"} {
			t.Run(tc.code+method, func(t *testing.T) {
				w := httptest.NewRecorder()
				sidecarFailure(tc.status).write(w, httptest.NewRequest(method, "/", nil))

				body := "<Error><Code>" + tc.code + "</Code><Message>" + http.StatusText(tc.status) + "</Message></Error>"
				if w.Code != tc.status || w.Header().Get("Content-Type") != "application/xml" || w.Header().Get("Content-Length") != strconv.Itoa(len(body)) {
					t.Fatalf("error response: %d %v", w.Code, w.Header())
				}

				if method == "HEAD" {
					body = ""
				}

				if w.Body.String() != body {
					t.Fatalf("body %q, want %q", w.Body.String(), body)
				}
			})
		}
	}

	for _, tc := range []struct {
		err    error
		status sidecarFailure
	}{
		{errors.New("private"), 502},
		{racersdk.ErrRangeNotSatisfiable, 416},
		{context.Canceled, 503},
		{context.DeadlineExceeded, 503},
		{net.ErrClosed, 503},
		{fmt.Errorf("wrapped: %w", racersdk.ErrNotFound), 404},
	} {
		if got := sidecarSDKFailure(tc.err); got != tc.status {
			t.Errorf("mapping %v: %d, want %d", tc.err, got, tc.status)
		}
	}

	w := &sidecarFailingWriter{ResponseRecorder: httptest.NewRecorder()}
	sidecarFailure(400).write(w, httptest.NewRequest("GET", "/", nil))

	if w.Code != 400 || w.writes != 1 {
		t.Fatalf("failed error write: status=%d writes=%d", w.Code, w.writes)
	}
}

type sidecarFailingWriter struct {
	*httptest.ResponseRecorder
	writes int
}

func (w *sidecarFailingWriter) Write([]byte) (int, error) {
	w.writes++
	return 0, io.ErrClosedPipe
}

func TestSidecarPathFallbackAndDefaultHeaders(t *testing.T) {
	origin := sidecarTestOrigin(t, "data", `"v"`, nil)

	h, _ := sidecarTestHandler(t, func(ctx context.Context, r racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		want, err := NewRequest("test", "bucket", "a%zz", "")
		if err != nil || r.Key != want.Key {
			t.Error("invalid RawPath changed identity")
		}

		m, body, err := origin(ctx, r)
		m.ContentType = ""

		return m, body, err
	})
	for _, method := range []string{"GET", "HEAD"} {
		r := httptest.NewRequest(method, "/bucket/a%25zz?x-id="+map[string]string{"GET": "GetObject", "HEAD": "HeadObject"}[method], nil)
		r.URL.RawPath = "/%zz/bad"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)

		if w.Code != 200 || w.Header().Get("Content-Type") != "application/octet-stream" || w.Header().Get("Content-Length") != "4" || w.Header().Get("x-amz-version-id") != "" {
			t.Fatalf("default headers: %d %v", w.Code, w.Header())
		}
	}
}

func TestSidecarEmptyStreamClosedBeforeCompletion(t *testing.T) {
	_, client := sidecarTestHandler(t, sidecarTestOrigin(t, "", `"empty"`, nil))

	request, err := NewRequest("test", "bucket", "key", "")
	if err != nil {
		t.Fatal(err)
	}

	m := sidecarTestMetadata(t, 0, `"empty"`)

	v, err := client.Get(t.Context(), request, racersdk.ReadOptions{ETag: m.ETag})
	if err != nil {
		t.Fatal(err)
	}

	if err := v.Close(); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()

	failure := (sidecarRead{metadata: m}).writeBody(w, v)
	if failure != 503 || len(w.Header()) != 0 || w.Body.Len() != 0 || w.Flushed {
		t.Fatalf("empty failure committed success: failure=%d headers=%v body=%q", failure, w.Header(), w.Body.String())
	}
}
