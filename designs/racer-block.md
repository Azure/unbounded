# Racer: block device (racer-block)

## Summary

racer-block is a host daemon. It shows one Racer object as a read-only Linux
block device, `/dev/ublkb<N>`. The kernel sends block reads to racer-block
through ublk. racer-block serves them from a local chunk cache and fills
misses from the Racer client socket, `/run/racer/<cache>/client/socket`.

Use it to mount a disk image or a file system image that lives in Racer,
without copying the whole object to local disk first.

## Goals

- One Racer object version maps to one block device.
- The device is read-only. Writes fail with `EROFS`.
- The device never changes under the reader. racer-block pins the object ETag
  at start. If Racer can no longer serve that version, reads fail with `EIO`
  instead of returning bytes from a newer version.
- Random reads are fast after the first touch. Sequential reads are fast from
  the start.
- No new Go dependencies. The ublk binding is written in house on top
  of `golang.org/x/sys/unix`.
- Run as a plain host process. No Kubernetes objects.

## Non-goals

- Writes, copy-on-write overlays, or write-back to the origin.
- Kubernetes integration, such as a CSI driver or a DaemonSet. This can come
  later on top of the same packages.
- Changes to the Racer client protocol or the dataplane.
- Kernels older than 6.8.

## Layout

| Path | Role |
| --- | --- |
| `cmd/racer-block` | Flags, logging, signal handling. Thin `main`. |
| `internal/racerblock` | `Run`: joins the backend and the ublk device, and owns their life cycle. |
| `internal/racerblock/ublk` | Minimal Linux ublk server over io_uring. |
| `internal/racerblock/backend` | Chunk cache, readahead, retries, and ETag pinning over `pkg/racersdk`. |

## How a read flows

1. The kernel puts a read request in the ublk queue and completes the
   queue's pending `FETCH_REQ` command.
2. The queue goroutine reads the request descriptor (sector, length) from the
   mapped descriptor buffer and calls `Backend.ReadAt` with the queue's I/O
   buffer for that tag.
3. The backend splits the range into fixed-size chunks (default 4 MiB). Each
   chunk is either in the cache, already being fetched, or missing.
4. Missing chunks are fetched with `racersdk.Client.Get` and
   `ReadOptions{Offset, Length, ETag}`. Concurrent requests for the same chunk
   share one fetch.
5. The backend copies the bytes into the I/O buffer. The queue sends
   `COMMIT_AND_FETCH_REQ` with the result and waits for the next request.

## ublk binding

The kernel ublk interface uses io_uring `URING_CMD` with 128-byte SQEs. Go has
no io_uring support in the standard library, so the binding sets up the rings
directly with `io_uring_setup`, `mmap`, and `io_uring_enter`.

- Control: `ADD_DEV`, `SET_PARAMS`, `START_DEV`, `STOP_DEV`, `DEL_DEV` on
  `/dev/ublk-control`. Shutdown uses `DEL_DEV_ASYNC` when the kernel supports it,
  and falls back to `DEL_DEV` otherwise.
- Data: one active io_uring, one reserved rescue ring, and one locked OS thread
  per queue. Each tag keeps one outstanding `FETCH_REQ` or
  `COMMIT_AND_FETCH_REQ`.
- Parameters: logical block size 512, the device size rounded up to 512, the
  read-only attribute, and a maximum I/O size (default 1 MiB).
- Defaults: `min(NumCPU, 4)` queues and a depth of 128.
- Operations: `READ` is served. `FLUSH` succeeds. `WRITE` fails with `EROFS`.
  All others, such as `DISCARD` and `WRITE_ZEROES`, fail with `EOPNOTSUPP`.
- Errors: a handler error that is a `syscall.Errno` is passed to the kernel.
  Any other error, or a handler panic, becomes `EIO`.

If racer-block exits without cleanup, the device stays in the kernel. A new run
with the same `--id` stops and deletes that device first, even if it is still
in use. Only choose an ID for a device you own. Without `--id`, the kernel
chooses a free ID.

A queue failure during startup must not release resources while `START_DEV`
is still using them. A failed queue keeps its ring open and answers `EIO`
while the device stops. For a started device, it keeps serving until
`STOP_DEV` returns. This also finishes partition-scan reads that arrive after
`START_DEV` returns.

Each queue reserves a rescue ring before startup, so recovery does not need
to allocate a new kernel ring after a failure. If the active ring cannot
serve I/O, the same queue thread uses the spare to finish requests while
keeping the old ring, buffers, and char device open. This includes requests
already handed to the old ring, which would otherwise strand `STOP_DEV` on
Linux 7.0. Startup fails if a spare ring cannot be reserved.

If both rings fail, the queue waits for startup to return or for both the
kernel state and sysfs to show that the disk is published before closing a
ring. If neither happens, it logs a warning and keeps waiting. Cleanup can
still block if neither ring can finish requests already handed to the server.

## Backend

- Start: `Stat` the object. With `--etag`, the backend also checks that Racer
  can still serve that version. With `--warm-ttl`, it uses `Warm` instead of
  `Stat`. The device size and ETag are fixed from this point on.
- Chunk size: power of two from 64 KiB to 16 MiB. Default 4 MiB. 16 MiB matches
  the Racer page size, so one chunk never spans two Racer pages when the chunk
  size divides 16 MiB.
- Fetches: one `Get` per run of adjacent missing chunks. A request that needs a
  chunk already being fetched waits for that fetch.
- Cache: LRU of chunks. Default 1 GiB. It must hold at least 4 chunks plus the
  readahead window. Chunks being read are pinned and never evicted. If the
  cache is full of pinned chunks, a demand read goes over the budget instead
  of waiting, and readahead is skipped.
