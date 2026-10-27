# Racer dataplane ublk devices

Status: implemented (draft PR). See "Implementation" for what was built.

## Summary

racer can expose a cached object as a read-only Linux block device. It uses
[ublk](https://docs.kernel.org/block/ublk.html). Each device maps to one cache key. The
dataplane's I/O workers serve the device queues from their own io_uring rings. Reads use the
same page cache, peer, and origin path as client reads.

This replaces the separate `racer-block` daemon from PR #985. That daemon read through the
client Unix socket, kept its own chunk cache, and copied data twice. Here there is no socket
hop and no second cache. Bytes go from the cached page to the kernel request with one kernel
copy.

## Goals

- One cache key (one object) maps to one read-only `/dev/ublkbN`.
- The device is pinned to one ETag. If that version can no longer be read, reads fail with
  EIO. They never return newer data.
- Serve each ublk queue on a dataplane I/O worker thread. No extra threads on the read path.
- Reuse the dataplane read path: memory, peers, disk spill, and origin.
- No new Rust dependencies.

## Non-goals

- Writes, discard, or copy-on-write overlays. Writes fail with EROFS.
- Kubernetes or CSI integration. Devices are set by dataplane config for now.
- Kernels older than 6.8.
- Recovery after a dataplane crash (`UBLK_F_USER_RECOVERY`).

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `RACER_UBLK_DEVICES` | unset | JSON array of devices. Unset or `[]` turns ublk off. |
| `RACER_UBLK_QUEUE_DEPTH` | 128 | Tags per queue, 1 to 4096. |
| `RACER_UBLK_MAX_IO_BYTES` | 1048576 | Largest request, a multiple of 4096, at most 32 MiB. |

Each device is an object:

```json
[{"cache": "models", "key": "<64 lowercase hex>", "etag": "\"v1\"", "id": 7}]
```

- `cache` is the cache name from the control plane snapshot.
- `key` is the cache key.
- `etag` is optional. When unset, the dataplane resolves the current version once and pins it.
- `id` is optional. When unset, the kernel picks the device id. When set and a stale device
  with that id exists, the dataplane stops and deletes it first.

Device nodes are opened under `RACER_DEVICE_DIRECTORY` (default `/host/dev`).

Keep ublk devices out of the `racer.unbounded-cloud.io/block-devices` Node annotation. If the
dataplane used one of its own ublk devices for disk spill, it would read its cache through
itself.

## Design

### Queues and threads

- `nr_hw_queues` equals the number of I/O workers. Worker `q` serves queue `q` of every
  device. The kernel requires all commands for a queue to come from one task. Workers are
  pinned to one thread each, so this holds.
- Queue commands (`FETCH_REQ`, `COMMIT_AND_FETCH_REQ`) carry a 16-byte payload. They fit
  a normal 64-byte SQE, so they run on the worker's existing ring through a new
  `Reactor::uring_cmd` operation in `uring-runtime`.
- Control commands need 128-byte SQEs. A small `ublk-control` thread owns
  `/dev/ublk-control` and its own SQE128 ring. It never serves reads.

### Device flags and parameters

- Flags: `UBLK_F_CMD_IOCTL_ENCODE | UBLK_F_USER_COPY`.
- Basic parameters: read only, logical block 512, physical block 4096, `io_opt` and
  `io_min` 4096, `max_sectors = max_io_bytes / 512`.
- Size is the object length rounded up to 512. Bytes past the object end read as zero.

### Startup

1. A worker resolves metadata for the cache key, with the pinned ETag if one is set. This
   gives the version and length. It waits until the cache appears in the snapshot.
2. The control thread sends `ADD_DEV` and `SET_PARAMS`.
3. The control thread opens `/dev/ublkcN` once. The kernel allows only one open of the
   character device, so each worker takes a `dup` of that descriptor, maps its own queue's
   descriptor array, and sends `FETCH_REQ` for every tag. The control thread drops its own
   copy once every worker has attached.
4. The control thread sends `START_DEV`. The kernel waits until every tag has a fetch
   outstanding, then adds the disk. Workers are already serving, so the partition scan reads
   complete.

A device that fails to start is logged and torn down. It does not stop the dataplane.

### Reads

For each `FETCH_REQ` completion the worker reads the tag's descriptor:

| Op | Result |
|---|---|
| READ | bytes read, or `-EIO` |
| FLUSH | 0 |
| WRITE, WRITE_ZEROES, DISCARD | `-EROFS` |
| other | `-EOPNOTSUPP` |

A READ maps the byte range to cache pages (16 MiB each) of the pinned version. The worker
gets each page with `WorkerDirectory::acquire`. Remote owners are reached through the
existing mailboxes. With `UBLK_F_USER_COPY` the worker writes the page bytes into the
request by writing to `/dev/ublkcN` at `ublk_pos(queue, tag, offset)`. This uses the same
`write_shared_at` call that `host-read` uses, so the bytes go straight from the shared page.
The worker then sends `COMMIT_AND_FETCH_REQ` with the result.

The first touch of a page fetches the whole 16 MiB page, which acts as readahead. A
sequential reader also starts the next page early.

### Shutdown

1. On stop, the control thread sends `STOP_DEV` for each started device. The kernel waits
   for in-flight requests, which workers keep serving while they drain. Until the control
   thread exits, or the drain deadline passes, each worker keeps its read mailbox open and
   does not stop its flights, so these reads can still reach the cache, peers, and origin.
2. The kernel then completes every outstanding fetch with `UBLK_IO_RES_ABORT` (`-ENODEV`).
   When all tags of a queue are aborted, the worker closes its `/dev/ublkcN` descriptor.
3. When every worker has closed its descriptor, the control thread sends `DEL_DEV`.

Every wait is bounded by `RACER_SHUTDOWN_TIMEOUT_MS`. If queues are still attached after
that, the control thread tells workers to abandon them. A worker also abandons its queues
when its own drain deadline passes or when the control thread has exited. Abandoning drops
the tag futures. The reactor sends `ASYNC_CANCEL` for each outstanding command and keeps the
descriptor open until the kernel completes it, so no kernel reference outlives its owner.

If the process dies, closing the rings and descriptors aborts all requests. The device stays
until something deletes it. A fixed `id` lets the next start replace it.

## Implementation

Code lives in `cmd/racer/src/ublk/`.

- `service.rs`: the per-tag loop (`serve_tag`) and `QueueService`, which runs every tag of
  one queue plus readahead. The kernel side (`QueueKernel`) and the pages (`PageSource`) are
  traits, so tests drive the loop without a device.
- `worker.rs`: `UblkWorker`, one per I/O worker. It resolves device versions (worker 0 only),
  attaches queues, and polls them. `ReactorKernel` implements `QueueKernel` with
  `Reactor::uring_cmd` and `write_shared_at` on the worker's ring.
- `directory.rs`: `DirectoryBackend` and `DirectorySource`. They resolve metadata with
  `WorkerDirectory::resolve_with_budget` and read pages with `WorkerDirectory::acquire`,
  pinned to the resolved version. A version lookup retries every second while the cache is
  missing from the snapshot or the read path is unavailable.
- `manager.rs`: `Manager`, the node-level device table shared by workers and the
  `ublk-control` thread, and the control loop. `DeviceControl` abstracts the control
  commands for tests.
- `app.rs`: `Application::start_ublk` creates the manager and thread only when devices are
  configured. `reserve_ublk_submissions` adds `2 * devices * queue_depth` reactor queue
  entries and the matching request-context bytes to each worker, for the long-lived queue
  commands plus one write per tag. `WorkerApplication` polls `UblkWorker` from
  `poll_services`, signals stop from `stop_admission`, and does not finish `drain` until its
  queues are closed.

Workers find out about new devices on their next poll. The control thread wakes them through
the task waker each worker registers with the manager.

Readahead is simple. When a read ends in the last quarter of a page, the queue starts
fetching the next page, at most two pages at a time per queue.

## Differences from racer-block

- No chunk cache, readahead tracker, or retry loop. The dataplane page cache, flights, and
  origin retries do this work.
- No rescue ring, startup hold, or pinned delete. racer-block needed them because a failed
  queue ring could strand requests while the process lived on. Here a failed ring means a
  failed worker, and the dataplane exits. Process exit releases the character device, which
  aborts stranded requests.
- No authorization or opaque metadata files yet. Devices use an empty origin context.

## Testing

- Unit tests for the UAPI layouts, config parsing, request decoding, and page range math.
- Simulation tests for the new `uring_cmd` reactor operation.
- Unit tests for the tag loop against an in-memory kernel: page crossing, zero tail, short
  writes, error mapping, aborts, command failures, and readahead.
- Unit tests for the control loop against a fake kernel: normal lifecycle, fixed id
  replacement, resolve, attach, start, and serving failures, and shutdown timeouts.
- Tests that ublk off does nothing and that queue headroom is reserved.
- Unit tests for `DirectoryBackend` and `DirectorySource` over a real `WorkerDirectory` and
  read worker with a scripted origin: missing cache, fresh and pinned lookups, page bytes,
  and version and not-found errors.
- Tests that a stopping worker keeps its read mailbox open until the control thread exits.
- Three root-only tests, run with `RACER_UBLK_TEST=1` as root. One drives the raw kernel
  interface. One runs the real `Manager`, control thread, and two worker threads with real
  reactors serving through `UblkWorker` and a stub page source. It reads `/dev/ublkbN` with
  `pread` at several offsets (including across a 16 MiB page and the zero tail), then stops
  and checks that the device is gone. The third serves one queue through `DirectorySource`,
  `WorkerDirectory`, and a scripted origin, and compares the bytes read.

## Follow-ups not done

- An end-to-end test through a full multi-worker dataplane with peers. The root tests cover
  one worker's read path with a scripted origin, not `WorkerApplication` startup or peers.
- A device that fails is not retried. It stays down until the dataplane restarts.
- No metrics for ublk requests yet. Failures are logged.
- No per-request deadline from the kernel. Each page read uses `RACER_REQUEST_TIMEOUT_MS`.

## Open questions

- Should devices come from the control plane (for example a field on the cache) instead of
  dataplane config?
- Should authorization and origin metadata be supported per device?
- Should we use `UBLK_F_AUTO_BUF_REG` zero copy on newer kernels? It needs registered
  buffers in `uring-runtime`.
