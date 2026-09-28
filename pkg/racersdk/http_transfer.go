// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"io"
	"net"
	"net/http"
)

// WriteToHTTP delivers a Value through the server's normal response lifecycle.
// The caller sets headers, including the selected range's Content-Length, and
// owns Close. Unlike WriteTo, this explicitly trusts the HTTP writer's ReadFrom
// implementation. It must consume synchronously without retaining the reader.
// Wrappers must preserve framing, accounting, cancellation and write deadlines.
// net/http can splice a bounded Unix source to a plaintext TCP response on Linux,
// while TLS, HTTP/2 and writers without ReadFrom retain ordinary copying. No
// connection is hijacked: keep-alive, ranges and server shutdown remain intact.
func (v *Value) WriteToHTTP(w http.ResponseWriter) (int64, error) {
	return v.writeTo(w, true)
}

func (v *Value) writeHTTPBody(w io.Writer) (int64, bool, error) {
	rf, ok := w.(io.ReaderFrom)
	if !ok {
		return 0, false, nil
	}

	v.mu.Lock()
	body, ok := v.body.(*responseBody)
	terminal := v.terminal
	v.mu.Unlock()

	if terminal != nil {
		return 0, true, terminal
	}

	if !ok || body.conn.reader.Buffered() != 0 {
		return 0, false, nil
	}
	// Never expose a pooled wrapper or an unbounded connection. The concrete
	// UnixConn is required by net's splice implementation, and the limiter keeps
	// net/http's final EOF probe away from the next HTTP exchange.
	source, ok := body.conn.Conn.(*net.UnixConn)
	if !ok {
		return 0, false, nil
	}

	// Bound each raw transfer so its read deadline is renewed between chunks.
	// Clear the deadline before returning control to a potentially slow caller.
	reader := &io.LimitedReader{R: source, N: min(v.remaining, 256*1024)}
	before := reader.N

	if err := body.beginRead(); err != nil {
		v.finish(ioFailure("HTTP transfer deadline", err))
		return 0, true, v.err()
	}

	n, err := rf.ReadFrom(reader)
	if clearErr := body.endRead(); err == nil {
		err = clearErr
	}

	consumed := before - reader.N

	body.mu.Lock()
	body.remaining -= consumed
	body.mu.Unlock()
	body.client.stats.bytesRead.Add(uint64(consumed))
	v.offset += consumed

	v.remaining -= consumed
	if err == nil && (consumed != before || n != consumed) {
		err = io.ErrUnexpectedEOF
	}

	if err != nil {
		v.finish(ioFailure("HTTP transfer", err))
		return n, true, v.err()
	}

	return n, true, nil
}