- Readahead: the backend tracks up to 8 sequential streams. After 2 reads in a
  row on a stream, it fetches the next chunks in the background. The window
  starts at 2 chunks and doubles up to `--readahead-max` (default 64 MiB). A
  negative value turns readahead off.
- Tail: if the object size is not a multiple of 512, the last sector is filled
  with zeros past the end of the object.
- Retries: `ErrUnavailable`, short reads, and protocol errors are retried, up
  to 5 attempts. Backoff starts at 25 ms and doubles up to 1 s, with jitter. A
  retry starts at the first chunk not yet complete, and progress resets the
  count. `ErrNotFound`, `ErrVersionMismatch`, `ErrUnauthorized`,
  `ErrForbidden`, `ErrInvalidRequest`, and `ErrRangeNotSatisfiable` are not
  retried. A failed read returns `EIO` to the kernel.
- Warm (optional): with `--warm-ttl`, racer-block asks Racer to retain the
  pinned version and refreshes the request at about half the TTL.

## Connections

Each `Get` opens a new socket connection, sends one HTTP request, and closes
the connection when done. This is the client protocol today.

`--max-connections` sets the SDK `MaxConnections`. The default is
`min(queues * depth, 128)`. The backend shares fetches per chunk, so it needs
far fewer connections than there are block requests in flight. The cap also
stays near the dataplane limit, `RACER_CLIENT_CONNECTIONS` (64 per worker).

## Operations

```
sudo modprobe ublk_drv
sudo racer-block --cache <cache-name> --key <64 hex> [--id 0]
```

- Run as root, or with `CAP_SYS_ADMIN` and access to `/dev/ublk-control` and the
  device's `/dev/ublkc<N>` node.
- It needs access to `/run/racer/<cache>/client/socket` on the same host.
- It prints the device path when the device is ready. In normal operation,
  `SIGINT` or `SIGTERM` stops and deletes the device.
- If another process stops or deletes the device, racer-block cleans up and
  exits with an error instead of waiting for a signal.
- `--authorization-file` and `--metadata-file` set the opaque Racer
  `Authorization` and `Racer-Metadata` headers. They are only sent to the
  origin on a cache miss.

Keep ublk devices out of the dataplane block device selector, the Node
annotation `racer.unbounded-cloud.io/block-devices`. If the dataplane claims a
racer-block device as slab storage, it would read its own cache through itself.

## Testing

- `ublk`: unit tests for struct layouts and request handling. A privileged
  test starts a device on a memory buffer and reads it with `O_DIRECT`. It
  skips unless the process is root and `/dev/ublk-control` exists.
- `backend`: unit tests with a fake `Source` for chunking, cache eviction,
  shared fetches, readahead, retries, ETag pinning, and the zero-filled tail.
  `racersdktest` covers the real SDK path.
- End to end: a privileged test runs `racersdktest` as a fake Racer, starts
  racer-block on it, and compares device reads with the object bytes.

The `Racer Block Device` CI jobs run both device suites as root with the race
detector inside a QEMU guest, once per guest kernel: the Ubuntu 24.04 generic
6.8 kernel, the minimum supported, and the Ubuntu 26.04 generic 7.0 kernel.
The guest loads `ublk_drv`, so a kernel bug cannot hang the runner. Each job
fails if QEMU exits nonzero, a suite fails or hangs, a required test does not
print PASS, or any test other than the manual fio and delete-ID tests skips.
The guest sets `RACER_BLOCK_TEST_VM=1`, which enables tests that are unsafe on
a shared host. Run the same check locally without root:

```
go test -race -c -o bin/racer-block-tests/ublk.test ./internal/racerblock/ublk
go test -race -c -o bin/racer-block-tests/racerblock.test ./internal/racerblock
GUEST_KERNEL=6.8 hack/scripts/racer-block-vm-test.sh run
GUEST_KERNEL=7.0 hack/scripts/racer-block-vm-test.sh run
```

The kernel packages come from the Ubuntu archive through a private APT
configuration under `bin/racer-block-vm/<kernel>`. APT checks them against
the Ubuntu archive keyring. Host APT is not changed and nothing is installed.

The script needs `qemu-system-x86_64`, `busybox-static`, `cpio`, and `zstd`.
It uses KVM when QEMU can start it and TCG otherwise. Tests time out at 4
minutes, each suite at 270 seconds in the guest, and the VM at 600 seconds.
`hack/scripts/racer-block-vm-test.sh self-test` checks the result parsing
without booting a VM. See the script header for options.

To run the tests directly on a host, as root:

```
sudo modprobe ublk_drv
go test -c -o tmp/racerblock.test ./internal/racerblock
sudo tmp/racerblock.test -test.run EndToEnd -test.v
go test -c -o tmp/ublk.test ./internal/racerblock/ublk
sudo UBLK_TEST_FIO=1 tmp/ublk.test -test.v
```

`UBLK_TEST_FIO=1` also runs `fio` against the device when `fio` is installed.

## Follow-ups

- The SDK uses 2 page credits and 32 MiB byte credits for every read. Large
  sequential reads would benefit from a larger window. This needs an SDK
  option.
- The SDK opens a new connection for each read. Reusing connections, or
  reading many chunks in one subscription, would lower the cost of small
  random reads.
- Zero-copy: read from the socket straight into the ublk I/O buffer instead of
  the chunk cache, for reads that bypass the cache.
- A CSI node plugin or DaemonSet to run racer-block in Kubernetes.
- The backend has no stall timeout per fetch attempt. It relies on the SDK
  read timeouts.
