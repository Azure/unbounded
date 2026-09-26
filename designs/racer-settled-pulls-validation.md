# Settled pull failure: accepted connections bypass idle reclamation

Base: `06a6002a3a533103dc19cb5cfa21efd313ba80d0`.
Worktree and branch: `tmp/racer-fix-settled-pulls`, `racer-fix-settled-pulls`.

## Fresh live evidence

Read-only AKS context: `joolshev-scale-test`, September 26, 2026. Every kubectl
used request timeout 20s and subprocess timeout 30s. Outer diagnostic commands
were bounded at 120s or less. Temporary tracing used existing net-node access,
automatic detachment, and no host sysctl or durable remote state changes.

The fresh baseline is in the sibling validation worktree:
`tmp/racer-v2-cluster-validation/tmp/racer-06a600-settled-investigator-1.{json,md}`.
It verified the expected dataplane image and loadgen concurrency 1, layer
concurrency 4, eight layers, and verification enabled. Results were zero complete
images, seven layer 503s, three premature EOFs, and six successful layers. Two
sampled nodes had stable pod identities/restarts; node `vmss00004b` did not, so
its manifest 503 is not treated as settled evidence. Its previous dataplane log
reported `DeadlineExceeded`. Fleet Prometheus still reported zero successful
five-minute pull rate; this is context, not causal evidence for this fix.

The decisive synchronized capture is this worktree's ignored
`tmp/unbounded-net-node-kwgql-wire-9.json`. Node:
`aks-ddsv6-84072342-vmss0000a8`; dataplane pod `racer-dataplane-gqxx4`.
The explicit GET is Gantry's layer digest
`sha256:515e6f5a38f01eda728d0b6497ff117551ea816b14b684ffae35d2134d710c9e`,
expected length 68,935,168. Ten sequential GETs all returned HTTP 503. Each
individual GET window contains one local client-listener acceptance rejection at
connection usage 64, limit 64, with at least 15 or 16 idle pooled sockets.

Examples, Unix timestamps:

| Probe | GET starts | Acceptance rejects | 503 received | Idle lower bound |
|---|---|---|---|---|
| 0 | 1790435712.1485176 | 1790435712.1685746 | 1790435712.1692328 | 15 |
| 1 | 1790435712.3693209 | 1790435712.3701642 | 1790435712.3705463 | 15 |
| 2 | 1790435712.5706230 | 1790435712.5713272 | 1790435712.5717874 | 16 |

`tmp/correlate.py` asserts exactly one matching rejection per probe window and
writes all ten rows to `tmp/accepted-correlation.md`. This is same-node,
per-probe-window correlation at acceptance, not a unique HTTP request-ID match
across the Unix socket. The capture only decodes selected TCP peer headers.
No page payloads or credentials are retained.

The live ELF was inspected before probes were attached:

- `tmp/unbounded-net-node-kwgql-disasm.log`: reserve_inner virtual address
  `0x150da0`, rejection `0x151564`, and executable segment file offset difference
  4096. Probes use file offsets `0x14fda0` and `0x150564`.
- `tmp/unbounded-net-node-kwgql-from_accepted-disasm.log`: return address
  `0x2278e3` proves the rejected reservation is inside from_accepted. Its caller
  `0x215ad5` resolves to ClientListeners::poll_budgeted. Accepted FD is 75 in the
  decisive capture.
- `tmp/unbounded-net-node-kwgql-drop-disasm.log`: the pool state and hash entry
  layout were established from actual idle-return code. The trace sums idle Vec
  lengths in the first 20 hash buckets, yielding a lower bound, not an inference
  from empty kernel socket queues. It observes the worker's last returned pool.
  Production constructs one shared outbound pool (`src/app.rs:531`).

Earlier captures are retained as leads, including failed bpftrace compilation
attempts. Only capture 9 waits for attachment before probing; earlier captures
must not be used for synchronized idle-count claims. A final bounded pgrep found
no bpftrace process on the traced host.

