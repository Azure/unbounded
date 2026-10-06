// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestOriginCopyBufferRetentionAndClearing(t *testing.T) {
	for range cap(originCopyBuffers) + 1 {
		b := new([copyBufferSize]byte)
		b[0], b[len(b)-1] = 1, 2
		releaseOriginBuffer(b)
	}

	if len(originCopyBuffers) != cap(originCopyBuffers) {
		t.Fatal("unbounded retention")
	}

	for range cap(originCopyBuffers) + 1 {
		b := acquireOriginBuffer()
		for _, value := range b {
			if value != 0 {
				t.Fatal("retained payload")
			}
		}
	}
}

func TestOwnedOriginCrashChild(t *testing.T) {
	path := os.Getenv("RACER_ORIGIN_CRASH_PATH")
	if path == "" {
		return
	}

	volume, err := ParseVolumeName("gantry")
	if err != nil {
		t.Fatal(err)
	}

	err = serveOrigin(context.Background(), OriginConfig{Volume: volume, RecoverStaleSocket: true},
		func(context.Context, OriginRequest) (Metadata, io.ReadCloser, error) { return originMeta(0), nil, nil }, path)
	t.Fatal(err)
}

func TestOwnedOriginSIGKILLRestart(t *testing.T) {
	path := filepath.Join(socketDir(t), "socket")
	lockPath := filepath.Join(filepath.Dir(path), ".racer-origin.lock")

	for range 2 {
		child := exec.Command(os.Args[0], "-test.run=^TestOwnedOriginCrashChild$")

		child.Env = append(os.Environ(), "RACER_ORIGIN_CRASH_PATH="+path)

		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() {
			_ = child.Process.Kill()
			if child.ProcessState == nil {
				_ = child.Wait()
			}
		})
		client := originClient(t, path, 1)
		deadline := time.Now().Add(10 * time.Second)

		for {
			conn, err := net.DialTimeout("unix", path, 50*time.Millisecond)
			if err == nil {
				closeBody(conn)
				break
			}

			if time.Now().After(deadline) {
				t.Fatalf("child did not start: %v", err)
			}

			time.Sleep(10 * time.Millisecond)
		}
		// A real protocol request proves the restarted origin serves, not just binds.
		value, err := client.Get(t.Context(), Request{Key: Key{}})
		if err != nil {
			t.Fatal(err)
		}

		closeBody(value)

		before, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}

		lock, err := os.Stat(lockPath)
		if err != nil {
			t.Fatal(err)
		}

		if _, _, err := listenOwnedOrigin(path, 0o600); err == nil {
			t.Fatal("replaced live owner")
		}

		if err := child.Process.Kill(); err != nil {
			t.Fatal(err)
		}

		if err := child.Wait(); err == nil {
			t.Fatal("child was not killed")
		}

		current, err := os.Lstat(path)
		if err != nil || !os.SameFile(before, current) {
			t.Fatalf("SIGKILL did not retain socket: %v", err)
		}

		if _, _, err := listenOrigin(path, 0o600); err == nil {
			t.Fatal("default SDK contract recovered an existing socket")
		}

		current, err = os.Stat(lockPath)
		if err != nil || !os.SameFile(lock, current) {
			t.Fatalf("lock changed: %v", err)
		}
	}

	_, cleanup, err := listenOwnedOrigin(path, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cleanup()
	cleanup() // Cleanup is idempotent and cannot act on a reused directory FD.

	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("clean shutdown retained socket: %v", err)
	}

	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("clean shutdown removed lock: %v", err)
	}
}

func startOrigin(t *testing.T, config OriginConfig, origin Origin) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	path := filepath.Join(socketDir(t), "socket")
	config.Volume = VolumeName{value: "test"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() { done <- serveOrigin(ctx, config, origin, path) }()

	t.Cleanup(func() { cancel() })

	deadline := time.Now().Add(3 * time.Second)

	for {
		if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
			break
		}

		select {
		case err := <-done:
			t.Fatal("serve", err)
		default:
		}

		if time.Now().After(deadline) {
			t.Fatal("listen timeout")
		}

		time.Sleep(time.Millisecond)
	}

	return path, cancel, done
}

func originDial(t *testing.T, path string) net.Conn {
	t.Helper()

	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { closeBody(conn) })

	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}

	return conn
}

func originExchange(t *testing.T, path, method, fields string) *http.Response {
	t.Helper()
	conn := originDial(t, path)

	if _, err := conn.Write(rawRequest(method, fields)); err != nil {
		t.Fatal(err)
	}

	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: method})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { closeBody(response.Body) })

	return response
}

