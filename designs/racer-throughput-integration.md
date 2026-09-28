# Racer throughput integration

The throughput branch incorporates concurrent `racer-v2` work through
`6499e8e4`. The integration preserves the newer controller reconciliation,
content-type metadata, SDK reserved connection pools, range APIs, and Gantry
integration alongside the six throughput phases.

## Compatibility decisions

- Peer protocol v4 remains a coordinated-upgrade boundary. Certificate sessions,
  request MACs, authority proofs, and completion fencing remain mandatory.
- Disk writers emit record v3: CRC64 plus optional content-type metadata. Readers
  accept released v1 and v2 records, with an explicit v2 compatibility test.
  Intermediate throughput-branch v2 CRC records were never a released format;
  they are not a supported migration source. Slab contents remain disposable
  cache data, verified before client delivery.
- Control deltas are an optional v1 extension with exact base/target hashes and
  full-publication fallback. The Go-generated delta fixture is consumed by Rust.
- SDK `ClientConfig.PageWindow > 1` enables bounded concurrent continuation
  pages. Zero preserves the concurrently introduced single pinned-remainder
  default. Windows share existing pool admission and never wait for extra slots
  while retaining all capacity themselves. Small-object reserved pools retain
  their existing behavior.
- SDK FD delivery now accounts for consumed bytes in the custom connection
  pool, allowing completely framed spliced responses to reuse their source
  connection. Concurrent-window wrappers use the bounded copy fallback.
  `ServeHTTP` preserves original content type and accepts only unconsumed
  full-object Values.

## Integration fixes

Runnable-aware client scheduling now watches actual socket hangup while a read
is pending. The watch owns connection admission through its cancellation CQE
and is canceled and reaped before response submission, avoiding a queue-slot
dependency under small budgets. A half-close is not treated as full disconnect.

The controller's pure rotation planner includes preparation within the 24-hour
activation interval. Prepared delta generation uses the assigned publication
sequence and membership version, preserving the newer canonical preparation API.

## Final verification

All test commands used an external 300-second timeout; Go tests also used
`-timeout=5m`. No external timeout fired.

| Check | Result |
| --- | --- |
| All-feature Rust library | 779 passed, 7 explicitly ignored |
| Real executable/io_uring process gates | 15 passed |
| Production dataplane | 15 passed, 2 explicitly ignored |
| Client/origin conformance | 19 passed |
| Explicit Go SDK/Rust UDS conformance | 1 passed, including all Go fixtures |
| Ownership/documentation tests | 32 passed |
| Controller, wire, SDK, Gantry adapter under Go race detector | Passed |
| Gantry agent/mirror/adapter and Racer operator tests | Passed |
| Scoped `make fmt` including Go lint | Zero issues |
| Rust formatting and whitespace checks | Passed |

The final record compatibility assertion and SDK HTTP metadata test were run
after the library suite. The explicit SDK conformance fixture was adjusted to
release idle HTTP staging before asserting live-owner accounting and to keep its
Unix socket path within the kernel limit in the longer worktree path.

## Remaining architectural limits

The phase reports remain authoritative about measured coverage. This change does
not establish 100,000-node throughput or GB200 multi-rail throughput. The native
adapter built successfully but discovered zero eligible type-2B provider ports
on this host, so no real-provider DMA result is claimed.

Page ownership remains stable and independent of rail assignment. A worker uses
only funded, NUMA-compatible rails; other paths fall back to HTTP. Cross-worker
rail dispatch and strict registered-memory NUMA binding remain follow-up work.
Origin UDS reads still use receive operations rather than an all-splice path.
CRC64 remains a separate pass from AEAD. Periodic checkpoints are best-effort
cache recovery hints, pause new writes during a cut, and provide no bounded
power-loss durability guarantee. Cold exact placement still requires work
proportional to membership size when a cached ranking cannot be reused.

An exploratory strict Clippy `-D warnings` run reported existing and new style
diagnostics; this is not a clean strict-Clippy result. No blanket lint exclusions
were introduced. See the phase-six report for the pre-integration diagnostic
counts and hardware limitations.
