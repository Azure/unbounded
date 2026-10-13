// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package apiserver

import (
	"context"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternal "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	"k8s.io/apimachinery/pkg/watch"
	apistorage "k8s.io/apiserver/pkg/storage"
	"k8s.io/client-go/tools/cache"

	rebound "github.com/Azure/unbounded/api/rebound/v1alpha1"
	"github.com/Azure/unbounded/internal/rebound/ledger"
)

const historyLimit = 100

var watchHistoryTimeout = 3 * time.Second

type legacyWatchKey struct{}

type snapshot struct {
	ledger  *ledger.Ledger
	rv      string
	changed chan struct{}
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

	if s.closed {
		return
	}

	s.closed = true
	if changed := s.history[len(s.history)-1].changed; changed != nil {
		close(changed)
	}

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
			obj := grantObject(g, current.rv)
			if !g.Expired(s.clock.Now()) && match(obj) {
				initial = append(initial, watch.Event{Type: watch.Added, Object: obj})
			}
		}
	} else {
		index, err := s.watchHistoryIndex(ctx, options.ResourceVersion)
		if err != nil {
			return nil, err
		}

		for i := index + 1; i < len(s.history); i++ {
			initial = append(initial, diff(s.history[i-1], s.history[i]).matching(match)...)
		}
	}

	w := &grantWatch{storage: s, result: make(chan watch.Event, len(initial)+128), done: make(chan struct{}), match: match}
	for _, event := range initial {
		w.result <- event
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

// watchHistoryIndex is called with s.mu held and returns with it held.
func (s *storage) watchHistoryIndex(ctx context.Context, rv string) (int, error) {
	requested, err := strconv.ParseUint(rv, 10, 64)
	if err != nil {
		return -1, apierrors.NewResourceExpired("resourceVersion is not in ledger history; relist")
	}

	timer := time.NewTimer(watchHistoryTimeout)
	defer timer.Stop()

	timedOut := false

	for {
		if err := ctx.Err(); err != nil {
			return -1, err
		}

		if s.closed {
			return -1, apierrors.NewServiceUnavailable("storage is closed")
		}

		latest := &s.history[len(s.history)-1]

		newest, err := strconv.ParseUint(latest.rv, 10, 64)
		if err != nil {
			return -1, apierrors.NewResourceExpired("ledger resourceVersion is invalid; relist")
		}

		if newest >= requested {
			// History contains every informer observation. Revision gaps can include
			// unrelated writes or coalesced updates; replay from the floor so the
			// next observed transition is not skipped.
			for i := len(s.history) - 1; i >= 0; i-- {
				version, err := strconv.ParseUint(s.history[i].rv, 10, 64)
				if err == nil && version <= requested {
					return i, nil
				}
			}

			return -1, apierrors.NewResourceExpired("resourceVersion is not in ledger history; relist")
		}

		if timedOut {
			return -1, apistorage.NewTooLargeResourceVersionError(requested, newest, 1)
		}

		if latest.changed == nil {
			latest.changed = make(chan struct{})
		}

		changed := latest.changed
		s.mu.Unlock()

		select {
		case <-changed:
		case <-ctx.Done():
		case <-timer.C:
			timedOut = true
		}

		s.mu.Lock()
	}
}

type eventBatch struct {
	events []watch.Event
	rv     string
}

func (b eventBatch) matching(match func(*rebound.NodeGrant) bool) []watch.Event {
	var events []watch.Event

	for _, event := range b.events {
		if g, ok := event.Object.(*rebound.NodeGrant); ok && match(g) {
			events = append(events, watch.Event{Type: event.Type, Object: g.DeepCopy()})
		}
	}

	// Only the last event this watcher receives advances past the batch.
	if len(events) != 0 {
		if g, ok := events[len(events)-1].Object.(*rebound.NodeGrant); ok {
			g.ResourceVersion = b.rv
		}
	}

	return events
}

func diff(old, next snapshot) eventBatch {
	var events []watch.Event

	for _, g := range old.ledger.Grants() {
		n, ok := next.ledger.Get(g.Key())
		if !ok || n.UID != g.UID {
			events = append(events, watch.Event{Type: watch.Deleted, Object: grantObject(g, old.rv)})
		}
	}

	for _, g := range next.ledger.Grants() {
		o, ok := old.ledger.Get(g.Key())
		if !ok || o.UID != g.UID {
			events = append(events, watch.Event{Type: watch.Added, Object: grantObject(g, old.rv)})
		}
	}

	return eventBatch{events: events, rv: next.rv}
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

	if s.closed {
		return
	}

	old := s.history[len(s.history)-1]
	if !deleted && old.rv == next.rv {
		return
	}

	batch := diff(old, next)
	for w := range s.watchers {
	deliver:
		for _, event := range batch.matching(w.match) {
			select {
			case w.result <- event:
			default:
				// A slow client reconnects using its last RV, or relists if it fell behind.
				w.stopLocked()
				break deliver
			}
		}
	}

	// The empty RV 0 seed is not an observed state before the initial list.
	if len(s.history) == 1 && old.rv == "0" {
		s.history = nil
	}

	s.history = append(s.history, next)
	if len(s.history) > historyLimit {
		s.history = s.history[len(s.history)-historyLimit:]
	}

	if old.changed != nil {
		close(old.changed)
	}
}
