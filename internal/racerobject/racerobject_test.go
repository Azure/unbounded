// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racerobject

import (
	"context"
	"crypto/sha256"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/Azure/unbounded/pkg/racersdk"
	"github.com/Azure/unbounded/pkg/racersdk/racersdktest"
)

func TestNewRequestCanonicalIdentity(t *testing.T) {
	want := `{"schema":1,"namespace":"store","bucket":"bucket","key":"a/../b%2Fc +雪","versionId":"v+/="}`

	request, err := NewRequest("store", "bucket", "a/../b%2Fc +雪", "v+/=")
	if err != nil {
		t.Fatal(err)
	}

	if request.Key != racersdk.Key(sha256.Sum256([]byte(want))) || request.Metadata != want || request.Authorization != "" {
		t.Fatal("canonical identity changed")
	}

	for _, fields := range [][4]string{
		{"other", "bucket", "a/../b%2Fc +雪", "v+/="},
		{"store", "other", "a/../b%2Fc +雪", "v+/="},
		{"store", "bucket", "b%2Fc +雪", "v+/="},
		{"store", "bucket", "a/../b/c +雪", "v+/="},
		{"store", "bucket", "a/../b%2Fc +雪", ""},
	} {
		other, err := NewRequest(fields[0], fields[1], fields[2], fields[3])
		if err != nil || other.Key == request.Key {
			t.Fatalf("identity collision: %v", err)
		}
	}

	plain, err := NewRequest("store", "bucket", "key", "")
	if err != nil || strings.Contains(plain.Metadata, "versionId") {
		t.Fatal("empty version must be omitted", err)
	}
}

func TestNewRequestValidation(t *testing.T) {
	for _, fields := range [][4]string{
		{"", "bucket", "key", ""},
		{"with space", "bucket", "key", ""},
		{"store\n", "bucket", "key", ""},
		{"\xff", "bucket", "key", ""},
		{"store", "", "key", ""},
		{"store", "https://evil", "key", ""},
		{"store", "../bucket", "key", ""},
		{"store", "bucket", "", ""},
		{"store", "bucket", strings.Repeat("k", 1025), ""},
		{"store", "bucket", "\xff", ""},
		{"store", "bucket", "key", "\xff"},
		{"store", "bucket", "key", strings.Repeat("v", 1025)},
		{strings.Repeat("n", 257), "bucket", "key", ""},
		{"store\u00a0", "bucket", "key", ""},
		{"store\x00", "bucket", "key", ""},
		{"store", ".", "key", ""},
		{"store", "..", "key", ""},
		{"store", strings.Repeat("b", 256), "key", ""},
		{"store", "buckét", "key", ""},
	} {
		if _, err := NewRequest(fields[0], fields[1], fields[2], fields[3]); err == nil {
			t.Fatalf("accepted invalid fields %q", fields)
		}
	}

	for _, key := range []string{"/", ".", "../", "a//b", "a\x00b", "line\nbreak", strings.Repeat("k", 1024)} {
		if _, err := NewRequest("store", "bucket", key, "null"); err != nil {
			t.Fatalf("rejected exact key %q: %v", key, err)
		}
	}

	for _, fields := range [][4]string{
		{strings.Repeat("n", 256), strings.Repeat("b", 255), strings.Repeat("k", 1024), strings.Repeat("v", 1024)},
		{"雪", "Legacy_Bucket-9.example", "key", "v\x00\n雪"},
	} {
		if _, err := NewRequest(fields[0], fields[1], fields[2], fields[3]); err != nil {
			t.Fatalf("rejected valid boundary fields %q: %v", fields, err)
		}
	}

	// Both strings fit the object limits, but escaping exceeds the SDK field cap.
	_, err := NewRequest("store", "bucket", strings.Repeat("\x00", 1024), strings.Repeat("\x00", 1024))
	originAssertError(t, err, racersdk.ErrInvalidRequest)
}

type originS3Stub struct {
	head func(context.Context, *s3.HeadObjectInput) (*s3.HeadObjectOutput, error)
	get  func(context.Context, *s3.GetObjectInput) (*s3.GetObjectOutput, error)
}

func (s *originS3Stub) HeadObject(ctx context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	return s.head(ctx, in)
}

func (s *originS3Stub) GetObject(ctx context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	return s.get(ctx, in)
}

func originHead(size int64) *s3.HeadObjectOutput {
	return &s3.HeadObjectOutput{ContentLength: aws.Int64(size), ETag: aws.String(`"opaque-multipart-2"`), ContentType: aws.String("application/test")}
}

