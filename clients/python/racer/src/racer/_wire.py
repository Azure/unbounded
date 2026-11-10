# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Racer client socket wire format.

This mirrors the Go SDK in pkg/racersdk/internal/wire/wire.go and the
dataplane in cmd/racer/wire/src/object/client.rs.
"""

from __future__ import annotations

import struct
from dataclasses import dataclass

from .errors import ProtocolError, RacerError, status_error

MAX_HEAD_BYTES = 32 * 1024
MAX_FIELD_BYTES = 8192
OBJECT_PREFIX = "/v1/objects/"
PAGE_SIZE = 16 * 1024 * 1024
FRAME_SIZE = 21
CREDIT_SIZE = 12
PAGE_FRAME = 1
COMPLETE_FRAME = 2
MAX_WIRE_INTEGER = (1 << 63) - 1

# Credits the Go SDK grants per Get (pkg/racersdk/client.go: pageCredits = 2).
PAGE_CREDITS = 2
BYTE_CREDITS = PAGE_CREDITS * PAGE_SIZE

# Writes (designs/racer-writes.md, Client API).
MAX_OBJECT_PAGES = 50_000
MAX_HOLD_SECONDS = 2_592_000
UPLOAD_MODES = frozenset({"auto", "manual", "never"})
UPLOAD_STATES = frozenset({"pending", "uploading", "committed", "failed", "never", "manual"})
PUT = "put"
HOST_WRITE = "host-write"
PERSIST = "persist"
DELETE = "delete"


def max_object_bytes() -> int:
    return MAX_OBJECT_PAGES * PAGE_SIZE


_FRAME = struct.Struct(">BQQI")
_CREDIT = struct.Struct(">QI")


@dataclass(frozen=True)
class Frame:
    kind: int
    number: int
    offset: int
    length: int


def decode_frame(raw: bytes) -> Frame:
    if len(raw) != FRAME_SIZE:
        raise ProtocolError("short frame")
    return Frame(*_FRAME.unpack(raw))


def encode_frame(kind: int, number: int, offset: int, length: int) -> bytes:
    return _FRAME.pack(kind, number, offset, length)


def encode_credit(number: int, length: int) -> bytes:
    return _CREDIT.pack(number, length)


def decode_credit(raw: bytes) -> tuple[int, int]:
    return _CREDIT.unpack(raw)


def page_count(first: int, end: int) -> int:
    if end <= first:
        return 0
    return (end - 1) // PAGE_SIZE - first // PAGE_SIZE + 1


class Sequence:
    """Validates page and completion frames like wire.Sequence.Accept.

    Pages must arrive strictly in page order, so tracking the count of
    delivered pages is enough to reject duplicates.
    """

    def __init__(self, first: int, end: int) -> None:
        self.first = first
        self.end = end
        self.pages = page_count(first, end)
        self.delivered = 0
        self.bytes = 0
        self.complete = False

    def accept(self, f: Frame) -> None:
        if self.complete:
            raise ProtocolError("frame after completion")
        if f.kind == COMPLETE_FRAME:
            if (
                f.number != self.pages
                or self.delivered != self.pages
                or f.offset != self.end - self.first
                or self.bytes != f.offset
                or f.length != 0
            ):
                raise ProtocolError("invalid completion frame")
            self.complete = True
            return
        if (
            f.kind != PAGE_FRAME
            or f.length == 0
            or f.offset < self.first
            or f.offset >= self.end
            or f.number != f.offset // PAGE_SIZE
        ):
            raise ProtocolError("invalid page frame")
        start = max(self.first, f.number * PAGE_SIZE)
        end = min(self.end, (f.number + 1) * PAGE_SIZE)
        if (
            f.offset != start
            or f.length != end - start
            or f.number != self.first // PAGE_SIZE + self.delivered
        ):
            raise ProtocolError("page frame out of order")
        self.delivered += 1
        self.bytes += f.length


def decimal(s: str | None) -> int:
    """Parse a canonical non-negative int63 like wire.Decimal."""
    if s is None or s == "" or len(s) > 19 or (len(s) > 1 and s[0] == "0"):
        raise ProtocolError("invalid decimal")
    if not all("0" <= c <= "9" for c in s):
        raise ProtocolError("invalid decimal")
    n = int(s)
    if n > MAX_WIRE_INTEGER:
        raise ProtocolError("invalid decimal")
    return n


def validate_etag(s: str) -> None:
    if len(s) < 2 or len(s) > MAX_FIELD_BYTES or s[0] != '"' or s[-1] != '"':
        raise RacerError("invalid etag")
    for c in s[1:-1]:
        o = ord(c)
        if o != 0x21 and not 0x23 <= o <= 0x7E:
            raise RacerError("invalid etag")


def validate_opaque(s: str) -> None:
    raw = s.encode()
    if len(raw) > MAX_FIELD_BYTES:
        raise RacerError("header field too large")
    if not raw or raw[0] == 0x20 or raw[-1] == 0x20 or any(b < 0x20 or b == 0x7F for b in raw):
        raise RacerError("invalid header field")


def validate_content_type(s: str) -> bool:
    return 0 < len(s) <= 256 and s.strip() == s and all(0x20 <= ord(c) <= 0x7E for c in s)


@dataclass
class ResponseHead:
    status: int
    headers: dict[str, str]  # lower-case names

    def get(self, name: str) -> str | None:
        return self.headers.get(name.lower())


_SINGLETONS = {
    "content-length",
    "content-type",
    "content-range",
    "etag",
    "racer-expires-at",
    "racer-content-type",
    "racer-object-length",
    "racer-range-start",
    "racer-range-end",
    "racer-delivery",
    "racer-upload-state",
    "racer-write-error",
    "retry-after",
}


def parse_response_head(raw: bytes) -> ResponseHead:
    """Parse an HTTP/1.1 response head ending in CRLF CRLF."""
    try:
        text = raw.decode("latin-1")
    except Exception as exc:  # pragma: no cover - latin-1 never fails
        raise ProtocolError("bad head") from exc
    lines = text.split("\r\n")
    if len(lines) < 3 or lines[-1] != "" or lines[-2] != "":
        raise ProtocolError("bad head")
    line = lines[0]
    if len(line) < 12 or not line.startswith("HTTP/1.1 ") or (len(line) > 12 and line[12] != " "):
        raise ProtocolError("bad status line")
    code = line[9:12]
    if not code.isdigit():
        raise ProtocolError("bad status line")
    headers: dict[str, str] = {}
    for h in lines[1:-2]:
        name, sep, value = h.partition(":")
        if not sep or not name or name != name.strip():
            raise ProtocolError("bad header")
        key = name.lower()
        value = value.strip(" \t")
        if key in headers:
            if key in _SINGLETONS:
                raise ProtocolError("duplicate header " + name)
            headers[key] += ", " + value
        else:
            headers[key] = value
    return ResponseHead(int(code), headers)


def connection_close(h: ResponseHead) -> bool:
    v = h.get("connection") or ""
    return any(t.strip().lower() == "close" for t in v.split(","))


def check_error_status(h: ResponseHead) -> None:
    """Raise the mapped error for a non-200 response, like subscriptionStatusError."""
    if h.status == 200:
        return
    if decimal(h.get("content-length")) != 0:
        raise ProtocolError("error response with a body")
    if h.status == 416:
        v = h.get("content-range") or ""
        if not v.startswith("bytes */"):
            raise ProtocolError("416 without Content-Range")
        decimal(v[len("bytes */") :])
    elif h.get("content-range") is not None:
        raise ProtocolError("unexpected Content-Range")
    raise status_error(h.status)


def has_version_id(metadata: str) -> bool:
    """Report whether opaque metadata is a JSON object naming a versionId."""
    import json

    try:
        value = json.loads(metadata)
    except ValueError:
        return False
    return isinstance(value, dict) and "versionId" in value


def write_head(
    kind: str,
    key_hex: str,
    *,
    length: int = 0,
    source_offset: int = 0,
    replicas: int | None = None,
    durability_origin: bool = False,
    upload: str | None = None,
    hold_seconds: int | None = None,
    content_type: str | None = None,
    metadata: str | None = None,
    authorization: str | None = None,
) -> bytes:
    """Build the request head of a write, like wire.WriteHead in the Go SDK."""
    if metadata and has_version_id(metadata):
        raise RacerError("writes must not name a versionId")
    target = OBJECT_PREFIX + key_hex
    if kind == PUT:
        if not 0 < length <= max_object_bytes():
            raise RacerError("invalid object length")
        head = f"PUT {target} HTTP/1.1\r\nHost: racer\r\nContent-Length: {length}\r\nIf-None-Match: *\r\n"
    elif kind == HOST_WRITE:
        if not 0 < length <= max_object_bytes() or source_offset < 0 or source_offset > MAX_WIRE_INTEGER - length:
            raise RacerError("invalid source window")
        head = (
            f"POST {target}/host-write HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\nIf-None-Match: *\r\n"
            f"Racer-Source-Offset: {source_offset}\r\nRacer-Source-Length: {length}\r\n"
        )
    elif kind == PERSIST:
        head = f"POST {target}/persist HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\n"
    elif kind == DELETE:
        head = f"DELETE {target} HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\n"
    else:
        raise RacerError("unknown write kind")
    if kind in (PUT, HOST_WRITE):
        if replicas is not None and (isinstance(replicas, bool) or replicas not in (1, 2, 3)):
            raise RacerError("replicas must be 1, 2, or 3")
        if durability_origin and replicas is not None:
            raise RacerError("origin durability excludes replicas")
        if upload is not None and upload not in UPLOAD_MODES:
            raise RacerError("invalid upload mode")
        if durability_origin and upload in ("manual", "never"):
            raise RacerError("origin durability needs auto upload")
        if hold_seconds is not None and (isinstance(hold_seconds, bool) or not 0 < hold_seconds <= MAX_HOLD_SECONDS):
            raise RacerError("invalid hold")
        if durability_origin:
            head += "Racer-Durability: origin\r\n"
        elif replicas is not None:
            head += f"Racer-Durability: replicas={replicas}\r\n"
        if upload is not None:
            head += f"Racer-Upload: {upload}\r\n"
        if hold_seconds is not None:
            head += f"Racer-Hold-Seconds: {hold_seconds}\r\n"
        if content_type is not None:
            if not validate_content_type(content_type):
                raise RacerError("invalid content type")
            head += f"Racer-Content-Type: {content_type}\r\n"
    if metadata:
        validate_opaque(metadata)
        head += "Racer-Metadata: " + metadata + "\r\n"
    if authorization:
        validate_opaque(authorization)
        head += "Authorization: " + authorization + "\r\n"
    raw = (head + "\r\n").encode()
    if len(raw) > MAX_HEAD_BYTES:
        raise RacerError("request head too large")
    return raw


@dataclass(frozen=True)
class WriteResponse:
    status: int
    etag: str | None
    upload_state: str | None
    error: str
    retry_after: float | None


def parse_write_response(h: ResponseHead, kind: str) -> WriteResponse:
    """Validate a write response head. Non-success statuses are returned for the caller to raise."""
    if h.get("transfer-encoding") is not None:
        raise ProtocolError("unexpected Transfer-Encoding")
    cl = h.get("content-length")
    if cl is not None or h.status != 204:
        if decimal(cl) != 0:
            raise ProtocolError("write response with a body")
    state = h.get("racer-upload-state")
    if state is not None and state not in UPLOAD_STATES:
        raise ProtocolError("bad Racer-Upload-State")
    retry = None
    value = (h.get("retry-after") or "").strip()
    if value.isascii() and value.isdigit() and len(value) <= 19:
        retry = float(int(value))
    etag = h.get("etag")
    if h.status == 201:
        if kind not in (PUT, HOST_WRITE) or state is None or etag is None:
            raise ProtocolError("bad write response")
        try:
            validate_etag(etag)
        except RacerError as exc:
            raise ProtocolError("bad ETag") from exc
    elif h.status == 204:
        if kind in (PUT, HOST_WRITE) or (kind == PERSIST and state is None):
            raise ProtocolError("bad write response")
    return WriteResponse(h.status, etag, state, h.get("racer-write-error") or "", retry)
