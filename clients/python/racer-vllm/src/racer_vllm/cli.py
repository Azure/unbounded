# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""The racer-vllm command.

Subcommands: materialize, import, prepare, warm, inspect. Only prepare
imports vLLM (and through it torch); the others run in the plugin image,
which has neither.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import sys
import time

from . import manifest as mf
from .importer import SnapshotError
from .sharded import artifact_prefix
from .store import StoreError, sha256_file
from .uri import ModelURI, parse_uri

PROG = "racer-vllm"
DEFAULT_PREPARE_DIR = "racer-prepare"


class CommandError(RuntimeError):
    """A command failed for a reason the message explains."""


def _client(uri: ModelURI, socket_path: str | None, timeout: float | None = 60.0):
    import racer

    return racer.Client(cache=uri.cache, socket_path=socket_path, timeout=timeout)


def _read_manifest(client, uri: ModelURI) -> dict:
    import racer

    try:
        data = client.read(uri.request(mf.MANIFEST_NAME))
    except racer.NotFound:
        raise CommandError(
            f"{uri}/{mf.MANIFEST_NAME} not found; import the model with 'racer-vllm import' first"
        ) from None
    return mf.loads(data)


def _write_atomic(path: str, data: bytes) -> None:
    os.makedirs(os.path.dirname(path) or ".", exist_ok=True)
    tmp = path + ".tmp"
    with open(tmp, "wb") as f:
        f.write(data)
    os.replace(tmp, path)


# materialize ------------------------------------------------------------------


def materialize(client, uri: ModelURI, directory: str, out=print) -> dict:
    """Copy the aux files and the manifest into directory, pin every file's ETag.

    racer-source.json is written last, so its presence means the directory is
    complete. Returns the manifest.
    """
    m = _read_manifest(client, uri)
    os.makedirs(directory, exist_ok=True)
    stale = os.path.join(directory, mf.SOURCE_NAME)
    if os.path.exists(stale):
        os.remove(stale)
    etags = {}
    for f in m["files"]:
        req = uri.request(f["name"])
        meta = client.stat(req)
        if meta.size != f["size"]:
            raise mf.ManifestError(
                f"{uri.key(f['name'])} is {meta.size} bytes in Racer but {f['size']} in the manifest; "
                "the origin does not match its manifest, so import the model again"
            )
        etags[f["name"]] = meta.etag
        if f["kind"] != "aux":
            continue
        data = client.read(req, if_match=meta.etag)
        mf.check_sha256(f["name"], data, f["sha256"])
        _write_atomic(os.path.join(directory, f["name"]), data)
        out(f"fetched {f['name']} ({len(data)} bytes)")
    _write_atomic(os.path.join(directory, mf.MANIFEST_NAME), mf.dumps(m).encode())
    _write_atomic(os.path.join(directory, mf.SOURCE_NAME), mf.dumps_source(str(uri), etags).encode())
    weights = sum(1 for f in m["files"] if f["kind"] == "safetensors")
    out(f"materialized {uri} into {directory}: {len(m['files']) - weights} files copied, {weights} weight files pinned")
    return m


def _cmd_materialize(args) -> int:
    uri = args.uri
    with _client(uri, args.socket) as c:
        materialize(c, uri, args.dir)
    return 0


# import -----------------------------------------------------------------------


def _cmd_import(args) -> int:
    from . import importer
    from .store import open_store

    files = importer.scan(args.source)
    source = importer.guess_source(args.source)
    if args.repo is not None:
        source["repo"] = args.repo
    if args.revision is not None:
        source["revision"] = args.revision
    print(f"hashing {len(files)} files from {args.source}")
    m = importer.build_manifest(files, source)
    store = open_store(args.dest, args.max_concurrency)
    done = importer.upload(store, files, m)
    print(f"imported {store.url}: {done['uploaded']} uploaded ({done['bytes']} bytes), {done['skipped']} unchanged")
    return 0


# prepare ----------------------------------------------------------------------


