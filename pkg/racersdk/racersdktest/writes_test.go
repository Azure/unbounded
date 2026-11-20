// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdktest_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Azure/unbounded/pkg/racersdk"
	"github.com/Azure/unbounded/pkg/racersdk/racersdktest"
)

const azureMetadata = `{"schema":1,"bucket":"b","key":"k"}`

func writeRequest(b byte) racersdk.Request {
	var key racersdk.Key

	key[0] = b

	return racersdk.Request{Key: key, Metadata: azureMetadata}
}

func readAll(t *testing.T, c *racersdk.Client, r racersdk.Request) []byte {
	t.Helper()

	obj, err := c.Get(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	defer obj.Close()

	data, err := io.ReadAll(obj)
	if err != nil {
		t.Fatal(err)
	}

	return data
}

func TestPutRoundTrip(t *testing.T) {
	c, writes := racersdktest.NewClientWithWrites(t, nil)
	ctx := context.Background()
	r := writeRequest(1)
	data := bytes.Repeat([]byte("racer"), 1000)

	result, err := c.Put(ctx, r, bytes.NewReader(data), int64(len(data)), racersdk.WriteOptions{
		Replicas: 3, Upload: racersdk.UploadManual, Hold: 1500 * time.Millisecond, ContentType: "text/plain",
	})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(result.ETag, `"racer-`) || len(result.ETag) != len(`"racer-`)+32+1 {
		t.Fatalf("etag %q", result.ETag)
	}

	if result.UploadState != racersdk.UploadStateManual {
		t.Fatalf("state %q", result.UploadState)
	}

	o, ok := writes.Object(r.Key)
	if !ok || o.Replicas != 3 || o.Upload != racersdk.UploadManual || o.Hold != 2*time.Second || o.ContentType != "text/plain" {
		t.Fatalf("object %+v", o)
	}

	calls := writes.Calls()
	if len(calls) != 1 || calls[0].Kind != "put" || calls[0].Header["If-None-Match"][0] != "*" ||
		calls[0].Header["Racer-Durability"][0] != "replicas=3" || calls[0].Header["Racer-Hold-Seconds"][0] != "2" {
		t.Fatalf("calls %+v", calls)
	}

	if got := readAll(t, c, r); !bytes.Equal(got, data) {
		t.Fatal("read back mismatch")
	}

	m, err := c.Stat(ctx, r)
	if err != nil {
		t.Fatal(err)
	}

	if m.ETag != result.ETag || m.Size != int64(len(data)) || m.UploadState != racersdk.UploadStateManual || m.ContentType != "text/plain" {
		t.Fatalf("stat %+v", m)
	}

	state, err := c.Persist(ctx, r)
	if err != nil || state != racersdk.UploadStatePending {
		t.Fatalf("persist %q %v", state, err)
	}

	if err := c.Delete(ctx, r); err != nil {
		t.Fatal(err)
	}

	if _, ok := writes.Object(r.Key); ok {
		t.Fatal("object survived delete")
	}

	if err := c.Delete(ctx, r); !errors.Is(err, racersdk.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestPutMultiPage(t *testing.T) {
	c, _ := racersdktest.NewClientWithWrites(t, nil)
	r := writeRequest(2)
	data := make([]byte, racersdk.PageSize+12345)

	for i := range data {
		data[i] = byte(i * 7)
	}

	if _, err := c.Put(context.Background(), r, bytes.NewReader(data), int64(len(data)), racersdk.WriteOptions{Upload: racersdk.UploadNever}); err != nil {
		t.Fatal(err)
	}

	if got := readAll(t, c, r); !bytes.Equal(got, data) {
		t.Fatal("read back mismatch")
	}
}

func TestPutCreateOnce(t *testing.T) {
	origin := func(_ context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		if req.Key[0] == 9 {
			return racersdk.Metadata{Size: 1, ETag: `"o"`, ExpiresAt: time.Now().Add(time.Hour)}, io.NopCloser(strings.NewReader("x")), nil
		}

		return racersdk.Metadata{}, nil, racersdk.ErrNotFound
	}
	c, _ := racersdktest.NewClientWithWrites(t, origin)
	ctx := context.Background()

	r := writeRequest(3)
	if _, err := c.Put(ctx, r, strings.NewReader("a"), 1, racersdk.WriteOptions{}); err != nil {
		t.Fatal(err)
	}

	_, err := c.Put(ctx, r, strings.NewReader("b"), 1, racersdk.WriteOptions{})
	if !errors.Is(err, racersdk.ErrExists) {
		t.Fatalf("second put: %v", err)
	}

	var we *racersdk.WriteError
	if !errors.As(err, &we) || we.Status != 412 || we.Op != "put" {
		t.Fatalf("error %#v", err)
	}

	if _, err := c.Put(ctx, writeRequest(9), strings.NewReader("b"), 1, racersdk.WriteOptions{}); !errors.Is(err, racersdk.ErrExists) {
		t.Fatalf("origin object: %v", err)
	}
}

// TestPutCreateOnceOriginFailure checks that a create-once probe that fails
// for any reason other than not-found fails the write with 503, as Racer does,
// and creates nothing.
func TestPutCreateOnceOriginFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"unavailable", racersdk.ErrUnavailable},
		{"unauthorized", racersdk.ErrUnauthorized},
		{"other", errors.New("boom")},
		{"panic", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var fail atomic.Bool

			fail.Store(true)

			origin := func(_ context.Context, _ racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
				if fail.Load() {
					if tc.err == nil {
						panic("origin panic")
					}

					return racersdk.Metadata{}, nil, tc.err
				}

				return racersdk.Metadata{}, nil, racersdk.ErrNotFound
			}
			c, writes := racersdktest.NewClientWithWrites(t, origin)
			ctx := context.Background()
			r := writeRequest(4)

			_, err := c.Put(ctx, r, strings.NewReader("a"), 1, racersdk.WriteOptions{})

			var we *racersdk.WriteError
			if !errors.As(err, &we) || we.Status != 503 || !errors.Is(err, racersdk.ErrBusy) || errors.Is(err, racersdk.ErrExists) {
				t.Fatalf("put: %v", err)
			}

			if _, ok := writes.Object(r.Key); ok {
				t.Fatal("failed probe created the object")
			}

			fail.Store(false)

			if _, err := c.Put(ctx, r, strings.NewReader("a"), 1, racersdk.WriteOptions{}); err != nil {
				t.Fatalf("put after recovery: %v", err)
			}

			if _, ok := writes.Object(r.Key); !ok {
				t.Fatal("not-found probe did not create the object")
			}
		})
	}
}

