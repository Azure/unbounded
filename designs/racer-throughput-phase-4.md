# Racer throughput architecture: Phase 4

## Authority and execution

The approved Phase 4 scope and `/home/azureuser/design.md` govern this work on
`racer-throughput-architecture`. One phase owner works only in this worktree;
no subagents. The user approved the cross-package work and resolution of the
prose contradictions below. Parent integration retains eventual merge ownership.
This plan is committed before code. Preserve existing tests, cancellation,
admission, credential isolation, and accepted-operation completion fences.

Every test, benchmark, or script invocation uses
`timeout --signal=TERM --kill-after=10s 300s ...` (shorter allowed), with tool
timeout <=320000 ms. Go tests additionally use `-timeout=5m`. Split suites,
investigate timeouts, and stop/reap owned children. All scratch stays here.
Run `cargo fmt` for Rust and `make fmt` for Go. Before each incremental commit,
inspect status, diff, and the last ten commits; stage intended files only.

## Inspected baseline and approved corrections

Rust paths below are relative to `cmd/racer-dataplane`.

- `src/read/serve.rs:409-421` serves Acquire by obtaining a verified plaintext
  PageResult then returning only ciphertext. `src/read/fill.rs:479-525` reserves
  plaintext and decrypts disk copies; `:633-679` decrypts requester copies. CopyOnly
  already uses ciphertext (`:419-460`). Acquisition must retain origin authority,
  singleflight, corruption fallback, and conditional disk invalidation.
- `src/read/metadata.rs:593-618` loses bootstrap intent in a Metadata operation;
  `:623-632` uses one GET only on the local bootstrap path. Forwarding deliberately
  rejects Metadata/Page substitution (`src/security/forwarding.rs:574-600`). Add
  an explicit versioned Bootstrap operation rather than weakening that check.
- `pkg/racersdk/value.go:96-143` opens one pinned continuation after the previous
  body ends. `designs/racer-sdk.md:35-38,109-143` explicitly rejects a per-Value
  scheduler. The canonical design requires concurrent pooled page requests
  (`/home/azureuser/design.md:48-53`); update the SDK prose and implementation.
- `src/memory/pool.rs:63-109` allocates fresh buffers and charges live capacity.
  Pooling must include retained idle allocation capacity and zeroization, rather
  than merely renaming these allocation methods. Mutable kernel buffers must
  remain exclusively owned and address-stable through completion.
- `src/security/protocol.rs:21` identifies the current profile as racer-peer-v2.
  Certificate/session authentication remains mandatory. Shared-key MAC must be
  purpose-separated from payload and credential encryption and bind canonical
  request content without replacing original/destination proofs.

Phase 3 interfaces remain intact: distributed pre-session ingress, target-worker
connection reservations, same-owner admitted local contexts, runnable drivers,
indexed memory/admission, and incremental admitted HTTP heads. See the Phase 3
handoff for exact ownership APIs and prior results.

## Implementation sequence and acceptance

### A. One acquisition lifecycle

Represent ciphertext-ready separately from verified-plaintext-ready. One elected
ingest retains its original ciphertext and nonce/tag; candidate/relay delivery
does not require decrypting it. Local readers lazily join one decrypt and share
the verified allocation. Unverified ciphertext cannot be delivered to clients.
Requester AEAD is mandatory. Corruption rejects only the observed copy, permits
the existing bounded fallback, and conditionally invalidates only the observed
disk incarnation. Keep origin 401/403 waiter isolation, origin 412 copy probing,
membership/UID/key admission, and completion-owned budgets/contexts.

Tests count serving-candidate and requester decryptions, elected ingests, shared
plaintext identity, disk corruption fallback, cancellation, and concurrent
authorized/unauthorized readers.

### B. Explicit authenticated remote bootstrap

Introduce a coordinated peer profile with Bootstrap request and response types:
metadata plus optional encrypted page zero; empty objects have no page. Bind
intent, object, strong ETag, total length, page number, envelope, exact body
length, request proof, and existing deadline/attempt/link credits. HEAD remains
metadata-only. Update Rust and Go canonical codecs, public protocol text, and
cross-language vectors together. Preserve destination authority on successful
and negative replies and original requester proofs across relays.