func originPage(body io.ReadCloser, first, last, total int64) *s3.GetObjectOutput {
	return &s3.GetObjectOutput{
		Body: body, ContentLength: aws.Int64(last - first + 1), ETag: aws.String(`"opaque-multipart-2"`),
		ContentType: aws.String("application/test"), ContentRange: aws.String(fmt.Sprintf("bytes %d-%d/%d", first, last, total)),
	}
}

func originClient(t *testing.T, stub S3Client, config OriginConfig) *racersdk.Client {
	t.Helper()

	origin, err := NewOrigin(stub, config)
	if err != nil {
		t.Fatal(err)
	}

	return testSDKClient(t, origin)
}

// testSDKClient starts an in-process racersdk client that is closed with the test.
func testSDKClient(t *testing.T, origin racersdk.Origin) *racersdk.Client {
	t.Helper()

	return racersdktest.NewClient(t, origin)
}

// testS3Client returns a real, signing AWS S3 client that talks to server
// with path-style addressing and no retries.
func testS3Client(server *httptest.Server) *s3.Client {
	return s3.New(s3.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(server.URL), UsePathStyle: true,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		Credentials:                credentials.NewStaticCredentialsProvider("placeholder", "placeholder", ""),
		RetryMaxAttempts:           1, HTTPClient: server.Client(),
	})
}

func objectRequest(t *testing.T, version string) racersdk.Request {
	t.Helper()

	request, err := NewRequest("store", "bucket", "a/../exact%2F +雪", version)
	if err != nil {
		t.Fatal(err)
	}

	return request
}

func originAssertError(t *testing.T, err, want error) {
	t.Helper()

	if want == errInvalidS3Response {
		if err == nil || sidecarSDKFailure(err) != http.StatusBadGateway {
			t.Fatalf("error = %v, want unclassified origin failure", err)
		}

		return
	}

	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func originReadValue(t *testing.T, client *racersdk.Client, request racersdk.Request, options ...racersdk.ReadOptions) ([]byte, error) {
	t.Helper()

	value, err := client.Get(t.Context(), request, options...)
	if err != nil {
		return nil, err
	}
	defer value.Close()

	return io.ReadAll(value)
}

func TestOriginBridgeHeadBootstrapPinnedAndEmpty(t *testing.T) {
	for _, version := range []string{"", "version+/="} {
		t.Run(version, func(t *testing.T) {
			var heads, gets atomic.Int32

			stub := &originS3Stub{}
			stub.head = func(ctx context.Context, in *s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
				heads.Add(1)

				if (in.VersionId == nil) != (version == "") {
					t.Error("HEAD changed absent version to an empty parameter")
				}

				if ctx.Err() != nil || aws.ToString(in.Bucket) != "bucket" || aws.ToString(in.Key) != "a/../exact%2F +雪" || aws.ToString(in.VersionId) != version {
					t.Error("HEAD changed exact identity")
				}

				head := originHead(int64(racersdk.PageSize) + 3)
				head.VersionId = aws.String(version)

				return head, nil
			}
			stub.get = func(_ context.Context, in *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
				gets.Add(1)

				if (in.VersionId == nil) != (version == "") {
					t.Error("GET changed absent version to an empty parameter")
				}

				if aws.ToString(in.IfMatch) != `"opaque-multipart-2"` || aws.ToString(in.VersionId) != version || aws.ToString(in.Key) != "a/../exact%2F +雪" || aws.ToString(in.Bucket) != "bucket" || aws.ToString(in.Range) != "bytes=16777216-16777218" {
					t.Error("GET was not pinned to exact identity/range")
				}

				out := originPage(io.NopCloser(strings.NewReader("end")), int64(racersdk.PageSize), int64(racersdk.PageSize)+2, int64(racersdk.PageSize)+3)
				out.VersionId = aws.String(version)

				return out, nil
			}
			client := originClient(t, stub, OriginConfig{Namespace: "store", MetadataTTL: 30 * time.Second})
			request := objectRequest(t, version)
			before := time.Now()

			metadata, err := client.Stat(t.Context(), request)
			if err != nil || metadata.Size != racersdk.PageSize+3 || metadata.ContentType != "application/test" || metadata.ExpiresAt.Before(before.Add(29*time.Second)) || metadata.ExpiresAt.Nanosecond()%int(time.Millisecond) != 0 {
				t.Fatal("HEAD metadata", metadata, err)
			}

			got, err := originReadValue(t, client, request, racersdk.ReadOptions{Offset: racersdk.PageSize, Length: 3, ETag: metadata.ETag})
			if err != nil || string(got) != "end" || heads.Load() != 3 || gets.Load() != 1 {
				t.Fatalf("pinned page = %q, %v; calls %d/%d", got, err, heads.Load(), gets.Load())
			}
		})
	}

	for _, data := range []string{"", "hello"} {
		t.Run("bootstrap/"+data, func(t *testing.T) {
			stub := &originS3Stub{head: func(context.Context, *s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
				return originHead(int64(len(data))), nil
			}}
			stub.get = func(_ context.Context, in *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
				if data == "" || aws.ToString(in.IfMatch) != `"opaque-multipart-2"` || aws.ToString(in.Range) != "bytes=0-4" {
					t.Error("bad bootstrap GET")
				}

				return originPage(io.NopCloser(strings.NewReader(data)), 0, int64(len(data))-1, int64(len(data))), nil
			}
			client := originClient(t, stub, OriginConfig{Namespace: "store"})

			got, err := originReadValue(t, client, objectRequest(t, ""))
			if err != nil || string(got) != data {
				t.Fatalf("bootstrap = %q, %v", got, err)
			}

			metadata, err := client.Stat(t.Context(), objectRequest(t, ""))
			if err != nil || metadata.ExpiresAt.After(time.Now()) || metadata.ExpiresAt.Before(time.Now().Add(-time.Second)) {
				t.Fatal("zero TTL was defaulted", metadata, err)
			}
		})
	}
}

func TestOriginStrictIdentityBridge(t *testing.T) {
	canonical := objectRequest(t, "").Metadata
	for name, raw := range map[string]string{
		"absent": "", "null": "null", "unknown": strings.TrimSuffix(canonical, "}") + `,"endpoint":"https://evil"}`,
		"duplicate": strings.Replace(canonical, `"schema":1`, `"schema":1,"schema":1`, 1),
		"case":      strings.Replace(canonical, "schema", "Schema", 1), "whitespace": strings.Replace(canonical, `"schema":1`, `"schema": 1`, 1),
		"schema":   strings.Replace(canonical, `"schema":1`, `"schema":2`, 1),
		"trailing": canonical + "{}", "empty-version": strings.TrimSuffix(canonical, "}") + `,"versionId":""}`,
		"escaped": strings.Replace(canonical, "store", `\u0073tore`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			stub := &originS3Stub{head: func(context.Context, *s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
				t.Error("invalid identity reached S3")
				return nil, nil
			}}
			client := originClient(t, stub, OriginConfig{Namespace: "store"})

			_, err := client.Stat(t.Context(), racersdk.Request{Key: sha256.Sum256([]byte(raw)), Metadata: raw})
			originAssertError(t, err, racersdk.ErrInvalidRequest)
		})
	}

	for _, mode := range []string{"key", "namespace", "bucket"} {
		t.Run(mode, func(t *testing.T) {
			stub := &originS3Stub{head: func(context.Context, *s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
				t.Error("forbidden request reached S3")
				return nil, nil
			}}
			config := OriginConfig{Namespace: "store"}
			request := objectRequest(t, "")
			want := racersdk.ErrForbidden

			switch mode {
			case "key":
				request.Key[0] ^= 1
				want = racersdk.ErrInvalidRequest
			case "namespace":
				config.Namespace = "other"
			case "bucket":
				config.Buckets = []string{"other"}
			}

			client := originClient(t, stub, config)
			_, err := client.Stat(t.Context(), request)
			originAssertError(t, err, want)
		})
	}
}

