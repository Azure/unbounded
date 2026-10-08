// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Azure/unbounded/internal/gantry/metrics"
)

type racerPausedReader struct {
	resume <-chan struct{}
	reader io.Reader
}

func (r racerPausedReader) Read(p []byte) (int, error) {
	<-r.resume
	return r.reader.Read(p)
}

func TestRacerIOHTTP2UpstreamWaits(t *testing.T) {
	for _, phase := range []string{"WriteHeader", "early Flush", "Write", "ReadFrom fallback"} {
		t.Run(phase, func(t *testing.T) {
			const budget = 50 * time.Millisecond

			server := httptest.NewUnstartedServer(WrapHTTP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Length", "6")
				w.WriteHeader(http.StatusOK)

				if phase == "WriteHeader" {
					time.Sleep(4 * budget)
				}

				if err := http.NewResponseController(w).Flush(); err != nil {
					t.Error(err)
				}

				if phase == "early Flush" {
					time.Sleep(4 * budget)
				}

				if _, err := w.Write([]byte("abc")); err != nil {
					t.Error(err)
				}

				if phase == "Write" {
					time.Sleep(4 * budget)
				}

				if phase == "ReadFrom fallback" {
					// Simulate the SDK deadline surrounding a ReadFrom call whose
					// HTTP/2 fallback waits on source bytes before invoking Write.
					if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(3 * time.Second)); err != nil {
						t.Error(err)
					}

					resume := make(chan struct{})

					timer := time.AfterFunc(4*budget, func() { close(resume) })
					defer timer.Stop()

					if _, err := w.(io.ReaderFrom).ReadFrom(racerPausedReader{resume, bytes.NewReader([]byte("def"))}); err != nil {
						t.Error(err)
					}

					if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil {
						t.Error(err)
					}
				} else if _, err := w.Write([]byte("def")); err != nil {
					t.Error(err)
				}
			}), budget, nil))
			server.EnableHTTP2 = true

			server.StartTLS()
			defer server.Close()

			server.Client().Timeout = 5 * time.Second
			for range 2 {
				resp, err := server.Client().Get(server.URL)
				if err != nil {
					t.Fatal(err)
				}

				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()

				if resp.ProtoMajor != 2 || string(body) != "abcdef" || err != nil {
					t.Fatalf("proto=%s body=%q err=%v", resp.Proto, body, err)
				}
				// The finalization deadline must not poison a completed stream or
				// the next stream on this connection after the handler returns.
				time.Sleep(2 * budget)
			}
		})
	}
}

type racerSocketResponse struct {
	http.ResponseWriter
	bytes *atomic.Int64
}

func (w racerSocketResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type racerSocketFallback struct{ http.ResponseWriter }

func (w racerSocketFallback) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w racerSocketResponse) ReadFrom(r io.Reader) (int64, error) {
	source, ok := r.(*io.LimitedReader)
	if !ok || source.N <= 0 || source.N > racerSocketChunk {
		return 0, errors.New("socket transfer is not bounded")
	}

	if _, unix := source.R.(*net.UnixConn); !unix {
		return 0, errors.New("socket hidden by wrapper or nested limiter")
	}

	fast, ok := w.ResponseWriter.(io.ReaderFrom)
	if !ok {
		return 0, errors.New("net/http ReaderFrom unavailable")
	}

	n, err := fast.ReadFrom(source)
	w.bytes.Add(n)

	return n, err
}

func TestRacerSocketHTTPReuse(t *testing.T) {
	for _, mode := range []string{"plaintext", "TLS", "fallback"} {
		t.Run(mode, func(t *testing.T) { testRacerSocketHTTPReuse(t, mode) })
	}
}

// transportSocketSource keeps the raw Unix connection visible to ReaderFrom.
func transportSocketSource(listener net.Listener, data []byte, budget time.Duration) (net.Conn, <-chan error, error) {
	source, err := net.DialTimeout("unix", listener.Addr().String(), budget)
	if err != nil {
		return nil, nil, err
	}

	peer, err := listener.Accept()
	if err != nil {
		_ = source.Close()
		return nil, nil, err
	}

	deadline := time.Now().Add(budget)
	if err := errors.Join(source.SetReadDeadline(deadline), peer.SetWriteDeadline(deadline)); err != nil {
		_ = source.Close()
		_ = peer.Close()

		return nil, nil, err
	}

	done := make(chan error, 1)

	go func() {
		defer peer.Close()

		_, err := peer.Write(data)
		done <- err
	}()

	return source, done, nil
}

