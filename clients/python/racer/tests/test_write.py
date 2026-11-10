# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Write API tests against the fake server (designs/racer-writes.md, WP9)."""

import io
import os
import threading

import pytest

import racer
import racer._wire as wire
from racer import errors

META = '{"schema":1,"bucket":"b","key":"k"}'
ETAG_LEN = len('"racer-"') + 32


def req(n=1, metadata=META):
    return racer.Request(bytes([n]) + bytes(31), metadata)


@pytest.fixture
def client(server):
    return racer.Client(socket_path=server.path, timeout=10)


def test_write_round_trip(server, client, small_pages):
    data = os.urandom(3 * small_pages + 17)
    r = client.write(req(), data, replicas=3, upload="manual", hold_seconds=60, content_type="a/b")
    assert r.etag.startswith('"racer-') and len(r.etag) == ETAG_LEN
    assert r.upload_state == "manual"
    seen = server.seen[-1]
    assert seen.method == "PUT"
    assert seen.headers["if-none-match"] == "*"
    assert seen.headers["racer-durability"] == "replicas=3"
    assert seen.headers["racer-upload"] == "manual"
    assert seen.headers["racer-hold-seconds"] == "60"
    assert seen.headers["racer-content-type"] == "a/b"
    assert seen.headers["content-length"] == str(len(data))
    assert client.read(req()) == data
    m = client.stat(req())
    assert (m.etag, m.size, m.upload_state, m.content_type) == (r.etag, len(data), "manual", "a/b")
    assert client.persist(req()) == "pending"
    assert client.stat(req()).upload_state == "pending"
    client.delete(req())
    with pytest.raises(racer.NotFound):
        client.stat(req())
    with pytest.raises(errors.WriteNotFound) as e:
        client.delete(req())
    assert isinstance(e.value, racer.NotFound)


def test_write_defaults_send_no_option_headers(server, client):
    client.write(req(), b"x")
    h = server.seen[-1].headers
    for name in ("racer-durability", "racer-upload", "racer-hold-seconds", "racer-content-type"):
        assert name not in h
    assert h["racer-metadata"] == META
    assert server.written[req().hex].state == "pending"


def test_stat_of_origin_object_has_no_upload_state(server, client):
    from fakeserver import Obj

    server.objects[req().hex] = Obj(b"abc")
    assert client.stat(req()).upload_state is None


@pytest.mark.parametrize("kind", ["bytes", "bytearray", "memoryview", "file", "stream"])
def test_write_data_kinds(server, client, kind):
    data = os.urandom(10000)
    if kind == "bytes":
        src, length = data, None
    elif kind == "bytearray":
        src, length = bytearray(data), None
    elif kind == "memoryview":
        src, length = memoryview(data), None
    elif kind == "file":
        src = io.BytesIO(b"skip" + data)
        src.read(4)
        length = None
    else:
        r, w = os.pipe()
        threading.Thread(target=lambda: (os.write(w, data), os.close(w)), daemon=True).start()
        src, length = os.fdopen(r, "rb"), len(data)
    client.write(req(), src, length=length, upload="never")
    assert client.read(req()) == data


def test_write_prefix_of_buffer(server, client):
    client.write(req(), b"abcdef", length=3, upload="never")
    assert client.read(req()) == b"abc"


def test_large_stream(server, client, small_pages):
    data = os.urandom(5 << 20)
    client.write(req(), io.BytesIO(data), upload="never")
    assert client.read(req()) == data


def test_create_once(server, client):
    client.write(req(), b"a")
    with pytest.raises(racer.Exists) as e:
        client.write(req(), b"b")
    assert e.value.status == 412 and e.value.op == "put"
    server.origin_keys.add(req(2).hex)
    with pytest.raises(racer.Exists):
        client.write(req(2), b"b")


def test_writes_disabled(server, client):
    server.writes_enabled = False
    with pytest.raises(racer.WritesDisabled):
        client.write(req(), b"a")
    with pytest.raises(racer.WritesDisabled):
        client.persist(req())
    with pytest.raises(racer.WritesDisabled):
        client.delete(req())


