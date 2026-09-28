// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"net"
	"net/http"
	"strconv"
)

// FDSink explicitly permits direct body delivery to a stream connection. The
// caller owns the connection and its framing; it must not write concurrently.
// Use ordinary writers for TLS or HTTP ResponseWriters, whose framing and
// encryption cannot be bypassed. Context cancellation interrupts blocked writes.
type FDSink struct {
	connection net.Conn
	spliced    int64
}

func NewFDSink(connection net.Conn) (*FDSink, error) {
	switch connection.(type) {
	case *net.TCPConn, *net.UnixConn:
		return &FDSink{connection: connection}, nil
	default:
		return nil, failure(ErrorInvalidArgument, "FD sink", nil)
	}
}

func (s *FDSink) Write(p []byte) (int, error) { return s.connection.Write(p) }

// SplicedBytes reports bytes delivered by actual kernel splice calls. Access it
// only after WriteTo returns; it is not a concurrent metric.
func (s *FDSink) SplicedBytes() int64 { return s.spliced }

// ServeHTTP serves an unconsumed full-object Value as a fixed-length HTTP response. For plain
// HTTP/1 it explicitly takes connection ownership through Hijack, flushes headers,
// and closes the connection after FD delivery. TLS and non-hijackable responses
// use the normal ResponseWriter lifecycle. The handler owns and closes the Value.
func (v *Value) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer closeBody(v)

	if v.offset != 0 || v.end != int64(v.metadata.Size) || v.err() != nil {
		http.Error(w, "value unavailable", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Length", strconv.FormatUint(uint64(v.metadata.Size), 10))

	contentType := v.metadata.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("ETag", v.metadata.ETag.String())

	if r.Method == http.MethodHead {
		return
	}

	if r.TLS == nil && r.ProtoMajor == 1 {
		if hijacker, ok := w.(http.Hijacker); ok {
			w.Header().Set("Connection", "close")
			w.WriteHeader(http.StatusOK)

			connection, buffered, err := hijacker.Hijack()
			if err != nil {
				return
			}
			defer closeBody(connection)

			if err := buffered.Flush(); err != nil {
				return
			}

			sink, err := NewFDSink(connection)
			if err != nil {
				return
			}

			if _, err := v.WriteTo(sink); err != nil {
				return
			}

			return
		}
	}

	if _, err := v.WriteTo(w); err != nil {
		return
	}
}

// spliceTo bypasses only a fully parsed body with no buffered read-ahead. The
// custom pool's body counter observes every consumed byte, allowing safe reuse.
func (v *Value) spliceTo(sink *FDSink) (int64, bool, error) {
	v.mu.Lock()
	body, ok := v.body.(*responseBody)
	terminal := v.terminal
	v.mu.Unlock()

	if terminal != nil {
		return 0, false, terminal
	}

	if !ok || body.conn.reader.Buffered() != 0 {
		return 0, false, nil
	}

	n, used, err := spliceBody(v.ctx, body, sink, v.remaining)
	if used {
		v.offset += n
		v.remaining -= n
	}

	if err != nil {
		body.mu.Lock()
		body.reusable = false
		body.mu.Unlock()
		v.finish(ioFailure("splice", err))

		return n, used, v.err()
	}

	return n, used, nil
}