func testRacerSocketHTTPReuse(t *testing.T, mode string) {
	t.Helper()
	// Exercise the bounded raw source shape supplied by Object.WriteTo without
	// modifying canonical /run paths.
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(t.TempDir(), "s"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { listener.Close() })

	data := bytes.Repeat([]byte("0123456789abcdef"), racerSocketChunk/4+1)

	var fastBytes, connections atomic.Int64

	observed := make(chan HTTPObservation, 1)
	handler := WrapHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offset, err := strconv.Atoi(r.URL.Query().Get("offset"))
		if err != nil {
			t.Error(err)
			return
		}

		conn, done, err := transportSocketSource(listener, data[offset:], 5*time.Second)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()

		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)-offset))

		if offset != 0 {
			w.WriteHeader(http.StatusPartialContent)
		}

		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Error(err)
		}

		n, err := w.(io.ReaderFrom).ReadFrom(&io.LimitedReader{R: conn, N: int64(len(data) - offset)})
		if err != nil || n != int64(len(data)-offset) {
			t.Errorf("socket transfer: bytes=%d err=%v", n, err)
		}

		if err := <-done; err != nil {
			t.Error(err)
		}
	}), time.Second, func(o HTTPObservation) { observed <- o })
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w = racerSocketResponse{w, &fastBytes}
		if mode == "fallback" {
			w = racerSocketFallback{w}
		}

		handler.ServeHTTP(w, r)
	}))

	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	if mode == "TLS" {
		server.StartTLS()
	} else {
		server.Start()
	}

	t.Cleanup(server.Close)
	server.Client().Timeout = 10 * time.Second

	var total int64

	for _, offset := range []int{0, 17, len(data) - 1} {
		resp, err := server.Client().Get(server.URL + "?offset=" + strconv.Itoa(offset))
		if err != nil {
			t.Fatal(err)
		}

		got, err := io.ReadAll(resp.Body)
		resp.Body.Close()

		status := http.StatusOK
		if offset != 0 {
			status = http.StatusPartialContent
		}

		if err != nil || !bytes.Equal(got, data[offset:]) || resp.StatusCode != status || resp.ProtoMajor != 1 || resp.Close {
			t.Fatalf("response: status=%d bytes=%d err=%v", resp.StatusCode, len(got), err)
		}

		select {
		case o := <-observed:
			if o.Aborted || o.Bytes != int64(len(got)) || o.Status != status {
				t.Fatalf("observation=%+v", o)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("missing observation")
		}

		total += int64(len(got))
	}

	if mode == "fallback" {
		total = 0
	}

	if fastBytes.Load() != total || connections.Load() != 1 {
		t.Fatalf("bounded fast bytes=%d want=%d connections=%d", fastBytes.Load(), total, connections.Load())
	}
}

type racerObservedListener struct {
	net.Listener
	bytes atomic.Int64
}

type racerAcceptResult struct {
	conn net.Conn
	err  error
}

type racerScriptListener struct {
	results chan racerAcceptResult
	done    chan struct{}
	once    sync.Once
}