func originMeta(size ByteLength) Metadata {
	return Metadata{Size: size, ETag: ETag{value: `"v"`}, ExpiresAt: time.UnixMilli(0)}
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

func TestOriginOperationsAndReuse(t *testing.T) {
	var calls atomic.Int32

	path, cancel, done := startOrigin(t, OriginConfig{}, func(_ context.Context, r OriginRequest) (Metadata, io.ReadCloser, error) {
		calls.Add(1)

		if r.Context().Authorization().ForOrigin() != "secret\xff" {
			t.Error("context changed")
		}

		if r.Operation() == OperationHead {
			return originMeta(PageSize + 1), nil, nil
		}

		return originMeta(PageSize + 1), io.NopCloser(strings.NewReader("x")), nil
	})

	conn := originDial(t, path)

	reader := bufio.NewReader(conn)
	for _, request := range []struct{ method, fields string }{{"HEAD", ""}, {"GET", "Range: bytes=16777216-33554431\r\nIf-Match: \"v\"\r\n"}, {"HEAD", "If-Match: \"v\"\r\n"}} {
		if _, err := conn.Write(rawRequest(request.method, request.fields+"Authorization: secret\xff\r\n")); err != nil {
			t.Fatal(err)
		}

		response, err := http.ReadResponse(reader, &http.Request{Method: request.method})
		if err != nil {
			t.Fatal(err)
		}

		body, err := io.ReadAll(response.Body)
		closeBody(response.Body)

		if err != nil {
			t.Fatal(err)
		}

		if request.method == "GET" && (string(body) != "x" || response.StatusCode != 206 || response.Header.Get("Content-Range") != "bytes 16777216-16777216/16777217") {
			t.Fatal("page response")
		}

		if request.method == "HEAD" && (len(body) != 0 || response.StatusCode != 200 || response.ContentLength != int64(PageSize)+1) {
			t.Fatal("HEAD response")
		}
	}

	if calls.Load() != 3 {
		t.Fatal("callback count")
	}

	cancel()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}

	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("socket retained")
	}
}

func TestOriginInterruptedPipelinedHead(t *testing.T) {
	for _, split := range []int{1, 20, 110, 114} {
		t.Run(strconv.Itoa(split), func(t *testing.T) {
			testOriginInterruptedPipelinedHead(t, split, "Authorization: secret\r\n", http.StatusOK)
		})
	}

	t.Run("malformed", func(t *testing.T) {
		testOriginInterruptedPipelinedHead(t, 110, "Authorization: secret \r\n", http.StatusBadRequest)
	})
	t.Run("oversized", func(t *testing.T) {
		testOriginInterruptedPipelinedHead(t, maxHeadBytes-1, "X: "+strings.Repeat("x", maxHeadBytes)+"\r\n", http.StatusRequestHeaderFieldsTooLarge)
	})
}

func testOriginInterruptedPipelinedHead(t *testing.T, split int, fields string, status int) {
	t.Helper()

	entered, release := make(chan struct{}), make(chan struct{})

	var calls atomic.Int32

	path, cancel, done := startOrigin(t, OriginConfig{}, func(ctx context.Context, _ OriginRequest) (Metadata, io.ReadCloser, error) {
		if calls.Add(1) == 1 {
			close(entered)

			select {
			case <-release:
			case <-ctx.Done():
				return Metadata{}, nil, ctx.Err()
			}
		}

		return originMeta(0), nil, nil
	})

	defer func() { cancel(); <-done }()

	conn := originDial(t, path)
	_, err := conn.Write(rawRequest("HEAD", ""))
	require.NoError(t, err)

	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first callback did not start")
	}

	second := rawRequest("HEAD", fields)
	require.Less(t, split, len(second))
	_, err = conn.Write(second[:split])
	require.NoError(t, err)
	time.Sleep(50 * time.Millisecond)
	close(release)

	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: "HEAD"})
	require.NoError(t, err)
	closeBody(response.Body)
	require.Equal(t, http.StatusOK, response.StatusCode)
	time.Sleep(20 * time.Millisecond)

	_, err = conn.Write(second[split:])
	require.NoError(t, err)
	response, err = http.ReadResponse(reader, &http.Request{Method: "HEAD"})
	require.NoError(t, err)
	closeBody(response.Body)
	require.Equal(t, status, response.StatusCode)

	if status == http.StatusOK {
		require.EqualValues(t, 2, calls.Load())
	} else {
		require.Zero(t, response.ContentLength)
		require.True(t, response.Close)
		require.EqualValues(t, 1, calls.Load())
	}
}

