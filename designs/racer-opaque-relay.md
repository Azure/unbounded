# Bounded opaque HTTP transit

Base: `cccae518171425fae7e41391772f68d98a7940c4`.

## Item and evidence

Implement one performance item: eliminate full-page userspace transit storage and
receive/send copying for HTTP ciphertext relays. Endpoint decryption and native
RDMA transfer remain separate existing paths. No dependencies or wire changes.

The supplied `tmp/racer-stage7-results.md:106-125` profiles show pinned reactors
near one core with receive/send copy, kernel page clearing and residual recycle
costs, while crypto workers have spare capacity. That report also rejects its
fleet throughput comparison because node coverage failed. This change makes no
cluster-level performance claim.

`peer/transfer.rs` now exposes an unfinished HTTP response to the relay instead
of allocating its page-sized WireBuffer. `security/forwarding.rs` shares reverse
verification between materialized and opaque responses; `security/protocol.rs`
shares canonical page-field encoding. The opaque path validates signed logical
metadata against exact transport length and the outstanding request digest before
forwarding anything. The original and reverse signatures are preserved.

`http/relay.rs` owns fixed-length transit, including read-ahead and fallback.
`memory/pipe.rs` adds bounded socket-to-pipe receipt and a validated-connection
send fast path. Both connections, pipe and Relay reservation survive readiness
cancellation fences. The body loop never appends an error after a success head.

## Local measurements

Explicit ignored tests provide bounded reproducible mechanism comparisons. Use
`CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_RUNNER='sudo -n'` only on hosts whose
io_uring policy requires it. Both use real loopback TCP and eight 16 MiB + 16-byte
pages per sample. Order is old/new/new/old. No CPU affinity or NIC isolation was
applied. Test profile is optimized with assertions enabled, not release.

The isolated `opaque_body_cpu_benchmark` puts producer and consumer on separate
threads. It compares the old WireBuffer -> CiphertextPage -> send -> recycle
sequence with the new bounded body loop, measuring the relay thread only:

| Path | Wall ms | Relay CPU ms |
| --- | ---: | ---: |
| Materialized | 185.753 | 185.688 |
| Opaque | 121.589 | 121.575 |
| Opaque | 110.080 | 110.043 |
| Materialized | 156.655 | 156.620 |

Mean measured relay CPU falls from 171.154 to 115.809 ms (32.3%) for 128 MiB
of transit. This is a local body-only result; executor polling and loopback kernel
work are included. It does not establish production throughput or NIC saturation.

The signed `opaque_relay_benchmark` includes all three roles on one thread,
handshakes, canonical verification and requester AEAD. Before the final removal
of repeated destination socket validation, samples were:

| Path | Wall ms | Combined thread CPU ms |
| --- | ---: | ---: |
| Materialized | 401.555 | 401.517 |
| Opaque | 442.840 | 442.770 |
| Opaque | 452.397 | 452.384 |
| Materialized | 495.096 | 495.044 |

This comparison is neutral and order-sensitive; do not claim an end-to-end gain.
The deterministic regression establishes zero relay Ciphertext/Plaintext admission
while forwarding the full signed encrypted page, with one bounded pipe.

## Validation and operation

New real-socket tests cover signed Bootstrap and Page, fragmented heads/body,
read-ahead, slow reader, exact ciphertext and decrypted plaintext, keepalive,
truncation after signed success, source stalls and destination backpressure,
deadline/cancel/drop fences, disconnect, pipe/relay admission saturation and
fallback after pipe bytes are already buffered. Canonical/reverse verification
tests reject digest, length, authority, unknown fields, path and hop substitution.

Existing full-page relayed-layer simulations and native tests are included in the
all-feature library suite. Production and client/origin integration run separately.
All commands are externally TERM/kill bounded to 300 seconds.

| Check | Result |
| --- | --- |
| All-feature library | 797 passed, 10 explicit ignores (including both new benchmarks) |
| Final boxed-owner peer regression | 3 passed, 1 benchmark ignored |
| Production dataplane integration | 15 passed, 2 explicit ignores |
| Client/origin conformance | 19 passed, 1 explicit ignore |
| Doctests | 32 passed, including 26 compile-fail contracts |
| No-default all-target compile | Passed |
| Rust formatter | Passed |
| Scoped `make fmt` with `GOTOOLCHAIN=go1.26.6` | Passed, zero lint issues; incidental SDK whitespace excluded |
| Strict all-target Clippy | Blocked by repository-wide style/type-size findings |

Strict Clippy reported 97 library diagnostics and 123 including tests before the
new RelayResponse variant was boxed to remove its size warning. Existing examples
include nested conditionals in `memory/pipe.rs`, `security/forwarding.rs`, and
`read/flight.rs`, plus oversized existing enums. The final focused tests compile
the boxed owner successfully. No claim of a clean repository-wide Clippy run.

Automatic on Linux. No new configuration, capabilities or dependencies. Existing
pipe quota is shared with client delivery; budget two descriptors per active pipe
and at most 64 KiB kernel capacity per admitted pipe. Read-ahead/fallback uses at
most one additional admitted zeroizing 64 KiB allocation per active transit.
Kernel pipe-size growth is best-effort. Full deployment/parent A/B belongs after
the separate SMT change and a healthy measurement window; no cluster operations
or image publication are part of this item.