func (l *racerScriptListener) Accept() (net.Conn, error) {
	select {
	case result := <-l.results:
		return result.conn, result.err
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *racerScriptListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *racerScriptListener) Addr() net.Addr { return &net.TCPAddr{} }

func TestRacerLimitedListenerLifecycle(t *testing.T) {
	t.Run("default limit", func(t *testing.T) {
		base := &racerScriptListener{done: make(chan struct{})}

		listener := LimitListener(base, 0).(*racerLimitedListener)
		defer listener.Close()

		if cap(listener.slots) != 512 {
			t.Fatalf("default connection limit=%d", cap(listener.slots))
		}
	})

	base := &racerScriptListener{results: make(chan racerAcceptResult, 1), done: make(chan struct{})}
	l := LimitListener(base, 1).(*racerLimitedListener)

	t.Cleanup(func() { _ = l.Close() })

	sentinel := errors.New("accept failed")
	base.results <- racerAcceptResult{err: sentinel}

	if _, err := l.Accept(); !errors.Is(err, sentinel) || len(l.slots) != 0 {
		t.Fatalf("accept failure leaked slot: %v", err)
	}

	left, right := net.Pipe()

	t.Cleanup(func() { _ = right.Close() })

	base.results <- racerAcceptResult{conn: left}

	c, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = c.Close() })

	if _, ok := c.(io.ReaderFrom); ok {
		t.Fatal("invented ReaderFrom for unsupported connection")
	}

	if err := c.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}

	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("read deadline not forwarded")
	}

	if err := c.SetWriteDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}

	if _, err := c.Write([]byte{1}); err == nil {
		t.Fatal("write deadline not forwarded")
	}

	accepted := make(chan racerAcceptResult, 1)

	base.results <- racerAcceptResult{err: sentinel}

	go func() { conn, acceptErr := l.Accept(); accepted <- racerAcceptResult{conn, acceptErr} }()

	select {
	case <-accepted:
		t.Fatal("connection limit bypassed")
	case <-time.After(20 * time.Millisecond):
	}

	var closes sync.WaitGroup
	for range 2 {
		closes.Go(func() { _ = c.Close() })
	}

	closes.Wait()

	select {
	case result := <-accepted:
		if !errors.Is(result.err, sentinel) {
			t.Fatal(result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("close did not release slot")
	}

	if len(l.slots) != 0 {
		t.Fatal("slot leaked")
	}
	// Closing the listener must unblock Accept both at the semaphore and inside
	// the underlying listener, without waiting for existing connections to close.
	for _, full := range []bool{false, true} {
		t.Run(strconv.FormatBool(full), func(t *testing.T) {
			b := &racerScriptListener{results: make(chan racerAcceptResult), done: make(chan struct{})}

			limited := LimitListener(b, 1).(*racerLimitedListener)
			if full {
				limited.slots <- struct{}{}
			}

			done := make(chan error, 1)

			go func() { _, acceptErr := limited.Accept(); done <- acceptErr }()

			_ = limited.Close()

			select {
			case acceptErr := <-done:
				if !errors.Is(acceptErr, net.ErrClosed) {
					t.Fatal(acceptErr)
				}
			case <-time.After(time.Second):
				t.Fatal("listener close did not unblock accept")
			}
		})
	}
}

func (l *racerObservedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}

	return &racerObservedConn{TCPConn: c.(*net.TCPConn), bytes: &l.bytes}, nil
}

type racerObservedConn struct {
	*net.TCPConn
	bytes *atomic.Int64
}

func (c *racerObservedConn) ReadFrom(r io.Reader) (int64, error) {
	n, err := c.TCPConn.ReadFrom(r)
	c.bytes.Add(n)

	return n, err
}

func TestRacerLimitedListenerHTTPReaderFrom(t *testing.T) {
	unix, err := net.Listen("unix", filepath.Join(t.TempDir(), "source.sock"))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = unix.Close() })

	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	observed := &racerObservedListener{Listener: tcp}
	listener := LimitListener(observed, 1)
	data := bytes.Repeat([]byte("verified Unix to TCP payload"), 32768)
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: WrapHTTP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		source, done, sourceErr := transportSocketSource(unix, data, 3*time.Second)
		if sourceErr != nil {
			t.Error(sourceErr)
			return
		}
		defer source.Close()

		w.Header().Set("Content-Length", strconv.Itoa(len(data)))

		if flushErr := http.NewResponseController(w).Flush(); flushErr != nil {
			t.Error(flushErr)
			return
		}

		n, copyErr := w.(io.ReaderFrom).ReadFrom(&io.LimitedReader{R: source, N: int64(len(data))})
		if copyErr != nil || n != int64(len(data)) {
			t.Errorf("transfer: %d, %v", n, copyErr)
		}

		if writeErr := <-done; writeErr != nil {
			t.Error(writeErr)
		}
	}), time.Second, nil)}
	served := make(chan error, 1)

	go func() { served <- server.Serve(listener) }()

	t.Cleanup(func() {
		_ = server.Close()

		if serveErr := <-served; !errors.Is(serveErr, http.ErrServerClosed) {
			t.Error(serveErr)
		}
	})

	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)

	client := &http.Client{Transport: transport, Timeout: 4 * time.Second}
	for range 2 {
		response, getErr := client.Get("http://" + tcp.Addr().String())
		if getErr != nil {
			t.Fatal(getErr)
		}

		got, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()

		if readErr != nil || !bytes.Equal(got, data) {
			t.Fatalf("integrity: %d bytes, %v", len(got), readErr)
		}
	}

	if got := observed.bytes.Load(); got != 2*int64(len(data)) {
		t.Fatalf("underlying TCP ReaderFrom bytes = %d, want %d", got, 2*len(data))
	}
}

