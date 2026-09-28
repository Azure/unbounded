// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import "sync/atomic"

// Stats is a fixed-size, credential-free client telemetry snapshot. Counters are
// cumulative since construction; gauges describe the sampling instant. Fields
// are sampled independently and need not form a transaction during concurrent I/O.
type Stats struct {
	// QueueDepth sums calls waiting in all three independently bounded queues.
	QueueDepth int
	// Per-pool queue depths allow saturation to be diagnosed without request labels.
	BulkQueueDepth        int
	MetadataQueueDepth    int
	SmallObjectQueueDepth int
	// QueueWaits counts calls that entered the bounded admission queue.
	QueueWaits uint64
	// QueueWaitNanoseconds sums completed admission waits, including failures.
	QueueWaitNanoseconds uint64
	// QueueRejections counts calls rejected because the queue was full.
	QueueRejections uint64
	// QueueTimeouts counts waits that exceeded QueueTimeout.
	QueueTimeouts uint64
	// ActiveBulk and ActiveMetadata count occupied admission slots, including dials.
	ActiveBulk     int
	ActiveMetadata int
	// ActiveSmallObjects counts occupied small-object slots, including dials.
	ActiveSmallObjects int
	// Connections counts open SDK connections, both active and idle, across pools.
	Connections int64
	// IdleConnections counts reusable connections currently in all three pools.
	IdleConnections int
	// Dials counts attempted connection dials, including failed attempts.
	Dials uint64
	// ConnectionReuses counts leases taken from any idle pool.
	ConnectionReuses uint64
	// Retries counts single fresh-connection retries after stale pooled failures.
	Retries uint64
	// BytesRead counts body bytes consumed by Values, excluding headers and Stat.
	// It includes bytes read before a later body or destination-writer failure.
	BytesRead uint64
}

type clientStats struct {
	queueWaits, queueWaitNanoseconds, queueRejections, queueTimeouts atomic.Uint64
	dials, connectionReuses, retries, bytesRead                      atomic.Uint64
	connections                                                      atomic.Int64
}

// Stats returns bounded telemetry without allocating per-request labels or
// depending on a metrics framework. It is safe concurrently with Get, Stat and Close.
func (c *Client) Stats() Stats {
	c.mu.Lock()
	idle := len(c.bulk.idle) + len(c.metadataPool.idle) + len(c.smallPool.idle)
	c.mu.Unlock()

	bulk, metadata, small := len(c.bulk.queued), len(c.metadataPool.queued), len(c.smallPool.queued)

	return Stats{
		QueueDepth: bulk + metadata + small, BulkQueueDepth: bulk, MetadataQueueDepth: metadata, SmallObjectQueueDepth: small, QueueWaits: c.stats.queueWaits.Load(),
		QueueWaitNanoseconds: c.stats.queueWaitNanoseconds.Load(),
		QueueRejections:      c.stats.queueRejections.Load(), QueueTimeouts: c.stats.queueTimeouts.Load(),
		ActiveBulk: len(c.slots), ActiveMetadata: len(c.metadataPool.slots),
		ActiveSmallObjects: len(c.smallPool.slots),
		Connections:        c.stats.connections.Load(), IdleConnections: idle,
		Dials: c.stats.dials.Load(), ConnectionReuses: c.stats.connectionReuses.Load(),
		Retries: c.stats.retries.Load(), BytesRead: c.stats.bytesRead.Load(),
	}
}
