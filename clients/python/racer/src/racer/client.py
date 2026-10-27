# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Racer client over the node-local Unix socket.

Request heads and response checks mirror pkg/racersdk (client.go, object.go)
and pkg/racersdk/internal/wire/wire.go. The host-read and gpu-read RPCs follow
designs/racer-gpudirect.md.
"""

from __future__ import annotations

import array
import contextlib
import logging
import os
import re
import socket
import threading
import time
from dataclasses import dataclass
from typing import Callable, Optional

from . import _wire as wire
from .errors import ProtocolError, RacerError, StatusError, TruncatedError, UnsatisfiableRange
from .keys import Request

OnPage = Optional[Callable[[int, int], None]]

GPU = "gpu"
HOST = "host"
STREAM = "stream"

# Statuses that mean "this dataplane does not support the mode" on a first
# attempt (designs/racer-gpudirect.md, Fallback).
FALLBACK_STATUSES = frozenset({400, 404, 405, 501})

DEFAULT_STAGING_BYTES = 4 * wire.PAGE_SIZE

# Seconds before a mode remembered as unsupported is tried again.
DEFAULT_MODE_RETRY_SECONDS = 300.0

_SUPPORTED = "supported"
_UNSUPPORTED = "unsupported"

log = logging.getLogger(__name__)


@dataclass
class _ModeEntry:
    state: str
    since: float  # clock() when the state was recorded
    socket_id: tuple | None  # (st_dev, st_ino) of the socket file then


# Mode support per socket path. An entry is dropped when the socket file is
# replaced (the dataplane restarted), and an unsupported entry also expires
# after the client's mode_retry_seconds.
_mode_lock = threading.Lock()
_mode_state: dict[str, dict[str, _ModeEntry]] = {}


def _socket_id(path: str) -> tuple | None:
    try:
        st = os.stat(path)
    except OSError:
        return None
    return (st.st_dev, st.st_ino)


def _dest_device(dest) -> int | None:
    """Return the CUDA device ordinal of a read destination, if it has one."""
    device = getattr(dest, "device", None)
    if isinstance(device, int) and not isinstance(device, bool):
        return device
    index = getattr(device, "index", None)
    return index if isinstance(index, int) else None


_UUID = re.compile(r"GPU-[0-9a-fA-F]{8}(-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}")


def _dest_uuid(dest) -> str | None:
    """Return the GPU UUID of a gpu-read destination, or None if unknown.

    The dataplane uses it to pick its context for the client's GPU, because
    device ordinals differ between containers. Without it the dataplane tries
    each GPU, so failing to learn the UUID is never an error.
    """
    get = getattr(dest, "gpu_uuid", None)
    if get is None:
        return None
    try:
        uuid = get() if callable(get) else get
    except Exception:
        return None
    if not isinstance(uuid, str):
        return None
    if not uuid.upper().startswith("GPU-"):
        uuid = "GPU-" + uuid
    return uuid if _UUID.fullmatch(uuid) else None


def socket_path_for(cache: str | None = None, socket_path: str | None = None) -> str:
    """Resolve the client socket path.

    Order: explicit socket_path, then $RACER_SOCKET, then
    /run/racer/<cache>/client/socket (pkg/racersdk/client.go).
    """
    if socket_path:
        return socket_path
    env = os.environ.get("RACER_SOCKET")
    if env:
        return env
    if not cache:
        raise RacerError("need a cache name, a socket path, or RACER_SOCKET")
    if "/" in cache or cache in (".", ".."):
        raise RacerError("invalid cache name")
    return f"/run/racer/{cache}/client/socket"


def forget_modes(path: str | None = None) -> None:
    """Clear remembered mode support (for tests, or after a dataplane upgrade)."""
    with _mode_lock:
        if path is None:
            _mode_state.clear()
        else:
            _mode_state.pop(path, None)


@dataclass(frozen=True)
class Metadata:
    etag: str
    size: int
    expires_at_ms: int
    content_type: str | None = None
    retain_until_ms: int | None = None


@dataclass(frozen=True)
class ReadResult:
    metadata: Metadata
    first: int
    end: int
    delivery: str
    pages: int

    @property
    def length(self) -> int:
        return self.end - self.first


class _Conn:
    def __init__(self, path: str, timeout: float | None) -> None:
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(timeout)
        try:
            self.sock.connect(path)
        except BaseException:
            self.sock.close()
            raise
        self.buf = bytearray()

    def close(self) -> None:
        self.sock.close()

    def send_head(self, head: bytes, fd: int | None = None) -> None:
        if fd is None:
            self.sock.sendall(head)
            return
        # The descriptor rides on the sendmsg that carries the first head byte.
        anc = [(socket.SOL_SOCKET, socket.SCM_RIGHTS, array.array("i", [fd]).tobytes())]
        sent = self.sock.sendmsg([head], anc)
        if sent < len(head):
            self.sock.sendall(head[sent:])

    def send_credit(self, number: int, length: int) -> None:
        # Racer may finish and close while a credit is in flight; the next read
        # reports whether the transfer was cut short (object.go credit()).
        with contextlib.suppress(BrokenPipeError, ConnectionResetError):
            self.sock.sendall(wire.encode_credit(number, length))

    def read_head(self) -> bytes:
        while True:
            i = self.buf.find(b"\r\n\r\n")
            if i >= 0:
                head = bytes(self.buf[: i + 4])
                del self.buf[: i + 4]
                return head
            if len(self.buf) > wire.MAX_HEAD_BYTES:
                raise ProtocolError("response head too large")
            chunk = self.sock.recv(65536)
            if not chunk:
                raise TruncatedError("connection closed before the response head")
            self.buf += chunk

    def readinto(self, mv: memoryview) -> None:
        n = min(len(self.buf), len(mv))
        if n:
            mv[:n] = self.buf[:n]
            del self.buf[:n]
        while n < len(mv):
            got = self.sock.recv_into(mv[n:])
            if got == 0:
                raise TruncatedError("connection closed mid-transfer")
            n += got

    def read_exact(self, size: int) -> bytes:
        out = bytearray(size)
        self.readinto(memoryview(out))
        return bytes(out)


def _response_metadata(h: wire.ResponseHead) -> Metadata | None:
    etag = h.get("etag") or ""
    try:
        wire.validate_etag(etag)
        expiry = wire.decimal(h.get("racer-expires-at"))
    except RacerError:
        return None
    ct = h.get("racer-content-type")
    if ct is not None and not wire.validate_content_type(ct):
        return None
    return Metadata(etag=etag, size=0, expires_at_ms=expiry, content_type=ct)


class Client:
    """Reads Racer objects through the client socket.

    Each read uses its own connection, so one Client may be shared by threads.
    """

    def __init__(
        self,
        cache: str | None = None,
        socket_path: str | None = None,
        timeout: float | None = 60.0,
        staging_bytes: int = DEFAULT_STAGING_BYTES,
        pin_staging: bool = True,
        mode_retry_seconds: float | None = DEFAULT_MODE_RETRY_SECONDS,
        clock: Callable[[], float] = time.monotonic,
    ) -> None:
        self.socket_path = socket_path_for(cache, socket_path)
        self.timeout = timeout
        if staging_bytes <= 0:
            raise RacerError("staging_bytes must be positive")
        self.staging_bytes = staging_bytes
        self.pin_staging = pin_staging
        if mode_retry_seconds is not None and mode_retry_seconds < 0:
            raise RacerError("mode_retry_seconds must not be negative")
        # None keeps an unsupported mode until the socket file changes.
        self.mode_retry_seconds = mode_retry_seconds
        self._clock = clock
        self._pool: list = []
        self._pool_lock = threading.Lock()

    # Mode memory -----------------------------------------------------------

    def mode_state(self, mode: str) -> str | None:
        """Return "supported", "unsupported", or None when unknown.

        Memory is dropped when the socket file was replaced since it was
        recorded, and "unsupported" expires after mode_retry_seconds.
        """
        sid = _socket_id(self.socket_path)
        with _mode_lock:
            modes = _mode_state.get(self.socket_path)
            if not modes:
                return None
            if any(e.socket_id != sid for e in modes.values()):
                # The dataplane restarted (or the socket is gone): forget all.
                _mode_state.pop(self.socket_path, None)
                return None
            e = modes.get(mode)
            if e is None:
                return None
            if (
                e.state == _UNSUPPORTED
                and self.mode_retry_seconds is not None
                and self._clock() - e.since >= self.mode_retry_seconds
            ):
                del modes[mode]
                return None
            return e.state

    def _set_mode(self, mode: str, state: str, reason: str | None = None, next_mode: str | None = None) -> None:
        sid = _socket_id(self.socket_path)
        with _mode_lock:
            modes = _mode_state.setdefault(self.socket_path, {})
            prev = modes.get(mode)
            modes[mode] = _ModeEntry(state, self._clock(), sid)
        if state == _UNSUPPORTED and (prev is None or prev.state != _UNSUPPORTED):
            log.warning(
                "racer: %s-read is not available from %s (%s); falling back to %s until %s",
                mode,
                self.socket_path,
                reason or "unsupported",
                f"{next_mode} mode" if next_mode else "the next mode",
                "the dataplane restarts"
                if self.mode_retry_seconds is None
                else f"a retry in {self.mode_retry_seconds:g}s or a dataplane restart",
            )
        elif state == _SUPPORTED and prev is not None and prev.state == _UNSUPPORTED:
            log.info("racer: %s-read is available again from %s", mode, self.socket_path)

    # Staging pool ----------------------------------------------------------

    def _take_staging(self, device: int | None = None):
        from .host import HostBuffer

        with self._pool_lock:
            if self._pool:
                return self._pool.pop()
        buf = HostBuffer(self.staging_bytes, "racer-staging")
        if self.pin_staging:
            buf.pin(0 if device is None else device)
        return buf

    def _give_staging(self, buf) -> None:
        with self._pool_lock:
            self._pool.append(buf)

    def close(self) -> None:
        with self._pool_lock:
            pool, self._pool = self._pool, []
        for b in pool:
            b.close()

    def __enter__(self) -> "Client":
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()

    # Heads -----------------------------------------------------------------

    @staticmethod
    def _context_headers(request: Request) -> str:
        out = ""
        if request.metadata:
            wire.validate_opaque(request.metadata)
            out += "Racer-Metadata: " + request.metadata + "\r\n"
        if request.authorization:
            wire.validate_opaque(request.authorization)
            out += "Authorization: " + request.authorization + "\r\n"
        return out

    @staticmethod
    def _range_header(offset: int, length: int | None) -> str:
        if offset < 0 or (length is not None and length < 0) or offset > wire.MAX_WIRE_INTEGER:
            raise RacerError("invalid range")
        if length and offset + length - 1 > wire.MAX_WIRE_INTEGER:
            raise RacerError("invalid range")
        if offset == 0 and not length:
            return ""
        out = f"Range: bytes={offset}-"
        if length:
            out += str(offset + length - 1)
        return out + "\r\n"

    def _head_bytes(self, head: str) -> bytes:
        raw = head.encode()
        if len(raw) > wire.MAX_HEAD_BYTES:
            raise RacerError("request head too large")
        return raw

    def _connect(self) -> _Conn:
        return _Conn(self.socket_path, self.timeout)

    # Stat ------------------------------------------------------------------

    def stat(self, request: Request, if_match: str | None = None, retain_seconds: int | None = None) -> Metadata:
        """HEAD an object. retain_seconds asks Racer to keep it warm."""
        head = f"HEAD {wire.OBJECT_PREFIX}{request.hex} HTTP/1.1\r\nHost: racer\r\n"
        if if_match:
            wire.validate_etag(if_match)
            head += "If-Match: " + if_match + "\r\n"
        if retain_seconds:
            head += f"Cache-Control: max-age={int(retain_seconds)}\r\n"
        head += self._context_headers(request) + "\r\n"
        conn = self._connect()
        try:
            conn.send_head(self._head_bytes(head))
            h = wire.parse_response_head(conn.read_head())
        finally:
            conn.close()
        wire.check_error_status(h)
        size = wire.decimal(h.get("content-length"))
        if h.get("content-type") != "application/octet-stream":
            raise ProtocolError("bad Content-Type")
        m = _response_metadata(h)
        if m is None or (if_match and m.etag != if_match):
            raise ProtocolError("bad HEAD metadata")
        retain = h.get("racer-retain-until")
        return Metadata(
            etag=m.etag,
            size=size,
            expires_at_ms=m.expires_at_ms,
            content_type=m.content_type,
            retain_until_ms=wire.decimal(retain) if retain is not None else None,
        )

    # Shared response checks ------------------------------------------------

    @staticmethod
    def _check_selection(
        h: wire.ResponseHead, offset: int, length: int | None, if_match: str | None
    ) -> tuple[Metadata, int, int]:
        """Validate a 200 subscription-style head (wire.ParseSubscriptionResponse)."""
        if not wire.connection_close(h) or h.get("content-range") is not None:
            raise ProtocolError("bad response head")
        if h.get("content-type") != "application/octet-stream":
            raise ProtocolError("bad Content-Type")
        size = wire.decimal(h.get("racer-object-length"))
        first = wire.decimal(h.get("racer-range-start"))
        end = wire.decimal(h.get("racer-range-end"))
        m = _response_metadata(h)
        if m is None or first > end or end > size or first != offset:
            raise ProtocolError("bad response selection")
        if not length and end != size:
            raise ProtocolError("bad response selection")
        if if_match and m.etag != if_match:
            raise ProtocolError("response ETag does not match If-Match")
        if length and end - first != length:
            if end == size and length > end - first:
                raise UnsatisfiableRange(416, "range extends past the object")
            raise ProtocolError("bad response selection")
        meta = Metadata(m.etag, size, m.expires_at_ms, m.content_type)
        return meta, first, end

    # Stream mode -----------------------------------------------------------

    def _stream(
        self,
        request: Request,
        offset: int,
        length: int | None,
        if_match: str | None,
        scratch: memoryview,
        write: Callable[[int, memoryview], None],
        on_page: OnPage,
        max_length: int | None = None,
        on_head: Callable[[], None] | None = None,
    ) -> ReadResult:
        head = (
            f"POST {wire.OBJECT_PREFIX}{request.hex} HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\n"
            f"Racer-Page-Credits: {wire.PAGE_CREDITS}\r\nRacer-Byte-Credits: {wire.BYTE_CREDITS}\r\n"
        )
        head += self._range_header(offset, length)
        if if_match:
            wire.validate_etag(if_match)
            head += "If-Match: " + if_match + "\r\n"
        head += self._context_headers(request) + "\r\n"
        if len(scratch) == 0:
            raise RacerError("empty scratch buffer")
        conn = self._connect()
        try:
            conn.send_head(self._head_bytes(head))
            h = wire.parse_response_head(conn.read_head())
            wire.check_error_status(h)
            meta, first, end = self._check_selection(h, offset, length, if_match)
            pages = wire.page_count(first, end)
            body = wire.decimal(h.get("content-length"))
            if body != end - first + wire.FRAME_SIZE * (pages + 1):
                raise ProtocolError("bad Content-Length")
            if max_length is not None and end - first > max_length:
                # An earlier direct mode may have refused this read with 400
                # for the same reason; that refusal says nothing about support
                # (designs/racer-gpudirect.md, Fallback), so do not confirm it.
                raise RacerError("destination is smaller than the range")
            if on_head:
                on_head()
            seq = wire.Sequence(first, end)
            for _ in range(pages):
                f = wire.decode_frame(conn.read_exact(wire.FRAME_SIZE))
                seq.accept(f)
                if f.kind != wire.PAGE_FRAME:
                    raise ProtocolError("missing page frame")
                pos = 0
                while pos < f.length:
                    n = min(len(scratch), f.length - pos)
                    conn.readinto(scratch[:n])
                    write(f.offset + pos, scratch[:n])
                    pos += n
                conn.send_credit(f.number, f.length)
                if on_page:
                    on_page(f.offset, f.length)
            f = wire.decode_frame(conn.read_exact(wire.FRAME_SIZE))
            seq.accept(f)
            if f.kind != wire.COMPLETE_FRAME:  # pragma: no cover - accept() enforces it
                raise ProtocolError("missing completion")
            return ReadResult(meta, first, end, STREAM, pages)
        finally:
            conn.close()

    def read(self, request: Request, offset: int = 0, length: int | None = None, if_match: str | None = None) -> bytes:
        """Read a range into new bytes using stream mode."""
        out = bytearray()

        def write(o: int, piece: memoryview) -> None:
            out.extend(piece)

        self._stream(request, offset, length, if_match, memoryview(bytearray(1 << 20)), write, None)
        return bytes(out)

    # RPC modes -------------------------------------------------------------

    def _rpc(
        self,
        mode: str,
        request: Request,
        fd: int,
        dest_offset: int,
        dest_length: int,
        allocation_size: int | None,
        offset: int,
        length: int | None,
        if_match: str | None,
        on_frame: Callable[[int, int], None] | None,
        on_head: Callable[[], None] | None = None,
        gpu_uuid: str | None = None,
    ) -> ReadResult:
        if dest_offset < 0 or dest_length <= 0:
            raise RacerError("invalid destination window")
        head = f"POST {wire.OBJECT_PREFIX}{request.hex}/{mode}-read HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\n"
        head += self._range_header(offset, length)
        if if_match:
            wire.validate_etag(if_match)
            head += "If-Match: " + if_match + "\r\n"
        head += f"Racer-Destination-Offset: {dest_offset}\r\nRacer-Destination-Length: {dest_length}\r\n"
        if allocation_size is not None:
            head += f"Racer-Allocation-Size: {allocation_size}\r\n"
        if gpu_uuid is not None:
            if not _UUID.fullmatch(gpu_uuid):
                raise RacerError("invalid GPU UUID")
            head += f"Racer-Gpu-Uuid: {gpu_uuid}\r\n"
        head += self._context_headers(request) + "\r\n"
        conn = self._connect()
        try:
            conn.send_head(self._head_bytes(head), fd)
            h = wire.parse_response_head(conn.read_head())
            wire.check_error_status(h)
            if h.get("racer-delivery") != mode:
                raise ProtocolError("bad Racer-Delivery")
            meta, first, end = self._check_selection(h, offset, length, if_match)
            if end - first > dest_length:
                raise ProtocolError("range is larger than the destination")
            if on_head:
                on_head()
            pages = wire.page_count(first, end)
            if wire.decimal(h.get("content-length")) != wire.FRAME_SIZE * (pages + 1):
                raise ProtocolError("bad Content-Length")
            seq = wire.Sequence(first, end)
            for _ in range(pages):
                f = wire.decode_frame(conn.read_exact(wire.FRAME_SIZE))
                seq.accept(f)
                if f.kind != wire.PAGE_FRAME:
                    raise ProtocolError("missing page frame")
                if on_frame:
                    on_frame(f.offset, f.length)
            f = wire.decode_frame(conn.read_exact(wire.FRAME_SIZE))
            seq.accept(f)
            return ReadResult(meta, first, end, mode, pages)
        finally:
            conn.close()

    def host_read(
        self,
        request: Request,
        fd: int,
        dest_offset: int,
        dest_length: int,
        *,
        offset: int = 0,
        length: int | None = None,
        if_match: str | None = None,
        on_page: OnPage = None,
    ) -> ReadResult:
        """One host-read RPC with no fallback. fd is a regular file, normally a memfd."""
        return self._rpc(HOST, request, fd, dest_offset, dest_length, None, offset, length, if_match, on_page)

    def gpu_read(
        self,
        request: Request,
        fd: int,
        allocation_size: int,
        dest_offset: int,
        dest_length: int,
        *,
        offset: int = 0,
        length: int | None = None,
        if_match: str | None = None,
        on_page: OnPage = None,
        gpu_uuid: str | None = None,
    ) -> ReadResult:
        """One gpu-read RPC with no fallback. fd comes from cuMemExportToShareableHandle.

        gpu_uuid (``GPU-xxxxxxxx-...``, see racer.cuda.device_uuid) names the
        GPU that holds the allocation. Send it when the process sees more than
        one GPU or its device numbers may differ from the host's.
        """
        if dest_offset + dest_length > allocation_size:
            raise RacerError("destination window is outside the allocation")
        return self._rpc(
            GPU,
            request,
            fd,
            dest_offset,
            dest_length,
            allocation_size,
            offset,
            length,
            if_match,
            on_page,
            gpu_uuid=gpu_uuid,
        )

    # Fallback driver -------------------------------------------------------

    def _run_modes(self, attempts: list[tuple[str, Callable[[Callable[[], None]], ReadResult]]]) -> ReadResult:
        """Try modes in order.

        A mode with no remembered state that answers 400/404/405/501 is
        skipped. 501 marks it unsupported at once. 400/404/405 mark it
        unsupported only once a later mode gets a 200 head, so a missing
        object (404 everywhere) does not disable a mode for good.
        """
        pending: list[tuple[str, str]] = []
        last: StatusError | None = None
        current = [""]

        def understood() -> None:
            for m, why in pending:
                self._set_mode(m, _UNSUPPORTED, why, current[0])
            pending.clear()

        for i, (mode, run) in enumerate(attempts):
            current[0] = mode
            state = self.mode_state(mode) if mode != STREAM else _SUPPORTED
            if state == _UNSUPPORTED:
                continue
            try:
                result = run(understood)
            except _Skip:
                continue
            except StatusError as exc:
                if mode == STREAM or state is not None or exc.status not in FALLBACK_STATUSES:
                    raise
                why = f"status {exc.status}: {exc}"
                if exc.status == 501:
                    nxt = attempts[i + 1][0] if i + 1 < len(attempts) else None
                    self._set_mode(mode, _UNSUPPORTED, why, nxt)
                else:
                    pending.append((mode, why))
                last = exc
                continue
            if mode != STREAM and state != _SUPPORTED:
                self._set_mode(mode, _SUPPORTED)
            return result
        if last is not None:  # pragma: no cover - stream always runs last
            raise last
        raise RacerError("no read mode available")

    # High level: host ------------------------------------------------------

    def read_into_host(
        self,
        request: Request,
        dest,
        dest_offset: int = 0,
        *,
        offset: int = 0,
        length: int | None = None,
        if_match: str | None = None,
        on_page: OnPage = None,
    ) -> ReadResult:
        """Read a range into host memory at dest_offset.

        dest is a HostBuffer (host-read, then stream) or any writable buffer
        (stream only). Object offset o lands at dest_offset + (o - offset).
        """
        view = memoryview(dest.view if hasattr(dest, "view") else dest).cast("B")
        window = len(view) - dest_offset
        if dest_offset < 0 or window <= 0:
            raise RacerError("invalid destination window")
        if length and length > window:
            raise RacerError("destination is smaller than the range")
        attempts = []
        if hasattr(dest, "fileno"):
            attempts.append(
                (
                    HOST,
                    lambda ok: self._rpc(
                        HOST, request, dest.fileno(), dest_offset, window, None, offset, length, if_match, on_page, ok
                    ),
                )
            )

        def stream(ok):
            def write(o: int, piece: memoryview) -> None:
                at = dest_offset + o - offset
                view[at : at + len(piece)] = piece

            scratch = memoryview(bytearray(min(window, 1 << 20)))
            return self._stream(request, offset, length, if_match, scratch, write, on_page, window, ok)

        attempts.append((STREAM, stream))
        try:
            return self._run_modes(attempts)
        finally:
            view.release()

    # High level: GPU -------------------------------------------------------

    def read_into_gpu(
        self,
        request: Request,
        dest,
        dest_offset: int = 0,
        *,
        offset: int = 0,
        length: int | None = None,
        if_match: str | None = None,
        on_page: OnPage = None,
    ) -> ReadResult:
        """Read a range into GPU memory at dest_offset.

        dest is a racer.cuda.GpuAllocation, a racer.tensors.TensorTarget, or
        any object with ``size``, ``copy_from_host(offset, data)`` and
        optionally ``export_fd()`` and ``gpu_uuid()``. Modes: gpu-read, then
        host-read into a pinned staging memfd plus a host-to-device copy, then
        stream into the staging buffer plus a copy. on_page(object_offset, length) runs once a
        page is in GPU memory.
        """
        window = dest.size - dest_offset
        if dest_offset < 0 or window <= 0:
            raise RacerError("invalid destination window")
        if length and length > window:
            raise RacerError("destination is smaller than the range")

        attempts: list = []
        if hasattr(dest, "export_fd"):

            def gpu(ok):
                try:
                    fd = dest.export_fd()
                except NotImplementedError:
                    raise _Skip() from None
                try:
                    return self._rpc(
                        GPU,
                        request,
                        fd,
                        dest_offset,
                        window,
                        dest.size,
                        offset,
                        length,
                        if_match,
                        on_page,
                        ok,
                        gpu_uuid=_dest_uuid(dest),
                    )
                finally:
                    os.close(fd)

            attempts.append((GPU, gpu))
        attempts.append((HOST, lambda ok: self._gpu_via_host(request, dest, dest_offset, window, offset, length, if_match, on_page, ok)))
        attempts.append((STREAM, lambda ok: self._gpu_via_stream(request, dest, dest_offset, window, offset, length, if_match, on_page, ok)))

        return self._run_modes(attempts)

    def _gpu_via_host(self, request, dest, dest_offset, window, offset, length, if_match, on_page, ok) -> ReadResult:
        staging = self._take_staging(_dest_device(dest))
        try:
            chunk = staging.size
            if length:
                total = length
            else:
                # Resolve the size once so every chunk reads the same version.
                meta = self.stat(request, if_match=if_match)
                if offset > meta.size:
                    raise UnsatisfiableRange(416)
                total = meta.size - offset
                if_match = meta.etag
                ok()
                if total > window:
                    raise RacerError("destination is smaller than the range")
                if total == 0:
                    return self._rpc(HOST, request, staging.fileno(), 0, chunk, None, offset, None, if_match, None, ok)
            first_result = None
            pos = 0
            pages = 0
            while pos < total:
                n = min(chunk, total - pos)
                base = offset + pos

                def frame(o: int, size: int, base=base) -> None:
                    dest.copy_from_host(dest_offset + (o - offset), staging.view[o - base : o - base + size])
                    if on_page:
                        on_page(o, size)

                r = self._rpc(HOST, request, staging.fileno(), 0, n, None, base, n, if_match, frame, ok)
                if first_result is None:
                    first_result = r
                    if_match = r.metadata.etag
                pages += r.pages
                pos += n
            assert first_result is not None
            return ReadResult(first_result.metadata, offset, offset + total, HOST, pages)
        finally:
            self._give_staging(staging)

    def _gpu_via_stream(self, request, dest, dest_offset, window, offset, length, if_match, on_page, ok) -> ReadResult:
        staging = self._take_staging(_dest_device(dest))
        try:

            def write(o: int, piece: memoryview) -> None:
                dest.copy_from_host(dest_offset + (o - offset), piece)

            return self._stream(request, offset, length, if_match, staging.view, write, on_page, window, ok)
        finally:
            self._give_staging(staging)


class _Skip(Exception):
    """The destination cannot use this mode. Nothing is remembered."""
