// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"errors"
	"net"
	"sync"
)

type responseState struct {
	mu       sync.Mutex
	request  OriginRequest
	snapshot *Metadata
	result   wireResponse
	err      error
}

// responseConn replays validated heads to Transport and passes fixed-length body
// bytes directly. Transport's idle read may start before the next GotConn hook;
// inspect the operation only after receiving that operation's response head.
type responseConn struct {
	net.Conn
	reader    *bufio.Reader
	head      []byte
	remaining int64
	mu        sync.Mutex
	state     *responseState
	failed    error
}

func newResponseConn(c net.Conn) *responseConn {
	return &responseConn{Conn: c, reader: bufio.NewReader(c)}
}

func (c *responseConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	if c.failed != nil {
		return 0, c.failed
	}

	if len(c.head) != 0 {
		n := copy(p, c.head)

		c.head = c.head[n:]
		if len(c.head) == 0 {
			c.head = nil
		}

		return n, nil
	}

	if c.remaining != 0 {
		if int64(len(p)) > c.remaining {
			p = p[:c.remaining]
		}

		n, err := c.reader.Read(p)
		c.remaining -= int64(n)

		return n, err
	}

	head, err := readRawHead(c.reader, true)
	if err != nil {
		var typed *Error
		if errors.As(err, &typed) && typed.Kind() == ErrorProtocol {
			return c.reject(p, err)
		}

		return 0, err
	}

	c.mu.Lock()
	state := c.state
	c.mu.Unlock()

	if state == nil {
		return 0, failure(ErrorProtocol, "unsolicited response", nil)
	}

	result, err := parseResponseHead(head, state.request, state.snapshot)

	var typed *Error
	if err != nil && (!errors.As(err, &typed) || typed.StatusCode() == 0) {
		return c.reject(p, err)
	}

	state.mu.Lock()
	state.result, state.err = result, err
	state.mu.Unlock()
	c.mu.Lock()
	c.state = nil
	c.mu.Unlock()

	c.head, c.remaining = head, result.length

	return c.Read(p)
}

// A rejected response is not a stale idle connection. Give Transport a harmless
// incomplete status prefix before the error so its zero-response-byte retry rule
// cannot replay a request whose response was actually received and rejected.
func (c *responseConn) reject(p []byte, err error) (int, error) {
	c.mu.Lock()
	state := c.state
	c.state = nil
	c.mu.Unlock()

	if state != nil {
		state.mu.Lock()
		state.err = err
		state.mu.Unlock()
	}

	c.failed = err
	p[0] = 'H'

	return 1, nil
}
