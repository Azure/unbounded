# Racer extent volume

This document describes the on-disk allocator and index that replaced the
log-structured segments, the in-memory `PageIndex`, and the page part of the
checkpoint file in `cmd/racer-dataplane`. See
[racer-page-alloc.md](racer-page-alloc.md) for the buffer and slab layers
under the volume. The code is in `cmd/racer-dataplane/alloc/src/volume/`.

## Goals

- Support 8 TB and larger disks with bounded index memory. Memory grows with
  the number of slots (about 24 bytes per 16 MiB slot), not with the number of
  records. The index itself lives on disk; a page cache of a set byte size
  holds the hot part, and cold lookups read pages from disk.
- Two durability tiers:
  - Cache: best effort. Allocation always makes progress by evicting cache
    data.
  - Storage: crash safe. A page is acknowledged only after it is durable. One
    bad bit in the metadata must not lose more than a small, bounded set of
    pages.
- Fast, with low overhead. Each worker owns its volume; there is no shared
  mutable state on the hot path. Commits write large contiguous runs.
- Allocate both 16 MiB pages and small pages (4 KiB to 64 KiB, and anything in
  between).
- No write-ahead log. All metadata is in copy-on-write trees. A commit writes
  new pages and then switches one of two superblocks.

## Ownership

Each I/O worker owns one volume, backed by its own `Slab` (a file or a list of
device ranges). The volume is single threaded and uses `Rc` and `Cell`, like
the rest of the store.

## Geometry

- `UNIT_BYTES = 4096`. Every on-disk object starts on a unit boundary. The
  device's direct I/O alignment must divide 4096, or the volume is
  `Unsupported`.
- `SLOT_UNITS = 4104`, so `SLOT_BYTES = 16 MiB + 32 KiB`. A largest page
  record (header, 16 MiB of ciphertext, tag) fits in one slot.
- Slot `i` covers bytes `[i * SLOT_BYTES, (i + 1) * SLOT_BYTES)` of the slab.
  At least 4 slots, at most `1 << 26`.
- Slot 0 is the meta slot. Unit 0 holds superblock A, unit 1 superblock B,
  units 2 and 3 the meta slot's two bitmap copies. Units 4 and up hold
  metadata pages.
- Other node slots hold their two bitmap copies in units 0 and 1 and metadata
  pages from unit 2.
- A page address (`NodeAddr`) is `slot << 16 | unit`. `NodeAddr::NONE` is
  `u64::MAX`.

## Slots and size classes

A slot is free, a node slot (metadata pages), a data slot, or quarantined. A
data slot has one size class and one tier. The classes, in units:

```
1 2 3 4 6 8 12 16 24 32 48 64 96 128 192 256 384 512 768 1024 1536 2048 4104
```

Class 22 (4104 units) is a whole slot. A slot of class `c` holds
`SLOT_UNITS / c` records. A record uses the smallest class that fits it.

Every time a slot is taken it gets a new incarnation number (a volume-wide
counter, persisted in the superblock). Record map entries and retired log
entries carry the incarnation, so entries left over from an earlier use of the
slot are ignored.

### Memory

Resident state (`table.rs`):

- 24 bytes per slot: kind, tier, class, used and reserved record counts,
  incarnation, `retain_until` bound, flags (open, CLOCK reference bit,
  evicting, bitmap copy), and a 4 byte lease count.
- A 520 byte bitmap per node slot. Node slots hold about 3% of the volume for
  4 KiB records, so this is about 10 MB for 8 TB.
- A 520 byte bitmap per data slot that is open for allocation (at most one
  per tier and class), plus closed slots whose bitmap was just loaded for
  reuse.
- A Fenwick tree of used bytes per slot (8 bytes per slot).
- The metadata page cache, bounded by `VolumeOptions::metadata_cache_bytes`
  (default 64 MiB). Pages in use by a running lookup or commit stay in memory
  until released, so the cache can exceed its budget briefly.
- The overlay of uncommitted changes, bounded by `VolumeOptions::dirty_bytes`
  (default: a quarter of the cache budget, at least 8 MiB, about 50,000
  changes). When it is full, `allocate` returns `Busy` and `needs_commit` is
  true until a commit drains it.
