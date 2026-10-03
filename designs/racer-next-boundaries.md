# Racer next library boundaries

## R1: implemented control contract

`racer-control-wire` owns the bounded control protocol, not application policy.
Its independent consumer is the performance controller through
`internal/racer-test/control.rs`. That package no longer depends on the dataplane.
See `cmd/racer-dataplane/control-wire/README.md` for the contract, validation
commands, secret ownership, and explicit application adapters.

Runtime members remain local so their placement trait implementations do not
violate Rust's orphan rules. Snapshot installation, enrollment lifecycle,
transport, admission, readiness, and topology algorithms stay in the application.
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
`tests/identity_integration.rs`. See the component README for focused gates.
This component boundary is justified by cohesive ownership, not an invented
second consumer. The independent performance controller remains wire-only.

## R2: circuit-core extraction deferred

There is no identified independent circuit-core consumer. The existing consumers
are Racer peer, origin, and control adapters:

- `cmd/racer-dataplane/src/peer.rs:653-688` acquires health probes and attributes
  verified results and selected socket failures.
- `cmd/racer-dataplane/src/origin.rs:334-394` owns endpoint health and classifies
  origin operations.
- `cmd/racer-dataplane/src/control/transport.rs:201-270` owns controller health.
- `cmd/racer-dataplane/rest/src/transport.rs:14-33` owns generic transport state,
  not endpoint-circuit policy.

The current local boundary already offers explicit-time observation and boolean
acquisition, bounded state, and an owned exclusive probe
(`cmd/racer-dataplane/src/topology.rs:46-143`). Its owned-probe test asserts
exclusivity even after timer expiry (`:175-202`). The comment at `:142` about a
"hung probe" releasing eligibility is misleading for an owned guard: the probe
set continues blocking acquisition until guard drop. This decision follows the
implementation and assertions, not that comment; no behavior is changed here.

The capacity bound is a memory bound: `run` ignores observation errors, including
capacity failure (`:62-80`). Any future extraction must preserve or deliberately
specify that distinction. Racer error classification and endpoint-specific
jitter remain outside a future generic state machine.

Peer AIMD admission is separate, with active-work limits and generation-fenced
recovery (`cmd/racer-dataplane/src/peer/adaptive.rs:41-75,112-234`). It must not be
merged with endpoint circuits. The recovery description in
`designs/racer-tail-latency-controls.md:28-45` applies to that adaptive owner,
not `LinkHealth`.

Reconsider only for a named independent component needing explicit-time,
bounded endpoint state and exclusive owned half-open probes. No crate, second
consumer, or speculative API is introduced by this mission.

## R3: store-format extraction deferred

No concrete offline record inspector or recovery-tool consumer was identified.
The existing slice parser is already separate from admitted buffer allocation:
`cmd/racer-dataplane/src/store/format.rs:160-253`. It returns owned Racer metadata,
not an entirely borrowed schema. The online reader is its production consumer
(`cmd/racer-dataplane/src/store.rs:194-245`).

Checkpoint recovery restores index and segment metadata without scanning page
records (`cmd/racer-dataplane/src/store/checkpoint.rs:382-418`). Its decoder shares
a cursor, not the page-record schema (`:834-864`). The fallback test deliberately
retains invalid slab content while recovering checkpoint metadata
(`cmd/racer-dataplane/src/store/tests/checkpoint.rs::alternating_publication_falls_back_to_valid_older_cut_and_ignores_temp_and_payload`). This is not evidence
of an offline page-record recovery consumer.

Integrity has distinct stages: the format parser verifies header SHA-256 and
returns the stored CRC; the reader installs that CRC; `CiphertextPage` verifies
it before AEAD decryption (`cmd/racer-dataplane/src/memory.rs::CiphertextPage::verify_checksum`,
`cmd/racer-dataplane/src/security/aead.rs:312-339`). The format module's opening
comment mentions header SHA and AEAD but omits this mandatory CRC stage. The
payload-corruption assertions explicitly check the later checksum failure
(`cmd/racer-dataplane/src/store/tests.rs:1231-1257`). Do not infer that successful
framing decode authenticates ciphertext.

Reconsider only when an existing named offline tool needs bounded borrowed
record fields, framing, schema, and checksum inspection without Store,
checkpoint/catalog, admission, or runtime ownership. No tool is invented to
justify extraction; no storage or recovery behavior changes in this mission.
