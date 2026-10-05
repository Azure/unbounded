// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/Azure/unbounded/pkg/racersdk/internal/connpool"
	"github.com/Azure/unbounded/pkg/racersdk/internal/wire"
)

// NewFakeClient returns a real Client backed by a sequential, noncaching fake
// Racer and the SDK's real origin validation/serving machinery. It is a test
// helper, not a Racer implementation or evidence of real Racer compatibility.
// It uses private loopback listeners, requires no /run provisioning, and adds no
// production endpoint overrides. Client and origin resource defaults apply.
//
// The fake forwards request metadata and authorization unchanged. It supports
// v2 HEAD and credit-controlled subscriptions, forwarding pinned continuations
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
		if r.Method == http.MethodPost && strings.HasPrefix(r.RequestURI, clientObjectPrefix) {
			serveFakeSubscription(w, r, transport)
			return
		}

		serveFakeHead(w, r, transport)
	}), false)
	if err != nil {
		cleanup()
		return nil, nil, err
	}

	client.configurePools(connpool.Config{Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp4", address)
	}})

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
	s, err := wire.ParseSubscriptionRequest(r)
	return fakeSubscriptionRequest{request: fromWireRequest(s.Request), first: s.First, end: s.End, ranged: s.Ranged, pageCredits: s.PageCredits, byteCredits: s.ByteCredits}, fromWireError(err)
}

type fakeSubscriptionCredits struct {
	mu          sync.Mutex
	outstanding map[uint64]uint32
	bytes       uint64
	changed     chan struct{}
}

func (c *fakeSubscriptionCredits) releases(reader io.Reader, cancel context.CancelFunc) {
	defer cancel()

	var release [wire.CreditSize]byte

	for {
		if _, err := io.ReadFull(reader, release[:]); err != nil {
			return
		}

		credit := wire.DecodeCredit(release)
		page, length := credit.Number, credit.Length

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
	return wire.WriteSubscriptionHead(w, status, h)
}

func fakeSubscriptionFrame(w io.Writer, kind byte, page, offset uint64, length uint32) error {
	return wire.WriteFrame(w, wire.Frame{Kind: kind, Number: page, Offset: offset, Length: length})
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

	pages := wire.PageCount(s.first, end)
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

	h, err := wire.SubscriptionHeaders(snapshot.wire(), s.first, end)
	if err != nil {
		var typed *wire.Error
		if errors.As(err, &typed) && typed.Kind == wire.ErrorProtocol {
			if fakeSubscriptionHead(rw, 502, http.Header{"Content-Length": {"0"}}) != nil || rw.Flush() != nil {
				return
			}
		}

		return
	}

	if fakeSubscriptionHead(rw, 200, h) != nil || rw.Flush() != nil {
		return
	}

	buffer := make([]byte, copyBufferSize)
	sequence := wire.NewSequence(s.first, end, true)

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

		err = sequence.Write(rw, wire.Frame{Kind: wire.PageFrame, Number: number, Offset: first, Length: n})
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

	if sequence.Write(rw, wire.Frame{Kind: wire.CompleteFrame, Number: pages, Offset: length}) != nil || rw.Flush() != nil {
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

func serveFakeHead(w http.ResponseWriter, r *http.Request, transport *http.Transport) {
	descriptor, err := wire.ParseClientHead(r)
	if err != nil {
		writeOriginErrorResponse(w, 400, Metadata{})
		return
	}

	res, _, err := fakePage(r.Context(), transport, fromWireRequest(descriptor), nil)
	if res != nil {
		defer closeBody(res.Body)
	}

	var typed *Error
	if err != nil && (res == nil || !errors.As(err, &typed) || typed.StatusCode() == 0) {
		writeOriginErrorResponse(w, 502, Metadata{})
		return
	}

	for name, values := range res.Header {
		w.Header()[name] = values
	}

	w.WriteHeader(res.StatusCode)
}
