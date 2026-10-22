# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Golden bytes shared with the dataplane.

Sources: cmd/racer-dataplane/object-wire/src/client.rs (RequestParser::parse,
direct_head, frame) and object-wire/tests/direct.rs.
"""

import array
import fcntl
import os
import socket

import pytest

import racer
from racer import _wire as wire

KEY = "ab" * 32


def test_frame_layout_matches_rust_frame():
    # frame(kind, number, offset, length): u8, u64 BE, u64 BE, u32 BE.
    raw = wire.encode_frame(1, 0x0102030405060708, 0x1112131415161718, 0x21222324)
    assert raw == bytes([1, 1, 2, 3, 4, 5, 6, 7, 8, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x21, 0x22, 0x23, 0x24])
    # Direct completion is frame(2, pages, sent, 0).
    assert wire.decode_frame(wire.encode_frame(2, 3, 2 * wire.PAGE_SIZE + 5, 0)) == wire.Frame(2, 3, 2 * wire.PAGE_SIZE + 5, 0)


def _capture(mode, call):
    """Run one RPC against a socketpair-like listener and return head bytes and fds."""
    import tempfile
    import threading

    d = tempfile.mkdtemp(dir=os.environ.get("TMPDIR"))
    path = os.path.join(d, "s")
    lst = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    lst.bind(path)
    lst.listen(1)
    got = {}

    def serve():
        conn, _ = lst.accept()
        # The first recvmsg must carry the descriptor with the first head bytes.
        data, anc, _, _ = conn.recvmsg(65536, socket.CMSG_SPACE(4 * 8))
        fds = array.array("i")
        for level, typ, cdata in anc:
            if level == socket.SOL_SOCKET and typ == socket.SCM_RIGHTS:
                fds.frombytes(cdata[: len(cdata) - len(cdata) % 4])
        while b"\r\n\r\n" not in data:
            data += conn.recv(65536)
        got["head"] = data
        got["flags"] = [fcntl.fcntl(fd, fcntl.F_GETFL) for fd in fds]
        for fd in fds:
            os.close(fd)
        conn.sendall(b"HTTP/1.1 501 X\r\nContent-Length: 0\r\n\r\n")
        conn.close()

    t = threading.Thread(target=serve)
    t.start()
    c = racer.Client(socket_path=path)
    with pytest.raises(racer.StatusError):
        call(c)
    t.join(5)
    lst.close()
    os.unlink(path)
    os.rmdir(d)
    return got


def test_host_read_request_bytes():
    req = racer.Request(bytes.fromhex(KEY))
    with racer.HostBuffer(4196) as buf:
        got = _capture(
            "host",
            lambda c: c.host_read(req, buf.fileno(), 4096, 100, offset=1, length=3, if_match='"v1"'),
        )
    assert got["head"] == (
        f"POST /v1/objects/{KEY}/host-read HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\n"
        'Range: bytes=1-3\r\nIf-Match: "v1"\r\n'
        "Racer-Destination-Offset: 4096\r\nRacer-Destination-Length: 100\r\n\r\n"
    ).encode()
    # HostSink::open needs a writable, non-append descriptor.
    (flags,) = got["flags"]
    assert flags & os.O_ACCMODE == os.O_RDWR and not flags & os.O_APPEND


def test_gpu_read_request_bytes():
    req = racer.Request(bytes.fromhex(KEY))
    with racer.HostBuffer(1) as buf:
        got = _capture("gpu", lambda c: c.gpu_read(req, buf.fileno(), 4196, 4096, 100))
    assert got["head"] == (
        f"POST /v1/objects/{KEY}/gpu-read HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\n"
        "Racer-Destination-Offset: 4096\r\nRacer-Destination-Length: 100\r\nRacer-Allocation-Size: 4196\r\n\r\n"
    ).encode()
    assert len(got["flags"]) == 1


def test_direct_head_from_rust_is_accepted():
    # direct_head for a 3-byte range of an object of 2 pages + 5 bytes:
    # the Get head with Content-Length 21 * (pages + 1) and Racer-Delivery
    # inserted before Connection.
    length = 2 * wire.PAGE_SIZE + 5
    raw = (
        'HTTP/1.1 200 OK\r\nETag: "v1"\r\nRacer-Expires-At: 1700000000000\r\n'
        "Content-Type: application/octet-stream\r\nRacer-Object-Length: %d\r\n"
        "Racer-Range-Start: 3\r\nRacer-Range-End: 6\r\nContent-Length: 42\r\n"
        "Racer-Delivery: host\r\nConnection: close\r\n\r\n" % length
    ).encode()
    h = wire.parse_response_head(raw)
    from racer.client import Client

    meta, first, end = Client._check_selection(h, 3, 3, None)
    assert (meta.size, first, end) == (length, 3, 6)
    assert wire.decimal(h.get("content-length")) == wire.FRAME_SIZE * (wire.page_count(first, end) + 1)
    seq = wire.Sequence(first, end)
    seq.accept(wire.decode_frame(wire.encode_frame(1, 0, 3, 3)))
    seq.accept(wire.decode_frame(wire.encode_frame(2, 1, 3, 0)))
    assert seq.complete
