---
title: "Save PyTorch Checkpoints to Racer"
weight: 11
description: "Write PyTorch Distributed Checkpoints to a Racer cache and upload some of them to Azure Blob."
---

{{< callout type="warning" >}}
Racer is pre-release. Its APIs, flags, and on-disk formats may change without a
migration path. See the [Racer reference]({{< relref "reference/racer" >}}).
{{< /callout >}}

This guide saves PyTorch Distributed Checkpoint (DCP) checkpoints to a Racer
cache. `dcp.save` returns once each page has 2 copies on node disks. Racer
uploads chosen checkpoints to Azure Blob in the background.

A checkpoint that is not uploaded is safe only while its copies survive. Upload
the checkpoints you cannot afford to lose.

## Before You Start

- Every Racer node runs a version that supports writes.
- A ClusterCache with `spec.writes` and an Azure origin. See
  [Write settings]({{< relref "reference/racer#write-settings" >}}).
- The cache's Azure identity has the `Storage Blob Data Contributor` role on
  the container.
- The training pods mount the cache's client socket. See
  [Client socket mount]({{< relref "reference/racer#client-socket-mount" >}}).

```yaml
apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: ClusterCache
metadata:
  name: ckpt
spec:
  origin:
    azure:
      containerURL: https://<account>.blob.core.windows.net/<container>
      credential:
        workloadIdentity:
          clientID: <client ID>
  writes: {}
```

## Install

```sh
pip install 'racer-client[dcp]'
```

The `dcp` extra needs `torch>=2.4`.

## Save and Load

```python
import racer
import torch.distributed.checkpoint as dcp
from racer.dcp import RacerStorageReader, RacerStorageWriter

client = racer.Client(cache="ckpt")

writer = RacerStorageWriter(client, "ckpt", "runs/a", step=step, persist_every=10)
dcp.save(state_dict, checkpoint_id=f"step-{step}", storage_writer=writer)

reader = RacerStorageReader(client, "ckpt", "runs/a")
dcp.load(state_dict, checkpoint_id=f"step-{step}", storage_reader=reader)
```

How it works:

- Each rank writes one data object with `host_write` and `replicas=2`.
- The coordinator rank writes `.metadata` last, after every data object is
  stored. A checkpoint without `.metadata` is incomplete, and
  `RacerStorageReader` raises `IncompleteCheckpoint`.
- Steps that are a multiple of `persist_every` go to the origin. Data objects
  use upload mode `auto`. Uploads finish in any order, so `.metadata` uses
  `manual`: the coordinator waits until every data object's upload is
  `committed`, then persists `.metadata`. A data object that is already
  served from the origin (a non-Racer `ETag` with no upload state) counts as
  committed. The origin never has `.metadata`
  without its data. If an upload fails or does not finish within
  `upload_timeout`, the save raises `CheckpointUploadError` and `.metadata`
  stays only in the cache.
- Other steps use `never` and stay only in the cache until their hold ends.
  `persist_every=0` keeps every step cache-only.
- `RacerStorageReader.is_complete` checks the cache, not the origin. It is true
  once `.metadata` is in the cache.
- Writes are create-once. Saving to an existing `checkpoint_id` raises
  `racer.Exists`.

`RacerStorageWriter` also takes `replicas`, `hold_seconds` (default:
the cache's `writes.holdTTL`), `retry_busy` (seconds to retry when nodes
are at their dirty limit), `upload_poll_interval` (seconds between upload state
checks, default 1), and `upload_timeout` (seconds to wait for uploads, default
3600, `None` for no limit).

## Write Single Objects

The Python client writes single objects too:

```python
request = racer.object_request("ckpt", "runs/a/step-100/weights.bin")
result = client.write(request, data, replicas=2, upload="manual", hold_seconds=3600)
state = client.persist(request)  # start the upload
client.delete(request)           # or drop it from the cache
```

`host_write(request, fd, offset, length)` writes from a memfd or regular file
without copying through the socket. Pass `durability_origin=True` to wait
until the upload commits. See the
[Go SDK writes]({{< relref "reference/racer#go-writes" >}}) for Go.

## See Also

- [Object writes]({{< relref "reference/racer#object-writes" >}}) for headers,
  status codes, and upload states.
- [Racer concepts]({{< relref "concepts/racer#how-a-write-works" >}}).
