// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package apiserver

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternal "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"

	rebound "github.com/Azure/unbounded/api/rebound/v1alpha1"
	"github.com/Azure/unbounded/internal/rebound/ledger"
)

const historyLimit = 100

type legacyWatchKey struct{}

type snapshot struct {
	ledger *ledger.Ledger
	rv     string
}

type grantWatch struct {
	storage *storage
	result  chan watch.Event
	done    chan struct{}
	match   func(*rebound.NodeGrant) bool
}

func (w *grantWatch) ResultChan() <-chan watch.Event { return w.result }
func (w *grantWatch) Stop() {
	w.storage.mu.Lock()
	defer w.storage.mu.Unlock()

	w.stopLocked()
}

func (w *grantWatch) stopLocked() {
	if _, ok := w.storage.watchers[w]; !ok {
		return
	}

	delete(w.storage.watchers, w)
	close(w.done)
	close(w.result)
}

func (s *storage) Destroy() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closed = true
	for w := range s.watchers {
		w.stopLocked()
	}
}

func (s *storage) Watch(ctx context.Context, options *metainternal.ListOptions) (watch.Interface, error) {
	if options == nil {
		options = &metainternal.ListOptions{}
	}

	if options.SendInitialEvents != nil && *options.SendInitialEvents && ctx.Value(legacyWatchKey{}) != true {
		return nil, apierrors.NewBadRequest("sendInitialEvents is not supported; use list followed by watch")
	}

	match, err := selector(ctx, options)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil, apierrors.NewServiceUnavailable("storage is closed")
	}

	var initial []watch.Event

	if options.ResourceVersion == "" || options.ResourceVersion == "0" {
		current := s.history[len(s.history)-1]
		for _, g := range current.ledger.Grants() {
			if !g.Expired(s.clock.Now()) {
				initial = append(initial, watch.Event{Type: watch.Added, Object: grantObject(g, current.rv)})
			}
		}
	} else {
		index := -1

		for i, snap := range s.history {
			if snap.rv == options.ResourceVersion {
				index = i
				break
			}
		}

		if index < 0 {
			return nil, apierrors.NewResourceExpired("resourceVersion is not in ledger history; relist")
		}

		for i := index + 1; i < len(s.history); i++ {
			initial = append(initial, diff(s.history[i-1], s.history[i])...)
		}
	}

	w := &grantWatch{storage: s, result: make(chan watch.Event, len(initial)+128), done: make(chan struct{}), match: match}
	for _, event := range initial {
		if g, ok := event.Object.(*rebound.NodeGrant); ok && match(g) {
			w.result <- event
		}
	}

	s.watchers[w] = struct{}{}

	go func() {
		select {
		case <-ctx.Done():
			w.Stop()
		case <-w.done:
		}
	}()

	return w, nil
}

func diff(old, next snapshot) []watch.Event {
	var events []watch.Event

	for _, g := range old.ledger.Grants() {
		n, ok := next.ledger.Get(g.Key())
		if !ok || n.UID != g.UID {
			events = append(events, watch.Event{Type: watch.Deleted, Object: grantObject(g, next.rv)})
		}
	}

	for _, g := range next.ledger.Grants() {
		o, ok := old.ledger.Get(g.Key())
		if !ok || o.UID != g.UID {
			events = append(events, watch.Event{Type: watch.Added, Object: grantObject(g, next.rv)})
		}
	}

	return events
}

func (s *storage) observe(obj interface{}, deleted bool) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}

	cm, ok := obj.(*corev1.ConfigMap)
	if !ok || cm.Name != ledger.ConfigMapName || cm.Namespace != s.store.Namespace {
		return
	}

	next := snapshot{ledger: ledger.Parse(cm.Data), rv: cm.ResourceVersion}
	if deleted {
		next.ledger = ledger.New()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	old := s.history[len(s.history)-1]
	if !deleted && old.rv == next.rv {
		return
	}

	for _, event := range diff(old, next) {
		for w := range s.watchers {
			g, ok := event.Object.(*rebound.NodeGrant)
			if !ok || !w.match(g) {
				continue
			}

			select {
			case w.result <- watch.Event{Type: event.Type, Object: g.DeepCopy()}:
			default:
				// A slow client reconnects using its last RV, or relists if it fell behind.
				w.stopLocked()
			}
		}
	}

	s.history = append(s.history, next)
	if len(s.history) > historyLimit {
		s.history = s.history[len(s.history)-historyLimit:]
	}
}