- The retired log of the last commit (16 bytes per entry, bounded by the size
  of one commit).

For 8 TB (about 500,000 slots): 12 MB of slot entries, 4 MB of Fenwick tree,
about 10 MB of node bitmaps, plus the configured cache and overlay.

### Leases

A lease pins one slot. Eviction skips leased slots. A read holds a lease while
its I/O runs. A reservation holds a lease until it is published or dropped.

## Trees

All metadata is in three copy-on-write B+trees with the same page format:

| Tree | Key (24 bytes) | Value |
|---|---|---|
| Index | page key | 32 byte `Value` |
| Record map | slot (u32 BE), unit (u16 BE), zeros | 40 byte `RecordDesc`: index key, generation, incarnation |
| Slot table | slot (u32 BE), zeros | 32 byte `SlotSummary` |

The record map maps each record back to its index key. Eviction uses it to
find a slot's keys, and allocation uses it to rebuild the bitmap of a closed
slot before reusing its free records. The slot table is read in full at mount;
the other two trees are read on demand.

### Key

```
cache_hash:   8 bytes  first 8 bytes of SHA-256("racer.volume.cache.v1\0" || cache)
version_hash: 12 bytes first 12 bytes of SHA-256("racer.volume.version.v1\0" ||
                       len(cache) || cache || len(key) || key || len(etag) || etag)
page:         4 bytes  big-endian page number
```

Lengths are 4 byte big-endian. Keys sort as byte strings, so all pages of one
cache, and of one object version, are contiguous. A hash collision makes two
pages share one key; the read path compares the full page identity in the
record header, so a collision causes a miss, not wrong data.

### Value

Little-endian, 32 bytes:

| Field | Type | Meaning |
|---|---|---|
| slot | u32 | data slot |
| unit | u16 | first unit of the record in the slot |
| class | u8 | size class index |
| tier | u8 | 0 = cache, 1 = storage |
| length | u32 | record bytes written |
| flags | u32 | reserved, 0 |
| generation | u64 | write id, also stored in the record header |
| retain_until | u64 | Unix seconds. Storage entries are protected until then. `u64::MAX` means forever. |

### Slot summary

Little-endian, 32 bytes: kind u8 (1 node, 2 data, 3 quarantined), tier u8,
class u8, bitmap copy u8 (node slots), incarnation u32, used u32 (data slots:
record map entries of this incarnation), 4 reserved bytes, `retain_until`
bound u64 (data slots), and the seq of the commit that wrote the current
bitmap copy u64 (node slots). Free slots have no entry.

### Pages

Every page is one unit. Header, 64 bytes, little-endian:

| Offset | Field |
|---|---|
| 0 | magic `RCRVPAGE` (tree), `RCRVBMAP` (bitmap), or `RCRVRLOG` (retired log) |
| 8 | format version u16 = 2 |
| 10 | tree kind u8 (1 index, 2 record map, 3 slot table); 0 for others |
| 11 | level u8 (0 for leaves) |
| 12 | count u16 |
| 14 | reserved u16 |
| 16 | volume id, 16 bytes |
| 32 | seq u64, the commit that wrote the page |
| 40 | addr u64, the page's own address |
| 48 | checksum u64, CRC-64 of the 4096 bytes with this field zeroed |
| 56 | reserved u64 (bitmap: slot u32; log: next page address u64) |

- A leaf holds up to 72 index or slot entries, or 63 record map entries.
- An inner page holds up to 100 entries: the lowest key of the child, then
  two child addresses. Inner pages are written twice, to two addresses. Leaves
  are written once.
- A bitmap page holds 4104 bits, one per unit of its node slot.
- A retired log page holds up to 252 entries of 16 bytes: address,
  incarnation (records only), and a record flag.

A page is checked when it is read: magic, version, checksum, volume id, own
address, kind, level, count, key order, every key and value valid for its
tree, child addresses inside the volume, and zero padding. It is checked again
against what its parent expects on every use: tree, level, seq not newer than
the root set, and keys inside the parent's range.

### Superblock

4096 bytes, little-endian:

