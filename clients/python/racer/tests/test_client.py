# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
import os

import pytest
from conftest import FakeGpu
from fakeserver import Obj

import racer
from racer import _wire as wire
from racer.client import _mode_state

DATA = bytes(range(256)) * 50  # 12800 bytes: 4 pages of 4096 (last partial)


def add(server, data=DATA, etag='"v1"', name="obj"):
    req = racer.Request(racer.object_request("bucket", name).key)
    server.objects[req.hex] = Obj(data, etag, "text/plain")
    return req


def client(server, **kw):
    kw.setdefault("staging_bytes", 3 * 4096)
    return racer.Client(socket_path=server.path, **kw)


def test_socket_path(monkeypatch):
    monkeypatch.delenv("RACER_SOCKET", raising=False)
    assert racer.socket_path_for("c1") == "/run/racer/c1/client/socket"
    assert racer.socket_path_for("c1", "/x") == "/x"
    monkeypatch.setenv("RACER_SOCKET", "/env")
    assert racer.socket_path_for("c1") == "/env"
    monkeypatch.delenv("RACER_SOCKET")
    with pytest.raises(racer.RacerError):
        racer.socket_path_for(None)
    with pytest.raises(racer.RacerError):
        racer.socket_path_for("a/b")


def test_stat(server):
    req = add(server)
    m = client(server).stat(req)
    assert (m.etag, m.size, m.content_type, m.expires_at_ms) == ('"v1"', len(DATA), "text/plain", 1_700_000_000_000)
    seen = server.seen[-1]
    assert seen.method == "HEAD" and seen.headers["host"] == "racer"
    assert seen.headers["racer-metadata"] == req.metadata if req.metadata else True
    with pytest.raises(racer.VersionUnavailable):
        client(server).stat(req, if_match='"v2"')
    with pytest.raises(racer.NotFound):
        client(server).stat(racer.Request(b"\0" * 32))


def test_stream_read_full_and_range(server):
    req = add(server)
    c = client(server)
    assert c.read(req) == DATA
    seen = server.seen[-1]
    assert seen.headers["racer-page-credits"] == "2"
    assert seen.headers["racer-byte-credits"] == str(2 * 16 * 1024 * 1024)
    assert "range" not in seen.headers
    # Every page's credit comes back, including the last one.
    assert seen.credits == [(0, 4096), (1, 4096), (2, 4096), (3, 512)]
    assert c.read(req, offset=100, length=5000) == DATA[100:5100]
    assert server.seen[-1].headers["range"] == "bytes=100-5099"
    assert c.read(req, offset=5000) == DATA[5000:]
    assert server.seen[-1].headers["range"] == "bytes=5000-"


def test_stream_empty_object(server):
    req = add(server, data=b"")
    assert client(server).read(req) == b""


def test_stream_short_object_is_unsatisfiable(server):
    req = add(server)
    with pytest.raises(racer.UnsatisfiableRange):
        client(server).read(req, offset=len(DATA) - 10, length=100)
    with pytest.raises(racer.UnsatisfiableRange):
        client(server).read(req, offset=len(DATA) + 1)


def test_stream_bad_frame(server):
    req = add(server)
    server.bad_frame = True
    with pytest.raises(racer.ProtocolError):
        client(server).read(req)


def test_stream_truncated(server):
    req = add(server)
    server.truncate = True
    with pytest.raises(racer.TruncatedError):
        client(server).read(req)


def test_if_match_sent(server):
    req = add(server)
    assert client(server).read(req, if_match='"v1"') == DATA
    assert server.seen[-1].headers["if-match"] == '"v1"'
    with pytest.raises(racer.VersionUnavailable):
        client(server).read(req, if_match='"v0"')
    with pytest.raises(racer.RacerError):
        client(server).read(req, if_match="unquoted")


def test_host_read_passes_memfd(server):
    req = add(server)
    pages = []
    with racer.HostBuffer(len(DATA) + 10) as buf:
        r = client(server).read_into_host(req, buf, 10, on_page=lambda o, n: pages.append((o, n)))
        assert r.delivery == racer.HOST and r.pages == 4
        assert bytes(buf.view[10:]) == DATA
    seen = server.seen[-1]
    assert seen.target.endswith("/host-read")
    assert seen.fds == 1 and seen.fd_kind == "regular"
    assert seen.headers["racer-destination-offset"] == "10"
    assert seen.headers["racer-destination-length"] == str(len(DATA))
    assert "racer-allocation-size" not in seen.headers
    assert pages == [(0, 4096), (4096, 4096), (8192, 4096), (12288, 512)]


