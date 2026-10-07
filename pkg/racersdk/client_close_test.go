// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"net"
	"sync"
	"testing"
)

func TestClientCloseCancelsBoundContextsBeforeReturning(t *testing.T) {
	c := testClient(t, "unused", 1)
	contexts := make([]context.Context, 0, 64)

	for range cap(contexts) {
		ctx, done := c.bind(context.Background())
		defer done()

		contexts = append(contexts, ctx)
	}

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	for _, ctx := range contexts {
		assertIs(t, context.Cause(ctx), net.ErrClosed)
	}
}

func TestClientBindAfterClose(t *testing.T) {
	c := testClient(t, "unused", 1)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	ctx, done := c.bind(context.Background())
	defer done()

	assertIs(t, context.Cause(ctx), net.ErrClosed)
}

func TestClientClosePreservesCallerCancellation(t *testing.T) {
	c := testClient(t, "unused", 1)

	parent, cancel := context.WithCancel(context.Background())
	defer cancel()

	ctx, done := c.bind(parent)
	defer done()

	cancel()

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	assertIs(t, context.Cause(ctx), context.Canceled)
}

func TestClientBindRelease(t *testing.T) {
	c := testClient(t, "unused", 1)

	ctx, done := c.bind(context.Background())
	defer done()

	if ctx.Err() != nil {
		t.Fatal("bound context canceled before release")
	}

	done()
	done()
	assertIs(t, context.Cause(ctx), context.Canceled)

	c.mu.Lock()
	remaining := len(c.active)
	c.mu.Unlock()

	if remaining != 0 {
		t.Fatal("released context retained by client")
	}

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	assertIs(t, context.Cause(ctx), context.Canceled)
}

func TestClientConcurrentBindAndClose(t *testing.T) {
	c := testClient(t, "unused", 1)
	start := make(chan struct{})

	var active sync.WaitGroup

	for range 64 {
		active.Go(func() {
			<-start

			ctx, done := c.bind(context.Background())
			defer done()

			if err := c.Close(); err != nil {
				t.Error(err)
			}

			if ctx.Err() == nil {
				t.Error("Close returned before canceling bound context")
			}
		})
	}

	close(start)
	active.Wait()
}
