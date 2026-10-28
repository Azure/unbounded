# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""vLLM plugin that loads model weights from a Racer cache.

Adds the load format "racer" (vLLM v0.25.x) and the racer-vllm-fetch
command. See README.md for the options.
"""

from __future__ import annotations

import collections
import json
import logging
import math
import os
import string
import struct
import time
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass

import racer

# Under "vllm." so messages use vLLM's log handler and format.
log = logging.getLogger("vllm.racer_vllm.loader")

LOAD_FORMAT = "racer"
SCHEME = "racer://"
MARKER = "racer-model.json"
INDEX = "model.safetensors.index.json"
SINGLE = "model.safetensors"
DEFAULT_STAGING_BYTES = 256 * 1024 * 1024
DEFAULT_CONCURRENCY = 4
MAX_HEADER_BYTES = 100 * 1024 * 1024
MAX_PARTS = 100_000
TP_ONLY_PATTERN = "model-rank-{rank}-part-{part}.safetensors"
PP_AND_TP_PATTERN = "model-pp-{pp_rank}-tp-{tp_rank}-part-{part}.safetensors"

# Small files vLLM and transformers read to build the model and tokenizer.
CONFIG_FILES = (
    "config.json",
    "generation_config.json",
    "tokenizer.json",
    "tokenizer_config.json",
    "special_tokens_map.json",
    "tokenizer.model",
    "vocab.json",
    "vocab.txt",
    "merges.txt",
    "added_tokens.json",
    "chat_template.jinja",
    "chat_template.json",
    "preprocessor_config.json",
    "processor_config.json",
    "params.json",
    "tekken.json",
    INDEX,
)
SMALL_SUFFIXES = (".json", ".txt", ".model", ".jinja", ".py", ".tiktoken")

# safetensors dtype: (bytes per element, torch dtype name).
DTYPES = {
    "BOOL": (1, "bool"),
    "U8": (1, "uint8"),
    "I8": (1, "int8"),
    "F8_E4M3": (1, "float8_e4m3fn"),
    "F8_E5M2": (1, "float8_e5m2"),
    "F8_E8M0": (1, "float8_e8m0fnu"),
    "I16": (2, "int16"),
    "U16": (2, "uint16"),
    "F16": (2, "float16"),
    "BF16": (2, "bfloat16"),
    "I32": (4, "int32"),
    "U32": (4, "uint32"),
    "F32": (4, "float32"),
    "I64": (8, "int64"),
    "U64": (8, "uint64"),
    "F64": (8, "float64"),
}

_registered = False


class ModelChangedError(RuntimeError):
    """A model file no longer has the version the load started with."""


@dataclass(frozen=True)
class ModelURI:
    """A parsed racer://<cache>/<bucket>/<prefix>."""

    cache: str
    bucket: str
    prefix: str  # no leading or trailing slash; may be empty

    def key(self, name: str) -> str:
        return f"{self.prefix}/{name}" if self.prefix else name

    def request(self, name: str) -> racer.Request:
        return racer.object_request(self.bucket, self.key(name))

    def __str__(self) -> str:
        return f"{SCHEME}{self.cache}/{self.bucket}/{self.prefix}".rstrip("/")


def parse_uri(uri: str) -> ModelURI:
    """Parse racer://<cache>/<bucket>/<prefix>."""
    if not uri.startswith(SCHEME):
        raise ValueError(f"not a racer:// URI: {uri!r}")
    parts = uri[len(SCHEME) :].split("/", 2)
    if len(parts) < 2 or not parts[0] or not parts[1]:
        raise ValueError(f"racer URI needs a cache and a bucket: {uri!r}")
    prefix = parts[2].strip("/") if len(parts) == 3 else ""
    return ModelURI(parts[0], parts[1], prefix)


def _unsafe(name: str) -> bool:
    return name.startswith("/") or ".." in name.split("/")


