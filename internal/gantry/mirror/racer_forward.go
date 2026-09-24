// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"

	gantryracer "github.com/Azure/unbounded/internal/gantry/racer"
	sdk "github.com/Azure/unbounded/pkg/racer"
)

// racerState is present only on Racer mirrors, even when the backend is nil.
type racerState struct {
	backend              *gantryracer.Backend
	admission            chan struct{}
	manifestObservations chan struct{}
	onStream             func(sdk.TransferStats, bool, error)

	mu          sync.Mutex
	draining    bool
	connections map[net.Conn]context.CancelFunc
}

func (s *racerState) drain() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.draining = true
	for conn, cancel := range s.connections {
		cancel()

		_ = conn.Close() //nolint:errcheck // Interrupt hijacked streams during shutdown.
	}
}

func (s *racerState) register(conn net.Conn, cancel context.CancelFunc) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.draining {
		return false
	}

	if s.connections == nil {
		s.connections = make(map[net.Conn]context.CancelFunc)
	}

	s.connections[conn] = cancel

	return true
}

func (s *racerState) unregister(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.connections, conn)
}

type racerForwardOwnership uint8

const (
	racerResponseWriterOwned racerForwardOwnership = iota
	racerConnectionOwned
)

type racerForwardResult struct {
	// Once hijack succeeds, HTTP error responses are forbidden even if no headers or body
	// reached the client. Only this helper may write to or close the socket.
	ownership racerForwardOwnership
	written   int64
	err       error
}

// forward consumes a prepared stream and always closes it. It owns the complete
// hijacked connection lifecycle; the caller only reports the returned outcome.
func (s *racerState) forward(ctx context.Context, cancel context.CancelFunc, w http.ResponseWriter, stream *sdk.Stream, status int, header http.Header) racerForwardResult {
	defer cancel()
	defer stream.Close() //nolint:errcheck // Abandoned streams must release sockets.

	// Passing ResponseWriter or the buffered writer to WriteTo hides the TCP
	// socket and turns forwarding into a userspace copy. One response per
	// connection avoids maintaining a second HTTP keep-alive request parser.
	conn, buffered, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return racerForwardResult{ownership: racerResponseWriterOwned, err: err}
	}
	defer conn.Close() //nolint:errcheck // One response per hijacked connection.

	result := racerForwardResult{ownership: racerConnectionOwned}

	if !s.register(conn, cancel) {
		result.err = errors.New("mirror: Racer is draining")
		return result
	}
	defer s.unregister(conn)

	deadline, _ := ctx.Deadline()
	if result.err = conn.SetDeadline(deadline); result.err != nil {
		return result
	}

	stopWatcher := watchRacerDisconnect(conn, buffered.Reader, cancel)
	defer stopWatcher()

	_, result.err = fmt.Fprintf(buffered, "HTTP/1.1 %d %s\r\n", status, http.StatusText(status))
	if result.err == nil {
		result.err = header.Write(buffered)
	}

	if result.err == nil {
		_, result.err = buffered.WriteString("\r\n")
	}

	if result.err == nil {
		result.err = buffered.Flush()
	}

	if result.err == nil {
		result.written, result.err = stream.WriteTo(conn)
	}

	return result
}

// Hijack disables net/http's disconnect monitoring. Drain the hijacker's reader
// (including any buffered pipelined requests) until EOF, then cancel upstream.
// Connection: close means these bytes never represent another served request.
// No response payload passes through this watcher; WriteTo still gets raw conn.
func watchRacerDisconnect(conn net.Conn, reader *bufio.Reader, cancel context.CancelFunc) func() {
	done := make(chan struct{})

	go func() {
		defer close(done)

		var scratch [1024]byte
		for {
			if _, err := reader.Read(scratch[:]); err != nil {
				cancel()
				return
			}
		}
	}()

	return func() {
		_ = conn.Close() //nolint:errcheck // Wake and join the watcher before releasing the connection.

		<-done
	}
}
