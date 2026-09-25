# Racer store version 1 and integration

Storage ownership is `cmd/racer-dataplane/src/store.rs` and `src/store/` only.
Checkpoint/recovery implementation is delegated exclusively to the checkpoint,
checkpoint_format, and recovery modules and checkpoint_tests. Other storage files
are implemented by the storage owner. Dependencies and cross-component wiring
remain with their owners.

## Disk and record protocol

Each worker opens one bounded sparse slab, `worker-<worker>-slab-0.dat`. Configured
slab bytes are the capacity, not a growth increment. Files are exclusively flocked,
opened with O_DIRECT/O_NOFOLLOW, and use STATX_DIOALIGN from the opened descriptor.
Missing alignment information fails closed. Existing nonempty files must have the
configured size; initialization uses set_len, never payload scanning or zeroing.
Segment geometry must hold a full 16 MiB page, tag, maximum record header, and
alignment padding. Buffers, extents, quotas, FDs, and segment leases remain owned
through the reactor's final completion fence. Short I/O fails the record; no
buffered fallback or unaligned continuation is attempted.

Record integers are little endian. Version 1 layout is:

1. Eight bytes `RCRPAGE1`, u32 version (1), u32 header byte count.
2. u64 segment generation, u64 total object length, u64 page number.
3. u32 plaintext length, u32 ciphertext length (plaintext plus 16-byte tag).
4. 16-byte key ID, 24-byte nonce, 32-byte cache key.
5. u32 cache UID length, u32 ETag length, exact UTF-8 cache UID and strong ETag.
6. SHA-256 of all preceding header bytes, then exact original ciphertext.
7. Zero padding to the file's direct-I/O length/next-offset alignment.

UID and ETag encodings are bounded to 4096 and 8192 bytes; the header is bounded to
16384 bytes. Strong ETags use the model's quoted HTTP representation. The header
hash validates framing, not payload authenticity. No payload CRC/hash is added.
The fill/crypto boundary authenticates the entire ciphertext before delivery.
Record identity, immutable descriptor, key ID, generation, and expected extent
must match the selected index entry. The reader returns original nonce/tag/bytes.
Unknown versions are misses; changing this layout requires a new format version.

## Index, allocation, and write ordering

Page mappings include immutable version metadata and key ID. A bounded standalone
catalog retains HEAD-only and empty descriptors. Current-version TTL pointers are
volatile, only refreshed by metadata revalidation, and never checkpointed. Catalog
eviction cannot remove page-attached descriptors. Recovery is all-or-nothing at
the shard level and rejects duplicate identities or conflicting lengths.

Segments append whole padded records, seal insufficient tails, and use non-wrapping
generations. The second-chance clock removes only matching mappings, forbids new
leases on evicting segments, and waits for leases before recycling. No live bytes
are copied. Bounded state is independently limited by configured slab capacity,
page entries, metadata entries, dirty bytes, ciphertext bytes, and queue entries.

Enqueue retains a credential-free CiphertextCopy and dirty reservation and reserves
the exact padded staging charge before accepting a queue entry. Original
ciphertext and aligned staging are separately charged. Index publication follows
full-record completion and rechecks retirement and segment state. A failed or
abandoned write discards its dirty copy; reactor-owned staging and lease remain
alive until completion. A publication lease also protects the return-to-index
window. Dirty tickets prevent old cleanup from removing replacement work.

## Checkpoints and crash semantics

Checkpoint version 1 uses bounded little-endian encoding with magic `RACERCP\0`,
version, sequence, image size, and shard count, followed by geometry, segments,
page mappings (including key IDs), and standalone descriptors. A final SHA-256
covers the complete preceding image. Maximum encoded size is 64 MiB. Ordering is
canonical. Unknown versions, trailing bytes, bad checksums, impossible geometry,
overlapping/invalid extents, duplicate ownership, and conflicting metadata reject
the image. See checkpoint_format.rs for exact field order and structural bounds.

One coordinator collects snapshots while each owner holds its segment freeze.
Completed index state and allocation state are copied synchronously without an
await. Dirty mappings and all freshness are excluded. The coordinator must retain
every freeze through publication and release every owner with finish_snapshot on
success, error, or cancellation. It must serialize checkpoint publication with
key/cache retirement and with other publications.

Publication writes a complete uniquely named temporary file, closes it, then
renames over the slot opposite the newest valid checkpoint (`checkpoint.0` or
`checkpoint.1`). It does not fsync files or directories. Crash loss is unbounded;
neither O_DIRECT nor a successful rename claims power-loss durability. Startup
loads only those two bounded files, selects the newest structurally valid,
compatible image, seals recovered open segments, restores no freshness, and
discards unavailable-key mappings. Both invalid/missing means an empty cache.
Uncheckpointed slab contents are unreachable and are never scanned.