func TestOriginConfigValidation(t *testing.T) {
	for _, config := range []OriginConfig{{}, {Namespace: "bad space"}, {Namespace: "store", MetadataTTL: -1}, {Namespace: "store", Buckets: []string{""}}, {Namespace: "store", Buckets: []string{"https://evil"}}} {
		_, err := NewOrigin(&originS3Stub{}, config)
		originAssertError(t, err, racersdk.ErrInvalidRequest)
	}

	for _, client := range []S3Client{nil, (*originS3Stub)(nil)} {
		_, err := NewOrigin(client, OriginConfig{Namespace: "store"})
		originAssertError(t, err, racersdk.ErrInvalidRequest)
	}
}

func TestOriginInvalidHeadMetadata(t *testing.T) {
	for _, mode := range []string{"nil", "length-nil", "negative", "tag-nil", "weak-tag", "type", "range", "deleted", "version"} {
		t.Run(mode, func(t *testing.T) {
			stub := &originS3Stub{head: func(context.Context, *s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
				head := originHead(5)

				switch mode {
				case "nil":
					return nil, nil
				case "length-nil":
					head.ContentLength = nil
				case "negative":
					head.ContentLength = aws.Int64(-1)
				case "tag-nil":
					head.ETag = nil
				case "weak-tag":
					head.ETag = aws.String(`W/"weak"`)
				case "type":
					head.ContentType = aws.String("bad\r\nheader")
				case "range":
					head.ContentRange = aws.String("bytes 0-4/5")
				case "deleted":
					head.DeleteMarker = aws.Bool(true)
				}

				return head, nil
			}}
			client := originClient(t, stub, OriginConfig{Namespace: "store"})

			version := ""
			if mode == "version" {
				version = "requested"
			}

			_, err := client.Stat(t.Context(), objectRequest(t, version))
			originAssertError(t, err, errInvalidS3Response)
		})
	}
}

