# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Per-rank captured state dicts ("sharded artifacts").

``capture`` saves each rank's ``model.state_dict()`` right after
``load_weights`` and before ``process_weights_after_loading``, so a later
load with the same fingerprint copies tensors straight into parameters
without running weight loaders. Layout under a root (local dir or Racer
prefix)::

    sharded/<fingerprint>/<role>/rank-<rrrrr>/model.safetensors
    sharded/<fingerprint>/<role>/rank-<rrrrr>/racer-sharded.json   (written last)
"""

from __future__ import annotations

import collections
import hashlib
import json
import logging
import os
import struct

from . import manifest as mf
from .planner import plan

log = logging.getLogger("vllm.racer_vllm.sharded")

FORMAT = 1
DATA_NAME = "model.safetensors"
FINGERPRINT_FILE = "racer-fingerprint"


class ShardedMismatch(RuntimeError):
    """A sharded artifact does not match the model (sharded='require')."""


def artifact_prefix(fingerprint: str, role: str, rank: int) -> str:
    return f"sharded/{fingerprint}/{role}/rank-{rank:05d}"


def _dtype_name(dtype) -> str:
    return str(dtype).rsplit(".", 1)[-1]


def _json_safe(v):
    try:
        json.dumps(v)
        return v
    except (TypeError, ValueError):
        if hasattr(v, "to_dict"):
            try:
                return _json_safe(v.to_dict())
            except Exception:
                pass
        return repr(v)


def _dp_size(d, parallel_config) -> int:
    get = getattr(d, "get_dp_group", None)
    if get is not None:
        try:
            return int(get().world_size)
        except Exception:
            pass
    return int(getattr(parallel_config, "data_parallel_size", 1) or 1)


def parallel_info(parallel_config=None) -> dict:
    """tp, pp, ep, dp sizes, enable_expert_parallel, and this rank, from vLLM (best effort).

    dp is not part of the fingerprint: artifacts are laid out per tensor and
    pipeline rank, so every data parallel replica loads the same ones.

    rank and world_size are None when the layout cannot be read, so callers
    never mistake an unknown rank for rank 0.
    """
    out = {"tp": 1, "pp": 1, "ep": 1, "dp": 1, "enable_expert_parallel": False, "rank": None, "world_size": None}
    try:
        from vllm import distributed as d
    except Exception as exc:
        log.debug("racer: parallel info unavailable: %s", exc)
        out["dp"] = _dp_size(None, parallel_config)
        return out
    out["dp"] = _dp_size(d, parallel_config)
    try:

        tp = d.get_tensor_model_parallel_world_size()
        pp = d.get_pp_group().world_size
        rank = d.get_world_group().rank_in_group
    except Exception as exc:
        log.debug("racer: parallel info unavailable: %s", exc)
        return out
    out.update(tp=tp, pp=pp, rank=rank, world_size=tp * pp)
    ep = getattr(d, "get_ep_group", None)
    if ep is not None:
        try:
            out["ep"] = ep().world_size
        except Exception:
            pass
    return out


def rank_problem(par: dict):
    """Why par's rank cannot pick a sharded artifact, or None when it can."""
    rank, ws = par.get("rank"), par.get("world_size")
    if rank is None or ws is None:
        return "the tensor/pipeline parallel rank is unknown"
    if not 0 <= rank < ws:
        return f"rank {rank} is outside [0, {ws})"
    return None


def fingerprint(model, role: str, manifest, vllm_version: str, *, model_config=None, parallel: dict | None = None) -> str:
    """32 hex chars identifying everything that shapes a rank's state dict."""
    digest = manifest if isinstance(manifest, str) else mf.digest(manifest)
    p = dict(parallel or {})
    pc = getattr(model_config, "parallel_config", None)
    quant_cfg = getattr(getattr(model_config, "hf_config", None), "quantization_config", None)
    doc = {
        "format": FORMAT,
        "manifest": digest,
        "vllm": vllm_version,
        "class": type(model).__qualname__,
        "role": role,
        "dtype": str(getattr(model_config, "dtype", None)),
        "quant": {"method": str(getattr(model_config, "quantization", None)), "config": _json_safe(quant_cfg)},
        "tp": p.get("tp", 1),
        "pp": p.get("pp", 1),
        "ep": p.get("ep", 1),
        "enable_expert_parallel": bool(
            p.get("enable_expert_parallel", getattr(pc, "enable_expert_parallel", False))
        ),
        "hf_overrides": _json_safe(getattr(model_config, "hf_overrides", None)),
    }
    return hashlib.sha256(mf.canonical(doc)).hexdigest()[:32]