def _changed(uri: ModelURI, name: str, etag: str) -> ModelChangedError:
    return ModelChangedError(
        f"racer: {uri.key(name)} in {uri} changed during the load (expected ETag {etag}); "
        "restart the load, and rerun racer-vllm-fetch if the model was replaced"
    )


def stat_pinned(client, uri: ModelURI, name: str, etag: str | None = None):
    """Stat a file, at version etag when given."""
    try:
        return client.stat(uri.request(name), if_match=etag)
    except (racer.VersionUnavailable, racer.NotFound):
        if etag is None:
            raise
        raise _changed(uri, name, etag) from None


def read_pinned(client, uri: ModelURI, name: str, etag: str | None = None):
    """Read a whole small file as (bytes, etag), or None when it does not exist.

    The bytes are read at the ETag of the stat, so they match. With etag
    set, any other version raises ModelChangedError.
    """
    try:
        meta = stat_pinned(client, uri, name, etag)
    except racer.NotFound:
        return None
    try:
        return client.read(uri.request(name), if_match=meta.etag), meta.etag
    except (racer.VersionUnavailable, racer.NotFound):
        raise _changed(uri, name, meta.etag) from None


def weight_files(client, uri: ModelURI, pins: dict | None = None) -> list:
    """Safetensors files named by the index, else model.safetensors."""
    pins = pins or {}
    got = read_pinned(client, uri, INDEX, pins.get(INDEX))
    if got is None:
        stat_pinned(client, uri, SINGLE, pins.get(SINGLE))  # raises NotFound when absent
        return [SINGLE]
    files = sorted(set((json.loads(got[0]).get("weight_map") or {}).values()))
    if not files:
        raise ValueError(f"{INDEX} has an empty weight_map")
    for f in files:
        if _unsafe(f):
            raise ValueError(f"unsafe file name in {INDEX}: {f!r}")
    return files


def fetch(uri, dest: str, client=None) -> list:
    """Copy the small model files to dest and write racer-model.json.

    racer-model.json records the URI and the ETag of every fetched file and
    every weight file, so the loader reads the same model version.
    """
    u = parse_uri(uri) if isinstance(uri, str) else uri
    client = client or racer.Client(cache=u.cache)
    os.makedirs(dest, exist_ok=True)
    names = list(CONFIG_FILES)
    fetched = []
    etags = {}
    i = 0
    while i < len(names):
        name = names[i]
        i += 1
        if name in etags:
            continue
        got = read_pinned(client, u, name)
        if got is None:
            if name == "config.json":
                raise FileNotFoundError(f"{u.key(name)} not found in {u}")
            continue
        data, etags[name] = got
        _write(os.path.join(dest, name), data)
        fetched.append(name)
        if name == INDEX:
            # Small files named in the index metadata, such as a chat template.
            for v in (json.loads(data).get("metadata") or {}).values():
                if isinstance(v, str) and v.endswith(SMALL_SUFFIXES) and not v.startswith("/") and ".." not in v:
                    names.append(v)
    try:
        weights = weight_files(client, u, etags)
    except racer.NotFound:
        weights = []  # for example a sharded_state checkpoint; the loader stats its own files
    for name in weights:
        etags[name] = stat_pinned(client, u, name).etag
    _write(os.path.join(dest, MARKER), json.dumps({"uri": str(u), "etags": etags}, sort_keys=True).encode())
    return fetched


def _write(path: str, data: bytes) -> None:
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path + ".tmp", "wb") as f:
        f.write(data)
    os.replace(path + ".tmp", path)


def _read_marker(model_path: str):
    path = os.path.join(model_path, MARKER)
    if not os.path.exists(path):
        return None
    with open(path) as f:
        return json.load(f)