Compatibility policy: old profiles are explicitly rejected for new operations;
never silently substitute Metadata/Page or retry as a different intent. Any
negotiation must be authenticated and selected before operation execution.
Tests cover exact one-logical-bootstrap/one-origin-GET cold noncandidate reads,
empty values, credential failures, version changes, corrupt page zero, and
mixed-profile rejection. Document the final wire/storage compatibility matrix.

### C. Bounded pooled SDK pages and FD delivery

Add a configurable sane per-Value window bounded by connection-pool capacity.
Schedule bounded pinned pages, emit in byte order, and preserve initial metadata,
ETag, total length, per-response checks, and the original context deadline.
Avoid object-sized descriptor/buffer collections. Close and terminal failure
cancel all scheduled requests and release bodies/workers without draining.
Retain the Read/Close API and partial-error semantics.

Add FD-aware WriteTo with Linux splice for the parsed Content-Length body,
including parser read-ahead and exact framing boundaries. Provide a portable
bounded-copy fallback and a typed loopback-server sink that preserves net/http
response ownership. Tests must observe actual splice execution, ordered bytes,
framing exclusion, short writes, errors, cancellation, and bounded scheduling.

### D. Real recycling and immutable send ownership

Recycle worker-local pipes, aligned disk staging, and suitable page/network
buffers. Bound and admit retained capacity, return only after exclusive ownership
and kernel/crypto fences, and zeroize payload secrets before reuse/release.
Separate immutable reactor send ownership from mutable receive ownership so
ciphertext can be submitted without an avoidable full-page copy. Audit every
kernel pointer lifetime and cancellation path. No vmsplice reuse without a true
kernel reuse fence. Origin splice is useful only up to an actual userspace crypto
boundary; do not claim end-to-end zero-copy encryption.

Tests verify allocation/descriptor reuse, retained charges, cache isolation,
concurrent ownership, short I/O and canceled completions.

### E. Checksums and rotating request MAC

Implement canonical CRC64 with accelerated hardware dispatch and portable
equivalence, executed on crypto workers and fused with existing passes where
practical. Version records explicitly: supported old records remain readable or
become a safe miss; unknown/corrupt records never yield client bytes. CRC is not
authentication; retain page AEAD and verify before plaintext publication.

Add rotating shared-key request MAC with existing standard primitives, purpose
separation, key overlap/retirement, certificate-bound sessions, monotonic session
counters, freshness, original requester and authoritative destination proofs.
Coordinate profile changes with B. Tests cover rotation overlap, retired/missing
keys, canonical mutation, replay, cross-purpose misuse, profile mismatch, CRC
vectors/acceleration equivalence and corrupted records.

## Verification and delivered mapping

Run focused bounded groups with each implementation commit, then the full Rust
library (baseline 739 pass/7 ignore), production (13/2), conformance (19/1),
doctests (31), affected Go packages, and all 13 explicitly selected strict actual
Application process gates. Use the existing process README and fixture child
guards. Add exit counters/tests for the new behavior, not only existing passes.
Record exact commands, counts, compatibility decisions, implementation files,
and unresolved goals here. Hardware ignores are not passes; report RDMA, NUMA,
NIC and unsupported accelerator gaps honestly. Finish with a clean worktree.

Implementation results will be recorded below as each tested increment lands.

### Incremental results

- `8c02fa8e`: plan committed before code.
- `02b022f8`: sealed immutable SendBuffer, HTTP immutable subranges, peer HTTP and
  native fallback send original ciphertext directly. Mutable receives retain
  IoBuffer ownership. Simulation never creates mutable aliases for sends. New
  tests check backing-pointer identity, short sends, retained charges and both
  cancellation-completion orders; a compile-fail test rejects ciphertext receives.
- `ac5adc7c`: ClientConfig.PageWindow defaults to four, capped by the
  pool. Connection permits precede worker creation, speculative pages never wait
  for permits, and pending response storage is bounded by the window. Read consumes
  in order; Close cancels and joins every worker. Bootstrap remains header-only
  until consumption and continuations start after page zero. Updated SDK prose
  and fake-origin assertions for concurrent page arrival with exact ordered bytes.
- `5dc0c999`: actual worker-local pipe descriptor recycling after completion fences.
  Empty pipes retain admission while idle; partially drained pipes close. Lease
  ownership uses weak pool links, avoiding a reactor cycle. Updated cleanup tests
  distinguish idle capacity from active delivery; descriptor-identity tests prove
  actual reuse. Production cancellation initially waited for idle charges to reach
  zero and hit its internal bounded stall assertion; it now checks zero active
  leases and verifies full original cancellation, bytes, and resource contracts.
