// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package fakeracer implements a sequential, noncaching v2 Racer test daemon.
// It fetches whole pages from a v1 origin and is not a compatibility oracle for
// the real Racer runtime. Socket ownership belongs to the caller.
package fakeracer

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/Azure/unbounded/pkg/racersdk/internal/wire"
)

// NewHandler serves v2 HEAD and subscriptions through transport, which must
// reach a v1 origin at http://racer. The caller owns transport and server lifetime,
// including canceling request contexts to close hijacked subscription sockets.
func NewHandler(transport http.RoundTripper) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasPrefix(r.RequestURI, wire.ClientObjectPrefix) {
			serveSubscription(w, r, transport)
			return
		}

		serveHead(w, r, transport)
	})
}

// Credits tracks outstanding slices for a subscription. Use NewCredits; its
// methods are safe concurrently. Exported for SDK white-box protocol fixtures.
type Credits struct {
	mu          sync.Mutex
	outstanding map[uint64]uint32
	bytes       uint64
	changed     chan struct{}
}

// NewCredits returns an empty credit ledger.
func NewCredits() *Credits {
	return &Credits{outstanding: make(map[uint64]uint32), changed: make(chan struct{})}
}

// Releases reads credit records until EOF or an invalid release, then cancels.
// The caller must close reader to unblock this method during shutdown.
func (c *Credits) Releases(reader io.Reader, cancel context.CancelFunc) {
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

// Reserve waits for both page and byte credit before recording a selected slice.
func (c *Credits) Reserve(ctx context.Context, s wire.SubscriptionRequest, page uint64, length uint32) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		c.mu.Lock()
		if uint64(len(c.outstanding)) < s.PageCredits && c.bytes+uint64(length) <= s.ByteCredits {
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

func serveSubscription(w http.ResponseWriter, r *http.Request, transport http.RoundTripper) {
	s, err := wire.ParseSubscriptionRequest(r)
	if err != nil {
		writeError(w, 400)
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

	credits := NewCredits()
	released := make(chan struct{})

	go func() { defer close(released); credits.Releases(rw.Reader, cancel) }()

	defer func() { closeBody(conn); <-released }()

	page := s.Request
	if page.Pin == "" && s.First == 0 {
		page.Operation, page.Range = wire.OperationBootstrap, wire.Range{Present: true, Last: wire.PageSize - 1}
	}

	res, response, err := fetchPage(ctx, transport, page, nil)
	if res != nil {
		defer closeBody(res.Body)
	}

	if err != nil {
		status, h := 502, http.Header{"Content-Length": {"0"}}

		var typed *wire.Error
		if res != nil && errors.As(err, &typed) && typed.Status != 0 {
			status, h = res.StatusCode, res.Header.Clone()
		}

		if wire.WriteSubscriptionHead(rw, status, h) != nil || rw.Flush() != nil {
			return
		}

		return
	}

	snapshot := response.Metadata

	end := min(s.End, snapshot.Size)
	if s.Ranged && s.First >= snapshot.Size {
		h := http.Header{"Content-Length": {"0"}, "Content-Range": {"bytes */" + strconv.FormatUint(uint64(snapshot.Size), 10)}}
		if wire.WriteSubscriptionHead(rw, 416, h) != nil || rw.Flush() != nil {
			return
		}

		return
	}

	pages := wire.PageCount(s.First, end)
	// Like the real listener, acquire the first selected slice before committing
	// success headers. In particular, a first-page 401 must remain a 401.
	if pages != 0 && page.Operation != wire.OperationBootstrap {
		closeBody(res.Body)

		page.Operation, page.Pin = wire.OperationPinned, snapshot.ETag
		page.Range = wire.Range{Present: true, First: s.First / wire.PageSize * wire.PageSize, Last: wire.NominalPageEnd(s.First / wire.PageSize * wire.PageSize)}

		res, response, err = fetchPage(ctx, transport, page, &snapshot)
		if err != nil {
			status, headers := 502, http.Header{"Content-Length": {"0"}}

			var typed *wire.Error

			if res != nil {
				defer closeBody(res.Body)

				if errors.As(err, &typed) && typed.Status != 0 {
					status, headers = res.StatusCode, res.Header.Clone()
				}
			}

			if wire.WriteSubscriptionHead(rw, status, headers) != nil || rw.Flush() != nil {
				return
			}

			return
		}
		defer closeBody(res.Body)
	}

	length := end - s.First

	h, err := wire.SubscriptionHeaders(snapshot, s.First, end)
	if err != nil {
		var typed *wire.Error
		if errors.As(err, &typed) && typed.Kind == wire.ErrorProtocol {
			if wire.WriteSubscriptionHead(rw, 502, http.Header{"Content-Length": {"0"}}) != nil || rw.Flush() != nil {
				return
			}
		}

		return
	}

	if wire.WriteSubscriptionHead(rw, 200, h) != nil || rw.Flush() != nil {
		return
	}

	buffer := make([]byte, 256*1024)
	sequence := wire.NewSequence(s.First, end, true)

	for first := s.First; first < end; {
		number := first / wire.PageSize
		pageEnd := min(wire.NominalPageEnd(number*wire.PageSize)+1, end)

		n := uint32(pageEnd - first)
		if credits.Reserve(ctx, s, number, n) != nil {
			return
		}

		if first != s.First {
			page.Operation, page.Pin = wire.OperationPinned, snapshot.ETag
			page.Range = wire.Range{Present: true, First: number * wire.PageSize, Last: wire.NominalPageEnd(number * wire.PageSize)}

			res, response, err = fetchPage(ctx, transport, page, &snapshot)
			if err != nil {
				if res != nil {
					closeBody(res.Body)
				}

				return
			}
		}

		err = sequence.Write(rw, wire.Frame{Kind: wire.PageFrame, Number: number, Offset: first, Length: n})
		if err == nil {
			_, err = io.CopyN(io.Discard, res.Body, int64(first-response.First))
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

		page.Operation = wire.OperationPinned
		first = pageEnd
	}

	if sequence.Write(rw, wire.Frame{Kind: wire.CompleteFrame, Number: pages, Offset: length}) != nil || rw.Flush() != nil {
		return
	}
}

// fetchPage uses the wire validators as well as the actual origin server. The
// response head is reconstructed from net/http only on this private fake hop;
// Client still validates raw bytes on its ordinary direct streaming path.
func fetchPage(ctx context.Context, transport http.RoundTripper, request wire.Request, snapshot *wire.Metadata) (*http.Response, wire.Response, error) {
	if err := wire.ValidateRequest(request); err != nil {
		return nil, wire.Response{}, err
	}

	method := http.MethodGet
	if request.Operation == wire.OperationHead {
		method = http.MethodHead
	}

	req, err := http.NewRequestWithContext(ctx, method, "http://racer"+wire.ObjectPrefix+hex.EncodeToString(request.Key[:]), nil)
	if err != nil {
		return nil, wire.Response{}, err
	}

	req.Header = wire.RequestHeaders(request)
	req.Header["User-Agent"] = nil

	res, err := transport.RoundTrip(req)
	if err != nil {
		return nil, wire.Response{}, err
	}

	var raw bytes.Buffer
	raw.WriteString("HTTP/1.1 " + res.Status + "\r\n")

	if err := res.Header.Write(&raw); err != nil {
		closeBody(res.Body)
		return nil, wire.Response{}, err
	}

	raw.WriteString("\r\n")
	response, err := wire.ParseResponseHead(raw.Bytes(), request, snapshot)

	return res, response, err
}

func serveHead(w http.ResponseWriter, r *http.Request, transport http.RoundTripper) {
	descriptor, err := wire.ParseClientHead(r)
	if err != nil {
		writeError(w, 400)
		return
	}

	res, _, err := fetchPage(r.Context(), transport, descriptor, nil)
	if res != nil {
		defer closeBody(res.Body)
	}

	var typed *wire.Error
	if err != nil && (res == nil || !errors.As(err, &typed) || typed.Status == 0) {
		writeError(w, 502)
		return
	}

	for name, values := range res.Header {
		w.Header()[name] = values
	}

	w.WriteHeader(res.StatusCode)
}

func closeBody(body io.Closer) {
	if body != nil {
		if err := body.Close(); err != nil {
			return
		}
	}
}

func writeError(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(status)
}
