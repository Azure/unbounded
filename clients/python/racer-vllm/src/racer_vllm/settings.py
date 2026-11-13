# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Loader options from ``--model-loader-extra-config``."""

from __future__ import annotations

from dataclasses import dataclass

DEFAULT_STAGING_BYTES = 256 * 1024 * 1024
DEFAULT_MAX_STAGING_BYTES = 4 * 1024 * 1024 * 1024
DEFAULT_CONCURRENCY = 4

CHOICES = {
    "sharded": ("auto", "require", "off"),
    "delivery": ("auto", "gpu", "host"),
    "clone": ("auto", "always", "never"),
    "verify": ("none", "size", "full"),
    "role": ("target", "draft"),
}
REMOVED = {
    "use_vmm": "delivery selection is automatic; set delivery to 'host' to avoid exportable GPU memory",
    "pattern": "sharded_state patterns are gone; use 'racer-vllm prepare' and sharded='auto'",
    "mode_retry_seconds": "delivery modes are probed per process; restart to probe again",
    "sharded_state": "use sharded='auto' or 'require' with an artifact from 'racer-vllm prepare'",
}


@dataclass(frozen=True)
class Settings:
    uri: str | None = None
    sharded: str = "auto"
    delivery: str = "auto"
    staging_bytes: int = DEFAULT_STAGING_BYTES
    max_staging_bytes: int = DEFAULT_MAX_STAGING_BYTES
    concurrency: int = DEFAULT_CONCURRENCY
    clone: str = "auto"
    verify: str = "size"
    capture_dir: str | None = None
    socket: str | None = None
    role: str = "target"


def _int(key: str, v) -> int:
    if isinstance(v, bool):
        raise ValueError(f"racer loader: {key} must be an integer, got {v!r}")
    try:
        n = int(v)
    except (TypeError, ValueError):
        raise ValueError(f"racer loader: {key} must be an integer, got {v!r}") from None
    if n <= 0:
        raise ValueError(f"racer loader: {key} must be positive, got {n}")
    return n


def parse(extra: dict | None) -> Settings:
    """Validate extra config. Unknown or removed keys raise ValueError."""
    extra = dict(extra or {})
    known = set(Settings.__dataclass_fields__)
    for key in extra:
        if key in REMOVED:
            raise ValueError(f"racer loader: option {key!r} was removed: {REMOVED[key]}")
        if key not in known:
            raise ValueError(f"racer loader: unknown option {key!r}; known options: {', '.join(sorted(known))}")
    for key, allowed in CHOICES.items():
        if key in extra and extra[key] not in allowed:
            raise ValueError(f"racer loader: {key} must be one of {', '.join(allowed)}, got {extra[key]!r}")
    for key in ("staging_bytes", "max_staging_bytes", "concurrency"):
        if key in extra:
            extra[key] = _int(key, extra[key])
    for key in ("uri", "capture_dir", "socket"):
        if extra.get(key) is not None and not isinstance(extra[key], str):
            raise ValueError(f"racer loader: {key} must be a string")
    s = Settings(**extra)
    if s.max_staging_bytes < s.staging_bytes:
        raise ValueError("racer loader: max_staging_bytes must be at least staging_bytes")
    return s
