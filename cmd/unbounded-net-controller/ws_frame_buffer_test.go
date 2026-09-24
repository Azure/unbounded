// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestNodeWSBufferSizes(t *testing.T) {
	for _, size := range []int{0, 1, nodeWSMinBufferSize - 1, nodeWSMinBufferSize, nodeWSMinBufferSize + 1, 512 * 1024, maxNodeWSFrameBytes, maxNodeWSFrameBytes + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			pool := newNodeWSBufferPool(2)
			payload := bytes.Repeat([]byte{0x5a}, size)

			data, err := pool.read(bytes.NewReader(payload))
			defer pool.put(data)

			if size > maxNodeWSFrameBytes {
				if !errors.Is(err, websocket.ErrMessageTooBig) || data != nil {
					t.Fatalf("oversized frame returned %d bytes, %v", len(data), err)
				}

				return
			}

			if err != nil || !bytes.Equal(data, payload) {
				t.Fatalf("frame size %d: got %d bytes, %v", size, len(data), err)
			}
		})
	}
}

func TestNodeWSBufferOwnershipAndBound(t *testing.T) {
	pool := newNodeWSBufferPool(2)

	first, err := pool.read(bytes.NewReader([]byte("first")))
	if err != nil {
		t.Fatal(err)
	}

	second, err := pool.read(bytes.NewReader([]byte("second")))
	if err != nil {
		t.Fatal(err)
	}

	pool.put(second)

	reused := pool.get(nodeWSMinBufferSize)
	copy(reused[:cap(reused)], []byte("overwrite"))

	if string(first) != "first" {
		t.Fatal("reusing another frame mutated an in-flight frame")
	}

	pool.put(first)
	pool.put(reused)

	retained := 0

	for i, buffers := range pool.buffers {
		size := nodeWSMinBufferSize << i
		for range 4 {
			pool.put(make([]byte, size))
		}

		if len(buffers) != 2 {
			t.Fatalf("class %d retained %d buffers, want 2", i, len(buffers))
		}

		retained += len(buffers) * size
	}

	if retained != 2*(2*maxNodeWSFrameBytes-nodeWSMinBufferSize) {
		t.Fatalf("unexpected retention: %d bytes", retained)
	}
}

type failingWSReader struct {
	err error
}

func (r failingWSReader) Read([]byte) (int, error) {
	return 0, r.err
}

func TestNodeWSBufferReadErrors(t *testing.T) {
	readFailure := errors.New("read failure")
	for _, tc := range []struct {
		name   string
		reader io.Reader
		want   error
	}{
		{"failure", failingWSReader{readFailure}, readFailure},
		{"cancellation", failingWSReader{context.Canceled}, context.Canceled},
		{"wrapped-eof", failingWSReader{fmt.Errorf("closed mid-frame: %w", io.EOF)}, io.EOF},
		{"no-progress", failingWSReader{}, io.ErrNoProgress},
		{"failure-after-growth", io.MultiReader(bytes.NewReader(make([]byte, 2*nodeWSMinBufferSize)), failingWSReader{readFailure}), readFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := newNodeWSBufferPool(2)

			data, err := pool.read(tc.reader)
			if data != nil || !errors.Is(err, tc.want) {
				t.Fatalf("read returned %d bytes, %v", len(data), err)
			}

			if len(pool.buffers[0]) == 0 {
				t.Fatal("failed read did not release its buffer")
			}
		})
	}
}

func TestNodeWSBufferAllocations(t *testing.T) {
	pool := newNodeWSBufferPool(2)
	reader := bytes.NewReader(make([]byte, 512*1024))

	allocs := testing.AllocsPerRun(100, func() {
		if _, err := reader.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}

		data, err := pool.read(reader)
		if err != nil {
			t.Fatal(err)
		}

		pool.put(data)
	})
	if allocs > 1 {
		t.Fatalf("warmed frame reader allocated %.1f objects; want at most 1", allocs)
	}
}