func TestWriteDisabled(t *testing.T) {
	c, writes := racersdktest.NewClientWithWrites(t, nil)
	writes.SetEnabled(false)

	ctx := context.Background()
	r := writeRequest(4)

	if _, err := c.Put(ctx, r, strings.NewReader("a"), 1, racersdk.WriteOptions{}); !errors.Is(err, racersdk.ErrWritesDisabled) {
		t.Fatalf("put: %v", err)
	}

	if _, err := c.Persist(ctx, r); !errors.Is(err, racersdk.ErrWritesDisabled) {
		t.Fatalf("persist: %v", err)
	}

	if err := c.Delete(ctx, r); !errors.Is(err, racersdk.ErrWritesDisabled) {
		t.Fatalf("delete: %v", err)
	}
}

func TestWriteWithoutWriteSupport(t *testing.T) {
	c := racersdktest.NewClient(t, func(context.Context, racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		return racersdk.Metadata{}, nil, racersdk.ErrNotFound
	})

	if _, err := c.Put(context.Background(), writeRequest(5), strings.NewReader("a"), 1, racersdk.WriteOptions{}); !errors.Is(err, racersdk.ErrWritesDisabled) {
		t.Fatalf("put: %v", err)
	}
}

func TestWriteRejections(t *testing.T) {
	cases := []struct {
		status int
		code   string
		want   []error
	}{
		{503, "dirty-cap", []error{racersdk.ErrBusy, racersdk.ErrUnavailable}},
		{502, "upload", []error{racersdk.ErrUploadFailed}},
		{409, "conflict", []error{racersdk.ErrConflict}},
		{428, "", []error{racersdk.ErrInvalidRequest}},
		{413, "", []error{racersdk.ErrInvalidRequest}},
		{504, "", []error{racersdk.ErrUnavailable}},
	}

	for _, tc := range cases {
		c, writes := racersdktest.NewClientWithWrites(t, nil)
		writes.Reject(func(racersdktest.WriteCall) *racersdktest.WriteRejection {
			return &racersdktest.WriteRejection{Status: tc.status, Code: tc.code, RetryAfter: 2 * time.Second}
		})

		_, err := c.Put(context.Background(), writeRequest(6), strings.NewReader("abc"), 3, racersdk.WriteOptions{})
		for _, want := range tc.want {
			if !errors.Is(err, want) {
				t.Errorf("%d: %v is not %v", tc.status, err, want)
			}
		}

		var we *racersdk.WriteError
		if !errors.As(err, &we) || we.Status != tc.status || we.Code != tc.code || we.RetryAfter != 2*time.Second {
			t.Errorf("%d: %#v", tc.status, err)
		}

		if errors.Is(err, racersdk.ErrExists) {
			t.Errorf("%d matched ErrExists", tc.status)
		}
	}
}

