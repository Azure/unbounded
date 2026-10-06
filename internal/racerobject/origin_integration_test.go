// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racerobject

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
)

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

	s3Client := s3.NewFromConfig(aws.Config{
		Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("test-access", "test-secret", ""),
		HTTPClient: server.Client(), RetryMaxAttempts: 1,
	}, func(options *s3.Options) { options.BaseEndpoint = aws.String(server.URL); options.UsePathStyle = true })
	client := originClient(t, s3Client, OriginConfig{Namespace: "store", Buckets: []string{"bucket"}})

	got, err := readOriginValue(t, client, objectRequest(t, "v+/="))
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

			pin, err := racersdk.ParseETag(`"opaque-multipart-2"`)
			if err != nil {
				t.Fatal(err)
			}

			_, err = readOriginValue(t, client, objectRequest(t, ""), racersdk.ReadOptions{Pin: pin})

			want := racersdk.ErrorVersionUnavailable
			if mode == "get-range" {
				want = racersdk.ErrorBadGateway
			}

			assertOriginKind(t, err, want)

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
		want racersdk.ErrorKind
	}{
		{"NoSuchKey", racersdk.ErrorNotFound},
		{"AccessDenied", racersdk.ErrorForbidden},
		{"SlowDown", racersdk.ErrorUnavailable},
		{"InvalidRange", racersdk.ErrorBadGateway},
	} {
		t.Run(tt.code, func(t *testing.T) {
			stub := &originS3Stub{head: func(context.Context, *s3.HeadObjectInput) (*s3.HeadObjectOutput, error) {
				return nil, &smithy.GenericAPIError{Code: tt.code, Message: "private"}
			}}
			client := originClient(t, stub, OriginConfig{Namespace: "store"})
			_, err := client.Stat(t.Context(), objectRequest(t, ""))
			assertOriginKind(t, err, tt.want)
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
	_, err := readOriginValue(t, client, objectRequest(t, ""), racersdk.ReadOptions{Offset: racersdk.ByteOffset(racersdk.PageSize), Length: 3})
	// The origin returns 416 with the new size. The SDK rejects that size because
	// it differs from the selected snapshot for the same ETag.
	assertOriginKind(t, err, racersdk.ErrorBadGateway)

	if heads.Load() != 2 {
		t.Fatal("did not resolve page against second HEAD", heads.Load())
	}
}
