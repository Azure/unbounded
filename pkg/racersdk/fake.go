// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// NewFakeClient returns a real Client backed by a sequential, noncaching fake
// Racer and the SDK's real origin validation/serving machinery. It is a test
// helper, not a Racer implementation or evidence of real Racer compatibility.
// It uses private loopback listeners, requires no /run provisioning, and adds no
// production endpoint overrides. Client and origin resource defaults apply.
//
// The fake forwards request metadata and authorization unchanged. It supports
// v1 reads and credit-controlled v2 subscriptions, forwarding pinned continuations
// as whole-page origin requests without object-sized buffering. Origin
// callback errors before response headers retain their HTTP classification;
// later errors abort the stream. Origin must obey the same cancellation/body
// ownership contract as ServeOrigin.
//
// Always call the returned, concurrent-safe, idempotent cleanup function (for
// example with t.Cleanup). It closes the Client, cancels origin work, and closes
// both servers and their connections. Client.Close alone does not stop the fake
// servers. Cleanup does not wait for callbacks that ignore cancellation; bodies
// returned late are still closed. A nil origin is invalid.
func NewFakeClient(origin Origin) (*Client, func(), error) {
	if origin == nil {
		return nil, nil, failure(ErrorInvalidArgument, "fake origin", nil)
	}

	cache := CacheName{value: "sdk-fake"}

	config, err := (OriginConfig{Cache: cache}).defaults()
	if err != nil {
		return nil, nil, err
	}

	client, err := NewClient(ClientConfig{Cache: cache})
	if err != nil {
		return nil, nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	transport := &http.Transport{
		DisableCompression: true, MaxConnsPerHost: 16, MaxIdleConns: 16, MaxIdleConnsPerHost: 16,
		ResponseHeaderTimeout: config.RequestTimeout, IdleConnTimeout: config.IdleTimeout,
		MaxResponseHeaderBytes: maxHeadBytes,
	}

	var (
		servers []*http.Server
		serving sync.WaitGroup
		once    sync.Once
	)

	cleanup := func() {
		once.Do(func() {
			closeBody(client)
			cancel()

			for _, server := range servers {
				closeBody(server)
			}

			transport.CloseIdleConnections()
			serving.Wait()
		})
	}

	start := func(handler http.Handler, isOrigin bool) (string, error) {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return "", ioFailure("fake listen", err)
		}

		address := listener.Addr().String()

		server := &http.Server{
			Handler: handler, ReadHeaderTimeout: config.ReadHeaderTimeout,
			IdleTimeout: config.IdleTimeout, MaxHeaderBytes: maxHeadBytes,
			ErrorLog:    log.New(io.Discard, "", 0),
			BaseContext: func(net.Listener) context.Context { return ctx },
		}
		if isOrigin {
			listener = &originListener{Listener: listener, ctx: ctx, slots: make(chan struct{}, config.MaxConnections), config: config}
			server.ConnContext = func(ctx context.Context, conn net.Conn) context.Context {
				return context.WithValue(ctx, originConnKey{}, conn)
			}
		}

		servers = append(servers, server)

		serving.Add(1)

		go func() {
			defer serving.Done()
			// The listener is already bound. Cleanup owns normal serve termination.
			if err := server.Serve(listener); err != nil {
				closeBody(listener)
				return
			}

			closeBody(listener)
		}()

		return address, nil
	}

	slots := make(chan struct{}, config.MaxConcurrentRequests)
	headSlots := make(chan struct{}, config.MaxConcurrentHeadRequests)

	originAddress, err := start(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveOperation(w, r, config, origin, slots, headSlots)
	}), true)
	if err != nil {
		cleanup()
		return nil, nil, err
	}

	dialer := &net.Dialer{Timeout: config.ReadHeaderTimeout}
	transport.DialContext = func(dialCtx context.Context, _, _ string) (net.Conn, error) {
		dialCtx, stop := context.WithCancel(dialCtx)
		defer stop()

		unhook := context.AfterFunc(ctx, stop)
		defer unhook()

		return dialer.DialContext(dialCtx, "tcp4", originAddress)
	}

	address, err := start(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.RequestURI, "/v2/") {
			serveFakeSubscription(w, r, transport)
			return
		}

		serveFakePages(w, r, transport)
	}), false)
	if err != nil {
		cleanup()
		return nil, nil, err
	}

	client.dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp4", address)
	}

	return client, cleanup, nil
}

