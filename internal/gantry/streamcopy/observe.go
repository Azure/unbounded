// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package streamcopy

import (
	"io"
	"sync"
)

// BeginActivity returns an idempotent finish callback for one active operation.
func BeginActivity(change func(int)) func() {
	if change == nil {
		return func() {}
	}

	change(1)

	return sync.OnceFunc(func() { change(-1) })
}

// Observe reports bytes as they are read and finishes on a terminal read or
// Close. Callbacks must be safe for concurrent calls.
func Observe(body io.ReadCloser, bytes func(int64), finish func()) io.ReadCloser {
	return &observedReadCloser{ReadCloser: body, bytes: bytes, finish: sync.OnceFunc(finish)}
}

type observedReadCloser struct {
	io.ReadCloser
	bytes  func(int64)
	finish func()
}

func (body *observedReadCloser) Read(buffer []byte) (int, error) {
	n, err := body.ReadCloser.Read(buffer)
	if n > 0 && body.bytes != nil {
		body.bytes(int64(n))
	}

	if err != nil {
		body.finish()
	}

	return n, err
}

func (body *observedReadCloser) Close() error {
	err := body.ReadCloser.Close()
	body.finish()

	return err
}
