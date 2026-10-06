// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdktest

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk"
)

func originMeta(size racersdk.ByteLength) racersdk.Metadata {
	tag, _ := racersdk.ParseETag(`"v"`)
	return racersdk.Metadata{Size: size, ETag: tag, ExpiresAt: time.UnixMilli(0)}
}

func assertKind(t *testing.T, err error, kind racersdk.ErrorKind) {
	t.Helper()

	var typed *racersdk.Error
	if !errors.As(err, &typed) || typed.Kind() != kind {
		t.Fatalf("error = %v; want kind %v", err, kind)
	}
}

type ownedReader struct {
	io.Reader
	closed atomic.Int32
}

func (b *ownedReader) Close() error { b.closed.Add(1); return nil }

func waitClosed(t *testing.T, b *ownedReader) {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for b.closed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	if b.closed.Load() != 1 {
		t.Fatal("body close count", b.closed.Load())
	}
}

type blockedBody struct {
	done   chan struct{}
	once   sync.Once
	closed atomic.Int32
	first  bool
}

func (b *blockedBody) Read(p []byte) (int, error) {
	if !b.first {
		b.first = true
		p[0] = 'x'

		return 1, nil
	}

	<-b.done

	return 0, context.Canceled
}

func (b *blockedBody) Close() error { b.closed.Add(1); b.once.Do(func() { close(b.done) }); return nil }

type repeatedByte byte

func (b repeatedByte) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}

	return len(p), nil
}

type fakeLateBody struct{ closed chan struct{} }

func (*fakeLateBody) Read([]byte) (int, error) { return 0, io.EOF }
func (b *fakeLateBody) Close() error           { close(b.closed); return nil }

func TestFakeClientOriginValidation(t *testing.T) {
	for _, test := range []struct {
		name string
		size racersdk.ByteLength
		data string
		kind racersdk.ErrorKind
	}{
		{name: "short", size: 3, data: "ab", kind: racersdk.ErrorIO},
		{name: "long", size: 1, data: "ab", kind: racersdk.ErrorIO},
		{name: "empty excess", data: "x", kind: racersdk.ErrorBadGateway},
		{name: "invalid metadata", kind: racersdk.ErrorBadGateway},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := &ownedReader{Reader: strings.NewReader(test.data)}

			client, cleanup, err := NewClient(func(context.Context, racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
				m := originMeta(test.size)
				if test.name == "invalid metadata" {
					m.ETag = racersdk.ETag{}
				}

				return m, body, nil
			})
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(cleanup)

			v, err := client.Get(context.Background(), racersdk.Request{})
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
	client, cleanup, err := NewClient(nil)
	assertKind(t, err, racersdk.ErrorInvalidArgument)

	if client != nil || cleanup != nil {
		t.Fatal("invalid construction returned resources")
	}

	if err.Error() != "racersdk fake origin: invalid argument" {
		t.Fatal("nil origin error changed", err)
	}

	for _, kind := range []racersdk.ErrorKind{racersdk.ErrorUnauthorized, racersdk.ErrorForbidden, racersdk.ErrorNotFound, racersdk.ErrorVersionUnavailable, racersdk.ErrorUnavailable, racersdk.ErrorInternal} {
		t.Run(kind.String(), func(t *testing.T) {
			body := &ownedReader{Reader: strings.NewReader("")}

			client, cleanup, err := NewClient(func(context.Context, racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
				return racersdk.Metadata{}, body, racersdk.NewOriginError(kind, nil)
			})
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(cleanup)

			_, err = client.Get(context.Background(), racersdk.Request{})
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
			client, cleanup, err := NewClient(func(_ context.Context, r racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
				// Fail the selected page, not whichever callback arrives first.
				requested, _ := r.Range()

				first, _, err := requested.Resolve(3 * racersdk.PageSize)
				if err != nil {
					return racersdk.Metadata{}, nil, err
				}

				if uint64(first)/uint64(racersdk.PageSize) == uint64(failPage) {
					return racersdk.Metadata{}, nil, racersdk.NewOriginError(racersdk.ErrorNotFound, nil)
				}

				return originMeta(3 * racersdk.PageSize), io.NopCloser(io.LimitReader(repeatedByte('x'), int64(racersdk.PageSize))), nil
			})
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(cleanup)

			v, err := client.Get(context.Background(), racersdk.Request{})
			if err != nil {
				t.Fatal(err)
			}

			n, err := io.Copy(io.Discard, v)
			if n != int64(failPage)*int64(racersdk.PageSize) {
				t.Fatal("incorrect partial byte count", n)
			}

			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatal("late multipage failure must truncate the committed frame", err)
			}
		})
	}
}

