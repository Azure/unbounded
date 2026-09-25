# Control owner integration

All constructors are inert. The sole owner attaches
`Rc::new(ReactorControlIo::new(reactor))` through `attach_io`.
The worker must poll both the control future and `Reactor::poll_budgeted`.
No production executor, thread, blocking DNS lookup, or synchronous socket wait is
created by control. Serving file opens, metadata, reads, writes, fsync, rename, and
unlink use completion-owned io_uring SQEs with `IOSQE_ASYNC`. `attach_io` propagates
`ControlIo::reactor()` to enrollment and SecretWatcher. Legacy synchronous methods
are for explicit pre-worker provisioning/tests only and reject use after reactor
attachment. The serving `start/progress` path uses only async filesystem methods.

## Filesystem/runtime handoff

The new `src/runtime/filesystem.rs` is a reactor child via the exact declaration
`#[path = "filesystem.rs"] pub mod filesystem;` in `runtime/reactor.rs`. This shares
the existing private submission table and original/cancel completion fences. No
auxiliary ring or hidden executor is created. The runtime owner is free to edit
`reactor.rs`; the module declaration is the only control-owned hookup there.

New methods: `Reactor::file_open`, `file_stat`, `file_mkdir`, `file_sync`,
`file_rename`, `file_unlink`, `file_buffer`, `file_bytes`, `file_fence(RequestId)`.
Reads/writes use existing `read_at`/`write_at` with owned zeroizing buffers and quota.
Each enrollment transaction uses a distinct internal RequestId. Retry fences the
previous transaction before reusing staging names, including a late rename after
future abandonment. Activation follows write completion, file fsync, rename,
directory fsync, pending unlink, and directory fsync. Recovery validates committed
state and reestablishes the directory durability fence before reuse. Failure never
advances the publication cursor. Projection reads pin `..data` using one `openat2`
with `RESOLVE_BENEATH | RESOLVE_NO_MAGICLINKS`, then read only relative to that FD.

Direct application startup must attach the reactor to `Enrollment` and use
`prepare(scope)`, `read_token_async(scope)`, `load_identity_async(scope)`, and
`accept_response_async(response, scope)`. SecretWatcher provides
`attach_reactor`, `read_bundle_async(scope)`, and `reload_async(scope)`.
`bind_keyring/activate_identity` perform no filesystem work, consuming the bundle
staged by async startup. These APIs preserve authenticated NodeId resolution.

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

Exact application adapter signatures:

```rust,ignore
trait CacheLifecycle {
    fn stage(&self, definitions: &[CacheDefinition]) -> Result<Box<dyn CacheTransition>>;
}
trait CacheTransition { fn commit(self: Box<Self>); }
ControlClient::attach_cache_lifecycle(&self, lifecycle: Rc<dyn CacheLifecycle>);
```

`stage` must consume/reserve already asynchronously prepared resources, never run
blocking filesystem operations. If preparation is unavailable it returns
`Overloaded`/`Unavailable`, preserving the cursor for retry. `commit` updates
admission and resource ownership and queues asynchronous drains/fences; it must
not perform blocking bind/unlink or wait for I/O under the publication lock.

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
During shared-worktree interface changes, run
`CONTROL_CHECK_BASE=3d3db7f2 python3 cmd/racer-dataplane/src/control/check-component.py`.
This compiles every live control unit test against the exact committed production
dependencies at that revision, including the real reactor filesystem extension.
It does not fake dependencies or claim the concurrent application graph compiles.
When unrelated unit tests prevent compilation in the shared worktree, run
`bash cmd/racer-dataplane/src/control/check.sh` to link the public-API smoke tests
against the real production library. That additional harness requires the sibling
`racer-control-plane-implementation` worktree for the authoritative Go rejection
vectors. It does not replace the module's full unit tests.

All 20 control tests passed against the pinned production graph, including real
kernel io_uring filesystem/TLS tests. The full Cargo filter passed all 16 tests
before the additional persistence-boundary tests were added; its latest rerun was
blocked by concurrent read/peer interface changes. Rerun Cargo after integration.

Public Go fixtures are copied under `testdata/` with canonical content and membership
SHA-256 assertions in `codec.rs`. Certificate private keys used by TLS/enrollment
tests are generated per test in the crate's ignored target directory and removed
on fixture drop. Tests cover malformed JSON/projections, lease bounds, durable
identity retry/recovery, incorrect SAN identity, server-authenticated TLS and mTLS.

The Unicode-fabric mismatch was reported and fixed by the topology owner in
`d141c08f`. Control did not edit topology files. The Go Unicode fixture remains
the cross-language regression contract.

End-to-end Go control server interoperability remains unavailable while that server
is a stub. The real loopback rustls fixture verifies transport but is not a
controller end-to-end test. Both the retained test-only readiness driver and a real
`ReactorControlIo` TLS fixture are tested. Real io_uring tests executed successfully
on this implementation host; restricted kernels skip only ENOSYS/EPERM/EACCES at
ring setup and report that explicitly. Filesystem tests interrupt each persistence
boundary, check old-or-complete-new visibility, retry fencing, pinned projection
generations, partial reads, and completion-owned quota/FD lifetime.
