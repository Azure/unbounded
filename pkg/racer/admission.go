// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"io"
	"net/http"
	"sync"
)

// A nil admission preserves unlimited active requests. Permits are shared by
// both transports and all views, and never acquired under a pool mutex.
type requestAdmission struct {
	slots chan struct{}
}

type requestPermit struct {
	admission *requestAdmission
	once      sync.Once
}

func (a *requestAdmission) acquire(ctx context.Context) (*requestPermit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if a == nil {
		return nil, nil
	}

	select {
	case a.slots <- struct{}{}:
		p := &requestPermit{admission: a}
		if err := ctx.Err(); err != nil {
			p.release()
			return nil, err
		}

		return p, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// tryAcquire is for optional speculative work: never wait while a foreground
// response holds a permit. A false result means skip speculation. Any future
// prefetch must pass the acquired permit to its request, not acquire twice.
func (a *requestAdmission) tryAcquire(ctx context.Context) (*requestPermit, bool) {
	if ctx.Err() != nil {
		return nil, false
	}

	if a == nil {
		return nil, true
	}

	select {
	case a.slots <- struct{}{}:
		p := &requestPermit{admission: a}
		if ctx.Err() != nil {
			p.release()
			return nil, false
		}

		return p, true
	default:
		return nil, false
	}
}

func (p *requestPermit) release() {
	if p != nil {
		p.once.Do(func() { <-p.admission.slots })
	}
}

func (c *Client) do(r *http.Request) (*http.Response, error) {
	if c.admission == nil {
		return c.http.Do(r)
	}

	// Include admission waiting in the existing per-request timeout.
	ctx := r.Context()

	var cancel context.CancelFunc
	if c.http.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, c.http.Timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}

	p, err := c.admission.acquire(ctx)
	if err != nil {
		cancel()
		return nil, err
	}

	resp, err := c.http.Do(r.WithContext(ctx))
	if err != nil {
		p.release()
		cancel()

		return nil, err
	}

	b := &admittedBody{ReadCloser: resp.Body, remaining: resp.ContentLength}
	// Publish the callback only after its stop function is initialized. Closing
	// the underlying body interrupts a blocked read before releasing admission.
	ready := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		<-ready

		_ = b.Close() //nolint:errcheck // Cancellation cleanup.
	})
	b.done = func() { stop(); p.release(); cancel() }

	close(ready)

	resp.Body = b

	return resp, nil
}

type admittedBody struct {
	io.ReadCloser
	remaining int64
	done      func()
	once      sync.Once
	closeErr  error
}

func (b *admittedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)

	b.remaining -= int64(n)
	if err != nil || b.remaining == 0 {
		_ = b.Close() //nolint:errcheck // Preserve the read result.
	}

	return n, err
}

func (b *admittedBody) Close() error {
	b.once.Do(func() {
		b.closeErr = b.ReadCloser.Close()
		b.done()
	})

	return b.closeErr
}