def test_host_read_range(server):
    req = add(server)
    with racer.HostBuffer(5000) as buf:
        r = client(server).read_into_host(req, buf, offset=4000, length=5000)
        assert (r.first, r.end) == (4000, 9000)
        assert bytes(buf.view) == DATA[4000:9000]


def test_old_dataplane_falls_back_to_stream_and_remembers(server):
    req = add(server)
    server.old = True
    c = client(server)
    with racer.HostBuffer(len(DATA)) as buf:
        r = c.read_into_host(req, buf)
        assert r.delivery == racer.STREAM
        assert bytes(buf.view) == DATA
        assert c.mode_state(racer.HOST) == "unsupported"
        n = len(server.seen)
        r = c.read_into_host(req, buf)
        assert r.delivery == racer.STREAM
        # The second read goes straight to stream mode.
        assert len(server.seen) == n + 1 and "/host-read" not in server.seen[-1].target


def test_missing_object_does_not_disable_mode(server):
    c = client(server)
    with racer.HostBuffer(10) as buf:
        with pytest.raises(racer.NotFound):
            c.read_into_host(racer.Request(b"\1" * 32), buf)
    assert c.mode_state(racer.HOST) is None


def test_supported_mode_errors_are_not_fallbacks(server):
    req = add(server)
    c = client(server)
    with racer.HostBuffer(len(DATA)) as buf:
        c.read_into_host(req, buf)
        assert c.mode_state(racer.HOST) == "supported"
        server.host_status = 400
        with pytest.raises(racer.StatusError):
            c.read_into_host(req, buf)


def test_plain_buffer_uses_stream(server):
    req = add(server)
    out = bytearray(len(DATA))
    r = client(server).read_into_host(req, out)
    assert r.delivery == racer.STREAM and bytes(out) == DATA
    assert server.seen[-1].fds == 0


def test_destination_too_small(server):
    req = add(server)
    with racer.HostBuffer(100) as buf:
        # host-read answers 400, which looks like an old dataplane, so the
        # stream fallback reports the size mismatch.
        c = client(server)
        with pytest.raises(racer.RacerError, match="smaller"):
            c.read_into_host(req, buf)
        # The refusal was about this read, not about host-read support.
        assert c.mode_state(racer.HOST) is None
    out = bytearray(100)
    with pytest.raises(racer.RacerError):
        client(server).read_into_host(req, out)


def test_gpu_read_success(server, gpu_writer):
    req = add(server)
    server.gpu_status = 200
    server.gpu_write = gpu_writer
    g = FakeGpu(len(DATA) + 100)
    try:
        r = client(server).read_into_gpu(req, g, 100)
        assert r.delivery == racer.GPU
        assert g.read(100, len(DATA)) == DATA
        seen = server.seen[-1]
        assert seen.target.endswith("/gpu-read") and seen.fds == 1
        assert seen.headers["racer-allocation-size"] == str(g.size)
        assert seen.headers["racer-destination-length"] == str(len(DATA))
        assert g.copies == 0
    finally:
        g.close()


def test_gpu_falls_back_to_host_chunks(server):
    req = add(server)
    g = FakeGpu(len(DATA))
    pages = []
    try:
        c = client(server)
        r = c.read_into_gpu(req, g, on_page=lambda o, n: pages.append((o, n)))
        assert r.delivery == racer.HOST and (r.first, r.end, r.pages) == (0, len(DATA), 4)
        assert g.read(0, len(DATA)) == DATA
        assert c.mode_state(racer.GPU) == "unsupported"
        assert pages == [(0, 4096), (4096, 4096), (8192, 4096), (12288, 512)]
        reads = [s for s in server.seen if s.target.endswith("/host-read")]
        # Staging holds 3 pages, so the object takes two host-reads.
        assert len(reads) == 2 and reads[1].headers["if-match"] == '"v1"'
    finally:
        g.close()


def test_gpu_falls_back_to_stream_on_old_dataplane(server):
    req = add(server)
    server.old = True
    g = FakeGpu(len(DATA))
    try:
        c = client(server)
        r = c.read_into_gpu(req, g, offset=10, length=9000)
        assert r.delivery == racer.STREAM
        assert g.read(0, 9000) == DATA[10:9010]
        assert c.mode_state(racer.GPU) == "unsupported"
        assert c.mode_state(racer.HOST) == "unsupported"
    finally:
        g.close()


