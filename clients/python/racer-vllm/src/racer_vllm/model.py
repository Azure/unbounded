# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""racer:// model locations and small-file fetching."""

from __future__ import annotations

import json
import os
from dataclasses import dataclass

import racer

SCHEME = "racer://"
MARKER = "racer-model.json"
INDEX = "model.safetensors.index.json"
SINGLE = "model.safetensors"

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
REQUIRED_FILES = ("config.json",)
_SMALL_SUFFIXES = (".json", ".txt", ".model", ".jinja", ".py", ".tiktoken")


@dataclass(frozen=True)
class ModelURI:
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


def client_for(uri: ModelURI, socket_path: str | None = None, **kw) -> racer.Client:
    """Client for the URI's cache. RACER_SOCKET or socket_path override the path."""
    return racer.Client(cache=uri.cache, socket_path=socket_path, **kw)


class ModelChangedError(RuntimeError):
    """A model file no longer has the version the load started with."""


def _changed(uri: ModelURI, name: str, etag: str) -> ModelChangedError:
    return ModelChangedError(
        f"racer: {uri.key(name)} in {uri} changed during the load (expected ETag {etag}); "
        "restart the load, and rerun racer-vllm-fetch if the model was replaced"
    )


def stat_pinned(client: racer.Client, uri: ModelURI, name: str, etag: str | None = None) -> racer.Metadata:
    """Stat a file, at version etag when given. Raises ModelChangedError on a mismatch."""
    try:
        return client.stat(uri.request(name), if_match=etag)
    except racer.VersionUnavailable:
        if etag is None:
            raise
        raise _changed(uri, name, etag) from None


def read_pinned(client: racer.Client, uri: ModelURI, name: str, etag: str | None = None) -> tuple | None:
    """Read a whole small object as (bytes, etag), or None when it does not exist.

    The object is stat'ed first and read at that ETag, so the bytes and the
    ETag match. With etag set, a different version raises ModelChangedError.
    """
    try:
        meta = stat_pinned(client, uri, name, etag)
    except racer.NotFound:
        if etag is not None:
            raise _changed(uri, name, etag) from None
        return None
    try:
        return client.read(uri.request(name), if_match=meta.etag), meta.etag
    except (racer.VersionUnavailable, racer.NotFound):
        raise _changed(uri, name, meta.etag) from None


def read_small(client: racer.Client, uri: ModelURI, name: str) -> bytes | None:
    """Read a whole small object, or None when it does not exist."""
    got = read_pinned(client, uri, name)
    return None if got is None else got[0]


def index_extra_files(index: dict) -> list:
    """Small file names listed as string values in the index metadata."""
    out = []
    for v in (index.get("metadata") or {}).values():
        if isinstance(v, str) and v.endswith(_SMALL_SUFFIXES) and not v.startswith("/") and ".." not in v:
            out.append(v)
    return out


def weight_files(client: racer.Client, uri: ModelURI, pins: dict | None = None) -> list:
    """List safetensors files from the index, else the single model.safetensors.

    pins maps file names to ETags (from racer-model.json); a pinned index is
    read at that version.
    """
    pins = pins or {}
    got = read_pinned(client, uri, INDEX, pins.get(INDEX))
    if got is None:
        stat_pinned(client, uri, SINGLE, pins.get(SINGLE))  # raises NotFound with a clear key
        return [SINGLE]
    index = json.loads(got[0])
    files = sorted(set((index.get("weight_map") or {}).values()))
    if not files:
        raise ValueError(f"{INDEX} has an empty weight_map")
    for f in files:
        if f.startswith("/") or ".." in f.split("/"):
            raise ValueError(f"unsafe file name in {INDEX}: {f!r}")
    return files


def fetch(uri: str | ModelURI, dest: str, client: racer.Client | None = None, extra=()) -> list:
    """Copy the small model files to dest and write the racer-model.json marker.

    Point vLLM at dest (--model dest, and --tokenizer dest if needed) with
    --load-format racer. The loader reads the marker to find the weights.

    Each file is read at one ETag, and the marker records the ETags of the
    fetched files and of the weight files, so the loader reads the same
    version of the model that the config and tokenizer came from.
    """
    u = parse_uri(uri) if isinstance(uri, str) else uri
    client = client or client_for(u)
    os.makedirs(dest, exist_ok=True)
    names = list(CONFIG_FILES) + list(extra)
    fetched = []
    etags: dict = {}
    seen = set()
    while names:
        name = names.pop(0)
        if name in seen:
            continue
        seen.add(name)
        got = read_pinned(client, u, name)
        if got is None:
            if name in REQUIRED_FILES:
                raise FileNotFoundError(f"{u.key(name)} not found in {u}")
            continue
        data, etags[name] = got
        path = os.path.join(dest, name)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        tmp = path + ".tmp"
        with open(tmp, "wb") as f:
            f.write(data)
        os.replace(tmp, path)
        fetched.append(name)
        if name == INDEX:
            names.extend(index_extra_files(json.loads(data)))
    weights = None
    if INDEX in etags:
        weights = weight_files(client, u, etags)
    else:
        try:
            weights = weight_files(client, u)
        except racer.NotFound:
            pass  # e.g. a sharded_state checkpoint; the loader stats its own files
    for name in weights or ():
        etags[name] = stat_pinned(client, u, name).etag
    tmp = os.path.join(dest, MARKER + ".tmp")
    with open(tmp, "w") as f:
        json.dump({"uri": str(u), "etags": etags}, f, sort_keys=True)
    os.replace(tmp, os.path.join(dest, MARKER))
    return fetched


def read_marker(model_path: str) -> dict | None:
    marker = os.path.join(model_path, MARKER)
    if not os.path.exists(marker):
        return None
    with open(marker) as f:
        return json.load(f)


def resolve_pins(model_path: str, uri: ModelURI) -> dict:
    """ETags recorded by racer-vllm-fetch for uri, or {} when there are none."""
    doc = read_marker(model_path)
    if not doc or doc.get("uri") is None or parse_uri(doc["uri"]) != uri:
        return {}
    pins = doc.get("etags") or {}
    if not isinstance(pins, dict) or not all(isinstance(k, str) and isinstance(v, str) for k, v in pins.items()):
        raise ValueError(f"bad etags in {os.path.join(model_path, MARKER)}")
    return pins


def resolve_uri(model_path: str, extra: dict) -> ModelURI:
    """Find the racer:// URI from extra config, the model name, or the marker."""
    if extra.get("uri"):
        return parse_uri(extra["uri"])
    if model_path.startswith(SCHEME):
        return parse_uri(model_path)
    doc = read_marker(model_path)
    if doc is not None:
        return parse_uri(doc["uri"])
    raise ValueError(
        "racer loader: no model URI; set model_loader_extra_config.uri or run racer-vllm-fetch into the model directory"
    )


def main(argv=None) -> int:
    import argparse

    p = argparse.ArgumentParser(prog="racer-vllm-fetch", description="Fetch model config files from Racer.")
    p.add_argument("uri", help="racer://<cache>/<bucket>/<prefix>")
    p.add_argument("dest", help="local directory")
    p.add_argument("--file", action="append", default=[], help="extra small file to fetch")
    a = p.parse_args(argv)
    for name in fetch(a.uri, a.dest, extra=a.file):
        print(name)
    return 0
