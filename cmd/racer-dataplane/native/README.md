# Optional native RDMA adapter

The Rust `rdma` feature dynamically loads `libracer_rdma.so.1`. Cargo needs no
new dependency, native link flag, or `build.rs` change. Default and all-features
builds work without verbs headers. Without the feature, adapter, usable device,
explicit rail mapping, or type-2B memory-window support, readiness is false and
the peer owner must use its HTTP transport.

## Native installation

On a native deployment builder install a C compiler, make, pkg-config, and
libibverbs development headers (Debian: `libibverbs-dev`). Run:

```sh
make -C cmd/racer-dataplane/native
```

Install the resulting `libracer_rdma.so.1` in the deployment's library search
path alongside libibverbs and its hardware provider libraries. For a local test,
set `LD_LIBRARY_PATH` to this directory. Never accept a library path from peers.
The adapter's ABI version is checked before use. Rust only sees private fixed-width
records and opaque handles; the C compiler supplies all actual verbs layouts,
unions, provider dispatch, and enum constants. The adapter uses GID index zero
and requires 4 KiB base pages; other host page sizes select HTTP. Registered quota
includes the rounded-up pinned final page.

## Integration contract

- Keep constructors side-effect-free. During startup call `Verbs::discover`,
  validate administrator Node rail mappings, and call `Devices::configure` with
  exact `RailPort { rail, device, port, gid }` matches. Retain this `Rc<Devices>`
  for the session and registered pools. Call `RdmaTransfer::ready(rail)` when
  choosing transport. Discovery does not alter membership or invent rail IDs.
- The topology owner must validate the entire authenticated route and arrange
  worker affinity/local memory policy before enabling aligned rails. This adapter
  does not implement NUMA placement or choose GID indexes.
- Call `Sessions::prepare(&VerifiedPeer, rail)` on both peers. Put
  `prepared.setup().header_value()` in `racer-rdma-setup`. Authenticate offers
  with the existing `Signatures`. A signed acknowledgment must include both the
  sender's setup and `racer-rdma-setup-binding` containing the remote offer's
  `binding_header_value()`. `PreparedSession::finish(&VerifiedHead)` checks the
  certified peer, exact offer acknowledgment, rail, endpoint bounds, and signed
  components before RTR/RTS. Both sides must acknowledge; this is a two-sided
  control exchange, not the old one-message `establish` API.
- These header values are canonical padded base64. The current security profile
  signs all headers. Keep `racer-rdma-setup`, `racer-rdma-setup-binding`,
  `racer-rdma-descriptor`, and `racer-rdma-completion` in the signed component
  list. Request/page/membership/deadline binding remains the peer protocol's job.
- Receiver: `prepare_receive(session, envelope, transfer, scope)`, then
  `grant.wait_bound(scope)`. Only after the successful bind CQE may
  `grant.header_value()` be sent as `racer-rdma-descriptor`.
- Sender: after verifying the control message and its page/request binding, use
  `AuthenticatedDescriptor::from_verified(head, session, transfer)`, then
  `send_to(session, ciphertext_page, descriptor, scope)`. Only its successful
  `SendCompletion::header_value()` may be signed as `racer-rdma-completion`.
- Receiver: verify completion response correlation, then use `finish_receive`.
  It waits for local invalidation and destroys the single-use QP before reading
  registered memory. It returns ciphertext, never verified plaintext. The normal
  AEAD pipeline must authenticate it before publication/client delivery.
- Every transfer uses a fresh QP and window. No reusable MR rkey is exported.
  Session count is capped per neighbor and across 36 neighbors; each QP has a
  bounded CQ and pending table. Registered allocations are charged through
  `Admission::reserve(ResourceClass::Registered)` and capped at 16 MiB + tag.
  The receive copy separately reserves ciphertext quota.
- Drive `Sessions::progress()` (or `RdmaTransfer::progress()`) on every I/O-worker
  reactor turn with active sessions, including when request futures were dropped.
  Each QP polls at most 32 CQEs without blocking and wakes its waiters. Arrange a
  bounded timer tick for deadlines. There is no internal executor or spin loop.
- Drop/cancel invokes a terminal QP fence. Failed fences retain region, window,
  PD, library and quota; a final failed teardown deliberately leaks these native
  resources rather than allowing late DMA into freed memory. A successful write
  CQE releases source DMA ownership; a bind CQE never releases receive ownership.
  `drain()` fences live sessions. Treat errors as unavailable and initiate a new
  HTTP attempt with the original request deadline and a fresh transfer identity.
- Compatibility `send`/`receive` fail unavailable because their old signatures
  contain no authenticated grant/completion. `establish` fails unauthorized
  because it cannot acknowledge the local QP. Integration must use the handoffs
  above to activate native transfers. HTTP remains the ordinary fallback.

## Verification and limits

```sh
cargo test --lib rdma:: --all-features
LD_LIBRARY_PATH="$PWD/native" cargo test --lib --all-features native_no_device -- --ignored
RACER_RDMA_TEST_DEVICE=mlx5_0 LD_LIBRARY_PATH="$PWD/native" \
  cargo test --lib --all-features native_available_provider -- --ignored
```

The no-device test requires the real adapter and asserts zero usable ports; it
does not turn missing libraries into a passing test. The explicitly ignored
provider test requires an operator-selected active type-2B device and performs
real RC loopback bind/write/invalidate/fence/readback. Do not run both ignored
tests indiscriminately: their environmental requirements are mutually exclusive.
Fault injection is compiled only for tests and exercises production ownership
logic, including failed fences, late writes, failed CQ polling, unknown CQEs,
submission rejection, quota retention, expiry, and teardown order.

`ownership_tests.rs` can compile the actual adapter ownership tests independently
with rustc and Cargo's built libc/getrandom rlibs during concurrent crate editing.
It does not substitute a fake production adapter.
`component_tests.py` emits a test crate containing the production modules except
application and telemetry composition, so the signed setup and quota tests can
also run while those composition files are being edited.

Hardware success, multi-host routing, provider-specific remote invalidation
behavior, NUMA placement, and receiver failure under active remote DMA require
provider testing. The component intentionally uses terminal QP destruction even
after invalidation instead of assuming an invalidation CQE fences admitted remote
writes. This implementation does not provide reusable neighbor QP pooling; its
single-use QPs favor an explicit terminal fence at the cost of setup overhead.

Implementation-host verification: the native adapter built with `-Werror`, the
standalone ownership suite passed (11 tests), and the real native no-device test
passed. The production component graph passed all 16 RDMA tests, including real
Ed25519 setup verification, tampering and replay rejection, with two explicitly
gated native tests; the no-device test was then run explicitly and passed.
Both `cargo check --lib --all-features` and
`cargo check --lib --no-default-features` passed. The provider test was not run:
this host had no usable type-2B port. Full crate test runs during implementation
were blocked by concurrent application/telemetry compilation errors outside RDMA.