| Offset | Field |
|---|---|
| 0 | magic `RCRVOLSB` |
| 8 | format version u16 = 2 |
| 16 | volume id, 16 bytes, random at format |
| 32 | seq u64 |
| 40 | unit bytes u32 |
| 44 | slot units u32 |
| 48 | slot count u64 |
| 56 | next generation u64 |
| 64 | next incarnation u32 |
| 72 | three tree roots (index, record map, slot table), 32 bytes each: root address 0 u64, root address 1 u64, level u8, 7 reserved bytes, entry count u64 |
| 168 | retired log head address u64 |
| 176 | retired log entry count u64 |
| 184 | checksum u64, CRC-64 of the 4096 bytes with this field zeroed |

One superblock publishes one root set. An empty tree has both root addresses
set to `NONE`.

## Page cache

`pager.rs` keeps validated tree pages by address, charged at
`FRAME_BYTES` (4096 + 96) each against the budget.

- Eviction is CLOCK. A cache hit gives a leaf 1 credit and an inner page 3,
  so inner pages of all trees stay cached first.
- Pages read by scans (range scans, eviction, scrub, mount) go to a probation
  queue and are evicted before any other page, so a scan does not flush the
  hot set. A later lookup hit promotes them.
- A page whose `Rc` is held by a running operation is never evicted.
- Faults are shared: the first caller for an address reads it, later callers
  wait for its result. A leader dropped before it finishes wakes the waiters,
  and one of them retries.
- Pages written by a commit are put in the cache. Pages are removed when
  their unit is released for reuse.

Reads go through `Slab::read_at` on the caller's reactor. A read error is
retried 3 times and then returned to the caller. A page with no valid copy is
lost (see below).

## Reads and writes

- `get`, `range`, `next_after`, `remove`, `remove_if`, `remove_range`,
  `set_retain`, `evict_one`, `allocate_evicting`, `refresh_retain`, `scrub`,
  and `commit` are `Operation`s. A cached lookup completes without I/O.
- `try_get` answers from memory only: the overlays and cached pages. It
  returns `None` when a page read is needed.
- `allocate`, `publish`, and `next_generation` stay synchronous. `publish`
  only records the change; it does not read the old value. The old value's
  record is released when the change is committed, or at once if the old
  value was itself never committed.

A lookup checks the active overlay, then the frozen overlay of a running
commit, then the durable tree. It remembers the root set it started on. If a
commit published a new root set while it read pages, it starts again, so the
answer is never older than the latest state when it returns. A page that was
freed and reused while a slow reader held an old root fails the seq or range
check and the reader starts again.

Range scans work one leaf at a time and merge the leaf with both overlays.

## Commit

A commit makes all changes made so far durable. Concurrent callers share one
attempt; a caller whose changes came after an attempt started waits for it
and then runs (or joins) a follow-up. A failed or canceled attempt reports its
error to every caller it covered, and the next attempt retries the same
changes.

1. Freeze: the active overlay becomes the frozen overlay (merged into a
   frozen overlay left by a failed attempt), and a new active overlay takes
   later changes. Queued slot table changes are taken too.
2. Apply the frozen index changes to the index tree in key order. Only pages
   on changed paths are read (children of one inner page are read
   concurrently). A changed leaf, merged with an underfull neighbor when
   needed, is split evenly and written at once. For each old value the commit
   replaces or deletes, it queues a release of the old record.
3. Apply the record map changes: published records, releases from step 2,
   and the storage records retired by the previous commit. A release removes
   the entry only if it still names the same key and generation. A storage
   record is not removed; it goes to this commit's retired log. Count the
   record changes per slot and incarnation.
4. Apply the slot table changes: taken, freed, and quarantined slots, raised
   `retain_until` bounds, record counts from step 3, and the bitmap copy each
   changed node slot will get. Pages for the slot table, inner pages of all
   trees, and the retired log are reserved up front; reserving may take a new
   node slot, which changes the slot table, so this step repeats until the
   reservation covers what it needs.
5. Place new pages children first, encode them straight into run buffers,
   and write each run of up to 64 contiguous pages (256 KiB, never crossing a
   slot) with one request. Write the bitmap of every node slot that changed to
   the copy that is not durable, with the units this commit releases already
   clear.
