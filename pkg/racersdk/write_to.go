// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"net"
	"net/http"
	"strconv"
)

// FDSink explicitly permits direct body delivery to a stream connection. The
// caller owns the connection and its framing; it must not write concurrently
// or change its write deadline during WriteTo.
// Use ordinary writers for TLS or HTTP ResponseWriters, whose framing and
// encryption cannot be bypassed. Context cancellation interrupts blocked writes.
type FDSink struct {
	connection net.Conn
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

// SplicedBytes is zero: subscription frames require page validation before copy.
func (s *FDSink) SplicedBytes() int64 { return 0 }

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
