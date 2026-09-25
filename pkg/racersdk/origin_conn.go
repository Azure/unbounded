// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

type originHead struct {
	request OriginRequest
	err     error
	at      time.Time
}

type originConn struct {
	net.Conn
	config  OriginConfig
	release func()
	once    sync.Once
	reader  *bufio.Reader
	head    []byte
	first   bool
	mu      sync.Mutex
	pending []originHead
	failed  bool
}

func (c *originConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)

	return err
}

// Only validated canonical requests reach net/http. Unknown fields are discarded
// after counting toward raw limits. Malformed requests become a private error
// operation so the handler, rather than net/http's text error writer, sends the
// empty response. All reads retain this same reader, including background reads
// used by net/http to detect peer disconnects.
func (c *originConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	if len(c.head) == 0 {
		if c.failed {
			return 0, io.EOF
		}

		if c.reader == nil {
			c.reader = bufio.NewReader(c.Conn)
		}

		first := !c.first
		if first {
			c.first = true
			if err := c.SetReadDeadline(time.Now().Add(c.config.ReadHeaderTimeout)); err != nil {
				return 0, err
			}
		}

		if _, err := c.reader.Peek(1); err != nil {
			return 0, err
		}

		if !first {
			if err := c.SetReadDeadline(time.Now().Add(c.config.ReadHeaderTimeout)); err != nil {
				return 0, err
			}
		}

		raw, err := readRawHead(c.reader, false)
		at := time.Now()

		var request OriginRequest
		if err == nil {
			request, err = parseRequestHead(raw, true)
		}

		entry := originHead{request: request, err: err, at: at}
		if err == nil {
			c.head, err = requestHead(request)
			if err != nil {
				return 0, err
			}

			closeRequested := false

			for _, value := range headHeaders(raw).Values("Connection") {
				for _, token := range strings.Split(value, ",") {
					if strings.EqualFold(strings.TrimSpace(token), "close") {
						closeRequested = true
					}
				}
			}

			if closeRequested {
				c.head = append(c.head[:len(c.head)-2], []byte("Connection: close\r\n\r\n")...)
			}
		} else {
			c.failed = true
			c.head = []byte("HEAD / HTTP/1.1\r\nHost: racer\r\nConnection: close\r\n\r\n")
		}

		c.mu.Lock()
		// net/http permits at most one background byte read, so only the current
		// and next head can be resident, independent of peer pipelining volume.
		c.pending = append(c.pending, entry)
		c.mu.Unlock()
	}

	n := copy(p, c.head)

	c.head = c.head[n:]
	if len(c.head) == 0 {
		c.head = nil
	}

	return n, nil
}

func (c *originConn) takeHead() originHead {
	c.mu.Lock()
	defer c.mu.Unlock()

	h := c.pending[0]
	c.pending[0] = originHead{}
	c.pending = c.pending[1:]

	return h
}
