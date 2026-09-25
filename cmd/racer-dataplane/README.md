# Racer dataplane

One Rust package containing the node dataplane. `app.rs` composes worker-local
services; the binary enters through configuration and the application lifecycle.
The Go control plane is outside this package.

[Control API](CONTROL_API.md) defines the controller/dataplane contract, Kubernetes
membership inputs, enrollment, and projected credential rotation.

```sh
cargo fmt --manifest-path cmd/racer-dataplane/Cargo.toml --check
cargo check --manifest-path cmd/racer-dataplane/Cargo.toml --all-targets --all-features
cargo test --manifest-path cmd/racer-dataplane/Cargo.toml --all-features
```

Constructors connect dependencies without opening files, accepting requests, or
spawning threads. Activation is explicit through the application lifecycle. See
[configuration](CONFIGURATION.md) for environment variables, resource limits, and
startup requirements. Linux io_uring and direct-I/O-capable storage are required.
See [deployment](DEPLOYMENT.md) for release artifacts, mounts, permissions, and
trusted local native-port configuration. The [Go controller](../racer-controller/README.md)
implements enrollment and publication serving; follow its installation procedure
to initialize durable state and provision serving TLS and bootstrap trust.

The optional `rdma` feature loads the separately built native libibverbs adapter.
See [native adapter](native/README.md) for installation and provider tests. Missing
devices or incompatible capabilities select HTTP. Native DMA and teardown behavior
must be verified on the deployment's provider; no usable type-2B device was available
in the implementation test environment.

## Implementation contracts

- `model` defines identity and immutable values; request context is a separate type.
- `runtime` owns explicit pinned workers, bounded handoffs, admission, and completion
  lifetimes. Worker-local futures and services do not require `Send`. Actual
  cross-worker commands must transfer resource ownership explicitly.
  Each logical worker has two separate threads: I/O and page crypto. The default
  cap is eight total userspace threads (up to four pairs), sized against allowed
  CPUs and cgroup quotas. Prefer separate NIC-local cores; on one allowed CPU both
  threads share it. Control and diagnostics fit within that budget. This explicitly
  supersedes `tmp/design.md`'s combined-role single-core description.
  `WorkerPair` represents both roles and local CPU/core/NIC/NUMA constraints;
  pure pair sizing floors odd caps/cores and effective quotas to complete pairs.
  Local discovery/planning validates accepted rail mappings without changing the
  cluster rail contract or deterministic page-to-rail selection.
  The `Sync` factory builds separate I/O and crypto services on their pinned
  threads. Group APIs specify start, concurrent drain, shutdown, and join,
   including partial-start rollback.
  `runtime::crypto` defines owned Send jobs/completions, generation/sequence IDs,
  key leases, original deadlines/cancellation, and a non-cloneable pair-bound
  permit reserving both job and completion space before enqueue. Rejection returns
  ownership. Completion consumption releases capacity, including for canceled or
  abandoned waiters. Wakeable polling and bounded quanta must permit single-CPU
   progress. Completion capacity is reserved before a crypto job is accepted.
  I/O-local `PageCrypto` selects a key lease and submits owned inputs via its
  runtime's `CryptoClient`; the paired `PageCryptoEngine` cannot access the Rc
  service graph. Output reservations move to jobs, rather than borrowing a fill's
  admission state. I/O alone owns flights, storage, metadata, and admission.