func TestNodeWSFrameTransport(t *testing.T) {
	for _, compression := range []websocket.CompressionMode{websocket.CompressionDisabled, websocket.CompressionContextTakeover} {
		for _, size := range []int{0, 32769, maxNodeWSFrameBytes, maxNodeWSFrameBytes + 1} {
			t.Run(fmt.Sprintf("compression-%d/bytes-%d", compression, size), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()

				pool := newNodeWSBufferPool(2)
				result := make(chan error, 1)
				payload := bytes.Repeat([]byte("x"), size)

				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: compression})
					if err != nil {
						result <- err
						return
					}

					defer func() {
						if err := conn.CloseNow(); err != nil && !errors.Is(err, net.ErrClosed) {
							t.Errorf("server close: %v", err)
						}
					}()

					conn.SetReadLimit(maxNodeWSFrameBytes)

					frame, err := pool.readFrame(ctx, conn)
					defer pool.put(frame.data)

					if err == nil && (frame.msgType != websocket.MessageBinary || !bytes.Equal(frame.data, payload)) {
						err = errors.New("frame type or payload changed")
					}

					result <- err
				}))
				defer server.Close()

				conn, _, err := websocket.Dial(ctx, server.URL, &websocket.DialOptions{CompressionMode: compression})
				if err != nil {
					t.Fatal(err)
				}

				defer func() {
					if err := conn.CloseNow(); err != nil && !errors.Is(err, net.ErrClosed) {
						t.Errorf("client close: %v", err)
					}
				}()

				if err := conn.Write(ctx, websocket.MessageBinary, payload); err != nil && size <= maxNodeWSFrameBytes {
					t.Fatal(err)
				}

				if size > maxNodeWSFrameBytes {
					_, _, err := conn.Read(ctx)
					if websocket.CloseStatus(err) != websocket.StatusMessageTooBig {
						t.Fatalf("oversize close: %v", err)
					}
				}

				select {
				case err := <-result:
					if size > maxNodeWSFrameBytes {
						if !errors.Is(err, websocket.ErrMessageTooBig) {
							t.Fatalf("expected size rejection, got %v", err)
						}
					} else if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			})
		}
	}
}

func TestNodeWSFrameCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	pool := newNodeWSBufferPool(2)
	started := make(chan struct{})
	result := make(chan error, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			result <- err
			return
		}

		defer func() {
			if err := conn.CloseNow(); err != nil && !errors.Is(err, net.ErrClosed) {
				t.Errorf("server close: %v", err)
			}
		}()

		close(started)

		frame, err := pool.readFrame(ctx, conn)
		pool.put(frame.data)

		result <- err
	}))
	defer server.Close()

	conn, _, err := websocket.Dial(t.Context(), server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}

	defer func() {
		if err := conn.CloseNow(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("client close: %v", err)
		}
	}()

	<-started
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled reader did not return")
	}
}

func BenchmarkNodeWSFrameBuffer(b *testing.B) {
	for _, size := range []int{8 * 1024, 512 * 1024, maxNodeWSFrameBytes} {
		for _, pooled := range []bool{false, true} {
			b.Run(fmt.Sprintf("bytes-%d/pooled-%t", size, pooled), func(b *testing.B) {
				pool := newNodeWSBufferPool(2)

				reader := bytes.NewReader(make([]byte, size))
				if pooled {
					data, err := pool.read(reader)
					if err != nil {
						b.Fatal(err)
					}

					pool.put(data)
				}

				b.ReportAllocs()
				b.SetBytes(int64(size))

				for b.Loop() {
					if _, err := reader.Seek(0, io.SeekStart); err != nil {
						b.Fatal(err)
					}

					var (
						data []byte
						err  error
					)
					if pooled {
						data, err = pool.read(reader)
					} else {
						data, err = io.ReadAll(reader)
					}

					if err != nil || len(data) != size {
						b.Fatalf("read returned %d bytes, %v", len(data), err)
					}

					if pooled {
						pool.put(data)
					}
				}
			})
		}
	}
}