@pytest.mark.parametrize(
    "status,code,cls,also",
    [
        (503, "dirty-cap", racer.Busy, errors.Unavailable),
        (502, "upload", racer.UploadFailed, racer.WriteError),
        (409, "conflict", racer.Conflict, racer.WriteError),
        (428, "", errors.WriteInvalid, errors.InvalidArgument),
        (413, "", errors.WriteInvalid, errors.InvalidArgument),
        (411, "", errors.WriteInvalid, errors.InvalidArgument),
        (504, "", errors.WriteUnavailable, errors.Unavailable),
        (500, "", racer.WriteError, racer.StatusError),
    ],
)
def test_rejections(server, client, status, code, cls, also):
    server.write_reject = lambda kind, key, headers: (status, code, 7)
    with pytest.raises(cls) as e:
        client.write(req(), b"abc")
    assert isinstance(e.value, also)
    assert (e.value.status, e.value.code, e.value.retry_after) == (status, code, 7.0)
    assert not isinstance(e.value, racer.Exists)


def test_early_rejection_with_large_body(server, client):
    server.write_reject = lambda kind, key, headers: (503, "owners", 1)
    with pytest.raises(racer.Busy):
        client.write(req(), io.BytesIO(bytes(8 << 20)))


def test_retry_busy(server, monkeypatch):
    sleeps = []
    monkeypatch.setattr("racer.client.time.sleep", sleeps.append)
    client = racer.Client(socket_path=server.path, timeout=10)
    calls = []

    def reject(kind, key, headers):
        calls.append(kind)
        return (503, "dirty-cap", 2) if len(calls) < 3 else None

    server.write_reject = reject
    data = io.BytesIO(b"hello")
    client.write(req(), data, retry_busy=60)
    assert len(calls) == 3 and sleeps == [2.0, 2.0]
    assert client.read(req()) == b"hello"


def test_retry_busy_needs_opt_in_and_replayable_data(server, monkeypatch):
    monkeypatch.setattr("racer.client.time.sleep", lambda s: None)
    client = racer.Client(socket_path=server.path, timeout=10)
    calls = []

    def reject(kind, key, headers):
        calls.append(kind)
        return (503, "dirty-cap", None)

    server.write_reject = reject
    with pytest.raises(racer.Busy):
        client.write(req(), b"x")
    assert len(calls) == 1
    r, w = os.pipe()
    os.write(w, b"x")
    os.close(w)
    with pytest.raises(racer.Busy), os.fdopen(r, "rb") as f:
        client.write(req(), f, length=1, retry_busy=60)
    assert len(calls) == 2


def test_retry_busy_respects_budget(server, monkeypatch):
    now = [0.0]
    monkeypatch.setattr("racer.client.time.sleep", lambda s: now.__setitem__(0, now[0] + s))
    client = racer.Client(socket_path=server.path, timeout=10, clock=lambda: now[0])
    server.write_reject = lambda kind, key, headers: (503, "dirty-cap", 5)
    with pytest.raises(racer.Busy):
        client.write(req(), b"x", retry_busy=12)
    assert now[0] == 10.0


@pytest.mark.parametrize(
    "kwargs",
    [
        {"replicas": 4},
        {"replicas": 0},
        {"replicas": True},
        {"durability_origin": True, "replicas": 2},
        {"durability_origin": True, "upload": "never"},
        {"durability_origin": True, "upload": "manual"},
        {"upload": "later"},
        {"hold_seconds": 0},
        {"hold_seconds": wire.MAX_HOLD_SECONDS + 1},
        {"content_type": " x"},
    ],
)
def test_invalid_options(server, client, kwargs):
    with pytest.raises(racer.RacerError):
        client.write(req(), b"x", **kwargs)
    assert server.seen == []


def test_invalid_data(server, client, small_pages):
    with pytest.raises(racer.RacerError):
        client.write(req(), b"")
    with pytest.raises(racer.RacerError):
        client.write(req(), b"ab", length=3)
    with pytest.raises(racer.RacerError):
        client.write(req(), io.BytesIO(b""), length=wire.max_object_bytes() + 1)
    with pytest.raises(racer.RacerError):
        client.write(req(metadata='{"schema":1,"bucket":"b","key":"k","versionId":"1"}'), b"x")
    r, w = os.pipe()
    os.close(w)
    with pytest.raises(racer.RacerError), os.fdopen(r, "rb") as f:
        client.write(req(), f)  # unseekable without a length
    assert server.seen == []


def test_short_stream_is_not_created(server, client):
    with pytest.raises(racer.RacerError):
        client.write(req(), io.BytesIO(b"ab"), length=3)
    assert req().hex not in server.written


