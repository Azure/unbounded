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

## Object keys

`object_key(bucket, key, version_id)` gives the same 64-hex key as
racer-object (`internal/racerobject/racerobject.go`).

## Tests

```
make racer-python-test
```