func TestFakeClientCancellationAndCleanup(t *testing.T) {
	for _, action := range []string{"context", "value", "client", "cleanup"} {
		t.Run(action, func(t *testing.T) {
			body := &blockedBody{done: make(chan struct{}), first: true}

			client, cleanup, err := NewClient(func(context.Context, racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
				return originMeta(1), body, nil
			})
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(cleanup)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			v, err := client.Get(ctx, racersdk.Request{})
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
					assertKind(t, err, racersdk.ErrorClosed)
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

			_, err = client.Get(context.Background(), racersdk.Request{})
			assertKind(t, err, racersdk.ErrorClosed)

			if body.closed.Load() != 1 {
				t.Fatal("body close count", body.closed.Load())
			}
		})
	}
}

func TestFakeClientPendingCallbackCleanup(t *testing.T) {
	entered, release, closed := make(chan struct{}), make(chan struct{}), make(chan struct{})

	client, cleanup, err := NewClient(func(ctx context.Context, _ racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
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

	go func() { _, err := client.Get(context.Background(), racersdk.Request{}); result <- err }()

	<-entered
	cleanup()
	close(release)

	select {
	case err := <-result:
		assertKind(t, err, racersdk.ErrorClosed)
	case <-time.After(5 * time.Second):
		t.Fatal("pending Get retained")
	}

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("late callback body leaked")
	}
}

func TestFakeClientMetadataUnderSaturation(t *testing.T) {
	for _, phase := range []string{"callback", "body"} {
		t.Run(phase, func(t *testing.T) {
			const connections = 16

			entered := make(chan *blockedBody, connections)

			client, cleanup, err := NewClient(func(ctx context.Context, r racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
				if r.Operation() == racersdk.OperationHead {
					return originMeta(1), nil, nil
				}

				if phase == "callback" {
					entered <- nil

					<-ctx.Done()

					return racersdk.Metadata{}, nil, ctx.Err()
				}

				body := &blockedBody{done: make(chan struct{}), first: true}
				entered <- body

				return originMeta(1), body, nil
			})
			if err != nil {
				t.Fatal(err)
			}

			results := make(chan error, connections)

			t.Cleanup(func() {
				cleanup()

				deadline := time.After(5 * time.Second)

				for range connections {
					select {
					case err := <-results:
						if err == nil {
							t.Error("blocked GET completed without an error")
						}
					case <-deadline:
						t.Error("cleanup retained blocked GETs")
						return
					}
				}
			})

			for range connections {
				go func() {
					v, err := client.Get(context.Background(), racersdk.Request{})
					if err == nil {
						_, err = io.Copy(io.Discard, v)
						closeBody(v)
					}

					results <- err
				}()
			}

			var bodies []*blockedBody

			deadline := time.After(5 * time.Second)

			for range connections {
				select {
				case body := <-entered:
					if body != nil {
						bodies = append(bodies, body)
					}
				case <-deadline:
					t.Fatal("GETs did not saturate the origin transport")
				}
			}

			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()

			metadata, err := client.Stat(ctx, racersdk.Request{})

			want := originMeta(1)
			if err != nil || metadata.Size != want.Size || metadata.ETag != want.ETag || !metadata.ExpiresAt.Equal(want.ExpiresAt) || metadata.ContentType != want.ContentType {
				t.Fatalf("HEAD starved behind blocked GETs: metadata=%+v, error=%v", metadata, err)
			}

			cleanup()

			for _, body := range bodies {
				select {
				case <-body.done:
				case <-time.After(5 * time.Second):
					t.Fatal("cleanup retained an origin body")
				}

				if body.closed.Load() != 1 {
					t.Fatal("origin body close count", body.closed.Load())
				}
			}
		})
	}
}

func TestFakeClientImmutableContinuation(t *testing.T) {
	for _, change := range []string{"pin", "size"} {
		t.Run(change, func(t *testing.T) {
			client, cleanup, err := NewClient(func(_ context.Context, r racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
				m := originMeta(3 * racersdk.PageSize)
				page, _ := r.Range()

				first, _, err := page.Resolve(m.Size)
				if err != nil {
					return racersdk.Metadata{}, nil, err
				}

				if first == racersdk.ByteOffset(2*racersdk.PageSize) {
					if change == "pin" {
						m.ETag, _ = racersdk.ParseETag(`"different"`)
					} else {
						m.Size++
					}
				}

				return m, io.NopCloser(io.LimitReader(repeatedByte('x'), int64(racersdk.PageSize))), nil
			})
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(cleanup)

			v, err := client.Get(context.Background(), racersdk.Request{})
			if err != nil {
				t.Fatal(err)
			}

			n, err := io.Copy(io.Discard, v)
			if n != 2*int64(racersdk.PageSize) {
				t.Fatal("changed immutable version was accepted", n, err)
			}

			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatal("late immutable metadata failure must abort the committed frame", err)
			}
		})
	}
}

