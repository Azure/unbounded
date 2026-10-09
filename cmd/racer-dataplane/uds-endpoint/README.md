# uds-endpoint

Private Linux filesystem-ownership helper for `racer-dataplane`. This unpublished
crate depends only on `libc`. It does not depend on an application model, runtime,
protocol, reactor, or service-activation policy. It owns Unix stream socket names
and a worker-local publication journal, not application readiness or admission.

## Platform and trust boundary

Linux with procfs mounted at `/proc` is required. Operations use descriptor-relative
`openat`/`linkat`/`unlinkat`, `O_PATH`, `O_NOFOLLOW`, `flock`, and `renameat2` with
`RENAME_EXCHANGE`/`RENAME_NOREPLACE`. The filesystem must support socket hard links
and those rename operations. Missing capabilities are errors, not fallbacks to
unsafe pathname operations. No additional privileges are needed for owned paths.

Cooperating effective-UID owners must serialize directory mutation and respect
the persistent lock. A hostile process with the same UID or root is outside the
boundary: Linux has no compare-inode-and-unlink syscall. Inode checks prevent
accidental deletion of already-replaced names, not adversarial races between a
check and a mutation. Ancestor renames do not redirect retained directory handles.

## Layout and ownership API

`Layout::new(lock, canonical, witness_prefix, temporary_name)` returns
`io::Result<Layout>`. All configured names must be normal single components
(nonempty, not `.`/`..`, no slash or NUL, at most 255 bytes). The lock, canonical,
witness, and temporary namespaces must be disjoint and stable across restarts.
Construction checks known overlaps. Because the temporary-name predicate is a
function, bind and recovery also check each actual temporary and derived witness
name. A filesystem-valid name can still exceed Linux's Unix socket address limit.

- `open_directory(absolute_path, mode)` walks from `/`, opening every component
  without following symlinks and creating missing directories. Relative paths and
  explicit `.`/`..` components are rejected before creation. `child_directory`
  accepts exactly one normal component. Creation modes are subject to umask.
- `restrict_directory(directory, remove)` only removes permission bits on an
  effective-user-owned directory; non-permission bits in `remove` are ignored.
  It verifies inode, UID, GID, and resulting mode. Linux can also clear setgid.
- `EndpointOwner::acquire(directory, layout)` requires an effective-user-owned
  directory without group/world write access. There is no caller-selected lock
  mode. The persistent lock must be a single-link regular file owned by the
  effective UID, with no execute, special, group, or other permission bits.
  Restrictive owner permissions, including mode 000, are repaired to 0600 on the
  pinned inode before nonblocking exclusive `flock`. The lock is never unlinked.
  `validate` checks directory and lock identity and their permissions.
- `BoundSocket::bind(Rc<EndpointOwner>, temporary)` derives both directory and
  witness from the owner: callers cannot bind under an unrelated owner. It binds
  the witness first, pins the socket, sets mode 0600, makes the listener nonblocking,
  and hard-links the temporary name. `accept`, `AsFd`/`AsRawFd`, and
  `identity` expose listener operations. `basename()` returns a read-only `String`
  snapshot, not mutable publication bookkeeping.
- `set_mode(mode)` accepts only 000 through 0777 and changes the retained socket
  inode through procfs, never a replacement path. It validates the owner first.
  The caller chooses the final client-access policy before publication.
- `file_path`, `same_inode`, and `pin_socket` support pinned-inode operations.
  Descriptor paths remain usable only while the descriptor is retained;
  `pin_socket` rejects non-sockets, foreign ownership, and identity changes.

Final socket drop removes only matching inode names, retaining the owner until
cleanup finishes. Cleanup is best effort. A stat/unlink error on the cleanup name
retains its witness for recovery. Explicit owner drop unlocks the persistent lock,
including when an inherited or test-only cloned descriptor aliases it. Callers
must not continue operating an inherited listener after its owner unlocks.

## Crash and recovery ordering

The witness is the proof of ownership, not merely a name that refused a
connection. Binding it before chmod and temporary linking ensures even a crash
under restrictive umask leaves evidence. Recovery runs while acquiring the lock:

