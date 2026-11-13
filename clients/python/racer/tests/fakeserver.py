# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""A small fake Racer client-socket server for tests."""

from __future__ import annotations

import array
import fcntl
import json
import os
import socket
import stat
import threading
from dataclasses import dataclass, field

import racer
import racer._wire as wire
from fakeprefetch import PATH as PREFETCH_PATH
from fakeprefetch import FakePrefetches


@dataclass
class Obj:
    data: bytes
    etag: str = '"v1"'
    content_type: str | None = None
    expires_ms: int = 1_700_000_000_000


@dataclass
class Seen:
    method: str
    target: str
    headers: dict
    fds: int
    fd_kind: str | None = None
    credits: list = field(default_factory=list)
    body: bytes = b""


class FakeRacer:
    """Serves HEAD, stream Get, host-read, gpu-read, and prefetch on a Unix socket.

    Prefetch tickets live in self.prefetches (see fakeprefetch.FakePrefetches).

    old=True mimics a dataplane that predates host-read/gpu-read: the new
    paths fail key parsing and get 400. gpu_status is the answer to gpu-read
    (501 by default, the "no usable GPU" case). gpu_write, when set, is
    called as gpu_write(fd, dest_offset, bytes) to emulate the GPU write.
    """

    def __init__(self, path: str) -> None:
        self.path = path
        self.objects: dict[str, Obj] = {}
        self.old = False
        self.gpu_status = 501
        self.gpu_write = None
        self.host_status: int | None = None
        self.bad_frame = False
        self.truncate = False
        self.seen: list[Seen] = []
        self.prefetches = FakePrefetches()
        # Answer to GET /v1/capabilities; None mimics a dataplane without it (404).
        self.capabilities = {"schema": 1, "delivery": ["gpu", "host", "stream"], "gpu_uuids": [], "page_bytes": wire.PAGE_SIZE}
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.bind(path)
        self.sock.listen(16)
        self._stop = False
        self._thread = threading.Thread(target=self._serve, daemon=True)
        self._thread.start()

    def close(self) -> None:
        self._stop = True
        try:
            c = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            c.connect(self.path)
            c.close()
        except OSError:
            # The connect only wakes the accept loop; if it fails, the
            # server is already gone or stopping, so there is nothing to do.
            pass
        self._thread.join(5)
        self.sock.close()

    def _serve(self) -> None:
        while not self._stop:
            conn, _ = self.sock.accept()
            if self._stop:
                conn.close()
                return
            threading.Thread(target=self._handle, args=(conn,), daemon=True).start()

    # ------------------------------------------------------------------

    def _recv_head(self, conn):
        buf = b""
        fds: list[int] = []
        while b"\r\n\r\n" not in buf:
            data, anc, flags, _ = conn.recvmsg(65536, socket.CMSG_SPACE(4 * 8), socket.MSG_CMSG_CLOEXEC)
            for level, typ, cdata in anc:
                if level == socket.SOL_SOCKET and typ == socket.SCM_RIGHTS:
                    a = array.array("i")
                    a.frombytes(cdata[: len(cdata) - len(cdata) % 4])
                    fds.extend(a)
            if not data:
                return None, None, fds
            buf += data
        head, _, rest = buf.partition(b"\r\n\r\n")
        return head.decode("utf-8", "surrogateescape"), rest, fds

    @staticmethod
    def _send(conn, status, headers, body=b""):
        lines = [f"HTTP/1.1 {status} X"] + [f"{k}: {v}" for k, v in headers]
        conn.sendall(("\r\n".join(lines) + "\r\n\r\n").encode() + body)

    def _error(self, conn, status, size=None):
        h = [("Content-Length", "0")]
        if status == 405:
            h.append(("Allow", "HEAD, POST"))
        if status == 416:
            h.append(("Content-Range", f"bytes */{size}"))
        self._send(conn, status, h)

    def _handle(self, conn) -> None:
        fds: list[int] = []
        try:
            head, rest, fds = self._recv_head(conn)
            if head is None:
                return
            lines = head.split("\r\n")
            method, target, _ = lines[0].split(" ")
            headers = {}
            for line in lines[1:]:
                k, _, v = line.partition(":")
                headers[k.strip().lower()] = v.strip()
            kind = None
            if len(fds) == 1:
                kind = "regular" if stat.S_ISREG(os.fstat(fds[0]).st_mode) else "other"
            seen = Seen(method, target, headers, len(fds), kind)
            self.seen.append(seen)
            self._dispatch(conn, method, target, headers, fds, seen, rest)
        except OSError:
            pass  # the client went away, as tests for bad frames expect
        finally:
            for fd in fds:
                os.close(fd)
            conn.close()

    def _dispatch(self, conn, method, target, headers, fds, seen, rest):
        path = target.partition("?")[0]
        if headers.get("host") == "racer" and (path == PREFETCH_PATH or path.startswith(PREFETCH_PATH + "/")):
            early = self.prefetches.check_head(method, target, headers)
            if early is not None:
                self._send(conn, *early)
                return
            body = rest or b""
            try:
                want = int(headers.get("content-length", "0"))
            except ValueError:
                want = 0
            want = min(want, self.prefetches.max_body)
            while len(body) < want:
                got = conn.recv(65536)
                if not got:
                    return
                body += got
            seen.body = body
            status, h, payload = self.prefetches.handle(method, target, headers, body)
            self._send(conn, status, h, payload)
            return
        if headers.get("host") == "racer" and path == "/v1/capabilities":
            if method != "GET":
                self._error(conn, 405)
            elif self.capabilities is None:
                self._error(conn, 404)
            else:
                body = json.dumps(self.capabilities).encode()
                self._send(
                    conn,
                    200,
                    [("Content-Type", "application/json"), ("Content-Length", str(len(body))), ("Connection", "close")],
                    body,
                )
            return
        if headers.get("host") != "racer" or not target.startswith(wire.OBJECT_PREFIX):
            self._error(conn, 400)
            return
        tail = target[len(wire.OBJECT_PREFIX) :]
        key, _, mode = tail.partition("/")
        if mode and (self.old or mode not in ("host-read", "gpu-read")):
            self._error(conn, 400)
            return
        # Order follows the dataplane: RequestParser::parse
        # (wire/src/object/client.rs), then direct::open_sink and
        # HostSink::open (src/client/direct.rs), then the object read.
        if mode and method != "POST":
            self._error(conn, 405)
            return
        if mode:
            if headers.get("content-length") != "0":
                self._error(conn, 400)
                return
            try:
                off = wire.decimal(headers.get("racer-destination-offset"))
                length = wire.decimal(headers.get("racer-destination-length"))
                alloc = wire.decimal(headers.get("racer-allocation-size")) if mode == "gpu-read" else None
            except racer.RacerError:
                self._error(conn, 400)
                return
            if length == 0 or off + length > wire.MAX_WIRE_INTEGER or (alloc is not None and off + length > alloc):
                self._error(conn, 400)
                return
        if fds and not mode:
            self._error(conn, 400)
            return
        if mode and len(fds) != 1:
            self._error(conn, 400)
            return
        if mode == "gpu-read" and self.gpu_status != 200:
            self._error(conn, self.gpu_status)
            return
        if mode == "host-read" and self.host_status:
            self._error(conn, self.host_status)
            return
        if mode == "host-read":
            flags = fcntl.fcntl(fds[0], fcntl.F_GETFL)
            if seen.fd_kind != "regular" or flags & os.O_ACCMODE == os.O_RDONLY or flags & os.O_APPEND:
                self._error(conn, 400)
                return
        obj = self.objects.get(key)
        if obj is None:
            self._error(conn, 404)
            return
        if "if-match" in headers and headers["if-match"] != obj.etag:
            self._error(conn, 412)
            return
        meta = [("ETag", obj.etag), ("Racer-Expires-At", str(obj.expires_ms))]
        if obj.content_type:
            meta.append(("Racer-Content-Type", obj.content_type))
        size = len(obj.data)
        if method == "HEAD":
            self._send(
                conn, 200, meta + [("Content-Length", str(size)), ("Content-Type", "application/octet-stream")]
            )
            return
        if method != "POST":
            self._error(conn, 405)
            return
        first, end = 0, size
        if "range" in headers:
            spec = headers["range"][len("bytes=") :]
            a, _, b = spec.partition("-")
            first = int(a)
            end = min(size, int(b) + 1) if b else size
            if first >= size and size > 0 or first > size:
                self._error(conn, 416, size)
                return
        pages = wire.page_count(first, end)
        sel = meta + [
            ("Content-Type", "application/octet-stream"),
            ("Racer-Object-Length", str(size)),
            ("Racer-Range-Start", str(first)),
            ("Racer-Range-End", str(end)),
            ("Connection", "close"),
        ]
        if mode:
            if length < end - first:
                self._error(conn, 400)
                return
            self._send(
                conn,
                200,
                sel
                + [
                    ("Racer-Delivery", "host" if mode == "host-read" else "gpu"),
                    ("Content-Length", str(wire.FRAME_SIZE * (pages + 1))),
                ],
            )
            for f in self._page_frames(first, end):
                chunk = obj.data[f.offset : f.offset + f.length]
                if mode == "host-read":
                    os.pwrite(fds[0], chunk, off + f.offset - first)
                else:
                    self.gpu_write(fds[0], off + f.offset - first, chunk)
                conn.sendall(wire.encode_frame(f.kind, f.number, f.offset, f.length))
            if self.truncate:
                return
            conn.sendall(wire.encode_frame(wire.COMPLETE_FRAME, pages, end - first, 0))
            return
        body = end - first + wire.FRAME_SIZE * (pages + 1)
        self._send(conn, 200, sel + [("Content-Length", str(body))])
        credits = rest or b""
        inflight = 0
        for i, f in enumerate(self._page_frames(first, end)):
            number = f.number + (1 if self.bad_frame and i == 1 else 0)
            conn.sendall(wire.encode_frame(f.kind, number, f.offset, f.length))
            conn.sendall(obj.data[f.offset : f.offset + f.length])
            inflight += 1
            if inflight >= 2:
                credits = self._credit(conn, credits, seen)
                inflight -= 1
        while inflight:
            credits = self._credit(conn, credits, seen)
            inflight -= 1
        if self.truncate:
            return
        conn.sendall(wire.encode_frame(wire.COMPLETE_FRAME, pages, end - first, 0))

    def _credit(self, conn, buf, seen):
        while len(buf) < wire.CREDIT_SIZE:
            got = conn.recv(64)
            if not got:
                raise ConnectionError("client closed")
            buf += got
        seen.credits.append(wire.decode_credit(buf[: wire.CREDIT_SIZE]))
        return buf[wire.CREDIT_SIZE :]

    @staticmethod
    def _page_frames(first, end):
        if end <= first:
            return
        n = first // wire.PAGE_SIZE
        o = first
        while o < end:
            e = min(end, (n + 1) * wire.PAGE_SIZE)
            yield wire.Frame(wire.PAGE_FRAME, n, o, e - o)
            n += 1
            o = e
