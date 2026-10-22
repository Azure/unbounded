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
- No new third-party dependencies. The ublk binding is written in house on top
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
| `internal/racerblock/ublk` | Minimal ublk server: control commands on `/dev/ublk-control` and one io_uring per queue on `/dev/ublkc<N>`. |
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
  `/dev/ublk-control`.
- Data: one io_uring and one locked OS thread per queue. Each tag keeps one
  outstanding `FETCH_REQ` or `COMMIT_AND_FETCH_REQ`.
- Parameters: logical block size 512, the device size rounded up to 512, the
  read-only attribute, and a maximum I/O size (default 1 MiB).
- Defaults: `min(NumCPU, 4)` queues and a depth of 128.
- Operations: `READ` is served. `FLUSH` succeeds. `WRITE`, `DISCARD`, and
  `WRITE_ZEROES` fail with `EROFS`. Others fail with `EOPNOTSUPP`.

If racer-block exits without cleanup, the device stays in the kernel. A new run
with the same `--id` deletes the old device first.

## Backend

- Start: `Stat` the object, or check the `--etag` the user gave. The device
  size and ETag are fixed from this point on.
- Chunk size: power of two from 64 KiB to 16 MiB. Default 4 MiB. 16 MiB matches
  the Racer page size, so one chunk never spans two Racer pages when the chunk
  size divides 16 MiB.
- Cache: LRU of chunks. Default 1 GiB. Chunks being read are pinned.
- Readahead: when reads are sequential, the backend fetches the next chunks in
  the background. The window doubles on each sequential hit up to
  `--readahead-max` (default 64 MiB) and resets on a random read.
- Tail: if the object size is not a multiple of 512, the last sector is filled
  with zeros past the end of the object.
- Retries: `ErrUnavailable` and short reads are retried with backoff, up to 5
  attempts. `ErrNotFound`, `ErrVersionMismatch`, `ErrUnauthorized`, and
  `ErrForbidden` are not retried and fail the read with `EIO`.
- Warm (optional): with `--warm-ttl`, racer-block asks Racer to retain the
  pinned version and refreshes the request at about half the TTL.

## Connections

Each `Get` opens a new socket connection, sends one HTTP request, and closes
the connection when done. This is the client protocol today. racer-block sets
the SDK `MaxConnections` from the queue count and depth, so many chunk fetches
can run at once.

## Operations

```
sudo modprobe ublk_drv
sudo racer-block --cache <cache-name> --key <64 hex> [--id 0]
```

- racer-block needs root, or `CAP_SYS_ADMIN`, to use `/dev/ublk-control`.
- It needs access to `/run/racer/<cache>/client/socket` on the same host.
- It prints the device path when the device is ready. `SIGINT` or `SIGTERM`
  stops and deletes the device.
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