func TestOriginPipelinedHeadTimeout(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})

	var calls atomic.Int32

	path, cancel, done := startOrigin(t, OriginConfig{ReadHeaderTimeout: 100 * time.Millisecond}, func(ctx context.Context, _ OriginRequest) (Metadata, io.ReadCloser, error) {
		calls.Add(1)
		close(entered)

		select {
		case <-release:
		case <-ctx.Done():
			return Metadata{}, nil, ctx.Err()
		}

		return originMeta(0), nil, nil
	})

	defer func() { cancel(); <-done }()

	conn := originDial(t, path)
	_, err := conn.Write(rawRequest("HEAD", ""))
	require.NoError(t, err)

	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first callback did not start")
	}

	_, err = io.WriteString(conn, "HEAD ")
	require.NoError(t, err)
	time.Sleep(200 * time.Millisecond)
	close(release)

	reader := bufio.NewReader(conn)
	for _, status := range []int{http.StatusOK, http.StatusBadRequest} {
		response, err := http.ReadResponse(reader, &http.Request{Method: "HEAD"})
		require.NoError(t, err)
		closeBody(response.Body)
		require.Equal(t, status, response.StatusCode)

		if status == http.StatusBadRequest {
			require.Zero(t, response.ContentLength)
			require.True(t, response.Close)
		}
	}

	require.EqualValues(t, 1, calls.Load())
}

func TestOriginHeadRepeatedInterruptions(t *testing.T) {
	server, client := net.Pipe()
	defer closeBody(server)
	defer closeBody(client)

	require.NoError(t, client.SetDeadline(time.Now().Add(3*time.Second)))

	c := &originConn{Conn: server, config: OriginConfig{ReadHeaderTimeout: time.Second}}
	wire := rawRequest("HEAD", "Authorization: secret\r\n")

	var deadline time.Time

	for i, end := range []int{1, 20, len(wire) - 1} {
		result := make(chan error, 1)
		start := len(c.raw)

		go func() {
			var one [1]byte

			n, err := c.Read(one[:])
			if n != 0 {
				err = fmt.Errorf("interrupted read returned %d bytes", n)
			}

			result <- err
		}()

		_, err := client.Write(wire[start:end])
		require.NoError(t, err)
		require.NoError(t, c.SetReadDeadline(time.Unix(1, 0)))

		select {
		case err := <-result:
			var timeout net.Error
			require.ErrorAs(t, err, &timeout)
			require.True(t, timeout.Timeout())
		case <-time.After(time.Second):
			t.Fatal("read did not abort")
		}

		require.Equal(t, wire[:end], c.raw)
		require.Empty(t, c.pending)
		require.False(t, c.failed)

		if i == 0 {
			deadline = c.headerDeadline
		} else {
			require.Equal(t, deadline, c.headerDeadline)
		}

		require.NoError(t, c.SetReadDeadline(time.Time{}))
	}

	c.reader = bufio.NewReader(io.MultiReader(bytes.NewReader(wire[len(c.raw):]), server))
	buffer := make([]byte, maxHeadBytes)
	n, err := c.Read(buffer)
	require.NoError(t, err)
	require.Equal(t, wire, buffer[:n])
	require.Len(t, c.pending, 1)
	require.NoError(t, c.takeHead().err)
	require.Empty(t, c.raw)
}

func TestOriginInterruptedHeadExpiredWithBufferedRemainder(t *testing.T) {
	server, client := net.Pipe()
	defer closeBody(server)
	defer closeBody(client)

	wire := rawRequest("HEAD", "")
	c := &originConn{
		Conn: server, first: true, config: OriginConfig{ReadHeaderTimeout: time.Second},
		raw: wire[:1], reader: bufio.NewReader(bytes.NewReader(wire[1:])),
		headerDeadline: time.Now().Add(-time.Millisecond),
	}
	buffer := make([]byte, maxHeadBytes)
	_, err := c.Read(buffer)
	require.NoError(t, err)
	require.True(t, c.failed)
	require.Len(t, c.pending, 1)
	require.ErrorIs(t, c.takeHead().err, os.ErrDeadlineExceeded)
	_, err = c.Read(buffer)
	require.ErrorIs(t, err, io.EOF)
}