type fakeSubscriptionRequest struct {
	request                  OriginRequest
	first, end               uint64
	ranged                   bool
	pageCredits, byteCredits uint64
}

// Translate only the subscription envelope. Origin traffic still uses the v1
// validators and whole-page requests, including immutable continuation checks.
func parseFakeSubscription(r *http.Request) (fakeSubscriptionRequest, error) {
	s := fakeSubscriptionRequest{end: math.MaxInt64, pageCredits: 2, byteCredits: 2 * uint64(PageSize)}
	bad := failure(ErrorInvalidArgument, "fake subscription", nil)

	const prefix = "/v2/objects/"

	if r.Method != http.MethodPost || r.Proto != "HTTP/1.1" || r.Host != "racer" ||
		len(r.RequestURI) != len(prefix)+64 || !strings.HasPrefix(r.RequestURI, prefix) ||
		r.Header.Get("Content-Length") != "0" || len(r.TransferEncoding) != 0 || forbiddenHeaders(r.Header) {
		return s, bad
	}

	for _, name := range []string{"Content-Length", "If-Match", "Range", "Racer-Page-Credits", "Racer-Byte-Credits", "Racer-Ordered", "Racer-Metadata", "Authorization"} {
		if len(r.Header.Values(name)) > 1 {
			return s, bad
		}
	}

	for _, credit := range []struct {
		name string
		dest *uint64
		min  uint64
		max  uint64
	}{
		{"Racer-Page-Credits", &s.pageCredits, 1, 64},
		{"Racer-Byte-Credits", &s.byteCredits, uint64(PageSize), 64 * uint64(PageSize)},
	} {
		if values, ok := r.Header[credit.name]; ok {
			n, err := decimal(values[0])
			if err != nil || n < credit.min || n > credit.max {
				return s, bad
			}

			*credit.dest = n
		}
	}

	if values, ok := r.Header["Racer-Ordered"]; ok && values[0] != "0" && values[0] != "1" {
		return s, bad
	}

	h := r.Header.Clone()
	if values, ok := h["Range"]; ok {
		s.ranged = true

		value := values[0]
		if strings.HasPrefix(value, "bytes=") && strings.HasSuffix(value, "-") {
			first, err := decimal(strings.TrimSuffix(strings.TrimPrefix(value, "bytes="), "-"))
			if err != nil {
				return s, bad
			}

			s.first = first
		} else {
			bounds, err := parseRange(value)
			if err != nil {
				return s, bad
			}

			s.first, s.end = bounds.first, bounds.last+1
		}
	}

	h.Del("Range")

	var raw bytes.Buffer
	fmt.Fprintf(&raw, "HEAD %s%s HTTP/1.1\r\nHost: racer\r\n", objectPrefix, r.RequestURI[len(prefix):])

	if err := h.Write(&raw); err != nil {
		return s, err
	}

	raw.WriteString("\r\n")
	request, err := parseRequestHead(raw.Bytes(), false)
	s.request = request

	return s, err
}

type fakeSubscriptionCredits struct {
	mu          sync.Mutex
	outstanding map[uint64]uint32
	bytes       uint64
	changed     chan struct{}
}

func (c *fakeSubscriptionCredits) releases(reader io.Reader, cancel context.CancelFunc) {
	defer cancel()

	var release [12]byte

	for {
		if _, err := io.ReadFull(reader, release[:]); err != nil {
			return
		}

		page, length := binary.BigEndian.Uint64(release[:8]), binary.BigEndian.Uint32(release[8:])

		c.mu.Lock()

		want, ok := c.outstanding[page]
		if !ok || want != length {
			c.mu.Unlock()
			return
		}

		delete(c.outstanding, page)
		c.bytes -= uint64(length)
		close(c.changed)
		c.changed = make(chan struct{})
		c.mu.Unlock()
	}
}

