// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"net/http"
	"sync"

	"github.com/Azure/unbounded/internal/racer/wire"
)

// Lifecycle is one-shot, leader-scoped readiness infrastructure. Phase 4 reports
// usable issuer/trust; Phase 5 reports listener readiness and uses Wait to gate
// serving. A canceled leadership can never be restarted or made ready again.
type Lifecycle struct {
	mu               sync.Mutex
	started          bool
	leader           context.Context
	synced           bool
	issuer           bool
	serving          bool
	changed          chan struct{}
	publications     *Publications
	waitForCacheSync func(context.Context) bool
}

func newLifecycle(p *Publications) *Lifecycle {
	return &Lifecycle{publications: p, changed: make(chan struct{})}
}

func (*Lifecycle) NeedLeaderElection() bool { return true }

// LeaderContext binds a child of parent to the current leadership. Missing or
// canceled leadership returns an already-canceled child. Cancel releases the link.
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

func (l *Lifecycle) notifyLocked() { close(l.changed); l.changed = make(chan struct{}) }

func (l *Lifecycle) Start(ctx context.Context) error {
	l.mu.Lock()
	if l.started {
		l.mu.Unlock()
		return wire.Conflict
	}

	l.started, l.leader = true, ctx
	l.notifyLocked()
	l.mu.Unlock()

	defer func() {
		l.mu.Lock()
		l.synced, l.issuer, l.serving = false, false, false
		l.notifyLocked()
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
	l.notifyLocked()
	l.mu.Unlock()
	<-ctx.Done()

	return nil
}

// SetIssuerReady must be reset on loss of usable signing material/trust.
func (l *Lifecycle) SetIssuerReady(ready bool) {
	l.mu.Lock()
	if l.issuer != ready {
		l.issuer = ready
		l.notifyLocked()
	}
	l.mu.Unlock()
}

// SetServingReady is set only after the authenticated listener is accepting.
func (l *Lifecycle) SetServingReady(ready bool) {
	l.mu.Lock()
	if l.serving != ready {
		l.serving = ready
		l.notifyLocked()
	}
	l.mu.Unlock()
}

func (l *Lifecycle) readyLocked(serving bool) error {
	if l.leader == nil || l.leader.Err() != nil || !l.synced || !l.issuer || serving && !l.serving {
		return wire.Unavailable
	}

	return nil
}

func (l *Lifecycle) Ready(_ *http.Request) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if err := l.readyLocked(true); err != nil {
		return err
	}

	return l.publications.Ready(nil)
}

// Wait gates listener startup on leadership, synchronized inputs, issuer/trust,
// and the first committed publication. It also terminates on leadership loss.
func (l *Lifecycle) Wait(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		l.mu.Lock()
		err := l.readyLocked(false)
		changed := l.changed

		var stopped <-chan struct{}
		if l.leader != nil {
			stopped = l.leader.Done()
		}

		_, published, publicationErr := l.publications.CurrentAndSubscribe()
		if err == nil {
			err = publicationErr
		}
		l.mu.Unlock()

		if err == nil {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-stopped:
			return context.Canceled
		case <-changed:
		case <-published:
		}
	}
}
