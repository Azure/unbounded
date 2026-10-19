# Racer extent volume

This document describes the on-disk allocator and index that replaced the
log-structured segments, the in-memory `PageIndex`, and the page part of the
checkpoint file in `cmd/racer-dataplane`. The old segment table and
`PageIndex` were removed from `page-alloc`. See
[racer-page-alloc.md](racer-page-alloc.md) for the buffer and slab layers
under the volume.

## Goals

- Support 8 TB and larger disks. Index memory stays under 2 GB for 8 TB.
- Two durability tiers:
  - Cache: best effort. Allocation always makes progress by evicting cache
    data.
  - Storage: crash safe. A page is acknowledged only after it is durable. One
    bad bit in the index must not lose more than a small, bounded set of pages.
- Fast, with low overhead. Each worker owns its volume; there is no shared
  mutable state on the hot path.
- Allocate both 16 MiB pages and small pages (4 KiB to 64 KiB, and anything in
  between).
- No write-ahead log. The index is a copy-on-write B+tree. A commit writes new
  tree nodes and then switches one of two superblocks.

## Ownership

Each I/O worker owns one volume, backed by its own `Slab` (a file or a list of
device ranges). There are no fixed partitions: the number of volumes equals
the number of I/O workers. The volume is single threaded and uses `Rc` and
`Cell`, like the rest of the store. Routing of pages to workers does not
change.

## Geometry

- `UNIT_BYTES = 4096`. Every on-disk object starts on a unit boundary. The
  device's direct I/O alignment must divide 4096, or the volume is
  `Unsupported`.
- `SLOT_UNITS = 4104`, so `SLOT_BYTES = 16 MiB + 32 KiB`. A largest page record
  is a header (at most 16 KiB), 16 MiB of ciphertext, and a 16 byte tag. It fits
  in one slot.
- The slab's segment size is `SLOT_BYTES`. Slot `i` covers bytes
  `[i * SLOT_BYTES, (i + 1) * SLOT_BYTES)` of the slab address space.
- `slot_count = capacity / SLOT_BYTES`. At least 4 slots. At most `1 << 26`
  slots (about 1 PiB).
- Slot 0 is the meta slot. Unit 0 holds superblock A and unit 1 holds
  superblock B. Units 2 and above hold tree nodes.
- A node address (`NodeAddr`) is `slot << 16 | unit`. `NodeAddr::NONE` is
  `u64::MAX`.

## Slots and size classes

A slot is free, a node slot, or a data slot. A data slot has one size class and
one tier. A size class is a unit count. The classes are:

```
1 2 3 4 6 8 12 16 24 32 48 64 96 128 192 256 384 512 768 1024 1536 2048 4104
```

Class index 22 (4104 units) is a whole slot and is used for full pages. A slot
of class `c` holds `SLOT_UNITS / c` records. A record uses the smallest class
that fits it. Rounding up wastes at most a third of a record and about a sixth
on average.

Node slots use 1 unit records. Node slots are taken from the free pool like
data slots.

Each worker keeps at most one open (partly filled) slot per tier and class.

### In-memory slot table

Per slot: kind, class, tier, used record count, lease count, generation, a
CLOCK reference bit, a free bitmap (only for classes with more than one record
per slot), and the list of index keys stored in the slot. The keys let eviction
remove a whole slot from the index without scanning the tree. Cost is a 24 byte
entry plus a 4 byte lease count per slot, plus 26 bytes per record in
multi-record slots, or 24 bytes for a whole-slot record (see
`alloc/src/volume/slots.rs`).

### Leases

A lease pins one slot. Eviction skips leased slots. A read takes a lease at
lookup and drops it after it validates the record. A write takes a lease at
allocation and drops it after insert or abort.

## Index

The index maps a 24 byte key to a 32 byte value. The whole tree is kept in
memory. Disk is used only to survive restarts. Reads never go to disk for the
index.

### Key

```
cache_hash:   8 bytes  first 8 bytes of SHA-256("racer.volume.cache.v1\0" || cache)
version_hash: 12 bytes first 12 bytes of SHA-256("racer.volume.version.v1\0" ||
                       len(cache) || cache || len(key) || key || len(etag) || etag)
page:         4 bytes  big-endian page number
```

Lengths are 4 byte big-endian. Keys sort as byte strings. All pages of one
cache are contiguous, and all pages of one object version are contiguous.
Pages above `u32::MAX` are not stored. The hashing lives in the dataplane; the
allocator only sees `[u8; 24]`.

A hash collision makes two pages share one key. The read path compares the full
page identity in the record header, so a collision causes a miss, not wrong
data.

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

The generation is unique per write within a volume. The read path checks that
the record header has the same generation. This rejects stale or reused space.

### Nodes

Every node is one unit (4096 bytes). Header, 64 bytes, little-endian:

| Offset | Field |
|---|---|
| 0 | magic `RCRVNODE` |
| 8 | format version u16 = 1 |
| 10 | kind u8 (0 = leaf, 1 = inner) |
| 11 | level u8 (0 for leaves) |
| 12 | count u16 |
| 14 | reserved u16 |
| 16 | volume id, 16 bytes |
| 32 | seq u64, the commit that wrote the node |
| 40 | addr u64, the node's own address |
| 48 | checksum u64, CRC-64 of the 4096 bytes with this field zeroed |
| 56 | reserved u64 |

- A leaf holds up to 72 entries of 56 bytes (key then value).
- An inner node holds up to 100 entries of 40 bytes: the lowest key of the
  child, then two child addresses. The first key of an inner node is a lower
  bound only.
- Inner nodes are written twice, to two different addresses (ditto copies).
  Leaves are written once and their second address is `NONE`.

