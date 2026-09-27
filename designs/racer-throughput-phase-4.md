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