func TestRacerProductionMetrics(t *testing.T) {
	t.Run("HTTP metrics only", func(t *testing.T) {
		reg := metrics.New()
		m := NewMetrics(reg)
		m.MirrorResponse(HTTPObservation{Method: http.MethodPost, Status: 200, Bytes: 3})
		m.OriginRequest(http.MethodPost, 0)

		if got := testutil.ToFloat64(m.requests.WithLabelValues("other", "200", "complete")); got != 1 {
			t.Fatalf("complete responses=%v", got)
		}

		families, err := reg.PrometheusRegistry().Gather()
		if err != nil {
			t.Fatal(err)
		}

		for _, family := range families {
			if strings.HasPrefix(family.GetName(), "gantry_racer_sdk_") {
				t.Fatal("registered removed SDK metrics")
			}
		}
	})

	reg := metrics.New()
	m := NewMetrics(reg)
	m.MirrorResponse(HTTPObservation{Method: http.MethodGet, Status: 206, Bytes: 11, Duration: time.Second, Aborted: true})
	m.OriginRequest(http.MethodHead, 200)
	m.OriginRequest(http.MethodGet, 206)
	m.OriginBytes("blob", 17)

	if got := testutil.ToFloat64(m.requests.WithLabelValues("GET", "206", "aborted")); got != 1 {
		t.Fatalf("aborted responses=%v", got)
	}

	if got := testutil.ToFloat64(m.bytes.WithLabelValues("GET")); got != 11 {
		t.Fatalf("partial bytes=%v", got)
	}

	if got := testutil.ToFloat64(m.originRequests.WithLabelValues("HEAD", "200")); got != 1 {
		t.Fatalf("HEAD requests=%v", got)
	}

	if got := testutil.ToFloat64(m.originRequests.WithLabelValues("GET", "206")); got != 1 {
		t.Fatalf("GET requests=%v", got)
	}

	if got := testutil.ToFloat64(m.originBodyBytes.WithLabelValues("blob")); got != 17 {
		t.Fatalf("origin bytes=%v", got)
	}
}

type racerDeadlineWriter struct {
	*httptest.ResponseRecorder
	deadlines   []time.Time
	sizes       []int
	writeErr    error
	deadlineErr error
}

type racerFastWriter struct {
	*racerDeadlineWriter
	reads int
}

func (w *racerFastWriter) ReadFrom(r io.Reader) (int64, error) {
	w.reads++
	return io.Copy(w.ResponseRecorder, r)
}

func TestRacerIOBoundedReadFrom(t *testing.T) {
	for _, test := range []struct {
		name    string
		missing int64
		wantErr error
	}{
		{"complete", 0, nil},
		{"truncated", 1, io.ErrUnexpectedEOF},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := &racerFastWriter{racerDeadlineWriter: &racerDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}}
			wrapped := &racerResponseWriter{ResponseWriter: w, timeout: time.Second}
			length := 3*racerSocketChunk + 1
			source := &io.LimitedReader{R: bytes.NewReader(make([]byte, length)), N: int64(length) + test.missing}

			n, err := wrapped.ReadFrom(source)
			if n != int64(length) || !errors.Is(err, test.wantErr) || wrapped.failed != (test.wantErr != nil) {
				t.Fatalf("bytes=%d err=%v failed=%v", n, err, wrapped.failed)
			}

			if w.reads != 4 || len(w.deadlines) != 10 || wrapped.bytes != n || source.N != test.missing {
				t.Fatalf("reads=%d deadlines=%d accounted=%d remaining=%d", w.reads, len(w.deadlines), wrapped.bytes, source.N)
			}
		})
	}
}