def run_vllm(model_dir: str, extra: dict, vllm_args: list) -> None:
    """Build a vLLM engine for model_dir with --load-format racer, then shut it down.

    Every rank loads its weights, and the loader captures them on the way
    (capture_dir in extra).
    """
    from . import compat, register

    register()
    try:
        from vllm import LLMEngine
        from vllm.engine.arg_utils import EngineArgs
    except ImportError as exc:
        raise compat.VllmMissing(f"racer-vllm prepare needs vLLM: {exc}") from exc
    parser = compat.argument_parser()
    EngineArgs.add_cli_args(parser)
    ns = parser.parse_args(
        ["--model", model_dir, *vllm_args, "--load-format", "racer", "--model-loader-extra-config", json.dumps(extra)]
    )
    engine = LLMEngine.from_engine_args(EngineArgs.from_cli_args(ns))
    shutdown = getattr(engine, "shutdown", None)
    if callable(shutdown):
        shutdown()


def _vllm_version() -> str:
    from . import compat

    try:
        return compat.vllm_version()
    except compat.VllmMissing:
        return ""


def publish_captured(store, capture_dir: str, vllm_version: str = "", out=print) -> list:
    """Upload every captured rank (data first, rank manifest last), then the index. Returns index entries."""
    from .sharded import captured

    found = captured(capture_dir)
    if not found:
        raise CommandError(f"no captured artifacts under {capture_dir}; did the load use --load-format racer?")
    entries = []
    for (fp, role), ranks in sorted(found.items()):
        for rank in sorted(ranks):
            d, doc = ranks[rank]
            prefix = artifact_prefix(fp, role, rank)
            for f in doc["files"]:
                path = os.path.join(d, f["name"])
                store.upload_file(f"{prefix}/{f['name']}", path, sha256_file(path))
            with open(os.path.join(d, mf.SHARDED_NAME), "rb") as fh:
                store.put(f"{prefix}/{mf.SHARDED_NAME}", fh.read())
            out(f"uploaded {prefix}")
        entry = {"fingerprint": fp, "role": role, "world_size": len(ranks)}
        if vllm_version:
            entry["vllm"] = vllm_version
        entries.append(entry)
    old = store.get(mf.SHARDED_INDEX_NAME)
    index = mf.merge_sharded_index(mf.loads_sharded_index(old) if old is not None else None, entries)
    store.put(mf.SHARDED_INDEX_NAME, mf.canonical(index))
    out(f"wrote {mf.SHARDED_INDEX_NAME} ({len(index['artifacts'])} artifacts)")
    return entries


def _data_parallel_size(vllm_args: list) -> int:
    """The --data-parallel-size (-dp) in vllm_args, or 1."""
    size = "1"
    for i, a in enumerate(vllm_args):
        for flag in ("--data-parallel-size", "-dp"):
            if a == flag and i + 1 < len(vllm_args):
                size = vllm_args[i + 1]
            elif a.startswith(flag + "="):
                size = a.split("=", 1)[1]
    try:
        return int(size)
    except ValueError:
        raise CommandError(f"--data-parallel-size {size!r} is not an integer") from None


def prepare(uri: ModelURI, dest: str, workdir: str, vllm_args: list, *, socket_path=None, role="target", runner=None):
    from .store import open_store

    dp = _data_parallel_size(vllm_args)
    if dp > 1:
        raise CommandError(
            f"racer-vllm prepare does not support --data-parallel-size {dp}: sharded artifacts are per tensor and "
            "pipeline rank and every replica loads the same ones, so capture with --data-parallel-size 1"
        )

    store = open_store(dest)  # fail on a bad destination before loading anything
    model_dir = os.path.join(workdir, "model")
    capture_dir = os.path.join(workdir, "capture")
    if os.path.isdir(os.path.join(capture_dir, "sharded")):
        raise CommandError(f"{capture_dir} holds an earlier capture; remove it or pass another --workdir")
    with _client(uri, socket_path) as c:
        materialize(c, uri, model_dir)
    extra = {"uri": str(uri), "capture_dir": capture_dir, "sharded": "off", "role": role}
    if socket_path:
        extra["socket"] = socket_path
    (runner or run_vllm)(model_dir, extra, list(vllm_args))
    return publish_captured(store, capture_dir, _vllm_version())


