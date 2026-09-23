// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const maxHeaderBytes = 32 << 10

// readHeader cannot consume OR peek at any payload byte. Each read is bounded
// by the earliest possible end of CRLFCRLF given the suffix already consumed.
// Parsing happens later, on this isolated header, never on the live socket.
func readHeader(r io.Reader) ([]byte, error) {
	const end = "\r\n\r\n"

	b := make([]byte, 0, 512)
	matched := 0

	var scratch [4]byte
	for len(b) < maxHeaderBytes {
		n, err := io.ReadFull(r, scratch[:4-matched])
		b = append(b, scratch[:n]...)

		if err != nil {
			return nil, err
		}

		if bytes.HasSuffix(b, []byte(end)) {
			return b, nil
		}

		matched = 0

		for i := 1; i < 4 && i <= len(b); i++ {
			if bytes.HasSuffix(b, []byte(end[:i])) {
				matched = i
			}
		}
	}

	return nil, fmt.Errorf("HTTP headers exceed limit")
}

type frontend struct {
	socket  string
	objects map[string]objectSpec
	limit   int
	timeout time.Duration
	bytes   atomic.Int64
}

func newFrontend(c *configuration, socket string, limit int, timeout time.Duration) *frontend {
	f := &frontend{socket: socket, objects: make(map[string]objectSpec), limit: limit, timeout: timeout}
	for _, o := range c.Objects {
		f.objects["/"+o.Bucket+"/"+o.Key] = o
	}

	return f
}

func (f *frontend) serve(ctx context.Context, listener *net.TCPListener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stop := context.AfterFunc(ctx, func() { closeResource(listener) })
	defer stop()

	var wg sync.WaitGroup

	defer func() { cancel(); wg.Wait() }()

	slots := make(chan struct{}, f.limit)

	for {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return nil
		}

		conn, err := listener.AcceptTCP()
		if err != nil {
			<-slots

			if ctx.Err() != nil {
				return nil
			}

			return err
		}

		wg.Add(1)

		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			defer closeResource(conn)

			stop := context.AfterFunc(ctx, func() { closeResource(conn) })
			defer stop()

			if err := f.connection(ctx, conn); err != nil && ctx.Err() == nil && !errors.Is(err, io.EOF) {
				slog.Warn("frontend connection closed", "error", err)
			}
		}()
	}
}

func (f *frontend) connection(ctx context.Context, downstream *net.TCPConn) error {
	p, err := newSplicePipe()
	if err != nil {
		return err
	}
	defer p.close()

	var upstream *net.UnixConn

	defer func() {
		if upstream != nil {
			closeResource(upstream)
		}
	}()

	for {
		if err := downstream.SetDeadline(time.Now().Add(f.timeout)); err != nil {
			return err
		}

		head, err := readHeader(downstream)
		if err != nil {
			return err
		}

		req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(head)))
		if err != nil {
			return s3Error(downstream, false, 400, "InvalidRequest")
		}

		if req.Proto != "HTTP/1.1" || req.ContentLength != 0 || len(req.TransferEncoding) != 0 || req.Header.Get("Expect") != "" {
			return s3Error(downstream, req.Method == "HEAD", 400, "InvalidRequest")
		}

		if req.Method != "GET" && req.Method != "HEAD" {
			return s3Error(downstream, false, 405, "MethodNotAllowed")
		}

		o, status, code := f.selectObject(req)
		if status != 0 {
			return s3Error(downstream, req.Method == "HEAD", status, code)
		}

		if !validRange(req.Header) {
			return s3Error(downstream, req.Method == "HEAD", 400, "InvalidArgument")
		}

		if upstream == nil {
			dialer := net.Dialer{Timeout: f.timeout}

			conn, err := dialer.DialContext(ctx, "unix", f.socket)
			if err != nil {
				return s3Error(downstream, req.Method == "HEAD", 503, "ServiceUnavailable")
			}

			var ok bool

			upstream, ok = conn.(*net.UnixConn)
			if !ok {
				closeResource(conn)
				return fmt.Errorf("expected Unix connection")
			}
		}

		if err := upstream.SetDeadline(time.Now().Add(f.timeout)); err != nil {
			return err
		}
		// Closing a blocked upstream read on shutdown does not wait for its deadline.
		stop := context.AfterFunc(ctx, func() { closeResource(upstream) })
		err = f.exchange(downstream, upstream, p, req, o)

		stop()

		if err != nil || req.Close {
			return err
		}
	}
}

func (f *frontend) selectObject(r *http.Request) (objectSpec, int, string) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return objectSpec{}, 400, "InvalidArgument"
	}

	for k := range q {
		// Signature parameters don't identify representations. Other S3 APIs,
		// including listing, partNumber and versionId, are not implemented.
		if !strings.HasPrefix(k, "X-Amz-") && k != "x-id" {
			return objectSpec{}, 501, "NotImplemented"
		}
	}

	if r.URL.IsAbs() || !strings.HasPrefix(r.RequestURI, "/") {
		return objectSpec{}, 400, "InvalidURI"
	}

	o, ok := f.objects[r.URL.Path]
	if !ok {
		return objectSpec{}, 404, "NoSuchKey"
	}

	return o, 0, ""
}

