# uds-endpoint

Linux filesystem ownership for Unix stream socket endpoints. This crate depends
only on libc, not Racer identifiers, control, HTTP, or a runtime.

## API and boundary

- `open_directory(path, mode)` and `child_directory(parent, name, mode)` open or
  create directories using O_NOFOLLOW. The walk starts at `/` (also for relative
  input, preserving the extracted behavior). Child names must be single normal
  components; callers validate their application names.
- `restrict_directory(directory, remove)` removes only requested permission bits
  from a directory owned by the effective UID and verifies inode/UID/GID/mode.
- `file_path`, `same_inode`, and `pin_socket` support operations on pinned inodes.
  A descriptor pathname remains valid only while its descriptor is retained.
- `Layout` supplies persistent lock/canonical names, witness prefix, and the
  temporary-name predicate. Names are caller-validated single components and
  namespaces must be disjoint and stable across restarts.
- `EndpointOwner::acquire(directory, layout, lock_mode)` holds a nonblocking
  exclusive flock on a private single-link regular file. Acquisition refuses
  group/world-writable or foreign-owned directories. `validate` checks directory
  and lock-path identity. The lock inode persists on drop; explicit unlock also
  releases ownership when a fork temporarily retains a descriptor alias.
- Recovery validates all witness and temporary inodes before deleting any socket
  pathname. A canonical socket requires a hard-link witness. Every witness must
  refuse a nonblocking connection; live and foreign sockets are preserved.
- `BoundSocket::bind(directory, owner, basename, witness)` binds the witness first,
  hard-links the staging name, and makes the listener nonblocking. It exposes
  `accept`, `AsRawFd`, `identity`, and a worker-local shared `basename` for the
  caller's publication transaction. It retains its owner until inode-checked
  cleanup finishes. Failed pathname removal retains the witness for recovery.
  The directory must already be covered by the supplied owner. The caller sets
  socket permissions after bind, using `pin_socket` to reject symlink swaps.

Racer retains `/run/racer/<name>/client`, cache definition validation, naming and
mode policy (directories 0755, lock 0600, socket 0666), retirement, admission,
readiness, and control transitions. Its simulation remains exclusively cfg(test)
in the application. The optional `test-util` feature exposes only an inherited
lock-descriptor clone for the retained root lifecycle test; it does not enable
simulation or replace production filesystem calls.

## Transactional publication

`publication::Endpoint` abstracts only inode checks, pinned-directory identity,
rename, and cleanup-name bookkeeping. `BoundSocket` implements real operations;
Racer's adapter retains its cfg(test) simulation and rename fault injection.
The generic journal has no runtime or simulation feature dependency.

- `Replacement::prepare(next, previous, temporary, canonical)` checks canonical
  ownership and directory identity, or canonical absence for a new endpoint.
  No pathname is changed during this precheck.
- `Publication::publish` rechecks both exchange inodes and uses RENAME_EXCHANGE
  when replacing an owned endpoint. New endpoints use RENAME_NOREPLACE, which
  rejects destinations created after prepare. Cleanup basenames change only
  after a successful rename. Only successful renames enter the journal.
- `rollback` and Drop restore replacements in reverse order. Foreign canonical
  or temporary inodes are not exchanged. If canonical is absent, the previous
  owned staging inode can be restored with RENAME_NOREPLACE. A failed rollback
  rename leaves cleanup names unchanged and is not retried by Drop.
- `commit(&mut self)` only disarms rollback: no filesystem operation, allocation,
  or owner drop. `previous()` lends the retained previous owners so the caller
  can queue them for deferred cleanup before dropping the journal. New endpoints
  with no previous owner are removed by final inode-owner cleanup, not rollback
  rename. After commit/rollback the journal must not be reused for publication.

The caller stages every bind/chmod before publishing, serializes directory access,
and supplies validated names and the correct endpoint owner. Racer retains cache
selection/maps, generation changes, cancellation checks and yields, preallocated
deferred cleanup, retirement, and the infallible CacheTransition commit. Its Drop
explicitly rolls back before resetting the preparation guard.

`ReadyListeners` remains application-owned: the epoll set is still coupled to
ClientListeners generations, weak BoundListener owners, Racer request scopes,
and HttpIo reactor leases. A later runtime-owned change could isolate an epoll
descriptor/index mechanism, but this crate does not extract service admission or
alter reactor ownership.

## Focused validation

Run from `cmd/racer-dataplane`, with `CARGO_BUILD_JOBS=2` and the repository's
external timeout wrapper:

```sh
timeout --signal=TERM --kill-after=10s 300s env CARGO_BUILD_JOBS=2 cargo test -p uds-endpoint
timeout --signal=TERM --kill-after=10s 300s env CARGO_BUILD_JOBS=2 cargo test -p uds-endpoint --test publication
timeout --signal=TERM --kill-after=10s 300s env CARGO_BUILD_JOBS=2 cargo test -p racer-dataplane --lib client::tests::
timeout --signal=TERM --kill-after=10s 300s env CARGO_BUILD_JOBS=2 cargo check -p racer-dataplane --lib
```

Generic tests use a non-Racer layout and project-local temporary directories.
Root tests remain intact, including actual HTTP exchange, simulated publication,
permission and rename fault injection, inherited lock references, and SIGKILL
recovery of both committed and prepared endpoints.