type originTrackedBody struct {
	io.Reader
	closed atomic.Int32
}

func (b *originTrackedBody) Close() error { b.closed.Add(1); return nil }

func TestOriginRejectsGetMetadataAndClosesBodies(t *testing.T) {
	for _, mode := range []string{"length", "nil-length", "range", "total", "etag", "missing-etag", "weak-etag", "type", "version", "encoding", "deleted", "nil", "nil-body", "error-body", "short", "long"} {
		t.Run(mode, func(t *testing.T) {
			body := &originTrackedBody{Reader: strings.NewReader("hello")}
			stub := &originS3Stub{head: func(context.Context, *s3.HeadObjectInput) (*s3.HeadObjectOutput, error) { return originHead(5), nil }}
			stub.get = func(context.Context, *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
				out := originPage(body, 0, 4, 5)

				switch mode {
				case "length":
					out.ContentLength = aws.Int64(4)
				case "nil-length":
					out.ContentLength = nil
				case "range":
					out.ContentRange = nil
				case "total":
					out.ContentRange = aws.String("bytes 0-4/6")
				case "etag":
					out.ETag = aws.String(`"changed"`)
				case "missing-etag":
					out.ETag = nil
				case "weak-etag":
					out.ETag = aws.String(`W/"opaque-multipart-2"`)
				case "type":
					out.ContentType = aws.String("text/plain")
				case "version":
					out.VersionId = aws.String("unexpected")
				case "encoding":
					out.ContentEncoding = aws.String("gzip")
				case "deleted":
					out.DeleteMarker = aws.Bool(true)
				case "nil":
					return nil, nil
				case "nil-body":
					out.Body = nil
				case "error-body":
					return out, &smithy.GenericAPIError{Code: "AccessDenied"}
				case "short":
					body.Reader = strings.NewReader("hell")
				case "long":
					body.Reader = strings.NewReader("hello!")
				}

				return out, nil
			}
			client := originClient(t, stub, OriginConfig{Namespace: "store"})

			got, err := originReadValue(t, client, objectRequest(t, ""))
			if mode == "short" || mode == "long" {
				if err == nil || len(got) >= 5 {
					t.Fatalf("bad body accepted: %q %v", got, err)
				}
			} else {
				want := errInvalidS3Response
				if mode == "etag" {
					want = racersdk.ErrVersionMismatch
				}

				if mode == "error-body" {
					want = racersdk.ErrForbidden
				}

				originAssertError(t, err, want)
			}

			if mode != "nil" && mode != "nil-body" {
				deadline := time.Now().Add(time.Second)
				for body.closed.Load() == 0 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}

				if body.closed.Load() != 1 {
					t.Fatal("body not closed exactly once", body.closed.Load())
				}
			}
		})
	}
}

type originStatusError struct{ code int }

func (e originStatusError) Error() string       { return "private upstream URL/credential" }
func (e originStatusError) HTTPStatusCode() int { return e.code }

