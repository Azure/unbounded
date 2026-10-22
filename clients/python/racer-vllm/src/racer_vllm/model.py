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


def read_small(client: racer.Client, uri: ModelURI, name: str) -> bytes | None:
    """Read a whole small object, or None when it does not exist."""
    try:
        return client.read(uri.request(name))
    except racer.NotFound:
        return None


def index_extra_files(index: dict) -> list:
    """Small file names listed as string values in the index metadata."""
    out = []
    for v in (index.get("metadata") or {}).values():
        if isinstance(v, str) and v.endswith(_SMALL_SUFFIXES) and not v.startswith("/") and ".." not in v:
            out.append(v)
    return out


def weight_files(client: racer.Client, uri: ModelURI) -> list:
    """List safetensors files from the index, else the single model.safetensors."""
    raw = read_small(client, uri, INDEX)
    if raw is None:
        client.stat(uri.request(SINGLE))  # raises NotFound with a clear key
        return [SINGLE]
    index = json.loads(raw)
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
    """
    u = parse_uri(uri) if isinstance(uri, str) else uri
    client = client or client_for(u)
    os.makedirs(dest, exist_ok=True)
    names = list(CONFIG_FILES) + list(extra)
    fetched = []
    seen = set()
    while names:
        name = names.pop(0)
        if name in seen:
            continue
        seen.add(name)
        data = read_small(client, u, name)
        if data is None:
            if name in REQUIRED_FILES:
                raise FileNotFoundError(f"{u.key(name)} not found in {u}")
            continue
        path = os.path.join(dest, name)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        tmp = path + ".tmp"
        with open(tmp, "wb") as f:
            f.write(data)
        os.replace(tmp, path)
        fetched.append(name)
        if name == INDEX:
            names.extend(index_extra_files(json.loads(data)))
    with open(os.path.join(dest, MARKER), "w") as f:
        json.dump({"uri": str(u)}, f)
    return fetched


def resolve_uri(model_path: str, extra: dict) -> ModelURI:
    """Find the racer:// URI from extra config, the model name, or the marker."""
    if extra.get("uri"):
        return parse_uri(extra["uri"])
    if model_path.startswith(SCHEME):
        return parse_uri(model_path)
    marker = os.path.join(model_path, MARKER)
    if os.path.exists(marker):
        with open(marker) as f:
            return parse_uri(json.load(f)["uri"])
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