func (c *fakeSubscriptionCredits) reserve(ctx context.Context, s fakeSubscriptionRequest, page uint64, length uint32) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		c.mu.Lock()
		if uint64(len(c.outstanding)) < s.pageCredits && c.bytes+uint64(length) <= s.byteCredits {
			c.outstanding[page] = length
			c.bytes += uint64(length)
			c.mu.Unlock()

			return nil
		}

		changed := c.changed
		c.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func fakeSubscriptionHead(w io.Writer, status int, h http.Header) error {
	h.Set("Connection", "close")

	if _, err := fmt.Fprintf(w, "HTTP/1.1 %d %s\r\n", status, http.StatusText(status)); err != nil {
		return err
	}

	if err := h.Write(w); err != nil {
		return err
	}

	_, err := io.WriteString(w, "\r\n")

	return err
}

func fakeSubscriptionFrame(w io.Writer, kind byte, page, offset uint64, length uint32) error {
	var frame [21]byte

	frame[0] = kind
	binary.BigEndian.PutUint64(frame[1:9], page)
	binary.BigEndian.PutUint64(frame[9:17], offset)
	binary.BigEndian.PutUint32(frame[17:], length)
	_, err := w.Write(frame[:])

	return err
}

func serveFakeSubscription(w http.ResponseWriter, r *http.Request, transport *http.Transport) {
	s, err := parseFakeSubscription(r)
	if err != nil {
		writeOriginErrorResponse(w, 400, Metadata{})
		return
	}

	// Release records follow a CL=0 HTTP head, not an HTTP request body. Hijack
	// before origin work so disconnects cancel even a pending origin callback.
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer closeBody(conn)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	unhook := context.AfterFunc(ctx, func() { closeBody(conn) })
	defer unhook()

	credits := &fakeSubscriptionCredits{outstanding: make(map[uint64]uint32), changed: make(chan struct{})}
	go credits.releases(rw.Reader, cancel)

	page := s.request
	if page.pin.value == "" && s.first == 0 {
		page.operation, page.byteRange = OperationBootstrap, bootstrapRange()
	}

	res, response, err := fakePage(ctx, transport, page, nil)
	if res != nil {
		defer closeBody(res.Body)
	}

	if err != nil {
		status, h := 502, http.Header{"Content-Length": {"0"}}

		var typed *Error
		if res != nil && errors.As(err, &typed) && typed.StatusCode() != 0 {
			status, h = res.StatusCode, res.Header.Clone()
		}

		if fakeSubscriptionHead(rw, status, h) != nil || rw.Flush() != nil {
			return
		}

		return
	}

	snapshot := response.metadata

	end := min(s.end, uint64(snapshot.Size))
	if s.ranged && s.first >= uint64(snapshot.Size) {
		h := http.Header{"Content-Length": {"0"}, "Content-Range": {"bytes */" + strconv.FormatUint(uint64(snapshot.Size), 10)}}
		if fakeSubscriptionHead(rw, 416, h) != nil || rw.Flush() != nil {
			return
		}

		return
	}

	pages := uint64(0)
	if end > s.first {
		pages = (end-1)/uint64(PageSize) - s.first/uint64(PageSize) + 1
	}
	// Like the real listener, acquire the first selected slice before committing
	// success headers. In particular, a first-page 401 must remain a 401.
	if pages != 0 && page.operation != OperationBootstrap {
		closeBody(res.Body)

		page.operation, page.pin = OperationPinned, snapshot.ETag
		page.byteRange = Range{present: true, first: s.first / uint64(PageSize) * uint64(PageSize), last: nominalPageEnd(s.first / uint64(PageSize) * uint64(PageSize))}

		res, response, err = fakePage(ctx, transport, page, &snapshot)
		if err != nil {
			status, headers := 502, http.Header{"Content-Length": {"0"}}

			var typed *Error

			if res != nil {
				defer closeBody(res.Body)

				if errors.As(err, &typed) && typed.StatusCode() != 0 {
					status, headers = res.StatusCode, res.Header.Clone()
				}
			}

			if fakeSubscriptionHead(rw, status, headers) != nil || rw.Flush() != nil {
				return
			}

			return
		}
		defer closeBody(res.Body)
	}

	length := end - s.first
	if pages+1 > (math.MaxInt64-length)/21 {
		if fakeSubscriptionHead(rw, 502, http.Header{"Content-Length": {"0"}}) != nil || rw.Flush() != nil {
			return
		}

		return
	}

	h, err := metadataHeaders(snapshot)
	if err != nil {
		return
	}

	h.Set("Content-Type", "application/octet-stream")
	h.Set("Racer-Object-Length", strconv.FormatUint(uint64(snapshot.Size), 10))
	h.Set("Racer-Range-Start", strconv.FormatUint(s.first, 10))
	h.Set("Racer-Range-End", strconv.FormatUint(end, 10))
	h.Set("Content-Length", strconv.FormatUint(length+21*(pages+1), 10))

	if fakeSubscriptionHead(rw, 200, h) != nil || rw.Flush() != nil {
		return
	}

	buffer := make([]byte, copyBufferSize)

	for first := s.first; first < end; {
		number := first / uint64(PageSize)
		pageEnd := min(nominalPageEnd(number*uint64(PageSize))+1, end)

		n := uint32(pageEnd - first)
		if credits.reserve(ctx, s, number, n) != nil {
			return
		}

		if first != s.first {
			page.operation, page.pin = OperationPinned, snapshot.ETag
			page.byteRange = Range{present: true, first: number * uint64(PageSize), last: nominalPageEnd(number * uint64(PageSize))}

			res, response, err = fakePage(ctx, transport, page, &snapshot)
			if err != nil {
				if res != nil {
					closeBody(res.Body)
				}

				return
			}
		}

		err = fakeSubscriptionFrame(rw, 1, number, first, n)
		if err == nil {
			_, err = io.CopyN(io.Discard, res.Body, int64(first-uint64(response.first)))
		}

		if err == nil {
			var copied int64

			copied, err = io.CopyBuffer(rw, io.LimitReader(res.Body, int64(n)), buffer)
			if err == nil && copied != int64(n) {
				err = io.ErrUnexpectedEOF
			}
		}

		if err == nil {
			_, err = io.CopyBuffer(io.Discard, res.Body, buffer)
		}

		closeBody(res.Body)

		if err != nil || rw.Flush() != nil {
			return
		}

		page.operation = OperationPinned
		first = pageEnd
	}

	if fakeSubscriptionFrame(rw, 2, pages, length, 0) != nil || rw.Flush() != nil {
		return
	}
}