func TestOriginErrorClassification(t *testing.T) {
	for _, tt := range []struct {
		err  error
		want error
	}{
		{context.Canceled, context.Canceled},
		{context.DeadlineExceeded, context.DeadlineExceeded},
		{&smithy.GenericAPIError{Code: "NoSuchKey"}, racersdk.ErrNotFound},
		{&smithy.GenericAPIError{Code: "NoSuchVersion"}, racersdk.ErrNotFound},
		{&smithy.GenericAPIError{Code: "NoSuchBucket"}, racersdk.ErrNotFound},
		{&smithy.GenericAPIError{Code: "NotFound"}, racersdk.ErrNotFound},
		{&smithy.GenericAPIError{Code: "AccessDenied"}, racersdk.ErrForbidden},
		{&smithy.GenericAPIError{Code: "InvalidAccessKeyId"}, racersdk.ErrForbidden},
		{&smithy.GenericAPIError{Code: "SignatureDoesNotMatch"}, racersdk.ErrForbidden},
		{&smithy.GenericAPIError{Code: "ExpiredToken"}, racersdk.ErrForbidden},
		{&smithy.GenericAPIError{Code: "InvalidToken"}, racersdk.ErrForbidden},
		{&smithy.GenericAPIError{Code: "TokenRefreshRequired"}, racersdk.ErrForbidden},
		{&smithy.GenericAPIError{Code: "PreconditionFailed"}, racersdk.ErrVersionMismatch},
		{&smithy.GenericAPIError{Code: "SlowDown"}, racersdk.ErrUnavailable},
		{&smithy.GenericAPIError{Code: "ServiceUnavailable"}, racersdk.ErrUnavailable},
		{&smithy.GenericAPIError{Code: "InternalError"}, racersdk.ErrUnavailable},
		{&smithy.GenericAPIError{Code: "RequestTimeout"}, racersdk.ErrUnavailable},
		{&smithy.GenericAPIError{Code: "Throttling"}, racersdk.ErrUnavailable},
		{&smithy.GenericAPIError{Code: "ThrottlingException"}, racersdk.ErrUnavailable},
		{&smithy.GenericAPIError{Code: "Unknown"}, errInvalidS3Response},
		{originStatusError{401}, racersdk.ErrUnauthorized},
		{originStatusError{403}, racersdk.ErrForbidden},
		{originStatusError{404}, racersdk.ErrNotFound},
		{originStatusError{412}, racersdk.ErrVersionMismatch},
		{originStatusError{416}, errInvalidS3Response},
		{originStatusError{408}, racersdk.ErrUnavailable},
		{originStatusError{499}, errInvalidS3Response},
		{originStatusError{429}, racersdk.ErrUnavailable},
		{originStatusError{500}, racersdk.ErrUnavailable},
		{originStatusError{600}, racersdk.ErrUnavailable},
		{errors.New("private upstream URL/credential"), errInvalidS3Response},
	} {
		for _, pinned := range []bool{false, true} {
			want := tt.want
			if pinned && want == racersdk.ErrNotFound {
				want = racersdk.ErrVersionMismatch
			}

			err := classifyS3Error(fmt.Errorf("wrapped: %w", tt.err), pinned)
			originAssertError(t, err, want)

			if strings.Contains(err.Error(), "private") || !errors.Is(err, tt.err) {
				t.Fatal("unsafe or lost cause", err)
			}
		}
	}
}

func TestOriginErrorClassificationPrecedence(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want error
	}{
		{"cancellation", errors.Join(context.Canceled, context.DeadlineExceeded, &smithy.GenericAPIError{Code: "AccessDenied"}, originStatusError{500}), context.Canceled},
		{"deadline", errors.Join(context.DeadlineExceeded, &smithy.GenericAPIError{Code: "NoSuchKey"}, originStatusError{500}), context.DeadlineExceeded},
		{"known-api", errors.Join(&smithy.GenericAPIError{Code: "AccessDenied"}, originStatusError{404}), racersdk.ErrForbidden},
		{"unknown-api", errors.Join(&smithy.GenericAPIError{Code: "Unknown"}, originStatusError{404}), racersdk.ErrNotFound},
		{"unknown-api-and-status", errors.Join(&smithy.GenericAPIError{Code: "Unknown"}, originStatusError{416}), errInvalidS3Response},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, pinned := range []bool{false, true} {
				want := tt.want
				if pinned && want == racersdk.ErrNotFound {
					want = racersdk.ErrVersionMismatch
				}

				err := classifyS3Error(tt.err, pinned)
				originAssertError(t, err, want)

				if !errors.Is(err, tt.err) || strings.Contains(err.Error(), "private") {
					t.Fatal("unsafe or lost cause", err)
				}
			}
		})
	}
}

func TestOriginReadGuards(t *testing.T) {
	origin := &objectOrigin{}

	for _, tt := range []struct {
		name  string
		cause error
		want  error
	}{
		{"invalid-identity", nil, racersdk.ErrInvalidRequest},
		{"canceled-before-identity", context.Canceled, context.Canceled},
		{"deadline-before-identity", context.DeadlineExceeded, context.DeadlineExceeded},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()

			switch tt.cause {
			case context.Canceled:
				canceled, cancel := context.WithCancel(ctx)
				cancel()

				ctx = canceled
			case context.DeadlineExceeded:
				expired, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()

				ctx = expired
			}

			metadata, body, err := origin.read(ctx, racersdk.OriginRequest{})
			originAssertError(t, err, tt.want)

			if metadata != (racersdk.Metadata{}) || body != nil || (tt.cause != nil && !errors.Is(err, tt.cause)) {
				t.Fatal("invalid request exposed metadata/body or lost cause", metadata, body, err)
			}
		})
	}

	// The SDK normally prevents this request; keep the defensive page guard tested.
	body, err := origin.readPage(t.Context(), racersdk.OriginRequest{}, nil, nil, racersdk.Metadata{})
	originAssertError(t, err, racersdk.ErrInvalidRequest)

	if body != nil {
		t.Fatal("missing range returned a body")
	}
}

