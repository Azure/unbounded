# Optional receiver-wide incast admission

This experimental dataplane gate limits outbound peer exchanges whose response can
contain a page. It is disabled by default and is not a congestion-control replacement.

| Environment variable | Default | Valid range |
| --- | --- | --- |
| `RACER_PEER_RECEIVE_MAX` | `0` (disabled) | 0 through 65536 |
| `RACER_PEER_RECEIVE_QUEUE_MAX` | `256` | 1 through 65536 |
| `RACER_PEER_RECEIVE_WAIT_MS` | `1000` | 1 through 30000 |

One cache-line-aligned gate is shared across workers. Page, Bootstrap, and Subscribe exchanges enter
before adaptive peer admission and socket checkout, including direct hedges and
opaque/materialized relays. Metadata bypasses this gate. Existing circuit, socket,
memory, pipe, subscription and acquisition limits still apply independently.

Queued requests use FIFO without barging, not per-tenant or per-sender fair shares.
Queue slots and active permit bookkeeping are charged to request-context quota.
Full queues reject; normal active-limit saturation waits. Waiting retains original
cancellation, hard deadline, candidate idle/total limits and routing/attempt credits.
The queue bound only shortens waiting; it never renews authority or refunds credits.
Every worker turn inspects a bounded batch of entries and explicitly wakes expired
children. Expiration precision depends on worker progress and queue length, not
unrelated network completions. Cancellation also has its own wake registration.

Permits follow HTTP connection and native QP completion ownership. Opaque response
headers do not release them, abandoned I/O must fence, and idle pooled connections
do not retain them. A native failed fence may quarantine capacity until recovery.

This is conservative exchange admission, not an exact count of active TCP bodies:
an admitted exchange may be waiting for checkout or downstream acquisition. Chained
requests can form distributed hold-and-wait even when each route is cycle-free.
Finite queue waits and original exchange deadlines bound these stalls; they do not
prove successful deadlock-free progress. There is no unlimited relay bypass or
priority lending. Small caps may increase timeouts or fallback traffic.

Metrics: `racer_peer_receive_active`, `racer_peer_receive_queued`, and counters
`racer_peer_receive_{admitted,full,timeout,canceled}_total`.
`racer_peer_receive_wait_ns_total` sums waits for granted exchanges only; divide by
admitted count for a mean, not a tail distribution. Check failures as well as goodput.

Canary acceptance requires improved verified completion tails and TCP recovery
without new consumer errors or lower useful throughput. Compare matched load with
an unchanged control; fewer active connections alone is not success. Parent owns
rollout, rollback and load. No crypto, page layout, routing or thread placement is
changed by this experiment.

Validation includes a real nonempty signed requester with pooled reuse, real TCP
connect/relay abandonment fences, worker-driven expiry, and simulated native
quarantine. Opposing dependency chains are tested at the transport admission
boundary; this is not an end-to-end proof for every multihop topology or native
hardware. The optional canary and unchanged control remain necessary.
