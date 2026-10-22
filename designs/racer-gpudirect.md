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

A client treats 400, 404, 405 and 501 on its first attempt at a mode as "mode
not supported" and remembers that for the socket path until the process exits.

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
- Calls run on the native helper thread, never on I/O threads.
- A simulation backend backs tests without GPUs. It can inject failures.
- The Cargo feature is `cuda`. Without it, GPU mode is unavailable.

At startup the dataplane lists GPUs with the CUDA driver API and NVML. It records
each GPU's UUID, PCI address, NUMA node and compute mode. GPUs in
`EXCLUSIVE_PROCESS` or `PROHIBITED` mode are skipped, because the dataplane's
context would block or be refused. `RACER_ENABLE_GPU=auto|true|false` controls
the feature. `auto` disables GPU mode when nothing usable is found. `true` makes
startup fail instead.

For each page of a `gpu-read`:

- If the plaintext is in memory, the dataplane copies it host to GPU.
- Otherwise it fetches the ciphertext and decrypts it on the GPU (below).

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

- **Memory, origin or HTTP peer:** one host-to-GPU copy.
- **Local disk:** cuFile reads it straight from NVMe (layout v2 only, below).
- **RDMA peer:** the NIC writes it into GPU memory registered with
  `ibv_reg_dmabuf_mr`. The dma-buf comes from `cuMemGetHandleForAddressRange`.

`RACER_GPU_STAGING_BYTES` caps the dataplane's ciphertext staging memory per GPU.

### Disk layout v2

v1 records put the ciphertext right after a variable-length header, so it is not
aligned. v2 pads the header so the ciphertext starts on a 4 KiB boundary inside
the record. The dataplane writes v2 and still reads v1. Only v2 records can use
cuFile. v1 records are read through host memory.

## Deployment

- The dataplane pod is privileged, so it already sees `/dev/nvidia*`.
- CUDA needs the host driver's user-space libraries. The dataplane asks the
  NVIDIA runtime to inject them without requesting `nvidia.com/gpu`:
  `NVIDIA_VISIBLE_DEVICES=all`, `NVIDIA_DRIVER_CAPABILITIES=compute,utility`,
  and the CDI annotation for `nvidia.com/gpu=all`. This relies on the CDI setup
  the unbounded agent does on NVIDIA nodes. On other nodes GPU mode stays off.
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
  window, disconnect during a write, counted leaks, v1 and v2 records.
- A CPU test that compiles the GPU AES-GCM header as C and compares it with the
  `crypto` crate.
- CI builds and loads the CUDA shim.
- Python tests run against a fake server.
- GPU hardware tests are manual or ignored, like the RDMA hardware tests.

## Order of work

1. Descriptor passing, `host-read`, the Python client and the vLLM plugin.
2. GPU discovery, the `gpu` crate, GPU decryption and `gpu-read`.
3. Disk layout v2 and cuFile reads.
4. GPUDirect RDMA from peers.