- `read` coordinates one per-page flight per worker. `client` and `peer` use that
  same coordinator; transport does not create another acquisition pipeline.
  Explicit version pins may use expired metadata; fresh/unpinned admission requires
  revalidation. Existing reads never switch ETags.
  `VersionMetadata` retains immutable version/total-length facts separately from
  the volatile `CurrentVersion` freshness pointer. Only page-zero revalidation
  publishes that pointer; pins and page hits publish immutable facts only.
  `MetadataService` shares the worker's live `Index`, `Fill`, and bounded worker
  directory. Bootstrap returns either metadata-only (empty) or a full `PageResult`.
  Missing pinned metadata can query retained descriptors across local page shards
  before a conditional page-zero probe. Fresh reads still require revalidation.
  `read::fill::PageResult` re-exports the cloneable memory-layer result containing
  metadata, verified plaintext, and original ciphertext for flight sharing.
  `Flights::join`/`AcquisitionWaiter::wait` elect one leader or return the complete
  result/failure. `join_copy` exposes only miss/wait/complete, with no ability to
  start acquisition or fund a retry. Publication validates the exact page and the
  bundle's metadata/plaintext/ciphertext association; page hits cannot refresh TTL.
  Acquisition waiters borrow non-cloneable request context and an exclusive
  remaining `AcquisitionBudget`. Neither headers nor credentials enter flight
  identity or completed entries. Different contexts share the same page flight.
  Membership and candidate authority belong to each elected acquisition; retry
  must resolve authority again under the selected caller's accepted membership.
  Credential-specific origin rejection fails only the supplying caller, then
  elects a remaining live acquisition caller under its original deadline,
  cancellation, attempt credits, and remaining link budget. It is never cached as
  page/version absence. Other terminal failures wake the current cohort.
  Cancellation/detach affects one waiter. Abandoned leadership enters `Draining`;
  accepted operations retain their owned I/O/crypto resources independently of
  futures. Only actual completions permit `RetryPending`, failure for copy-only
  survivors, or removal after the last waiter. Table identity, entry incarnation,
  and acquisition generation fence stale publication/failure/drain callbacks.
  The worker lifecycle retains the same flight table as Fill and exposes bounded
  polling/drain hooks for cleanup after request futures disappear.
   Registration, election, wakeups, Drop cleanup, and completion-driven drain are
   exercised by behavioral tests in `read`, including resource-pressure cases.
- Per-cache UDS paths are exactly `/run/racer/<cache name>/client/socket` and
  `/run/racer/<cache name>/origin/socket`. Racer owns the client listener; the
  application adapter owns the origin listener. Separate endpoint directories let
  pods mount only the UDS directory authorized by a future admission controller.
  Cache names must be safe single path components, and complete paths must fit the
  platform UDS limit. Cache reconciliation must reject noncanonical supplied paths.
- `security` uses separate page and ephemeral-credential encryption domains.
  Pass the exact cache key, `Racer-Metadata`, and optional Authorization to origin.
  Metadata stays signed plaintext across peers; Authorization is encrypted across
  peers and decrypted for the local origin socket. Credentials are opaque upstream
  fetch context, not Racer authorization, and never enter caches, disk, or logs.
  `CredentialCrypto::seal` synchronously borrows the request's `OriginContext` and
  original `RequestScope`, returning an independent owned `PeerOriginContext` per
  attempt. Retry/fanout reseals without cloning raw secrets. Each encryption must
  generate a fresh cryptographic nonce and bind the credential domain, key ID,
  request/attempt, object, and exact opaque metadata in canonical AAD. The facade
  shares worker admission; each envelope owns its request-context reservation and
   original cancellation/deadline through transport completion. Allocation is
   admitted before sealing, and secret-bearing buffers are zeroized on release.
  `SignedRequest` and `SignedResponse` own the logical message plus its original
  signed head and ordered forwarding heads. `Forwarding` signs/verifies complete
  envelopes and appends request/response hops without replacing the original.
  Its opaque `VerifiedRequest` is required by local service and relay; its
  `VerifiedResponse` is returned to logical requesters. Both retain the full chain
  and expose signed fields read-only. Local `PeerResponse` results are unsigned.
  `RequestBinding`, minted by request signing/verification, retains the exact
  original request head/signature and is required for response signing/verification.
  `PeerTransport` exchanges owned signed envelopes, including through relays.
   Canonical field agreement, original/hop signatures, replay/identity checks,
   response correlation, and route consumption are checked before verified types
   can be constructed. The versioned profile is documented in
   `designs/racer-peer-security.md` at the repository root.
