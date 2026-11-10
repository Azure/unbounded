// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/Azure/unbounded/pkg/racersdk/internal/wire"
)

// hostReadRequest is what the fake dataplane saw.
type hostReadRequest struct {
	line    string
	headers http.Header
	fds     int
}

// hostReadPeer is a fake dataplane that receives a host-read head with its
// descriptor and calls serve with the parsed request and the descriptor.
func hostReadPeer(t *testing.T, serve func(conn *net.UnixConn, req hostReadRequest, file *os.File)) (string, chan hostReadRequest) {
	t.Helper()

	path := filepath.Join(socketDir(t), "socket")

	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}

	seen := make(chan hostReadRequest, 16)
	done := make(chan struct{})

	go func() {
		defer close(done)

		for {
			conn, err := l.AcceptUnix()
			if err != nil {
				return
			}

			go func() {
				defer closeQuietly(conn)

				buf := make([]byte, 64*1024)
				oob := make([]byte, unix.CmsgSpace(4*4))

				n, oobn, _, _, err := conn.ReadMsgUnix(buf, oob)
				if err != nil {
					return
				}

				req := hostReadRequest{}

				var file *os.File

				msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
				if err == nil {
					for _, m := range msgs {
						fds, err := unix.ParseUnixRights(&m)
						if err != nil {
							continue
						}

						req.fds += len(fds)

						for i, fd := range fds {
							if i == 0 {
								file = os.NewFile(uintptr(fd), "dest")
							} else {
								_ = unix.Close(fd)
							}
						}
					}
				}

				if file != nil {
					defer closeQuietly(file)
				}

				r := bufio.NewReader(bytes.NewReader(buf[:n]))
				line, _ := r.ReadString('\n')
				req.line = strings.TrimSpace(line)

				hr, err := http.ReadRequest(bufio.NewReader(strings.NewReader(string(buf[:n]))))
				if err == nil {
					req.headers = hr.Header
				}

				seen <- req

				serve(conn, req, file)
			}()
		}
	}()

	t.Cleanup(func() { closeQuietly(l); <-done })

	return path, seen
}

// serveHostRead writes body[first:end] into file at the destination offset
// and answers with the frames Racer sends. complete=false omits the
// completion frame, like a read that fails partway.
func serveHostRead(body []byte, complete bool, delivery string) func(*net.UnixConn, hostReadRequest, *os.File) {
	return func(conn *net.UnixConn, req hostReadRequest, file *os.File) {
		if req.fds != 1 || file == nil {
			_, _ = fmt.Fprint(conn, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			return
		}

		first, end := uint64(0), uint64(len(body))

		if value := req.headers.Get("Range"); value != "" {
			spec := strings.TrimPrefix(value, "bytes=")
			a, b, _ := strings.Cut(spec, "-")
			first, _ = strconv.ParseUint(a, 10, 64)

			if b != "" {
				last, _ := strconv.ParseUint(b, 10, 64)
				end = min(last+1, end)
			}
		}

		destOffset, _ := strconv.ParseUint(req.headers.Get("Racer-Destination-Offset"), 10, 64)
		destLength, _ := strconv.ParseUint(req.headers.Get("Racer-Destination-Length"), 10, 64)

		if end-first > destLength {
			_, _ = fmt.Fprint(conn, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			return
		}

		pages := wire.PageCount(first, end)

		var b strings.Builder
		b.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nETag: \"v1\"\r\n")
		b.WriteString("Racer-Expires-At: 1234\r\n")
		fmt.Fprintf(&b, "Racer-Object-Length: %d\r\nRacer-Range-Start: %d\r\nRacer-Range-End: %d\r\n", len(body), first, end)
		fmt.Fprintf(&b, "Content-Length: %d\r\n", wire.FrameSize*(pages+1))

		if delivery != "" {
			fmt.Fprintf(&b, "Racer-Delivery: %s\r\n", delivery)
		}

		b.WriteString("Connection: close\r\n\r\n")

		if _, err := conn.Write([]byte(b.String())); err != nil {
			return
		}

		var sent uint64

		for n := uint64(0); n < pages; n++ {
			page := first/wire.PageSize + n
			start := max(first, page*wire.PageSize)
			stop := min(end, (page+1)*wire.PageSize)

			if _, err := file.WriteAt(body[start:stop], int64(destOffset+start-first)); err != nil {
				return
			}

			f := wire.Frame{Kind: wire.PageFrame, Number: page, Offset: start, Length: uint32(stop - start)}
			if err := wire.WriteFrame(conn, f); err != nil {
				return
			}

			sent += stop - start
		}

		if complete {
			_ = wire.WriteFrame(conn, wire.Frame{Kind: wire.CompleteFrame, Number: pages, Offset: sent})
		}
	}
}

func testMemfd(t *testing.T, size int64) *os.File {
	t.Helper()

	fd, err := unix.MemfdCreate("racersdk-test", unix.MFD_CLOEXEC)
	if err != nil {
		t.Skipf("memfd_create: %v", err)
	}

	f := os.NewFile(uintptr(fd), "memfd")

	t.Cleanup(func() { closeQuietly(f) })

	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}

	return f
}