// fakePage uses the wire validators as well as the actual origin server. The
// response head is reconstructed from net/http only on this private fake hop;
// Client still validates raw bytes on its ordinary direct streaming path.
func fakePage(ctx context.Context, transport *http.Transport, request OriginRequest, snapshot *Metadata) (*http.Response, wireResponse, error) {
	if err := validateRequest(request); err != nil {
		return nil, wireResponse{}, err
	}

	method := http.MethodGet
	if request.operation == OperationHead {
		method = http.MethodHead
	}

	req, err := http.NewRequestWithContext(ctx, method, "http://racer"+objectPrefix+request.key.String(), nil)
	if err != nil {
		return nil, wireResponse{}, err
	}

	req.Header = requestHeaders(request)
	req.Header["User-Agent"] = nil

	res, err := transport.RoundTrip(req)
	if err != nil {
		return nil, wireResponse{}, err
	}

	var raw bytes.Buffer
	raw.WriteString("HTTP/1.1 " + res.Status + "\r\n")

	if err := res.Header.Write(&raw); err != nil {
		closeBody(res.Body)
		return nil, wireResponse{}, err
	}

	raw.WriteString("\r\n")
	response, err := parseResponseHead(raw.Bytes(), request, snapshot)

	return res, response, err
}

func serveFakePages(w http.ResponseWriter, r *http.Request, transport *http.Transport) {
	// Reconstruct a canonical descriptor from the real Client's HTTP request.
	var raw bytes.Buffer
	raw.WriteString(r.Method + " " + r.RequestURI + " HTTP/1.1\r\nHost: " + r.Host + "\r\n")

	if err := r.Header.Write(&raw); err != nil {
		writeOriginErrorResponse(w, 400, Metadata{})
		return
	}

	raw.WriteString("\r\n")

	request, err := parseRequestHead(raw.Bytes(), false)
	if err != nil {
		writeOriginErrorResponse(w, 400, Metadata{})
		return
	}

	page := request
	if page.operation == OperationPinned {
		page.byteRange.first = page.byteRange.first / uint64(PageSize) * uint64(PageSize)
		page.byteRange.last = nominalPageEnd(page.byteRange.first)
	}

	var snapshot *Metadata

	buffer := make([]byte, copyBufferSize)

	for {
		res, response, err := fakePage(r.Context(), transport, page, snapshot)
		if err != nil {
			if res != nil {
				defer closeBody(res.Body)
			}

			if snapshot != nil {
				panic(http.ErrAbortHandler)
			}

			var typed *Error
			if res != nil && errors.As(err, &typed) && typed.StatusCode() != 0 {
				for name, values := range res.Header {
					w.Header()[name] = values
				}

				w.WriteHeader(res.StatusCode)
			} else {
				writeOriginErrorResponse(w, 502, Metadata{})
			}

			return
		}

		if snapshot == nil {
			for name, values := range res.Header {
				w.Header()[name] = values
			}

			if request.operation == OperationPinned {
				first, last, err := request.byteRange.resolve(response.metadata.Size)
				if err != nil {
					closeBody(res.Body)
					writeOriginErrorResponse(w, 502, Metadata{})

					return
				}

				cr, err := contentRangeValue(first, last, response.metadata.Size)
				if err != nil {
					closeBody(res.Body)
					writeOriginErrorResponse(w, 502, Metadata{})

					return
				}

				w.Header().Set("Content-Range", cr)
				w.Header().Set("Content-Length", strconv.FormatUint(uint64(last-first)+1, 10))
			}

			w.WriteHeader(res.StatusCode)

			if err := http.NewResponseController(w).Flush(); err != nil {
				closeBody(res.Body)
				panic(http.ErrAbortHandler)
			}

			snapshot = &response.metadata
		}

		if request.operation == OperationHead {
			closeBody(res.Body)
			return
		}

		if request.operation == OperationPinned {
			first, last := max(uint64(response.first), request.byteRange.first), min(uint64(response.last), request.byteRange.last)

			_, err = io.CopyN(io.Discard, res.Body, int64(first-uint64(response.first)))
			if err == nil {
				_, err = io.CopyBuffer(w, io.LimitReader(res.Body, int64(last-first+1)), buffer)
			}

			if err == nil {
				_, err = io.CopyBuffer(io.Discard, res.Body, buffer)
			}
		} else {
			_, err = io.CopyBuffer(w, res.Body, buffer)
		}

		closeBody(res.Body)

		if err != nil {
			panic(http.ErrAbortHandler)
		}

		if err := http.NewResponseController(w).Flush(); err != nil {
			panic(http.ErrAbortHandler)
		}

		if request.operation == OperationBootstrap || uint64(response.last) >= request.byteRange.last || ByteLength(response.last)+1 >= snapshot.Size {
			return
		}

		page.byteRange.first = uint64(response.last) + 1
		page.byteRange.last = nominalPageEnd(page.byteRange.first)
	}
}