1. Enumerate and validate the complete configured namespace. Every witness must
   be an effective-user-owned socket with a valid derived name. Every canonical
   or temporary entry must be a socket hard-linked to a recognized witness.
   Unknown or foreign entries in those namespaces fail closed, before repair or
   removal. Unrelated names are not adopted or removed.
2. Pin and revalidate each witness against the namespace snapshot one at a time,
   keeping descriptor use bounded independently of the number of witnesses.
   If a validated witness lacks owner-write permission, add that bit to its pinned
   inode. This permits recovery after bind-before-chmod crashes. Probe every
   witness with a nonblocking connection through its pinned procfs path. Only
   `ECONNREFUSED` proves staleness; success or a busy connection means live, and
   Permission errors or other syscall errors are not treated as stale.
   A successful probe can enqueue a connection that immediately closes on the
   live listener; this is an unavoidable side effect of this liveness check.
3. Revalidate the owner, remove the matching canonical name first, then temporary
   names, then witnesses. The persistent lock remains.

Recovery handles witness-only, prepared witness-plus-temporary, published, and
exchanged-but-uncommitted states. It removes stale owned endpoints; it does not
restore an application's previous generation. These guarantees concern process
failure, not power-loss durability: there is no fsync-backed durable journal.

## Publication and its limits

Stage every bind and final mode before publication, retaining `Rc` owners.
`Replacement::prepare(next, previous, temporary, canonical)` performs no rename.
It rejects identical names, the same owner `Rc` on both sides, foreign ownership,
directory mismatch, and names outside the configured layout. With no previous
owner, the canonical name must be absent.

`Replacement::prepare` allocates cleanup strings; `Publication::publish` rechecks
ownership and reserves journal storage before mutation. Replacements use
`RENAME_EXCHANGE`; new endpoints use
`RENAME_NOREPLACE`, rejecting a destination created after prepare. Successful
renames update cleanup bookkeeping. Each rename is atomic, but a multi-endpoint
publication is not globally atomic: readers can observe intermediate states.

`rollback` and Drop attempt reverse-order restoration only once. Owned replacements
are exchanged back; if canonical is absent, the old owned temporary can be moved
back without replacement. New endpoints move from canonical back to their vacant
temporary name even when an external `Rc` keeps the listener alive. Foreign names
are never knowingly exchanged or overwritten. Failed guards or renames leave
cleanup names unchanged. Rollback is best effort, has no error result, and does
not promise full restoration after interference or filesystem failure.

`commit(&mut self)` only disarms rollback: no filesystem calls, allocations, or
owner drops. `previous()` exposes retained previous owners for deferred cleanup.
After commit or rollback, further publication returns `PublicationCompleted`.
Application control-state commit, cancellation, retirement, readiness, and when to
drop old listeners remain the caller's responsibility.

**Exchange does not migrate the listen backlog.** Connections queued on the old
listener stay there; new connections to canonical reach the new listener. Already
accepted streams also remain independent. Callers must retain/drain or deliberately
retire the old listener according to their service policy.

`publication::Endpoint` is a low-level adapter contract. Successful rename and
cleanup-name bookkeeping must not unwind after filesystem mutation; bookkeeping
must not allocate or invoke user code. It is not an alternative public mechanism
for arbitrary changes to a bound socket's cleanup name.

## Validation

Run from `cmd/racer-dataplane`. No new dependencies or privileged test setup are
needed. Integration fixtures use Cargo's `CARGO_TARGET_TMPDIR`; cleanup avoids a
second panic during unwinding. Crash/umask cases run in isolated subprocesses,
not through process-wide umask changes or fork inside parallel test workers.

```sh
timeout --signal=TERM --kill-after=10s 300s env CARGO_BUILD_JOBS=2 cargo test -p uds-endpoint --all-features
timeout --signal=TERM --kill-after=10s 300s env CARGO_BUILD_JOBS=2 cargo clippy -p uds-endpoint --all-targets --all-features -- -D warnings -D clippy::undocumented_unsafe_blocks
timeout --signal=TERM --kill-after=10s 300s cargo fmt --all --check
```

The ignored `crash_state_driver` and `descriptor_limit_driver` tests are invoked
by their parent integration tests with private fixture arguments; do not run them
directly. The optional `test-util`
feature exposes only a cloned lock descriptor for lifecycle tests. It does not
substitute a filesystem model for production syscalls.