def resolve_uri(model_path: str, extra: dict) -> ModelURI:
    """The model URI from the extra config, the model name, or racer-model.json."""
    if extra.get("uri"):
        return parse_uri(extra["uri"])
    if model_path.startswith(SCHEME):
        return parse_uri(model_path)
    doc = _read_marker(model_path)
    if doc is not None:
        return parse_uri(doc["uri"])
    raise ValueError(
        "racer loader: no model URI; set model_loader_extra_config.uri or run racer-vllm-fetch into the model directory"
    )


def resolve_pins(model_path: str, uri: ModelURI) -> dict:
    """ETags that racer-vllm-fetch recorded for uri, or {}."""
    doc = _read_marker(model_path)
    if not doc or doc.get("uri") is None or parse_uri(doc["uri"]) != uri:
        return {}
    pins = doc.get("etags") or {}
    if not isinstance(pins, dict) or not all(isinstance(k, str) and isinstance(v, str) for k, v in pins.items()):
        raise ValueError(f"bad etags in {os.path.join(model_path, MARKER)}")
    return pins


# safetensors: an 8-byte little-endian header length N, N bytes of JSON, then
# the data. Each entry has dtype, shape and data_offsets relative to the data.


@dataclass(frozen=True)
class Tensor:
    name: str
    dtype: str
    shape: tuple
    start: int  # absolute file offset
    end: int


def header_length(prefix: bytes) -> int:
    if len(prefix) != 8:
        raise ValueError("short safetensors prefix")
    (n,) = struct.unpack("<Q", prefix)
    if n < 2 or n > MAX_HEADER_BYTES:
        raise ValueError(f"bad safetensors header length {n}")
    return n


def _uints(v) -> bool:
    # JSON integers only; bool is an int subclass, so check the exact type.
    return isinstance(v, list) and all(type(x) is int and x >= 0 for x in v)


def parse_header(raw: bytes, file_size: int | None = None) -> list:
    """Parse the header JSON (after the 8-byte length) into Tensors sorted by offset."""
    try:
        doc = json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, ValueError) as exc:
        raise ValueError("bad safetensors header JSON") from exc
    if not isinstance(doc, dict):
        raise ValueError("safetensors header is not an object")
    base = 8 + len(raw)
    doc.pop("__metadata__", None)
    tensors = []
    for name, spec in doc.items():
        if not isinstance(spec, dict) or spec.get("dtype") not in DTYPES:
            raise ValueError(f"bad entry for {name!r}")
        shape, offsets = spec.get("shape"), spec.get("data_offsets")
        if not _uints(shape) or not _uints(offsets) or len(offsets) != 2 or offsets[1] < offsets[0]:
            raise ValueError(f"bad shape or offsets for {name!r}")
        if offsets[1] - offsets[0] != math.prod(shape) * DTYPES[spec["dtype"]][0]:
            raise ValueError(f"size mismatch for {name!r}")
        tensors.append(Tensor(name, spec["dtype"], tuple(shape), base + offsets[0], base + offsets[1]))
    tensors.sort(key=lambda t: (t.start, t.end))
    prev = base
    for t in tensors:
        if t.start < prev:
            raise ValueError(f"overlapping tensor {t.name!r}")
        prev = t.end
    if file_size is not None and prev > file_size:
        raise ValueError("tensor data past end of file")
    return tensors


@dataclass
class Chunk:
    """A file range read with one request.

    tensors lie wholly inside it. parts are (tensor, offset, length) pieces of
    tensors split across chunks. A dedicated chunk holds one tensor bigger
    than a staging buffer.
    """

    file: str
    etag: str
    start: int
    end: int
    tensors: list
    parts: list
    dedicated: bool = False


