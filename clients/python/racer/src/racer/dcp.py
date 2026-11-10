# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""PyTorch Distributed Checkpoint (DCP) storage on Racer writes.

RacerStorageWriter saves each rank's tensors and byte items as one Racer
object per rank, ``__<rank>_<n>.distcp``, created with ``host_write`` from a
memfd and acknowledged after ``replicas`` durable cache copies. The
coordinator writes ``.metadata`` last, only after every data object returned
``201``, so a checkpoint without ``.metadata`` is incomplete.
RacerStorageReader reads ``.metadata`` first and raises IncompleteCheckpoint
when it is missing.

Objects are keyed like racer-object S3 objects:
``object_key(bucket, f"{prefix}/{checkpoint_id}/{name}")``. Writes are
create-once, so saving to an existing checkpoint_id fails with
racer.Exists. Every ``persist_every``-th step is uploaded to the origin
(``Racer-Upload: auto``); other steps stay cache-only (``never``) until their
hold ends.

Requires torch (``pip install racer-client[dcp]``). See designs/racer-writes.md,
WP10.
"""

from __future__ import annotations

import dataclasses
import io
import os
import pickle
import uuid
from dataclasses import dataclass
from typing import Any

import torch
from torch.futures import Future
from torch.distributed.checkpoint.metadata import Metadata, MetadataIndex, StorageMeta
from torch.distributed.checkpoint.planner import (
    LoadItemType,
    LoadPlan,
    LoadPlanner,
    SavePlan,
    SavePlanner,
    WriteItem,
    WriteItemType,
)
from torch.distributed.checkpoint.storage import StorageReader, StorageWriter, WriteResult

from .client import Client
from .errors import NotFound, RacerError
from .keys import Request, object_request

METADATA_NAME = ".metadata"
DATA_SUFFIX = ".distcp"
DEFAULT_REPLICAS = 2

__all__ = [
    "IncompleteCheckpoint",
    "RacerStorageReader",
    "RacerStorageWriter",
    "checkpoint_request",
    "upload_mode",
]


class IncompleteCheckpoint(RacerError, FileNotFoundError):
    """The checkpoint has no ``.metadata``: it was never finished, or it expired."""


@dataclass(frozen=True)
class RacerStorageInfo:
    """Where one DCP item lives: an object name and a byte range in it."""

    relative_path: str
    offset: int
    length: int


@dataclass(frozen=True)
class _Prefix:
    prefix: str


def upload_mode(step: int, persist_every: int) -> str:
    """Return the Racer-Upload mode for a checkpoint at step.

    persist_every > 0 uploads steps that are a multiple of it; other steps,
    and every step when persist_every is 0, stay cache-only.
    """
    if persist_every < 0:
        raise RacerError("persist_every must not be negative")
    if persist_every > 0 and step % persist_every == 0:
        return "auto"
    return "never"


def checkpoint_request(bucket: str, prefix: str, checkpoint_id: str, name: str) -> Request:
    """Return the Racer request for one file of a checkpoint."""
    path = f"{prefix.strip('/')}/{checkpoint_id}/{name}" if prefix.strip("/") else f"{checkpoint_id}/{name}"
    return object_request(bucket, path)


def _checkpoint_id(value: Any) -> str:
    text = os.fspath(value) if isinstance(value, os.PathLike) else str(value)
    text = text.strip("/")
    if not text:
        raise RacerError("checkpoint_id must not be empty")
    return text


def _cpu_tensor(tensor: torch.Tensor) -> torch.Tensor:
    """Detach and stage a tensor in host memory, without saving unrelated storage."""
    tensor = tensor.detach()
    if tensor.device.type != "cpu":
        # No GPU write path in v1: copy to pinned host memory first.
        staged = torch.empty(tensor.shape, dtype=tensor.dtype, pin_memory=torch.cuda.is_available())
        staged.copy_(tensor)
        return staged
    if tensor.untyped_storage().nbytes() != tensor.numel() * tensor.element_size() or not tensor.is_contiguous():
        tensor = tensor.clone()
    return tensor


class RacerStorageWriter(StorageWriter):
    """A DCP StorageWriter that writes checkpoints into a Racer cache."""

    def __init__(
        self,
        client: Client,
        bucket: str,
        prefix: str,
        checkpoint_id: str | os.PathLike | None = None,
        *,
        step: int,
        persist_every: int = 0,
        replicas: int = DEFAULT_REPLICAS,
        hold_seconds: int | None = None,
        retry_busy: float = 0.0,
    ) -> None:
        super().__init__()
        self.client = client
        self.bucket = bucket
        self.prefix = prefix
        self.checkpoint_id = None if checkpoint_id is None else _checkpoint_id(checkpoint_id)
        self.step = step
        self.persist_every = persist_every
        self.upload = upload_mode(step, persist_every)
        self.replicas = replicas
        self.hold_seconds = hold_seconds
        self.retry_busy = retry_busy
        self.save_id = uuid.uuid4().hex
        self.rank: int | None = None
        self.use_collectives = True
        self.written: list[str] = []

    # StorageWriter ---------------------------------------------------------

    def reset(self, checkpoint_id: str | os.PathLike | None = None) -> None:
        if checkpoint_id:
            self.checkpoint_id = _checkpoint_id(checkpoint_id)
        self.save_id = uuid.uuid4().hex
        self.written = []

    def set_up_storage_writer(self, is_coordinator: bool, *args: Any, **kwargs: Any) -> None:
        self.rank = kwargs.get("rank")
        self.use_collectives = kwargs.get("use_collectives", True)

    def prepare_local_plan(self, plan: SavePlan) -> SavePlan:
        if self.checkpoint_id is None:
            raise RacerError("checkpoint_id is required")
        if self.rank is not None and not self.use_collectives:
            plan = dataclasses.replace(plan, storage_data=_Prefix(f"__{self.rank}_"))
        return plan

    def prepare_global_plan(self, plans: list[SavePlan]) -> list[SavePlan]:
        return [
            dataclasses.replace(plan, storage_data=_Prefix(f"__{i}_")) if plan.storage_data is None else plan
            for i, plan in enumerate(plans)
        ]

    def write_data(self, plan: SavePlan, planner: SavePlanner) -> Future[list[WriteResult]]:
        future: Future[list[WriteResult]] = Future()
        try:
            future.set_result(self._write_data(plan, planner))
        except BaseException as exc:  # noqa: BLE001 - DCP reports failures through the future
            future.set_exception(exc)
        return future

    def finish(self, metadata: Metadata, results: list[list[WriteResult]]) -> None:
        """Write ``.metadata``. DCP calls this after every rank's data writes returned."""
        storage: dict[MetadataIndex, Any] = {}
        for rank_results in results:
            for result in rank_results:
                storage[result.index] = result.storage_data
        metadata.storage_data = storage
        metadata.storage_meta = self.storage_meta()
        name = METADATA_NAME
        if not self.use_collectives and self.rank is not None:
            name = f"__{self.rank}{METADATA_NAME}"
        self._put(name, pickle.dumps(metadata))

    def storage_meta(self) -> StorageMeta | None:
        return StorageMeta(checkpoint_id=self.checkpoint_id, save_id=self.save_id)

    @classmethod
    def validate_checkpoint_id(cls, checkpoint_id: str | os.PathLike) -> bool:
        # A Racer checkpoint needs a client and bucket; never infer this writer.
        return False

    # Helpers ---------------------------------------------------------------

    def request(self, name: str) -> Request:
        if self.checkpoint_id is None:
            raise RacerError("checkpoint_id is required")
        return checkpoint_request(self.bucket, self.prefix, self.checkpoint_id, name)

    def _options(self) -> dict:
        return {
            "replicas": self.replicas,
            "upload": self.upload,
            "hold_seconds": self.hold_seconds,
            "retry_busy": self.retry_busy,
        }

    def _put(self, name: str, data: bytes) -> None:
        self.client.write(self.request(name), data, **self._options())
        self.written.append(name)

    def _write_data(self, plan: SavePlan, planner: SavePlanner) -> list[WriteResult]:
        if not plan.items:
            return []
        prefix: _Prefix = plan.storage_data
        name = f"{prefix.prefix}0{DATA_SUFFIX}"
        fd = os.memfd_create("racer-dcp", os.MFD_CLOEXEC)
        try:
            results = []
            with os.fdopen(os.dup(fd), "wb", buffering=1 << 20) as stream:
                for item in sorted(plan.items, key=lambda i: i.type != WriteItemType.BYTE_IO):
                    results.append(self._write_item(stream, item, planner, name))
                size = stream.tell()
            self.client.host_write(self.request(name), fd, 0, size, **self._options())
        finally:
            os.close(fd)
        self.written.append(name)
        return results

    @staticmethod
    def _write_item(stream: io.BufferedWriter, item: WriteItem, planner: SavePlanner, name: str) -> WriteResult:
        offset = stream.tell()
        data = planner.resolve_data(item)
        if item.type == WriteItemType.BYTE_IO:
            stream.write(data.getbuffer())
        else:
            torch.save(_cpu_tensor(data), stream)
        length = stream.tell() - offset
        return WriteResult(index=item.index, size_in_bytes=length, storage_data=RacerStorageInfo(name, offset, length))


