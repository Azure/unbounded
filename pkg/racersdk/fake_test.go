// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFakeClientPages(t *testing.T) {
	for _, size := range []ByteLength{0, 1, PageSize, PageSize + 13, 3*PageSize + 13} {
		t.Run(strconv.FormatUint(uint64(size), 10), func(t *testing.T) {
			var calls atomic.Int32

			request := Request{Key: Key{1, 2, 3}, Context: FetchContext{
				metadata:      AdapterMetadata{value: "opaque, interior  spaces\\\xff"},
				authorization: Authorization{value: "Scheme opaque,credential\x80"},
			}}

			client, cleanup, err := NewFakeClient(func(_ context.Context, r OriginRequest) (Metadata, io.ReadCloser, error) {
				call := calls.Add(1)

				if r.Key() != request.Key || r.Context() != request.Context {
					t.Error("key or context changed")
				}

				m := originMeta(size)

				page, ok := r.Range()
				if !ok || validatePageShape(page) != nil {
					t.Error("origin received a non-page range")
				}

				if call == 1 {
					if _, pinned := r.Pin(); pinned || r.Operation() != OperationBootstrap {
						t.Error("bootstrap changed")
					}
				} else {
					m.ExpiresAt = time.UnixMilli(int64(call))

					pin, ok := r.Pin()
					if !ok || pin != m.ETag || r.Operation() != OperationPinned {
						t.Error("continuation lost immutable pin")
					}
				}

				if size == 0 {
					return m, nil, nil
				}

				first, last, err := page.Resolve(size)
				if err != nil {
					return m, nil, err
				}

				if first != ByteOffset(call-1)*ByteOffset(PageSize) {
					t.Error("page scheduling is not sequential")
				}

				return m, io.NopCloser(io.LimitReader(&offsetStream{offset: int64(first)}, int64(last-first)+1)), nil
			})
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(cleanup)

			value, err := client.Get(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			defer closeBody(value)

			if calls.Load() != 1 {
				t.Fatal("Get eagerly fetched a continuation")
			}

			sink := &offsetSink{}

			n, err := io.Copy(sink, value)
			if err != nil || n != int64(size) || sink.offset != int64(size) {
				t.Fatal("stream mismatch", n, err)
			}

			m := value.Metadata()
			if calls.Load() != int32(max(1, (size+PageSize-1)/PageSize)) || m.Size != size || m.ETag != originMeta(size).ETag || m.ExpiresAt.UnixMilli() != 0 {
				t.Fatal("page count or metadata snapshot changed")
			}
		})
	}
}

func TestFakeClientOriginValidation(t *testing.T) {
	for _, test := range []struct {
		name string
		size ByteLength
		data string
		kind ErrorKind
	}{
		{name: "short", size: 3, data: "ab", kind: ErrorIO},
		{name: "long", size: 1, data: "ab", kind: ErrorIO},
		{name: "empty excess", data: "x", kind: ErrorBadGateway},
		{name: "invalid metadata", kind: ErrorBadGateway},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := &ownedReader{Reader: strings.NewReader(test.data)}

			client, cleanup, err := NewFakeClient(func(context.Context, OriginRequest) (Metadata, io.ReadCloser, error) {
				m := originMeta(test.size)
				if test.name == "invalid metadata" {
					m.ETag = ETag{}
				}

				return m, body, nil
			})
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(cleanup)

			v, err := client.Get(context.Background(), Request{})
			if err == nil {
				_, err = io.Copy(io.Discard, v)
				closeBody(v)
			}

			assertKind(t, err, test.kind)
			waitClosed(t, body)

			if body.closed.Load() != 1 {
				t.Fatal("origin body not closed exactly once")
			}
		})
	}
}

func TestFakeClientErrors(t *testing.T) {
	client, cleanup, err := NewFakeClient(nil)
	assertKind(t, err, ErrorInvalidArgument)

	if client != nil || cleanup != nil {
		t.Fatal("invalid construction returned resources")
	}

	for _, kind := range []ErrorKind{ErrorUnauthorized, ErrorForbidden, ErrorNotFound, ErrorVersionUnavailable, ErrorUnavailable, ErrorInternal} {
		t.Run(kind.String(), func(t *testing.T) {
			body := &ownedReader{Reader: strings.NewReader("")}

			client, cleanup, err := NewFakeClient(func(context.Context, OriginRequest) (Metadata, io.ReadCloser, error) {
				return Metadata{}, body, NewOriginError(kind, nil)
			})
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(cleanup)

			_, err = client.Get(context.Background(), Request{})
			assertKind(t, err, kind)
			waitClosed(t, body)

			if body.closed.Load() != 1 {
				t.Fatal("error body leaked")
			}
		})
	}
}