def plan_chunks(file: str, etag: str, tensors: list, staging_bytes: int, page_bytes: int = 0) -> list:
    """Group sorted tensors into chunks of at most staging_bytes.

    With page_bytes no larger than staging_bytes, a chunk ends on a page
    boundary, and a tensor across the boundary is split into parts, so no
    page is read twice.
    """
    paged = 0 < page_bytes <= staging_bytes
    chunks = []
    cur = None
    limit = 0
    for t in tensors:
        size = t.end - t.start
        if size > staging_bytes:
            if cur is not None:
                chunks.append(cur)
                cur = None
            chunks.append(Chunk(file, etag, t.start, t.end, [t], [], dedicated=True))
            continue
        if cur is not None and t.end > limit and (t.start >= limit or not paged):
            chunks.append(cur)
            cur = None
        if cur is None:
            cur = Chunk(file, etag, t.start, t.start, [], [])
            limit = _limit(t.start, staging_bytes, page_bytes if paged else 0)
        if t.end <= limit:
            cur.tensors.append(t)
            cur.end = max(cur.end, t.end)
            continue
        pos = t.start
        while t.end > limit:
            cur.parts.append((t, pos - t.start, limit - pos))
            cur.end = pos = limit
            chunks.append(cur)
            cur = Chunk(file, etag, pos, pos, [], [])
            limit = _limit(pos, staging_bytes, page_bytes)
        cur.parts.append((t, pos - t.start, t.end - pos))
        cur.end = t.end
    if cur is not None:
        chunks.append(cur)
    return chunks


def _limit(start: int, staging_bytes: int, page_bytes: int) -> int:
    end = start + staging_bytes
    return end - end % page_bytes if page_bytes else end


def read_chunks(client, uri: ModelURI, files: list, staging_bytes: int) -> list:
    """Read every file's header and plan its chunks. files holds (name, Metadata)."""
    chunks = []
    for name, meta in files:
        req = uri.request(name)
        try:
            n = header_length(client.read(req, 0, 8, if_match=meta.etag))
            tensors = parse_header(client.read(req, 8, n, if_match=meta.etag), meta.size)
        except (racer.VersionUnavailable, racer.NotFound):
            raise _changed(uri, name, meta.etag) from None
        chunks += plan_chunks(name, meta.etag, tensors, staging_bytes, racer.PAGE_SIZE)
    return chunks


