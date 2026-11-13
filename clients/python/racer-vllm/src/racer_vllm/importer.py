# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""``racer-vllm import``: upload a Hugging Face snapshot and its manifest.

Snapshot entries are usually relative symlinks into ``../../blobs``; they
are followed, but a link must resolve inside the model's cache directory
(the snapshot's grandparent) or the snapshot itself. Every file is hashed,
blobs whose recorded sha256 matches are skipped, and ``racer-manifest.json``
is uploaded last. A stale manifest is deleted before any blob changes, so a
partial upload is never visible as complete.
"""

from __future__ import annotations

import os
import re

from . import manifest as mf
from .store import sha256_file

INDEX_NAME = "model.safetensors.index.json"
SKIP_DIRS = frozenset({".git", ".cache", ".huggingface"})
_HF_DIR = re.compile(r"^models--(.+)$")


class SnapshotError(RuntimeError):
    """The snapshot cannot be imported."""


def _allowed_roots(source: str) -> list:
    src = os.path.realpath(source)
    roots = [src]
    parent = os.path.dirname(src)
    if os.path.basename(parent) == "snapshots":
        roots.append(os.path.dirname(parent))
    return roots


def _inside(path: str, roots: list) -> bool:
    return any(path == r or path.startswith(r.rstrip("/") + "/") for r in roots)


def scan(source: str) -> list:
    """(name, real path) of every file under source, sorted by name."""
    if not os.path.isdir(source):
        raise SnapshotError(f"source {source!r} is not a directory")
    roots = _allowed_roots(source)
    out = []
    for dirpath, dirnames, filenames in os.walk(source):
        dirnames[:] = sorted(d for d in dirnames if d not in SKIP_DIRS)
        for fn in filenames:
            p = os.path.join(dirpath, fn)
            name = os.path.relpath(p, source).replace(os.sep, "/")
            real = os.path.realpath(p)
            if not _inside(real, roots):
                raise SnapshotError(f"{name} links outside the model cache directory: {real}")
            if not os.path.isfile(real):
                raise SnapshotError(f"{name} is a dangling link or not a regular file")
            out.append((name, real))
    if not out:
        raise SnapshotError(f"source {source!r} has no files")
    out.sort()
    return out


def guess_source(source: str) -> dict:
    """hub, repo and revision from a .../models--org--name/snapshots/<rev> path."""
    src = os.path.realpath(source)
    parent = os.path.dirname(src)
    repo, revision = "", ""
    if os.path.basename(parent) == "snapshots":
        revision = os.path.basename(src)
        m = _HF_DIR.match(os.path.basename(os.path.dirname(parent)))
        if m:
            repo = m.group(1).replace("--", "/")
    return {"hub": "huggingface", "repo": repo, "revision": revision}


def file_entry(name: str, path: str) -> dict:
    size = os.path.getsize(path)
    entry = {"name": name, "size": size, "sha256": sha256_file(path), "kind": "aux"}
    if not name.endswith(".safetensors"):
        return entry
    try:
        with open(path, "rb") as f:
            n = mf.header_length(f.read(8))
            if 8 + n > size:
                raise mf.ManifestError("header_bytes exceeds the file size")
            tensors = mf.parse_header_json(f.read(n), size)
    except mf.ManifestError as exc:
        raise mf.ManifestError(f"{name}: {exc}") from None
    entry.update(kind="safetensors", header_bytes=n, tensors=tensors)
    return entry


def build_manifest(files: list, source: dict) -> dict:
    """A validated model manifest for scanned files."""
    entries = [file_entry(n, p) for n, p in files if n != mf.MANIFEST_NAME]
    names = {e["name"] for e in entries}
    doc = {"schema": mf.SCHEMA, "source": source, "files": entries}
    if INDEX_NAME in names:
        doc["index"] = INDEX_NAME
    return mf.validate(doc)


def upload(store, files: list, m: dict, log=print) -> dict:
    """Upload files then the manifest. Returns counts of uploaded and skipped files."""
    data = mf.dumps(m).encode()
    current = store.get(mf.MANIFEST_NAME)
    if current is not None and current != data:
        # Hide the old manifest before any blob it names changes.
        store.delete(mf.MANIFEST_NAME)
    paths = dict(files)
    done = {"uploaded": 0, "skipped": 0, "bytes": 0}
    for e in m["files"]:
        if store.sha256(e["name"]) == e["sha256"]:
            done["skipped"] += 1
            log(f"skip   {e['name']} (unchanged)")
            continue
        store.upload_file(e["name"], paths[e["name"]], e["sha256"])
        done["uploaded"] += 1
        done["bytes"] += e["size"]
        log(f"upload {e['name']} ({e['size']} bytes)")
    store.put(mf.MANIFEST_NAME, data)
    log(f"wrote  {mf.MANIFEST_NAME} ({len(m['files'])} files)")
    return done