## Cause and fix

Paths below are relative to `cmd/racer-dataplane`.

At the base, `ConnectionLease::from_accepted` reserves connection quota directly
(`src/http/pool.rs:100-103`). ClientListeners drops the accepted socket on
Overloaded; PeerServer does likewise. Outbound checkout already reclaims an idle
lease before rejecting connection admission, but accepted connections bypassed
that path. Therefore an idle optimization prevents actual new image requests.
The prior outbound-only scope is explicitly recorded in
`designs/racer-fleet-layer-validation.md:59-70`.

`HttpPool::accept` now shares the bounded one-idle-lease reclamation operation
with checkout (`src/http/pool.rs:196-216`). It keeps the original accepted FD
until reservation succeeds or fails. Application wiring gives ClientListeners
the existing worker pool (`src/app.rs:729-739`); PeerServer uses its existing
Transfers pool (`src/peer/server.rs:77-87`). Connection count, per-neighbor
limits, deadlines, active leases, and completion fences retain their limits.

## Production-path old-fail/new-pass regression

`sdk_full_image_accepts_clients_with_idle_peer_capacity` extends the existing
four-peer SDK fixture in `src/peer/production_stream_tests.rs`. It completes
64 actual TCP/HTTP pool exchanges, fills the existing 64-connection quota with
idle leases, and then runs the actual Go SDK through production ClientListeners,
Coordinator, Fill/election, placement, signatures, crypto, and RangeStream.
Every layer page is remotely ranked; four layers run concurrently. Warm and
cold images verify manifest, config, and all eight layers, including exact
lengths and SHA-256, totaling 542,950,400 layer bytes per image.

With only HttpPool::accept restored to the old direct from_accepted operation,
the same regression fails immediately: SDK `EOF`, then manifest JSON EOF.
Restoring idle reclamation passes both complete images. All three SDK opt-ins
also pass concurrently, six complete images in total. Fixture slab paths now
include the test thread ID to isolate the new scenario from the existing one.

The pool regression additionally verifies the accepted FD remains usable,
reclaims an idle socket, preserves independently active charges, rejects fully
active capacity, releases rejected FDs, drains to zero, and rejects closed pools.

## Checks and limits

- Cargo format/check and release all-target/all-feature Clippy pass. Clippy
  retains existing warnings. The new immutable-loop condition was fixed.
- Release all-feature suite, serial: 563 library tests, two binary tests,
  18 conformance tests, nine production-graph tests, and 31 doc tests pass.
- The initial parallel suite had one failure in unchanged
  `runtime::reactor::tests::accepted_descriptor_is_retained_until_cancel_fence`.
  Its post-close raw-FD-number assertion (`src/runtime/reactor.rs:1629`) can race
  other tests reopening that number. FD reuse is an inference; the test passes
  in the full serial run. No test is removed or weakened.
- Scoped `make fmt` and `make lint` for `./pkg/racersdk`, including actionlint,
  pass with `GOTOOLCHAIN=go1.26.6`. Initial actionlint PATH mistakes were corrected.
- Go SDK and Gantry mirror tests pass with explicit test/outer timeouts.
- The production Containerfile builds local image
  `racer-dataplane:settled-pulls`, OCI index
  `sha256:32e4637df85ab44749af1a5133cf5fbe50b4cd43f7169008e7eacbb59b43cd88`.
  It was built before commit with revision `unknown`; later source change is
  test-only loop syntax. Publication should rebuild with the actual revision.

The full-image regression uses a fixture origin adapter and one worker per peer.
It does not run Gantry or Application startup; those paths have separate tests.
No new kind run or AKS rollout is claimed. Multi-dataplane kind creation remains
blocked by the reported host inotify limit. Native RDMA hardware and fleet
recovery remain unverified. Other captures still show ciphertext pressure and
remote failures; this fix establishes one preventable admission failure, not
the cause of every premature EOF or every fleet error. Only racer-dataplane
needs rebuilding. The parent owns deployment.