class RacerStorageReader(StorageReader):
    """A DCP StorageReader for checkpoints written by RacerStorageWriter."""

    def __init__(
        self,
        client: Client,
        bucket: str,
        prefix: str,
        checkpoint_id: str | os.PathLike | None = None,
    ) -> None:
        super().__init__()
        self.client = client
        self.bucket = bucket
        self.prefix = prefix
        self.checkpoint_id = None if checkpoint_id is None else _checkpoint_id(checkpoint_id)
        self.storage_data: dict[MetadataIndex, RacerStorageInfo] = {}
        self.load_id = uuid.uuid4().hex

    def request(self, name: str) -> Request:
        if self.checkpoint_id is None:
            raise RacerError("checkpoint_id is required")
        return checkpoint_request(self.bucket, self.prefix, self.checkpoint_id, name)

    def reset(self, checkpoint_id: str | os.PathLike | None = None) -> None:
        self.storage_data = {}
        if checkpoint_id:
            self.checkpoint_id = _checkpoint_id(checkpoint_id)
        self.load_id = uuid.uuid4().hex

    def is_complete(self, rank: int | None = None) -> bool:
        """Report whether the checkpoint's ``.metadata`` exists."""
        try:
            self.client.stat(self.request(self._metadata_name(rank)))
        except NotFound:
            return False
        return True

    @staticmethod
    def _metadata_name(rank: int | None) -> str:
        return METADATA_NAME if rank is None else f"__{rank}{METADATA_NAME}"

    def read_metadata(self, *args: Any, **kwargs: Any) -> Metadata:
        name = self._metadata_name(kwargs.get("rank"))
        try:
            raw = self.client.read(self.request(name))
        except NotFound:
            raise IncompleteCheckpoint(f"checkpoint {self.checkpoint_id!r} has no {name}") from None
        metadata = pickle.loads(raw)  # noqa: S301 - the checkpoint is trusted like a local DCP checkpoint
        if getattr(metadata, "storage_meta", None) is None:
            metadata.storage_meta = StorageMeta()
        metadata.storage_meta.load_id = self.load_id
        return metadata

    def set_up_storage_reader(self, metadata: Metadata, is_coordinator: bool, *args: Any, **kwargs: Any) -> None:
        if metadata.storage_data is None:
            raise RacerError("checkpoint metadata has no storage data")
        self.storage_data = metadata.storage_data

    def prepare_local_plan(self, plan: LoadPlan) -> LoadPlan:
        return plan

    def prepare_global_plan(self, plans: list[LoadPlan]) -> list[LoadPlan]:
        return plans

    def read_data(self, plan: LoadPlan, planner: LoadPlanner) -> Future[None]:
        future: Future[None] = Future()
        try:
            self._read_data(plan, planner)
            future.set_result(None)
        except BaseException as exc:  # noqa: BLE001 - DCP reports failures through the future
            future.set_exception(exc)
        return future

    def _read_data(self, plan: LoadPlan, planner: LoadPlanner) -> None:
        from torch.distributed._shard._utils import narrow_tensor_by_index

        etags: dict[str, str] = {}
        for item in plan.items:
            info = self.storage_data[item.storage_index]
            request = self.request(info.relative_path)
            if info.relative_path not in etags:
                etags[info.relative_path] = self.client.stat(request).etag
            raw = self.client.read(request, info.offset, info.length, if_match=etags[info.relative_path])
            if item.type == LoadItemType.BYTE_IO:
                planner.load_bytes(item, io.BytesIO(raw))
                continue
            tensor = torch.load(io.BytesIO(raw), map_location="cpu", weights_only=True)
            tensor = narrow_tensor_by_index(tensor, item.storage_offsets, item.lengths)
            target = planner.resolve_tensor(item).detach()
            if target.size() != tensor.size():
                raise RacerError(f"item {item.storage_index} has size {tuple(tensor.size())}, want {tuple(target.size())}")
            target.copy_(tensor)
            planner.commit_tensor(item, target)

    @classmethod
    def validate_checkpoint_id(cls, checkpoint_id: str | os.PathLike) -> bool:
        return False
