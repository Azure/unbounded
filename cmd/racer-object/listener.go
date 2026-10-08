// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"io"
	"net"
	"sync"
)

// limitSidecarListener retains TCP ReaderFrom for net/http's Unix-to-TCP splice
// path. Embedding only net.Conn, as netutil.LimitListener does, hides that method.
func limitSidecarListener(listener net.Listener, limit int) net.Listener {
	return &sidecarLimitedListener{Listener: listener, slots: make(chan struct{}, limit), done: make(chan struct{})}
}

type sidecarLimitedListener struct {
	net.Listener
	slots     chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

func (l *sidecarLimitedListener) Accept() (net.Conn, error) {
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

	limited := &sidecarLimitedConn{Conn: c, release: func() { <-l.slots }}
	if reader, ok := c.(io.ReaderFrom); ok {
		return &sidecarLimitedReaderConn{sidecarLimitedConn: limited, reader: reader}, nil
	}

	return limited, nil
}

func (l *sidecarLimitedListener) Close() error {
	l.closeOnce.Do(func() { close(l.done) })
	return l.Listener.Close()
}

type sidecarLimitedConn struct {
	net.Conn
	releaseOnce sync.Once
	release     func()
}

func (c *sidecarLimitedConn) Close() error {
	err := c.Conn.Close()
	c.releaseOnce.Do(c.release)

	return err
}

type sidecarLimitedReaderConn struct {
	*sidecarLimitedConn
	reader io.ReaderFrom
}

func (c *sidecarLimitedReaderConn) ReadFrom(r io.Reader) (int64, error) {
	return c.reader.ReadFrom(r)
}