func TestOriginBodyContracts(t *testing.T) {
	for _, mode := range []string{"valid", "empty", "nil", "head-body", "short", "long", "final-error", "panic-read", "empty-long", "empty-error", "callback-error", "panic-callback", "metadata", "pin", "416-invalid", "416-valid"} {
		t.Run(mode, func(t *testing.T) {
			body := &ownedReader{Reader: strings.NewReader("abc")}

			method, fields := "GET", "Range: bytes=0-16777215\r\n"
			if mode == "head-body" {
				method, fields = "HEAD", ""
			}

			if mode == "pin" {
				fields += "If-Match: \"other\"\r\n"
			}

			path, cancel, done := startOrigin(t, OriginConfig{}, func(context.Context, OriginRequest) (Metadata, io.ReadCloser, error) {
				m := originMeta(3)

				switch mode {
				case "empty":
					m.Size = 0
					body.Reader = strings.NewReader("")
				case "nil":
					return m, nil, nil
				case "short":
					body.Reader = strings.NewReader("ab")
				case "long":
					body.Reader = strings.NewReader("abcd")
				case "final-error":
					body.Reader = finalErrorReader{err: errors.New("private failure")}
				case "panic-read":
					body.Reader = panicReader{}
				case "empty-long":
					m.Size = 0
				case "empty-error":
					m.Size = 0
					body.Reader = errorReader{}
				case "callback-error":
					return Metadata{}, body, NewOriginError(ErrorForbidden, errors.New("secret"))
				case "panic-callback":
					panic("private credential")
				case "metadata":
					m.ETag = ETag{}
				case "416-invalid":
					return Metadata{}, body, NewOriginError(ErrorUnsatisfiableRange, nil)
				case "416-valid":
					return m, body, NewOriginError(ErrorUnsatisfiableRange, nil)
				}

				return m, body, nil
			})
			response := originExchange(t, path, method, fields)
			got, err := io.ReadAll(response.Body)
			status := 502

			switch mode {
			case "valid":
				status = 206

				if string(got) != "abc" || err != nil {
					t.Fatal("valid stream", err)
				}
			case "empty":
				status = 200
			case "short", "long", "final-error", "panic-read":
				status = 206

				if !errors.Is(err, io.ErrUnexpectedEOF) || len(got) >= 3 {
					t.Fatal("late failure not truncated", string(got), err)
				}
			case "callback-error":
				status = 403
			case "panic-callback":
				status = 500
			case "416-valid":
				status = 416

				if response.Header.Get("Content-Range") != "bytes */3" {
					t.Fatal("416 size")
				}
			}

			if response.StatusCode != status {
				t.Fatal("status", response.StatusCode, status)
			}

			if status >= 400 && (len(got) != 0 || response.ContentLength != 0 || response.Header.Get("ETag") != "") {
				t.Fatal("error framing")
			}

			if mode != "nil" && mode != "panic-callback" {
				waitClosed(t, body)
			}

			cancel()
			<-done
		})
	}
}

type panicReader struct{}

func (panicReader) Read([]byte) (int, error) { panic("secret panic") }

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("secret error") }

func TestOriginRawMaliciousRequests(t *testing.T) {
	var calls atomic.Int32

	path, cancel, done := startOrigin(t, OriginConfig{}, func(context.Context, OriginRequest) (Metadata, io.ReadCloser, error) {
		calls.Add(1)
		return originMeta(0), nil, nil
	})

	defer func() { cancel(); <-done }()

	for _, fields := range []string{
		"Content-Length: 0\r\nContent-Length: 0\r\n", "Content-Length: 1\r\n", "Transfer-Encoding: identity\r\n", "Transfer-Encoding: chunked\r\n", "Expect: 100-continue\r\n", "Content-Encoding: identity\r\n", "Authorization:  secret\r\n", "Authorization: secret \r\n", "Authorization:\tsecret\r\n", "Host: racer\r\n", "X: a\r\n folded\r\n", "Range: bytes=0-0\r\n", "If-Match: W/\"v\"\r\n", "Authorization: " + strings.Repeat("x", 8193) + "\r\n", "X: " + strings.Repeat("x", maxHeadBytes) + "\r\n",
	} {
		t.Run(strconv.Itoa(len(fields))+fields[:min(10, len(fields))], func(t *testing.T) {
			response := originExchange(t, path, "HEAD", fields)
			if response.StatusCode != 400 && response.StatusCode != 431 {
				t.Fatal("bad status", response.StatusCode)
			}

			if response.ContentLength != 0 || !response.Close {
				t.Fatal("malformed request not closed/empty")
			}
		})
	}

	response := originExchange(t, path, "POST", "")
	if response.StatusCode != 405 || response.Header.Get("Allow") != "HEAD, GET" {
		t.Fatal("method")
	}

	if calls.Load() != 0 {
		t.Fatal("malicious callback")
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

func TestOriginProbeCancellation(t *testing.T) {
	for _, tt := range []struct {
		name    string
		size    ByteLength
		timeout time.Duration
		status  int
		readErr error
	}{
		{"final byte", 1, 50 * time.Millisecond, 206, io.ErrUnexpectedEOF},
		{"empty", 0, 30 * time.Millisecond, 503, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := &blockedBody{done: make(chan struct{}), first: tt.size == 0}
			path, cancel, done := startOrigin(t, OriginConfig{RequestTimeout: tt.timeout}, func(context.Context, OriginRequest) (Metadata, io.ReadCloser, error) {
				return originMeta(tt.size), body, nil
			})

			defer func() { cancel(); <-done }()

			response := originExchange(t, path, "GET", "Range: bytes=0-16777215\r\n")
			require.Equal(t, tt.status, response.StatusCode)

			got, err := io.ReadAll(response.Body)
			require.Empty(t, got, "probe final byte leaked")
			require.ErrorIs(t, err, tt.readErr)

			if tt.size == 0 {
				require.Zero(t, response.ContentLength)
			}

			require.EqualValues(t, 1, body.closed.Load(), "probe body close count")
		})
	}
}

func TestOriginLateCallbackAndSaturation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	body := &ownedReader{Reader: strings.NewReader("")}
	path, cancel, done := startOrigin(t, OriginConfig{MaxConcurrentHeadRequests: 1, RequestTimeout: 50 * time.Millisecond}, func(context.Context, OriginRequest) (Metadata, io.ReadCloser, error) {
		close(entered)
		<-release

		return originMeta(0), body, nil
	})

	conn := originDial(t, path)
	if _, err := conn.Write(rawRequest("HEAD", "")); err != nil {
		t.Fatal(err)
	}

	<-entered

	response := originExchange(t, path, "HEAD", "")
	if response.StatusCode != 503 {
		t.Fatal("not saturated")
	}

	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "HEAD"})
	if err != nil || response.StatusCode != 503 {
		t.Fatal("deadline before headers", err)
	}

	closeBody(response.Body)

	response = originExchange(t, path, "HEAD", "")
	if response.StatusCode != 503 {
		t.Fatal("stuck callback released slot")
	}

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown waited for callback")
	}

	close(release)
	waitClosed(t, body)
}