func TestOriginHeadMaximumSizeAndTTLPrecision(t *testing.T) {
	origin := &objectOrigin{ttl: time.Nanosecond}

	metadata, err := origin.headMetadata(originHead(math.MaxInt64), Object{})
	if err != nil || metadata.Size != math.MaxInt64 || metadata.ExpiresAt.Nanosecond()%int(time.Millisecond) != 0 {
		t.Fatal("maximum full size or TTL precision", metadata, err)
	}
}

func TestOriginPageRangeBounds(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		size, offset, length int64
		want                 error
	}{
		{"empty", 0, 0, racersdk.PageSize, nil},
		{"past-end", 5, racersdk.PageSize, racersdk.PageSize, nil},
		{"at-end", racersdk.PageSize, racersdk.PageSize, racersdk.PageSize, nil},
		{"negative", 5, -racersdk.PageSize, racersdk.PageSize, racersdk.ErrInvalidRequest},
		{"unaligned", 5, 1, racersdk.PageSize, racersdk.ErrInvalidRequest},
		{"zero-length", 5, 0, 0, racersdk.ErrInvalidRequest},
		{"negative-length", 5, 0, -1, racersdk.ErrInvalidRequest},
		{"oversized", 5, 0, racersdk.PageSize + 1, racersdk.ErrInvalidRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			origin := &objectOrigin{}

			body, err := origin.readPage(t.Context(), racersdk.OriginRequest{Offset: tc.offset, Length: tc.length}, nil, nil, racersdk.Metadata{Size: tc.size})
			if body != nil || !errors.Is(err, tc.want) {
				t.Fatalf("body=%v error=%v, want nil body and %v", body, err, tc.want)
			}
		})
	}

	first := int64(math.MaxInt64 / racersdk.PageSize * racersdk.PageSize)
	head := originHead(math.MaxInt64)
	origin := &objectOrigin{client: &originS3Stub{
		get: func(_ context.Context, input *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
			if got, want := aws.ToString(input.Range), fmt.Sprintf("bytes=%d-%d", first, int64(math.MaxInt64-1)); got != want {
				t.Fatalf("range=%s, want %s", got, want)
			}

			return originPage(io.NopCloser(strings.NewReader("")), first, math.MaxInt64-1, math.MaxInt64), nil
		},
	}}

	body, err := origin.readPage(t.Context(), racersdk.OriginRequest{Offset: first, Length: racersdk.PageSize}, &s3.HeadObjectInput{}, head, racersdk.Metadata{
		Size: math.MaxInt64, ETag: aws.ToString(head.ETag), ContentType: aws.ToString(head.ContentType),
	})
	if err != nil || body == nil {
		t.Fatalf("overflow-safe range: body=%v error=%v", body, err)
	}
	defer body.Close()
}

func TestOriginAWSClientBridge(t *testing.T) {
	var heads, gets atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bucket/a/../exact%2F +雪" || r.URL.Query().Get("versionId") != "v+/=" || !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
			t.Error("AWS client changed object identity or omitted signing")
		}

		w.Header().Set("ETag", `"opaque-multipart-2"`)
		w.Header().Set("Content-Type", "application/test")
		w.Header().Set("Content-Length", "5")
		w.Header().Set("x-amz-version-id", "v+/=")

		switch r.Method {
		case http.MethodHead:
			heads.Add(1)
		case http.MethodGet:
			gets.Add(1)

			if r.Header.Get("Range") != "bytes=0-4" || r.Header.Get("If-Match") != `"opaque-multipart-2"` {
				t.Error("AWS GET missing range/pin")
			}

			w.Header().Set("Content-Range", "bytes 0-4/5")
			w.WriteHeader(http.StatusPartialContent)

			if _, err := io.WriteString(w, "hello"); err != nil {
				t.Error(err)
			}
		default:
			t.Error("unexpected S3 method")
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)

	client := originClient(t, testS3Client(server), OriginConfig{Namespace: "store", Buckets: []string{"bucket"}})

	got, err := originReadValue(t, client, objectRequest(t, "v+/="))
	if err != nil || string(got) != "hello" || heads.Load() != 1 || gets.Load() != 1 {
		t.Fatalf("AWS bridge = %q, %v, calls %d/%d", got, err, heads.Load(), gets.Load())
	}
}