func patterned(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}

	return b
}

func TestHostReadWholeObjectSpansPages(t *testing.T) {
	body := patterned(wire.PageSize + 5)
	path, seen := hostReadPeer(t, serveHostRead(body, true, "host"))
	c := testClient(t, path, 1)
	file := testMemfd(t, int64(len(body)))

	var pages [][2]int64

	result, err := c.HostRead(context.Background(), Request{Key: Key{1}, Metadata: "m"}, file, HostReadOptions{
		OnPage: func(offset, length int64) { pages = append(pages, [2]int64{offset, length}) },
	})
	if err != nil {
		t.Fatal(err)
	}

	req := <-seen
	if want := "POST /v1/objects/" + (Key{1}).String() + "/host-read HTTP/1.1"; req.line != want {
		t.Fatalf("request line %q, want %q", req.line, want)
	}

	if req.fds != 1 || req.headers.Get("Racer-Destination-Offset") != "0" ||
		req.headers.Get("Racer-Destination-Length") != strconv.Itoa(len(body)) ||
		req.headers.Get("Range") != "" || req.headers.Get("Racer-Metadata") != "m" ||
		req.headers.Get("Racer-Page-Credits") != "" {
		t.Fatalf("unexpected request %+v", req)
	}

	if result.Offset != 0 || result.Length != int64(len(body)) || result.Metadata.Size != int64(len(body)) || result.Metadata.ETag != `"v1"` {
		t.Fatalf("result %+v", result)
	}

	if want := [][2]int64{{0, wire.PageSize}, {wire.PageSize, 5}}; fmt.Sprint(pages) != fmt.Sprint(want) {
		t.Fatalf("pages %v, want %v", pages, want)
	}

	got := make([]byte, len(body))
	if _, err := file.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(got, body) {
		t.Fatal("file contents differ")
	}
}

func TestHostReadRangeAtDestinationOffset(t *testing.T) {
	body := []byte("hello")
	path, seen := hostReadPeer(t, serveHostRead(body, true, "host"))
	c := testClient(t, path, 1)
	file := testMemfd(t, 16)

	if _, err := file.WriteAt(bytes.Repeat([]byte("."), 16), 0); err != nil {
		t.Fatal(err)
	}

	result, err := c.HostRead(context.Background(), Request{}, file, HostReadOptions{
		Offset: 1, Length: 3, ETag: `"v1"`, DestinationOffset: 10,
	})
	if err != nil {
		t.Fatal(err)
	}

	req := <-seen
	if req.headers.Get("Range") != "bytes=1-3" || req.headers.Get("If-Match") != `"v1"` ||
		req.headers.Get("Racer-Destination-Offset") != "10" || req.headers.Get("Racer-Destination-Length") != "6" {
		t.Fatalf("unexpected request headers %v", req.headers)
	}

	if result.Offset != 1 || result.Length != 3 {
		t.Fatalf("result %+v", result)
	}

	got := make([]byte, 16)
	if _, err := file.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}

	if string(got) != "..........ell..." {
		t.Fatalf("file %q", got)
	}
}

func TestHostReadEmptyObject(t *testing.T) {
	path, _ := hostReadPeer(t, serveHostRead(nil, true, "host"))
	c := testClient(t, path, 1)

	result, err := c.HostRead(context.Background(), Request{}, testMemfd(t, 1), HostReadOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if result.Length != 0 || result.Metadata.Size != 0 {
		t.Fatalf("result %+v", result)
	}
}

func TestHostReadStatusErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{
		{404, ErrNotFound},
		{400, ErrInvalidRequest},
		{412, ErrVersionMismatch},
	} {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			path, _ := hostReadPeer(t, func(conn *net.UnixConn, _ hostReadRequest, _ *os.File) {
				_, _ = fmt.Fprintf(conn, "HTTP/1.1 %d X\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", tc.status)
			})
			c := testClient(t, path, 1)

			_, err := c.HostRead(context.Background(), Request{}, testMemfd(t, 8), HostReadOptions{})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err %v, want %v", err, tc.want)
			}
		})
	}
}

