// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"io"
	"net"
	"sync"
)

// limitRacerListener retains TCP ReaderFrom for net/http's Unix-to-TCP splice
// path. Embedding only net.Conn, as netutil.LimitListener does, hides that method.
func limitRacerListener(listener net.Listener, limit int) net.Listener {
	return &racerLimitedListener{Listener: listener, slots: make(chan struct{}, limit), done: make(chan struct{})}
}

type racerLimitedListener struct {
	net.Listener
	slots     chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

func (l *racerLimitedListener) Accept() (net.Conn, error) {
	select {
	case <-l.done:
		return nil, net.ErrClosed
	case l.slots <- struct{}{}:
	}

	c, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}

	select {
	case <-l.done:
		err := c.Close()

		<-l.slots

		return nil, errors.Join(net.ErrClosed, err)
	default:
	}

	limited := &racerLimitedConn{Conn: c, release: func() { <-l.slots }}
	if reader, ok := c.(io.ReaderFrom); ok {
		return &racerLimitedReaderConn{racerLimitedConn: limited, reader: reader}, nil
	}

	return limited, nil
}

func (l *racerLimitedListener) Close() error {
	l.closeOnce.Do(func() { close(l.done) })
	return l.Listener.Close()
}

type racerLimitedConn struct {
	net.Conn
	releaseOnce sync.Once
	release     func()
}

func (c *racerLimitedConn) Close() error {
	err := c.Conn.Close()
	c.releaseOnce.Do(c.release)

	return err
}

type racerLimitedReaderConn struct {
	*racerLimitedConn
	reader io.ReaderFrom
}

func (c *racerLimitedReaderConn) ReadFrom(r io.Reader) (int64, error) {
	return c.reader.ReadFrom(r)
}