def iter_weights(
    client, uri: ModelURI, chunks: list, staging_bytes: int, concurrency: int, clone=True, use_vmm=True, stats=None
):
    """Yield (name, CUDA tensor) for every tensor in chunks.

    Up to concurrency chunks are read ahead, each into its own staging
    buffer. A buffer is reused only after the consumer has moved past its
    tensors and the GPU work queued on it has finished (a CUDA event). Every
    buffer is fenced before it is dropped, also on early close or error.
    stats, a dict, gets counts and bytes of reads per delivery mode.
    """
    import torch
    from racer import cuda as rcuda
    from racer import tensors as rtensors

    if concurrency < 1:
        raise ValueError("concurrency must be at least 1")
    device = torch.cuda.current_device()
    cuda_device = f"cuda:{device}"

    def new_buffer(size):
        # Exportable cuMemCreate memory allows gpu-read; torch memory needs host-read.
        nonlocal use_vmm
        if use_vmm:
            try:
                alloc = rcuda.GpuAllocation(size, device)
                return {"target": alloc, "flat": rtensors.as_tensor(alloc), "event": None}
            except racer.RacerError as exc:
                log.info("racer: exportable GPU memory unavailable (%s); using torch memory", exc)
                use_vmm = False
        flat = torch.empty(size, dtype=torch.uint8, device=cuda_device)
        return {"target": rtensors.TensorTarget(flat), "flat": flat, "event": None}

    def done(buf):
        # Record on the target device's stream; this thread's device may differ.
        with torch.cuda.device(device):
            buf["event"] = torch.cuda.Event()
            buf["event"].record(torch.cuda.current_stream(device))

    def wait(buf):
        if buf["event"] is not None:
            buf["event"].synchronize()
            buf["event"] = None

    def read(chunk, buf):
        wait(buf)
        if chunk.end == chunk.start:
            return None
        try:
            return client.read_into_gpu(
                uri.request(chunk.file),
                buf["target"],
                0,
                offset=chunk.start,
                length=chunk.end - chunk.start,
                if_match=chunk.etag,
            )
        except (racer.VersionUnavailable, racer.NotFound):
            raise _changed(uri, chunk.file, chunk.etag) from None

    def tensor(t, raw):
        dtype = getattr(torch, DTYPES[t.dtype][1])
        return raw.view(dtype).view(t.shape)

    free = [new_buffer(staging_bytes) for _ in range(concurrency)]
    todo = collections.deque(chunks)
    inflight = collections.deque()
    joined = {}
    active = None
    pool = ThreadPoolExecutor(max_workers=concurrency, thread_name_prefix="racer-read")
    try:
        while todo or inflight:
            while todo and len(inflight) < concurrency:
                chunk = todo.popleft()
                buf = new_buffer(chunk.end - chunk.start) if chunk.dedicated else free.pop()
                inflight.append((pool.submit(read, chunk, buf), chunk, buf))
            fut, chunk, buf = inflight.popleft()
            result = fut.result()
            if result is not None and stats is not None:
                stats["reads"][result.delivery] += 1
                stats["bytes"][result.delivery] += result.length
            active = buf
            flat = buf["flat"]
            for t in chunk.tensors:
                size = t.end - t.start
                if size == 0:
                    yield t.name, torch.empty(t.shape, dtype=getattr(torch, DTYPES[t.dtype][1]), device=cuda_device)
                    continue
                rel = t.start - chunk.start
                raw = flat[rel : rel + size]
                # Realign odd offsets; copy out of staging unless clone is off.
                if rel % DTYPES[t.dtype][0] or (clone and not chunk.dedicated):
                    raw = raw.clone()
                yield t.name, tensor(t, raw)
            for t, offset, length in chunk.parts:
                if offset == 0:
                    joined[t.name] = torch.empty(t.end - t.start, dtype=torch.uint8, device=cuda_device)
                rel = t.start + offset - chunk.start
                joined[t.name][offset : offset + length].copy_(flat[rel : rel + length])
                if offset + length == t.end - t.start:
                    yield t.name, tensor(t, joined.pop(t.name))
            active = None
            done(buf)
            if chunk.dedicated:
                wait(buf)  # then drop it; tensors handed out keep their own reference
            else:
                free.append(buf)
    finally:
        for fut, _, _ in inflight:
            fut.cancel()
        pool.shutdown(wait=True)
        if active is not None:
            done(active)
        # Reads have stopped; wait for queued GPU work before buffers are dropped.
        for buf in free + [b for _, _, b in inflight] + ([active] if active else []):
            wait(buf)


def summary(stats: dict, seconds: float, uri) -> str:
    """The one-line load summary."""
    total = sum(stats["bytes"].values()) / (1 << 30)
    rate = total / seconds if seconds > 0 else 0.0
    modes = " ".join(
        f"{m}={stats['reads'][m]} ({stats['bytes'][m] / (1 << 30):.2f} GiB)"
        for m in (racer.GPU, racer.HOST, racer.STREAM)
    )
    return (
        f"racer: loaded {stats['files']} files, {total:.2f} GiB in {seconds:.2f}s ({rate:.2f} GiB/s) "
        f"from {uri}; reads by delivery mode: {modes}"
    )


# vLLM sharded_state checkpoints, as written by ShardedStateLoader.save_model:
# each tensor/pipeline parallel rank has its own files, already split.


