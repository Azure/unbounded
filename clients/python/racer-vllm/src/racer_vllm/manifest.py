# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Model manifests (v1) and per-rank sharded manifests.

A model manifest lists every file of a checkpoint with its size and sha256,
and, for safetensors files, the parsed header. The loader plans reads from it
without touching the file headers. ``racer-manifest.json`` is written last on
import, so its presence means every file it names is uploaded.

A sharded manifest (``racer-sharded.json``) describes one rank's captured
state dict, written last by ``sharded.capture``. ``racer-sharded-index.json``
at the model prefix lists the uploaded artifacts; ``racer-vllm prepare``
writes it after every rank's files.

``racer-source.json`` is local only: ``racer-vllm materialize`` writes it
next to the copied files with the model's racer:// URI and the ETag of every
file, so the loader reads the versions that were materialized.
"""

from __future__ import annotations

import hashlib
import json
import math
import os
import re
import struct
from dataclasses import dataclass

from .uri import unsafe_name

SCHEMA = "racer.unbounded-cloud.io/model-manifest/v1"
SHARDED_SCHEMA = "racer.unbounded-cloud.io/model-sharded/v1"
MANIFEST_NAME = "racer-manifest.json"
SHARDED_NAME = "racer-sharded.json"
SHARDED_INDEX_SCHEMA = "racer.unbounded-cloud.io/model-sharded-index/v1"
SHARDED_INDEX_NAME = "racer-sharded-index.json"
SOURCE_SCHEMA = "racer.unbounded-cloud.io/model-source/v1"
SOURCE_NAME = "racer-source.json"
MAX_HEADER_BYTES = 100 * 1024 * 1024
KINDS = ("aux", "safetensors")
ROLES = ("target", "draft")

_SHA = re.compile(r"[0-9a-f]{64}")
_FINGERPRINT = re.compile(r"[0-9a-f]{32}")

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
TORCH_TO_SAFETENSORS = {v[1]: k for k, v in DTYPES.items()}


class ManifestError(ValueError):
    """A manifest is malformed or does not match the data."""


@dataclass(frozen=True)
class Tensor:
    """A tensor in a safetensors file. start and end are absolute file offsets."""

    name: str
    dtype: str
    shape: tuple
    start: int
    end: int

    @property
    def nbytes(self) -> int:
        return self.end - self.start


@dataclass(frozen=True)
class SafetensorsFile:
    name: str
    size: int
    sha256: str
    header_bytes: int
    tensors: tuple  # of Tensor, sorted by start


# Canonical JSON ---------------------------------------------------------------


def canonical(doc) -> bytes:
    """Sorted keys, no whitespace, UTF-8. Digests are over these bytes."""
    return json.dumps(doc, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode()


def dumps(m: dict) -> str:
    """Validate a model manifest and return its canonical JSON."""
    validate(m)
    return canonical(m).decode()


def digest(m: dict) -> str:
    """sha256 hex of the canonical bytes of a model manifest."""
    return hashlib.sha256(canonical(validate(m))).hexdigest()


def loads(data) -> dict:
    try:
        doc = json.loads(data)
    except (UnicodeDecodeError, ValueError) as exc:
        raise ManifestError(f"manifest is not valid JSON: {exc}") from exc
    return validate(doc)


def load(path: str) -> dict:
    """Read and validate a model manifest from a local path."""
    try:
        with open(path, "rb") as f:
            data = f.read()
    except FileNotFoundError:
        raise ManifestError(
            f"{path} not found; run 'racer-vllm materialize <racer-uri> <dir>' to fetch the manifest"
        ) from None
    try:
        return loads(data)
    except ManifestError as exc:
        raise ManifestError(f"{path}: {exc}") from None


# safetensors headers ----------------------------------------------------------
# An 8-byte little-endian header length N, N bytes of JSON, then the data.
# data_offsets are relative to the data.


def header_length(prefix: bytes) -> int:
    if len(prefix) != 8:
        raise ManifestError("short safetensors prefix")
    (n,) = struct.unpack("<Q", prefix)
    if n < 2 or n > MAX_HEADER_BYTES:
        raise ManifestError(f"bad safetensors header length {n}")
    return n


def _uints(v) -> bool:
    # JSON integers only; bool is an int subclass, so check the exact type.
    return isinstance(v, list) and all(type(x) is int and x >= 0 for x in v)


def _tensor_entry(name: str, spec, data_size: int | None) -> dict:
    if not isinstance(spec, dict) or spec.get("dtype") not in DTYPES:
        raise ManifestError(f"bad entry for tensor {name!r}: dtype must be one of {sorted(DTYPES)}")
    shape, offsets = spec.get("shape"), spec.get("data_offsets")
    if not _uints(shape) or not _uints(offsets) or len(offsets) != 2 or offsets[1] < offsets[0]:
        raise ManifestError(f"bad shape or data_offsets for tensor {name!r}")
    if offsets[1] - offsets[0] != math.prod(shape) * DTYPES[spec["dtype"]][0]:
        raise ManifestError(f"size mismatch for tensor {name!r}: data_offsets do not match dtype and shape")
    if data_size is not None and offsets[1] > data_size:
        raise ManifestError(f"tensor {name!r} data is past the end of the file")
    return {"dtype": spec["dtype"], "shape": list(shape), "data_offsets": list(offsets)}


def parse_header_json(raw: bytes, file_size: int | None = None) -> dict:
    """Parse header JSON (after the 8-byte length) into manifest tensor entries."""
    try:
        doc = json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, ValueError) as exc:
        raise ManifestError("bad safetensors header JSON") from exc
    if not isinstance(doc, dict):
        raise ManifestError("safetensors header is not an object")
    doc.pop("__metadata__", None)
    data_size = None if file_size is None else file_size - 8 - len(raw)
    out = {name: _tensor_entry(name, spec, data_size) for name, spec in doc.items()}
    tensors_of(out, 8 + len(raw))  # overlap check
    return out


def parse_header(raw: bytes, file_size: int | None = None) -> list:
    """Parse header JSON into Tensors with absolute offsets, sorted by offset."""
    return tensors_of(parse_header_json(raw, file_size), 8 + len(raw))


def tensors_of(entries: dict, base: int) -> list:
    tensors = [
        Tensor(name, e["dtype"], tuple(e["shape"]), base + e["data_offsets"][0], base + e["data_offsets"][1])
        for name, e in entries.items()
    ]
    tensors.sort(key=lambda t: (t.start, t.end))
    prev = base
    for t in tensors:
        if t.start < prev:
            raise ManifestError(f"overlapping tensor {t.name!r}")
        prev = max(prev, t.end)
    return tensors


# Model manifest ---------------------------------------------------------------


def _need(cond: bool, msg: str) -> None:
    if not cond:
        raise ManifestError(msg)


def validate(doc) -> dict:
    """Check a model manifest and return it. Errors say which field is wrong."""
    _need(isinstance(doc, dict), "manifest must be a JSON object")
    _need(doc.get("schema") == SCHEMA, f"manifest schema must be {SCHEMA!r}, got {doc.get('schema')!r}")
    extra = set(doc) - {"schema", "source", "index", "files"}
    _need(not extra, f"unknown manifest fields {sorted(extra)}")
    src = doc.get("source")
    _need(isinstance(src, dict), "manifest source must be an object with hub, repo and revision")
    for k in ("hub", "repo", "revision"):
        _need(isinstance(src.get(k), str), f"manifest source.{k} must be a string")
    files = doc.get("files")
    _need(isinstance(files, list), "manifest files must be a list")
    seen = set()
    for i, f in enumerate(files):
        _need(isinstance(f, dict), f"files[{i}] must be an object")
        name = f.get("name")
        _need(isinstance(name, str) and not unsafe_name(name), f"files[{i}].name is missing or unsafe: {name!r}")
        _need(name not in seen, f"file {name!r} is listed twice")
        seen.add(name)
        _need(type(f.get("size")) is int and f["size"] >= 0, f"file {name!r}: size must be a non-negative integer")
        _need(isinstance(f.get("sha256"), str) and _SHA.fullmatch(f["sha256"]), f"file {name!r}: sha256 must be 64 hex")
        kind = f.get("kind")
        _need(kind in KINDS, f"file {name!r}: kind must be 'aux' or 'safetensors', got {kind!r}")
        if kind == "aux":
            _need("tensors" not in f and "header_bytes" not in f, f"aux file {name!r} must not carry tensors")
            continue
        hb = f.get("header_bytes")
        _need(type(hb) is int and 2 <= hb <= MAX_HEADER_BYTES, f"safetensors file {name!r}: bad header_bytes")
        _need(8 + hb <= f["size"], f"safetensors file {name!r}: header_bytes exceeds the file size")
        tensors = f.get("tensors")
        _need(isinstance(tensors, dict), f"safetensors file {name!r}: tensors must be an object")
        try:
            for tname, spec in tensors.items():
                _tensor_entry(tname, spec, f["size"] - 8 - hb)
            tensors_of(tensors, 8 + hb)
        except ManifestError as exc:
            raise ManifestError(f"safetensors file {name!r}: {exc}") from None
    index = doc.get("index")
    _need(index is None or index in seen, f"manifest index {index!r} is not one of the files")
    return doc


def safetensors_files(m: dict) -> list:
    """SafetensorsFile for each kind == "safetensors" file, in manifest order."""
    out = []
    for f in m["files"]:
        if f["kind"] != "safetensors":
            continue
        tensors = tuple(tensors_of(f["tensors"], 8 + f["header_bytes"]))
        out.append(SafetensorsFile(f["name"], f["size"], f["sha256"], f["header_bytes"], tensors))
    return out


def file_entry(m: dict, name: str) -> dict:
    for f in m["files"]:
        if f["name"] == name:
            return f
    raise ManifestError(f"{name!r} is not in the manifest")


def check_sha256(name: str, data: bytes, expected: str) -> None:
    got = hashlib.sha256(data).hexdigest()
    if got != expected:
        raise ManifestError(f"{name}: sha256 mismatch: manifest has {expected}, data has {got}")


# Sharded manifest -------------------------------------------------------------


def validate_sharded(doc) -> dict:
    _need(isinstance(doc, dict), "sharded manifest must be a JSON object")
    _need(doc.get("schema") == SHARDED_SCHEMA, f"sharded manifest schema must be {SHARDED_SCHEMA!r}")
    extra = set(doc) - {"schema", "fingerprint", "rank", "world_size", "role", "source_manifest_sha256", "files"}
    _need(not extra, f"unknown sharded manifest fields {sorted(extra)}")
    _need(isinstance(doc.get("fingerprint"), str) and _FINGERPRINT.fullmatch(doc["fingerprint"]), "bad fingerprint")
    ws = doc.get("world_size")
    _need(type(ws) is int and ws >= 1, "world_size must be a positive integer")
    _need(type(doc.get("rank")) is int and 0 <= doc["rank"] < ws, "rank must be in [0, world_size)")
    _need(doc.get("role") in ROLES, "role must be 'target' or 'draft'")
    sms = doc.get("source_manifest_sha256")
    _need(isinstance(sms, str) and _SHA.fullmatch(sms), "source_manifest_sha256 must be 64 hex")
    files = doc.get("files")
    _need(isinstance(files, list), "sharded files must be a list")
    seen_files, seen_tensors = set(), set()
    for i, f in enumerate(files):
        _need(isinstance(f, dict) and isinstance(f.get("name"), str), f"files[{i}] needs a name")
        _need(not unsafe_name(f["name"]) and f["name"] not in seen_files, f"files[{i}].name is unsafe or repeated")
        seen_files.add(f["name"])
        tensors = f.get("tensors")
        _need(isinstance(tensors, dict), f"files[{i}].tensors must be an object")
        for tname, t in tensors.items():
            _need(tname not in seen_tensors, f"tensor {tname!r} appears twice")
            seen_tensors.add(tname)
            _need(isinstance(t, dict) and t.get("dtype") in DTYPES, f"tensor {tname!r}: bad dtype")
            _need(_uints(t.get("shape")), f"tensor {tname!r}: bad shape")
            _need(type(t.get("offset")) is int and t["offset"] >= 0, f"tensor {tname!r}: bad offset")
            size = math.prod(t["shape"]) * DTYPES[t["dtype"]][0]
            _need(t.get("size") == size, f"tensor {tname!r}: size does not match dtype and shape")
            _need(isinstance(t.get("sha256"), str) and _SHA.fullmatch(t["sha256"]), f"tensor {tname!r}: bad sha256")
    return doc


def dumps_sharded(doc: dict) -> str:
    validate_sharded(doc)
    return canonical(doc).decode()


def loads_sharded(data) -> dict:
    try:
        doc = json.loads(data)
    except (UnicodeDecodeError, ValueError) as exc:
        raise ManifestError(f"sharded manifest is not valid JSON: {exc}") from exc
    return validate_sharded(doc)


def load_sharded(path: str) -> dict:
    with open(path, "rb") as f:
        return loads_sharded(f.read())


# Sharded index ----------------------------------------------------------------


def validate_sharded_index(doc) -> dict:
    _need(isinstance(doc, dict), "sharded index must be a JSON object")
    _need(doc.get("schema") == SHARDED_INDEX_SCHEMA, f"sharded index schema must be {SHARDED_INDEX_SCHEMA!r}")
    arts = doc.get("artifacts")
    _need(isinstance(arts, list), "sharded index artifacts must be a list")
    seen = set()
    for i, a in enumerate(arts):
        _need(isinstance(a, dict), f"artifacts[{i}] must be an object")
        _need(isinstance(a.get("fingerprint"), str) and _FINGERPRINT.fullmatch(a["fingerprint"]), f"artifacts[{i}]: bad fingerprint")
        _need(a.get("role") in ROLES, f"artifacts[{i}]: role must be 'target' or 'draft'")
        _need(type(a.get("world_size")) is int and a["world_size"] >= 1, f"artifacts[{i}]: bad world_size")
        _need(isinstance(a.get("vllm", ""), str), f"artifacts[{i}]: vllm must be a string")
        key = (a["fingerprint"], a["role"])
        _need(key not in seen, f"artifact {key} is listed twice")
        seen.add(key)
    return doc


def loads_sharded_index(data) -> dict:
    try:
        doc = json.loads(data)
    except (UnicodeDecodeError, ValueError) as exc:
        raise ManifestError(f"sharded index is not valid JSON: {exc}") from exc
    return validate_sharded_index(doc)


def merge_sharded_index(old, entries: list) -> dict:
    """old (a validated index or None) with entries added, replacing same fingerprint and role."""
    arts = {(a["fingerprint"], a["role"]): a for a in (old or {}).get("artifacts", [])}
    for e in entries:
        arts[(e["fingerprint"], e["role"])] = e
    doc = {"schema": SHARDED_INDEX_SCHEMA, "artifacts": [arts[k] for k in sorted(arts)]}
    return validate_sharded_index(doc)


# Materialized source ----------------------------------------------------------


def validate_source(doc) -> dict:
    _need(isinstance(doc, dict), "source file must be a JSON object")
    _need(doc.get("schema") == SOURCE_SCHEMA, f"source file schema must be {SOURCE_SCHEMA!r}")
    _need(isinstance(doc.get("uri"), str) and doc["uri"].startswith("racer://"), "source file uri must be a racer:// URI")
    etags = doc.get("etags")
    _need(isinstance(etags, dict), "source file etags must be an object")
    for k, v in etags.items():
        _need(isinstance(k, str) and isinstance(v, str) and v, f"source file etag for {k!r} must be a string")
    return doc


def dumps_source(uri: str, etags: dict) -> str:
    return canonical(validate_source({"schema": SOURCE_SCHEMA, "uri": uri, "etags": etags})).decode()


def load_source(directory: str):
    """The validated racer-source.json in directory, or None when there is none."""
    p = os.path.join(directory, SOURCE_NAME)
    try:
        with open(p, "rb") as f:
            data = f.read()
    except FileNotFoundError:
        return None
    try:
        return validate_source(json.loads(data))
    except (UnicodeDecodeError, ValueError) as exc:
        raise ManifestError(f"{p}: {exc}") from None
