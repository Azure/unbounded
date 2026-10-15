# uds-endpoint

Standalone Linux filesystem-ownership helper intended for the replacement
`racer-dataplane`. It is a workspace member, not yet an application dependency.
This unpublished crate depends only on `libc` in production. The optional
`simulation` feature adds `uring-runtime` and its simulated filesystem. The crate
does not choose an application model, protocol, or service-activation policy. It
owns Unix stream socket names and a worker-local publication journal, not
application readiness or admission.

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
check and a mutation.

Callers must secure the endpoint's parent and all ancestors against untrusted
rename or replacement for as long as clients use its pathname. The directory walk
rejects symlinks, but does not validate ancestor ownership or permissions.
`EndpointOwner` validates the supplied directory and its lock, not the pathname
leading to them. Ancestor renames do not redirect retained directory handles:
publication, chmod, and cleanup still target the pinned directory and inodes.
Clients resolving the original pathname can instead reach a replacement directory
and socket. Pinned-inode safety does not guarantee client pathname integrity.

Callers must also ensure that access and default ACLs grant only intended access,
including on directories created by the walk. The crate does not inspect or remove
ACLs. `restrict_directory` removes mode bits, not default ACLs. A socket can inherit
named-user or named-group entries from its directory's default ACL. Mode 0600 masks
those entries without deleting them; a later `set_mode(0o660)` can make their read
and write permissions effective again. With an extended access ACL, the group mode
bits describe the ACL mask, not just the owning group's permissions. Remove
unwanted default ACLs before creating endpoints, or verify that all inherited
entries fit the final client-access policy; checking mode bits alone is not enough.

## Layout and ownership API

`Layout::new(lock, canonical, witness_prefix, temporary_name)` returns
`io::Result<Layout>`. All configured names must be normal single components
(nonempty, not `.`/`..`, no slash or NUL, at most 255 bytes). The lock, canonical,
witness, and temporary namespaces must be disjoint and stable across restarts.
Construction checks known overlaps. Because the temporary-name predicate is a
function, bind and recovery also check each actual temporary and derived witness
name. A filesystem-valid name can still exceed Linux's Unix socket address limit.

Use a fresh temporary generation token for each bind attempt. Do not reuse it
while either its derived witness or its temporary pathname remains. Failed
staging can retain a witness until a later owner recovers it. After an exchange,
the previous generation uses the next generation's temporary pathname for cleanup,
while the next generation keeps its own witness. Dropping either generation alone
may therefore leave the token unavailable, even if the temporary pathname is gone.

- `open_directory(absolute_path, mode)` walks from `/`, opening every component
  without following symlinks and creating missing directories. Relative paths and
  explicit `.`/`..` components are rejected before creation. `child_directory`
  accepts exactly one normal component. Creation modes are subject to umask.
- `restrict_directory(directory, remove)` only removes permission bits on an
  effective-user-owned directory; non-permission bits in `remove` are ignored.
  It verifies inode, UID, GID, and resulting mode. Linux can also clear setgid.
  Actual permission changes require a descriptor usable with `fchmod`, such as
  those returned by the directory helpers. An `O_PATH` descriptor succeeds only
  for a no-op; a required change returns `EBADF`.
- `EndpointOwner::acquire(directory, layout)` requires an effective-user-owned
  directory without group/world write access. There is no caller-selected lock
  mode. The persistent lock must be a single-link regular file owned by the
  effective UID, with no execute, special, group, or other permission bits.
  Restrictive owner permissions, including mode 000, are repaired to 0600 on the
  pinned inode before nonblocking exclusive `flock`. The lock is never unlinked.
  `validate` checks directory and lock identity and their permissions.
- [`BoundSocket::bind(Rc<EndpointOwner>, temporary)`](src/lib.rs) derives both
  directory and witness from the owner: callers cannot bind under an unrelated
  owner. It binds a nonblocking socket at the witness name, pins its inode, and
  sets mode 0600 before calling `listen`, then hard-links the temporary name.
  `accept`, `AsFd`/`AsRawFd`, and `identity` expose listener operations.
  `basename()` returns a read-only `String` snapshot, not mutable publication
  bookkeeping.
- `set_mode(mode)` accepts only 000 through 0777 and changes the retained socket
  inode through procfs, never a replacement path. It validates the owner first.
  The caller chooses the final client-access policy before publication.
- `file_path`, `same_inode`, and `pin_socket` support pinned-inode operations.
  Descriptor paths remain usable only while the descriptor is retained;
  `pin_socket` rejects non-sockets, foreign ownership, and identity changes.

Final socket drop validates the owner before removing matching inode names,
retaining the owner until cleanup finishes. Cleanup is best effort. Failed owner
validation or a stat/unlink error on the cleanup name retains its witness for
recovery. Explicit owner drop unlocks the persistent lock,
including when an inherited or test-only cloned descriptor aliases it. Callers
must not continue operating an inherited listener after its owner unlocks.

Named metadata checks temporarily open an `O_PATH` descriptor. Descriptor
exhaustion can therefore prevent validation and cleanup, leaving names and witnesses
for recovery rather than risking an unverified unlink. Recovery uses a bounded
number of descriptors, but still needs spare descriptors. Restore available
descriptors and close the old listeners and owner before acquiring a new owner to
recover retained names; do not blindly unlink witnesses or retry the same token.

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
   permission errors or other syscall errors are not treated as stale.
   Restore any changed mode after each probe, even for stale witnesses, before
   probing another witness or removing names. A restore error stops recovery and
   takes precedence over the probe result; otherwise preserve the probe error.
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