func TestPutEarlyRejectionLargeBody(t *testing.T) {
	c, writes := racersdktest.NewClientWithWrites(t, nil)
	writes.Reject(func(racersdktest.WriteCall) *racersdktest.WriteRejection {
		return &racersdktest.WriteRejection{Status: 503, Code: "owners", RetryAfter: time.Second}
	})

	size := int64(3 * racersdk.PageSize)

	_, err := c.Put(context.Background(), writeRequest(7), io.LimitReader(zeroReader{}, size), size, racersdk.WriteOptions{})
	if !errors.Is(err, racersdk.ErrBusy) {
		t.Fatalf("put: %v", err)
	}
}

func TestPutRetryBusy(t *testing.T) {
	c, writes := racersdktest.NewClientWithWrites(t, nil)

	var attempts atomic.Int32

	writes.Reject(func(racersdktest.WriteCall) *racersdktest.WriteRejection {
		if attempts.Add(1) == 1 {
			return &racersdktest.WriteRejection{Status: 503, Code: "dirty-cap", RetryAfter: time.Second}
		}

		return nil
	})

	r := writeRequest(8)
	if _, err := c.Put(context.Background(), r, strings.NewReader("hello"), 5, racersdk.WriteOptions{RetryBusy: 10 * time.Second}); err != nil {
		t.Fatal(err)
	}

	if attempts.Load() != 2 {
		t.Fatalf("attempts %d", attempts.Load())
	}

	if got := readAll(t, c, r); string(got) != "hello" {
		t.Fatalf("got %q", got)
	}
}

func TestPutNoRetryWithoutOptIn(t *testing.T) {
	c, writes := racersdktest.NewClientWithWrites(t, nil)

	var attempts atomic.Int32

	writes.Reject(func(racersdktest.WriteCall) *racersdktest.WriteRejection {
		attempts.Add(1)
		return &racersdktest.WriteRejection{Status: 503, RetryAfter: time.Second}
	})

	_, err := c.Put(context.Background(), writeRequest(8), strings.NewReader("hello"), 5, racersdk.WriteOptions{})
	if !errors.Is(err, racersdk.ErrBusy) || attempts.Load() != 1 {
		t.Fatalf("%v after %d attempts", err, attempts.Load())
	}

	// A plain reader cannot be rewound, so opting in does not retry it.
	attempts.Store(0)

	_, err = c.Put(context.Background(), writeRequest(8), io.LimitReader(strings.NewReader("hello"), 5), 5, racersdk.WriteOptions{RetryBusy: time.Minute})
	if !errors.Is(err, racersdk.ErrBusy) || attempts.Load() != 1 {
		t.Fatalf("%v after %d attempts", err, attempts.Load())
	}
}

