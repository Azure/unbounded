// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk/internal/wire"
)

// Invalidate drops the cached version of key whose ETag is etag, so the next
// read fetches it from the [Origin] again. Use it when a consumer detects
// corrupt content, for example a digest mismatch.
//
// etag must be the strong tag a read reported, such as [Metadata.ETag]. A
// newer version of the same key is never dropped. Invalidate succeeds
// whether or not anything was cached, so it is safe to retry.
//
// The local node drops the version at once. Other nodes that cache its pages
// are told best effort: an unreachable node may keep serving its copy until
// it is evicted. Very large objects drop their metadata and their first pages
// only. Racer bounds concurrent invalidations per node and answers excess ones
// with [ErrUnavailable].
func (c *Client) Invalidate(ctx context.Context, key Key, etag string) error {
	const op = "invalidate"

	head, err := wire.InvalidateHead(key, etag)
	if err != nil {
		return invalid(op, err)
	}

	release, err := c.admit(ctx, &c.stat, op)
	if err != nil {
		return err
	}
	defer release()

	ctx, done := c.bind(ctx)
	defer done()

	for attempt := 0; ; attempt++ {
		conn, reused, err := c.conn(ctx, op, attempt > 0)
		if err != nil {
			return err
		}

		started, err := c.exchangeInvalidate(ctx, conn, head)
		// Racer closes the connection after every invalidation.
		closeQuietly(conn)

		if err == nil {
			return nil
		}

		if ctx.Err() != nil {
			return contextError(op, ctx)
		}

		// A pooled connection may have been closed by Racer while idle.
		// Retry once on a fresh connection if nothing was received.
		if attempt == 0 && reused && !started && staleConnectionError(err) {
			continue
		}

		return ioFailure(op, err)
	}
}

func (c *Client) exchangeInvalidate(ctx context.Context, conn *clientConn, head []byte) (bool, error) {
	// Peers are told within a few seconds; allow for that past the head timeout.
	if err := conn.SetDeadline(time.Now().Add(c.limits.headerTimeout + invalidatePropagation)); err != nil {
		return false, err
	}

	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) }) //nolint:errcheck // Best effort interrupt.
	defer stop()

	if _, err := conn.Write(head); err != nil {
		return false, err
	}

	raw, err := wire.ReadHeadBytes(conn.r, true)
	started := len(raw) != 0

	if err != nil {
		return started, err
	}

	return started, wire.ParseInvalidateResponse(raw)
}

// invalidatePropagation is the longest Racer waits for peers before answering.
const invalidatePropagation = 5 * time.Second
