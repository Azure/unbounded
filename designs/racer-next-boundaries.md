# Racer next library boundaries

Current source references follow the within-main-crate consolidation; deferred
library extractions below remain separate decisions. These navigation updates do
not claim that historical validation or performance measurements were rerun.

## Current application source ownership

Paths here are relative to `cmd/racer-dataplane/`; reusable workspace crates keep
their own directories and APIs.

- Flat `src/admission.rs`, `src/runtime.rs`, and `src/worker.rs` own Racer admission,
  request scopes/reactor adaptation, and worker execution respectively. Worker
  services implement `uring_runtime::group::Service<RequestScope>` directly;
  local page-to-worker selection is `src/read/dispatch.rs::WorkerMap`.
- Flat `src/control.rs`, `src/security.rs`, `src/memory.rs`, `src/http.rs`,
  `src/telemetry.rs`, `src/topology.rs`, and `src/rdma.rs` retain their application
  policies. Peer signature/session handling is in `src/peer/protocol.rs`;
  signed-hop forwarding is in `src/peer/forwarding.rs`.
- Record framing is in `src/store.rs`; catalog and checkpoint owners remain in
  `src/store/`. Client responses are in `src/client.rs`. The `Origin` trait remains
  in `src/origin.rs`; source consolidation does not remove that testable boundary.
- Flat owners collect their tests under one bottom `cfg(test)` module. Service
  scenario trees remain under `src/{app,client,origin,read,peer,store}/`.

## R1: implemented control contract

`racer-control-wire` owns the bounded control protocol, not application policy.
Its independent consumer is the performance controller through
`internal/racer-test/control.rs`. That package no longer depends on the dataplane.
See `cmd/racer-dataplane/control-wire/README.md` for the contract, validation
commands, secret ownership, and explicit application adapters.

Runtime members remain local so their placement trait implementations do not
violate Rust's orphan rules (`src/topology.rs::Member`, relative to
`cmd/racer-dataplane/`). `src/control.rs::SnapshotStore::prepare` converts wire
members at validation; wire publications, enrollment records, rail mappings,
cache definitions, and codecs are consumed directly. Snapshot installation,
enrollment lifecycle, transport, admission, readiness, and authenticated routing
policy stay in the application. The existing `topology` workspace crate owns
frozen membership, integer placement, and cooperative path algorithms; it is a
Racer dataplane component, not a general distributed framework. Its stable-ID
32-ring overlay replaces radix routing, and its v5 next-hop schema separately
length-prefixes the seed and endpoint IDs. Placement hash compatibility is
unchanged, but routing requires a coordinated cluster version transition, not
assumed safe mixed-version operation. See the crate documentation in
`cmd/racer-dataplane/topology/src/lib.rs`
for cache ownership, memory estimates, and compatibility details.
Live updates call `SnapshotStore::prepare_async` in `src/control.rs`, which owns
one unfinished off-thread preparation job per store. Canceled waiters do not
release that slot early, and store drop joins the job without an independent
timeout. This keeps construction off I/O polling and bounds concurrent work,
but trades prompt shutdown for owned completion. Same-version validated content
reuses the whole membership; new versions with identical frozen IDs reuse Arc
adjacency through `Membership::new_with_predecessor`. The crate remains
synchronous and runtime-independent; the application owns this scheduling and
publication policy.
Shared identifiers are limited to the contract's needs; no general model crate
is introduced.

## Identity: cohesive component ownership

`racer-identity` now owns CSR/recovery, signing identity and peer certificate
validation, atomic key epochs, and immutable purpose-bound leases. It consumes
wire bundles directly, with private zeroizing validation staging. Application
secret DTO duplication is removed. The wire storage KeyId is shared; the
application configuration helper explicitly maps its constructor failure, and
identity validates opaque, zero, future, and resurrected epochs independently.

Page and credential AEAD use borrowed inputs and caller output. Request MAC
derivation stays inside the credential lease. No raw lease key getter or generic
key callback is exported. Held operations never reconsult current admission.
The application still owns canonical AAD/messages, nonce generation, quotas,
CRC/telemetry/cancellation ordering, transport, persistence, and accepted cursors.
Worker-local certificate caches retain Rc/RefCell ownership.