- `d56a4cc9`: WriteTo uses Linux splice for parsed fixed-length bodies
  and portable bounded copying otherwise. FDSink counts actual spliced bytes;
  loopback ServeHTTP uses explicit net/http Hijack/flush/close ownership. Spliced
  source bodies close through Transport; its private counters are never bypassed
  for pooled reuse. Tests check multi-page ordered bytes, framing, cancellation,
  subsequent requests, and loopback HTTP responses.

Commands executed from the crate for Rust and worktree root for Go; all tests
used the mandatory external `timeout --signal=TERM --kill-after=10s 300s` prefix:

| Command after timeout prefix | Result |
| --- | --- |
| `cargo fmt` | Passed. |
| `cargo test --locked --all-features --lib peer:: -j 2 -- --test-threads=2 --quiet` | 31 passed, one explicit ignore. |
| `cargo test --locked --all-features --lib immutable_ciphertext_send -j 2 -- --test-threads=2 --quiet` | One passed. |
| `cargo test --locked --all-features --lib -j 2 -- --test-threads=2 --quiet` | 740 passed, seven explicit ignores, 56.39 s. |
| `cargo test --locked --all-features --doc -j 2 -- --test-threads=2` | Six ordinary and 26 compile-fail tests passed. |
| `env GOTOOLCHAIN=go1.26.6 make fmt GO_PACKAGE_DIRS=./pkg/racersdk GO_PACKAGE_PATTERNS=./pkg/racersdk/...` | Passed, zero lint issues. |
| `env GOTOOLCHAIN=go1.26.6 go test -timeout=5m ./pkg/racersdk -run 'TestValueWindow\|TestClient' -count=1` | Passed, 5.214 s after creating missing worktree-local tmp fixture directory. |
| `env GOTOOLCHAIN=go1.26.6 go test -timeout=5m -race ./pkg/racersdk -count=1` | Passed, 19.710 s after correcting the two arrival-order assumptions described above. |
| `cargo test --locked --all-features --lib memory:: -j 2 -- --test-threads=2 --quiet` | 43 passed after retained-pipe accounting updates. |
| `cargo test --locked --all-features --lib client::listener:: -j 2 -- --test-threads=2 --quiet` | 28 passed. |
| `cargo test --locked --all-features --lib -j 2 -- --test-threads=2 --quiet` | After pipe recycling: 741 passed, seven explicit ignores, 65.60 s. |
| `cargo test --locked --all-features --test production_dataplane -j 2 -- --test-threads=2 --quiet` | 13 passed, two explicit ignores, 5.90 s. |
| `cargo test --locked --all-features --test production_dataplane --test client_origin_conformance -j 2 -- --test-threads=2 --quiet` | Conformance 19 passed/one ignore; production initially found the idle-charge stall described above. |
| `env GOTOOLCHAIN=go1.26.6 go test -timeout=5m -race ./pkg/racersdk -count=1` | After FD delivery: passed, 20.754 s. |
| `env GOTOOLCHAIN=go1.26.6 go test -timeout=5m -race ./pkg/racersdk -run 'TestValueWriteTo\|TestValueServeHTTP' -count=1` | Final cancellation error normalization: passed, 1.878 s. |
| `env CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_RUNNER='sudo -n env RACER_THROUGHPUT_STRICT_BASELINE=1' cargo test --locked --test process_restart -j 2 -- --ignored --test-threads=1 --quiet` | All 13 strict actual-Application gates passed, zero ignored/failures, 112.34 s. |
| `cargo test --locked --all-features --no-run -j 2` | All library, binary, and integration targets compiled. |
| `cargo test --locked --all-features --doc -j 2 -- --test-threads=2` | Final: six ordinary and 26 compile-fail tests passed. |
| `env GOTOOLCHAIN=go1.26.6 go test -timeout=5m ./internal/gantry/racer ./internal/gantry/mirror -count=1` | Mirror passed, 12.550 s. Racer exposed another sequential-arrival fixture assumption. |
| `env GOTOOLCHAIN=go1.26.6 make fmt GO_PACKAGE_DIRS=./internal/gantry/racer GO_PACKAGE_PATTERNS=./internal/gantry/racer/...` | Passed, zero lint issues after adding the test's sync import. |
| `env GOTOOLCHAIN=go1.26.6 go test -timeout=5m -race ./internal/gantry/racer -count=1` | Passed, 2.861 s. Fixture checks unique aligned offsets, exact bytes, credentials, pins, counts, and body closure independent of arrival order. |