func TestOriginPinnedHeadAndRaceFailures(t *testing.T) {
	for _, mode := range []string{"changed-head", "missing-head", "head-precondition", "get-precondition", "missing-get", "get-range"} {
		t.Run(mode, func(t *testing.T) {
			var gets atomic.Int32

			stub := &originS3Stub{}
			stub.head = func(_ context.Context, in *s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
				if aws.ToString(in.IfMatch) != `"opaque-multipart-2"` {
					t.Error("pinned HEAD omitted If-Match")
				}

				switch mode {
				case "changed-head":
					head := originHead(5)
					head.ETag = aws.String(`"new-version"`)

					return head, nil
				case "missing-head":
					return nil, &smithy.GenericAPIError{Code: "NoSuchKey"}
				case "head-precondition":
					return nil, &smithy.GenericAPIError{Code: "PreconditionFailed"}
				}

				return originHead(5), nil
			}
			stub.get = func(context.Context, *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
				gets.Add(1)

				switch mode {
				case "missing-get":
					return nil, &smithy.GenericAPIError{Code: "NoSuchKey"}
				case "get-range":
					return nil, originStatusError{416}
				default:
					return nil, &smithy.GenericAPIError{Code: "PreconditionFailed"}
				}
			}
			client := originClient(t, stub, OriginConfig{Namespace: "store"})

			_, err := originReadValue(t, client, objectRequest(t, ""), racersdk.ReadOptions{ETag: `"opaque-multipart-2"`})

			want := racersdk.ErrVersionMismatch
			if mode == "get-range" {
				want = errInvalidS3Response
			}

			originAssertError(t, err, want)

			if strings.HasSuffix(mode, "head") || mode == "head-precondition" {
				if gets.Load() != 0 {
					t.Fatal("GET after failed pin")
				}
			}
		})
	}
}

func TestOriginUpstreamErrorsBridge(t *testing.T) {
	for _, tt := range []struct {
		code string
		want error
	}{
		{"NoSuchKey", racersdk.ErrNotFound},
		{"AccessDenied", racersdk.ErrForbidden},
		{"SlowDown", racersdk.ErrUnavailable},
		{"InvalidRange", errInvalidS3Response},
	} {
		t.Run(tt.code, func(t *testing.T) {
			stub := &originS3Stub{head: func(context.Context, *s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
				return nil, &smithy.GenericAPIError{Code: tt.code, Message: "private"}
			}}
			client := originClient(t, stub, OriginConfig{Namespace: "store"})
			_, err := client.Stat(t.Context(), objectRequest(t, ""))
			originAssertError(t, err, tt.want)
		})
	}
}

func TestOriginMultipageAndCopiedAllowlist(t *testing.T) {
	var heads, gets atomic.Int32

	size := int64(racersdk.PageSize) + 3
	stub := &originS3Stub{}
	stub.head = func(_ context.Context, in *s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
		n := heads.Add(1)
		if n > 1 && aws.ToString(in.IfMatch) != `"opaque-multipart-2"` {
			t.Error("continuation HEAD not pinned")
		}

		return originHead(size), nil
	}
	stub.get = func(_ context.Context, in *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
		gets.Add(1)

		if aws.ToString(in.IfMatch) != `"opaque-multipart-2"` {
			t.Error("GET not pinned")
		}

		if aws.ToString(in.Range) == fmt.Sprintf("bytes=0-%d", racersdk.PageSize-1) {
			return originPage(io.NopCloser(io.LimitReader(originRepeatedByte('x'), int64(racersdk.PageSize))), 0, int64(racersdk.PageSize)-1, size), nil
		}

		if aws.ToString(in.Range) != fmt.Sprintf("bytes=%d-%d", racersdk.PageSize, size-1) {
			t.Error("wrong final range")
		}

		return originPage(io.NopCloser(strings.NewReader("end")), int64(racersdk.PageSize), size-1, size), nil
	}
	buckets := []string{"bucket"}
	client := originClient(t, stub, OriginConfig{Namespace: "store", Buckets: buckets})
	buckets[0] = "changed"

	value, err := client.Get(t.Context(), objectRequest(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer value.Close()

	if n, err := io.CopyN(io.Discard, value, int64(racersdk.PageSize)); err != nil || n != int64(racersdk.PageSize) {
		t.Fatal("first page", n, err)
	}

	got, err := io.ReadAll(value)
	if err != nil || string(got) != "end" || heads.Load() != 2 || gets.Load() != 2 {
		t.Fatal("continuation", string(got), err, heads.Load(), gets.Load())
	}
}

type originRepeatedByte byte

func (b originRepeatedByte) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}

	return len(p), nil
}

type originBlockedBody struct {
	entered   chan struct{}
	closed    chan struct{}
	readOnce  sync.Once
	closeOnce sync.Once
	closes    atomic.Int32
}

func (b *originBlockedBody) Read([]byte) (int, error) {
	b.readOnce.Do(func() { close(b.entered) })
	<-b.closed

	return 0, context.Canceled
}

