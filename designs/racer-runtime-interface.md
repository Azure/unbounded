# Rust runtime wiring contract

> Version note: Racer 1.0 replaces `/v4/config` below with `/v1/config` and
> uses `/v1/enroll`, `/v1/proof`, and `/v1/replica-proof`. See
> `cmd/racer-dataplane/CONTRACT.md` for the current wire and state formats.

The Rust runtime exports `kubernetes`, `subscription`, and `status`. Main/service
own TLS and leadership acquisition; the runtime owns inventory watches,
process-local topology/storage publication, and the `/v4/config` Router.
The [stateless control-plane contract](racer-rust-controlplane.md) supersedes
the former RecordStore pointers, immutable chunks, store gate, and collection.

## Interface and authorization

```rust
use racer_controlplane::kubernetes::{Runtime, RuntimeOptions, SecurityContext};
// None on election loss, cancellation, or incomplete security initialization.
// The immutable CaState snapshot contains bounded root/expiry state, not members.
let security: Arc<dyn Fn() -> Option<SecurityContext> + Send + Sync> = ...;
// SecurityContext { fence: String, state: Arc<security::CaState> }
let runtime = Runtime::new(RuntimeOptions::new(namespace), security);
let router = runtime.router(); // TLS is owned by service.
// Every accepted TLS request carries Extension<security::VerifiedPeer>.
// spawn runtime.clone().run(client, shutdown_token)
```

`runtime.selection(universe_hex, node_hex)` returns the currently selected
process binding (`Selection { node_name, pod_namespace, pod_name, pod_uid,
universe }`) or None. Selection is indexed, with no request-time Node list.
`CaState::verify_peer` checks certificate validity, signed namespace/identity,
and retained issuer. `/v4/config` additionally binds the process boot header,
node role, Pod identity, and current selection, including after long-poll wakeup
(`cmd/racer-controlplane/src/subscription.rs:257`). There is no durable member
or tombstone lookup. Enrollment and renewal separately perform direct live
Kubernetes authorization (`cmd/racer-controlplane/src/service.rs:730`).

The security getter becomes None immediately on election loss. A changed fence
requires a fresh revision reservation and full inventory rebuild before serving.
The getter and authoritative Lease/CA/checkpoint checks gate publication; stale
leadership cannot claim new revisions using a freshly fetched resourceVersion.

Live selection is computed directly from indexed Node/Pod eligibility and
deterministic Pod selection, independently of fabric, cache, socket, or placement
configuration validation. Rejected intent therefore cannot revoke unrelated
healthy members of the retained last-good publication. Conversely, retaining
that publication cannot authorize a binding that disappeared from live inventory
(`cmd/racer-controlplane/src/kubernetes.rs:850`,
`cmd/racer-controlplane/src/subscription.rs:101`).

An independent monotonic authority deadline gates `runtime.ready()`, enrollment
selection, and subscription authorization even if reconciliation or Kubernetes
I/O stalls. The production runtime starts expired; a successfully verified round
refreshes the deadline using its start time, not delayed I/O completion. The
default authority timeout is 15 seconds and must exceed the retry interval
(default five seconds). Held subscriptions recheck authorization every 250 ms;
expiry does not depend on the stalled runtime loop clearing its fence. This
deadline suspends serving, never authorizes checkpoint reset or takeover
(`cmd/racer-controlplane/src/kubernetes.rs:62`, `:101`, `:220`, `:275`;
`cmd/racer-controlplane/src/subscription.rs:595`).

## Fixed revision checkpoint

`RevisionStore` owns `ConfigMap/racer-runtime-revisions`. Its three data fields
are `format=1`, `high-water`, and `fence`; it contains no runtime payload or
per-node entries. A leader reserves ranges of 1,048,576 revision numbers using
resourceVersion CAS and authoritative readback. Unused numbers may be skipped
after restart. Missing/corrupt state, exhaustion, or observed replacement/regression
fails closed (`cmd/racer-controlplane/src/revision.rs:18`, `:160`).

Reservation captures the checkpoint resourceVersion before checking Lease/CA
authority and uses a unique reservation token. Before issuing the write it
invalidates the locally recorded prior reservation. Even a successful write
requires exact authoritative readback and another authority check before the
range can be returned. Cancellation or an uncertain outcome can burn revision
numbers, but cannot authorize their use. A retry reserves a fresh disjoint range
from the observed high-water mark; a delayed predecessor CAS conflicts once a
successor changes the resourceVersion. There is no durable operation lock to
release, no gate cleanup task, and no 30-second store-gate recovery protocol
(`cmd/racer-controlplane/src/revision.rs:178`).

Fresh CA bootstrap alone creates the initial checkpoint. Absence during restart
does not prove freshness (`cmd/racer-controlplane/src/security/kubernetes.rs:247`).
Acquisition discards predecessor payloads and reconstructs from complete live
inventory (`cmd/racer-controlplane/src/kubernetes.rs:184`). Operational status
annotations are output, not restart input (`cmd/racer-controlplane/src/status.rs:4`).

Topology and storage policy are independent in-memory publications using reserved
revisions. Invalid intent retains last-good state only within the current
process. Without prior valid storage intent, no valid new offer is published;
compatible persisted dataplane capacity is retained. Feedback is bound to the
exact offered Pod/boot/identity/version and is fresh for 75 seconds. It grants
neither publication permission nor CA rotation credit.

The only other durable runtime objects are the public trust ConfigMap, bounded
version-5 CA Secret, and leader Lease. CP CSR/responses overwrite bounded
annotations on existing Pods. There are no participant or replica ConfigMaps,
runtime history, chunk collector, or automatic obsolete-object cleanup. Use the
[explicit cutover workflow](racer-stateless-cutover.md).

## Independent subscriptions and bounded work

The handler emits DesiredState. Received cursor and locally applied state are
separate; no remote rollout phases exist. The default hold is 28 seconds.
`run(self, Client, CancellationToken) -> anyhow::Result<()>` drives the runtime;
`router()` returns `axum::Router`. `subscriptions.cache_usage()` and
`waiter_count()` expose process-local memory/receiver observations.

Per-universe placement has 262,144 slots shared by all caches. HRW balances
statistically and permits adjacent repeated owners; slot candidate retries are
unchanged. Snapshot construction uses sparse edge indexes and a dense fallback
for high local ownership. Builder and response admission bound concurrent work;
idle subscribers hold no durable state. See `cmd/racer-controlplane/src/topology.rs:34`
and `cmd/racer-controlplane/src/subscription.rs:62`.

Digest metadata survives snapshot payload eviction, so unchanged reconnects can
avoid rebuilding per-recipient content. Publication changes invalidate affected
digests; live selection changes also invalidate affected convergence metadata
and wake subscribers. A selected snapshot cache hit restores its digest metadata
only while its publication is still current, under the same lock order as
installation. This permits convergence after live reselection without resurrecting
a superseded generation's digest. Metadata remains capped at 100,000 recipients
(`cmd/racer-controlplane/src/subscription.rs:118`, `:337`, `:519`).

Runtime tests exercise the Kubernetes API seam, revision uncertainty, restart,
loss/replacement, and independent subscriptions. Real API and production-binary
campaigns remain separate gates. The historical scale/GC numbers in
`racer-go-retirement-audit.md` describe the retired implementation and must not
be cited as validation of this checkpoint or placement implementation.
