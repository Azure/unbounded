// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package imagepull

import (
	"container/list"
	"sync"
	"time"
)

// maxQuarantined bounds the number of (mirror, blob) pairs a Puller
// remembers as corrupt; the oldest entry is evicted first.
const maxQuarantined = 1024

type quarantineKey struct {
	mirror string
	blob   string
}

type quarantineEntry struct {
	key     quarantineKey
	expires time.Time
}

// quarantine remembers blobs a mirror served corrupted, so that later
// Images resolved by the same Puller read them from upstream only. It is
// bounded in size and entries expire after ttl.
type quarantine struct {
	mu    sync.Mutex
	ttl   time.Duration
	max   int
	now   func() time.Time
	order *list.List // of *quarantineEntry, oldest first
	byKey map[quarantineKey]*list.Element
}

func newQuarantine(ttl time.Duration, maxEntries int) *quarantine {
	return &quarantine{
		ttl:   ttl,
		max:   maxEntries,
		now:   time.Now,
		order: list.New(),
		byKey: map[quarantineKey]*list.Element{},
	}
}

func (q *quarantine) add(mirror, blob string) {
	if q == nil {
		return
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	k := quarantineKey{mirror, blob}
	if e, ok := q.byKey[k]; ok {
		q.order.Remove(e)
	}

	q.byKey[k] = q.order.PushBack(&quarantineEntry{key: k, expires: q.now().Add(q.ttl)})

	for q.order.Len() > q.max {
		oldest := q.order.Front()
		q.order.Remove(oldest)
		delete(q.byKey, oldest.Value.(*quarantineEntry).key) //nolint:errcheck,forcetypeassert // only entries are stored
	}
}

func (q *quarantine) has(mirror, blob string) bool {
	if q == nil {
		return false
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	e, ok := q.byKey[quarantineKey{mirror, blob}]
	if !ok {
		return false
	}

	if !q.now().Before(e.Value.(*quarantineEntry).expires) { //nolint:errcheck,forcetypeassert // only entries are stored
		q.order.Remove(e)
		delete(q.byKey, quarantineKey{mirror, blob})

		return false
	}

	return true
}

func (q *quarantine) len() int {
	if q == nil {
		return 0
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	return q.order.Len()
}
