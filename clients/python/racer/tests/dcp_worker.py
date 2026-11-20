# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Worker for the multi-rank DCP test (spawned, so it must be importable)."""

import warnings


def save_and_load(rank, world, store_path, socket_path, page_size):
    import torch
    import torch.distributed as dist
    import torch.distributed.checkpoint as dcp

    import racer
    import racer._wire as wire
    import racer.dcp as rdcp

    wire.PAGE_SIZE = page_size
    dist.init_process_group("gloo", init_method=f"file://{store_path}", rank=rank, world_size=world)
    try:
        client = racer.Client(socket_path=socket_path, timeout=30)
        torch.manual_seed(rank)
        sd = {f"rank{rank}": torch.randn(32, 8) + rank, "shared": torch.full((4,), 7.0)}
        with warnings.catch_warnings():
            warnings.simplefilter("ignore")
            dcp.save(sd, checkpoint_id="multi", storage_writer=rdcp.RacerStorageWriter(client, "ckpt", "runs/m", step=4, persist_every=2))
            out = {k: torch.zeros_like(v) for k, v in sd.items()}
            dcp.load(out, checkpoint_id="multi", storage_reader=rdcp.RacerStorageReader(client, "ckpt", "runs/m"))
        for k in sd:
            assert torch.equal(out[k], sd[k]), k
    finally:
        dist.destroy_process_group()
