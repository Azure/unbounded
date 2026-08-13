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
diff ID, then maps that diff ID to a byte range in a RACER segment. This lets
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
to a RACER segment, verifies it, and publishes the diff ID and chain ID records.
Container startup never waits for ingest.

Several nodes may unpack the same new layer. Kubernetes membership and
rendezvous hashing stagger their ingest attempts so one normally publishes it.
Catalog writes use optimistic compare-and-swap, so duplicate attempts are safe.
Without a membership view every node ingests eagerly, which is useful for a
single-node cluster.

## State

RACER holds the shared state: an append-only, CRC-protected catalog and the
EROFS layer blobs. The catalog also records which segment is open for new
blobs. Layer blobs are immutable after publication.

Local persistent state is under `/var/lib/gantry-snapshotter`: `metadata.db`
contains containerd snapshot metadata, `snapshots/` contains local overlayfs
upper and work directories, and `ingest/` is scratch space. Read-only EROFS
mounts and the Unix socket live under `/run/gantry-snapshotter` by default.
Device-mapper mappings and mounts are reused across container starts and are
removed by periodic cleanup when no snapshot refers to them.

The RACER devices available on a node are described by an operator-produced
device map. `segment.Watcher` polls that map, `holder` attaches or swaps the
catalog device, and `blockmap.Map` turns catalog blob addresses into local
EROFS mounts.