def render_pattern(pattern, part: int, tp_rank=0, tp_size=1, pp_rank=0, pp_size=1) -> str:
    """File name of one part, with vLLM's pattern checks."""
    if pattern is None:
        pattern = PP_AND_TP_PATTERN if pp_size > 1 else TP_ONLY_PATTERN
    fields = {f for _, f, _, _ in string.Formatter().parse(pattern) if f is not None}
    unsupported = fields - {"pp_rank", "tp_rank", "rank", "part"}
    if unsupported:
        raise ValueError(f"pattern {pattern!r} uses unsupported placeholders {sorted(unsupported)}")
    if "part" not in fields:
        raise ValueError(f"pattern {pattern!r} needs a {{part}} placeholder")
    if pp_size > 1 and "pp_rank" not in fields:
        raise ValueError(f"pattern {pattern!r} does not embed the pipeline parallel rank")
    if pp_size == 1 and "pp_rank" in fields:
        raise ValueError(f"pattern {pattern!r} requires pipeline parallelism, but the pipeline parallel size is 1")
    if tp_size > 1 and not fields & {"tp_rank", "rank"}:
        raise ValueError(f"pattern {pattern!r} does not embed the tensor parallel rank")
    name = pattern.format(pp_rank=pp_rank, tp_rank=tp_rank, rank=tp_rank, part=part)
    if _unsafe(name):
        raise ValueError(f"unsafe file name from pattern: {name!r}")
    return name


def rank_files(client, uri: ModelURI, pattern, ranks: dict) -> list:
    """This rank's files: part 0, 1, ... until one is missing (Racer has no listing)."""
    files = []
    for part in range(MAX_PARTS):
        name = render_pattern(pattern, part, **ranks)
        try:
            client.stat(uri.request(name))
        except racer.NotFound:
            break
        files.append(name)
    if not files:
        first = render_pattern(pattern, 0, **ranks)
        raise ValueError(f"racer loader: no sharded_state files for this rank in {uri} (looked for {uri.key(first)})")
    return files


def vllm_ranks() -> dict:
    from vllm.distributed import (
        get_pipeline_model_parallel_rank,
        get_pp_group,
        get_tensor_model_parallel_rank,
        get_tp_group,
    )

    return {
        "tp_rank": get_tensor_model_parallel_rank(),
        "tp_size": get_tp_group().world_size,
        "pp_rank": get_pipeline_model_parallel_rank(),
        "pp_size": get_pp_group().world_size,
    }


def load_state(state_dict: dict, weights) -> None:
    """Copy (name, tensor) pairs into the parameters, as vLLM's ShardedStateLoader does.

    A tensor smaller than its parameter (for example LoRA padding) goes into
    a narrowed view. Every key must be loaded exactly once.
    """
    remaining = dict(state_dict)
    for key, tensor in weights:
        param = remaining.pop(key, None)
        if param is None:
            what = "appears twice in" if key in state_dict else "is unexpected in"
            raise ValueError(f"racer loader: tensor {key!r} {what} the sharded checkpoint")
        if param.dtype != tensor.dtype:
            log.warning(
                "racer: loading weight %s with dtype %s into parameter with dtype %s", key, tensor.dtype, param.dtype
            )
        shape = tuple(param.shape)
        if len(tensor.shape) != len(shape):
            raise ValueError(f"racer loader: tensor {key!r} has shape {tuple(tensor.shape)}, parameter has {shape}")
        data = param.data
        for dim, size in enumerate(tensor.shape):
            if size < shape[dim]:
                data = data.narrow(dim, 0, size)
        if tuple(tensor.shape) != shape:
            log.warning(
                "racer: loading tensor of shape %s into parameter %r of shape %s", tuple(tensor.shape), key, shape
            )
        data.copy_(tensor)
    if remaining:
        raise ValueError(f"racer loader: missing keys {tuple(sorted(remaining))[:10]} in the sharded checkpoint")


def settings(extra: dict) -> dict:
    """Loader options from model_loader_extra_config, with defaults."""
    retry = extra.get("mode_retry_seconds", racer.DEFAULT_MODE_RETRY_SECONDS)
    return {
        "uri": extra.get("uri"),
        "staging_bytes": int(extra.get("staging_bytes", DEFAULT_STAGING_BYTES)),
        "concurrency": int(extra.get("concurrency", DEFAULT_CONCURRENCY)),
        "clone": bool(extra.get("clone", True)),
        "socket": extra.get("socket"),
        "use_vmm": bool(extra.get("use_vmm", True)),
        "mode_retry_seconds": None if retry is None else float(retry),
        "sharded_state": bool(extra.get("sharded_state", False)),
        "pattern": extra.get("pattern"),
    }


