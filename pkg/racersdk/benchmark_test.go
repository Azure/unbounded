// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// benchmarkPeer generates bytes with fixed scratch. Both clients receive the same
// bootstrap/continuation framing from this peer; no page-sized fixture is resident.
func benchmarkPeer(b *testing.B, size int64, origin bool) string {
	b.Helper()
	path := socketDir(b) + "/socket"

	listener, err := net.Listen("unix", path)
	if err != nil {
		b.Fatal(err)
	}

	server := &http.Server{ReadHeaderTimeout: time.Second}

	if origin {
		config, err := (OriginConfig{Cache: CacheName{value: "bench"}}).defaults()
		if err != nil {
			b.Fatal(err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		b.Cleanup(cancel)

		listener = &originListener{Listener: listener, ctx: ctx, config: config, slots: make(chan struct{}, 128)}
		server.BaseContext = func(net.Listener) context.Context { return ctx }
		server.ConnContext = func(ctx context.Context, conn net.Conn) context.Context {
			return context.WithValue(ctx, originConnKey{}, conn)
		}
		slots := make(chan struct{}, 64)
		callback := func(context.Context, OriginRequest) (Metadata, io.ReadCloser, error) {
			return originMeta(ByteLength(size)), io.NopCloser(io.LimitReader(repeatedByte('x'), size)), nil
		}
		server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serveOperation(w, r, config, callback, slots) })
	} else {
		server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			first, length := int64(0), min(size, int64(PageSize))
			if r.Header.Get("If-Match") != "" {
				first, length = int64(PageSize), size-int64(PageSize)
			}

			streamResponse(w, first, length, size, `"v"`)
		})
	}

	finished := make(chan struct{})

	go func() { defer close(finished); _ = server.Serve(listener) }()

	b.Cleanup(func() { _ = server.Close(); <-finished })

	return path
}

type benchmarkReader struct {
	client *Client
	plain  *http.Transport
	buffer []byte
	copyIO bool
	size   int64
}

func newBenchmarkReader(b *testing.B, path, implementation, mode string, size int64, bufferSize int) *benchmarkReader {
	b.Helper()

	r := &benchmarkReader{size: size, copyIO: mode == "Copy", buffer: make([]byte, bufferSize)}

	if implementation == "sdk" {
		var err error

		r.client, err = newClient(ClientConfig{Cache: CacheName{value: "bench"}}, path)
		if err != nil {
			b.Fatal(err)
		}

		b.Cleanup(func() { closeBody(r.client) })
	} else {
		r.plain = unixTransport(path)
		b.Cleanup(r.plain.CloseIdleConnections)
	}

	return r
}

func (r *benchmarkReader) copy(dst io.Writer, body io.Reader) (int64, error) {
	if r.copyIO {
		return io.Copy(struct{ io.Writer }{dst}, body)
	}

	return io.CopyBuffer(struct{ io.Writer }{dst}, struct{ io.Reader }{body}, r.buffer)
}

func (r *benchmarkReader) read(dst io.Writer, fresh bool) error {
	if r.client != nil {
		if fresh {
			r.client.transport.CloseIdleConnections()
		}

		value, err := r.client.Get(context.Background(), Request{})
		if err != nil {
			return err
		}
		defer closeBody(value)

		n, err := r.copy(dst, value)
		if err == nil && n != r.size {
			return fmt.Errorf("size %d, want %d", n, r.size)
		}

		return err
	}

	if fresh {
		r.plain.CloseIdleConnections()
	}

	var total int64

	for first := int64(0); ; first = int64(PageSize) {
		request, err := http.NewRequest("GET", "http://racer"+objectPrefix+(Key{}).String(), nil)
		if err != nil {
			return err
		}

		request.Header.Set("Range", "bytes=0-16777215")

		if first != 0 {
			request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", first, r.size-1))
			request.Header.Set("If-Match", `"v"`)
		}

		response, err := r.plain.RoundTrip(request)
		if err != nil {
			return err
		}

		n, err := r.copy(dst, response.Body)
		closeBody(response.Body)

		if err != nil {
			return err
		}

		total += n
		if total == r.size {
			return nil
		}

		if first != 0 || total != int64(PageSize) {
			return fmt.Errorf("size %d, want %d", total, r.size)
		}
	}
}