func TestFakeClientContinuationErrors(t *testing.T) {
	for _, failPage := range []int32{1, 2} {
		t.Run(strconv.Itoa(int(failPage)), func(t *testing.T) {
			var calls atomic.Int32

			client, cleanup, err := NewFakeClient(func(_ context.Context, r OriginRequest) (Metadata, io.ReadCloser, error) {
				if calls.Add(1)-1 == failPage {
					return Metadata{}, nil, NewOriginError(ErrorNotFound, nil)
				}

				return originMeta(3 * PageSize), io.NopCloser(io.LimitReader(repeatedByte('x'), int64(PageSize))), nil
			})
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(cleanup)

			v, err := client.Get(context.Background(), Request{})
			if err != nil {
				t.Fatal(err)
			}

			n, err := io.Copy(io.Discard, v)
			if n != int64(failPage)*int64(PageSize) {
				t.Fatal("incorrect partial byte count", n)
			}

			if failPage == 1 {
				assertKind(t, err, ErrorVersionUnavailable)
			} else if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatal("late failure did not abort", err)
			}
		})
	}
}

func TestFakeClientCancellationAndCleanup(t *testing.T) {
	for _, action := range []string{"context", "value", "client", "cleanup"} {
		t.Run(action, func(t *testing.T) {
			body := &blockedBody{done: make(chan struct{}), first: true}

			client, cleanup, err := NewFakeClient(func(context.Context, OriginRequest) (Metadata, io.ReadCloser, error) {
				return originMeta(1), body, nil
			})
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(cleanup)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			v, err := client.Get(ctx, Request{})
			if err != nil {
				t.Fatal(err)
			}

			result := make(chan error, 1)

			go func() { _, err := v.Read(make([]byte, 1)); result <- err }()

			switch action {
			case "context":
				cancel()
			case "value":
				closeBody(v)
			case "client":
				closeBody(client)
			case "cleanup":
				var wg sync.WaitGroup
				for range 4 {
					wg.Go(cleanup)
				}

				wg.Wait()
			}

			select {
			case err := <-result:
				if action == "context" {
					if !errors.Is(err, context.Canceled) {
						t.Fatal(err)
					}
				} else {
					assertKind(t, err, ErrorClosed)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("read did not stop")
			}

			select {
			case <-body.done:
			case <-time.After(5 * time.Second):
				t.Fatal("origin body retained after cancellation")
			}

			cleanup()

			_, err = client.Get(context.Background(), Request{})
			assertKind(t, err, ErrorClosed)

			if body.closed.Load() != 1 {
				t.Fatal("body close count", body.closed.Load())
			}
		})
	}
}

func TestFakeClientPendingCallbackCleanup(t *testing.T) {
	entered, release, closed := make(chan struct{}), make(chan struct{}), make(chan struct{})

	client, cleanup, err := NewFakeClient(func(ctx context.Context, _ OriginRequest) (Metadata, io.ReadCloser, error) {
		close(entered)
		<-ctx.Done()
		<-release

		return originMeta(0), &fakeLateBody{closed: closed}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(cleanup)

	result := make(chan error, 1)

	go func() { _, err := client.Get(context.Background(), Request{}); result <- err }()

	<-entered
	cleanup()
	close(release)

	select {
	case err := <-result:
		assertKind(t, err, ErrorClosed)
	case <-time.After(5 * time.Second):
		t.Fatal("pending Get retained")
	}

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("late callback body leaked")
	}
}

func TestFakeClientImmutableContinuation(t *testing.T) {
	for _, change := range []string{"pin", "size"} {
		t.Run(change, func(t *testing.T) {
			client, cleanup, err := NewFakeClient(func(_ context.Context, r OriginRequest) (Metadata, io.ReadCloser, error) {
				m := originMeta(3 * PageSize)
				page, _ := r.Range()

				first, _, err := page.Resolve(m.Size)
				if err != nil {
					return Metadata{}, nil, err
				}

				if first == ByteOffset(2*PageSize) {
					if change == "pin" {
						m.ETag = ETag{value: `"different"`}
					} else {
						m.Size++
					}
				}

				return m, io.NopCloser(io.LimitReader(repeatedByte('x'), int64(PageSize))), nil
			})
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(cleanup)

			v, err := client.Get(context.Background(), Request{})
			if err != nil {
				t.Fatal(err)
			}

			n, err := io.Copy(io.Discard, v)
			if n != 2*int64(PageSize) || !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatal("changed immutable version was accepted", n, err)
			}
		})
	}
}

func TestFakeClientFreshGets(t *testing.T) {
	var calls atomic.Int32

	client, cleanup, err := NewFakeClient(func(_ context.Context, r OriginRequest) (Metadata, io.ReadCloser, error) {
		if r.Operation() != OperationBootstrap {
			t.Error("fresh Get reused a pin")
		}

		version := strconv.Itoa(int(calls.Add(1)))
		m := originMeta(1)
		m.ETag = ETag{value: `"` + version + `"`}

		return m, io.NopCloser(strings.NewReader(version)), nil
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(cleanup)

	for _, want := range []string{"1", "2"} {
		v, err := client.Get(context.Background(), Request{})
		if err != nil {
			t.Fatal(err)
		}

		data, err := io.ReadAll(v)
		closeBody(v)

		if err != nil || string(data) != want || v.Metadata().ETag.String() != `"`+want+`"` {
			t.Fatal("fresh Get did not select a fresh version", err)
		}
	}
}

type fakeLateBody struct{ closed chan struct{} }

func (*fakeLateBody) Read([]byte) (int, error) { return 0, io.EOF }
func (b *fakeLateBody) Close() error           { close(b.closed); return nil }
