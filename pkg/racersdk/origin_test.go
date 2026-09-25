// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func startOrigin(t *testing.T, config OriginConfig, origin Origin) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	path := filepath.Join(socketDir(t), "socket")
	config.Cache = CacheName{value: "test"}
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

func originExchange(t *testing.T, path, method, fields string) *http.Response {
	t.Helper()

	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { closeBody(conn) })

	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}

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

	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(conn)

	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}

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
	body := &blockedBody{done: make(chan struct{})}
	path, cancel, done := startOrigin(t, OriginConfig{RequestTimeout: 50 * time.Millisecond}, func(context.Context, OriginRequest) (Metadata, io.ReadCloser, error) { return originMeta(1), body, nil })
	response := originExchange(t, path, "GET", "Range: bytes=0-16777215\r\n")

	got, err := io.ReadAll(response.Body)
	if len(got) != 0 || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("probe final byte leaked", got, err)
	}

	if body.closed.Load() != 1 {
		t.Fatal("cancel did not close")
	}

	cancel()
	<-done
}

func TestOriginLateCallbackAndSaturation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	body := &ownedReader{Reader: strings.NewReader("")}
	path, cancel, done := startOrigin(t, OriginConfig{MaxConcurrentRequests: 1, RequestTimeout: 50 * time.Millisecond}, func(context.Context, OriginRequest) (Metadata, io.ReadCloser, error) {
		close(entered)
		<-release

		return originMeta(0), body, nil
	})

	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(conn)

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

	response, err = http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "HEAD"})
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

func TestOriginSocketLifecycle(t *testing.T) {
	dir := socketDir(t)

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

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}

	cleanup()

	data, err := os.ReadFile(path)
	if err != nil || string(data) != "replacement" {
		t.Fatal("removed replacement", err)
	}
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

			conn, err := net.Dial("unix", path)
			if err != nil {
				t.Fatal(err)
			}

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

func TestOriginEmptyProbeDeadline(t *testing.T) {
	body := &blockedBody{done: make(chan struct{}), first: true}
	path, cancel, done := startOrigin(t, OriginConfig{RequestTimeout: 30 * time.Millisecond}, func(context.Context, OriginRequest) (Metadata, io.ReadCloser, error) { return originMeta(0), body, nil })

	response := originExchange(t, path, "GET", "Range: bytes=0-16777215\r\n")
	if response.StatusCode != 503 || response.ContentLength != 0 {
		t.Fatal("empty probe deadline")
	}

	if body.closed.Load() != 1 {
		t.Fatal("probe body not closed")
	}

	cancel()
	<-done
}

func TestOriginConfigAndCycles(t *testing.T) {
	for _, config := range []OriginConfig{{}, {Cache: CacheName{value: "test"}, MaxConnections: -1}, {Cache: CacheName{value: "test"}, MaxConcurrentRequests: -1}, {Cache: CacheName{value: "test"}, SocketMode: os.ModeSymlink}, {Cache: CacheName{value: "test"}, RequestTimeout: -1}} {
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

	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeBody(conn)

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
func (b *panicCloser) Close() error           { b.closed.Add(1); panic("credential in Close") }

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