func TestOriginDisconnectAndServerCancel(t *testing.T) {
	for _, serverCancel := range []bool{false, true} {
		t.Run(strconv.FormatBool(serverCancel), func(t *testing.T) {
			entered, canceled := make(chan struct{}), make(chan struct{})
			path, cancel, done := startOrigin(t, OriginConfig{}, func(ctx context.Context, _ OriginRequest) (Metadata, io.ReadCloser, error) {
				close(entered)
				<-ctx.Done()
				close(canceled)

				return Metadata{}, nil, ctx.Err()
			})

			conn := originDial(t, path)
			if _, err := conn.Write(rawRequest("HEAD", "")); err != nil {
				t.Fatal(err)
			}

			<-entered

			if serverCancel {
				cancel()
			} else {
				closeBody(conn)
			}

			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("callback not canceled")
			}

			cancel()
			<-done
			closeBody(conn)
		})
	}
}

func TestOriginConnectionAndHeaderLimits(t *testing.T) {
	var calls atomic.Int32

	path, cancel, done := startOrigin(t, OriginConfig{MaxConnections: 1, ReadHeaderTimeout: 80 * time.Millisecond, IdleTimeout: 40 * time.Millisecond}, func(context.Context, OriginRequest) (Metadata, io.ReadCloser, error) {
		calls.Add(1)
		return originMeta(0), nil, nil
	})

	defer func() { cancel(); <-done }()

	first, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(first)

	if _, err := io.WriteString(first, "HEAD "); err != nil {
		t.Fatal(err)
	}

	second, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(second)

	if _, err := second.Write(rawRequest("HEAD", "")); err != nil {
		t.Fatal(err)
	}

	time.Sleep(20 * time.Millisecond)

	if calls.Load() != 0 {
		t.Fatal("accepted beyond connection cap")
	}

	if err := second.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	response, err := http.ReadResponse(bufio.NewReader(second), &http.Request{Method: "HEAD"})
	if err != nil || response.StatusCode != 200 {
		t.Fatal("slot not released after slow header", err)
	}

	closeBody(response.Body)

	var one [1]byte
	if _, err := second.Read(one[:]); err != io.EOF {
		t.Fatal("idle timeout", err)
	}
}