def test_origin_durability(server, client):
    r = client.write(req(), b"x", durability_origin=True)
    assert r.upload_state == "committed"
    assert server.seen[-1].headers["racer-durability"] == "origin"
    with pytest.raises(racer.Conflict) as e:
        client.delete(req())
    assert e.value.code == "uploaded"


def test_persist_states(server, client):
    client.write(req(), b"x", upload="never")
    with pytest.raises(racer.Conflict) as e:
        client.persist(req())
    assert e.value.code == "not-manual"
    with pytest.raises(racer.NotFound):
        client.persist(req(2))
    client.write(req(3), b"x", upload="auto")
    assert client.persist(req(3)) == "pending"


def test_metadata_required_unless_never(server, client):
    with pytest.raises(errors.WriteInvalid):
        client.write(req(metadata=None), b"x")
    client.write(req(metadata=None), b"x", upload="never")


def test_host_write(server, client, small_pages):
    data = os.urandom(2 * small_pages + 5)
    with racer.HostBuffer(len(data) + 100) as buf:
        buf.view[100 : 100 + len(data)] = data
        r = client.host_write(req(), buf.fileno(), 100, len(data), replicas=1, upload="never")
    assert r.upload_state == "never"
    seen = server.seen[-1]
    assert (seen.method, seen.target) == ("POST", wire.OBJECT_PREFIX + req().hex + "/host-write")
    assert seen.fds == 1 and seen.fd_kind == "regular"
    assert seen.headers["content-length"] == "0"
    assert seen.headers["racer-source-offset"] == "100"
    assert seen.headers["racer-source-length"] == str(len(data))
    assert client.read(req()) == data


def test_host_write_rejects(server, client):
    with racer.HostBuffer(10) as buf:
        with pytest.raises(racer.RacerError):
            client.host_write(req(), buf.fileno(), 0, 0)
        with pytest.raises(racer.RacerError):
            client.host_write(req(), buf.fileno(), -1, 1)
        assert server.seen == []
        with pytest.raises(errors.WriteInvalid):
            client.host_write(req(), buf.fileno(), 5, 10)  # past the end of the file
        client.host_write(req(), buf.fileno(), 0, 10, upload="never")
        with pytest.raises(racer.Exists):
            client.host_write(req(), buf.fileno(), 0, 10, upload="never")


def test_host_write_retry_busy(server, monkeypatch):
    monkeypatch.setattr("racer.client.time.sleep", lambda s: None)
    client = racer.Client(socket_path=server.path, timeout=10)
    calls = []

    def reject(kind, key, headers):
        calls.append(kind)
        return (503, "dirty-cap", 1) if len(calls) < 2 else None

    server.write_reject = reject
    with racer.HostBuffer(4) as buf:
        buf.view[:4] = b"abcd"
        client.host_write(req(), buf.fileno(), 0, 4, upload="never", retry_busy=30)
    assert calls == ["host-write", "host-write"]


def test_bad_write_responses(tmp_path):
    import socket as s

    path = str(tmp_path / "s")
    lst = s.socket(s.AF_UNIX, s.SOCK_STREAM)
    lst.bind(path)
    lst.listen(4)
    replies = [
        b"HTTP/1.1 201 Created\r\nContent-Length: 0\r\n\r\n",
        b'HTTP/1.1 201 Created\r\nETag: "racer-x"\r\nRacer-Upload-State: bogus\r\nContent-Length: 0\r\n\r\n',
        b'HTTP/1.1 201 Created\r\nETag: "racer-x"\r\nRacer-Upload-State: never\r\nContent-Length: 2\r\n\r\nok',
        b"HTTP/1.1 204 No Content\r\n\r\n",
    ]

    def serve():
        for reply in replies:
            conn, _ = lst.accept()
            buf = b""
            while b"\r\n\r\n" not in buf:
                buf += conn.recv(65536)
            conn.sendall(reply)
            conn.close()

    t = threading.Thread(target=serve, daemon=True)
    t.start()
    client = racer.Client(socket_path=path, timeout=5)
    for _ in replies[:3]:
        with pytest.raises(racer.ProtocolError):
            client.write(req(), b"x", upload="never")
    with pytest.raises(racer.ProtocolError):
        client.persist(req())  # persist must report the upload state
    t.join(5)
    lst.close()
