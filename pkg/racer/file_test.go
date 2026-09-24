// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func destinationFile(t *testing.T) *os.File {
	t.Helper()

	f, err := os.Create(filepath.Join(t.TempDir(), "download"))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = f.Close() })

	return f
}

func fileObject(t *testing.T, data []byte) *Object {
	t.Helper()

	origin, err := NewOrigin(&memoryStore{data: data, meta: Metadata{Size: int64(len(data)), ETag: checksumTag(data)}})
	if err != nil {
		t.Fatal(err)
	}

	c := newTestClient(t, origin, ClientOptions{Concurrency: 3, MaxActiveRequests: 2})

	o, err := c.Open(t.Context(), "/file")
	if err != nil {
		t.Fatal(err)
	}

	return o
}

func TestFileDownloadOffsetsAndBounds(t *testing.T) {
	data := payload(2 << 20)
	o := fileObject(t, data)
	f := destinationFile(t)

	prefix := bytes.Repeat([]byte{0xa5}, 97)
	if _, err := f.Write(prefix); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		n, err := o.DownloadFile(t.Context(), f, 97)
		if err != nil || n != int64(len(data)) {
			t.Fatal(n, err)
		}

		cursor, err := f.Seek(0, io.SeekCurrent)
		if err != nil || cursor != 97 {
			t.Fatal("cursor moved", cursor, err)
		}

		got := make([]byte, len(data)+97)
		if _, err := f.ReadAt(got, 0); err != nil || !bytes.Equal(got[:97], prefix) || !bytes.Equal(got[97:], data) {
			t.Fatal("wrong bytes", err)
		}

		assertAdmissionFree(t, o.client)
	}

	for _, off := range []int64{-1, math.MaxInt64} {
		if n, err := o.DownloadFile(t.Context(), f, off); n != 0 || err == nil {
			t.Fatal(n, err)
		}
	}

	if _, err := o.DownloadFile(t.Context(), nil, 0); err == nil {
		t.Fatal("nil accepted")
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if n, err := o.DownloadFile(ctx, f, 0); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatal(n, err)
	}

	empty := fileObject(t, nil)
	if n, err := empty.DownloadFile(t.Context(), f, 0); n != 0 || err != nil {
		t.Fatal(n, err)
	}
}

func TestFileDownloadParallelPages(t *testing.T) {
	const size = 2*PageSize + 123

	var active, peak atomic.Int32

	started := make(chan struct{}, 2)
	release := make(chan struct{})
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", checksumTag(nil))

		w.Header()["Content-Type"] = nil
		if r.Method == "HEAD" {
			w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
			return
		}

		now := active.Add(1)
		defer active.Add(-1)

		for old := peak.Load(); now > old && !peak.CompareAndSwap(old, now); old = peak.Load() {
		}

		select {
		case started <- struct{}{}:
		default:
		}

		select {
		case <-release:
		case <-r.Context().Done():
			return
		}

		var start, end int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			t.Error(err)
			return
		}

		if start%PageSize != 0 || end != min(start+PageSize, size)-1 {
			t.Error("unaligned range", start, end)
		}

		w.Header().Set("Content-Range", contentRange(start, end, size))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(206)

		buf := bytes.Repeat([]byte{byte(start/PageSize + 1)}, 32<<10)
		for left := end - start + 1; left > 0; {
			n, err := w.Write(buf[:min(left, int64(len(buf)))])
			left -= int64(n)

			if err != nil {
				return
			}
		}
	}), ClientOptions{Concurrency: 3, MaxActiveRequests: 2})

	o, err := c.Open(t.Context(), "/parallel")
	if err != nil {
		t.Fatal(err)
	}

	f := destinationFile(t)
	done := make(chan error, 1)

	go func() {
		n, err := o.DownloadFile(t.Context(), f, 19)
		if err == nil && n != size {
			err = fmt.Errorf("count %d", n)
		}

		done <- err
	}()

	for range 2 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("pages not parallel")
		}
	}

	close(release)

	if err := <-done; err != nil {
		t.Fatal(err)
	}

	if peak.Load() != 2 {
		t.Fatal("admission peak", peak.Load())
	}

	buf := make([]byte, 32<<10)
	for off := int64(0); off < size; {
		length := min(int64(len(buf)), size-off, PageSize-off%PageSize)
		if _, err := f.ReadAt(buf[:length], 19+off); err != nil {
			t.Fatal(err)
		}

		if !bytes.Equal(buf[:length], bytes.Repeat([]byte{byte(off/PageSize + 1)}, int(length))) {
			t.Fatal("page corrupt", off)
		}

		off += length
	}

	if pos, _ := f.Seek(0, io.SeekCurrent); pos != 0 {
		t.Fatal("cursor moved", pos)
	}

	assertAdmissionFree(t, c)
}

