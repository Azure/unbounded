// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

func listenOrigin(path string, mode os.FileMode) (*net.UnixListener, func(), error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || len(path) > socketPathLimit {
		return nil, nil, failure(ErrorInvalidArgument, "socket path", nil)
	}

	parent := string(filepath.Separator)

	parts := strings.Split(strings.TrimPrefix(path, parent), parent)
	for _, part := range parts[:len(parts)-1] {
		parent = filepath.Join(parent, part)

		info, err := os.Lstat(parent)
		if err != nil {
			return nil, nil, ioFailure("socket directory", err)
		}

		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, nil, failure(ErrorInvalidArgument, "socket directory", nil)
		}
	}

	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		if err == nil {
			err = os.ErrExist
		}

		return nil, nil, ioFailure("socket exists", err)
	}

	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, nil, ioFailure("socket bind", err)
	}

	l.SetUnlinkOnClose(false)

	info, err := os.Lstat(path)
	if err != nil {
		closeBody(l)
		return nil, nil, ioFailure("socket identity", err)
	}

	cleanup := func() {
		closeBody(l)

		current, err := os.Lstat(path)
		if err == nil && current.Mode()&os.ModeSocket != 0 && os.SameFile(info, current) && info.ModTime().Equal(current.ModTime()) {
			if err := os.Remove(path); err != nil {
				return
			}
		}
	}
	if err := os.Chmod(path, mode); err != nil {
		cleanup()
		return nil, nil, ioFailure("socket mode", err)
	}

	return l, cleanup, nil
}

// Admission happens before Accept, so net/http never spawns an unbounded set of
// connection goroutines waiting on the limit. Close releases each slot once.
type originListener struct {
	net.Listener
	ctx    context.Context
	slots  chan struct{}
	config OriginConfig
}

func (l *originListener) Accept() (net.Conn, error) {
	select {
	case l.slots <- struct{}{}:
	case <-l.ctx.Done():
		return nil, l.ctx.Err()
	}

	c, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}

	return &originConn{Conn: c, config: l.config, release: func() { <-l.slots }}, nil
}

type onceBody struct {
	body interface{ Close() error }
	once sync.Once
}

// Callback Close is external code: suppress panic values, including during
// cancellation and late-return cleanup where net/http cannot recover them.
func (b *onceBody) close() {
	b.once.Do(func() {
		defer func() {
			if recover() != nil {
				return
			}
		}()

		closeBody(b.body)
	})
}