func (w *racerDeadlineWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadlines = append(w.deadlines, deadline)
	return w.deadlineErr
}

func (w *racerDeadlineWriter) Write(p []byte) (int, error) {
	w.sizes = append(w.sizes, len(p))
	if w.writeErr != nil {
		return 0, w.writeErr
	}

	return w.ResponseRecorder.Write(p)
}

func TestRacerIOBoundedWritesAndFlush(t *testing.T) {
	const batch = 256 * 1024

	w := &racerDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	data := bytes.Repeat([]byte("xyz"), batch+1)[:3*batch+1]

	var observed HTTPObservation

	handler := WrapHTTP(http.HandlerFunc(func(wrapped http.ResponseWriter, _ *http.Request) {
		if !errors.Is(http.NewResponseController(wrapped).SetReadDeadline(time.Now()), http.ErrNotSupported) {
			t.Fatal("unexpected controller behavior")
		}

		if unwrapped := wrapped.(interface{ Unwrap() http.ResponseWriter }).Unwrap(); unwrapped != w {
			t.Fatal("wrong underlying writer")
		}

		wrapped.WriteHeader(http.StatusPartialContent)

		if _, err := io.Copy(wrapped, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}

		wrapped.(http.Flusher).Flush()
	}), 0, func(o HTTPObservation) { observed = o })
	before := time.Now()

	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	if !slices.Equal(w.sizes, []int{batch, batch, batch, 1}) || !bytes.Equal(w.Body.Bytes(), data) || !w.Flushed {
		t.Fatalf("writes=%v flushed=%v", w.sizes, w.Flushed)
	}

	for i, deadline := range w.deadlines {
		if deadline.IsZero() {
			continue
		}

		if deadline.Before(before.Add(30*time.Second)) || i > 0 && deadline.Before(w.deadlines[i-1]) {
			t.Fatalf("deadline did not roll: %v", w.deadlines)
		}
	}

	if len(w.deadlines) != 15 || observed.Status != http.StatusPartialContent || observed.Bytes != int64(len(data)) || observed.Aborted || observed.Duration <= 0 {
		t.Fatalf("observation=%+v deadlines=%v", observed, w.deadlines)
	}
}

type racerShortWriter struct {
	*racerDeadlineWriter
}

func (w *racerShortWriter) Write(p []byte) (int, error) {
	if len(w.sizes) > 0 {
		return w.racerDeadlineWriter.Write(p[:7])
	}

	return w.racerDeadlineWriter.Write(p)
}

func TestRacerIOBatchedShortWrite(t *testing.T) {
	const batch = 256 * 1024

	w := &racerShortWriter{&racerDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}}
	wrapped := &racerResponseWriter{ResponseWriter: w, timeout: time.Second}

	n, err := wrapped.Write(make([]byte, 3*batch))
	if n != batch+7 || !errors.Is(err, io.ErrShortWrite) || !wrapped.failed || wrapped.bytes != int64(n) {
		t.Fatalf("bytes=%d err=%v failed=%v accounted=%d", n, err, wrapped.failed, wrapped.bytes)
	}

	if !slices.Equal(w.sizes, []int{batch, 7}) || len(w.deadlines) != 6 {
		t.Fatalf("writes=%v deadlines=%v", w.sizes, w.deadlines)
	}
}

