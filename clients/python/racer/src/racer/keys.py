# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Object keys for racer-object (S3) objects.

Replicates NewRequest in internal/racerobject/racerobject.go: the key is the
SHA-256 of the canonical schema-1 JSON produced by Go's encoding/json, and
that JSON is sent as Racer-Metadata.
"""

from __future__ import annotations

import hashlib
import json
import re
from dataclasses import dataclass

from .errors import RacerError

_BUCKET = re.compile(r"[A-Za-z0-9._-]{1,255}")


@dataclass(frozen=True)
class Request:
    """One object to read: its 32-byte key plus opaque origin context."""

    key: bytes
    metadata: str | None = None
    authorization: str | None = None

    def __post_init__(self) -> None:
        if len(self.key) != 32:
            raise RacerError("key must be 32 bytes")

    @property
    def hex(self) -> str:
        return self.key.hex()

    @classmethod
    def from_hex(cls, key: str, metadata: str | None = None, authorization: str | None = None) -> "Request":
        if len(key) != 64 or any(c not in "0123456789abcdef" for c in key):
            raise RacerError("key must be 64 lower-case hex digits")
        return cls(bytes.fromhex(key), metadata, authorization)


def _go_json_string(s: str) -> str:
    # Go escapes <, >, & and U+2028, U+2029 by default (json.Marshal HTMLEscape),
    # and writes other non-ASCII as raw UTF-8. Control characters match Python's
    # short escapes (\b \f \n \r \t) and \u00XX otherwise.
    out = json.dumps(s, ensure_ascii=False)
    return (
        out.replace("<", "\\u003c")
        .replace(">", "\\u003e")
        .replace("&", "\\u0026")
        .replace("\u2028", "\\u2028")
        .replace("\u2029", "\\u2029")
    )


def canonical_json(bucket: str, key: str, version_id: str | None = None) -> str:
    """Return the canonical schema-1 JSON, validating like validObject."""
    version_id = version_id or ""
    if bucket in (".", "..") or not _BUCKET.fullmatch(bucket):
        raise RacerError("invalid bucket")
    for s in (key, version_id):
        try:
            s.encode("utf-8")
        except UnicodeEncodeError as exc:
            raise RacerError("invalid UTF-8") from exc
    if not key or len(key.encode()) > 1024 or len(version_id.encode()) > 1024:
        raise RacerError("invalid key or version")
    out = '{"schema":1,"bucket":' + _go_json_string(bucket) + ',"key":' + _go_json_string(key)
    if version_id:
        out += ',"versionId":' + _go_json_string(version_id)
    out += "}"
    if len(out.encode()) > 8 << 10:
        raise RacerError("object identity too large")
    return out


def object_request(bucket: str, key: str, version_id: str | None = None) -> Request:
    """Build the Request racer-object uses for an S3 object."""
    canonical = canonical_json(bucket, key, version_id)
    return Request(hashlib.sha256(canonical.encode()).digest(), canonical)


def object_key(bucket: str, key: str, version_id: str | None = None) -> str:
    """Return the 64-hex-digit Racer key for an S3 object."""
    return object_request(bucket, key, version_id).hex
