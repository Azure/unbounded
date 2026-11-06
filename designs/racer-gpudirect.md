# Racer GPUDirect reads

## Goal

Let a client on a node read a Racer object straight into its own GPU memory, with
as few copies as possible. vLLM should be able to load model weights from a
ClusterCache through a loader plugin, with no other special code.

Out of scope for now:

- AMD GPUs. The design is NVIDIA-only.
- A capability endpoint such as `GET /v1/gpus`. Clients find out what works by
  trying it (see [Fallback](#fallback)).

## Delivery modes

| Mode | Endpoint | Who moves the bytes |
|---|---|---|
| GPU | `POST /v1/objects/<key>/gpu-read` | The dataplane writes the plaintext into a client GPU allocation. |
| Host | `POST /v1/objects/<key>/host-read` | The dataplane writes the plaintext into a client memfd. The client copies it to the GPU. |
| Stream | `POST /v1/objects/<key>` | Today's framed stream over the socket. |

Both new endpoints are RPCs. The client passes the destination with the request
and the dataplane forgets it when the request ends. Nothing is registered for
longer than one request.

## Wire protocol

### Request

```
POST /v1/objects/<64 hex key>/gpu-read HTTP/1.1     (or /host-read)
Host: racer
Content-Length: 0
Range: bytes=<start>-<end>              optional, as for Get
If-Match: "<etag>"                      optional, as for Get
Racer-Destination-Offset: <u64>         required
Racer-Destination-Length: <u64>         required, > 0
Racer-Allocation-Size: <u64>            required for gpu-read only
Racer-Gpu-Uuid: GPU-<uuid>              optional, gpu-read only
```

- The client sends exactly one file descriptor with the request head, as
  `SCM_RIGHTS` ancillary data on the same `sendmsg` as the first head byte.
- For `host-read` the descriptor is a regular file opened for writing, normally
  a `memfd`.
- For `gpu-read` the descriptor comes from `cuMemExportToShareableHandle` with
  `CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR`. `Racer-Allocation-Size` is the size
  passed to `cuMemCreate`.
- `Racer-Gpu-Uuid` is the UUID of the GPU that holds the allocation, from
  `cuDeviceGetUuid`, in the `GPU-xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx` form
  `nvidia-smi` prints. The `GPU-` prefix is optional and hex is case
  insensitive. Device numbers differ between the client's container and the
  dataplane (a pod with one GPU sees it as device 0), but UUIDs do not, so the
  dataplane uses the UUID to pick its own context for that GPU. Without it
  (older clients), the dataplane tries each usable GPU in turn.
- `Range`, `If-Match`, `Authorization` and `Racer-Metadata` mean the same as for
  Get. `Racer-Page-Credits` and `Racer-Byte-Credits` are ignored.

The dataplane rejects the request with 400, before writing anything, when:

- no descriptor, or more than one, came with the head;
- a descriptor came with any other request (the descriptor is closed);
- the ancillary data was truncated (`MSG_CTRUNC`);
- a destination header is missing, duplicated, malformed, or
  `offset + length` overflows;
- for `host-read`, the descriptor is not a regular file;
- for `gpu-read`, `offset + length > Racer-Allocation-Size`;
- for `gpu-read`, `Racer-Gpu-Uuid` is malformed or duplicated.

After resolving the range, the dataplane rejects with 400 if the destination
length is smaller than the range length. Range and version errors keep their
Get status codes (412, 416, and so on).

`gpu-read` returns 501 when the dataplane has no usable GPU, when
`Racer-Gpu-Uuid` names a GPU the dataplane cannot use (it logs
`stage=gpu state=import-skipped reason=unknown-gpu-uuid uuid=<uuid>`), or when
the allocation is on a GPU the dataplane did not discover. If the named GPU
does not own the allocation, the dataplane tries the others and logs
`stage=gpu state=uuid-mismatch` when another one does.

### Response

On success the status is 200 with the same headers as a Get, plus
`Racer-Delivery: gpu` or `Racer-Delivery: host`, and `Connection: close`.

The body uses the Get frame format: 21 bytes, big-endian,
`kind:u8 | number:u64 | offset:u64 | length:u32`. The difference is that page
frames carry no payload:

- A kind 1 frame means page `number` is fully written. `offset` is the object
  offset of the first written byte and `length` is the number of bytes. The
  bytes for object offset `o` are at destination offset
  `Racer-Destination-Offset + (o - range start)`.
- The dataplane sends a page frame only after the write is complete and visible
  to the client. For `gpu-read` that means the CUDA stream that wrote the page
  has finished.
- Page frames arrive in page order. The last frame is kind 2, as for Get.
- `Content-Length` is `21 * (pages + 1)`.
- The client sends no credits.

If the read fails after the head, the dataplane closes the connection without
the kind 2 frame, as for Get. The destination may then hold partial data.

### Descriptor lifetime

1. The dataplane receives the descriptor with `recvmsg` and `MSG_CMSG_CLOEXEC`.
2. For `gpu-read` it imports the handle, maps the whole allocation into its own
   CUDA context, and grants itself read and write access. Before mapping, the
   shim checks that the allocation lives on the context's GPU. It resolves the
   device the driver reports for the handle in the dataplane's own numbering
   and compares GPU UUIDs, not ordinals.
3. It writes pages.
4. When the read ends for any reason, including a client disconnect, it waits
   for all GPU work that touches the allocation, then unmaps it, releases the
   handle, and closes the descriptor.
5. Only after step 4 does it send the kind 2 frame.

If a CUDA free or unmap call fails, the dataplane keeps the resource and counts
the leak, as the `verbs` crate does. It never frees memory that a GPU may still
write.

`host-read` writes with io_uring `Write` at an explicit file offset. It never
maps the client's file, so a client that truncates its memfd cannot crash the
dataplane.

### Fallback

Clients try modes in order: GPU, then host, then stream.

- An older dataplane does not know the new paths and answers 400 or 404. The
  kernel discards the descriptor because the old dataplane reads with plain
  `recv`.
- A new dataplane without a usable GPU answers `gpu-read` with 501.

A client remembers an unsupported mode for the socket path, logs one warning
with the status, and tries the mode again after a retry interval (300 seconds
by default in the Python client) or when the socket file is replaced by a
restarted dataplane:

- 501 marks the mode unsupported at once.
- 400, 404 and 405 can also mean a bad request, so the client falls back for
  that read but marks the mode unsupported only when a later mode then
  succeeds.

A `host-read` refused because the destination is too small falls back to stream
for that read only.

### Security

The socket is already the trust boundary (`docs/content/concepts/racer.md`). The
new endpoints do not change who may read what. A descriptor only lets the
dataplane write into memory the client already owns. The dataplane never reads
from client memory and never writes outside
`[offset, offset + length)`.

## Dataplane changes

### Receiving descriptors

The reactor gains an io_uring `RecvMsg` operation with a control buffer. The
client listener reads request heads with it and keeps any received descriptors
as `OwnedFd`s attached to the parsed request. Descriptors that no request
claims are closed.

### Host mode

`host-read` reuses the Get read path (`RangeStream`). For each page it writes the
plaintext slice from the `VerifiedPage` into the client's file with io_uring
`Write`, then sends the page frame.

### GPU mode

A new crate, `cmd/racer/gpu`, follows the `verbs` pattern:

- A small C shim, `libracer_gpu_cuda.so.1`, wraps the CUDA driver API, NVML and
  cuFile. Racer loads it with `dlopen` and checks `racer_gpu_abi()`.
- Calls run on the GPU coordinator thread and the per-GPU executor threads,
  never on I/O threads; cuFile reads run on four reader threads (below). The
  `racer-gpu` handles are `Send` and `Sync` and the shim is thread safe, so
  these threads share them (see "Executor model").
- A simulation backend backs tests without GPUs. It can inject failures.
- The Cargo feature is `cuda`. Without it, GPU mode is unavailable.

At startup the dataplane lists GPUs with the CUDA driver API and NVML. It records
each GPU's UUID, PCI address, NUMA node and compute mode. GPUs in
`EXCLUSIVE_PROCESS` or `PROHIBITED` mode are skipped, because the dataplane's
context would block or be refused. `RACER_ENABLE_GPU=auto|true|false` controls
the feature. `auto` disables GPU mode when nothing usable is found. `true` makes
startup fail instead.

For each page of a `gpu-read`:

- If another reader still holds the page's plaintext, the dataplane copies it
  host to GPU.
- Otherwise it takes the ciphertext (the memory cache keeps only ciphertext)
  and decrypts it on the GPU (below).

### GPU decryption

Pages use XAES-256-GCM. The CPU does the XAES key derivation for each page and
passes the derived AES-256 key, the 12-byte GCM nonce and the AAD to the GPU.
The kernel:

1. computes the GCM tag over the ciphertext and AAD;
2. only if it matches, decrypts into the client's allocation.

The GCM tag is the only integrity check on the GPU. The GPU path never
computed anything the tag does not already cover, so the CRC-64 that the
kernels used to compute alongside the tag was removed (shim ABI 3). A
mismatch is reported as `Verdict::TagMismatch`.

**Immutable source.** The kernels read the ciphertext twice: the partials
read it for GHASH, and after the verdict the CTR kernel reads it again to
decrypt. If the bytes changed between the two reads, the client could get
plaintext that was never authenticated. Every source (a staging slot, a
landing slot, or the scratch slot a host or peer copy fills) is therefore
held unchanged from the moment the decrypt is queued until its stream or a
fence after it completes: staged and landed slots are owned by the sealed
page until then, scratch slots belong to the running job, and the Rust
wrappers hold every buffer and host source until completion. The simulation's
deferred mode checks this (see Testing).

The AES and GHASH code lives in a header that compiles as both C and CUDA, so
the CPU tests can check it against the `crypto` crate. Keys live only in the
dataplane's GPU memory and are wiped after use.

Ciphertext reaches GPU staging memory owned by the dataplane in one of three ways:

- **Memory, origin or HTTP peer:** one host-to-GPU copy. A page cached in
  memory is staged from there even if it is also on disk.
- **Local disk:** cuFile reads it straight from NVMe (aligned records only, below).
- **RDMA peer:** the NIC writes it into a GPU landing buffer registered with
  `ibv_reg_dmabuf_mr`. The dma-buf comes from `cuMemGetHandleForAddressRange`.

`RACER_GPU_STAGING_BYTES` caps the dataplane's ciphertext staging memory per GPU.

How the implementation works today, where it adds to or differs from the plan
above:

- The read path asks for sealed pages only for `gpu-read`
  (`RangeStream::set_sealed`, `Fill::acquire_sealed`). Pages whose plaintext
  another reader still holds, origin fills (the CPU encrypts them, so it has the
  plaintext), and pages another reader already decrypted are still delivered
  as plaintext and copied host to GPU. So are pages that a peer selection fans
  out to the read window, and pages whose key cannot be derived or whose AAD is
  longer than the kernel accepts (4 KiB).
- The XAES key derivation runs on the page's owning I/O thread
  (`PageCrypto::derive_page_key`), not on the crypto worker. It is three AES
  blocks.
- Disk records read through host memory keep the CPU checks of the existing
  fill path; the GPU checks the tag again. Records read with cuFile are
  checked only on the GPU, by the tag.
- Each GPU has one staging buffer of 16 MiB + 4 KiB slots, allocated on first
  use. Two slots are scratch for the helper's current job: host ciphertext,
  and the plaintext of a page the range covers only in part. The GPU decrypts
  the whole page there (the tag covers all of it), the helper checks the
  verdict, then copies only the requested bytes into the client's window. A
  whole page decrypts straight into the window. The scratch plaintext is not
  wiped; it is dataplane-owned GPU memory that is never read back to the host.
- The other slots hold cuFile reads. The page's owning worker reads the record
  header through host memory (`StoreReader::direct_read`), holds the segment
  lease, and asks the GPU helper to read the ciphertext into a free slot. The
  helper registers the file with cuFile once and hands the read to one of four
  reader threads, so several reads run at once while the helper decrypts. In a
  test on one NVMe drive, one thread reached about 5 GiB/s and two or more
  reached the drive's limit of about 6 GiB/s. When no slot is free, cuFile
  fails, or the record is v1, unaligned, or on a simulated descriptor, the
  worker reads the page through host memory instead.
- The helper opens the cuFile driver on another thread at startup. Opening it
  probes every GPU and can take most of a second, which would otherwise delay
  the first disk read. A cuFile driver or registration failure turns cuFile off
  until restart. When registration fails because cuFile does not support the
  file, its filesystem, or its device (`CU_FILE_IO_NOT_SUPPORTED`,
  `CU_FILE_DEVICE_NOT_SUPPORTED`, `CU_FILE_INVALID_FILE_TYPE`), only that
  device (`st_dev`) skips cuFile until restart. A failed read only drops that
  file's registration.
- `RACER_GPU_PCI_P2PDMA=true` makes the shim set cuFile's `use_pci_p2pdma`
  (`cuFileSetParameterBool`) before it opens the driver, so local NVMe reads
  use PCI P2PDMA without `nvidia-fs`. If cuFile cannot set it, the open fails
  and cuFile is off, as for any failed open. The flag is an argument of
  `racer_gpu_file_open`. Shim ABI 3 removes the CRC from
  `racer_gpu_decrypt` (72 bytes, a reserved word at offset 44) and from the
  verdict (8 bytes), and adds events, pinned host memory and the RDMA flush
  (see "Executor model").
- At startup the coordinator runs a zero-length decrypt on each GPU. A GPU whose
  shim has no decrypt kernels, or a staging cap smaller than its scratch
  slots, gets plaintext pages only.
- When the GPU rejects a page (tag mismatch), the read fails without a page frame
  and the page's owner drops that copy from memory and disk, as it does after a
  failed CPU decrypt. When it accepts a page that came from a peer, the owner
  may persist it, as it does after a CPU decrypt.
- RDMA peers write `gpu-read` pages straight into GPU memory (GPUDirect RDMA
  landing), described below. Other RDMA pages land in host memory and take
  one host-to-GPU copy when a GPU read needs them.

### GPUDirect RDMA landing

When GPU decryption and RDMA are both on, each decrypting GPU gets one
landing buffer: GPU memory separate from the staging buffer, sized by
`RACER_GPU_LANDING_BYTES` (default 256 MiB per GPU, not per rail; 0 turns
landing off). At each RDMA activation the GPU helper allocates it and exports
it once as a dma-buf. `plan_landing` in `src/rdma/links.rs` splits it into
disjoint slices, one per rail, so every GPU can land over every rail and a
node with more GPUs than rails leaves no GPU out. If the buffer has fewer
page slots than there are rails, rails on the GPU's NUMA node come first,
then the rest in rail order. PCI distance is not used; only NUMA nodes are
known on both sides. Each slice is registered as its own `rdma_verbs` device
pool on its rail, keyed by GPU ordinal, so a rail can hold one pool per GPU.
If allocation, export, or registration fails, that rail and GPU stay
host-only.

Lifetime: the buffer is freed when nothing refers to it. NIC registrations
and landed pages hold references, so it is freed after the RDMA node closes
and drains (shutdown, or before a new activation round), or right away when
no rail registered it. A registration `rdma_verbs` leaks on purpose (failed
deregistration) keeps its reference, so that memory is never freed under the
NIC. The next activation allocates and registers again. Buffers are not
resized at runtime: `RACER_GPU_LANDING_BYTES` comes from the ConfigMap, and
the operator restarts the pods when it changes.

A `gpu-read` page then takes this path (`Fill::land_from_peer`):

1. If this node is not a candidate for the page, has no local copy, and has
   a landing buffer on the request's GPU, it sends a CopyOnly request,
   carrying the GPU ordinal, to the first provider in visit order. On a miss
   or failure it asks the next one, up to two providers
   (`LANDING_PROVIDERS`), then takes the usual path.
2. When the hop is RDMA and the rail has a landing slice for that GPU, the
   receiver grants a landing slot instead of a host buffer. If no slot is
   free, it grants host memory as usual.
3. The sender writes the ciphertext into the slot. After the usual fence, the
   receiver checks the signed completion and that the signed response head
   describes exactly this page (`landed_response_head`), and the requester
   verifies the signature chain against its request binding.
4. The page becomes a sealed page whose ciphertext is in the landing slot.
   Before the GPU reads the slot, the helper makes the NIC's writes visible:
   `racer_gpu_flush_rdma` reads
   `CU_DEVICE_ATTRIBUTE_GPU_DIRECT_RDMA_WRITES_ORDERING` once per GPU and
   calls `cuFlushGPUDirectRDMAWrites` only when the GPU does not order remote
   writes for the needed scope: the owner for a decrypt in place, all
   devices when another GPU peer-copies the slot. A GPU that can neither
   order nor flush refuses landed pages. Then the GPU checks the tag and
   decrypts into the client's allocation.

A landed page does not join or create the host single flight, is not
published to memory, and is not persisted, because there are no host bytes.
Concurrent sealed fetches of the same page share the landed copy on the GPU
instead (see "Sharing ciphertext between GPUs" below). The slot is held while
any request still holds the copy. If the provider answers with a host page
(no free slot, or HTTP), that page is used privately by this request, so it
is not fetched twice. If every provider asked misses or fails, or the rail
has no slice for that GPU, the read takes the usual path. Host-only nodes and
non-GPU reads do not change. Relays never ask for landing.

Not caching landed pages is a design choice, not a gap. The host memory and
disk tiers are the cache. Putting a landed page there would need a
device-to-host copy of the ciphertext, which is the cost landing exists to
avoid. Keeping landed pages in GPU memory after the last request releases
them would hold scarce slots that peers need for new pages.

`racer_rdma_pages_landed_total` counts pages that landed, and the GPU
helper's `landed` counter counts landed pages it decrypted. A landed page the
GPU rejects fails the read like any other sealed page; there is no host copy
to drop.

The GPU memory used for landing is not charged to the host admission
budget. It is bounded by `RACER_GPU_LANDING_BYTES` per GPU.

Not covered yet: there is no end-to-end `gpu-read` test across two whole
dataplanes, because the test harness has no two-node fixture with simulated
RDMA and a simulated GPU on the client socket. Tests cover each layer
instead: verbs device pools, landing grants over two simulated nodes, the
peer transport over a simulated link, the fill path with a scripted
provider, and GPU decryption from a landing buffer.

### Sharing ciphertext between GPUs

A node with several GPUs often loads the same model weights on all of them,
so many `gpu-read` requests ask for the same pages at about the same time.
Without sharing, each GPU fetched each page again from disk or a peer. Now
GPU-resident ciphertext from a cuFile read or an RDMA landing is shared
between requests (`src/read/gpu_copies.rs`). Only ciphertext is shared. Each
GPU still checks the tag and decrypts by itself.

**Lookup.** `Fill::acquire_sealed` tries, in order:

1. plaintext another reader holds (host-to-GPU copy, as before);
2. ciphertext in the memory cache (sealed from host memory, as before);
3. a shared GPU copy: a live copy on the same GPU, then a live copy on a GPU
   this GPU can copy from; else join a fetch in flight on a reachable GPU;
   else lead a new fetch that others may join, even while a fetch is in
   flight on a GPU this one cannot reach;
4. the usual path through host memory.

A request's own fetch tries cuFile from local disk, then a peer landing.

**Peer access.** At startup the helper calls `cuDeviceCanAccessPeer` and
`cuCtxEnablePeerAccess` for every ordered pair of usable GPUs and logs the
result for each pair. `RACER_GPU_PEER=auto` (default) does this;
`RACER_GPU_PEER=off` never enables peer access. `GpuHelper::peers(dst, src)`
reports whether a pair has a working peer path.

**Copy paths.** When a request decrypts shared ciphertext (`queue_sealed`):

- Same GPU: decrypt in place.
- Other GPU with a peer path: `cuMemcpyPeerAsync` into the ciphertext slot of
  the page's scratch pair, then decrypt.
- Other GPU without a peer path: copy through host memory (host bounce).

If a peer copy fails, that direction of the pair is turned off until restart
and the page takes the host bounce. Only the failed direction is turned off.
After any failed page, the executor synchronizes the page's stream (and the
source GPU's utility stream). If the source cannot be fenced, its slot is
never reused (`state=ciphertext-quarantined`). If the page's stream cannot be
fenced, queued work may still write its scratch pair, so that GPU is tainted
(`state=scratch-tainted`): the executor sets the stream and pair aside, even
past the request's release, and refuses later sealed pages on that GPU with
`Unavailable` before touching scratch. Each refused page retries the fence;
once every set-aside stream synchronizes, the taint clears
(`state=scratch-fenced`) and the pairs come back.
Lookups only share across GPUs that have a peer path. A request whose GPU
cannot reach any fetch in flight leads a separate fetch, which requests on
its own GPU, or on GPUs that can reach it, join.

**Retention.** The table holds weak references. A copy is reusable only while
some request still holds it; the table never pins GPU memory or landing
slots. Dead entries are pruned on lookup and insert. The table is per owning
worker and capped at 4096 pages (`MAX_PAGES`). Past the cap, a new fetch runs
privately. Each page has at most 8 fetches in flight (`MAX_FLIGHTS`), one
per group of GPUs that cannot reach each other's; past that, a request with
no reachable fetch to join fetches privately. An abandoned fetch only sends
its own waiters to look again.

**Cancellation.** A waiter that is canceled stops waiting and returns its
error. If the leader is canceled or times out, waiters are told the fetch
was abandoned and look again, up to two times (`GPU_COPY_JOIN_RETRIES`),
then fetch on their own. If the leader's fetch ends with no GPU copy, waiters
take the usual path through host memory rather than repeat the leader's
cuFile read or landing.

**Verdicts.** Every verdict goes to the page's owner (`Fill::sealed_verdict`),
except a passed verdict for a landed page, which leaves the owner nothing to
do and settles where it was reported. A failed verdict drops the
host and disk copies as before and forgets every shared GPU copy of the page
(`GpuCopies::forget`), so later requests fetch it again. It also detaches the
page's in-flight fetches, which may have read the same rejected record: later
requests do not join them and their results are not shared, though requests
already waiting on them still receive and check them.

**Metrics.** `racer_gpu_direct_joined_total` counts sealed pages served from
another request's GPU copy. The GPU helper also counts joined pages, peer
copies and bytes, peer copy failures, and host bounces.

**Limitations and future work.**

- Ciphertext from host memory (memory cache, HTTP, host RDMA) is still copied
  to each GPU separately. Uploading it once and fanning out to other GPUs is
  future work.
- Each GPU runs at most four pages at once (one per stream). Whether more
  streams, or more than one page per request in flight, pay off needs
  measurement on hardware.

### Executor model

- **Threads.** A coordinator thread (`racer-gpu`) loads the backend, finds
  GPUs, imports client allocations, runs cuFile registration and hands reads
  to the cuFile reader threads, and allocates landing buffers. Each usable
  GPU gets an executor thread (`racer-gpu-N`) that runs the page jobs of
  requests imported on it. Every GPU thread calls `Gpu::enter_thread`, so
  tests run them all on the simulated backend.
- **Streams in flight.** An executor keeps up to four pages in flight
  (`STREAMS` in `src/gpu.rs`), each on its own stream. A stream has at most
  one decrypt outstanding, as the shim requires. A request has at most one
  page running, so its pages land in order.
- **Events and fences.** `Stream::fence_in` records an event from the GPU's
  `EventPool` (no `cuEventCreate` per fence) and moves the stream's holds
  (buffers, host sources) into a `Fence`, which releases them when
  `Fence::poll` (`cuEventQuery`, never blocks) sees the work complete. The
  executor completes pages by polling fences and never synchronizes a stream
  on the success path. The verdict is read once the decrypt's fence
  completes. A failed page synchronizes its stream; if that fails, the
  stream and its scratch pair are set aside and the GPU refuses sealed
  pages until they synchronize.
- **Waiting.** With nothing in flight the executor sleeps on its queue's
  condition variable. With fences pending it naps between polls, from 20 us
  doubling to 1 ms, and a new page wakes it at once.
- **Bounded queue.** Each executor accepts at most `queue_depth` page jobs,
  queued or running (128 by default). More are refused with `Overloaded`
  and counted in `racer_gpu_executor_queue_full_total`.
- **Scratch pairs.** The first slots of each staging buffer are scratch
  pairs (ciphertext, partial plaintext), one per stream when staging holds
  at least 32 slots per pair, at least one. A page takes a pair only when it
  copies host or other-GPU ciphertext in or decrypts part of a page; a page
  that needs a pair while none is free waits, and later pages may pass it.
  A partial-range decrypt stays two-phase: the copy out of scratch is queued
  only after its verdict passed.
- **Pinned host staging.** Each stream has one reused page-locked slot
  (`cuMemHostAlloc`, portable). Host ciphertext and plaintext are copied into
  it and DMAed to the GPU; a missing slot falls back to the pageable copy.
  `Context::register_host` (`cuMemHostRegister`) exists for long-lived pool
  slabs; nothing registers slabs yet.
- **Cross-GPU copies.** A peer copy is queued on the page's stream with no
  host wait; a failed queue call turns the pair off and the page bounces. A
  bounce copies device to the stream's pinned slot on the source GPU's
  utility stream, records a fence from the source GPU's pool, and the page's
  stream waits on it (`Stream::wait_for`, `cuStreamWaitEvent`) before
  copying in. The page keeps that fence until it is answered.
- **Shutdown.** When the helper is dropped, the coordinator closes every
  executor queue; each executor answers its queued and running pages, then
  releases its imports, streams and events before the coordinator joins the
  cuFile readers.
- **Landing registry.** GPU landing state (configuration, per-round slices and
  slot counts, exhaustion counts) lives in `gpu_landing::GpuLandingRegistry`.
  The RDMA link node holds a handle and drives it; GPU telemetry reads it.

### Disk layout v2

The store already writes format version 2 records (see
`cmd/racer/src/store.rs`): the header and its SHA-256 digest, then zero
padding up to `header_len`, then the ciphertext, then zero padding up to the
extent. `header_len` and the ciphertext body are rounded up to the record unit.
Readers treat other versions, including the old unpadded v1 layout, as cold
misses.

cuFile needs the ciphertext to start on a 4 KiB boundary on the device, so the
writer uses a record unit of at least 4 KiB: the slab's direct-I/O extent unit,
raised to 4 KiB when the extent unit is smaller. Readers still accept any
`header_len` that is a multiple of 512 bytes, so records written with a smaller
unit stay readable and are read through host memory. With 4 KiB record units,
every record in a segment starts 4 KiB aligned relative to the segment, and the
ciphertext is 4 KiB aligned on the device when the segment's physical start is
too. The dataplane does not assume this. It computes
the absolute offset for each read and falls back to host memory if the offset is
not aligned.

## Deployment

- The dataplane pod is privileged, so it already sees `/dev/nvidia*`.
- CUDA needs the host driver's user-space libraries. The dataplane asks the
  NVIDIA runtime to inject them without requesting `nvidia.com/gpu`:
  `NVIDIA_VISIBLE_DEVICES=all` and
  `NVIDIA_DRIVER_CAPABILITIES=compute,utility`. The operator knob
  `RACER_NVIDIA=off|env|cdi` (default `env`) controls this. `cdi`
  also adds the CDI annotation for `nvidia.com/gpu=all`. It is opt-in because
  containerd fails pod creation when a CDI device cannot be resolved, which
  would break the DaemonSet on nodes without NVIDIA GPUs. On other nodes the
  environment variables are ignored and GPU mode stays off.
- The image gains a `RACER_NATIVE_CUDA` build argument that builds the shim and
  the `cuda` feature.
- The injector gains a label that mounts the client socket directory into app
  containers, so a Python client can use it.

## Clients

### Python client

`clients/python/racer` is pure Python. It speaks the socket protocol, including
`SCM_RIGHTS`. torch is needed only for GPU helpers. Through ctypes on `libcuda`
it allocates exportable GPU memory with `cuMemCreate` and the POSIX descriptor
handle type, maps it, and wraps it as a torch tensor.

### vLLM plugin

`racer-vllm` registers `--load-format racer` through the
`vllm.general_plugins` entry point and `register_model_loader`. For each
safetensors file it reads the header, reads each tensor's bytes into a reusable
GPU staging buffer, and passes tensor views to `model.load_weights`. Files are
found through `model.safetensors.index.json`. Object keys follow racer-object's
rule (`internal/racerobject/racerobject.go`).

## Testing

- Rust simulation tests: descriptor handling, writes that would leave the
  window, disconnect during a write, counted leaks, aligned and unaligned records.
- A CPU test that compiles the GPU AES-GCM header as C and compares it with the
  `crypto` crate.
- CI builds and loads the CUDA shim.
- Python tests run against a fake server.
- GPU hardware tests are manual or ignored, like the RDMA hardware tests.
- The GPU simulation defers queued work by default, as a real GPU does: it
  runs at stream sync, at an event query or sync (a query answers "not ready"
  once first, so polling paths run), or when a waiting stream runs. It
  records a violation, and fails the work, when a queued source changes
  before the work runs (writes by earlier work on the stream are allowed) or
  when a buffer or pinned allocation is freed while queued work uses it.
  Unchecked violations panic when the simulation drops. `set_immediate`
  restores run-when-queued behavior.
- The kernel emulation harness (`run-kernel-emulation.sh`) runs the CUDA
  kernels on the CPU with one thread per CUDA thread and compares them with
  the serial core.

## Order of work

1. Descriptor passing, `host-read`, the Python client and the vLLM plugin.
2. GPU discovery, the `gpu` crate, GPU decryption and `gpu-read`.
3. Aligned disk records and cuFile reads.
4. GPUDirect RDMA from peers (landing for `gpu-read`, described above).
