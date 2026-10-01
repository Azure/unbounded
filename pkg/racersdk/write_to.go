// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"io"
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
// GetStreaming Values are not supported; use WriteToHTTP for those Values.
// HEAD returns metadata without consuming the subscription or requiring Complete.
func (v *Value) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer closeBody(v)

	if v.streaming || v.offset != 0 || v.end != int64(v.metadata.Size) || v.err() != nil {
		http.Error(w, "value unavailable", http.StatusServiceUnavailable)
		return
	}
	// An empty response has no final byte to withhold. Validate Complete before
	// any success headers or connection ownership transfer, except for HEAD.
	if v.metadata.Size == 0 && r.Method != http.MethodHead {
		var probe [1]byte
		if n, err := v.Read(probe[:]); n != 0 || err != io.EOF {
			http.Error(w, "value unavailable", http.StatusBadGateway)
			return
		}
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
				panic(http.ErrAbortHandler)
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

	tracked := &valueResponseWriter{ResponseWriter: w}
	if _, err := v.WriteTo(tracked); err != nil {
		if tracked.committed {
			panic(http.ErrAbortHandler)
		}

		w.Header().Del("Content-Length")
		w.Header().Del("Content-Type")
		w.Header().Del("ETag")
		http.Error(w, "value unavailable", http.StatusBadGateway)
	}
}

type valueResponseWriter struct {
	http.ResponseWriter
	committed bool
}

func (w *valueResponseWriter) Write(p []byte) (int, error) {
	w.committed = true
	return w.ResponseWriter.Write(p)
}