def _cmd_prepare(args) -> int:
    uri = args.uri
    vllm_args = list(args.vllm_args)
    for bad in ("--load-format", "--model-loader-extra-config", "--model"):
        if any(a == bad or a.startswith(bad + "=") for a in vllm_args):
            raise CommandError(f"{bad} is set by racer-vllm prepare; remove it from the vLLM arguments")
    prepare(uri, args.dest, args.workdir, vllm_args, socket_path=args.socket, role=args.role)
    return 0


# warm -------------------------------------------------------------------------


def warm_id(uri: ModelURI) -> str:
    return "racer-vllm-warm-" + hashlib.sha256(str(uri).encode()).hexdigest()[:16]


def warm(client, uri: ModelURI, *, timeout: float, poll: float = 5.0, priority=None, out=print) -> bool:
    """Prefetch every weight file and wait for the tickets to settle. True when all succeeded."""
    import racer

    m = _read_manifest(client, uri)
    refs = []
    total = 0
    for f in m["files"]:
        if f["kind"] != "safetensors":
            continue
        req = uri.request(f["name"])
        refs.append(racer.PrefetchRef(key=req.hex, metadata=req.metadata))
        total += f["size"]
    if not refs:
        out(f"{uri} has no weight files to warm")
        return True
    parts = racer.split_prefetch(warm_id(uri), racer.PrefetchSpec(refs=tuple(refs), priority=priority))
    for pid, spec in parts:
        client.prefetch(pid, spec)
    out(f"warming {len(refs)} weight files ({total} bytes) in {len(parts)} prefetch tickets")
    deadline = time.monotonic() + timeout
    while True:
        left = deadline - time.monotonic()
        statuses = [client.prefetch_status(pid, wait=max(0.0, min(poll, left)), coverage=True) for pid, _ in parts]
        resident = sum(s.coverage.resident_bytes for s in statuses if s.coverage)
        done = sum(s.refs_succeeded for s in statuses)
        failed = sum(s.refs_failed for s in statuses)
        pct = 100.0 * min(resident, total) / total if total else 100.0
        out(f"resident {resident}/{total} bytes ({pct:.1f}%), refs {done} succeeded, {failed} failed of {len(refs)}")
        if all(s.settled for s in statuses):
            bad = [s for s in statuses if s.state != "SUCCEEDED"]
            for s in bad:
                out(f"prefetch {s.id} ended {s.state}" + (f": {s.reason}" if s.reason else ""))
            return not bad
        if left <= 0:
            out(f"timed out after {timeout:g}s; the prefetch keeps running in the dataplane")
            return False


def _cmd_warm(args) -> int:
    uri = args.uri
    with _client(uri, args.socket) as c:
        ok = warm(c, uri, timeout=args.timeout, poll=args.poll, priority=args.priority)
    if not ok:
        raise CommandError("prefetch did not complete")
    return 0


# inspect ----------------------------------------------------------------------