## Integration API

- Call Store::configure(admission, queue_entries, page_entries) before admission.
- Await Store::open() on the owner worker before checkpoint load/install. This
  discovers direct geometry and configures the actual live segment table and both
  checkpoint/recovery components. Constructors do not create files or threads.
- The coordinator loads one image, verifies the exact configured worker set and
  page/metadata ownership against its stable WorkerMap, distributes each shard,
  and awaits recovery.install_shard_with_keys before opening listeners. The
  available-key list must come from the accepted keyring, never from disk.
  For cache-scoped keyrings, Recovery::filter_available_keys(&mut image, predicate)
  takes `(cache UID, key ID)` and must run before distribution/install. The legacy
  key-ID-only slice hooks assume globally unique key IDs.
- Drive writer.progress(budget, scope) with a worker maintenance scope, polling
  concurrently with reactor.poll_budgeted. Keep a pending future alive between
  polls. Do not borrow a request credential context or create an extra executor.
  Progress counts attempted copies, including discarded copies. Overloaded,
  ENOSPC/other cache I/O failure, missing keys, cancellation, and expired maintenance
  deadlines discard disposable persistence without a fatal progress error. Structural
  configuration/corruption errors remain errors. Exactly one progress driver is allowed.
- Fill can call reader.read_with_token and reader.invalidate(token) after AEAD
  failure; conditional invalidation cannot remove a replacement mapping.
- Retirement first serializes against checkpoint publication, calls
  writer.retire_key(cache,key) or remove_cache(cache), drains/fences outstanding
  I/O, and calls checkpoint.invalidate_persisted to remove both recoverable
  generations before key release. This sacrifices unrelated cache recovery
  instead of requiring payload scans. The security owner
  additionally drains memory/crypto/peer leases. Tombstone bounds fail closed.
- Stopping Admission does not prevent previously accepted fills from handing over
  their dirty reservations. Enqueue uses Admission::reserve_completion for padded
  staging, with the same hard ciphertext limit, and must follow Store::open.
  Once accepted fills have finished handing off, call writer.stop_admission or drain.
  On the shutdown deadline call writer.cancel_pending_writes: it closes enqueue,
  discards unsubmitted copies, and cancels the active maintenance scope. Continue
  polling the active progress future and reactor. Never refresh its deadline or
  retry discarded entries. Dropping the progress future abandons publication but
  the reactor keeps submitted buffers, dirty quota, and segment leases through CQEs.
  writer.drain performs these deadline actions and waits for write fences; poll it
  alongside any already active progress driver and reactor completions.
  is_idle includes slab write fences even after the progress future is dropped.
  pending_count includes active copies, queued_count only unsubmitted copies,
  writes_in_flight counts submitted/unconsumed fenced write owners, and
  discarded_count is a saturating persistence-loss counter. Key retirement may
  empty the index/queue before writes_in_flight reaches zero; key release must wait
  for that fence plus the independent memory/crypto/transport barriers.

Staging headroom is reserved per accepted dirty copy rather than borrowed later
from holders that may wait for that same dirty copy. If there is no padded staging
quota, enqueue returns Overloaded before acceptance and releases the supplied dirty
reservation; the already verified memory result remains usable. The fill owner
must treat this as skipped persistence, not an acquisition/node failure. Provision
ciphertext quota for original retained pages plus padded accepted writes, or reduce
dirty queue admission. Completion admission bypasses the stopped flag, not the hard
byte limit. No runtime/read edits or uncharged allocation are required by this policy.

External contracts required: Reservation::amount/class, StrongEtag::as_bytes and
parse, BufferPool::ciphertext, and real Reactor::read_at/write_at completion owners.
No storage code changes these owners' files.

## Focused verification

`python3 cmd/racer-dataplane/src/store/check-component.py` compiles the actual
storage, model, memory, admission, reactor, and configuration sources into a
focused test binary under the existing Cargo target directory. It substitutes no
production behavior and permits storage testing while unrelated application
integration does not compile. Build Cargo dependencies first. Normal cargo tests
also include the same storage tests. Real filesystem tests require O_DIRECT,
STATX_DIOALIGN, and io_uring and fail visibly if unavailable.

Checkpoint metadata file opening, bounded reads/writes, and rename currently run
synchronously within the checkpoint/recovery futures. Payload slab reads/writes
use the reactor. Application scheduling must treat checkpoint publication as a
maintenance operation; it is not a nonblocking reactor metadata-I/O adapter.