func TestHostReadProtocolFailures(t *testing.T) {
	body := []byte("hello")

	for name, serve := range map[string]func(*net.UnixConn, hostReadRequest, *os.File){
		"missing completion": serveHostRead(body, false, "host"),
		"stream delivery":    serveHostRead(body, true, ""),
		"gpu delivery":       serveHostRead(body, true, "gpu"),
	} {
		t.Run(name, func(t *testing.T) {
			path, _ := hostReadPeer(t, serve)
			c := testClient(t, path, 1)

			_, err := c.HostRead(context.Background(), Request{}, testMemfd(t, 8), HostReadOptions{})
			if err == nil {
				t.Fatal("expected an error")
			}

			if errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("protocol failure classified as invalid request: %v", err)
			}
		})
	}
}

func TestHostReadInvalidArguments(t *testing.T) {
	c := testClient(t, filepath.Join(t.TempDir(), "unused"), 1)
	file := testMemfd(t, 8)

	for name, tc := range map[string]struct {
		file *os.File
		o    HostReadOptions
	}{
		"nil file":                  {nil, HostReadOptions{}},
		"negative offset":           {file, HostReadOptions{Offset: -1}},
		"negative length":           {file, HostReadOptions{Length: -1}},
		"negative destination":      {file, HostReadOptions{DestinationOffset: -1}},
		"range past destination":    {file, HostReadOptions{Length: 9}},
		"explicit window too small": {file, HostReadOptions{Length: 4, DestinationLength: 3}},
		"offset at file end":        {file, HostReadOptions{DestinationOffset: 8}},
		"bad etag":                  {file, HostReadOptions{ETag: "v1"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := c.HostRead(context.Background(), Request{}, tc.file, tc.o)
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("err %v, want ErrInvalidRequest", err)
			}
		})
	}
}

func TestHostReadCanceledContext(t *testing.T) {
	block := make(chan struct{})
	path, _ := hostReadPeer(t, func(*net.UnixConn, hostReadRequest, *os.File) { <-block })
	t.Cleanup(func() { close(block) })

	c := testClient(t, path, 1)
	ctx, cancel := context.WithCancel(context.Background())

	errs := make(chan error, 1)

	go func() {
		_, err := c.HostRead(ctx, Request{}, testMemfd(t, 8), HostReadOptions{})
		errs <- err
	}()

	cancel()

	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want context.Canceled", err)
	}
}

func TestSendWithFileRejectsNonUnixConn(t *testing.T) {
	a, b := net.Pipe()
	defer closeQuietly(a)
	defer closeQuietly(b)

	if err := sendWithFile(a, []byte("x"), os.Stdin); err == nil {
		t.Fatal("expected an error")
	}
}

func TestHostReadPassesAWritableDescriptor(t *testing.T) {
	// The fake peer checks what Racer checks: a regular file, writable, no O_APPEND.
	path, _ := hostReadPeer(t, func(conn *net.UnixConn, _ hostReadRequest, file *os.File) {
		var st unix.Stat_t
		if err := unix.Fstat(int(file.Fd()), &st); err != nil || st.Mode&syscall.S_IFMT != syscall.S_IFREG {
			_, _ = fmt.Fprint(conn, "HTTP/1.1 400 X\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			return
		}

		flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFL, 0)
		if err != nil || flags&unix.O_ACCMODE == unix.O_RDONLY || flags&unix.O_APPEND != 0 {
			_, _ = fmt.Fprint(conn, "HTTP/1.1 400 X\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			return
		}

		serveHostRead([]byte("ok"), true, "host")(conn, hostReadRequest{fds: 1, headers: http.Header{
			"Racer-Destination-Length": {"2"},
		}}, file)
	})
	c := testClient(t, path, 1)

	if _, err := c.HostRead(context.Background(), Request{}, testMemfd(t, 2), HostReadOptions{}); err != nil {
		t.Fatal(err)
	}
}