def filter_subtensors(state: dict) -> dict:
    """Drop tensors whose memory lies within another kept tensor (vLLM's rule)."""
    try:
        from vllm.model_executor.model_loader.sharded_state_loader import ShardedStateLoader

        return ShardedStateLoader._filter_subtensors(state)
    except Exception:
        pass
    groups = collections.defaultdict(list)
    for key, t in state.items():
        if t.numel():
            groups[(str(t.device), t.untyped_storage().data_ptr())].append((key, t))

    def end(t):
        return t.view(-1)[-1].data_ptr() + t.element_size()

    out = {}
    for group in groups.values():
        for k, t in group:
            a, b = t.data_ptr(), end(t)
            for k2, t2 in group:
                if not t2.is_contiguous():
                    continue
                a2, b2 = t2.data_ptr(), end(t2)
                if a < a2 or b2 < b:
                    continue
                if a2 < a or b < b2 or not t.is_contiguous():
                    break
                if k2 < k:
                    break
            else:
                out[k] = t
    return out


def tensor_bytes(t) -> bytes:
    import torch

    flat = t.detach().reshape(-1).contiguous().cpu()
    return flat.view(torch.uint8).numpy().tobytes()


def capture(
    model, role: str, out_dir: str, *, fingerprint: str, rank: int, world_size: int, source_manifest_sha256: str
) -> str:
    """Write this rank's state dict and its sharded manifest (last). Returns the manifest path."""
    state = filter_subtensors(model.state_dict())
    d = os.path.join(out_dir, artifact_prefix(fingerprint, role, rank))
    os.makedirs(d, exist_ok=True)
    header, blobs, entries, pos = {}, [], {}, 0
    for name in sorted(state):
        t = state[name]
        dtype = mf.TORCH_TO_SAFETENSORS.get(_dtype_name(t.dtype))
        if dtype is None:
            raise ValueError(f"racer: cannot capture {name!r}: dtype {t.dtype} has no safetensors code")
        raw = tensor_bytes(t)
        shape = [int(x) for x in t.shape]
        header[name] = {"dtype": dtype, "shape": shape, "data_offsets": [pos, pos + len(raw)]}
        entries[name] = {"dtype": dtype, "shape": shape, "size": len(raw), "sha256": hashlib.sha256(raw).hexdigest()}
        blobs.append(raw)
        pos += len(raw)
    h = json.dumps(header, separators=(",", ":")).encode()
    h += b" " * (-len(h) % 8)
    base = 8 + len(h)
    for name, e in entries.items():
        e["offset"] = base + header[name]["data_offsets"][0]
    tmp = os.path.join(d, DATA_NAME + ".tmp")
    with open(tmp, "wb") as f:
        f.write(struct.pack("<Q", len(h)))
        f.write(h)
        for raw in blobs:
            f.write(raw)
    os.replace(tmp, os.path.join(d, DATA_NAME))
    doc = {
        "schema": mf.SHARDED_SCHEMA,
        "fingerprint": fingerprint,
        "rank": rank,
        "world_size": world_size,
        "role": role,
        "source_manifest_sha256": source_manifest_sha256,
        "files": [{"name": DATA_NAME, "size": 8 + len(h) + pos, "tensors": entries}],
    }
    path = os.path.join(d, mf.SHARDED_NAME)
    with open(path + ".tmp", "w") as f:
        f.write(mf.dumps_sharded(doc))
    os.replace(path + ".tmp", path)
    with open(os.path.join(out_dir, FINGERPRINT_FILE), "w") as f:
        f.write(fingerprint + "\n")
    return path