A node is valid when the magic, version, checksum, volume id, and own address
match, its seq is not newer than the superblock, its level is one less than its
parent, and its keys are sorted and inside the range given by the parent.

### Superblock

4096 bytes, little-endian:

| Offset | Field |
|---|---|
| 0 | magic `RCRVOLSB` |
| 8 | format version u16 = 1 |
| 16 | volume id, 16 bytes, random at format |
| 32 | seq u64 |
| 40 | unit bytes u32 |
| 44 | slot units u32 |
| 48 | slot count u64 |
| 56 | root address 0 u64 |
| 64 | root address 1 u64 |
| 72 | root level u8 |
| 80 | entry count u64 |
| 88 | next generation u64 |
| 96 | checksum u64, CRC-64 of the 4096 bytes with this field zeroed |

An empty tree has both root addresses set to `NONE`.

## Commit

A commit makes all index changes made so far durable.

1. Collect every dirty node, bottom up. Give each one or two new addresses.
   Encode it with the new seq.
2. Write the nodes and wait.
3. `fsync` every backing descriptor. This also makes all data writes durable,
   because an entry enters the tree only after its data write completed.
4. Write the superblock to the slot that does not hold the newest durable
   superblock, and wait.
5. `fsync` again.

If any step fails, the nodes of that attempt become dirty again, and the next
attempt writes them to new addresses. The newest durable superblock is never
overwritten.

### Reusing space

Space that a durable tree may still reference must not be reused too early.
Each commit attempt increments an epoch. A freed address is tagged with the
epoch at the time it was freed. When a commit succeeds, the volume releases
every address tagged at or before the previous successful commit. Both
superblocks then only reference live space.

This rule covers tree nodes and storage data. Cache data is reused at once,
because a stale cache entry fails its generation check and becomes a miss.

### Tiers

- Cache inserts are committed lazily, by default every second when the tree is
  dirty. A crash loses only cache inserts made after the last commit.
- Storage inserts wait for a commit before they are acknowledged. Concurrent
  writers share one commit.

## Mount

1. Read both superblocks. Keep the valid ones with a compatible geometry. The
   slot count may grow but not shrink. If none are valid, the caller formats a
   new volume. Old cache data is discarded.
2. Load the tree of the newest superblock. Each inner node is read from its
   first copy, then its second. A leaf or inner node with no valid copy loses
   its key range. If the root cannot be read, use the other superblock.
3. For each lost key range, copy the entries in that range from the other
   superblock's tree, if it is valid there.
4. Rebuild the slot table from the tree. Mark node and data space as used. If
   two entries overlap, keep nodes over data, storage over cache, and then the
   first one. Drop the rest.
5. Mark nodes of the other superblock's tree that are not in the mounted tree
   as pending, so they are released by the second successful commit.
6. Set the next generation to the larger of the superblock value and the
   largest entry generation plus one, then add `1 << 40`. This skips any ids
   used by writes that were not committed.

The mount report lists the seq used, lost ranges, salvaged entries, and dropped
entries.

## Allocation and eviction

- `allocate(tier, length)` picks the class, uses the open slot for that tier and
  class, or takes a free slot. The unit is reserved, and the slot is leased.
- Storage slots are limited by a quota. Small records (classes below the whole
  slot) are limited by a byte cap so the tree stays within its memory budget.
  Both limits return `Busy`.
- If there is no free slot, the caller evicts and retries. Eviction is a CLOCK
  over data slots. A slot can be evicted when it is not open, not leased, its
  reference bit is clear, and it is a cache slot or every entry in it has an
  expired `retain_until`. Eviction removes all of the slot's entries from the
  tree and frees the slot at once.
- Deleting a storage entry frees its unit using the reuse rule above. A storage
  slot returns to the free pool when its last record is released.

## Failure blast radius

| Damage | Effect |
|---|---|
| One superblock | The other superblock is used. |
| One inner node copy | The other copy is used. |
| Both copies of an inner node | Its range is salvaged from the older tree. Lost entries miss and are refilled or repaired from peers. |
| One leaf | Up to 72 entries. Salvaged from the older tree when possible. |
| One data record | Only that page. The header digest, CRC, and AEAD tag catch it. |

## Dataplane use

- The record format keeps its self-describing header. Its generation field is
  the value's generation.
- Held pages are stored in the storage tier with `retain_until` set to the hold
  expiry. Holds are rebuilt from the tree at mount. Storage-tier writes with no
  live hold use `retain_until` 0.
- `release_hold` only gives back unused hold capacity after a partial failure.
  Pages already written stay held until the hold expires. Nothing resets
  `retain_until` to 0. Removing a cache, or dropping a page whose record is
  invalid, deletes its entries from the tree instead (see
  `cmd/racer-dataplane/src/store/disk.rs`).
- Reading a held storage-tier page treats failures by kind. A transient
  failure (I/O or OS error, unavailable, overloaded, canceled) keeps the entry
  and the read misses. A definitely bad record (damaged header or record,
  another format version, metadata that contradicts the known version, or an
  AEAD or CRC failure in fill) is removed like a cache-tier entry. The hold,
  keyed by version, stays; the page returns to its unused capacity, so the
  next fill or warm writes the page again to the storage tier with the hold's
  `retain_until`. The removal is made durable by the next volume commit, and
  the old record's space follows the normal reuse rule (see
  `StoreReader::invalidate` in `cmd/racer-dataplane/src/store.rs`).
- The checkpoint file keeps only object metadata. It no longer stores segments,
  pages, or holds.
- An in-memory table maps version hashes to object identities for features
  that list resident pages. It is filled by writes and reads.

## Not in this change

- Compaction of partly empty storage slots.
- Repair of lost storage pages from peers.
- A scan that rebuilds lost index ranges from record headers.
