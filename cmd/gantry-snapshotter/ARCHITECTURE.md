# Architecture

`gantry-snapshotter` is a Linux-only containerd proxy snapshotter. It stores
uncompressed image layers once per cluster as read-only EROFS filesystems in
RACER. A node can then map an existing layer instead of downloading and
unpacking it again. Container writable layers remain ordinary local overlayfs
directories.

The command lives in `cmd/gantry-snapshotter`. Most of the implementation lives
in `internal/gantry/snapshotter`.

## Layer paths

containerd identifies a layer stack by its chain ID and the content of one
uncompressed layer by its diff ID. The cluster catalog maps a chain ID to a
diff ID, then maps that diff ID to a byte range in the image volume. This lets
different images share the same layer data even when the layer appears at a
different depth.

When `Prepare` finds the chain ID in the catalog, the snapshotter records a
committed snapshot immediately and returns `AlreadyExists`. This is the signal
containerd already uses for a layer that has been unpacked, so containerd skips
the registry download and apply. When the layer is mounted, the snapshotter
creates a read-only device-mapper mapping for its RACER byte range, mounts the
EROFS image, and uses that mount as an overlayfs lower directory.

When the catalog misses, the snapshotter behaves like containerd's normal
overlayfs snapshotter. It creates a local active snapshot and containerd
downloads and applies the layer. `Commit` completes locally first, then submits
the layer to a background ingest queue. Ingest reads the compressed blob from
containerd's content store, builds an EROFS image with `mkfs.erofs`, writes it
to a free segment, verifies it, and publishes the diff ID and chain ID records.
Container startup never waits for ingest.

Several nodes may unpack the same new layer. Kubernetes membership and
rendezvous hashing stagger their ingest attempts so one normally publishes it.
Catalog writes use optimistic compare-and-swap, so duplicate attempts are safe.
Without a membership view every node ingests eagerly, which is useful for a
single-node cluster.

## The image volume

The operator provisions one RACER volume for the whole cluster. Its composition
is an OCC catalog extent followed by a run of `IMMUTABLE_4M` extents, and all of
it is exported on every node as a single block device. A segment is one of those
immutable extents, addressed here by its offset into the device.

Capacity is fixed when the volume is created. A RACER device's extent list is
frozen for the device's life, so nothing can be appended later without
republishing the device and stranding every mapping already built over it.
Reclamation, not growth, is what keeps the volume usable.

## State

RACER holds the shared state: an append-only, CRC-protected catalog and the
EROFS layer blobs. The catalog also records which segment is open for new blobs,
how full each segment is, and how far each node has read the record log. Layer
blobs are immutable after publication.

Local persistent state is under `/var/lib/gantry-snapshotter`: `metadata.db`
contains containerd snapshot metadata, `snapshots/` contains local overlayfs
upper and work directories, and `ingest/` is scratch space. Read-only EROFS
mounts and the Unix socket live under `/run/gantry-snapshotter` by default.
Device-mapper mappings and mounts are reused across container starts and are
removed by periodic cleanup when no snapshot refers to them.

The image device available on a node is described by an operator-produced device
map. `segment.Watcher` polls that map, `holder` attaches or swaps the catalog,
and `blockmap.Map` turns catalog blob addresses into local EROFS mounts.

## Reclamation

Blobs are appended to one open segment until it will not fit the next one, at
which point the segment is sealed and another is opened. Deleting a layer only
marks its bytes dead; RACER cannot free a page on its own, because an immutable
page that has been trimmed still holds its slot until the whole extent is
collected. So space comes back one segment at a time, and only through the
cleaner.

A cycle runs `Sealed -> Cleaning -> Draining -> Empty`, one step per pass, on
whichever node rendezvous hashing elects for that segment:

- **Cleaning.** The layers still live in the victim are copied into the open
  segment and re-published as blob records at a higher generation. Readers take
  the highest generation for a diff ID, so a node that has not caught up keeps
  resolving the old location, which is still valid.
- **Draining.** Once every node's watermark has passed the generation those
  records were written at, no node can still resolve a layer into the victim.
  The cleaner then discards the victim's whole byte range. A discarded
  `IMMUTABLE_4M` page reads back as zeroes rather than an error, which is
  precisely why the wait is not optional.
- **Empty.** The trimmed pages are tombstones until the control plane advances
  the extent's tombstone epoch, which it does once every node reports the extent
  holds no live pages. The new epoch arrives in the device map, and the segment
  goes back to being empty and reusable.

The watermark table lives in the catalog rather than in Kubernetes, so the gate
needs no API access and no new failure mode: a node that cannot publish its
watermark holds reclamation up, which is the safe direction.

For that to be true a node has to be in the table before it can read anything
out of the catalog, so a node claims its slot as part of attaching the catalog,
at generation zero, which no cleaner can be past. An attach that cannot claim
fails, and the daemon falls back to unpacking layers locally rather than
mounting pages nothing has promised to wait for. The claim is refreshed on a
fifth of the grace period, independently of the much slower sweep that raises
the generation, so a slow sweep reads as a slow node rather than a departed one.
A node's identity in that table is its `-node-name`, which is why the flag is
required and has to be stable across restarts.