- `store` accepts only encrypted pages. Slabs require `O_DIRECT` with discovered
  address/offset/length alignment. Aligned padded record lengths differ from
  authenticated ciphertext lengths. Padding is initialized and never delivered.
  Memory entries and dirty `CiphertextCopy` bundles retain metadata with each page;
  completed index entries and record headers retain `VersionMetadata`. Total object
  length is never inferred from a page length. Per-page descriptors survive bounded
  page-zero catalog eviction, so old cached pages remain usable by explicit pins.
  HEAD-only and empty-object descriptors have a bounded standalone catalog and
  checkpoint representation without allocating an encrypted page. Checkpoints
  include descriptors atomically with completed page mappings, omit dirty pages and
  current-version pointers, and reject conflicting lengths for one version.
  Recovery is connected to the same live index/segments and restores one consistent
  cut before admission; recovered descriptors carry no freshness claim. Request
  context, Authorization, and opaque adapter metadata are absent from these types.
- `memory`, `runtime`, and `rdma` retain leases until all applicable kernel/NIC
  fences finish, including cancellation. Disk persistence is bounded and async;
  failed dirty writes may be discarded.
  `IoBuffer` is sealed and requires `'static`, exclusively owned, address-stable
  backing plus its quota reservation: fixed boxed staging bytes or an aligned
  allocation, never borrowed slices or inline arrays. Before submitting I/O, the
  reactor must put buffers, owned FD references, and connection/segment leases in
  its in-flight table, independently of the waiting future. Future drop abandons
  the result; release/reuse requires original and cancellation completion fences,
  including during shutdown. `Completion<B, L>` returns the buffer and lease after
  fencing. HTTP heads transfer/return connection ownership and require owned byte
  staging; bodies consume owned buffers and return them with the connection lease.
  Shared/borrowed body slices require explicit bounded staging. Slab operations
   consume a segment lease alongside the aligned buffer. Cancellation requests do
   not release those resources before the corresponding completion fences.
- `control` publishes immutable accepted snapshots and coherent key bundles.
  Bootstrap returns the local node certificate directly; common Secret bundles
  contain only peer trust and cache keys. Token-authenticated bootstrap also renews
  certificates; subsequent snapshot polls use mTLS, not control HTTP signatures.
  One selected worker owns control enrollment; worker handles share node-wide
  snapshot, key-epoch, and replay roots. Bounded dispatch routes work to page owners.
- Live key retirement and cache removal use a node-wide quiescent cut. Admission
  pauses while accepted read, write, crypto, kernel, and native operations finish;
  recoverable checkpoints are invalidated before omitted keys are destroyed.
  The control worker alone restores client, peer, and diagnostic listeners.
  This conservative path temporarily interrupts unrelated caches and diagnostics.
- `test_support` is test-only. Inline test sections identify the owning contracts;
  implement behavioral tests with each feature, rather than tests of placeholders.

## Protocol and verification references

- `CLIENT_ORIGIN_API.md`: approved SDK and origin-adapter wire contract.
- `src/topology/ALGORITHM_V1.md`: deterministic placement and routing profile.
- `designs/racer-peer-security.md`: authenticated peer and encryption profile.
- `designs/racer-store-protocol.md`: record/checkpoint formats and recovery rules.
- `designs/racer-sdk-conformance.md`: independent wire and Go SDK checks.

The `designs/` paths above are relative to the repository root. Component
`INTEGRATION.md` files describe ownership and lifecycle APIs. Passing component
tests does not establish deployment interoperability or hardware DMA guarantees;
run the combined suite and applicable native-provider tests for a release.
`designs/racer-production-validation.md` records real multi-page streaming,
zero-TTL, cancellation, disk-hit, and memory-pressure validation.
