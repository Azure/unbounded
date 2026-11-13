# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""RacerModelLoader: vLLM's DefaultModelLoader reading weights from Racer.

The class is built on first use from vLLM's DefaultModelLoader (``build``),
so everything vLLM does around the weight iterator (secondary weights,
prefixes, weight-dict filters, missing-weight checks) still applies. Only the
file listing, the byte source, and the optional sharded fast path change.
"""

from __future__ import annotations

import json
import logging
import os
import time

from . import compat, filters, reader, settings
from . import manifest as mf
from . import sharded as sh
from .planner import plan
from .uri import SCHEME, ModelURI, parse_uri

log = logging.getLogger("vllm.racer_vllm.loader")

LOAD_FORMAT = "racer"
CAPTURE_EXIT_ENV = "RACER_VLLM_CAPTURE_EXIT"


class CaptureComplete(SystemExit):
    """Raised after a capture with RACER_VLLM_CAPTURE_EXIT=1, so no KV cache is allocated."""

    def __init__(self, path: str = "") -> None:
        super().__init__(0)
        self.path = path


def summary(role: str, rank: int, stats: dict, seconds: float) -> str:
    """The one line logged after each load."""
    total = sum(stats["bytes"].values())
    rate = total / seconds / 1e6 if seconds > 0 else 0.0
    modes = " ".join(f"{m}={stats['reads'][m]}" for m in reader.MODES)
    return (
        f"racer: loaded role={role} rank={rank} files={stats['files']} bytes={total} "
        f"seconds={seconds:.2f} throughput={rate:.1f}MB/s reads {modes}"
    )


def _client_factory(cache: str, socket_path: str | None):
    import racer

    return racer.Client(cache=cache, socket_path=socket_path)


def _allocator_factory(mode: str):
    device = 0
    try:
        import torch

        if torch.cuda.is_available():
            device = torch.cuda.current_device()
    except ImportError:
        pass
    return reader.GpuAllocator(device) if mode == reader.GPU else reader.HostAllocator(device)


class RacerLoaderMixin:
    """Overrides on top of DefaultModelLoader. See build()."""

    client_factory = staticmethod(_client_factory)
    allocator_factory = staticmethod(_allocator_factory)
    event_factory = None

    def _racer_init(self, load_config) -> None:
        self.racer = settings.parse(getattr(load_config, "model_loader_extra_config", None) or {})
        if self.racer.verify == "full" and self.racer.delivery == "gpu":
            raise ValueError(
                "racer loader: verify=full hashes data in host memory, so it cannot use delivery=gpu; "
                "set delivery to host or auto, or verify to size"
            )
        self._racer_model = None
        self._racer_stats = reader.new_stats()
        self._racer_clients: dict = {}
        self._racer_readers: dict = {}
        self._racer_allocators: dict = {}
        self._racer_stat_cache: dict = {}
        self._racer_manifests: dict = {}
        self._racer_sources: dict = {}
        self._racer_primary = None
        self._racer_gap_note = False

    # Locations ---------------------------------------------------------------

    def _uri_for(self, path: str) -> ModelURI:
        if self.racer.uri and (self._racer_primary is None or path == self._racer_primary):
            return parse_uri(self.racer.uri)
        if path.startswith(SCHEME):
            return parse_uri(path)
        src = self._source_for(path)
        if src is not None:
            return parse_uri(src["uri"])
        raise ValueError(
            f"racer loader: no model URI for {path!r}; set model_loader_extra_config.uri or run "
            "'racer-vllm materialize <racer-uri> <dir>' and serve that directory"
        )

    def _source_for(self, path: str):
        """The racer-source.json that materialize wrote in path, or None."""
        if path.startswith(SCHEME):
            return None
        if path not in self._racer_sources:
            self._racer_sources[path] = mf.load_source(path)
        return self._racer_sources[path]

    def _pin(self, path: str, name: str):
        src = self._source_for(path)
        return None if src is None else src["etags"].get(name)

    def _manifest_for(self, path: str) -> dict:
        if path in self._racer_manifests:
            return self._racer_manifests[path]
        if path.startswith(SCHEME):
            uri = parse_uri(path)
            client = self._client(uri)
            m = mf.loads(client.read(uri.request(mf.MANIFEST_NAME)))
        else:
            m = mf.load(os.path.join(path, mf.MANIFEST_NAME))
        self._racer_manifests[path] = m
        return m

    def _client(self, uri: ModelURI):
        c = self._racer_clients.get(uri.cache)
        if c is None:
            c = self.client_factory(uri.cache, self.racer.socket)
            self._racer_clients[uri.cache] = c
        return c

    def _reader(self, client):
        r = self._racer_readers.get(id(client))
        if r is not None:
            return r
        allocs = self._racer_allocators

        def make(mode):
            key = reader.GPU if mode == reader.GPU else reader.HOST
            if key not in allocs:
                allocs[key] = self.allocator_factory(key)
            allocs[mode] = allocs[key]
            return allocs[mode]

        # verify=full hashes every byte on the host, so it never takes the GPU path.
        delivery = "host" if self.racer.verify == "full" else self.racer.delivery
        modes = reader.probe(client, delivery, make)
        pool = reader.StagingPool(
            self.racer.staging_bytes,
            self.racer.max_staging_bytes,
            self.racer.concurrency,
            allocs[modes[0]],
            self.racer.clone,
            self.event_factory,
        )
        r = reader.WeightReader(client, pool, modes, dict(allocs), self._racer_stats)
        self._racer_readers[id(client)] = r
        return r

    def _etag(self, client, uri: ModelURI, entry: dict, pin=None):
        """ETag for if_match.

        pin is the ETag materialize recorded. With a pin, verify=full and
        verify=none use it as is (full checks sha256 as well). Otherwise the
        file is stat'ed, at the pinned version when there is one, and verify=size
        and verify=full check its size.
        """
        if pin and self.racer.verify in ("full", "none"):
            return pin
        key = (uri.cache, uri.bucket, uri.key(entry["name"]))
        meta = self._racer_stat_cache.get(key)
        if meta is None:
            import racer

            try:
                meta = client.stat(uri.request(entry["name"]), if_match=pin)
            except racer.VersionUnavailable:
                raise mf.ManifestError(
                    f"racer: {uri.key(entry['name'])} changed since materialize recorded ETag {pin}; "
                    "run 'racer-vllm materialize' again"
                ) from None
            self._racer_stat_cache[key] = meta
        if self.racer.verify in ("size", "full") and meta.size != entry["size"]:
            raise mf.ManifestError(
                f"racer: {uri.key(entry['name'])} is {meta.size} bytes in Racer but {entry['size']} in the "
                "manifest; the model changed since materialize, so run 'racer-vllm materialize' again"
            )
        return meta.etag

    def _weight_names(self, path: str, m: dict) -> list:
        names = [f.name for f in mf.safetensors_files(m)]
        index = m.get("index")
        if index and not path.startswith(SCHEME):
            p = os.path.join(path, index)
            if os.path.isfile(p):
                with open(p) as f:
                    wm = (json.load(f).get("weight_map") or {}).values()
                wanted = set(wm)
                if wanted:
                    names = [n for n in names if n in wanted]
        return names

    # vLLM overrides ----------------------------------------------------------

    def download_model(self, model_config, model=None):
        path = str(model_config.model)
        if not path.startswith(SCHEME) and not os.path.isfile(os.path.join(path, mf.MANIFEST_NAME)):
            raise FileNotFoundError(
                f"racer loader: {os.path.join(path, mf.MANIFEST_NAME)} is missing; run "
                f"'racer-vllm materialize <racer-uri> {path}' first"
            )
        return path

    def _prepare_weights(self, model_name_or_path, *args, **kwargs):
        """(folder, weight files, use_safetensors), plus the index file where vLLM expects four."""
        path = str(model_name_or_path)
        m = self._manifest_for(path)
        out = (path, self._weight_names(path, m), True)
        if compat.prepare_weights_arity(type(self)) == 4:
            out += (m.get("index") or "",)
        return out

    def get_all_weights(self, model_config, model, *args, **kwargs):
        self._racer_model = model
        if self._racer_primary is None:
            self._racer_primary = str(model_config.model)
        yield from super().get_all_weights(model_config, model, *args, **kwargs)

    def _get_weights_iterator(self, source):
        path = str(source.model_or_path)
        m = self._manifest_for(path)
        uri = self._uri_for(path)
        client = self._client(uri)
        rd = self._reader(client)
        keep = filters.keep_predicate(
            self._racer_model,
            self.racer.role,
            compat.source_is_unused(source),
            getattr(self, "local_expert_ids", None),
        )
        prefix = getattr(source, "prefix", "") or ""
        files = {f.name: f for f in mf.safetensors_files(m)}
        jobs, hashers, gaps = [], {}, []
        for name in self._encoder_only_filter(path, m, self._weight_names(path, m)):
            sf = files[name]
            entry = mf.file_entry(m, name)
            etag = self._etag(client, uri, entry, self._pin(path, name))
            req = uri.request(name)
            reads = plan(name, sf.tensors, keep, self.racer.staging_bytes, file_size=sf.size)
            if self.racer.verify == "full":
                hashers[name] = self._full_hasher(client, req, sf, reads, etag)
                if hashers[name].gaps:
                    gaps.append(name)
            jobs += [(req, r, etag) for r in reads]
            self._racer_stats["files"] += 1

        if gaps and not self._racer_gap_note:
            self._racer_gap_note = True
            log.warning(
                "racer: verify=full reads skipped byte ranges of %s only to hash them; "
                "these files cost more than the weights this rank loads",
                ", ".join(gaps),
            )

        def on_data(read, mv):
            h = hashers.get(read.file)
            if h is not None:
                h(read, mv)

        for name, tensor in rd.iter(jobs, on_data if hashers else None):
            yield prefix + name, tensor
        for h in hashers.values():
            h.finish()

    def _encoder_only_filter(self, path: str, m: dict, names: list) -> list:
        """vLLM's --mm-encoder-only shard filter, applied to the manifest's file list.

        vLLM's filter reads the index from a folder. For a racer:// model the
        index is read from Racer (at the ETag its stat returned) into a
        temporary folder first, at the same relative path it has in the model.
        """
        prefixes = getattr(self, "_encoder_only_lm_prefixes", None)
        index = m.get("index")
        if prefixes is None or not index:
            return names
        fn = compat.mm_encoder_only_filter()
        if fn is None:
            return names
        mapper = getattr(self, "_encoder_only_weights_mapper", None)

        def run(folder):
            # vLLM matches files by os.path.relpath(file, folder) against the
            # index weight_map, so pass absolute paths inside the folder and
            # map the kept ones back to manifest names.
            folder = os.path.abspath(folder)
            full = {os.path.join(folder, n): n for n in names}
            kept = fn(list(full), folder, index, prefixes, weights_mapper=mapper)
            return [full.get(k) or os.path.relpath(k, folder) for k in kept]

        if path.startswith(SCHEME):
            import tempfile

            uri = parse_uri(path)
            client = self._client(uri)
            req = uri.request(index)
            meta = client.stat(req)
            data = client.read(req, if_match=meta.etag)
            with tempfile.TemporaryDirectory(prefix="racer-vllm-index-") as folder:
                p = os.path.join(folder, index)
                os.makedirs(os.path.dirname(p), exist_ok=True)
                with open(p, "wb") as f:
                    f.write(data)
                kept = run(folder)
        else:
            kept = run(path)
        if not kept:
            raise RuntimeError(f"racer loader: the mm-encoder-only filter removed every weight file of {path}")
        return list(kept)

    def _full_hasher(self, client, req, sf, reads, etag=None):
        """An on_data callback hashing the whole file; finish() checks the sha256.

        Bytes the reads skip (filtered tensors, the header, a tail) are read
        from Racer when the hasher reaches them, so the hash always covers the
        file. The callback's gaps attribute is True when that happens.
        """
        import hashlib

        h = hashlib.sha256()
        state = {"pos": 0, "done": False}

        def fill(upto):
            if upto > state["pos"]:
                h.update(client.read(req, state["pos"], upto - state["pos"], if_match=etag))
                state["pos"] = upto

        def update(read, mv):
            fill(read.offset)
            skip = state["pos"] - read.offset
            if skip < read.length:
                h.update(mv[skip : read.length])
                state["pos"] = read.end

        def finish():
            if state["done"]:
                return
            state["done"] = True
            fill(sf.size)
            got = h.hexdigest()
            if state["pos"] != sf.size or got != sf.sha256:
                raise mf.ManifestError(f"{sf.name}: sha256 mismatch: manifest has {sf.sha256}, data has {got}")

        data_start = 8 + sf.header_bytes
        pos, gaps = data_start, False
        for r in reads:
            gaps = gaps or r.offset > pos
            pos = max(pos, r.end)
        update.gaps = gaps or pos < sf.size
        update.finish = finish
        return update

    def _vllm_version(self) -> str:
        try:
            return compat.vllm_version()
        except compat.VllmMissing:
            return "unknown"

    def _fingerprint(self, model, model_config) -> tuple:
        path = str(model_config.model)
        digest = mf.digest(self._manifest_for(path))
        par = sh.parallel_info(getattr(model_config, "parallel_config", None))
        fp = sh.fingerprint(
            model, self.racer.role, digest, self._vllm_version(), model_config=model_config, parallel=par
        )
        return fp, digest, par

    def load_weights(self, model, model_config):
        """Load from a matching sharded artifact or the checkpoint, then capture if asked.

        vLLM's load_model calls this between creating the model and
        process_weights_after_loading, so a capture here holds the weights as
        the model's weight loaders left them, before any kernel repacking.
        """
        started = time.monotonic()
        self._racer_model = model
        self._racer_primary = str(model_config.model)
        rank = None
        try:
            done = False
            if self.racer.sharded != "off" and not self.racer.capture_dir:
                fp, _, par = self._fingerprint(model, model_config)
                why = sh.rank_problem(par)
                if why is None:
                    rank = par["rank"]
                    uri = self._uri_for(self._racer_primary)
                    client = self._client(uri)
                    done = sh.load_sharded(
                        client,
                        uri,
                        model,
                        rank,
                        self.racer.role,
                        fp,
                        lambda: self._reader(client),
                        require=self.racer.sharded == "require",
                    )
                elif self.racer.sharded == "require":
                    raise sh.ShardedMismatch(f"racer: cannot pick a sharded artifact: {why}")
                else:
                    log.warning("racer: not using sharded artifacts: %s; loading from the checkpoint", why)
            if not done:
                super().load_weights(model, model_config)
        finally:
            for r in self._racer_readers.values():
                r.pool.close()
            self._racer_readers.clear()
            for c in self._racer_clients.values():
                close = getattr(c, "close", None)
                if close is not None:
                    close()
            self._racer_clients.clear()
        log.info("%s", summary(self.racer.role, rank, self._racer_stats, time.monotonic() - started))
        if self.racer.capture_dir:
            self._capture(model, model_config)

    def _capture(self, model, model_config) -> None:
        fp, digest, par = self._fingerprint(model, model_config)
        why = sh.rank_problem(par)
        if why is None and par.get("dp", 1) > 1:
            why = f"data parallel size is {par['dp']}; capture with one replica (artifacts are per tp/pp rank)"
        if why is not None:
            raise RuntimeError(f"racer: cannot capture a sharded artifact: {why}")
        path = sh.capture(
            model,
            self.racer.role,
            self.racer.capture_dir,
            fingerprint=fp,
            rank=par["rank"],
            world_size=par["world_size"],
            source_manifest_sha256=digest,
        )
        log.info("racer: captured %s", path)
        if os.environ.get(CAPTURE_EXIT_ENV) == "1":
            raise CaptureComplete(path)


_built: dict = {}


def build(base):
    """Return RacerModelLoader as a subclass of base (vLLM's DefaultModelLoader)."""
    cls = _built.get(base)
    if cls is not None:
        return cls

    def __init__(self, load_config, *args, **kwargs):
        # DefaultModelLoader rejects extra config keys outside its own small
        # allowlist, so hide the Racer settings from it while it initializes.
        extra = getattr(load_config, "model_loader_extra_config", None)
        load_config.model_loader_extra_config = {}
        try:
            base.__init__(self, load_config, *args, **kwargs)
        finally:
            load_config.model_loader_extra_config = extra
        self._racer_init(load_config)

    cls = type("RacerModelLoader", (RacerLoaderMixin, base), {"__init__": __init__, "__module__": __name__})
    _built[base] = cls
    return cls


def loader_class():
    return build(compat.default_model_loader())