func validRange(h http.Header) bool {
	v := h.Values("Range")
	if len(v) == 0 {
		return true
	}

	if len(v) != 1 || !strings.HasPrefix(v[0], "bytes=") {
		return false
	}

	a, b, ok := strings.Cut(strings.TrimPrefix(v[0], "bytes="), "-")
	if !ok || a == "" && b == "" {
		return false
	}

	parse := func(s string) (int64, bool) {
		if s == "" {
			return 0, true
		}

		for _, c := range s {
			if c < '0' || c > '9' {
				return 0, false
			}
		}

		n, err := strconv.ParseInt(s, 10, 64)

		return n, err == nil
	}
	x, okA := parse(a)
	y, okB := parse(b)

	return okA && okB && (a == "" || b == "" || x <= y)
}

func (f *frontend) exchange(dst *net.TCPConn, src *net.UnixConn, p *splicePipe, req *http.Request, o objectSpec) error {
	r := &http.Request{Method: req.Method, URL: &url.URL{Path: o.target}, Host: "racer", Header: make(http.Header)}
	for _, key := range []string{"Range", "If-Match", "If-None-Match", "If-Range"} {
		if values := req.Header.Values(key); len(values) != 0 {
			r.Header[key] = values
		}
	}

	r.Header.Set("Accept-Encoding", "identity")

	if err := r.Write(src); err != nil {
		return err
	}

	head, err := readHeader(src)
	if err != nil {
		return err
	}

	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(head)), r)
	if err != nil {
		return err
	}

	if len(resp.TransferEncoding) != 0 || resp.Header.Get("Content-Encoding") != "" && resp.Header.Get("Content-Encoding") != "identity" {
		return fmt.Errorf("unexpected upstream encoding")
	}

	if resp.StatusCode >= 400 {
		code := map[int]string{400: "InvalidRequest", 403: "AccessDenied", 404: "NoSuchKey", 412: "PreconditionFailed", 416: "InvalidRange", 503: "ServiceUnavailable"}[resp.StatusCode]
		if code == "" {
			code = "InternalError"
		}

		if err := s3Error(dst, req.Method == "HEAD", resp.StatusCode, code); err != nil {
			return err
		}

		return io.EOF
	}

	if resp.StatusCode != 200 && resp.StatusCode != 206 && resp.StatusCode != 304 {
		return fmt.Errorf("unexpected upstream status")
	}

	if resp.ContentLength < 0 && resp.StatusCode != 304 {
		return fmt.Errorf("missing upstream length")
	}

	if resp.Header.Get("ETag") != o.etag {
		return fmt.Errorf("unexpected upstream representation")
	}

	if err := validateResponseRange(req, resp); err != nil {
		return err
	}

	h := make(http.Header)

	for _, k := range []string{"Content-Length", "Content-Range", "ETag", "Accept-Ranges", "Cache-Control"} {
		if v := resp.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}

	h.Set("Content-Type", "application/octet-stream")

	if req.Close || resp.Close {
		h.Set("Connection", "close")
	}

	if _, err := fmt.Fprintf(dst, "HTTP/1.1 %d %s\r\n", resp.StatusCode, http.StatusText(resp.StatusCode)); err != nil {
		return err
	}

	if err := h.Write(dst); err != nil {
		return err
	}

	if _, err := io.WriteString(dst, "\r\n"); err != nil {
		return err
	}

	if req.Method != "HEAD" && resp.StatusCode != 304 {
		n, err := p.transfer(dst, src, resp.ContentLength)
		f.bytes.Add(n)

		if err != nil {
			return fmt.Errorf("splice body: %w", err)
		}
	}

	if resp.Close {
		return io.EOF
	}

	return nil
}

func s3Error(w io.Writer, head bool, status int, code string) error {
	body := "<?xml version=\"1.0\" encoding=\"UTF-8\"?><Error><Code>" + code + "</Code><Message>" + code + "</Message></Error>"
	if _, err := fmt.Fprintf(w, "HTTP/1.1 %d %s\r\nContent-Type: application/xml\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", status, http.StatusText(status), len(body)); err != nil {
		return err
	}

	if !head {
		_, err := io.WriteString(w, body)
		return err
	}

	return nil
}

func validateResponseRange(req *http.Request, resp *http.Response) error {
	if resp.StatusCode != http.StatusPartialContent {
		if resp.Header.Get("Content-Range") != "" {
			return fmt.Errorf("unexpected upstream content range")
		}

		if resp.StatusCode == 200 && req.Method == "GET" && req.Header.Get("Range") != "" && req.Header.Get("If-Range") == "" {
			return fmt.Errorf("upstream ignored range")
		}

		return nil
	}

	var start, end, size int64

	v := resp.Header.Get("Content-Range")
	if n, err := fmt.Sscanf(v, "bytes %d-%d/%d", &start, &end, &size); err != nil || n != 3 ||
		v != fmt.Sprintf("bytes %d-%d/%d", start, end, size) || start < 0 || end < start || end >= size || resp.ContentLength != end-start+1 {
		return fmt.Errorf("invalid upstream content range")
	}

	a, b, ok := strings.Cut(strings.TrimPrefix(req.Header.Get("Range"), "bytes="), "-")
	if !ok || req.Method != "GET" {
		return fmt.Errorf("unsolicited upstream partial response")
	}

	if a == "" {
		suffix, err := strconv.ParseInt(b, 10, 64)
		if err != nil || suffix == 0 || start != max(0, size-suffix) || end != size-1 {
			return fmt.Errorf("upstream suffix mismatch")
		}
	} else {
		first, err := strconv.ParseInt(a, 10, 64)
		if err != nil || start != first {
			return fmt.Errorf("upstream range start mismatch")
		}

		last := size - 1
		if b != "" {
			last, err = strconv.ParseInt(b, 10, 64)
			if err != nil {
				return err
			}
		}

		if end != min(last, size-1) {
			return fmt.Errorf("upstream range end mismatch")
		}
	}

	return nil
}