6. Barrier 1: `fsync` the backing descriptors that completed a write since
   their last sync (`Slab::sync_dirty`), in parallel. This also makes every
   published record durable, because a record is published only after its
   write completed.
7. Barrier 2: write the superblock that does not hold the newest durable one
   and `fsync` only its descriptor (`Slab::sync_at`).
8. In memory: publish the new root set, drop the frozen overlay, release what
   the previous commit retired, and free cache records released in step 3.

Before an attempt starts it waits for metadata writes left by a canceled
attempt. Pages taken by an attempt that failed before its superblock write are
freed at once; after it, they are retired by the next commit, because that
superblock may be durable.

### Reusing space

A durable superblock references only its own root set, and the older
superblock references the one before. A page or storage record that commit N
stops referencing is still referenced by superblock N-1. Commit N lists it in
its retired log, which superblock N references. Commit N+1 releases everything
in log N: its persisted state (bitmaps, record map, slot table) already shows
it free, and the memory state frees it when N+1 is durable, at which point
superblock N-1 is gone. A failed attempt releases nothing.

`is_dirty` stays true while a retired log holds anything other than the
previous log's own pages and leftovers, so a lazy committer drains freed
space within two commits and then stops.

Cache records are reused at once: a stale cache entry fails its header check
and becomes a miss. An evicted slot is reused at once too, including storage
slots whose `retain_until` bound has passed.

### Metadata space

Metadata pages come from the open node slot, then partly used node slots, then
a free slot, taking the lowest free units so runs are contiguous. A node slot
whose pages are all released returns to the free pool. Bitmap copies live at
fixed units, so writing them needs no allocation and cannot recurse.

### Tiers

- Cache writes are committed lazily (for example every second when the
  volume is dirty, or as soon as `needs_commit` is true). A crash loses only
  cache writes after the last commit.
- Storage writes wait for a commit before they are acknowledged.

## Mount

Mount reads O(slots) metadata and no index or record map leaves.

1. Read both superblocks. Keep the valid ones with this geometry and a slot
   count not above the slab's. If none are valid, format (or fail with
   `Corrupt` when formatting is not allowed). A superblock read error fails
   the mount.
2. For the newest superblock, read the whole slot table, the retired log,
   and the bitmap copy each node slot's summary names (it must carry the seq
   the summary names). If the slot table or log has a page with no valid
   copy, try the older superblock instead (`fell_back`). If neither is
   readable, mount the newest one and quarantine the slots in its unreadable
   slot table ranges (`root_lost`).
3. A node slot whose bitmap copy is missing or does not match gets its bitmap
   rebuilt by walking the inner pages of all three trees (which name every
   leaf) and the retired log. This happens after a superblock fallback, or
   when an attempt that failed after writing its superblock was followed by a
   retry that overwrote the bitmap copy that superblock names. If the walk
   finds a lost inner page, the slot stays full.
4. Keep the retired log in memory; the first commit releases it.
5. Set the next generation to the superblock's value plus `1 << 40`, which
   skips ids handed out but not committed. The first commit persists it.

The mount report lists the seq used, invalid superblocks, fallback, lost slot
table ranges, rebuilt bitmaps, pending pages and records, and the number of
metadata pages read.

## Corruption and scrub

- An inner page with one bad copy is read from the other.
- A leaf, or an inner page with no valid copy, is lost. Lookups in its key
  range miss and the range is listed by `lost_ranges`. The next commit that
  changes a key in the range rebuilds it from the commit's changes alone.
  Records of a lost index leaf stay allocated until their slot is evicted.
- `scrub` walks the index and record map one leaf at a time with the scan
  priority (bounded memory). It reports lost ranges, removes index entries
  that do not lie in a data slot of their tier and class, quarantines slots
  covered by a lost record map range, and corrects the entry count.
- Eviction quarantines a slot instead of freeing it if any record map range
  of the slot is lost, because an index entry might still point into it.

| Damage | Effect |
|---|---|
| One superblock | The other superblock is used (one commit less). |
| Slot table or retired log page of the newest root set | The older root set is mounted (one commit less). |
| One inner page copy | The other copy is used. |
| Both copies of an inner page | Its key range is lost; for the index, the entries miss. |
| One index leaf | Up to 72 entries miss. |
| One record map leaf | Its slots are quarantined (space lost, entries still readable). |
| One bitmap copy | Rebuilt from the trees at mount. |
| One data record | Only that page. The header digest, CRC, and AEAD tag catch it. |