func TestOriginSelectedRangeErrors(t *testing.T) {
	for _, tt := range []struct {
		fields string
		size   ByteLength
		kind   ErrorKind
		status int
	}{
		{"Range: bytes=0-0\r\nIf-Match: \"v\"\r\n", 3, 0, 400},
		{"Range: bytes=16777216-33554431\r\nIf-Match: \"v\"\r\n", 3, 0, 416},
		{"Range: bytes=0-16777215\r\nIf-Match: \"v\"\r\n", 3, ErrorNotFound, 412},
		{"Range: bytes=0-16777215\r\n", 0, ErrorNotFound, 404},
		{"Range: bytes=0-16777215\r\n", 0, ErrorUnauthorized, 401},
	} {
		t.Run(strconv.Itoa(tt.status), func(t *testing.T) {
			body := &ownedReader{Reader: strings.NewReader("")}
			path, cancel, done := startOrigin(t, OriginConfig{}, func(context.Context, OriginRequest) (Metadata, io.ReadCloser, error) {
				var err error
				if tt.kind != 0 {
					err = NewOriginError(tt.kind, nil)
				}

				return originMeta(tt.size), body, err
			})

			response := originExchange(t, path, "GET", tt.fields)
			if response.StatusCode != tt.status {
				t.Fatal("status", response.StatusCode)
			}

			if tt.status == 416 && response.Header.Get("Content-Range") != "bytes */3" {
				t.Fatal("selected size")
			}

			waitClosed(t, body)
			cancel()
			<-done
		})
	}
}

func TestOriginConfigAndCycles(t *testing.T) {
	for _, config := range []OriginConfig{{}, {Volume: VolumeName{value: "test"}, MaxConnections: -1}, {Volume: VolumeName{value: "test"}, MaxConcurrentRequests: -1}, {Volume: VolumeName{value: "test"}, SocketMode: os.ModeSymlink}, {Volume: VolumeName{value: "test"}, RequestTimeout: -1}} {
		_, err := config.defaults()
		assertKind(t, err, ErrorInvalidArgument)
	}

	for range 15 {
		path, cancel, done := startOrigin(t, OriginConfig{}, func(context.Context, OriginRequest) (Metadata, io.ReadCloser, error) { return originMeta(0), nil, nil })

		response := originExchange(t, path, "HEAD", "")
		if response.StatusCode != 200 {
			t.Fatal("cycle")
		}

		cancel()

		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}

		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatal("cycle leaked socket")
		}
	}
}

func TestOriginSlowDestination(t *testing.T) {
	body := &ownedReader{Reader: io.LimitReader(repeatedByte('x'), int64(PageSize))}
	path, cancel, done := startOrigin(t, OriginConfig{WriteTimeout: 30 * time.Millisecond}, func(context.Context, OriginRequest) (Metadata, io.ReadCloser, error) {
		return originMeta(PageSize), body, nil
	})

	conn := originDial(t, path)
	if _, err := conn.Write(rawRequest("GET", "Range: bytes=0-16777215\r\n")); err != nil {
		t.Fatal(err)
	}
	// No response reads: the finite kernel send buffer must apply backpressure.
	waitClosed(t, body)
	cancel()
	<-done
}

func TestOriginExactRawLimitsAndTarget(t *testing.T) {
	var calls atomic.Int32

	path, cancel, done := startOrigin(t, OriginConfig{}, func(_ context.Context, r OriginRequest) (Metadata, io.ReadCloser, error) {
		calls.Add(1)

		if len(r.Context().Metadata().ForOrigin()) != 8192 {
			t.Error("opaque limit not preserved")
		}

		return originMeta(0), nil, nil
	})

	defer func() { cancel(); <-done }()

	fields := "Racer-Metadata: " + strings.Repeat("x", 8192) + "\r\nX: \r\n"
	base := rawRequest("HEAD", fields)
	fields = strings.Replace(fields, "X: \r\n", "X: "+strings.Repeat("x", maxHeadBytes-len(base))+"\r\n", 1)

	response := originExchange(t, path, "HEAD", fields)
	if response.StatusCode != 200 || calls.Load() != 1 {
		t.Fatal("exact 32 KiB head rejected")
	}

	for _, target := range []string{objectPrefix + (Key{}).String() + "?", "http://racer" + objectPrefix + (Key{}).String(), objectPrefix + strings.Repeat("A", 64), objectPrefix + "%30" + strings.Repeat("0", 63), "/v1//objects/" + (Key{}).String()} {
		conn, err := net.Dial("unix", path)
		if err != nil {
			t.Fatal(err)
		}

		if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}

		wire := "HEAD " + target + " HTTP/1.1\r\nHost: racer\r\n\r\n"
		if _, err := io.WriteString(conn, wire); err != nil {
			t.Fatal(err)
		}

		response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "HEAD"})
		if err != nil || response.StatusCode != 400 || response.ContentLength != 0 {
			t.Fatal("target accepted", err)
		}

		closeBody(response.Body)
		closeBody(conn)
	}

	if calls.Load() != 1 {
		t.Fatal("invalid target invoked origin")
	}
}

type panicCloser struct{ closed atomic.Int32 }

func (*panicCloser) Read([]byte) (int, error) { return 0, io.EOF }

func (b *panicCloser) Close() error { b.closed.Add(1); panic("credential in Close") }