No external timeout fired in these increments. These results do not yet establish
the full Phase 4 exit gates; acquisition/bootstrap, disk/page-buffer recycling,
CRC64 and rotating request MAC remain to be delivered.

### Current scope and compatibility

### Continued acquisition/bootstrap increment

Commits: `8b2b4dc7` implements the unified flight and v3 Bootstrap;
`a8ba9bd6` adds ciphertext-only memory residency, admitted provenance validation,
conditional allocation-identity invalidation, and the v3 RFC 9421 tag/vector update.
The latter full library run passed 744 tests with seven ignores (56.27 s).
Follow-up tests prove corrupted prefetched ciphertext is never exposed and falls
back to the retained original (two decrypt attempts, one ingest), and the actual
four-process test now issues a distinct HEAD and requires exactly one HEAD and
one GET across the entire bootstrap-plus-HEAD scenario (2.99 s). Ciphertext
reclamation uses a bounded ordered cursor so busy entries cannot starve later
idle entries; focused memory-cache verification passes nine tests.

The page flight now has ciphertext-ready state and a single elected plaintext
promotion in the same table. `UnverifiedPage` cannot enter client delivery;
`AcquiredPage` explicitly separates it from PageResult. CopyOnly and peer Acquire
can consume ciphertext without plaintext allocation/decrypt on pending/disk hits.
Plaintext waiters elect one promotion, preserving the original ciphertext, disk
invalidation token, remaining budget, and completion fences. Origin encryption
remains elected once; origin-produced plaintext is retained for concurrent local
readers. Requester copy validation remains mandatory and corrupt-copy fallback
retains existing bounded candidate policy. New counters expose page decrypts and
peer Bootstrap acquisitions.

Peer v3 adds explicit Bootstrap through codec, signature binding, forwarding,
candidate policy, dispatch, metadata coordinator and HTTP body transport. Empty
objects have metadata only. HEAD and Bootstrap refresh keys differ so a HEAD
cannot acquire body-fetch side effects. Bootstrap responses retain unverified
ciphertext until the local plaintext consumer enters the page flight. The four
actual-Application test proves a cold noncandidate gets one logical Bootstrap,
exactly one unpinned origin GET, no HEAD, zero candidate decrypts, and one requester
decrypt for a nonempty object (zero for empty). Signed checks cover destination,
intent, object, page-zero and presence/length agreement; wire tests reject v1/v2,
unknown profiles, and the old exchange target. Bootstrap uses HTTP because rail
selection needs the not-yet-known ETag; native fault tests now explicitly request
a pinned page and retain all required native fault coverage.

Compatibility supersedes the earlier partial handoff below: peer signed profile
and outer exchange are v3, requiring a coordinated upgrade, with no downgrade.
Existing v2 session endpoint/digest domains remain, but their signed profile is
v3. Records remain v1, control and client/origin HTTP remain unchanged. No Go peer
codec exists in this checkout; no unrelated Go control schema is changed.

Bounded verification for this increment (same mandatory external timeout prefix):

| Command | Result |
| --- | --- |
| `cargo test --locked --all-features --lib ciphertext_ready -j 2 -- --test-threads=2 --quiet` | One passed: zero ciphertext-serving decrypts, one shared plaintext promotion, one ingest. |
| `cargo test --locked --all-features --lib read:: -j 2 -- --test-threads=2 --quiet` | 109 passed. |
| `cargo test --locked --all-features --lib security:: -j 2 -- --test-threads=2 --quiet` | 59 passed. |
| `cargo test --locked --all-features --lib -j 2 -- --test-threads=2 --quiet` | 743 passed, seven explicit ignores, 61.77 s. |
| `env CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_RUNNER='sudo -n env RACER_THROUGHPUT_STRICT_BASELINE=1' cargo test --locked --test process_restart -j 2 -- --ignored --test-threads=1 --quiet` | All 14 selected process tests passed, 115.78 s. |
| `cargo test --locked --all-features --test production_dataplane --test client_origin_conformance -j 2 -- --test-threads=2 --quiet` | Production 13 passed/two ignores, conformance 19 passed/one ignore. |

