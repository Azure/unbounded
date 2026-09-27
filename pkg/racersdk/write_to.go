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

// ServeHTTP serves an unconsumed Value as a fixed-length HTTP response. For plain
// HTTP/1 it explicitly takes connection ownership through Hijack, flushes headers,
// and closes the connection after FD delivery. TLS and non-hijackable responses
// use the normal ResponseWriter lifecycle. The handler owns and closes the Value.
func (v *Value) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer closeBody(v)

	if v.offset != 0 || v.err() != nil {
		http.Error(w, "value unavailable", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Length", strconv.FormatUint(uint64(v.metadata.Size), 10))
	w.Header().Set("Content-Type", "application/octet-stream")
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

// WriteTo copies the remaining immutable stream in order. A FDSink permits Linux
// splice after parsed HTTP read-ahead has been consumed; other writers use
// bounded scratch. On a destination failure the Value is canceled and closed.
// Spliced source bodies are closed through net/http rather than returned to its
// pool, because its private body counter did not observe those bytes.
func (v *Value) WriteTo(w io.Writer) (int64, error) {
	var total int64

	buffer := make([]byte, copyBufferSize)

	for {
		if sink, ok := w.(*FDSink); ok && v.remaining > 0 && v.err() == nil {
			v.mu.Lock()
			body, ok := v.body.(*pageBody)
			v.mu.Unlock()

			if ok && body.socket != nil {
				// Transport may have consumed body bytes while parsing the head.
				// Drain those and our raw parser's read-ahead through the official
				// body reader before touching the descriptor.
				ahead := v.remaining - body.socket.remaining
				if ahead == 0 && body.socket.reader.Buffered() == 0 {
					n, used, err := spliceBody(v.ctx, body.socket, sink, v.remaining)
					if used {
						v.offset += n
						v.remaining -= n
						total += n

						if err != nil {
							if v.ctx.Err() != nil {
								err = v.ctx.Err()
							}

							v.finish(ioFailure("splice", err))

							return total, v.err()
						}

						continue
					}
				} else {
					if ahead == 0 {
						ahead = int64(body.socket.reader.Buffered())
					}

					buffer = buffer[:min(int64(cap(buffer)), ahead)]
				}
			}
		}

		n, readErr := v.Read(buffer)
		if n != 0 {
			written, err := w.Write(buffer[:n])

			total += int64(written)
			if err == nil && written != n {
				err = io.ErrShortWrite
			}

			if err != nil {
				v.finish(ioFailure("write", err))
				return total, err
			}
		}

		buffer = buffer[:cap(buffer)]

		if readErr == io.EOF {
			return total, nil
		}

		if readErr != nil {
			return total, readErr
		}
	}
}