def inspect(client, uri: ModelURI, out=print) -> dict:
    """Print the manifest summary, sharded artifacts, and per-file status. Returns counts."""
    import racer

    m = _read_manifest(client, uri)
    src = m["source"]
    weights = mf.safetensors_files(m)
    out(f"model     {uri}")
    out(f"source    {src['hub']} {src['repo'] or '-'} @ {src['revision'] or '-'}")
    out(f"manifest  {mf.digest(m)}")
    out(f"files     {len(m['files'])} ({len(weights)} weight files, {sum(len(w.tensors) for w in weights)} tensors)")
    out(f"bytes     {sum(f['size'] for f in m['files'])}")
    try:
        index = mf.loads_sharded_index(client.read(uri.request(mf.SHARDED_INDEX_NAME)))
    except racer.NotFound:
        index = None
    if not index or not index["artifacts"]:
        out("sharded   none")
    for a in (index or {}).get("artifacts", []):
        have = 0
        for rank in range(a["world_size"]):
            try:
                client.stat(uri.child(artifact_prefix(a["fingerprint"], a["role"], rank)).request(mf.SHARDED_NAME))
                have += 1
            except racer.NotFound:
                pass
        out(
            f"sharded   {a['fingerprint']} role={a['role']} ranks {have}/{a['world_size']}"
            + (f" vllm={a['vllm']}" if a.get("vllm") else "")
        )
    counts = {"ok": 0, "missing": 0, "mismatch": 0}
    for f in m["files"]:
        try:
            meta = client.stat(uri.request(f["name"]))
        except racer.NotFound:
            counts["missing"] += 1
            out(f"missing   {f['name']}")
            continue
        if meta.size != f["size"]:
            counts["mismatch"] += 1
            out(f"mismatch  {f['name']} ({meta.size} bytes, manifest {f['size']})")
        else:
            counts["ok"] += 1
            out(f"ok        {f['name']} {meta.size} {meta.etag}")
    out(f"status    {counts['ok']} ok, {counts['missing']} missing, {counts['mismatch']} size mismatch")
    return counts


def _cmd_inspect(args) -> int:
    uri = args.uri
    with _client(uri, args.socket) as c:
        counts = inspect(c, uri)
    return 0 if counts["missing"] == 0 and counts["mismatch"] == 0 else 1


# Parser -----------------------------------------------------------------------


def _uri_arg(value: str) -> ModelURI:
    try:
        return parse_uri(value)
    except ValueError as exc:
        raise argparse.ArgumentTypeError(str(exc)) from None


def parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(prog=PROG, description="Serve vLLM models whose weights live in a Racer cache.")
    sub = p.add_subparsers(dest="command", metavar="COMMAND", required=True)

    def socket_arg(sp):
        sp.add_argument("--socket", help="Racer client socket (default: RACER_SOCKET, then the cache's socket)")

    sp = sub.add_parser(
        "materialize",
        help="copy config, tokenizer and other small files plus the manifest into a directory",
        description="Copy every aux file of the model and racer-manifest.json into DIR, and pin each "
        "file's ETag in racer-source.json, so 'vllm serve DIR --load-format racer' needs nothing else.",
    )
    sp.add_argument("uri", metavar="RACER_URI", type=_uri_arg, help="racer://<cache>/<bucket>/<prefix>")
    sp.add_argument("dir", metavar="DIR", help="local directory to serve from")
    socket_arg(sp)
    sp.set_defaults(func=_cmd_materialize)

    sp = sub.add_parser(
        "import",
        help="upload a Hugging Face snapshot and write its manifest last",
        description="Upload every file of a snapshot (following links into ../../blobs), skipping blobs whose "
        "sha256 is unchanged, then write racer-manifest.json. Azure credentials come from "
        "DefaultAzureCredential (workload identity: AZURE_CLIENT_ID, AZURE_TENANT_ID, "
        "AZURE_FEDERATED_TOKEN_FILE). Needs racer-vllm[azure] for https destinations.",
    )
    sp.add_argument("--source", required=True, metavar="DIR", help="snapshot directory")
    sp.add_argument(
        "--dest",
        required=True,
        metavar="URL",
        help="https://<account>.blob.core.windows.net/<container>[/<prefix>], or file:///path",
    )
    sp.add_argument("--repo", help="source repo recorded in the manifest (default: from the cache path)")
    sp.add_argument("--revision", help="source revision recorded in the manifest (default: the snapshot name)")
    sp.add_argument("--max-concurrency", type=int, default=8, help="parallel connections per blob (default 8)")
    sp.set_defaults(func=_cmd_import)

    sp = sub.add_parser(
        "prepare",
        help="capture per-rank sharded artifacts with vLLM and upload them",
        description="Load the model once with vLLM and the given arguments, capture each rank's weights after "
        "load_weights and before process_weights_after_loading, upload them under "
        "sharded/<fingerprint>/ at DEST, and write racer-sharded-index.json last. DEST is the "
        "model's own location in the origin, as given to 'import --dest'. Pass vLLM engine arguments "
        "after '--', for example: -- --tensor-parallel-size 8 --enable-expert-parallel. Needs vLLM and GPUs.",
    )
    sp.add_argument("uri", metavar="RACER_URI", type=_uri_arg, help="racer://<cache>/<bucket>/<prefix>")
    sp.add_argument("--dest", required=True, metavar="URL", help="the model's origin location (as for import)")
    sp.add_argument("--workdir", default=DEFAULT_PREPARE_DIR, help=f"local scratch directory (default ./{DEFAULT_PREPARE_DIR})")
    sp.add_argument("--role", choices=("target", "draft"), default="target", help="model role (default target)")
    socket_arg(sp)
    sp.set_defaults(func=_cmd_prepare)

    sp = sub.add_parser(
        "warm",
        help="prefetch every weight file into the cache and wait",
        description="Prefetch every weight file through the dataplane and wait until the prefetch settles.",
    )
    sp.add_argument("uri", metavar="RACER_URI", type=_uri_arg, help="racer://<cache>/<bucket>/<prefix>")
    sp.add_argument("--timeout", type=float, default=3600.0, help="seconds to wait (default 3600)")
    sp.add_argument("--poll", type=float, default=5.0, help="seconds between progress lines (default 5)")
    sp.add_argument("--priority", choices=("high", "normal", "low"), help="prefetch priority")
    socket_arg(sp)
    sp.set_defaults(func=_cmd_warm)

    sp = sub.add_parser(
        "inspect",
        help="print the manifest, sharded artifacts and per-file status",
        description="Print the manifest summary, the sharded artifacts listed in racer-sharded-index.json, and "
        "whether the dataplane serves each file at the manifest's size. Exits 1 if a file is missing.",
    )
    sp.add_argument("uri", metavar="RACER_URI", type=_uri_arg, help="racer://<cache>/<bucket>/<prefix>")
    socket_arg(sp)
    sp.set_defaults(func=_cmd_inspect)
    return p


def main(argv=None) -> int:
    import racer

    from .compat import VllmMissing

    argv = list(sys.argv[1:] if argv is None else argv)
    extra = []
    if "--" in argv:
        i = argv.index("--")
        argv, extra = argv[:i], argv[i + 1 :]
    p = parser()
    try:
        args = p.parse_args(argv)
    except SystemExit as exc:  # --help and usage errors
        return exc.code if isinstance(exc.code, int) else 2
    if extra and args.command != "prepare":
        print(f"{PROG} {args.command}: error: only prepare takes arguments after '--'", file=sys.stderr)
        return 2
    args.vllm_args = extra
    if args.command == "warm" and (args.timeout <= 0 or args.poll <= 0):
        print(f"{PROG} warm: error: --timeout and --poll must be positive", file=sys.stderr)
        return 2
    if args.command == "import" and args.max_concurrency < 1:
        print(f"{PROG} import: error: --max-concurrency must be at least 1", file=sys.stderr)
        return 2
    try:
        return args.func(args)
    except (CommandError, mf.ManifestError, racer.RacerError, StoreError, SnapshotError, VllmMissing, OSError, ValueError) as exc:
        print(f"{PROG} {args.command}: error: {exc}", file=sys.stderr)
        return 1


def fetch_main(argv=None) -> int:
    """racer-vllm-fetch RACER_URI DIR: deprecated alias of 'racer-vllm materialize'."""
    argv = list(sys.argv[1:] if argv is None else argv)
    print("racer-vllm-fetch is deprecated; use 'racer-vllm materialize'", file=sys.stderr)
    if argv and argv[0] in ("-h", "--help"):
        return main(["materialize", "--help"])
    return main(["materialize", *argv])