func TestFakeClientFreshGets(t *testing.T) {
	var calls atomic.Int32

	client, cleanup, err := NewClient(func(_ context.Context, r racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		if r.Operation() != racersdk.OperationBootstrap {
			t.Error("fresh Get reused a pin")
		}

		version := strconv.Itoa(int(calls.Add(1)))
		m := originMeta(1)
		m.ETag, _ = racersdk.ParseETag(`"` + version + `"`)

		return m, io.NopCloser(strings.NewReader(version)), nil
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(cleanup)

	for _, want := range []string{"1", "2"} {
		v, err := client.Get(context.Background(), racersdk.Request{})
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

func TestTemporarySockets(t *testing.T) {
	parent := t.TempDir()
	t.Setenv("TMPDIR", parent)

	client, cleanup, err := NewClient(func(context.Context, racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		return originMeta(0), nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(cleanup)

	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 1 {
		t.Fatal("temporary directory", entries, err)
	}

	dir := filepath.Join(parent, entries[0].Name())
	for _, name := range []string{"o", "c"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode()&os.ModeSocket == 0 {
			t.Fatal("missing socket", name, err)
		}
	}

	closeBody(client)

	if _, err := os.Stat(dir); err != nil {
		t.Fatal("Client.Close stopped servers", err)
	}

	cleanup()

	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cleanup retained directory", err)
	}
}

func TestTMPDIRCanonicalPaths(t *testing.T) {
	for _, name := range []string{"relative", "symlink"} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()

			parent := filepath.Join(base, "real", "tmp")
			if err := os.MkdirAll(parent, 0o700); err != nil {
				t.Fatal(err)
			}

			if name == "relative" {
				t.Chdir(base)
				t.Setenv("TMPDIR", filepath.Join("real", "tmp"))
			} else {
				link := filepath.Join(base, "link")
				if err := os.Symlink(filepath.Join(base, "real"), link); err != nil {
					t.Fatal(err)
				}

				t.Setenv("TMPDIR", filepath.Join(link, "tmp"))
			}

			client, cleanup, err := NewClient(func(context.Context, racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
				return originMeta(5), io.NopCloser(strings.NewReader("hello")), nil
			})
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(cleanup)

			entries, err := os.ReadDir(parent)
			if err != nil || len(entries) != 1 {
				t.Fatal("temporary directory", entries, err)
			}

			dir := filepath.Join(parent, entries[0].Name())
			for _, socket := range []string{"o", "c"} {
				info, err := os.Stat(filepath.Join(dir, socket))
				if err != nil || info.Mode()&os.ModeSocket == 0 {
					t.Fatal("missing socket", socket, err)
				}
			}

			v, err := client.Get(context.Background(), racersdk.Request{})
			if err != nil {
				t.Fatal(err)
			}

			data, err := io.ReadAll(v)
			closeBody(v)

			if err != nil || string(data) != "hello" {
				t.Fatal("Get round trip", string(data), err)
			}

			cleanup()

			if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("cleanup retained directory", err)
			}
		})
	}
}

func TestTemporarySocketFailures(t *testing.T) {
	for _, name := range []string{"long", "missing"} {
		t.Run(name, func(t *testing.T) {
			parent := filepath.Join(t.TempDir(), strings.Repeat("x", 108))
			if name == "long" {
				if err := os.Mkdir(parent, 0o700); err != nil {
					t.Fatal(err)
				}
			}

			t.Setenv("TMPDIR", parent)

			client, cleanup, err := NewClient(func(context.Context, racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
				return originMeta(0), nil, nil
			})
			if err == nil || client != nil || cleanup != nil {
				t.Fatal("failure returned resources", err)
			}

			if name == "long" {
				if !strings.Contains(err.Error(), "107-byte limit") {
					t.Fatal(err)
				}

				entries, err := os.ReadDir(parent)
				if err != nil || len(entries) != 0 {
					t.Fatal("failed startup leaked directory", entries, err)
				}
			}
		})
	}
}

func TestTrackedHandlerStop(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	h := &trackedHandler{next: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	})}
	done := make(chan struct{})

	go func() {
		defer close(done)

		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}()

	<-entered
	h.stop()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	select {
	case <-done:
		t.Fatal("active handler returned before release")
	default:
	}

	close(release)
	h.active.Wait()
	<-done
}

func TestOriginStartupFailure(t *testing.T) {
	want := errors.New("origin startup failed")

	done := make(chan error, 1)
	done <- want

	if err := waitOrigin(filepath.Join(t.TempDir(), "missing"), done); !errors.Is(err, want) {
		t.Fatalf("startup error = %v, want %v", err, want)
	}
}