func BenchmarkClientStream(b *testing.B) {
	for _, size := range []int64{0, 4096, int64(PageSize), 1 << 30} {
		b.Run(strconv.FormatInt(size, 10), func(b *testing.B) {
			path := benchmarkPeer(b, size, false)

			for _, implementation := range []string{"stdlib", "sdk"} {
				for _, fresh := range []bool{false, true} {
					for _, mode := range []struct {
						name  string
						bytes int
					}{{"Read4K", 4096}, {"Read32K", 32768}, {"Read256K", 262144}, {"Copy", 0}} {
						b.Run(fmt.Sprintf("%s/fresh=%t/%s", implementation, fresh, mode.name), func(b *testing.B) {
							r := newBenchmarkReader(b, path, implementation, mode.name, size, mode.bytes)
							if err := r.read(io.Discard, false); err != nil {
								b.Fatal(err)
							}

							b.ReportAllocs()
							b.SetBytes(size)
							b.ResetTimer()

							for range b.N {
								if err := r.read(io.Discard, fresh); err != nil {
									b.Fatal(err)
								}
							}
						})
					}
				}
			}
		})
	}
}

func BenchmarkOriginStream(b *testing.B) {
	for _, size := range []int64{0, 4096, int64(PageSize)} {
		for _, origin := range []bool{false, true} {
			b.Run(fmt.Sprintf("%d/sdk=%t", size, origin), func(b *testing.B) {
				path := benchmarkPeer(b, size, origin)

				r := newBenchmarkReader(b, path, "stdlib", "Read32K", size, 32768)
				if err := r.read(io.Discard, false); err != nil {
					b.Fatal(err)
				}

				b.ReportAllocs()
				b.SetBytes(size)
				b.ResetTimer()

				for range b.N {
					if err := r.read(io.Discard, false); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func BenchmarkConcurrentStream(b *testing.B) {
	for _, implementation := range []string{"stdlib", "sdk"} {
		for _, concurrency := range []int{1, 16} {
			b.Run(fmt.Sprintf("%s/concurrency=%d", implementation, concurrency), func(b *testing.B) {
				const size = int64(PageSize)

				path := benchmarkPeer(b, size, false)

				readers := make([]*benchmarkReader, concurrency)
				for i := range readers {
					readers[i] = newBenchmarkReader(b, path, implementation, "Read32K", size, 32768)
					if i > 0 {
						readers[i].client, readers[i].plain = readers[0].client, readers[0].plain
					}

					if err := readers[i].read(io.Discard, false); err != nil {
						b.Fatal(err)
					}
				}

				var (
					next    atomic.Int64
					workers sync.WaitGroup
				)

				b.ReportAllocs()
				b.SetBytes(size)
				b.ResetTimer()

				for _, reader := range readers {
					workers.Go(func() {
						for next.Add(1) <= int64(b.N) {
							if err := reader.read(io.Discard, false); err != nil {
								b.Error(err)
								return
							}
						}
					})
				}

				workers.Wait()
			})
		}
	}
}

// Live-heap sampling is separate from throughput: forced GC and runtime sampling
// intentionally perturb timing. Report absolute process heap and the warmed baseline
// separately: collection of old server work can make a baseline delta negative.
type heapSink struct {
	bytes, next int64
	base, peak  uint64
	first, last uint64
	slow        bool
	nextPause   int64
}

func (w *heapSink) Write(p []byte) (int, error) {
	w.bytes += int64(len(p))
	if w.slow && w.bytes >= w.nextPause {
		w.nextPause = w.bytes + 1<<20

		time.Sleep(time.Millisecond)
	}

	if w.bytes >= w.next {
		w.next = w.bytes + 64<<20

		runtime.GC()

		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)

		w.peak = max(w.peak, stats.HeapAlloc)

		w.last = stats.HeapAlloc
		if w.first == 0 {
			w.first = w.last
		}
	}

	return len(p), nil
}

func BenchmarkStreamingLiveHeap(b *testing.B) {
	for _, implementation := range []string{"stdlib", "sdk"} {
		for _, slow := range []bool{false, true} {
			b.Run(fmt.Sprintf("%s/slow=%t", implementation, slow), func(b *testing.B) {
				const size = 1 << 30

				path := benchmarkPeer(b, size, false)

				r := newBenchmarkReader(b, path, implementation, "Read32K", size, 32768)
				if err := r.read(io.Discard, false); err != nil {
					b.Fatal(err)
				}

				runtime.GC()

				var stats runtime.MemStats
				runtime.ReadMemStats(&stats)
				sink := &heapSink{base: stats.HeapAlloc, slow: slow}

				b.ResetTimer()

				for range b.N {
					if err := r.read(sink, false); err != nil {
						b.Fatal(err)
					}
				}

				b.ReportMetric(float64(sink.peak), "peak-live-B")
				b.ReportMetric(float64(sink.base), "baseline-live-B")
				b.ReportMetric(float64(sink.first), "first-live-B")
				b.ReportMetric(float64(sink.last), "last-live-B")
			})
		}
	}
}
