# memsnap

`memsnap` is a containerd snapshotter that keeps pulled image blocks in RACER
and container changes on the host filesystem.

```text
container rootfs   overlayfs
├── upper/work     /var/lib/memsnap/snapshots/<id>   (local, writable, durable)
└── lower          ext4 on read-only dm-thin volume  (RACER image blocks)
```

RACER is written only while an image is pulled and unpacked. After commit,
image volumes are mounted through read-only device-mapper mappings. Container
writes go to local overlayfs upper directories. Thin-pool metadata is also a
local loop-backed file, and `ignore_discard` prevents image cleanup from issuing
discards to RACER.

State under `-root` is retained across snapshotter restarts. The RACER device
must retain the corresponding image blocks too.

## Requirements

Linux, root, containerd 2.x, `ublk`, `dm-thin-pool`, overlayfs, `dmsetup`,
`losetup`, and `mkfs.ext4`.

```sh
sudo modprobe ublk_drv dm-thin-pool overlay
sudo apt-get install -y dmsetup e2fsprogs util-linux
make
```

Register the proxy in `/etc/containerd/config.toml`:

```toml
[proxy_plugins]
  [proxy_plugins.memsnap]
    type = "snapshot"
    address = "/run/memsnap/memsnap.sock"

[[plugins.'io.containerd.transfer.v1.local'.unpack_config]]
  platform = 'linux/amd64'
  snapshotter = 'overlayfs'

[[plugins.'io.containerd.transfer.v1.local'.unpack_config]]
  platform = 'linux/amd64'
  snapshotter = 'memsnap'
```

Start it with the RACER block device, then pull and run an image:

```sh
sudo ./bin/memsnap -device /dev/ublkbN
sudo systemctl restart containerd
sudo ctr images pull --snapshotter memsnap docker.io/library/alpine:latest
sudo ctr run --rm --snapshotter memsnap docker.io/library/alpine:latest c1 \
  sh -c 'echo local-write >/marker'
```

`./demo.sh` runs the complete flow and checks Racer's block-device counters for
post-pull writes, discards, or flushes. Demo state remains in
`/var/lib/memsnap-demo`.

## Flags

| Flag | Default | Purpose |
| --- | --- | --- |
| `-device` | required | RACER image-data block device |
| `-root` | `/var/lib/memsnap` | Durable local metadata and writable layers |
| `-address` | `/run/memsnap/memsnap.sock` | containerd proxy socket |
| `-pool` | `memsnap-pool` | Device-mapper pool name |
| `-volume-size` | 2 GiB | Virtual image filesystem size |
| `-meta-size` | 64 MiB | Local thin-metadata file size |

Limitations: the prototype uses fixed-size image filesystems, does not grow the
pool, and does not support committing a container writable layer as an image.
