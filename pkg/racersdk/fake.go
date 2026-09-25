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
	"sync"
)

// NewFakeClient returns a real Client backed by a sequential, noncaching fake
// Racer and the SDK's real origin validation/serving machinery. It is a test
// helper, not a Racer implementation or evidence of real Racer compatibility.
// It uses private loopback listeners, requires no /run provisioning, and adds no
// production endpoint overrides. Client and origin resource defaults apply.
//
// The fake forwards request metadata and authorization unchanged. Each Get opens
// a fresh bootstrap; its lazy pinned continuation is split into whole-page origin
// requests, streamed sequentially without object-sized buffering. Origin callback
// errors before response headers retain their HTTP classification; later errors
// abort the stream. Origin must obey the same cancellation/body ownership contract
// as ServeOrigin.
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

	originAddress, err := start(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveOperation(w, r, config, origin, slots)
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

// fakePage uses the wire validators as well as the actual origin server. The
// response head is reconstructed from net/http only on this private fake hop;
// Client still validates raw bytes on its ordinary responseConn path.
func fakePage(ctx context.Context, transport *http.Transport, request OriginRequest, snapshot *Metadata) (*http.Response, wireResponse, error) {
	if err := validateRequest(request); err != nil {
		return nil, wireResponse{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://racer"+objectPrefix+request.key.String(), nil)
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
	if err != nil || request.operation == OperationHead {
		writeOriginErrorResponse(w, 400, Metadata{})
		return
	}

	page := request
	if page.operation == OperationPinned {
		page.byteRange.last = min(page.byteRange.last, nominalPageEnd(page.byteRange.first))
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

		_, err = io.CopyBuffer(w, res.Body, buffer)
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
		page.byteRange.last = min(request.byteRange.last, nominalPageEnd(page.byteRange.first))
	}
}
