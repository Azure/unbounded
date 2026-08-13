// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package ingest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// DefaultWorkers is how many layers a node converts at once.
//
// One is the default on purpose. Building an EROFS image is CPU and disk bound
// and it runs on a node that is at that very moment also unpacking the same
// image the ordinary way, which is already the busiest the node gets. Ingest is
// off the container start path, so making it fast buys nothing; making it quiet
// keeps it from slowing down the very pods it is trying to help.
const DefaultWorkers = 1

// DefaultRetryDelay is how long a failed ingest waits before its one retry.
const DefaultRetryDelay = time.Minute

// DefaultQueueDepth bounds the backlog.
//
// A full queue drops the oldest waiting request rather than blocking Commit.
// Dropping is safe: the layer stays in the catalog's miss set, so the next node
// that starts a pod from that image submits it again. Blocking would put ingest
// back on the container start path, which is the one thing it must never be on.
const DefaultQueueDepth = 256

// QueueOptions configures a Queue.
type QueueOptions struct {
	// Ingester does the work. Required.
	Ingester *Ingester

	// Elector sets the per-layer delay. Defaults to Immediate.
	Elector Elector

	// Workers is the number of concurrent builds. Defaults to
	// DefaultWorkers.
	Workers int

	// Depth bounds the backlog. Defaults to DefaultQueueDepth.
	Depth int

	// RetryDelay is the wait before the single retry of a failed request.
	// Defaults to DefaultRetryDelay.
	RetryDelay time.Duration

	// Observe is called once per completed attempt. It exists so the
	// snapshotter can log and count without this package importing a
	// logger. Optional.
	Observe func(req Request, res Result, err error)
}

// Queue runs ingests in the background, one layer at a time by default.
//
// It exists so Commit can return the moment the local unpack is durable. The
// node that missed in the catalog has already paid the full cost of a normal
// snapshotter and its pod is starting; converting the layer for everybody else
// is strictly future work and must not be on that path.
type Queue struct {
	ing        *Ingester
	elector    Elector
	workers    int
	retryDelay time.Duration
	observe    func(Request, Result, error)

	work chan Request

	mu      sync.Mutex
	pending map[catalogKey]struct{}
}

// catalogKey deduplicates by the pair a request actually publishes.
type catalogKey struct {
	diffID  string
	chainID string
}

// NewQueue builds a Queue. Call Run to start it.
func NewQueue(opts QueueOptions) (*Queue, error) {
	if opts.Ingester == nil {
		return nil, errors.New("ingest: queue has no ingester")
	}

	if opts.Elector == nil {
		opts.Elector = Immediate{}
	}

	if opts.Workers <= 0 {
		opts.Workers = DefaultWorkers
	}

	if opts.Depth <= 0 {
		opts.Depth = DefaultQueueDepth
	}

	if opts.RetryDelay <= 0 {
		opts.RetryDelay = DefaultRetryDelay
	}

	return &Queue{
		ing:        opts.Ingester,
		elector:    opts.Elector,
		workers:    opts.Workers,
		retryDelay: opts.RetryDelay,
		observe:    opts.Observe,
		work:       make(chan Request, opts.Depth),
		pending:    make(map[catalogKey]struct{}),
	}, nil
}

// Submit queues a layer. It never blocks.
//
// It reports whether the request was accepted. A duplicate of something already
// queued and a request that does not fit both return false, and both are
// ordinary: the layer is still in the catalog's miss set and the next pod that
// needs it will submit it again.
func (q *Queue) Submit(req Request) bool {
	if err := req.validate(); err != nil {
		return false
	}

	k := catalogKey{diffID: req.DiffID.String(), chainID: req.ChainID.String()}

	q.mu.Lock()

	if _, dup := q.pending[k]; dup {
		q.mu.Unlock()

		return false
	}

	q.pending[k] = struct{}{}
	q.mu.Unlock()

	select {
	case q.work <- req:
		return true
	default:
		q.mu.Lock()
		delete(q.pending, k)
		q.mu.Unlock()

		return false
	}
}

// Pending is the number of queued requests, for tests and metrics.
func (q *Queue) Pending() int {
	q.mu.Lock()
	defer q.mu.Unlock()

	return len(q.pending)
}

// Run processes the queue until ctx is cancelled, then returns once the
// in-flight requests have finished.
func (q *Queue) Run(ctx context.Context) {
	var wg sync.WaitGroup

	for range q.workers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			q.worker(ctx)
		}()
	}

	wg.Wait()
}

func (q *Queue) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case req := <-q.work:
			q.handle(ctx, req)
		}
	}
}

func (q *Queue) handle(ctx context.Context, req Request) {
	defer func() {
		k := catalogKey{diffID: req.DiffID.String(), chainID: req.ChainID.String()}

		q.mu.Lock()
		delete(q.pending, k)
		q.mu.Unlock()
	}()

	// The delay is the election. Waiting here rather than inside Ingest is
	// what makes the check after the wait meaningful: by then the favoured
	// node has usually published, and Ingest's first act is to re-read the
	// catalog and find it.
	if !sleepCtx(ctx, q.elector.Delay(req.Layer)) {
		return
	}

	res, err := q.ing.Ingest(ctx, req)
	if err == nil {
		q.report(req, res, nil)

		return
	}

	// One retry, and only one. A second failure is almost always a
	// permanent condition, a missing content blob or a full volume, and
	// hammering it would compete with the pods this node is trying to run.
	if !sleepCtx(ctx, q.retryDelay) {
		return
	}

	res, err = q.ing.Ingest(ctx, req)

	q.report(req, res, err)
}

func (q *Queue) report(req Request, res Result, err error) {
	if q.observe != nil {
		q.observe(req, res, err)
	}
}

// sleepCtx waits for d and reports whether the wait completed rather than being
// cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}

	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// String renders a request for logs.
func (r Request) String() string {
	return fmt.Sprintf("layer %s chain %s", r.DiffID.Short(), r.ChainID.Short())
}