Component tests retain private lifetime assertions; the runtime/page-engine
ownership scenario and real decode/BundleInstaller rotation scenario are in
`tests/identity_integration.rs`. See the crate docs in
`cmd/racer-dataplane/identity/src/lib.rs` for focused gates.
This component boundary is justified by cohesive ownership, not an invented
second consumer. The independent performance controller remains wire-only.

## R2: circuit-core extraction deferred

There is no identified independent circuit-core consumer. The existing consumers
are Racer peer, origin, and control adapters:

- `cmd/racer-dataplane/src/peer.rs::Requester::exchange_inner_mode` acquires health
  probes and attributes verified results and selected socket failures.
- `cmd/racer-dataplane/src/origin.rs::OriginClient` owns endpoint health and classifies
  origin operations.
- `cmd/racer-dataplane/src/control.rs::ControlTransport` and `ControlConnection`
  own controller health.
- `cmd/racer-dataplane/rest/src/transport.rs::Transport` owns generic transport state,
  not endpoint-circuit policy.

The current local boundary already offers explicit-time observation and boolean
acquisition, bounded state, and an owned exclusive probe
(`cmd/racer-dataplane/src/topology.rs::LinkHealth`). Its
`tests::circuits_tests::owned_half_open_probe_remains_exclusive_until_completion_or_drop`
test asserts exclusivity even after timer expiry. The comment in `try_acquire_at`
about a "hung probe" releasing eligibility is misleading for an owned guard: the probe
set continues blocking acquisition until guard drop. This decision follows the
implementation and assertions, not that comment; no behavior is changed here.

The capacity bound is a memory bound: `run` ignores observation errors, including
capacity failure (`src/topology.rs::LinkHealth::run`, relative to
`cmd/racer-dataplane/`). Any future extraction must preserve or deliberately
specify that distinction. Racer error classification and endpoint-specific
jitter remain outside a future generic state machine.

Peer AIMD admission is separate, with active-work limits and generation-fenced
recovery (`cmd/racer-dataplane/src/peer.rs::AdaptivePeers` and `Permit`). It must not be
merged with endpoint circuits. The recovery description in
section 1 of `designs/racer-tail-latency-controls.md` applies to that adaptive owner,
not `LinkHealth`.

Reconsider only for a named independent component needing explicit-time,
bounded endpoint state and exclusive owned half-open probes. No crate, second
consumer, or speculative API is introduced by this mission.

## R3: store-format extraction deferred

No concrete offline record inspector or recovery-tool consumer was identified.
The existing slice parser is already separate from admitted buffer allocation:
`cmd/racer-dataplane/src/store.rs::parse_bytes`. It returns owned Racer metadata,
not an entirely borrowed schema. The online reader is its production consumer
(`cmd/racer-dataplane/src/store.rs::StoreReader::read`).

Checkpoint recovery restores index and segment metadata without scanning page
records (`cmd/racer-dataplane/src/store/checkpoint.rs::Recovery::install_shard`).
Its decoder shares a cursor, not the page-record schema (`decode_with_budget` in
the same file). The fallback test deliberately retains invalid slab content
while recovering checkpoint metadata
(`cmd/racer-dataplane/src/store/tests.rs::checkpoint::alternating_publication_falls_back_to_valid_older_cut_and_ignores_temp_and_payload`). This is not evidence
of an offline page-record recovery consumer.

Integrity has distinct stages: the format parser verifies header SHA-256 and
returns the stored CRC; the reader installs that CRC; `CiphertextPage` verifies
it before AEAD decryption (`cmd/racer-dataplane/src/memory.rs::CiphertextPage::verify_checksum`,
`cmd/racer-dataplane/src/security.rs::PageCryptoEngine::prepare`). The current
`src/store.rs` module documentation also states this mandatory CRC stage. The
payload-corruption assertions explicitly check the later checksum failure
(`cmd/racer-dataplane/src/store/tests.rs::stored_payload_and_tag_corruption_fail_mandatory_checksum`). Do not infer that successful
framing decode authenticates ciphertext.

Reconsider only when an existing named offline tool needs bounded borrowed
record fields, framing, schema, and checksum inspection without Store,
checkpoint/catalog, admission, or runtime ownership. No tool is invented to
justify extraction; no storage or recovery behavior changes in this mission.