def _mismatch(doc: dict, state: dict, fp: str, rank: int, role: str):
    """The first difference between an artifact and the model's state dict, or None."""
    for k, want in (("fingerprint", fp), ("rank", rank), ("role", role)):
        if doc[k] != want:
            return f"{k} is {doc[k]!r}, expected {want!r}"
    seen = {}
    for f in doc["files"]:
        for name, t in f["tensors"].items():
            seen[name] = t
    for name in sorted(set(state) | set(seen)):
        if name not in seen:
            return f"tensor {name!r} is missing from the artifact"
        if name not in state:
            return f"tensor {name!r} is not in the model"
        p, t = state[name], seen[name]
        if mf.TORCH_TO_SAFETENSORS.get(_dtype_name(p.dtype)) != t["dtype"]:
            return f"tensor {name!r} dtype {t['dtype']} != model {p.dtype}"
        if [int(x) for x in p.shape] != list(t["shape"]):
            return f"tensor {name!r} shape {t['shape']} != model {list(p.shape)}"
    return None


def load_sharded(client, uri, model, rank: int, role: str, fp: str, make_reader, *, require: bool = False) -> bool:
    """Copy a matching artifact into the model. Returns False to fall back.

    make_reader() returns a reader.WeightReader. With require, any problem raises ShardedMismatch.
    """
    import racer

    prefix = uri.child(artifact_prefix(fp, role, rank))

    def fail(msg: str) -> bool:
        if require:
            raise ShardedMismatch(f"racer: sharded artifact {prefix}: {msg}")
        log.info("racer: not using sharded artifact %s: %s; loading from the checkpoint", prefix, msg)
        return False

    try:
        meta = client.stat(prefix.request(mf.SHARDED_NAME))
        doc = mf.loads_sharded(client.read(prefix.request(mf.SHARDED_NAME), if_match=meta.etag))
    except racer.NotFound:
        return fail("not found")
    except mf.ManifestError as exc:
        return fail(str(exc))
    state = filter_subtensors(model.state_dict())
    why = _mismatch(doc, state, fp, rank, role)
    if why is not None:
        log.warning("racer: sharded artifact mismatch: %s", why)
        return fail(why)
    reader = make_reader()
    jobs = []
    try:
        for f in doc["files"]:
            req = prefix.request(f["name"])
            fmeta = client.stat(req)
            if f.get("size") is not None and fmeta.size != f["size"]:
                raise mf.ManifestError(f"{f['name']} is {fmeta.size} bytes in Racer but {f['size']} in the manifest")
            tensors = [
                mf.Tensor(n, t["dtype"], tuple(t["shape"]), t["offset"], t["offset"] + t["size"])
                for n, t in f["tensors"].items()
            ]
            for r in plan(f["name"], tensors, None, reader.pool.staging_bytes, file_size=fmeta.size):
                jobs.append((req, r, fmeta.etag))
            reader.stats["files"] += 1
    except racer.NotFound as exc:
        return fail(f"data file missing: {exc}")
    except (mf.ManifestError, ValueError) as exc:
        return fail(str(exc))
    for name, tensor in reader.iter(jobs):
        state[name].data.copy_(tensor)
    return True


def captured(out_dir: str) -> dict:
    """Captured artifacts under out_dir: {(fingerprint, role): {rank: (dir, doc)}}.

    Raises ManifestError for an invalid rank manifest or a set of ranks that
    does not cover its world size, so an incomplete capture is never uploaded.
    """
    root = os.path.join(out_dir, "sharded")
    found: dict = {}
    if not os.path.isdir(root):
        return found
    for fp in sorted(os.listdir(root)):
        for role in sorted(os.listdir(os.path.join(root, fp))):
            rdir = os.path.join(root, fp, role)
            for rank_dir in sorted(os.listdir(rdir)):
                d = os.path.join(rdir, rank_dir)
                path = os.path.join(d, mf.SHARDED_NAME)
                if not os.path.isfile(path):
                    raise mf.ManifestError(f"{d}: no {mf.SHARDED_NAME}; the capture did not finish")
                doc = mf.load_sharded(path)
                if doc["fingerprint"] != fp or doc["role"] != role or rank_dir != f"rank-{doc['rank']:05d}":
                    raise mf.ManifestError(f"{path}: fingerprint, role or rank does not match its directory")
                found.setdefault((fp, role), {})[doc["rank"]] = (d, doc)
    for (fp, role), ranks in found.items():
        sizes = {doc["world_size"] for _, doc in ranks.values()}
        if len(sizes) != 1 or set(ranks) != set(range(sizes.pop())):
            raise mf.ManifestError(
                f"captured artifact {fp}/{role} has ranks {sorted(ranks)}, which do not cover its world size"
            )
    return found
