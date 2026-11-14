# racer-client

Python client for Racer, the node-local object cache in Unbounded. It talks
to the Racer dataplane over its Unix socket. It has no required dependencies.

```python
import racer

client = racer.Client(cache="models")  # /run/racer/models/client/socket
request = racer.object_request("my-bucket", "llama/config.json")
data = client.read(request)
meta = client.stat(request)
```

The socket path is, in order: `socket_path=...`, the `RACER_SOCKET`
environment variable, then `/run/racer/<cache>/client/socket`.

## Reading into host memory

```python
with racer.HostBuffer(meta.size) as buf:
    client.read_into_host(request, buf)
```

`HostBuffer` is a `memfd`. The client passes it to the dataplane with
`SCM_RIGHTS` (`host-read`), and the dataplane writes straight into it. Older
dataplanes do not know `host-read`; the client then uses the normal stream.

## Reading into GPU memory

```python
from racer import cuda, tensors

alloc = cuda.GpuAllocation(meta.size, device=0)
client.read_into_gpu(request, alloc)
t = tensors.as_tensor(alloc)  # uint8 CUDA tensor, no copy
```

Modes are tried in order: `gpu-read`, then `host-read` into a pinned staging
buffer plus a copy to the GPU, then the stream plus a copy. When a dataplane
says a mode is not supported, the client remembers that for the socket path
and logs one warning (logger `racer.client`) with the status that caused the
fallback. It tries the mode again after `mode_retry_seconds` (default 300;
`None` never retries), or at once when the socket file is replaced, which
happens when the dataplane restarts. `racer.forget_modes()` clears the memory.

`gpu-read` sends the GPU's UUID (`cuda.device_uuid`) so the dataplane finds
the right GPU even when device numbers in this container differ from the
host's. A custom destination can provide it with a `gpu_uuid()` method.

`GpuAllocation` uses the CUDA driver API (`libcuda.so.1`, through ctypes).
Set `RACER_LIBCUDA` to load a different library. torch is needed only for
`racer.tensors`. A tensor from `as_tensor` keeps the allocation alive.

## Capabilities

```python
caps = client.capabilities()
caps["delivery"]  # for example ["gpu", "host", "stream"]
```

`capabilities` asks the dataplane (`GET /v1/capabilities`) which delivery
modes, GPUs, page size, and page ciphers it supports, so a caller can choose a
mode instead of trying one. It returns the decoded JSON object and uses the
client timeout. A dataplane without the endpoint raises `NotFound`.

## Prefetching

```python
spec = racer.PrefetchSpec(refs=[racer.PrefetchRef(request.hex, metadata=request.metadata)])
status = client.prefetch("epoch-17", spec)
status = client.prefetch_status("epoch-17", wait=30, refs="failed")
```

`prefetch` submits a ticket of object ranges for Racer to load in the
background. Pages are filled on their owners in the cache cluster and evicted
like any other page; nothing is pinned. Tickets are local to the node and kept
in memory, so after a dataplane restart resubmit the same id and only missing
pages are loaded. Resubmitting the same spec is idempotent.

`prefetch_status` reports progress, optionally waiting for the ticket to
settle (`wait`), probing resident bytes (`coverage=True`), and paging through
per-ref outcomes (`refs="failed"` or `"all"`, then `cursor=status.next_cursor`).
`cancel_prefetch` releases a ticket and `list_prefetches` lists this cache's
tickets. `PrefetchWindow` replaces a ticket's desired set as a reader moves
forward: `update(refs)` advances the sequence and `reset(refs)` the
generation. `split_prefetch` divides a spec that exceeds Racer's per-ticket
limits into tickets `<id>.0`, `<id>.1`, and so on. Rejections raise
`PrefetchError` with `status`, `code` (for example `stale_sequence`),
`message`, and `retry_after`.

## Object keys

`object_key(bucket, key, version_id)` gives the same 64-hex key as
racer-object (`internal/racerobject/racerobject.go`).

## Tests

```
make racer-python-test
```