Initial failures were fixture expectations: empty Bootstrap is HTTP 200 (the
range measurement oracle requires 206), remote absence now permits explicit
Bootstrap, and native-fault injection must use a known pinned page. No tests were
removed or required coverage weakened. No external timeout fired.

Still outstanding: comprehensive new Bootstrap corruption/credential/version
mutation cases, durable ciphertext-only memory residency, full disk/page/network
recycling, CRC64/versioned records, and rotating shared-key MAC. Existing mutation,
credential retry, disk invalidation, version and cancellation regressions pass.

### Historical partial handoff (superseded above for A/B)

### Aligned staging recycling increment

Committed as `15bbd902`. Follow-up production assertions now compare ciphertext
charges to the explicit retained-staging byte count after active pages are freed;
the full production target passes 13 tests/two ignores (5.99 s). Final doctests
pass six ordinary plus 26 compile-fail contracts. An exploratory HTTP staging
pool exposed six legacy release assertions in its focused group; that uncommitted
experiment was removed and is not part of the delivered implementation. Network
buffer recycling therefore remains outstanding.

Slabs now owns one retained aligned staging allocation per worker. Its original
reservation remains charged while idle; allocation reuses only matching geometry
and rebinds to a newly admitted cache reservation. Last-owner Drop zeroizes bytes,
releases dirty completion charges and recycles only after kernel ownership fences.
Pool links are weak, with no reactor cycle. Read/write staging allocation uses
this path; admission pressure reclaims idle staging before retrying a bounded
reservation, and writer drain releases retained staging. Cancellation and failed
write tests still require zero active owners and zero charges after explicit idle
reclamation. A pointer-identity test proves real reuse and zeroed contents.

Verification, with the mandatory external timeout prefix:

- `cargo test --locked --all-features --lib -j 2 -- --test-threads=2 --quiet`:
  746 passed, seven explicit ignores, 58.90 s.
- `env CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_RUNNER='sudo -n env RACER_THROUGHPUT_STRICT_BASELINE=1' cargo test --locked --test process_restart -j 2 -- --ignored --test-threads=1 --quiet`:
  all 14 process gates passed, 135.22 s.

Still remaining: network/page allocation recycling, CRC64 hardware dispatch and
versioned records, rotating shared-key MAC, and expanded Bootstrap mutation and
credential-specific concurrency coverage. Phase 4 is not complete.

This is a partial Phase 4 implementation, not full phase acceptance.

| Goal | Delivered | Still required |
| --- | --- | --- |
| A | Existing AEAD and acquisition contracts preserved by regression tests. | Unified ciphertext-ready/lazy-plaintext lifecycle, serving/requester decrypt counters, shared-decrypt exit tests. |
| B | Existing explicit v2 operation matching retained. | Versioned Bootstrap, Go/Rust/public protocol coordination, exact remote origin counts and mixed-profile tests. |
| C | Bounded concurrent pinned pages, ordered Read/Close, Linux FD splice, portable copy branch, typed sink and loopback HTTP handler. | Broader multi-Value contention/deadline and splice-error edge coverage; spliced UDS connections deliberately close rather than pool. |
| D | Actual recycled worker-local pipes and immutable ciphertext sends. | Aligned disk, network/page allocation recycling and their retained-capacity/zeroization tests; origin splice evaluation. |
| E | Existing certificate sessions, purpose-separated keys and AEAD preserved. | Accelerated CRC64 on crypto workers, record versioning, rotating shared-key MAC and compatibility tests. |

Peer wire remains v2 and records remain v1. No protocol negotiation, shared-key
MAC, CRC64 format, or compatibility downgrade behavior has been introduced.
Client/origin HTTP framing remains v1. SDK changes add optional config and methods;
per-Value continuations may now reach origin out of order, while emitted bytes
remain ordered. Parent merge has not been performed.

Hardware evidence is local Linux TCP/UDS splice, io_uring and O_DIRECT process
coverage. No RDMA/NIC/NUMA throughput or CRC hardware measurement is claimed.
Process checks after the selected suite found other host workloads under
containerd and the parent checkout's `tmp/racer-sdk-throughput`; those were not
owned by this invocation and were left running. Fixture guards reaped this
worktree's process-test children.