def test_gpu_not_exportable_skips_without_memory(server):
    req = add(server)
    g = FakeGpu(len(DATA), exportable=False)
    try:
        c = client(server)
        r = c.read_into_gpu(req, g)
        assert r.delivery == racer.HOST
        assert c.mode_state(racer.GPU) is None
    finally:
        g.close()


def test_modes_remembered_per_socket(server, tmp_path):
    req = add(server)
    server.old = True
    with racer.HostBuffer(len(DATA)) as buf:
        client(server).read_into_host(req, buf)
    assert _mode_state[server.path][racer.HOST] == "unsupported"
    other = racer.Client(socket_path=str(tmp_path / "other"))
    assert other.mode_state(racer.HOST) is None


def test_send_fd_with_head(server):
    """The descriptor arrives with the head even when nothing else is sent."""
    req = add(server)
    with racer.HostBuffer(len(DATA)) as buf:
        c = client(server)
        c.host_read(req, buf.fileno(), 0, len(DATA))
        assert server.seen[-1].fds == 1
        assert bytes(buf.view) == DATA
    # Our copy of the descriptor is still open and owned by the caller.
    fd = os.memfd_create("x")
    c.host_read(req, fd, 0, len(DATA))
    os.fstat(fd)
    os.close(fd)


def test_gpu_read_window_check(server):
    req = add(server)
    with pytest.raises(racer.RacerError):
        client(server).gpu_read(req, 0, 100, 50, 60)


def test_frame_sequence_rules(small_pages):
    s = wire.Sequence(100, 9000)
    with pytest.raises(racer.ProtocolError):
        s.accept(wire.Frame(1, 0, 0, 4096))  # starts before range
    s = wire.Sequence(100, 9000)
    s.accept(wire.Frame(1, 0, 100, 3996))
    with pytest.raises(racer.ProtocolError):
        s.accept(wire.Frame(1, 2, 8192, 808))  # skipped a page
    s = wire.Sequence(100, 9000)
    s.accept(wire.Frame(1, 0, 100, 3996))
    s.accept(wire.Frame(1, 1, 4096, 4096))
    with pytest.raises(racer.ProtocolError):
        s.accept(wire.Frame(2, 3, 8900, 0))  # completion too early
    s.accept(wire.Frame(1, 2, 8192, 808))
    with pytest.raises(racer.ProtocolError):
        s.accept(wire.Frame(2, 3, 8900, 1))  # nonzero length
    s.accept(wire.Frame(2, 3, 8900, 0))
    with pytest.raises(racer.ProtocolError):
        s.accept(wire.Frame(2, 3, 8900, 0))  # after completion
    assert wire.decode_frame(wire.encode_frame(1, 2, 3, 4)) == wire.Frame(1, 2, 3, 4)
    assert wire.encode_credit(1, 2) == b"\0" * 7 + b"\1" + b"\0\0\0\2"


def test_decimal():
    assert wire.decimal("0") == 0
    for bad in ["", "01", "-1", "1a", "9" * 20, str(1 << 63)]:
        with pytest.raises(racer.ProtocolError):
            wire.decimal(bad)


def test_response_head_parsing():
    h = wire.parse_response_head(b"HTTP/1.1 200 OK\r\nETag: \"a\"\r\nConnection: keep-alive, close\r\n\r\n")
    assert h.status == 200 and h.get("ETag") == '"a"' and wire.connection_close(h)
    with pytest.raises(racer.ProtocolError):
        wire.parse_response_head(b"HTTP/1.1 200 OK\r\nETag: a\r\nETag: b\r\n\r\n")
    with pytest.raises(racer.ProtocolError):
        wire.parse_response_head(b"HTTP/1.0 200 OK\r\n\r\n")
    with pytest.raises(racer.ProtocolError):
        wire.check_error_status(wire.parse_response_head(b"HTTP/1.1 404 N\r\nContent-Length: 3\r\n\r\n"))
    with pytest.raises(racer.ProtocolError):
        wire.check_error_status(wire.parse_response_head(b"HTTP/1.1 416 N\r\nContent-Length: 0\r\n\r\n"))