func TestPutInvalid(t *testing.T) {
	c, writes := racersdktest.NewClientWithWrites(t, nil)
	ctx := context.Background()
	r := writeRequest(10)

	checks := []struct {
		name string
		err  error
	}{
		{"zero length", put(c, r, "", 0, racersdk.WriteOptions{})},
		{"too large", put(c, r, "a", racersdk.MaxWriteSize+1, racersdk.WriteOptions{})},
		{"replicas", put(c, r, "a", 1, racersdk.WriteOptions{Replicas: 4})},
		{"origin with replicas", put(c, r, "a", 1, racersdk.WriteOptions{OriginDurable: true, Replicas: 2})},
		{"origin never", put(c, r, "a", 1, racersdk.WriteOptions{OriginDurable: true, Upload: racersdk.UploadNever})},
		{"upload", put(c, r, "a", 1, racersdk.WriteOptions{Upload: "sometimes"})},
		{"hold", put(c, r, "a", 1, racersdk.WriteOptions{Hold: racersdk.MaxHold + time.Second})},
		{"content type", put(c, r, "a", 1, racersdk.WriteOptions{ContentType: " x"})},
		{"short body", put(c, r, "ab", 3, racersdk.WriteOptions{})},
		{"version id", put(c, racersdk.Request{Key: r.Key, Metadata: `{"schema":1,"bucket":"b","key":"k","versionId":"v"}`}, "a", 1, racersdk.WriteOptions{})},
	}

	for _, check := range checks {
		if !errors.Is(check.err, racersdk.ErrInvalidRequest) {
			t.Errorf("%s: %v", check.name, check.err)
		}
	}

	if _, err := c.Put(ctx, r, nil, 1, racersdk.WriteOptions{}); !errors.Is(err, racersdk.ErrInvalidRequest) {
		t.Errorf("nil body: %v", err)
	}

	// Only the short body can reach the fake; client-side checks send nothing.
	if n := len(writes.Calls()); n > 1 {
		t.Errorf("%d calls", n)
	}

	if _, ok := writes.Object(r.Key); ok {
		t.Error("short body created an object")
	}
}

func put(c *racersdk.Client, r racersdk.Request, body string, length int64, o racersdk.WriteOptions) error {
	_, err := c.Put(context.Background(), r, strings.NewReader(body), length, o)
	return err
}

func TestPutOriginDurable(t *testing.T) {
	c, writes := racersdktest.NewClientWithWrites(t, nil)
	r := writeRequest(11)

	result, err := c.Put(context.Background(), r, strings.NewReader("x"), 1, racersdk.WriteOptions{OriginDurable: true})
	if err != nil || result.UploadState != racersdk.UploadStateCommitted {
		t.Fatalf("%+v %v", result, err)
	}

	if h := writes.Calls()[0].Header["Racer-Durability"]; len(h) != 1 || h[0] != "origin" {
		t.Fatalf("durability %v", h)
	}

	if err := c.Delete(context.Background(), r); !errors.Is(err, racersdk.ErrConflict) {
		t.Fatalf("delete committed: %v", err)
	}
}

func TestPersistStates(t *testing.T) {
	c, writes := racersdktest.NewClientWithWrites(t, nil)
	ctx := context.Background()

	never := writeRequest(12)
	if _, err := c.Put(ctx, never, strings.NewReader("x"), 1, racersdk.WriteOptions{Upload: racersdk.UploadNever}); err != nil {
		t.Fatal(err)
	}

	if _, err := c.Persist(ctx, never); !errors.Is(err, racersdk.ErrConflict) {
		t.Fatalf("persist never: %v", err)
	}

	if _, err := c.Persist(ctx, writeRequest(13)); !errors.Is(err, racersdk.ErrNotFound) {
		t.Fatalf("persist missing: %v", err)
	}

	if !writes.SetUploadState(never.Key, racersdk.UploadStateCommitted) {
		t.Fatal("set state")
	}

	m, err := c.Stat(ctx, never)
	if err != nil || m.UploadState != racersdk.UploadStateCommitted {
		t.Fatalf("stat %+v %v", m, err)
	}
}

func TestStatOriginObjectHasNoUploadState(t *testing.T) {
	c, _ := racersdktest.NewClientWithWrites(t, func(context.Context, racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		return racersdk.Metadata{Size: 1, ETag: `"o"`, ExpiresAt: time.Now().Add(time.Hour)}, nil, nil
	})

	m, err := c.Stat(context.Background(), writeRequest(14))
	if err != nil || m.UploadState != "" {
		t.Fatalf("%+v %v", m, err)
	}
}

func memfd(t *testing.T, data []byte) *os.File {
	t.Helper()

	fd, err := unix.MemfdCreate("racer-test", unix.MFD_CLOEXEC)
	if err != nil {
		t.Skipf("memfd: %v", err)
	}

	f := os.NewFile(uintptr(fd), "memfd")

	t.Cleanup(func() { _ = f.Close() })

	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}

	return f
}

