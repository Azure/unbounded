# Racer store protocol and integration

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

Record integers are little endian. Version 4 is the only accepted/written layout:

1. Eight bytes `RCRPAGE1`, u32 version (4), u32 header byte count.
2. u64 segment generation, u64 total object length, u64 page number.
3. u32 plaintext length, u32 ciphertext length (plaintext plus 16-byte tag).
4. 16-byte key ID, 24-byte nonce, 32-byte cache key.
5. u32 cache UID length, u32 ETag length, exact UTF-8 cache UID and strong ETag.
6. u64 CRC-64/XZ of exact ciphertext including the 16-byte AEAD tag.
7. u32 content-type byte length (zero means absent), then exact content-type bytes.
8. SHA-256 of all preceding header bytes, then exact original ciphertext.
9. Zero padding to the file's direct-I/O length/next-offset alignment.

UID and ETag encodings are bounded to 4096 and 8192 bytes; the header is bounded to
16384 bytes. Strong ETags use the model's quoted HTTP representation. The header
hash validates framing, not payload authenticity. CRC detects accidental corruption,
not malicious modification; it never replaces AEAD authentication.
The fill/crypto boundary authenticates the entire ciphertext before delivery.
Record identity, immutable descriptor, key ID, generation, and expected extent
must match the selected index entry. The reader returns original nonce/tag/bytes.
All versions other than 4, including 1-3, are misses; changing this layout requires
a new format version. There is no legacy reader, migration, or slab rewrite.

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

When a new page encounters a full page index, serialized writer progress runs at
most two rotations of the same segment-level second-chance clock, independently
of free slab space. It forgets the cold victim segment's mappings, including those
in a still-open segment, without changing its bytes, append position, or generation.
Normal slab reclamation still fences recycling on leases. Existing-page replacements
need no index eviction. Queue and staging limits bound acceptance before this pass;
failed writes may leave fewer cached mappings, since the cache is disposable.

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
  Production calls Recovery::filter_available before distribution/install, using
  both the accepted cache UID set and available page keys. Standalone metadata
  requires a current UID and an active page key; entries require their exact key.
  The low-level key-ID-only slice hooks assume globally unique IDs and are not
  sufficient for production cache-UID admission.
- Drive writer.progress(budget, scope) with a worker maintenance scope, polling
  concurrently with reactor.poll_budgeted. Keep a pending future alive between
  polls. Do not borrow a request credential context or create an extra executor.
  Progress counts attempted copies, including discarded copies. Overloaded,
  ENOSPC/other cache I/O failure, missing keys, cancellation, and expired maintenance
  deadlines discard disposable persistence without a fatal progress error. Structural
  configuration/corruption errors remain errors. Exactly one progress driver is allowed.
- Fill can call reader.read_with_token and reader.invalidate(token) after AEAD
  failure; conditional invalidation cannot remove a replacement mapping.
- Removal closes admission using the current positive cache/key set. Lookup and
  late publication consult that set; eviction drops only cache references. A
  submitted writer also checks its original dirty ticket before publishing, so
  removal/reinsertion cannot let an older completion replace a newer write.
  Submitted buffers and segment leases stay owned until actual I/O completion.
  Checkpoint invalidation is not part of retirement: historical keyless ciphertext
  can remain, and recovery filters both slots before installing any shard.
  Production writers use `with_availability` for the bounded positive cache/key
  checks, so rotations do not accumulate writer tombstones. Generation-bound
  key IDs prevent newer bundles from reopening admission for erased epochs.
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
  discarded_count is a saturating persistence-loss counter. Eviction may empty the
  index/queue before writes_in_flight reaches zero; I/O resource owners still await
  their own fences. Ciphertext persistence does not need a secret-key lease.

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

`invalidate_persisted_async` is an explicit reset utility, not a serving-loop key
or cache retirement requirement. Its filesystem operations retain completion owners.
## Record v4 checksum implementation

The pre-deployment format change intentionally rejects all earlier record bytes.
Version 3 used non-reflected CRC-64/ECMA-182 with zero initial/final XOR. Version 4
uses CRC-64/XZ: polynomial `0x42f0e1eba9ea3693` (reflected representation
`0xc96c5795d7870f42`), reflected input/output, initial and final XOR `0xffffffffffffffff`.
Empty input checksums to zero; `123456789` checksums to `0x995dc9bbdf1939fa`.

`crc64fast` **1.1.0** is the locked TiKV implementation (MIT OR Apache-2.0,
declared MSRV Rust 1.70.0, no runtime dependencies). Its released code, not merely
the crate description, defines the XZ parameters: `src/lib.rs` initializes and
complements all-one state and its tests compare against `CRC_64_XZ`.
The package calls `Digest::new`, not `new_table`, in production:

- `src/pclmulqdq/x86.rs`: runtime PCLMULQDQ + SSE2 + SSE4.1 detection.
- `src/pclmulqdq/aarch64.rs`: runtime PMULL + NEON detection and `vmull_p64`.
- `src/pclmulqdq/mod.rs`: eight-way 128-byte folding and Barrett reduction;
  table processing for short inputs and unaligned prefix/suffix bytes.
- Unsupported CPUs use the table backend. No global CPU target flags or optional
  features are needed. The old `pmull` feature is deprecated and has no effect;
  `fake-simd` must not be enabled.

These source paths refer to the
[released v1.1.0 source](https://github.com/tikv/crc64fast/tree/v1.1.0).
This avoids maintaining Racer-specific unsafe SIMD code. The crate's latest
release at investigation was 1.1.0 (January 2024); no claim of recent release
cadence or hardware performance is implied.

The store reader always installs the persisted checksum expectation. Crypto workers
verify it before AEAD plaintext publication. CRC is not authentication: an attacker
can recompute both CRC and the unkeyed header digest, and AEAD remains mandatory.
Resource admission, original ciphertext ownership, padding, and completion fences
are unchanged.

Tests retain legacy fixtures as rejection cases, compare the dispatched and forced
table backends against an independent bitwise reference, and cover random lengths,
alignment/folding boundaries, misaligned input, and a full 16 MiB page plus tag.
The explicit ignored `pclmul_hardware_path_executes_when_available` and
`pmull_hardware_path_executes_when_available` gates assert their required CPU
features and fail on the wrong host rather than silently passing. A cross-build
does not count as executing the ARM backend; Grace hardware verification remains
a separate requirement.

Replacement verification on the x86 implementation host (Rust 1.96.0):

- All-feature unit suite: 817 library tests and 2 binary tests passed, 12 explicit
  library ignores. The store subset independently passed 64 tests (one benchmark
  ignored); the production-dataplane integration target passed 15 (two ignored).
- The explicit PCLMUL hardware gate passed with `--ignored --exact --nocapture`.
  Cargo's resolved feature tree enabled neither `fake-simd` nor `pmull`.
- The released checksum library and Racer checksum facade cross-built for the
  installed `aarch64-unknown-linux-gnu` target. No cross C linker was installed,
  so this was not a full dataplane executable cross-build. No Grace hardware was
  available; the PMULL execution gate was not run or counted as a pass.
- Tests used two build jobs/two test threads, an external 300-second TERM timeout
  with ten-second kill grace, and the project memory-safe cgroup wrapper.
- Cargo formatting ran and changed Rust sources passed targeted rustfmt checks.
  Its unrelated pre-existing module-order change was reverted. Scoped `make fmt`
  ran, but installed lint tooling failed: the default binary was built with Go
  1.26 against Go 1.27 sources, and the available Go-1.27-built binary could not
  decode Go 1.27 export data version 4. No Go files changed; this is not a clean
  Go lint result.
