// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"net/http"
	"sync"

	"github.com/Azure/unbounded/internal/racer/wire"
)

// Lifecycle owns process serving, independently of the leader-owned publishers.
// The historical leader field and LeaderContext method bind process cancellation.
type Lifecycle struct {
	mu               sync.Mutex
	started          bool
	leader           context.Context
	synced           bool
	issuer           bool
	serving          bool
	publications     *Publications
	waitForCacheSync func(context.Context) bool
}

func newLifecycle(p *Publications) *Lifecycle {
	return &Lifecycle{publications: p}
}

func (*Lifecycle) NeedLeaderElection() bool { return false }

// LeaderContext is the legacy name for binding a request to the serving process.
// Missing or canceled process lifetime returns an already-canceled child.
func (l *Lifecycle) LeaderContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	if l == nil {
		cancel()
		return ctx, cancel
	}

	l.mu.Lock()
	leader := l.leader
	l.mu.Unlock()

	if leader == nil {
		cancel()
		return ctx, cancel
	}

	stop := context.AfterFunc(leader, cancel)
	if leader.Err() != nil {
		cancel()
	}

	return ctx, func() { stop(); cancel() }
}

func (l *Lifecycle) Start(ctx context.Context) error {
	l.mu.Lock()
	if l.started {
		l.mu.Unlock()
		return wire.Conflict
	}

	l.started, l.leader = true, ctx
	l.publications.bindProcess(ctx)
	l.mu.Unlock()

	defer func() {
		l.mu.Lock()
		l.synced, l.issuer, l.serving = false, false, false
		l.mu.Unlock()
	}()

	if l.waitForCacheSync == nil || !l.waitForCacheSync(ctx) {
		if ctx.Err() != nil {
			return nil
		}

		return wire.Unavailable
	}

	l.mu.Lock()
	l.synced = ctx.Err() == nil
	l.mu.Unlock()
	<-ctx.Done()

	return nil
}

// SetIssuerReady must be reset on loss of usable signing material/trust.
func (l *Lifecycle) SetIssuerReady(ready bool) {
	l.mu.Lock()
	l.issuer = ready
	l.mu.Unlock()
}

// SetServingReady is set only after the authenticated listener is accepting.
func (l *Lifecycle) SetServingReady(ready bool) {
	l.mu.Lock()
	l.serving = ready
	l.mu.Unlock()
}

func (l *Lifecycle) Ready(_ *http.Request) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.leader == nil || l.leader.Err() != nil || !l.synced || !l.issuer || !l.serving {
		return wire.Unavailable
	}

	return l.publications.Ready(nil)
}