func TestOriginClosePanicContained(t *testing.T) {
	body := &panicCloser{}
	path, cancel, done := startOrigin(t, OriginConfig{}, func(context.Context, OriginRequest) (Metadata, io.ReadCloser, error) { return originMeta(0), body, nil })

	response := originExchange(t, path, "GET", "Range: bytes=0-16777215\r\n")
	if response.StatusCode != 200 {
		t.Fatal("empty success")
	}

	cancel()
	<-done

	deadline := time.Now().Add(time.Second)
	for body.closed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	if body.closed.Load() != 1 {
		t.Fatal("panic close count")
	}
}

func TestOriginHeadReservedFromFullBodyAdmission(t *testing.T) {
	path, cancel, done := startOrigin(t, OriginConfig{MaxConcurrentRequests: 2, MaxConcurrentHeadRequests: 1}, func(_ context.Context, r OriginRequest) (Metadata, io.ReadCloser, error) {
		if r.Operation() == OperationHead {
			return originMeta(1), nil, nil
		}

		return originMeta(1), &blockedBody{done: make(chan struct{}), first: true}, nil
	})

	defer func() { cancel(); <-done }()

	c := originClient(t, path, 2)
	for range 2 {
		v, err := c.Get(context.Background(), Request{})
		if err != nil {
			t.Fatal(err)
		}
		defer closeBody(v)
	}

	ctx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()

	m, err := c.Stat(ctx, Request{})
	if err != nil || m.Size != 1 {
		t.Fatal("GET bodies starved origin HEAD", err)
	}
}

func TestOriginSocketBindingFailuresAndWitnessCleanup(t *testing.T) {
	dir := ownedSocketTestDir(t)
	t.Run("missing witness directory", func(t *testing.T) {
		_, _, err := listenOriginAtWitness(filepath.Join(dir, "missing", "socket"), 0o600)
		assertKind(t, err, ErrorIO)

		var typed *Error
		if !errors.As(err, &typed) || typed.Operation() != "socket bind" || !errors.Is(err, os.ErrNotExist) {
			t.Fatal("changed bind error", err)
		}
	})
	t.Run("witness cleanup", func(t *testing.T) {
		path := filepath.Join(dir, "witness")

		_, cleanup, err := listenOriginAtWitness(path, 0o640)
		if err != nil {
			t.Fatal(err)
		}

		cleanup()
		cleanup()

		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatal("witness retained", err)
		}
	})
	t.Run("witness replacement", func(t *testing.T) {
		path := filepath.Join(dir, "replacement")

		_, cleanup, err := listenOriginAtWitness(path, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()

		require.NoError(t, os.Remove(path))
		require.NoError(t, os.WriteFile(path, []byte("keep"), 0o600))
		cleanup()
		originAssertFileContent(t, path, "keep")
	})
	t.Run("public validation", func(t *testing.T) {
		assertKind(t, ServeOrigin(context.Background(), OriginConfig{}, nil), ErrorInvalidArgument)
	})
}

func originAssertFileContent(t *testing.T, path, want string) {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, want, string(data), "file changed: %s", path)
}

func ownedSocketTestDir(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("../../tmp", "sdk-")
	if err != nil {
		t.Fatal(err)
	}

	path, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := os.RemoveAll(path); err != nil {
			t.Error(err)
		}
	})

	return path
}

