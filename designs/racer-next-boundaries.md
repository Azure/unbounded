# Racer next library boundaries

Current source references include the generic controlplane and durable enrollment
extractions; deferred library extractions below remain separate decisions. These navigation updates do
not claim that historical validation or performance measurements were rerun.

## Current application source ownership

Paths here are relative to `cmd/racer-dataplane/`; reusable workspace crates keep
their own directories and APIs.

- Flat `src/admission.rs`, `src/runtime.rs`, and `src/worker.rs` own Racer admission,
  request scopes/reactor adaptation, and worker execution respectively. Worker
  services implement `uring_runtime::group::Service<RequestScope>` directly;
  local page-to-worker selection is `src/read/dispatch.rs::WorkerMap`.
- `src/control.rs` and `src/control/publication.rs`, plus flat `src/security.rs`, `src/memory.rs`, `src/http.rs`,
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
`cmd/racer-dataplane/`). `src/control/publication.rs` converts wire members at
validation; wire publications, enrollment records, rail mappings, cache
definitions, and codecs are consumed directly. Domain projection, resource
installation, admission, readiness, and authenticated routing policy stay in the
application. Generic synchronization and publication belong to `controlplane`,
durable enrollment to `racer_crypto::enrollment`, and reusable REST/TLS transport
to `wire_codec::rest`. The existing `topology` workspace crate owns
frozen membership, integer placement, and cooperative path algorithms; it is a
Racer dataplane component, not a general distributed framework. Its stable-ID
32-ring overlay replaces radix routing, and its v5 next-hop schema separately
length-prefixes the seed and endpoint IDs. Placement hash compatibility is
unchanged, but routing requires a coordinated cluster version transition, not
assumed safe mixed-version operation. See the crate documentation in
`cmd/racer-dataplane/topology/src/lib.rs`
for cache ownership, memory estimates, and compatibility details.
`controlplane::Sync` owns at most one unfinished off-thread preparation job per
feed, separately tracking received and accepted documents. Canceled turns do not
release that job early. Shutdown waits under a bounded scope; dropping the owner
joins any remaining job without an independent timeout. Preparation closures must
therefore terminate. `PublicationTarget` supplies Racer topology projection and
installation to the generic `Target` interface. Same-version validated content
reuses the whole membership; new versions with identical frozen IDs reuse Arc
adjacency through `Membership::new_with_predecessor`. Topology remains synchronous
and runtime-independent.

`controlplane::Published` atomically replaces coherent immutable generations and
accounts for retained parts; raw Arc/Weak leases make retention an observed soft
ceiling, not revocation of external leases. `controlplane::Rollout` gates commits
on generation-tagged acknowledgments from a fixed worker set. Application adapters
retain domain validation, resource staging, rollback, and infallible commit hooks.
The generic crate contains no Racer wire, crypto, or topology dependency. Its
owner-local I/O and credentials do not cross threads with preparation closures.
See `cmd/racer-dataplane/controlplane/src/lib.rs` and the existing control-wire
README for APIs and `make controlplane-check` for isolated crate checks.
Shared identifiers are limited to the contract's needs; no general model crate
is introduced.

## Identity: cohesive component ownership

The `racer_crypto::identity` module owns signing identity and peer
certificate validation, atomic key epochs, and immutable purpose-bound leases. It consumes
wire bundles directly, with private zeroizing validation staging. Application
secret DTO duplication is removed. The wire storage KeyId is shared; the
application configuration helper explicitly maps its constructor failure, and
identity validates opaque, zero, future, and resurrected epochs independently.

Page and credential AEAD use borrowed inputs and caller output. Request MAC
derivation stays inside the credential lease. No raw lease key getter or generic
key callback is exported. Held operations never reconsult current admission.
The application still owns canonical AAD/messages, nonce generation, quotas,
CRC/telemetry/cancellation ordering, and application transport adapters.
Worker-local certificate caches retain Rc/RefCell ownership.

`racer_crypto::enrollment` owns retry-stable CSR/private-key persistence, response
validation, identity recovery, token reads, and renewal timing. The caller owns
NIC inventory and durable NIC reservations, supplies current shares, and owns the
identity namespace exclusively. The crypto component uses runtime
`reactor::filesystem::secure` helpers for descriptor-relative private access,
bounded zeroizing reads, durable replacement/removal, and abandoned-attempt
fencing. Publication errors retain their before-rename, uncertain-rename, or
published phase. Projected token reads permit normal symlinks; private identity
files do not. See `crypto/src/enrollment.rs` and `runtime/src/reactor/filesystem.rs`
under `cmd/racer-dataplane/` for these contracts.

Component tests retain private lifetime assertions; the runtime/page-engine
ownership scenario and real decode/BundleInstaller rotation scenario are in
`tests/identity_integration.rs`. See the module docs in
`cmd/racer-dataplane/crypto/src/identity.rs` for focused gates.
This component boundary is justified by cohesive ownership, not an invented
second consumer. The independent performance controller remains wire-only.

## R2: circuit-core extraction deferred

There is no identified independent circuit-core consumer. Endpoint health and
control-feed progress have different owners:

- `cmd/racer-dataplane/src/peer.rs::Requester::exchange_inner_mode` acquires health
  probes and attributes verified results and selected socket failures.
- `cmd/racer-dataplane/src/origin.rs::OriginClient` owns endpoint health and classifies
  origin operations.
- `cmd/racer-dataplane/src/control/session.rs::Session` owns controller authentication,
  renewal, and node-binding policy. `controlplane::Sync` owns per-feed retry and
  acceptance diagnostics; neither is a `LinkHealth` circuit owner.
- `cmd/racer-dataplane/wire-codec/src/rest.rs::Transport` owns generic transport state,
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
