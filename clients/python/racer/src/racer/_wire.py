# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Racer client socket wire format.

This mirrors the Go SDK in pkg/racersdk/internal/wire/wire.go and the
dataplane in cmd/racer/object-wire/src/client.rs.
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
    "racer-retain-until",
    "racer-delivery",
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