func TestFileDownloadShortAndCancel(t *testing.T) {
	for _, mode := range []string{"short", "cancel", "version"} {
		t.Run(mode, func(t *testing.T) {
			started := make(chan struct{})
			c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", checksumTag(nil))
				w.Header().Set("Content-Length", "100000")
				w.Header()["Content-Type"] = nil

				if r.Method == "HEAD" {
					return
				}

				if mode == "version" {
					w.Header().Set("ETag", checksumTag([]byte("changed")))
				}

				w.Header().Set("Content-Range", "bytes 0-99999/100000")
				w.WriteHeader(206)
				_, _ = w.Write([]byte("short"))
				_ = http.NewResponseController(w).Flush()

				close(started)

				if mode == "cancel" {
					<-r.Context().Done()
				}
			}), ClientOptions{MaxActiveRequests: 1})

			o, err := c.Open(t.Context(), "/file")
			if err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			if mode == "cancel" {
				go func() { <-started; cancel() }()
			}

			f := destinationFile(t)
			n, err := o.DownloadFile(ctx, f, 7)

			want := io.ErrUnexpectedEOF
			if mode == "cancel" {
				want = context.Canceled
			}

			if mode == "version" {
				want = ErrVersionChanged
			}

			if !errors.Is(err, want) {
				t.Fatal(n, err)
			}

			if mode == "short" {
				got := make([]byte, 5)

				_, readErr := f.ReadAt(got, 7)
				if n != 5 || readErr != nil || string(got) != "short" {
					t.Fatal(n, string(got), readErr)
				}
			}

			assertAdmissionFree(t, c)
		})
	}
}

type shortFileWriter struct{}

func (shortFileWriter) WriteAt(p []byte, _ int64) (int, error) { return len(p) / 2, nil }

func TestFilePortableShortWriteAndDestinationErrors(t *testing.T) {
	o := fileObject(t, payload(100))

	s, _ := o.Stream(t.Context())
	defer s.Close()

	n, err := s.copyToFile(shortFileWriter{}, 0)
	if n != 50 || !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(n, err)
	}

	f := destinationFile(t)

	readOnly, err := os.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()

	if n, err := o.DownloadFile(t.Context(), readOnly, 0); n != 0 || err == nil {
		t.Fatal(n, err)
	}

	appendFile, err := os.OpenFile(f.Name(), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer appendFile.Close()

	if n, err := o.DownloadFile(t.Context(), appendFile, 0); n != 0 || err == nil {
		t.Fatal(n, err)
	}

	_ = f.Close()
	if n, err := o.DownloadFile(t.Context(), f, 0); n != 0 || err == nil {
		t.Fatal(n, err)
	}
}

func TestFilePortableBytesAndAdmissionCancellation(t *testing.T) {
	data := payload(100000)
	o := fileObject(t, data)
	f := destinationFile(t)
	s, _ := o.ReadRange(t.Context(), 13, int64(len(data))-13)
	n, err := s.copyToFile(f, 29)
	_ = s.Close()
	got := make([]byte, len(data)-13)

	_, readErr := f.ReadAt(got, 29)
	if err != nil || readErr != nil || n != int64(len(got)) || !bytes.Equal(got, data[13:]) || s.Stats().SpliceCalls != 0 {
		t.Fatal(n, err, readErr, s.Stats())
	}

	if pos, _ := f.Seek(0, io.SeekCurrent); pos != 0 {
		t.Fatal("portable copy moved cursor", pos)
	}
	// Hold both shared admission permits; a file download must honor its deadline
	// while queued, without starting a GET or writing output.
	a, err := o.client.admission.acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer a.release()

	b, err := o.client.admission.acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer b.release()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	if n, err := o.DownloadFile(ctx, f, 0); n != 0 || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("file download bypassed shared admission", n, err)
	}
}