def register() -> None:
    """vllm.general_plugins entry point: add --load-format racer. Safe to call twice."""
    global _registered
    if _registered:
        return
    from vllm.model_executor.model_loader import register_model_loader
    from vllm.model_executor.model_loader.base_loader import BaseModelLoader

    class RacerModelLoader(BaseModelLoader):
        """Loads safetensors weights from a Racer cache."""

        def __init__(self, load_config):
            super().__init__(load_config)
            self.opts = settings(dict(getattr(load_config, "model_loader_extra_config", None) or {}))

        def _open(self, model_config):
            """Return (uri, client, file names, pins)."""
            o = self.opts
            uri = resolve_uri(str(model_config.model), o)
            pins = resolve_pins(str(model_config.model), uri)
            client = racer.Client(
                cache=uri.cache,
                socket_path=o["socket"],
                staging_bytes=o["staging_bytes"],
                mode_retry_seconds=o["mode_retry_seconds"],
            )
            try:
                if o["sharded_state"]:
                    files = rank_files(client, uri, o["pattern"], vllm_ranks())
                else:
                    files = weight_files(client, uri, pins)
            except BaseException:
                client.close()
                raise
            return uri, client, files, pins

        def download_model(self, model_config) -> None:
            # Nothing to download; check that the weights are there.
            _, client, _, _ = self._open(model_config)
            client.close()

        def load_weights(self, model, model_config) -> None:
            o = self.opts
            started = time.monotonic()
            uri, client, files, pins = self._open(model_config)
            stats = {"files": len(files), "reads": dict.fromkeys((racer.GPU, racer.HOST, racer.STREAM), 0)}
            stats["bytes"] = dict(stats["reads"])
            with client:
                # Stat every file before any data read, so the model is read at one version.
                pinned = [(name, stat_pinned(client, uri, name, pins.get(name))) for name in files]
                chunks = read_chunks(client, uri, pinned, o["staging_bytes"])
                weights = iter_weights(
                    client, uri, chunks, o["staging_bytes"], o["concurrency"], o["clone"], o["use_vmm"], stats
                )
                try:
                    if o["sharded_state"]:
                        load_state(_filter_subtensors(model.state_dict()), weights)
                        loaded = None
                    else:
                        loaded = model.load_weights(weights)
                finally:
                    # Fence and stop reads before the client closes.
                    weights.close()
            log.info("%s", summary(stats, time.monotonic() - started, uri))
            if loaded is not None and getattr(model_config, "quantization", None) is None:
                missing = {n for n, _ in model.named_parameters()} - set(loaded)
                if missing:
                    raise ValueError(f"racer loader: weights not loaded: {sorted(missing)[:10]}")

    register_model_loader(LOAD_FORMAT)(RacerModelLoader)
    _registered = True


def _filter_subtensors(state_dict: dict) -> dict:
    """vLLM's filter for tensors that share memory."""
    from vllm.model_executor.model_loader.sharded_state_loader import ShardedStateLoader

    return ShardedStateLoader._filter_subtensors(state_dict)


def main(argv=None) -> int:
    """racer-vllm-fetch: copy a model's config and tokenizer files from Racer."""
    import argparse

    p = argparse.ArgumentParser(prog="racer-vllm-fetch", description="Fetch model config files from Racer.")
    p.add_argument("uri", help="racer://<cache>/<bucket>/<prefix>")
    p.add_argument("dest", help="local directory")
    a = p.parse_args(argv)
    for name in fetch(a.uri, a.dest):
        print(name)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
