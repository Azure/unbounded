# Racer dataplane scaffold

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

This is an API scaffold, not a functioning cache. Constructors connect dependencies
without opening files, accepting requests, or spawning threads. Operational methods
return `Error::Unimplemented`; the executable exits unsuccessfully at configuration
loading. No mock security, storage, network, or data-path success is provided.
The `rdma` feature reserves native adapter integration and currently links no verbs
library. There are no third-party dependencies yet.

## Implementation contracts

- `model` defines identity and immutable values; request context is a separate type.
- `runtime` owns explicit pinned workers, bounded handoffs, admission, and completion
  lifetimes. Worker-local futures and services do not require `Send`. Actual
  cross-worker commands must transfer resource ownership explicitly.
  Each logical worker has two separate threads: I/O and page crypto. The default
  cap is eight total userspace threads (up to four pairs), sized against allowed
  CPUs and cgroup quotas. Prefer separate NIC-local cores; on one allowed CPU both
  threads share it. Control and diagnostics fit within that budget. Pair-aware
  lifecycle and owned crypto job/completion interfaces still need to be specified.
- `read` coordinates one per-page flight per worker. `client` and `peer` use that
  same coordinator; transport does not create another acquisition pipeline.
  Explicit version pins may use expired metadata; fresh/unpinned admission requires
  revalidation. Existing reads never switch ETags.
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
  response correlation, route consumption, and reverse-path I/O remain fail-closed
  implementation work. These API states do not implement authentication.
- `store` accepts only encrypted pages. Slabs require `O_DIRECT` with discovered
  address/offset/length alignment. Aligned padded record lengths differ from
  authenticated ciphertext lengths. Padding is initialized and never delivered.
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
  consume a segment lease alongside the aligned buffer. Submission, staging,
  cancellation accounting, and shutdown fencing remain fail-closed stubs.
- `control` publishes immutable accepted snapshots and coherent key bundles.
  One selected worker owns control enrollment; worker handles share node-wide
  snapshot, key-epoch, and replay roots. Bounded dispatch routes work to page owners.
- `test_support` is test-only. Inline test sections identify the owning contracts;
  implement behavioral tests with each feature, rather than tests of placeholders.

Wire canonicalization, key/path encoding, exact status policy, placement hash/score
arithmetic, replay restart protocol, checkpoint encoding/publication order, concrete
resource budgets, and RDMA mechanics remain explicit implementation specifications.
Module documentation describes intended completed behavior, not existing behavior.
Choose vetted libraries for crypto, HTTP, io_uring, and verbs when implementing
those boundaries. Replace fail-closed stubs with tested vertical slices.
