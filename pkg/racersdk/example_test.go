// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk"
)

// Deployment-only, compile-checked example: requires a provisioned Racer client
// socket at /run/racer/models/client/socket. Get opens the full object stream.
func ExampleClient_Get() { //nolint:testableexamples // Requires the canonical /run/racer deployment socket.
	cache, err := racersdk.ParseCacheName("models")
	if err != nil {
		panic(err)
	}

	client, err := racersdk.NewClient(racersdk.ClientConfig{Cache: cache})
	if err != nil {
		panic(err)
	}
	defer client.Close()

	key, err := racersdk.ParseKey(strings.Repeat("01", 32))
	if err != nil {
		panic(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel() // Keep ctx alive through the last read, not just Get.

	value, err := client.Get(ctx, racersdk.Request{Key: key})
	if err != nil {
		panic(err)
	}
	defer value.Close()

	fmt.Println("total bytes:", value.Metadata().Size)

	var prefix [8]byte

	n, err := value.Read(prefix[:])
	fmt.Printf("prefix: %q\n", prefix[:n]) // Process n even when err != nil.

	if err != nil && err != io.EOF {
		panic(err)
	}

	// Sequential Read then WriteTo is allowed; this copies only the remainder.
	copied, err := io.Copy(io.Discard, value)
	if err != nil {
		fmt.Println("incomplete after bytes:", int64(n)+copied)
		panic(err)
	}

	fmt.Println("complete bytes:", int64(n)+copied)
}

func ExampleNewFetchContext() {
	key, err := racersdk.ParseKey(strings.Repeat("01", 32))
	if err != nil {
		panic(err)
	}

	metadata, err := racersdk.ParseAdapterMetadata("bucket=models;object=weights")
	if err != nil {
		panic(err)
	}
	// Demonstration credential only. In an application, obtain it from its owner.
	authorization, err := racersdk.ParseAuthorization("Bearer example-token")
	if err != nil {
		panic(err)
	}

	fetch, err := racersdk.NewFetchContext(metadata, authorization)
	if err != nil {
		panic(err)
	}

	request := racersdk.Request{Key: key, Context: fetch}
	fmt.Println(request.Key == key)
	fmt.Println(request.Context)
	fmt.Println(request.Context.Authorization())
	// Only an origin adapter should extract fields with ForOrigin for upstream
	// requests. These credentials do not authorize access to Racer itself.
	fmt.Println((racersdk.FetchContext{}).Authorization().ForOrigin() == "")
	// Output:
	// true
	// FetchContext([redacted])
	// Authorization([redacted])
	// true
}

func ExampleMetadata_Validate() {
	tag, err := racersdk.ParseETag(`"revision-7"`)
	if err != nil {
		panic(err)
	}

	metadata := racersdk.Metadata{
		Size:      racersdk.ByteLength(123),
		ETag:      tag,
		ExpiresAt: time.UnixMilli(1_900_000_000_123),
	}
	fmt.Println(metadata.Validate())
	fmt.Println(metadata.ETag.String(), metadata.ExpiresAt.UnixMilli())
	metadata.ExpiresAt = metadata.ExpiresAt.Add(time.Nanosecond)

	var typed *racersdk.Error
	if errors.As(metadata.Validate(), &typed) {
		fmt.Println(typed.Kind()) // No silent rounding of backend metadata.
	}
	// Output:
	// <nil>
	// "revision-7" 1900000000123
	// invalid argument
}

func ExampleError() {
	_, err := racersdk.ParseETag(`W/"weak"`)

	var typed *racersdk.Error
	if errors.As(err, &typed) {
		fmt.Println(typed.Kind(), typed.Operation(), typed.StatusCode())
	}

	// Callback causes remain inspectable without appearing in diagnostics.
	// StatusCode is zero here: this is a local callback error. ServeOrigin maps
	// it to an empty HTTP 503; a receiving Client reports that HTTP status.
	err = racersdk.NewOriginError(racersdk.ErrorUnavailable, context.DeadlineExceeded)
	fmt.Println(err)
	fmt.Println("deadline:", errors.Is(err, context.DeadlineExceeded))
	// Output:
	// invalid argument etag 0
	// racersdk origin: unavailable
	// deadline: true
}

func ExampleRange_Resolve() {
	// The final page has only three bytes. Bounds are inclusive.
	page, err := racersdk.ClosedRange(
		racersdk.ByteOffset(racersdk.PageSize),
		racersdk.ByteOffset(2*racersdk.PageSize-1),
	)
	if err != nil {
		panic(err)
	}

	first, last, err := page.Resolve(racersdk.PageSize + 3)
	if err != nil {
		panic(err)
	}

	fmt.Println(first, last, racersdk.ByteLength(last-first+1))

	_, _, err = page.Resolve(racersdk.PageSize)

	var typed *racersdk.Error
	if errors.As(err, &typed) {
		fmt.Println(typed.Kind())
	}
	// Output:
	// 16777216 16777218 3
	// unsatisfiable range
}

// exampleSnapshot holds immutable data and its metadata together. A production
// backend should use an immutable version handle or a conditional read instead of
// stat/open of a mutable name. The tiny string here is fixture storage, not an SDK
// buffering requirement. Each call opens an independent reader over the snapshot.
type exampleSnapshot struct {
	metadata racersdk.Metadata
	data     string
}

func newExampleSnapshot(data, version string) exampleSnapshot {
	tag, err := racersdk.ParseETag(version)
	if err != nil {
		panic(err)
	}

	return exampleSnapshot{
		metadata: racersdk.Metadata{
			Size: racersdk.ByteLength(len(data)), ETag: tag,
			ExpiresAt: time.UnixMilli(1_900_000_000_000),
		},
		data: data,
	}
}

// open is the backend portion of the Origin callback below, factored out so its
// HEAD/bootstrap/conditional-range behavior can run without a Unix deployment.
func (s exampleSnapshot) open(ctx context.Context, operation racersdk.Operation, pin racersdk.ETag, page racersdk.Range) (racersdk.Metadata, io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return racersdk.Metadata{}, nil, err
	}
	// Resolve the version before considering HEAD or range satisfiability.
	if pin != (racersdk.ETag{}) && pin != s.metadata.ETag {
		return racersdk.Metadata{}, nil, racersdk.NewOriginError(racersdk.ErrorVersionUnavailable, nil)
	}

	if operation == racersdk.OperationHead {
		return s.metadata, nil, nil
	}

	if operation == racersdk.OperationBootstrap && s.metadata.Size == 0 {
		return s.metadata, nil, nil
	}

	first, last, err := page.Resolve(s.metadata.Size)
	if err != nil {
		// Keep selected-version metadata for the SDK's 416 Content-Range.
		return s.metadata, nil, err
	}
	// String reads never block; NopCloser is safe concurrently with them. For a
	// blocking backend, Close must actively unblock Read and ctx must reach I/O.
	body := io.NopCloser(strings.NewReader(s.data[int(first) : int(last)+1]))

	return s.metadata, body, nil // Ownership transfers; do not defer body.Close.
}

// This executes the immutable backend used by ExampleServeOrigin, without sockets.
// OriginRequest itself is constructed only by the SDK after wire validation.
func ExampleOrigin_immutableSnapshot() {
	snapshot := newExampleSnapshot("hello", `"v1"`)

	page, err := racersdk.ClosedRange(0, racersdk.ByteOffset(racersdk.PageSize-1))
	if err != nil {
		panic(err)
	}

	oldTag, err := racersdk.ParseETag(`"old"`)
	if err != nil {
		panic(err)
	}

	for _, operation := range []racersdk.Operation{
		racersdk.OperationHead, racersdk.OperationBootstrap, racersdk.OperationPinned,
	} {
		pin := racersdk.ETag{}
		if operation == racersdk.OperationPinned {
			pin = snapshot.metadata.ETag
		}

		metadata, body, err := snapshot.open(context.Background(), operation, pin, page)
		if err != nil {
			panic(err)
		}

		if body == nil {
			fmt.Println("HEAD size:", metadata.Size)
			continue
		}
		// io.ReadAll is appropriate only for this known five-byte fixture.
		data, readErr := io.ReadAll(body)

		closeErr := body.Close() // Direct caller owns it here; ServeOrigin normally does.
		if readErr != nil || closeErr != nil {
			panic(errors.Join(readErr, closeErr))
		}

		fmt.Printf("GET: %q\n", data)
	}

	empty := newExampleSnapshot("", `"empty"`)
	metadata, body, err := empty.open(context.Background(), racersdk.OperationBootstrap, racersdk.ETag{}, page)
	fmt.Println("empty:", metadata.Size, body == nil, err)
	_, _, err = empty.open(context.Background(), racersdk.OperationPinned, oldTag, page)

	var typed *racersdk.Error
	if errors.As(err, &typed) {
		fmt.Println("old pin:", typed.Kind()) // Version wins over empty range.
	}

	metadata, _, err = empty.open(context.Background(), racersdk.OperationPinned, empty.metadata.ETag, page)
	if errors.As(err, &typed) {
		fmt.Println("empty pin:", typed.Kind(), "size:", metadata.Size)
	}
	// Output:
	// HEAD size: 5
	// GET: "hello"
	// GET: "hello"
	// empty: 0 true <nil>
	// old pin: version unavailable
	// empty pin: unsatisfiable range size: 0
}

// Deployment-only, compile-checked example: precreate and protect
// /run/racer/models/origin before running. No existing socket may occupy the path.
func ExampleServeOrigin() { //nolint:testableexamples // Requires a provisioned /run/racer origin directory.
	cache, err := racersdk.ParseCacheName("models")
	if err != nil {
		panic(err)
	}
	// This map and its snapshots remain immutable after publication. If replacing
	// a catalog, atomically publish a new one and retain old snapshots for readers.
	objects := map[racersdk.Key]exampleSnapshot{
		{}: newExampleSnapshot("hello", `"v1"`),
	}
	origin := racersdk.Origin(func(ctx context.Context, request racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		if err := ctx.Err(); err != nil {
			return racersdk.Metadata{}, nil, err
		}

		snapshot, found := objects[request.Key()]
		if !found {
			// ServeOrigin maps pinned not-found to 412, unpinned to 404.
			return racersdk.Metadata{}, nil, racersdk.NewOriginError(racersdk.ErrorNotFound, nil)
		}

		pin, _ := request.Pin()
		page, _ := request.Range() // Absent for HEAD; open handles HEAD first.

		return snapshot.open(ctx, request.Operation(), pin, page)
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := racersdk.ServeOrigin(ctx, racersdk.OriginConfig{Cache: cache}, origin); err != nil && !errors.Is(err, context.Canceled) {
		panic(err)
	}
}