func TestOriginSocketLifecycle(t *testing.T) {
	dir := ownedSocketTestDir(t)

	path := filepath.Join(dir, "socket")
	if err := os.WriteFile(path, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := listenOrigin(path, 0o600); err == nil {
		t.Fatal("replaced file")
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink("missing", path); err != nil {
		t.Fatal(err)
	}

	if _, _, err := listenOrigin(path, 0o600); err == nil {
		t.Fatal("followed socket symlink")
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(dir, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}

	if _, _, err := listenOrigin(filepath.Join(link, "socket"), 0o600); err == nil {
		t.Fatal("followed parent symlink")
	}

	l, cleanup, err := listenOrigin(path, 0o640)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatal("mode", err)
	}

	if _, _, err := listenOrigin(path, 0o600); err == nil {
		t.Fatal("replaced live socket")
	}

	closeBody(l)

	if _, _, err := listenOrigin(path, 0o600); err == nil {
		t.Fatal("replaced stale socket")
	}

	require.NoError(t, os.Remove(path))
	require.NoError(t, os.WriteFile(path, []byte("replacement"), 0o600))
	cleanup()
	originAssertFileContent(t, path, "replacement")
}

func TestOwnedOriginRecoversWitnessBeforePublication(t *testing.T) {
	dir := ownedSocketTestDir(t)
	path := filepath.Join(dir, "socket")
	// Keep sun_path short in deeply nested worktrees.
	anchor, err := openOriginDirectory(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(anchor)

	witness := fmt.Sprintf("/proc/self/fd/%d/.racer-origin.socket", anchor.Fd())

	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: witness, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}

	listener.SetUnlinkOnClose(false)
	closeBody(listener)

	_, cleanup, err := listenOwnedOrigin(path, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cleanup()
}

func TestOwnedOriginRejectsSymlinkAncestorAndStaleReplacement(t *testing.T) {
	dir := ownedSocketTestDir(t)

	link := filepath.Join(dir, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}

	if _, _, err := listenOwnedOrigin(filepath.Join(link, "socket"), 0o600); err == nil {
		t.Fatal("followed directory symlink")
	}

	path := filepath.Join(dir, "socket")

	l, cleanup, err := listenOwnedOrigin(path, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	closeBody(l)

	if err := os.Rename(path, filepath.Join(dir, "original")); err != nil {
		t.Fatal(err)
	}

	foreign, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}

	foreign.SetUnlinkOnClose(false)
	closeBody(foreign)

	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := recoverOriginSocket(path, filepath.Join(dir, ".racer-origin.socket")); !errors.Is(err, os.ErrExist) {
		t.Fatalf("accepted foreign stale inode: %v", err)
	}

	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, current) {
		t.Fatalf("foreign stale inode removed: %v", err)
	}
}

func TestOwnedOriginUnsafePathsAndForeignEndpoints(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "directory", "fifo", "permissions", "writable-directory", "foreign-live", "foreign-stale", "socket-symlink", "socket-file", "witness-symlink", "witness-file"} {
		t.Run(kind, func(t *testing.T) {
			dir := ownedSocketTestDir(t)
			path := filepath.Join(dir, "socket")
			lock := filepath.Join(dir, ".racer-origin.lock")

			target := filepath.Join(dir, "target")
			if err := os.WriteFile(target, []byte("preserve"), 0o600); err != nil {
				t.Fatal(err)
			}

			var err error

			switch kind {
			case "symlink":
				err = os.Symlink(target, lock)
			case "hardlink":
				err = os.Link(target, lock)
			case "directory":
				err = os.Mkdir(lock, 0o700)
			case "fifo":
				err = unix.Mkfifo(lock, 0o600)
			case "permissions":
				err = os.WriteFile(lock, nil, 0o644)
			case "writable-directory":
				err = os.Chmod(dir, 0o777)
			case "socket-symlink":
				err = os.Symlink(target, path)
			case "socket-file":
				err = os.WriteFile(path, []byte("foreign"), 0o600)
			case "witness-symlink":
				err = os.Symlink(target, filepath.Join(dir, ".racer-origin.socket"))
			case "witness-file":
				err = os.WriteFile(filepath.Join(dir, ".racer-origin.socket"), nil, 0o600)
			default:
				var listener *net.UnixListener

				listener, err = net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
				if err == nil {
					listener.SetUnlinkOnClose(false)
					t.Cleanup(func() { closeBody(listener) })

					if kind == "foreign-stale" {
						closeBody(listener)
					}
				}
			}

			if err != nil {
				t.Fatal(err)
			}

			before, _ := os.Lstat(path)
			if _, _, err := listenOwnedOrigin(path, 0o600); err == nil {
				t.Fatal("accepted unsafe endpoint")
			}

			if before != nil {
				current, err := os.Lstat(path)
				if err != nil || !os.SameFile(before, current) {
					t.Fatalf("foreign endpoint changed: %v", err)
				}
			}

			originAssertFileContent(t, target, "preserve")
		})
	}
}

func TestOwnedOriginPreservesReplacementAndLiveWitness(t *testing.T) {
	dir := ownedSocketTestDir(t)
	path := filepath.Join(dir, "socket")

	_, cleanup, err := listenOwnedOrigin(path, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	// Even loss of the lock path cannot authorize replacing a live witness.
	if err := os.Rename(filepath.Join(dir, ".racer-origin.lock"), filepath.Join(dir, "old-lock")); err != nil {
		t.Fatal(err)
	}

	if _, _, err := listenOwnedOrigin(path, 0o600); err == nil {
		t.Fatal("replaced live witness")
	}

	require.NoError(t, os.Remove(path))
	require.NoError(t, os.WriteFile(path, []byte("foreign"), 0o600))
	cleanup()
	originAssertFileContent(t, path, "foreign")

	if _, _, err := listenOwnedOrigin(path, 0o600); !errors.Is(err, os.ErrExist) {
		t.Fatalf("foreign replacement accepted: %v", err)
	}
}
