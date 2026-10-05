// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"

	"github.com/Azure/unbounded/pkg/racersdk/internal/originsock"
)

func listenOrigin(path string, mode os.FileMode) (*net.UnixListener, func(), error) {
	l, cleanup, err := originsock.Listen(path, mode, false)
	return l, cleanup, socketError(err)
}

func listenOwnedOrigin(path string, mode os.FileMode) (*net.UnixListener, func(), error) {
	l, cleanup, err := originsock.Listen(path, mode, true)
	return l, cleanup, socketError(err)
}

func socketError(err error) error {
	var typed *originsock.Error
	if !errors.As(err, &typed) {
		return err
	}

	if typed.Invalid {
		return failure(ErrorInvalidArgument, typed.Operation, typed.Cause)
	}

	return ioFailure(typed.Operation, typed.Cause)
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