func (b *originBlockedBody) Close() error {
	b.closes.Add(1)
	b.closeOnce.Do(func() { close(b.closed) })

	return nil
}

func TestOriginBodyCancellationBridge(t *testing.T) {
	body := &originBlockedBody{entered: make(chan struct{}), closed: make(chan struct{})}
	stub := &originS3Stub{
		head: func(context.Context, *s3.HeadObjectInput) (*s3.HeadObjectOutput, error) { return originHead(5), nil },
		get: func(context.Context, *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
			return originPage(body, 0, 4, 5), nil
		},
	}
	client := originClient(t, stub, OriginConfig{Namespace: "store"})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	value, err := client.Get(ctx, objectRequest(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer value.Close()

	select {
	case <-body.entered:
	case <-time.After(time.Second):
		t.Fatal("body read not started")
	}

	cancel()

	select {
	case <-body.closed:
	case <-time.After(time.Second):
		t.Fatal("canceled body was not closed")
	}

	if body.closes.Load() != 1 {
		t.Fatal("close count", body.closes.Load())
	}

	_, err = io.ReadAll(value)
	if err == nil {
		t.Fatal("canceled read succeeded")
	}
}

func TestOriginHeadCancellationBridge(t *testing.T) {
	entered, canceled := make(chan struct{}), make(chan struct{})
	stub := &originS3Stub{head: func(ctx context.Context, _ *s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
		close(entered)
		<-ctx.Done()
		close(canceled)

		return nil, ctx.Err()
	}}
	client := originClient(t, stub, OriginConfig{Namespace: "store"})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	request := objectRequest(t, "")

	go func() { _, err := client.Stat(ctx, request); done <- err }()

	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("HEAD not started")
	}

	cancel()

	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("context not passed upstream")
	}

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("cancellation cause", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stat did not stop")
	}
}

func TestOriginRangeResolvedAgainstPinnedHead(t *testing.T) {
	var heads atomic.Int32

	stub := &originS3Stub{
		head: func(_ context.Context, in *s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
			if heads.Add(1) == 1 {
				return originHead(int64(racersdk.PageSize) + 3), nil
			}

			if aws.ToString(in.IfMatch) != `"opaque-multipart-2"` {
				t.Error("missing HEAD pin")
			}

			return originHead(5), nil
		},
		get: func(context.Context, *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
			t.Error("unsatisfiable page reached S3 GET")
			return nil, nil
		},
	}
	client := originClient(t, stub, OriginConfig{Namespace: "store"})
	_, err := originReadValue(t, client, objectRequest(t, ""), racersdk.ReadOptions{Offset: racersdk.PageSize, Length: 3})
	// The origin returns 416 with the new size. The SDK rejects that size because
	// it differs from the selected snapshot for the same ETag.
	originAssertError(t, err, errInvalidS3Response)

	if heads.Load() != 2 {
		t.Fatal("did not resolve page against second HEAD", heads.Load())
	}
}

func TestOriginContentEncoding(t *testing.T) {
	for _, encoding := range []string{"", "identity", "gzip", "br", "gzip, identity"} {
		for _, operation := range []string{"HEAD", "bootstrap"} {
			t.Run(operation+"/"+encoding, func(t *testing.T) {
				var gets atomic.Int32

				stub := &originS3Stub{
					head: func(context.Context, *s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
						head := originHead(5)
						head.ContentEncoding = aws.String(encoding)

						return head, nil
					},
					get: func(context.Context, *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
						gets.Add(1)

						output := originPage(io.NopCloser(strings.NewReader("hello")), 0, 4, 5)
						output.ContentEncoding = aws.String(encoding)

						return output, nil
					},
				}
				client := originClient(t, stub, OriginConfig{Namespace: "store"})
				request := objectRequest(t, "")

				var (
					err error
					got []byte
				)

				if operation == "HEAD" {
					_, err = client.Stat(t.Context(), request)
				} else {
					got, err = originReadValue(t, client, request)
				}

				if encoding != "" && encoding != "identity" {
					originAssertError(t, err, errInvalidS3Response)

					if gets.Load() != 0 || len(got) != 0 {
						t.Fatal("unsupported encoding reached GET or exposed bytes")
					}

					return
				}

				if err != nil {
					t.Fatal("unencoded object rejected", err)
				}

				if operation == "HEAD" {
					if gets.Load() != 0 {
						t.Fatal("HEAD issued GET")
					}
				} else if string(got) != "hello" || gets.Load() != 1 {
					t.Fatalf("bootstrap = %q, GET calls = %d", got, gets.Load())
				}
			})
		}
	}
}

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
	for _, key := range []string{"a//b/../c", "/leading", "./relative", "a%2Fb", "a+b c", "雪/☃", "trailing/", "a?b#c"} {
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