func TestRacerIOFailuresObserved(t *testing.T) {
	for _, mode := range []string{"write", "deadline", "abort"} {
		t.Run(mode, func(t *testing.T) {
			w := &racerDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}

			failure := errors.New("failed")
			if mode == "write" {
				w.writeErr = failure
			}

			if mode == "deadline" {
				w.deadlineErr = failure
			}

			var observed HTTPObservation

			handler := WrapHTTP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if mode == "abort" {
					panic(http.ErrAbortHandler)
				}

				_, err := w.Write([]byte("body"))
				if err != nil {
					panic(http.ErrAbortHandler)
				}
			}), time.Second, func(o HTTPObservation) { observed = o })

			func() {
				defer func() {
					if recovered := recover(); recovered != http.ErrAbortHandler {
						t.Fatalf("panic = %v", recovered)
					}
				}()

				handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
			}()

			if !observed.Aborted || observed.Bytes != 0 {
				t.Fatalf("observation = %+v", observed)
			}
		})
	}
}

func TestRacerIOStalledDownstream(t *testing.T) {
	done := make(chan error, 1)

	server := httptest.NewServer(WrapHTTP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		data := make([]byte, racerWriteChunk)
		for {
			if _, err := w.Write(data); err != nil {
				done <- err
				return
			}
		}
	}), 50*time.Millisecond, nil))
	defer server.Close()

	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		var timeout net.Error
		if !errors.As(err, &timeout) || !timeout.Timeout() {
			t.Fatalf("expected write timeout, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stalled downstream was not interrupted")
	}
}

func TestRacerIOProgressBeyondTimeout(t *testing.T) {
	const timeout = 100 * time.Millisecond

	server := httptest.NewServer(WrapHTTP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for range 5 {
			// An upstream pause may exceed the write timeout. Each downstream
			// write still receives a fresh budget when bytes become available.
			time.Sleep(2 * timeout)

			if _, err := io.WriteString(w, "chunk"); err != nil {
				return
			}

			w.(http.Flusher).Flush()
		}
	}), timeout, nil))
	defer server.Close()

	client := &http.Client{Timeout: 5 * time.Second}

	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "chunkchunkchunkchunkchunk" {
		t.Fatalf("body=%q err=%v", body, err)
	}
}

func TestRacerIOCancellationDeadlineSurvivesRefresh(t *testing.T) {
	for _, operation := range transportOperations() {
		t.Run(operation.name, func(t *testing.T) {
			dst := &racerFastWriter{racerDeadlineWriter: &racerDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}}
			w := &racerResponseWriter{ResponseWriter: dst, timeout: time.Second}
			w.WriteHeader(http.StatusOK)
			controller := http.NewResponseController(w)
			// Reproduce the precise SDK interleaving: its initial deadline returns,
			// cancellation installs an immediate deadline, then the wrapper refreshes.
			if err := controller.SetWriteDeadline(time.Now().Add(time.Minute)); err != nil {
				t.Fatal(err)
			}

			interrupt := time.Now().Add(-time.Second)
			done := make(chan error, 1)

			go func() { done <- controller.SetWriteDeadline(interrupt) }()

			if err := <-done; err != nil {
				t.Fatal(err)
			}
			// Even another external refresh must not clear the interruption.
			if err := controller.SetWriteDeadline(time.Now().Add(time.Minute)); err != nil {
				t.Fatal(err)
			}

			err := operation.run(w, "payload")

			if !errors.Is(err, os.ErrDeadlineExceeded) || !w.failed || w.bytes != 0 || dst.reads != 0 || dst.Body.Len() != 0 || dst.Flushed {
				t.Fatalf("interruption lost: err=%v failed=%v bytes=%d reads=%d flushed=%v", err, w.failed, w.bytes, dst.reads, dst.Flushed)
			}

			if got := dst.deadlines[len(dst.deadlines)-1]; !got.Equal(interrupt) {
				t.Fatalf("deadline refreshed past interruption: %v; want %v", got, interrupt)
			}
			// SDK cleanup explicitly clears the latch. Rolling deadlines and the
			// handler's final flush must then work normally (failed stays recorded).
			if err := controller.SetWriteDeadline(time.Time{}); err != nil {
				t.Fatal(err)
			}

			if _, err := w.Write([]byte("next")); err != nil {
				t.Fatal(err)
			}

			if err := w.FlushError(); err != nil {
				t.Fatal(err)
			}

			if dst.Body.String() != "next" || !dst.Flushed || !dst.deadlines[len(dst.deadlines)-1].IsZero() {
				t.Fatal("rolling writes/final flush did not resume after clear")
			}
		})
	}
}

