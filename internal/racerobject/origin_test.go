// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racerobject

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/Azure/unbounded/pkg/racersdk"
	"github.com/Azure/unbounded/pkg/racersdk/racersdktest"
)

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

	client, cleanup, err := racersdktest.NewClient(origin)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(cleanup)

	return client
}

func objectRequest(t *testing.T, version string) racersdk.Request {
	t.Helper()

	request, err := NewRequest("store", "bucket", "a/../exact%2F +雪", version)
	if err != nil {
		t.Fatal(err)
	}

	return request
}

func assertOriginKind(t *testing.T, err error, want racersdk.ErrorKind) {
	t.Helper()

	var typed *racersdk.Error
	if !errors.As(err, &typed) || typed.Kind() != want {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func readOriginValue(t *testing.T, client *racersdk.Client, request racersdk.Request, options ...racersdk.ReadOptions) ([]byte, error) {
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

				if ctx.Err() != nil || aws.ToString(in.Bucket) != "bucket" || aws.ToString(in.Key) != "a/../exact%2F +雪" || aws.ToString(in.VersionId) != version {
					t.Error("HEAD changed exact identity")
				}

				head := originHead(int64(racersdk.PageSize) + 3)
				head.VersionId = aws.String(version)

				return head, nil
			}
			stub.get = func(_ context.Context, in *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
				gets.Add(1)

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

			got, err := readOriginValue(t, client, request, racersdk.ReadOptions{Offset: racersdk.ByteOffset(racersdk.PageSize), Length: 3, Metadata: &metadata})
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

			got, err := readOriginValue(t, client, objectRequest(t, ""))
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
	canonical := objectRequest(t, "").Context.Metadata().ForOrigin()
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

			var metadata racersdk.AdapterMetadata

			if raw != "" {
				var err error

				metadata, err = racersdk.ParseAdapterMetadata(raw)
				if err != nil {
					t.Fatal(err)
				}
			}

			fetch, err := racersdk.NewFetchContext(metadata, racersdk.Authorization{})
			if err != nil {
				t.Fatal(err)
			}

			_, err = client.Stat(t.Context(), racersdk.Request{Key: sha256.Sum256([]byte(raw)), Context: fetch})
			assertOriginKind(t, err, racersdk.ErrorInvalidArgument)
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
			want := racersdk.ErrorForbidden

			switch mode {
			case "key":
				request.Key[0] ^= 1
				want = racersdk.ErrorInvalidArgument
			case "namespace":
				config.Namespace = "other"
			case "bucket":
				config.Buckets = []string{"other"}
			}

			client := originClient(t, stub, config)
			_, err := client.Stat(t.Context(), request)
			assertOriginKind(t, err, want)
		})
	}
}

func TestOriginConfigValidation(t *testing.T) {
	for _, config := range []OriginConfig{{}, {Namespace: "bad space"}, {Namespace: "store", MetadataTTL: -1}, {Namespace: "store", Buckets: []string{""}}, {Namespace: "store", Buckets: []string{"https://evil"}}} {
		_, err := NewOrigin(&originS3Stub{}, config)
		assertOriginKind(t, err, racersdk.ErrorInvalidArgument)
	}

	for _, client := range []S3Client{nil, (*originS3Stub)(nil)} {
		_, err := NewOrigin(client, OriginConfig{Namespace: "store"})
		assertOriginKind(t, err, racersdk.ErrorInvalidArgument)
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
			assertOriginKind(t, err, racersdk.ErrorBadGateway)
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

			got, err := readOriginValue(t, client, objectRequest(t, ""))
			if mode == "short" || mode == "long" {
				if err == nil || len(got) >= 5 {
					t.Fatalf("bad body accepted: %q %v", got, err)
				}
			} else {
				want := racersdk.ErrorBadGateway
				if mode == "etag" {
					want = racersdk.ErrorVersionUnavailable
				}

				if mode == "error-body" {
					want = racersdk.ErrorForbidden
				}

				assertOriginKind(t, err, want)
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
		want racersdk.ErrorKind
	}{
		{context.Canceled, racersdk.ErrorCanceled},
		{context.DeadlineExceeded, racersdk.ErrorDeadline},
		{&smithy.GenericAPIError{Code: "NoSuchKey"}, racersdk.ErrorNotFound},
		{&smithy.GenericAPIError{Code: "NoSuchVersion"}, racersdk.ErrorNotFound},
		{&smithy.GenericAPIError{Code: "AccessDenied"}, racersdk.ErrorForbidden},
		{&smithy.GenericAPIError{Code: "PreconditionFailed"}, racersdk.ErrorVersionUnavailable},
		{&smithy.GenericAPIError{Code: "SlowDown"}, racersdk.ErrorUnavailable},
		{originStatusError{401}, racersdk.ErrorUnauthorized},
		{originStatusError{403}, racersdk.ErrorForbidden},
		{originStatusError{404}, racersdk.ErrorNotFound},
		{originStatusError{412}, racersdk.ErrorVersionUnavailable},
		{originStatusError{416}, racersdk.ErrorBadGateway},
		{originStatusError{429}, racersdk.ErrorUnavailable},
		{originStatusError{500}, racersdk.ErrorUnavailable},
		{errors.New("private upstream URL/credential"), racersdk.ErrorBadGateway},
	} {
		for _, pinned := range []bool{false, true} {
			want := tt.want
			if pinned && want == racersdk.ErrorNotFound {
				want = racersdk.ErrorVersionUnavailable
			}

			err := classifyS3Error(fmt.Errorf("wrapped: %w", tt.err), pinned)
			assertOriginKind(t, err, want)

			if strings.Contains(err.Error(), "private") || !errors.Is(err, tt.err) {
				t.Fatal("unsafe or lost cause", err)
			}
		}
	}
}

func TestOriginHeadMaximumSizeAndTTLPrecision(t *testing.T) {
	origin := &objectOrigin{ttl: time.Nanosecond}

	metadata, err := origin.headMetadata(originHead(math.MaxInt64), Object{})
	if err != nil || metadata.Size != math.MaxInt64 || metadata.ExpiresAt.Nanosecond()%int(time.Millisecond) != 0 {
		t.Fatal("maximum full size or TTL precision", metadata, err)
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
					got, err = readOriginValue(t, client, request)
				}

				if encoding != "" && encoding != "identity" {
					assertOriginKind(t, err, racersdk.ErrorBadGateway)

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