func TestHostWrite(t *testing.T) {
	c, writes := racersdktest.NewClientWithWrites(t, nil)
	ctx := context.Background()
	data := []byte("0123456789abcdef")
	f := memfd(t, data)
	r := writeRequest(15)

	result, err := c.HostWrite(ctx, r, f, 3, 10, racersdk.WriteOptions{Replicas: 1})
	if err != nil {
		t.Fatal(err)
	}

	if result.UploadState != racersdk.UploadStatePending {
		t.Fatalf("state %q", result.UploadState)
	}

	call := writes.Calls()[0]
	if call.Kind != "host-write" || call.Files != 1 || call.Header["Racer-Source-Offset"][0] != "3" ||
		call.Header["Racer-Source-Length"][0] != "10" || call.Header["Content-Length"][0] != "0" {
		t.Fatalf("call %+v", call)
	}

	if got := readAll(t, c, r); string(got) != "3456789abc" {
		t.Fatalf("got %q", got)
	}

	// The window must lie inside the file.
	if _, err := c.HostWrite(ctx, writeRequest(16), f, 10, 10, racersdk.WriteOptions{}); !errors.Is(err, racersdk.ErrInvalidRequest) {
		t.Fatalf("past end: %v", err)
	}

	if _, err := c.HostWrite(ctx, r, f, 0, 1, racersdk.WriteOptions{}); !errors.Is(err, racersdk.ErrExists) {
		t.Fatalf("exists: %v", err)
	}

	for _, bad := range []struct{ off, n int64 }{{0, 0}, {-1, 1}, {0, -1}} {
		if _, err := c.HostWrite(ctx, writeRequest(17), f, bad.off, bad.n, racersdk.WriteOptions{}); !errors.Is(err, racersdk.ErrInvalidRequest) {
			t.Errorf("%+v: %v", bad, err)
		}
	}

	if _, err := c.HostWrite(ctx, writeRequest(17), nil, 0, 1, racersdk.WriteOptions{}); !errors.Is(err, racersdk.ErrInvalidRequest) {
		t.Errorf("nil file: %v", err)
	}
}

func TestHostWriteRetryBusy(t *testing.T) {
	c, writes := racersdktest.NewClientWithWrites(t, nil)

	var attempts atomic.Int32

	writes.Reject(func(racersdktest.WriteCall) *racersdktest.WriteRejection {
		if attempts.Add(1) < 3 {
			return &racersdktest.WriteRejection{Status: 503, Code: "dirty-cap"}
		}

		return nil
	})

	f := memfd(t, []byte("abc"))
	if _, err := c.HostWrite(context.Background(), writeRequest(18), f, 0, 3, racersdk.WriteOptions{RetryBusy: 30 * time.Second}); err != nil {
		t.Fatal(err)
	}

	if attempts.Load() != 3 {
		t.Fatalf("attempts %d", attempts.Load())
	}
}

func TestWriteCanceled(t *testing.T) {
	c, writes := racersdktest.NewClientWithWrites(t, nil)
	writes.Reject(func(racersdktest.WriteCall) *racersdktest.WriteRejection {
		return &racersdktest.WriteRejection{Status: 503, RetryAfter: 5 * time.Second}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := c.Put(ctx, writeRequest(19), strings.NewReader("x"), 1, racersdk.WriteOptions{RetryBusy: time.Minute})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// TestHostReadUnsupported pins that the fake does not serve host-read, so a
// test cannot come to rely on host-read semantics the fake never checks.
func TestHostReadUnsupported(t *testing.T) {
	c, _ := racersdktest.NewClientWithWrites(t, nil)
	ctx := context.Background()
	r := writeRequest(42)

	if _, err := c.Put(ctx, r, strings.NewReader("data"), 4, racersdk.WriteOptions{Upload: racersdk.UploadNever}); err != nil {
		t.Fatal(err)
	}

	file, err := os.CreateTemp(t.TempDir(), "dest")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	if err := file.Truncate(4); err != nil {
		t.Fatal(err)
	}

	_, err = c.HostRead(ctx, r, file, racersdk.HostReadOptions{})
	if err == nil {
		t.Fatal("HostRead succeeded against the fake")
	}

	if got, _ := os.ReadFile(file.Name()); string(got) == "data" {
		t.Fatal("HostRead wrote the object through the fake")
	}
}