## Allocation and eviction

- `allocate(tier, length)` uses the open slot of the tier and class, then a
  closed partly filled slot whose bitmap was loaded, then a free slot. It
  returns `Busy` when there is no space, the storage quota or the small
  record byte cap is reached, or the overlay is full.
- `allocate_evicting` first loads the bitmap of a closed partly filled slot
  of the class from the record map (reopen), then evicts. When the storage
  quota blocks a storage allocation, only expired storage slots are evicted.
  It does not evict when the overlay is full.
- Eviction is a CLOCK over data slots. A slot can be evicted when it is not
  open, not leased, has no reservation, its reference bit is clear, and it is
  a cache slot or a storage slot whose `retain_until` bound has passed. When
  none is found, an idle open cache slot is closed and evicted.
- Eviction first reads the slot's record map leaves concurrently, then
  walks its entries `EVICT_BATCH` (1024) at a time. For each batch it
  looks up the keys together from cached pages (one descent shared by keys
  under the same inner pages), reads any missing index pages concurrently,
  removes the index entries that still point at the slot, and yields. It
  then frees the slot and returns the removed entries. The slot's record map
  entries are not deleted: the slot's next incarnation ignores them, the
  index deletions release them at the next commit, and reused units
  overwrite them. A full 4 KiB slot costs about 2 ms of CPU when the index
  is cached.
- `reserve_free` evicts ahead of allocation until a number of slots is free.
  The dataplane's maintenance keeps two slots free this way, so foreground
  allocations normally find a free slot and do not evict.
- Lowering a storage entry's `retain_until` does not scan its slot. The
  slot's bound stays loose and the slot is marked. `refresh_retain`
  recomputes marked slots; `allocate_evicting` calls it when nothing can be
  evicted. A slot that still holds retired storage records keeps its bound
  until they are released, because an older tree may reference them.

## Dataplane use

- The record format keeps its self-describing header. Its generation field is
  the value's generation.
- `RACER_DISK_METADATA_CACHE_BYTES` sets each worker's page cache budget
  (default 256 MiB).
- Async paths await index reads: the reader's lookup, the writer's
  `already_on_disk` check and allocation, hold reservation, startup cache
  retention, and hold rebuild at mount.
- Synchronous callers answer from memory (`try_get`, `try_range`) and queue
  work in `Index::maintain` when a page is not cached: enqueue's duplicate
  check (answers "not on disk"; the writer checks again), the ownership pass
  (`resident_pages_after`), held page metrics, and invalidating a bad record.
  `remove_cache` drops holds and version descriptors at once and hides the
  cache's pages from lookups; maintenance deletes the entries in batches,
  keeping pages published after the call. The lazy committer runs
  maintenance as soon as work is queued or the volume's dirty limit is
  reached, and also evicts ahead to keep two slots free.
- Holds are rebuilt at mount from the storage slots whose `retain_until`
  bound has not passed, through the record map, so the mount does not scan
  cache-tier entries.
- Restored version descriptors are checked against the volume, and
  residency of their pages restored, in maintenance batches after startup.
- Held pages are stored in the storage tier with `retain_until` set to the
  hold expiry. Storage-tier writes with no live hold use `retain_until` 0.
  Eviction compares `retain_until` with wall clock time.
- Removing a cache, or dropping a page whose record is invalid, deletes its
  entries from the index (see `cmd/racer-dataplane/src/store/disk.rs`).
- The checkpoint file keeps only object metadata.

## Not in this change

- Startup `retain_caches` still walks the whole index (in batches, with
  bounded memory) to drop pages of caches removed before a crash.
- Restoring residency reads every page entry of known versions, in the
  background; the dataplane keeps one residency entry per such page.
- With a cache smaller than the index, eviction of a cold slot reads its
  record map and index leaves (about 130 page reads for a full 4 KiB slot).

- Compaction of partly empty storage slots and of node slots.
- Repair of lost storage pages from peers.
- A scan that rebuilds lost index ranges from record headers.
- Salvage of a lost index range from the older tree.
