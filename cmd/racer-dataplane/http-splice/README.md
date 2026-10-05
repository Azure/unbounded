# HTTP splice mechanisms

`http-splice` composes `http1`, `flow-control`, and the explicit `uring-runtime`.
`http1` does not depend on flow control. No new external dependency is introduced:
the adapter uses the workspace's existing runtime and libc.

## Opaque relay profile

`relay::Relay<C, P>` transfers an already validated fixed-length body. `C` is the
existing HTTP context (buffer charge, connection state and admission); `P` is a
small synchronous `RelayPipe` adapter, implemented for `flow_control::PipeLease`.
There is no executor, telemetry selection, authentication, page identity, quota
class, or application protocol in the engine.

Each `step` performs at most 32 nonblocking actions. It handles HTTP read-ahead,
socket-to-pipe-to-socket splice, and fallback to one lazy 64 KiB admitted HTTP
buffer for unsupported splice. An accepted prefix is never replayed. A buffered
pipe suffix drains before copied receives resume. Excess read-ahead is restored
so application finalization still rejects pipelining. EINTR consumes a turn;
EAGAIN returns readiness. EOF is an I/O failure. Drain errors remain terminal,
matching the original relay profile rather than Delivery's retry policy.

The caller checks scope, holds the engine in `Rc<RefCell<_>>`, and passes that
**complete owner** as the lease to `readiness_with_lease`. The returned readiness
descriptor alone does not retain connections, pipe, or buffer accounting.
Abandonment/cancellation must fence that wait before owners can be released.
The engine does not submit asynchronous I/O itself. The caller decides deadline
clipping/retry (Racer uses a 10 ms observation tick), cooperative yielding,
exchange finish/poison ordering, and application reservation release.

`test-util` exposes deterministic fallback injection only. Production syscall
unsupported errors still trigger fallback without that feature.

## Immutable Delivery profile

`delivery::send` is separate from the relay state machine. `Owner<C>` supplies
the full immutable backing owner, accepted cursor, independent staging pipe, and
stable owning `SendBuffer` views. `DeliveryPipe` adapts bounded nonblocking pipe
write/drain/splice operations; the production adapter is `PipeLease<P>`. The
existing HTTP context allocates admitted scratch and the runtime reactor is the
owned writer. No extra writer framework, allocator, or dynamic observer is used.

The engine first copies into an empty pipe and splices its exact suffix. On
unsupported splice it resumes copying at the socket-accepted cursor, never the
staged cursor. On backpressure it drains the pipe into one admitted buffer and
submits a send owning the complete `(owner, connection)` tuple. Later sends use
immutable views to reconstruct any unsent suffix. No FD-only readiness wait can
release reader, pipe, or connection admission before its final completion fence.
Unlike relay, an interrupted drain yields and retries; partial/empty drains are
terminal. EINTR yields once. Accepted progress resets the stall clock; a turn
yields at 256 KiB or 32 successful calls, with 64 KiB maximum syscall chunks.

`Observer<C>` receives direct-byte/pipe-drain events and before/after-owned-send
boundaries and selects the checked send scope from the last-progress time. Racer's
adapter in `src/http.rs` retains telemetry labels, absolute versus progressing
deadline arithmetic, and `FinalSend` provisional subscription release decisions.
Callbacks never release completion-owned resources. Page/slice identity validation,
initial socket/framing checks, pre-poll `begin_io`, and final HTTP consumption also
remain in Racer. The generic engine does not finish an exchange or authorize data.

## Validation and performance

From the workspace root (`cmd/racer-dataplane`), use external TERM timeouts and
`CARGO_BUILD_JOBS=2` for `cargo test -p http-splice` and focused root relay tests.
The existing `opaque_relay_benchmark` compares materialized/opaque transfers with
identical byte counts. Its thread CPU/wall samples include all three endpoints
on the fixture's driving thread and are a regression signal, not
a throughput claim or isolated syscall benchmark. Keep baseline and post-change
results in the task checkpoint; do not rerun unchanged phases without a question.
`loopback_backpressure_thread_cpu` separately exercises immutable Delivery on TCP:
four samples (first warmup), 512 MiB/sample, 1024 page sends, a small send queue,
and a paced receiver. Sender thread CPU includes future/reactor turns but excludes
receiver reads, validation, and pacing. Neither benchmark is a production capacity
claim. Run once before and once after a changed mechanism under the same bounds.
