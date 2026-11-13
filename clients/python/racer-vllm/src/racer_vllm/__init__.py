# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""vLLM plugin that loads model weights from a Racer cache.

Adds the load format "racer" (vLLM v0.30 and v0.33) and the racer-vllm
command. vLLM and torch come from the host image and are imported lazily.
"""

from __future__ import annotations

from .loader import LOAD_FORMAT, CaptureComplete
from .uri import ModelURI, parse_uri

__all__ = ["LOAD_FORMAT", "CaptureComplete", "ModelURI", "RacerModelLoader", "fetch_main", "main", "parse_uri", "register"]

_registered = False


def register() -> None:
    """vllm.general_plugins entry point: add --load-format racer. Safe to call twice."""
    global _registered
    if _registered:
        return
    from . import compat, loader

    compat.register_model_loader()(LOAD_FORMAT)(loader.loader_class())
    _registered = True


def main(argv=None) -> int:
    from .cli import main as cli_main

    return cli_main(argv)


def fetch_main(argv=None) -> int:
    from .cli import fetch_main as cli_fetch_main

    return cli_fetch_main(argv)


def __getattr__(name: str):
    if name == "RacerModelLoader":
        from .loader import loader_class

        return loader_class()
    raise AttributeError(name)