func TestRacerIOExternalDeadlineCapsRollingDeadline(t *testing.T) {
	dst := &racerDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	w := &racerResponseWriter{ResponseWriter: dst, timeout: time.Hour}

	deadline := time.Now().Add(time.Minute)
	if err := http.NewResponseController(w).SetWriteDeadline(deadline); err != nil {
		t.Fatal(err)
	}

	if _, err := w.Write([]byte("body")); err != nil {
		t.Fatal(err)
	}

	if err := w.FlushError(); err != nil {
		t.Fatal(err)
	}

	for _, got := range dst.deadlines {
		if !got.IsZero() && !got.Equal(deadline) {
			t.Fatalf("rolling deadline exceeded external bound: %v; want %v", got, deadline)
		}
	}
}

func TestRacerIOConcurrentDeadlineFailureAccounting(t *testing.T) {
	failure := errors.New("deadline failure")
	dst := &racerDeadlineWriter{ResponseRecorder: httptest.NewRecorder(), deadlineErr: failure}
	w := &racerResponseWriter{ResponseWriter: dst, timeout: time.Second, status: http.StatusOK}
	done := make(chan struct{})

	go func() {
		defer close(done)

		for range 1000 {
			// This callback path must not modify handler-owned accounting, even
			// when the underlying deadline operation fails.
			if err := http.NewResponseController(w).SetWriteDeadline(time.Now()); !errors.Is(err, failure) {
				t.Errorf("external deadline error=%v", err)
			}
		}
	}()

	for range 1000 {
		_, err := w.Write([]byte("body"))
		if err == nil || !w.failed || w.status != http.StatusOK || w.bytes != 0 {
			t.Errorf("handler accounting: err=%v failed=%v status=%d bytes=%d", err, w.failed, w.status, w.bytes)
		}
	}

	<-done
}

type racerInterruptOnReturn struct {
	*racerDeadlineWriter
	interrupt func()
}

func (w racerInterruptOnReturn) Write(p []byte) (int, error) {
	w.interrupt()
	return w.ResponseRecorder.Write(p)
}

func (w racerInterruptOnReturn) ReadFrom(r io.Reader) (int64, error) {
	w.interrupt()
	return io.Copy(w.ResponseRecorder, r)
}

func (w racerInterruptOnReturn) FlushError() error {
	w.interrupt()
	w.Flush()

	return nil
}

func transportOperations() []struct {
	name string
	run  func(*racerResponseWriter, string) error
} {
	return []struct {
		name string
		run  func(*racerResponseWriter, string) error
	}{
		{"Write", func(w *racerResponseWriter, data string) error {
			_, err := w.Write([]byte(data))
			return err
		}},
		{"ReadFrom", func(w *racerResponseWriter, data string) error {
			_, err := w.ReadFrom(&io.LimitedReader{R: strings.NewReader(data), N: int64(len(data))})
			return err
		}},
		{"Flush", func(w *racerResponseWriter, _ string) error { return w.FlushError() }},
	}
}

func TestRacerIOOperationClearPreservesInterrupt(t *testing.T) {
	for _, operation := range transportOperations() {
		t.Run(operation.name, func(t *testing.T) {
			dst := &racerDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
			w := &racerResponseWriter{timeout: time.Second, status: http.StatusOK}
			interrupt := time.Now().Add(-time.Second)
			w.ResponseWriter = racerInterruptOnReturn{dst, func() {
				if err := http.NewResponseController(w).SetWriteDeadline(interrupt); err != nil {
					t.Error(err)
				}
			}}

			if err := operation.run(w, "body"); err != nil {
				t.Fatal(err)
			}

			if !w.interrupted || !dst.deadlines[len(dst.deadlines)-1].Equal(interrupt) {
				t.Fatal("operation cleanup erased cancellation interrupt")
			}
		})
	}
}
