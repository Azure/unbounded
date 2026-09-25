# Racer dataplane implementation coordination

Implementation baseline: `6938c593`. All component owners work in this isolated
worktree, stage only their owned paths, and commit reviewed changes. The integration
owner alone edits Cargo manifests, application composition, configuration, errors,
and final documentation. Existing tests are preserved and updated to test behavior.

## Ownership

- Runtime owner: `src/runtime/`, including memory through a focused delegate.
- Storage owner: `src/store/` and `src/store.rs`.
- Security owner: `src/security/` and `src/security.rs`.
- Control owner: `src/control/` and `src/control.rs`.
- HTTP owner: `src/http/` and `src/http.rs`.
- Topology owner: `src/topology/` and `src/topology.rs`.
- RDMA owner: `src/rdma/` and `src/rdma.rs`, native adapter assets.
- Read owner: `src/read/` and `src/read.rs`, through focused delegates.
- Client/origin owner: `src/client/`, `src/origin/`, their module roots, and `src/model/`.
- Peer owner: `src/peer/` and `src/peer.rs`.
- Integration owner: application, configuration, telemetry, package/build files,
  cross-component verification, and documentation.

All source paths above are relative to `cmd/racer-dataplane`. Components preserve
existing public APIs where possible and add explicit methods for missing ownership
handoffs. No owner overwrites another owner's files. Cross-component compiler errors
are reported with the exact required interface. Constructors stay side-effect-free.

## Decisions

The newer paired-worker and control contracts supersede the temporary design's
single-core combined role, environment shares, and mounted node certificates.
The SDK implementation defines client/origin HTTP compatibility: lowercase hex
object paths, quoted strong ETags, absolute expiration in Unix milliseconds, strict
single ranges, and exact opaque context bytes. Page payloads use XChaCha20-Poly1305;
peer authentication uses Ed25519. Control uses TLS and strict bounded JSON.

Unknown wire details are versioned, domain-separated, deterministic encodings with
test vectors. Placement uses deterministic SHA-256 inputs and integer weighted
ranking. No default randomized hasher participates in distributed decisions.

Completion ownership is mandatory: submitted operations retain buffers, FDs, quota,
and storage/NIC leases after request cancellation. Real implementations must not
replace production operations with test fakes or unconditional success. Optional
RDMA falls back to HTTP when native capabilities are unavailable.

## Client/origin integration decisions

The user explicitly confirmed `cmd/racer-dataplane/CLIENT_ORIGIN_API.md` as the
approved client/origin contract. Read the copy on the original branch at
`/home/azureuser/code/unbounded/cmd/racer-dataplane/CLIENT_ORIGIN_API.md` when the
implementation worktree baseline does not contain it. Verify against the actual Go
SDK and raw Unix sockets. Control-plane design does not replace this wire contract.

Client retains `ReadKind::Head` and adds `HeadPinned { etag: StrongEtag }`.
Read coordination must handle both variants. Shared `Error` now includes
`MethodNotAllowed`, `HeaderTooLarge`, `Forbidden`, `NotFound`, `BadGateway`,
`Internal`, `UnsatisfiableRangeWithLength(u64)`, `OriginRejected`, and
`OriginForbidden`. Only the two origin-specific rejections may trigger caller-only
flight failure and reelection; `Unauthorized` is not an origin retry signal.

HTTP raw parsing must require exactly one separator space after `Authorization:`
and `Racer-Metadata:`. Header decoding must reject a missing separator before it
loses raw framing information. Client heads are bounded at 32 KiB; opaque fields
and ETags at 8192 bytes. Origin keeps `OriginClient::new` and may add `with_buffers`
for admitted body allocation. Client listeners expose accepted-connection futures
and require explicit application polling.

## Verification

### Integration audit gates

- Application owner: parent unblocked composition test compilation by unwrapping
  both `WorkerApplication::assemble` results. Pre-start `poll_budgeted(0)` returns
  `Unavailable`; assert the fail-closed pre-start contract, not success.
- Runtime owner: combined test run found startup/drain races in
  `later_pair_allocation_failure_stops_and_joins_started_pair` (missing shutdown)
  and `owned_start_drains_and_joins_both_pinned_local_services` (`Unavailable`).
- Store owner: accepted dirty staging must use completion admission after stop;
  expired unsubmitted dirty copies must be discarded without stranding drain.
- Read owner: call safe idle memory eviction on byte-pressure admission failures;
  count-only eviction cannot recover exhausted page-byte budgets.
- Application owner: expected dirty-copy pressure/I/O failures are disposable cache
  failures, not node-fatal errors. Serialize omitted-key retirement across all worker
  memory, late fills, writer, crypto/transport fences, checkpoint invalidation, then
  acknowledge registered keyring barriers. Cache removal also retires memory.
- Application owner: select one node-wide checkpoint generation, validate exact
  worker set/current ownership and all capacities/keys, then distribute shards.
  Independent per-worker selection can restore inconsistent generations.
- RDMA owner/application: configure explicit native rail mappings before advertising
  availability; individual session expiration must fail that transfer and allow
  HTTP fallback without stopping unrelated workers.
- Control/native lifecycle: regular-file reads/fsync and native registration/QP
  destruction are synchronous today. Do not describe those paths as nonblocking;
  move accepted filesystem I/O onto the reactor and provision native resources
  outside request turns with lifetime-safe completion ownership.

Each owner adds meaningful success, failure, and edge tests and runs targeted checks.
The integration owner runs formatting, all-target/all-feature checks, tests, real
socket and storage exercises, and audits remaining placeholders and unreachable
operational paths before cherry-picking commits to the original branch.
