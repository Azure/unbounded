# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Prefetch tickets on the Racer client socket.

The wire format follows designs/racer-prefetch.md and matches the Go SDK in
pkg/racersdk/prefetch.go and pkg/racersdk/internal/wire/prefetch.go.
"""

from __future__ import annotations

import json
import math
import re
import threading
from dataclasses import dataclass, field, replace
from datetime import timedelta
from typing import TYPE_CHECKING, Any, Sequence
from urllib.parse import urlencode

from . import _wire as wire
from .errors import RacerError

if TYPE_CHECKING:  # pragma: no cover
    from .client import Client

PREFETCH_PATH = "/v1/prefetches"
MAX_COUNTER = (1 << 53) - 1
DEFAULT_MAX_REFS = 16384
DEFAULT_MAX_BODY_BYTES = 4194304
MAX_RESPONSE_BYTES = 16 << 20

_ID = re.compile(r"[A-Za-z0-9._-]{1,128}")
_KEY = re.compile(r"[0-9a-f]{64}")
_PRIORITIES = ("high", "normal", "low")

# Ticket states.
QUEUED = "QUEUED"
RUNNING = "RUNNING"
SUCCEEDED = "SUCCEEDED"
PARTIAL = "PARTIAL"
FAILED = "FAILED"
CANCELLED = "CANCELLED"


def validate_id(prefetch_id: str) -> None:
    """Check [A-Za-z0-9._-]{1,128}, excluding "." and ".."."""
    if not isinstance(prefetch_id, str) or not _ID.fullmatch(prefetch_id) or prefetch_id in (".", ".."):
        raise RacerError("invalid prefetch id")


@dataclass(frozen=True)
class PrefetchRef:
    """One object range. length 0 means to the end of the object."""

    key: str
    etag: str | None = None
    offset: int = 0
    length: int = 0
    metadata: str | None = None

    def to_wire(self) -> dict[str, Any]:
        if not isinstance(self.key, str) or not _KEY.fullmatch(self.key):
            raise RacerError("invalid key")
        out: dict[str, Any] = {"key": self.key}
        if self.etag is not None:
            wire.validate_etag(self.etag)
            out["etag"] = self.etag
        if not _uint(self.offset) or not _uint(self.length) or self.offset + self.length > wire.MAX_WIRE_INTEGER:
            raise RacerError("invalid range")
        if self.offset:
            out["offset"] = self.offset
        if self.length:
            out["length"] = self.length
        if self.metadata is not None:
            wire.validate_opaque(self.metadata)
            out["metadata"] = self.metadata
        return out


@dataclass(frozen=True)
class PrefetchSpec:
    """A ticket. ttl_seconds 0 and max_pending_bytes 0 use server defaults."""

    refs: Sequence[PrefetchRef] = ()
    cache_uid: str | None = None
    priority: str | None = None
    generation: int = 0
    sequence: int = 0
    ttl_seconds: int = 0
    max_pending_bytes: int = 0
    authorization: str | None = None

    def to_wire(self) -> dict[str, Any]:
        """Return the PUT body fields in canonical order."""
        out: dict[str, Any] = {}
        if self.cache_uid:
            out["cache_uid"] = self.cache_uid
        if self.priority:
            if self.priority not in _PRIORITIES:
                raise RacerError("invalid priority")
            out["priority"] = self.priority
        for name in ("generation", "sequence"):
            value = getattr(self, name)
            if not _uint(value) or value > MAX_COUNTER:
                raise RacerError(f"invalid {name}")
            out[name] = value
        if not _uint(self.ttl_seconds) or not _uint(self.max_pending_bytes):
            raise RacerError("invalid ttl_seconds or max_pending_bytes")
        if self.ttl_seconds:
            out["ttl_seconds"] = self.ttl_seconds
        if self.max_pending_bytes:
            out["max_pending_bytes"] = self.max_pending_bytes
        if not self.refs:
            raise RacerError("refs must not be empty")
        out["refs"] = [r.to_wire() for r in self.refs]
        if self.authorization:
            wire.validate_opaque(self.authorization)
        return out


def _uint(v: Any) -> bool:
    return isinstance(v, int) and not isinstance(v, bool) and v >= 0


def encode_body(spec: PrefetchSpec) -> bytes:
    return _dumps(spec.to_wire())


def _dumps(obj: Any) -> bytes:
    return json.dumps(obj, separators=(",", ":"), ensure_ascii=False).encode()


@dataclass(frozen=True)
class PrefetchBytes:
    logical_requested: int = 0
    aligned_requested: int = 0
    already_resident: int = 0
    queued: int = 0
    in_flight: int = 0
    joined: int = 0
    fetched_from_origin: int = 0
    admitted: int = 0
    failed: int = 0


@dataclass(frozen=True)
class PrefetchCoverage:
    resident_bytes: int = 0
    observed_at_ms: int = 0


@dataclass(frozen=True)
class PrefetchRefStatus:
    index: int
    key: str
    etag: str | None
    offset: int
    length: int
    outcome: str
    error: str | None = None


@dataclass(frozen=True)
class PrefetchStatus:
    """A ticket status. List summaries leave counters and refs at defaults."""

    id: str
    state: str
    cache_uid: str | None = None
    reason: str | None = None
    priority: str | None = None
    generation: int = 0
    sequence: int = 0
    settled: bool = False
    ttl_seconds: int = 0
    expires_at_ms: int = 0
    created_at_ms: int = 0
    updated_at_ms: int = 0
    refs_total: int = 0
    refs_succeeded: int = 0
    refs_failed: int = 0
    refs_pending: int = 0
    bytes: PrefetchBytes = field(default_factory=PrefetchBytes)
    coverage: PrefetchCoverage | None = None
    refs: tuple[PrefetchRefStatus, ...] | None = None
    next_cursor: str | None = None
    created: bool = False


def _pick(cls, data: Any) -> Any:
    if not isinstance(data, dict):
        raise ValueError("object expected")
    names = cls.__dataclass_fields__
    return cls(**{k: v for k, v in data.items() if k in names})


def decode_status(data: Any, created: bool = False) -> PrefetchStatus:
    """Decode a status leniently: unknown fields are ignored."""
    if not isinstance(data, dict) or not isinstance(data.get("id"), str) or not isinstance(data.get("state"), str):
        raise ValueError("bad status")
    fields = {k: v for k, v in data.items() if k in PrefetchStatus.__dataclass_fields__}
    fields["bytes"] = _pick(PrefetchBytes, data.get("bytes") or {})
    if data.get("coverage") is not None:
        fields["coverage"] = _pick(PrefetchCoverage, data["coverage"])
    if data.get("refs") is not None:
        fields["refs"] = tuple(_pick(PrefetchRefStatus, r) for r in data["refs"])
    fields["created"] = created
    return PrefetchStatus(**fields)


def format_wait(wait: float | timedelta) -> str:
    """Whole seconds as "<n>s", otherwise milliseconds rounded up as "<n>ms"."""
    seconds = wait.total_seconds() if isinstance(wait, timedelta) else float(wait)
    if seconds < 0 or math.isnan(seconds) or math.isinf(seconds):
        raise RacerError("invalid wait")
    if seconds == int(seconds):
        return f"{int(seconds)}s"
    return f"{math.ceil(seconds * 1000)}ms"


def status_query(wait=None, coverage: bool = False, refs: str | None = None, cursor: str | None = None) -> str:
    params = []
    if wait is not None and format_wait(wait) != "0s":
        params.append(("wait", format_wait(wait)))
    if coverage:
        params.append(("coverage", "1"))
    if refs is not None:
        if refs not in ("failed", "all"):
            raise RacerError("refs must be None, 'failed', or 'all'")
        params.append(("refs", refs))
    if cursor:
        params.append(("cursor", cursor))
    return urlencode(params)


def wait_seconds(wait) -> float:
    if wait is None:
        return 0.0
    return wait.total_seconds() if isinstance(wait, timedelta) else float(wait)


def split_prefetch(
    prefetch_id: str,
    spec: PrefetchSpec,
    max_refs: int = DEFAULT_MAX_REFS,
    max_body_bytes: int = DEFAULT_MAX_BODY_BYTES,
) -> list[tuple[str, PrefetchSpec]]:
    """Split spec into tickets of at most max_refs refs and max_body_bytes.

    Part ids are "<id>.<n>" from 0, even for one part, so they stay stable as
    the ref list grows. Raises RacerError if a single ref cannot fit or a part
    id would be invalid.
    """
    if max_refs <= 0:
        max_refs = DEFAULT_MAX_REFS
    if max_body_bytes <= 0:
        max_body_bytes = DEFAULT_MAX_BODY_BYTES
    body = spec.to_wire()
    refs = body["refs"]
    body["refs"] = []
    base = len(_dumps(body))
    parts: list[tuple[str, PrefetchSpec]] = []
    start, size = 0, base

    def flush(end: int) -> None:
        nonlocal start, size
        part_id = f"{prefetch_id}.{len(parts)}"
        validate_id(part_id)
        parts.append((part_id, replace(spec, refs=tuple(spec.refs[start:end]))))
        start, size = end, base

    for i, ref in enumerate(refs):
        encoded = len(_dumps(ref))
        cost = encoded + (1 if i > start else 0)
        if i > start and (i - start >= max_refs or size + cost > max_body_bytes):
            flush(i)
            cost = encoded
        if size + cost > max_body_bytes:
            raise RacerError(f"ref {i} does not fit in {max_body_bytes} bytes")
        size += cost
    flush(len(refs))
    return parts


class PrefetchWindow:
    """A rolling window: one ticket whose desired set each update replaces.

    The first submission uses the spec's generation and sequence. update()
    then increments the sequence and reset() increments the generation and
    sets the sequence to 0. Positions never roll back after a failed call,
    since the server may have applied it and a different payload at the same
    position would be rejected as payload_mismatch. Invalid refs are rejected
    locally and do not consume a position. Thread-safe.
    """

    def __init__(self, client: "Client", prefetch_id: str, spec: PrefetchSpec | None = None) -> None:
        validate_id(prefetch_id)
        self.client = client
        self.id = prefetch_id
        self._spec = replace(spec or PrefetchSpec(), refs=())
        self._submitted = False
        self._lock = threading.Lock()

    @property
    def position(self) -> tuple[int, int]:
        with self._lock:
            return self._spec.generation, self._spec.sequence

    def update(self, refs: Sequence[PrefetchRef]) -> PrefetchStatus:
        with self._lock:
            nxt = self._spec
            if self._submitted:
                if nxt.sequence >= MAX_COUNTER:
                    raise RacerError("sequence exhausted; call reset")
                nxt = replace(nxt, sequence=nxt.sequence + 1)
            return self._submit(nxt, refs)

    def reset(self, refs: Sequence[PrefetchRef]) -> PrefetchStatus:
        with self._lock:
            if self._spec.generation >= MAX_COUNTER:
                raise RacerError("generation exhausted")
            return self._submit(replace(self._spec, generation=self._spec.generation + 1, sequence=0), refs)

    def _submit(self, nxt: PrefetchSpec, refs: Sequence[PrefetchRef]) -> PrefetchStatus:
        spec = replace(nxt, refs=tuple(refs))
        spec.to_wire()
        self._spec, self._submitted = nxt, True
        return self.client.prefetch(self.id, spec)
