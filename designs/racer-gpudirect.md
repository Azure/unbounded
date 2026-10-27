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
```

- The client sends exactly one file descriptor with the request head, as
  `SCM_RIGHTS` ancillary data on the same `sendmsg` as the first head byte.
- For `host-read` the descriptor is a regular file opened for writing, normally
  a `memfd`.
- For `gpu-read` the descriptor comes from `cuMemExportToShareableHandle` with
  `CU_MEM_HANDLE_TYPE_POSIX_FILE_DESCRIPTOR`. `Racer-Allocation-Size` is the size
  passed to `cuMemCreate`.
- `Range`, `If-Match`, `Authorization` and `Racer-Metadata` mean the same as for
  Get. `Racer-Page-Credits` and `Racer-Byte-Credits` are ignored.

The dataplane rejects the request with 400, before writing anything, when:

- no descriptor, or more than one, came with the head;
- a descriptor came with any other request (the descriptor is closed);
- the ancillary data was truncated (`MSG_CTRUNC`);
- a destination header is missing, duplicated, malformed, or
  `offset + length` overflows;
- for `host-read`, the descriptor is not a regular file;
- for `gpu-read`, `offset + length > Racer-Allocation-Size`.

After resolving the range, the dataplane rejects with 400 if the destination
length is smaller than the range length. Range and version errors keep their
Get status codes (412, 416, and so on).

`gpu-read` returns 501 when the dataplane has no usable GPU, or when the
allocation is on a GPU the dataplane did not discover.

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
   CUDA context, and grants itself read and write access.
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

A new crate, `cmd/racer-dataplane/gpu`, follows the `verbs` pattern:

- A small C shim, `libracer_gpu_cuda.so.1`, wraps the CUDA driver API, NVML and
  cuFile. Racer loads it with `dlopen` and checks `racer_gpu_abi()`.
- Calls run on the native helper thread, never on I/O threads. The one
  exception is cuFile reads, which run on four reader threads (below).
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

1. checks the CRC64 of the ciphertext;
2. computes the GCM tag;
3. only if both match, decrypts into the client's allocation.

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
- Disk records read through host memory keep the CPU CRC check of the
  existing fill path; the GPU checks the CRC again. Records read with cuFile
  are checked only on the GPU, against the CRC in the record header.
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
  until restart. A failed read only drops that file's registration.
- At startup the helper runs a zero-length decrypt on each GPU. A GPU whose
  shim has no decrypt kernels, or a staging cap smaller than the two scratch
  slots, gets plaintext pages only.
- When the GPU rejects a page (CRC or tag), the read fails without a page frame
  and the page's owner drops that copy from memory and disk, as it does after a
  failed CPU decrypt. When it accepts a page that came from a peer, the owner
  may persist it, as it does after a CPU decrypt.
- RDMA peers write `gpu-read` pages straight into GPU memory (GPUDirect RDMA
  landing), described below. Other RDMA pages land in host memory and take
  one host-to-GPU copy when a GPU read needs them.

### GPUDirect RDMA landing

When GPU decryption and RDMA are both on, each GPU can get one landing
buffer: GPU memory separate from the staging buffer, sized by
`RACER_GPU_LANDING_BYTES` (default 256 MiB per GPU, 0 turns landing off).
The GPU helper allocates it once, exports it once as a dma-buf, and keeps it
until the process exits. At each RDMA activation, every rail is matched with
at most one GPU, and every GPU with at most one rail: a GPU on the rail's
NUMA node first, then any free GPU (`assign_landing` in
`src/rdma/links.rs`). PCI distance is not used; only NUMA nodes are known on
both sides. The rail registers the landing buffer as its `rdma_verbs` device
pool, so the buffer is split into page-sized slots. If allocation, export, or
registration fails, the rail stays host-only.

A `gpu-read` page then takes this path (`Fill::land_from_peer`):

1. If this node is not a candidate for the page, has no local copy, and has
   a landing buffer on the request's GPU, it sends one CopyOnly request to the
   first provider in visit order, carrying the GPU ordinal.
2. When the hop is RDMA and the rail's landing buffer is on that GPU, the
   receiver grants a landing slot instead of a host buffer. If no slot is
   free, it grants host memory as usual.
3. The sender writes the ciphertext into the slot. After the usual fence, the
   receiver checks the signed completion and that the signed response head
   describes exactly this page (`landed_response_head`), and the requester
   verifies the signature chain against its request binding.
4. The page becomes a sealed page whose ciphertext is in the landing slot.
   The GPU checks the tag (there is no CRC: no host bytes were seen) and
   decrypts into the client's allocation.

A landed page is used once. It does not join or create a single flight, is
not published to memory, and is not persisted, because there are no host
bytes to share. The slot is held until the GPU job finishes. If the provider
answers with a host page (no free slot, or HTTP), that page is used once in
the same way, so it is not fetched twice. A miss, any failure, or a landing
buffer on another GPU falls back to the usual path. Host-only nodes and
non-GPU reads do not change. Relays never ask for landing.

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

### Disk layout v2

The store already writes format version 2 records (see
`cmd/racer-dataplane/src/store.rs`): the header and its SHA-256 digest, then zero
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
  `RACER_DATAPLANE_NVIDIA=off|env|cdi` (default `env`) controls this. `cdi`
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

## Order of work

1. Descriptor passing, `host-read`, the Python client and the vLLM plugin.
2. GPU discovery, the `gpu` crate, GPU decryption and `gpu-read`.
3. Aligned disk records and cuFile reads.
4. GPUDirect RDMA from peers (landing for `gpu-read`, described above).