`rollback` attempts reverse-order restoration. Owned replacements
are exchanged back; if canonical is absent, the old owned temporary can be moved
back without replacement. New endpoints move from canonical back to their vacant
temporary name even when an external `Rc` keeps the listener alive. Foreign names
are never knowingly exchanged or overwritten. Failed guards or renames leave
cleanup names unchanged. The must-use `RollbackOutcome` distinguishes `Restored`
(all entries restored), `Incomplete` (some entries could not be restored), and
`Committed` (rollback disabled, even for an empty journal).

On `Incomplete`, use `entries()` to inspect each entry's `still_published` flag,
names, and `next`/`previous` owner references in publication order. This flag tracks
journal restoration, not current filesystem ownership. Keep the journal and retry
`rollback()` after fixing the failure; only unfinished entries are retried. If you
must drop the journal, first clone both owners of each unfinished entry to retain
them while handling the failure. Dropping the last `next` owner may still unlink
its canonical name. Drop attempts rollback once only if no explicit rollback has
started; it never retries an explicit failure. Neither form promises full
restoration after interference or filesystem failure.

`commit(&mut self)` only disarms rollback: no filesystem calls, allocations, or
owner drops. `previous()` exposes retained previous owners for deferred cleanup.
After commit or rollback, further publication returns `PublicationCompleted`.
Application control-state commit, cancellation, retirement, readiness, and when to
drop old listeners remain the caller's responsibility.

**Exchange does not migrate the listen backlog.** Connections queued on the old
listener stay there; new connections to canonical reach the new listener. Already
accepted streams also remain independent. Callers must retain/drain or deliberately
retire the old listener according to their service policy.

`publication::Endpoint` is a public, trusted adapter contract for the journal and
wrappers. Its `set_basename` hook assigns the supplied name without validation.
The caller must supply a prevalidated name holding the socket after a successful
rename. The update must be unconditional, with no I/O, allocation, panic, or user
code: a probe failure after rename must not leave stale cleanup bookkeeping.
Successful rename must also return without unwinding after filesystem mutation.
The read-only `basename()` snapshot does not hide or restrict this adapter hook.
Calling the hook with an arbitrary name can break cleanup; applications should
use the publication journal to keep rename and bookkeeping together.

The host adapter renames only its own socket inode between canonical or valid
temporary names, never lock or witness names. The caller must still check the
destination owner before an exchange.

## Simulation limits

`simulation::BoundSocket::bind(sim, directory, layout, temporary)` uses the same
`Layout` checks for temporary and derived witness names as the host adapter.
Publication requires the configured canonical name. To create a canonical socket
in a fixture, bind a temporary name and publish it. Completed publication returns
`PublicationCompleted`; journal reservation failure reports `OutOfMemory`.
Rename checks both names against the layout and requires ownership of the source,
as on the host. Only canonical and valid temporary names are allowed, not lock or
witness names. The caller must still check the destination owner before exchange.

`sim.inject("metadata", Fault::Errno(errno))` fails the next named metadata lookup
in that simulation, including through its clones. Checked ownership and absence
probes and rename preserve errors other than `NotFound`, which means absence.
The boolean `owns` and `absent` hooks return false on probe errors; they do not
provide diagnostics. This hook tests error handling, not host permission
enforcement or descriptor exhaustion.

The adapter tests inode-checked cleanup, retained directory identity, mode changes,
and the shared publication journal. Bind returns with mode 0600. The runtime
combines bind and listen, so this does not test the host's chmod-before-listen
ordering. `set_mode` checks the current named inode; it does not retain a separate
socket inode pin as the host does.

There is no simulated `EndpointOwner`, flock, hard-link witness, or startup
recovery. The adapter does not check directory/lock ownership or permissions,
model UID/GID or ACL enforcement, or prove crash recovery. Directory creation and
socket addresses follow the runtime model, not the host's descriptor-relative
path walk and Unix address limits. Use host tests for those guarantees.

## Validation

Run from `cmd/racer-dataplane`. No privileged test setup is needed. Integration
fixtures use Cargo's `CARGO_TARGET_TMPDIR`. Unit fixtures use `UDS_TEST_TMPDIR` if
set, otherwise `CARGO_TARGET_DIR/tmp`, falling back to the workspace's `target/tmp`.
Empty overrides are ignored; relative paths use the test process's working
directory. Both unit fixtures share this selection, including the admission
subprocess fixture. Cleanup avoids a second panic during unwinding. Crash/umask
cases run in isolated subprocesses, not through process-wide umask changes or fork
inside parallel test workers.

```sh
timeout --signal=TERM --kill-after=10s 300s cargo +1.96.0 test --locked -p uds-endpoint --no-default-features -j 2
timeout --signal=TERM --kill-after=10s 300s cargo +1.96.0 test --locked -p uds-endpoint --all-features -j 2
timeout --signal=TERM --kill-after=10s 300s cargo +1.96.0 clippy --locked -p uds-endpoint --all-targets --all-features --no-deps -j 2 -- -D warnings -D clippy::undocumented_unsafe_blocks
timeout --signal=TERM --kill-after=10s 300s cargo +1.96.0 fmt -p uds-endpoint --check
```

The ignored `crash_state_driver` and `descriptor_limit_driver` tests are invoked
by their parent integration tests with private fixture arguments. The ignored
`admission_tests::permissive_umask_driver` is invoked by
`admission_tests::permissions_precede_listen` in a subprocess with an isolated
umask. Do not run these drivers directly. The optional `test-util`
feature exposes only a cloned lock descriptor for lifecycle tests. It does not
substitute a filesystem model for production syscalls. The separate `simulation`
feature provides a simulated socket adapter; it does not replace the host tests.
