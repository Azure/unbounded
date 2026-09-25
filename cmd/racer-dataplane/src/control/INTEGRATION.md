# Control owner integration

All constructors are inert. The sole owner attaches
`Rc::new(ReactorControlIo::new(reactor))` through `attach_io`.
The worker must poll both the control future and `Reactor::poll_budgeted`.
No production executor, thread, blocking DNS lookup, or synchronous socket wait is
created by control. Small bounded local file reads and durable fsync/rename remain
synchronous owner operations.

## Identity and lifecycle

1. `ControlClient::start(scope)` reads the coherent projected trust bundle, loads
   a verified local identity or performs server-authenticated token enrollment,
   and returns `LocalSigningIdentity`. It does not use an environment Node UID.
   Retry transient failures at `next_attempt()`. The persisted pending CSR/key and
   enrollment ID survive restarts; the projected token is read per submission.
2. Construct node-dependent worker state from the returned `identity.node()`.
   `bind_keyring(keys)` installs the bundle and signing identity into a node-bound
   view of the shared epochs. If the original keyring already has the authenticated
   node, `activate_identity()` performs the installation instead.
3. Drive `progress(scope)` serially. It returns immutable snapshot leases and
   cache events, polls with mTLS, renews with token authentication, reloads bundles,
   and applies bounded jittered retries. Inspect `projection_error()` and
   `renewal_error()` for last reload/renewal failure. Call `next_attempt()` after
   an error. `run(scope)` is an optional loop for an already bound graph with a
   cache lifecycle adapter; it does not create an executor.
4. Call `shutdown()`, drop the owner future, and drain/fence the reactor. Cancellation
   of readiness retains its duplicated FD until the runtime's completion fences.

`LocalSigningIdentity::signing_identity(roots)` returns security's verified
`Arc<SigningIdentity>`. Its crate-private PKCS#8 accessor supports TLS without
exporting private material through control DTOs. Private buffers are non-Debug and
zeroized on drop. Bootstrap server roots are independent from peer issuer roots.

## Cache acceptance

`CacheRegistry` validates canonical names/paths and computes removal-first events.
It never unlinks application-owned origin sockets. Filesystem listener ownership
and draining belong to the client/listener owner.

For atomic resource admission, attach `CacheLifecycle`. Its `stage(definitions)`
must prepare all fallible changes and return a rollback-on-drop `CacheTransition`.
`commit()` is infallible, stops removed-cache admission, arranges lease/I/O draining,
and activates additions. `SnapshotStore::publish_staged` calls it only after all
validation/lease-cap checks and before making the immutable snapshot visible.
Commit runs under the publication lock and must not call back into the snapshot
store. Without this adapter, `progress` publishes configuration and returns events
for explicit caller-managed reconciliation; filesystem resource atomicity is then
the caller's responsibility. The convenience `run` requires the adapter.

`PublishedState` is shared across workers. `SnapshotStore::cursor` is an accepted
in-memory cursor, not a separately persisted counter. Restart starts with no cursor
and requests the complete current publication. Bounded history tracks both snapshot
and independently leased membership Arcs and rejects updates rather than evicting
in-flight state.

## Transport boundaries

Production uses rustls server-name/chain validation, optional client identity,
disabled resumption, and one HTTP request per TLS connection. Header/body limits,
strict framing, duplicate header rejection, chunked decoding, deadline/cancellation,
and certificate expiry bound connections. Retry-After accepts decimal seconds and
IMF-fixdate. The DNS adapter honors nameservers/search/ndots in resolv.conf and
performs bounded A/AAAA UDP queries. Truncated responses fail closed; TCP DNS,
NSS/hosts-file policy, HTTP proxies, HTTP/2, chunk extensions/trailers, and legacy
HTTP-date variants are not implemented.

## Verification and cross-owner constraints

Run `cargo test --manifest-path cmd/racer-dataplane/Cargo.toml --lib control::`.
When unrelated unit tests prevent compilation in the shared worktree, run
`bash cmd/racer-dataplane/src/control/check.sh` to link the public-API smoke tests
against the real production library. That additional harness requires the sibling
`racer-control-plane-implementation` worktree for the authoritative Go rejection
vectors. It does not replace the module's full unit tests.

Public Go fixtures are copied under `testdata/` with canonical content and membership
SHA-256 assertions in `codec.rs`. Certificate private keys used by TLS/enrollment
tests are generated per test in the crate's ignored target directory and removed
on fixture drop. Tests cover malformed JSON/projections, lease bounds, durable
identity retry/recovery, incorrect SAN identity, server-authenticated TLS and mTLS.

Topology's member validation must accept the wire contract's UTF-8 rail fabrics
(nonempty, excluding NUL/CR/LF), including the Go fixture's Unicode value. A stricter
ASCII-only topology validator rejects a valid decoded publication.

End-to-end Go control server interoperability remains unavailable while that server
is a stub. The real loopback rustls fixture verifies transport but is not a
controller end-to-end test. The test fixture's poll-based driver is test-only;
production io_uring transport also requires a host that permits io_uring setup.
