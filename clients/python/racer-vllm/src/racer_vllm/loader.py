# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""vLLM model loader for --load-format racer.

Targets vLLM v0.25.x. The loader interface used is
vllm/model_executor/model_loader/base_loader.py (BaseModelLoader with
download_model(model_config) and load_weights(model, model_config)), and
registration uses register_model_loader from
vllm/model_executor/model_loader/__init__.py. Recent main adds optional hooks
(get_external_weight_memory, create_model) with defaults, so this loader
works there too.
"""

from __future__ import annotations

import logging

import racer

from . import sharded
from .model import client_for, resolve_pins, resolve_uri, weight_files
from .weights import DEFAULT_DEPTH, DEFAULT_STAGING_BYTES, LoadStats, TorchBackend, iter_weights

# Under "vllm." so the summary goes through vLLM's log handler and format.
log = logging.getLogger("vllm.racer_vllm.loader")

LOAD_FORMAT = "racer"
_registered = False


def _settings(extra: dict) -> dict:
    return {
        "staging_bytes": int(extra.get("staging_bytes", DEFAULT_STAGING_BYTES)),
        "concurrency": int(extra.get("concurrency", DEFAULT_DEPTH)),
        "clone": bool(extra.get("clone", True)),
        "socket": extra.get("socket"),
        "use_vmm": bool(extra.get("use_vmm", True)),
        "mode_retry_seconds": _retry(extra.get("mode_retry_seconds", racer.DEFAULT_MODE_RETRY_SECONDS)),
        "sharded_state": bool(extra.get("sharded_state", False)),
        "pattern": extra.get("pattern"),
    }


def _retry(v):
    return None if v is None else float(v)


def make_loader_class(base):
    class RacerModelLoader(base):
        """Loads safetensors weights from a Racer cache.

        model_loader_extra_config keys:
          uri            racer://<cache>/<bucket>/<prefix>; optional when the
                         model directory has racer-model.json (racer-vllm-fetch)
          staging_bytes  size of each GPU staging buffer (default 256 MiB)
          concurrency    number of staging buffers and reads in flight (default 4)
          clone          copy each tensor out of staging (default true)
          socket         client socket path (else RACER_SOCKET, else the cache)
          use_vmm        allow exportable cuMemCreate staging (default true)
          mode_retry_seconds  retry a delivery mode the dataplane refused
                         after this many seconds (default 300; null: never)
          sharded_state  read a vLLM sharded_state checkpoint, where each
                         rank reads only its own files (default false)
          pattern        sharded_state file name pattern (default as vLLM)
        """

        def __init__(self, load_config):
            super().__init__(load_config)
            self.extra = dict(getattr(load_config, "model_loader_extra_config", None) or {})
            self.settings = _settings(self.extra)

        def _client(self, model_config):
            uri = resolve_uri(str(model_config.model), self.extra)
            client = client_for(
                uri,
                self.settings["socket"],
                staging_bytes=self.settings["staging_bytes"],
                mode_retry_seconds=self.settings["mode_retry_seconds"],
            )
            return uri, client

        def _files(self, client, uri, model_config):
            s = self.settings
            if s["sharded_state"]:
                return sharded.rank_files(client, uri, s["pattern"], sharded.vllm_ranks())
            return weight_files(client, uri, resolve_pins(str(model_config.model), uri))

        def download_model(self, model_config) -> None:
            # Nothing to download; check the weights are reachable.
            uri, client = self._client(model_config)
            with client:
                self._files(client, uri, model_config)

        def load_weights(self, model, model_config) -> None:
            uri, client = self._client(model_config)
            s = self.settings
            stats = LoadStats()
            pins = resolve_pins(str(model_config.model), uri)
            with client:
                files = self._files(client, uri, model_config)
                backend = TorchBackend(clone=s["clone"], use_vmm=s["use_vmm"])
                weights = iter_weights(
                    client, uri, backend, s["staging_bytes"], s["concurrency"], files=files, pins=pins, stats=stats
                )
                try:
                    if s["sharded_state"]:
                        sharded.load_state(sharded.filter_subtensors(model.state_dict()), weights)
                        loaded = None
                    else:
                        loaded = model.load_weights(weights)
                finally:
                    # Run the generator's cleanup (executor shutdown and GPU
                    # fences) before the client closes, even if load_weights
                    # raised or returned without exhausting the iterator.
                    weights.close()
                    stats.finish()
            log.info("%s", stats.summary(str(uri)))
            if loaded is not None and getattr(model_config, "quantization", None) is None:
                missing = {n for n, _ in model.named_parameters()} - set(loaded)
                if missing:
                    raise ValueError(f"racer loader: weights not loaded: {sorted(missing)[:10]}")

    return RacerModelLoader


def register() -> None:
    """vllm.general_plugins entry point. Safe to call more than once."""
    global _registered
    if _registered:
        return
    from vllm.model_executor.model_loader import register_model_loader
    from vllm.model_executor.model_loader.base_loader import BaseModelLoader

    register_model_loader(LOAD_FORMAT)(make_loader_class(BaseModelLoader))
    _registered = True
