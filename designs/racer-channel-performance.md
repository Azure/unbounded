# Focused channel benchmark

`cmd/racer-dataplane/runtime/examples/channel_bench.rs` uses only the runtime and
its existing libc dependency. It does not need io_uring privileges, an executor,
or additional Cargo dependencies. Run from `cmd/racer-dataplane`:

```sh
timeout --signal=TERM --kill-after=10s 300s cargo build --release -p uring-runtime --example channel_bench
timeout --signal=TERM --kill-after=10s 300s target/release/examples/channel_bench --transfers 1000000 --capacities 1,3,256 --repeats 3 --sample-every 1024
```

Use `--help` for all bounds and defaults. Optional `--cpus P,C` pins the producer
and consumer to distinct allowed Linux CPU IDs. The program validates the
initial mask and the resulting per-worker masks. It prints the complete
configuration, allowed CPUs, payload size, and build assertion mode. Choose CPU
IDs from that mask, ideally separate physical cores on the same NUMA node, and
record CPU topology, quota, compiler, source hash, and host load alongside results.

Each capacity gets an untimed warmup, then independent throughput and latency
passes for each repeat. Two dedicated threads own one endpoint each. Allocation,
thread creation, and affinity setup precede the shared start timestamp; startup
notification skew is included. FIFO sequence checks and sparse deadline checks
remain in both passes. The throughput pass never timestamps individual messages.
The separate latency pass timestamps every Nth message immediately before its
first send attempt and measures elapsed monotonic time just after receipt. Both
passes use the same payload layout. Latency includes sender backpressure, queue
residence, scheduling, and measurement overhead; it is not isolated atomic
operation latency, a closed-loop round trip, or an idle-to-wake measurement.
Sampling is deterministic and may alias periodic behavior; vary the interval
when that is a concern. Low sample counts do not support reliable tail claims.

Workers busy-spin and yield on retry at 1024-attempt checkpoints. No waiter is
registered, so this measures the no-waiter transfer path, not polling/waking.
Only capacity exhaustion is retried on send. Unexpected errors, premature
closure, FIFO mismatch, and sparse wall-clock deadline violations fail the pass.
Startup handshakes use timeouts, both workers are joined, and no startup barrier
can trap a worker after its peer's setup failure. An external process timeout is
still required to bound the whole matrix and any defect inside a channel call.

Focused example tests inject deadline expiry and terminal failure after eight
messages at either endpoint, then assert both scoped workers exit. Linux setup
tests pair one valid CPU with an invalid peer CPU in both directions and verify
that the valid worker really finished pinning. A bounded test-only handshake
forces an empty receive before the final send and sender destruction: the
consumer must recheck the queue after acquiring closure, not treat the earlier
empty observation as EOF. Hooks, counters, and synchronization are all gated by
`cfg(test)` and absent from the production measured loop. These fault-injection
tests use one-second internal deadlines and retain the external process timeout;
they do not detach watchdog threads or abandon child processes to implement
timeout assertions.

```sh
timeout --signal=TERM --kill-after=10s 300s cargo test -p uring-runtime --example channel_bench
timeout --signal=TERM --kill-after=10s 300s cargo clippy -p uring-runtime --example channel_bench -- -D warnings
```

Compare baseline and optimized release binaries using identical options, CPU
placement, compiler and host conditions. Preserve the baseline executable before
rebuilding, or use separate Cargo target directories. Record the channel source
hash at each build because safety work may happen concurrently. Report repeated
measurements rather than a single best run; smoke-sized runs only validate the
harness and cannot establish a performance improvement.

## Local implementation measurements

On October 5, 2026, release builds with Rust 1.96.0 were compared on Linux CPUs
0 and 2 (distinct physical cores on the same NUMA node), with one million
transfers, three repeats, and sampling every 1024 messages. The selected channel
uses conservative peer-cursor caches, conditional-subtraction indexing, and a
full check based on distinct cursors mapping to the same slot. Shared cursors
retain 64-byte alignment; endpoint handles no longer impose that alignment.

The fixed ablation phase produced these median throughputs in million messages
per second:

| Variant | Capacity 3 | Capacity 256 |
| --- | ---: | ---: |
| Original algorithm | 4.806 | 6.025 |
| Cached control | 5.128 | 6.693 |
| Modulo indexing instead of subtraction | 5.423 | 7.032 |
| Restored endpoint alignment | 5.023 | 6.828 |
| Unconditional peer acquisition | 5.192 | 5.950 |
| Selected exact-full check | 5.251 | 6.924 |

These are local observations, not proof of optimality or general speedups. Earlier
comparisons of the cached control showed capacity-3 losses of 5.5% and 8.2% in
opposite binary orders, while capacity 256 improved by 15.8% and 19.9%. The later
control did not reproduce the small-capacity loss, so the selected full check
cannot be credited with causally fixing it. Host load was uncontrolled, variants
ran serially, and modulo indexing also performed well. No hardware-counter or
cross-NUMA attribution was established.

The benchmark acquired test-only failure hooks during the ablation builds;
release loops and payloads were unchanged, but per-build harness hashes were not
captured. This limits provenance. Sparse latency samples and the no-waiter
workload do not justify executor, wakeup, production throughput, or tail-latency
claims. Wake gating, additional control-field padding, and batching remain
unmeasured options, not changes justified by these results.
