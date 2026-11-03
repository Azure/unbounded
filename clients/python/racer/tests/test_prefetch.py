# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
import json
import os
import socket
import threading
import time
from datetime import timedelta

import pytest
from fakeprefetch import Fail, Outcome

import racer
from racer.prefetch import encode_body, format_wait, status_query

KEY_A = "a" * 64
KEY_B = "b" * 64


@pytest.fixture
def client(server):
    return racer.Client(socket_path=server.path, timeout=5)


def last(server):
    return server.seen[-1]


class Scripted:
    """Answers each request with a fixed raw response and records requests."""

    def __init__(self, path, response):
        self.path = path
        self.response = response
        self.requests = []
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.bind(path)
        self.sock.listen(4)
        self.thread = threading.Thread(target=self.serve, daemon=True)
        self.thread.start()

    def serve(self):
        while True:
            try:
                conn, _ = self.sock.accept()
            except OSError:
                return
            with conn:
                data = b""
                while b"\r\n\r\n" not in data:
                    chunk = conn.recv(65536)
                    if not chunk:
                        break
                    data += chunk
                head, _, body = data.partition(b"\r\n\r\n")
                for line in head.split(b"\r\n"):
                    if line.lower().startswith(b"content-length:"):
                        n = int(line.split(b":")[1])
                        while len(body) < n:
                            body += conn.recv(65536)
                self.requests.append((head.decode(), body))
                conn.sendall(self.response)

    def close(self):
        self.sock.close()


@pytest.fixture
def scripted(tmp_path):
    made = []

    def make(status, headers=(), body=b""):
        lines = [f"HTTP/1.1 {status} X"] + [f"{k}: {v}" for k, v in headers]
        if body is not None:
            lines.append(f"Content-Length: {len(body)}")
        s = Scripted(str(tmp_path / f"s{len(made)}"), ("\r\n".join(lines) + "\r\n\r\n").encode() + (body or b""))
        made.append(s)
        return s, racer.Client(socket_path=s.path, timeout=5)

    yield make
    for s in made:
        s.close()


def test_request_encoding(scripted):
    status = {
        "id": "e",
        "state": "PARTIAL",
        "reason": None,
        "generation": 2,
        "sequence": 5,
        "settled": True,
        "bytes": {"failed": 8, "future": 1},
        "coverage": {"resident_bytes": 9, "observed_at_ms": 1},
        "refs": [{"index": 1, "key": "k", "etag": None, "offset": 0, "length": 1, "outcome": "failed", "error": "version_changed"}],
        "next_cursor": "c1",
        "future_field": True,
    }
    s, c = scripted(201, [("Content-Type", "application/json")], json.dumps(status).encode())
    spec = racer.PrefetchSpec(
        refs=[racer.PrefetchRef(KEY_A, etag='"v"', offset=1, length=2, metadata="m"), racer.PrefetchRef(KEY_B)],
        cache_uid="u",
        priority="high",
        generation=2,
        sequence=5,
        ttl_seconds=2,
        max_pending_bytes=1 << 30,
        authorization="Bearer x",
    )
    got = c.prefetch("epoch-17", spec)
    head, body = s.requests[0]
    assert head.startswith("PUT /v1/prefetches/epoch-17 HTTP/1.1\r\nHost: racer\r\n")
    assert "Content-Type: application/json\r\n" in head and "Authorization: Bearer x\r\n" in head
    assert body == (
        b'{"cache_uid":"u","priority":"high","generation":2,"sequence":5,"ttl_seconds":2,"max_pending_bytes":1073741824,'
        b'"refs":[{"key":"' + KEY_A.encode() + b'","etag":"\\"v\\"","offset":1,"length":2,"metadata":"m"},{"key":"' + KEY_B.encode() + b'"}]}'
    )
    assert got.created and got.state == "PARTIAL" and got.bytes.failed == 8 and got.coverage.resident_bytes == 9
    assert got.refs[0].error == "version_changed" and got.refs[0].etag is None and got.next_cursor == "c1"

    c.prefetch("min", racer.PrefetchSpec(refs=[racer.PrefetchRef(KEY_A)]))
    head, body = s.requests[1]
    assert "Authorization" not in head
    assert body == b'{"generation":0,"sequence":0,"refs":[{"key":"' + KEY_A.encode() + b'"}]}'

    c.prefetch_status("e", wait=timedelta(seconds=30), coverage=True, refs="failed", cursor="a/b")
    assert s.requests[2][0].startswith("GET /v1/prefetches/e?wait=30s&coverage=1&refs=failed&cursor=a%2Fb HTTP/1.1")
    assert "Content-Length" not in s.requests[2][0]
    c.cancel_prefetch("e")
    assert s.requests[3][0].startswith("DELETE /v1/prefetches/e HTTP/1.1")


def test_format_wait_and_query():
    assert format_wait(0) == "0s"
    assert format_wait(30) == "30s"
    assert format_wait(0.5) == "500ms"
    assert format_wait(1.0001) == "1001ms"
    assert format_wait(timedelta(minutes=2)) == "120s"
    assert status_query() == ""
    assert status_query(wait=0) == ""
    with pytest.raises(racer.RacerError):
        format_wait(-1)
    with pytest.raises(racer.RacerError):
        status_query(refs="some")


@pytest.mark.parametrize(
    "spec",
    [
        racer.PrefetchSpec(),
        racer.PrefetchSpec(refs=[racer.PrefetchRef("ABC")]),
        racer.PrefetchSpec(refs=[racer.PrefetchRef("A" * 64)]),
        racer.PrefetchSpec(refs=[racer.PrefetchRef(KEY_A, etag='W/"x"')]),
        racer.PrefetchSpec(refs=[racer.PrefetchRef(KEY_A, offset=-1)]),
        racer.PrefetchSpec(refs=[racer.PrefetchRef(KEY_A, length=-1)]),
        racer.PrefetchSpec(refs=[racer.PrefetchRef(KEY_A, metadata="a\nb")]),
        racer.PrefetchSpec(refs=[racer.PrefetchRef(KEY_A)], priority="urgent"),
        racer.PrefetchSpec(refs=[racer.PrefetchRef(KEY_A)], generation=1 << 53),
        racer.PrefetchSpec(refs=[racer.PrefetchRef(KEY_A)], sequence=-1),
        racer.PrefetchSpec(refs=[racer.PrefetchRef(KEY_A)], ttl_seconds=-1),
        racer.PrefetchSpec(refs=[racer.PrefetchRef(KEY_A)], authorization="x\r\n"),
    ],
)
def test_invalid_specs(spec):
    with pytest.raises(racer.RacerError):
        encode_body(spec)


@pytest.mark.parametrize("pid", ["", ".", "..", "a/b", "a b", "x" * 129])
def test_invalid_ids(tmp_path, pid):
    c = racer.Client(socket_path=str(tmp_path / "none"))
    for call in (lambda: c.prefetch(pid, racer.PrefetchSpec(refs=[racer.PrefetchRef(KEY_A)])), lambda: c.prefetch_status(pid), lambda: c.cancel_prefetch(pid)):
        with pytest.raises(racer.RacerError):
            call()


@pytest.mark.parametrize(
    "status,headers,body,code,retry",
    [
        (409, [], b'{"code":"stale_sequence","message":"old"}', "stale_sequence", None),
        (429, [("Retry-After", "7")], b'{"code":"ticket_limit","message":"x"}', "ticket_limit", 7.0),
        (429, [("Retry-After", "soon")], b'{"code":"pending_limit","message":"x"}', "pending_limit", None),
        (429, [("Retry-After", "\u00b2")], b'{"code":"ticket_limit","message":"x"}', "ticket_limit", None),
        (429, [("Retry-After", "9" * 400)], b'{"code":"ticket_limit","message":"x"}', "ticket_limit", None),
        (503, [], b"", "", None),
        (500, [("Content-Type", "text/html")], b"<html>", "", None),
    ],
)
def test_error_mapping(scripted, status, headers, body, code, retry):
    _, c = scripted(status, headers, body)
    with pytest.raises(racer.PrefetchError) as exc:
        c.prefetch("a", racer.PrefetchSpec(refs=[racer.PrefetchRef(KEY_A)]))
    assert exc.value.status == status and exc.value.code == code and exc.value.retry_after == retry
    assert isinstance(exc.value, racer.StatusError)


def test_body_without_content_length(scripted):
    _, c = scripted(200, [("Connection", "close")], None)
    with pytest.raises(racer.ProtocolError):
        c.list_prefetches()


def test_protocol_errors(scripted):
    for body in (b"not json", b'{"id":1}', b"[]"):
        _, c = scripted(200, [], body)
        with pytest.raises(racer.ProtocolError):
            c.prefetch_status("a")
    _, c = scripted(200, [], b'{"nope":1}')
    with pytest.raises(racer.ProtocolError):
        c.list_prefetches()


def test_idempotency_table(client, server):
    spec = racer.PrefetchSpec(refs=[racer.PrefetchRef(KEY_A)], generation=1, sequence=5)
    s = client.prefetch("w", spec)
    assert s.created and s.state == "SUCCEEDED" and s.settled
    assert not client.prefetch("w", spec).created
    cases = [
        (racer.PrefetchSpec(refs=[racer.PrefetchRef(KEY_B)], generation=1, sequence=5), "payload_mismatch"),
        (racer.PrefetchSpec(refs=[racer.PrefetchRef(KEY_A)], generation=1, sequence=5, authorization="a"), "payload_mismatch"),
        (racer.PrefetchSpec(refs=[racer.PrefetchRef(KEY_A)], generation=1, sequence=4), "stale_sequence"),
        (racer.PrefetchSpec(refs=[racer.PrefetchRef(KEY_A)], generation=0, sequence=9), "stale_generation"),
    ]
    for bad, code in cases:
        with pytest.raises(racer.PrefetchError) as exc:
            client.prefetch("w", bad)
        assert exc.value.status == 409 and exc.value.code == code
    assert client.prefetch("w", racer.PrefetchSpec(refs=[racer.PrefetchRef(KEY_B)], generation=1, sequence=6)).sequence == 6
    assert client.prefetch("w", racer.PrefetchSpec(refs=[racer.PrefetchRef(KEY_A)], generation=2)).generation == 2
    # Like Racer, the hash covers the body as sent: an explicit default
    # differs from an omitted field.
    with pytest.raises(racer.PrefetchError) as exc:
        client.prefetch("w", racer.PrefetchSpec(refs=[racer.PrefetchRef(KEY_A)], generation=2, priority="normal", ttl_seconds=600))
    assert exc.value.code == "payload_mismatch"

    s = client.cancel_prefetch("w")
    assert s.state == "CANCELLED" and s.reason == "client"
    assert client.prefetch("w", racer.PrefetchSpec(refs=[racer.PrefetchRef(KEY_A)], generation=2, sequence=1)).state == "SUCCEEDED"
    with pytest.raises(racer.PrefetchError) as exc:
        client.cancel_prefetch("missing")
    assert exc.value.status == 404 and exc.value.code == "not_found"


def test_limits(client, server):
    server.prefetches.passthrough = True
    with pytest.raises(racer.PrefetchError) as exc:
        client.prefetch("p", racer.PrefetchSpec(refs=[racer.PrefetchRef(KEY_A)]))
    assert exc.value.code == "passthrough"
    server.prefetches.passthrough = False

    server.prefetches.cache_uid = "uid"
    with pytest.raises(racer.PrefetchError) as exc:
        client.prefetch("p", racer.PrefetchSpec(refs=[racer.PrefetchRef(KEY_A)], cache_uid="other"))
    assert exc.value.code == "cache_uid_mismatch"

    server.prefetches.max_refs = 1
    with pytest.raises(racer.PrefetchError) as exc:
        client.prefetch("p", racer.PrefetchSpec(refs=[racer.PrefetchRef(KEY_A), racer.PrefetchRef(KEY_B)]))
    assert exc.value.status == 413 and exc.value.code == "too_large"

    server.prefetches.max_body = 4096
    with pytest.raises(racer.PrefetchError) as exc:
        client.prefetch("p", racer.PrefetchSpec(refs=[racer.PrefetchRef(KEY_A, metadata="m" * 5000)]))
    assert exc.value.status == 413

    server.prefetches.max_tickets = 1
    client.prefetch("p", racer.PrefetchSpec(refs=[racer.PrefetchRef(KEY_A)]))
    with pytest.raises(racer.PrefetchError) as exc:
        client.prefetch("q", racer.PrefetchSpec(refs=[racer.PrefetchRef(KEY_A)]))
    assert exc.value.status == 429 and exc.value.code == "ticket_limit" and exc.value.retry_after == 5.0
    # A retained finished ticket makes room before 429.
    client.cancel_prefetch("p")
    assert client.prefetch("q", racer.PrefetchSpec(refs=[racer.PrefetchRef(KEY_A)])).created
    with pytest.raises(racer.PrefetchError):
        client.prefetch_status("p")

    server.prefetches.admit = lambda pid, body: Fail(429, "pending_limit", "busy", 9)
    with pytest.raises(racer.PrefetchError) as exc:
        client.prefetch("p", racer.PrefetchSpec(refs=[racer.PrefetchRef(KEY_A)], sequence=1))
    assert exc.value.code == "pending_limit" and exc.value.retry_after == 9.0


def test_ttl_expiry(client, server):
    clock = [1000.0]
    server.prefetches.now = lambda: clock[0]
    server.prefetches.max_ttl = 10
    s = client.prefetch("t", racer.PrefetchSpec(refs=[racer.PrefetchRef(KEY_A)], ttl_seconds=3600))
    assert s.ttl_seconds == 10 and s.expires_at_ms == 1_010_000
    clock[0] += 11
    # A settled ticket keeps its outcome at expiry.
    s = client.prefetch_status("t")
    assert s.state == "SUCCEEDED" and s.reason is None and s.settled
    # Retention counts from expiry (1010).
    clock[0] = 1309.0
    assert client.prefetch_status("t").state == "SUCCEEDED"
    clock[0] = 1310.0
    with pytest.raises(racer.PrefetchError) as exc:
        client.prefetch_status("t")
    assert exc.value.status == 404


def test_ttl_expiry_cancels_unsettled(client, server):
    clock = [1000.0]
    server.prefetches.now = lambda: clock[0]
    server.prefetches.outcome = lambda pid, ref: Outcome(pending=True)
    client.prefetch("u", racer.PrefetchSpec(refs=[racer.PrefetchRef(KEY_A)], ttl_seconds=5))
    clock[0] += 5
    s = client.prefetch_status("u")
    assert s.state == "CANCELLED" and s.reason == "expired" and s.settled and s.bytes.in_flight == 0
    assert client.cancel_prefetch("u").reason == "expired"


def test_empty_object_and_wire_shapes(client, server):
    server.prefetches.outcome = lambda pid, ref: Outcome(empty=True, etag='"e"')
    refs = [racer.PrefetchRef(KEY_A), racer.PrefetchRef(KEY_A, offset=1), racer.PrefetchRef(KEY_A, length=1)]
    s = client.prefetch("e", racer.PrefetchSpec(refs=refs))
    assert s.state == "PARTIAL" and s.refs_succeeded == 1 and s.refs_failed == 2 and s.bytes.aligned_requested == 0
    status, headers, body = server.prefetches.handle("GET", "/v1/prefetches/e", {"host": "racer"}, b"")
    data = json.loads(body)
    assert ("Connection", "close") in headers
    for name in ("cache_uid", "reason", "ttl_seconds", "created_at_ms", "refs_total", "refs_pending", "bytes"):
        assert name in data
    assert not {"refs", "next_cursor", "coverage"} & set(data)
    _, _, body = server.prefetches.handle("GET", "/v1/prefetches/e?refs=all&wait=0.5s&coverage=true", {"host": "racer"}, b"")
    data = json.loads(body)
    assert data["next_cursor"] is None and "error" not in data["refs"][0] and data["refs"][1]["error"] == "invalid_range"
    status, headers, body = server.prefetches.handle("POST", "/v1/prefetches/e", {"host": "racer"}, b"")
    assert status == 405 and json.loads(body)["code"] == "method_not_allowed"
    for query in ("wait=%2B1s", "wait=1", "wait=61s", "refs=all&cursor=a%2Fb", "cursor=0.0.1"):
        status, _, _ = server.prefetches.handle("GET", "/v1/prefetches/e?" + query, {"host": "racer"}, b"")
        assert status == 400, query


def test_wait_coverage_and_paging(client, server):
    release = threading.Event()

    def outcome(_, ref):
        if ref["key"] == KEY_B:
            return Outcome(error="forbidden")
        if not release.is_set():
            return Outcome(pending=True)
        return Outcome(etag='"v"')

    server.prefetches.outcome = outcome
    server.prefetches.refs_page = 2
    refs = [racer.PrefetchRef(KEY_B if i % 2 else KEY_A, offset=i, length=1) for i in range(5)]
    s = client.prefetch("w", racer.PrefetchSpec(refs=refs))
    assert s.state == "RUNNING" and not s.settled and s.refs_pending == 3

    start = time.monotonic()
    s = client.prefetch_status("w", wait=0.05)
    assert not s.settled and time.monotonic() - start >= 0.05

    threading.Timer(0.02, release.set).start()
    s = client.prefetch_status("w", wait=5, coverage=True, refs="all")
    assert s.settled and s.state == "PARTIAL" and s.coverage is not None and len(s.refs) == 2 and s.next_cursor
    indexes = [r.index for r in s.refs]
    while s.next_cursor:
        s = client.prefetch_status("w", refs="all", cursor=s.next_cursor)
        indexes += [r.index for r in s.refs]
    assert indexes == [0, 1, 2, 3, 4]

    s = client.prefetch_status("w", refs="failed")
    assert [r.index for r in s.refs] == [1, 3] and s.refs[0].error == "forbidden" and s.next_cursor is None

    server.prefetches.refs_page = 1
    cursor = client.prefetch_status("w", refs="all").next_cursor
    client.prefetch("w", racer.PrefetchSpec(refs=[racer.PrefetchRef(KEY_A)], sequence=1))
    with pytest.raises(racer.PrefetchError) as exc:
        client.prefetch_status("w", refs="all", cursor=cursor)
    assert exc.value.status == 400


def test_list(client, server):
    assert client.list_prefetches() == []
    for pid in ("c", "a", "b"):
        client.prefetch(pid, racer.PrefetchSpec(refs=[racer.PrefetchRef(KEY_A)]))
    got = client.list_prefetches()
    assert [s.id for s in got] == ["a", "b", "c"]
    assert all(s.refs is None and s.coverage is None and s.state == "SUCCEEDED" for s in got)


def test_window(client, server):
    seen = []
    fail = threading.Event()

    def admit(_, body):
        seen.append((body["generation"], body["sequence"]))
        return Fail(503, "unavailable", "down") if fail.is_set() else None

    server.prefetches.admit = admit
    refs = [racer.PrefetchRef(KEY_A)]
    w = racer.PrefetchWindow(client, "win", racer.PrefetchSpec(refs=[racer.PrefetchRef(KEY_B)], generation=3, sequence=7, priority="low"))
    assert w.position == (3, 7)
    s = w.update(refs)
    assert (s.generation, s.sequence, s.priority, s.created) == (3, 7, "low", True)
    assert w.update(refs).sequence == 8
    with pytest.raises(racer.RacerError):
        w.update([])
    fail.set()
    with pytest.raises(racer.PrefetchError):
        w.update(refs)
    fail.clear()
    assert w.update([racer.PrefetchRef(KEY_B)]).sequence == 10
    s = w.reset(refs)
    assert (s.generation, s.sequence) == (4, 0)
    assert w.update(refs).sequence == 1
    assert seen == [(3, 7), (3, 8), (3, 9), (3, 10), (4, 0), (4, 1)]


def test_window_concurrent(client, server):
    w = racer.PrefetchWindow(client, "c")
    threads = [threading.Thread(target=w.update, args=([racer.PrefetchRef(KEY_A)],)) for _ in range(8)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    assert w.position == (0, 7)


def test_window_exhaustion(tmp_path):
    w = racer.PrefetchWindow(racer.Client(socket_path=str(tmp_path / "x")), "x", racer.PrefetchSpec(generation=(1 << 53) - 1, sequence=(1 << 53) - 1))
    w._submitted = True
    with pytest.raises(racer.RacerError):
        w.update([racer.PrefetchRef(KEY_A)])
    with pytest.raises(racer.RacerError):
        w.reset([racer.PrefetchRef(KEY_A)])


def test_split():
    refs = [racer.PrefetchRef(KEY_A, offset=i + 1) for i in range(10)]
    spec = racer.PrefetchSpec(refs=refs, priority="high", generation=1, sequence=2, authorization="a")
    parts = racer.split_prefetch("job", spec, max_refs=4)
    assert [p[0] for p in parts] == ["job.0", "job.1", "job.2"]
    assert [len(p[1].refs) for p in parts] == [4, 4, 2]
    assert all(p[1].priority == "high" and p[1].authorization == "a" and p[1].sequence == 2 for p in parts)
    assert len(racer.split_prefetch("job", spec)) == 1

    limit = len(encode_body(racer.PrefetchSpec(refs=refs[:3], priority="high", generation=1, sequence=2)))
    parts = racer.split_prefetch("job", spec, max_refs=100, max_body_bytes=limit)
    assert [len(p[1].refs) for p in parts] == [3, 3, 3, 1]
    assert all(len(encode_body(p[1])) <= limit for p in parts)
    assert [r.offset for p in parts for r in p[1].refs] == list(range(1, 11))

    with pytest.raises(racer.RacerError):
        racer.split_prefetch("job", spec, max_body_bytes=50)
    with pytest.raises(racer.RacerError):
        racer.split_prefetch("x" * 127, spec)
    with pytest.raises(racer.RacerError):
        racer.split_prefetch("job", racer.PrefetchSpec())


def test_split_parts_accepted(client, server):
    server.prefetches.max_refs = 3
    refs = [racer.PrefetchRef(KEY_A, offset=i) for i in range(7)]
    for pid, spec in racer.split_prefetch("big", racer.PrefetchSpec(refs=refs), max_refs=3):
        client.prefetch(pid, spec)
    assert [s.id for s in client.list_prefetches()] == ["big.0", "big.1", "big.2"]


def test_stat_sends_no_retention(client, server):
    server.objects[KEY_A] = __import__("fakeserver").Obj(b"abc")
    m = client.stat(racer.Request(bytes.fromhex(KEY_A)))
    assert m.size == 3 and not hasattr(m, "retain_until_ms")
    assert "cache-control" not in last(server).headers
    assert os.path.exists(server.path)


def test_server_rejects_oversized_put_before_body(server):
    # Racer rejects the head without reading the body, so a client that
    # sends only headers must still get 413 instead of hanging.
    size = server.prefetches.max_body + 1
    raw = (
        "PUT /v1/prefetches/a HTTP/1.1\r\nHost: racer\r\nContent-Type: application/json\r\n"
        f"Content-Length: {size}\r\n\r\n"
    ).encode()
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as s:
        s.settimeout(5)
        s.connect(server.path)
        s.sendall(raw)
        out = b""
        while chunk := s.recv(65536):
            out += chunk
    head, _, body = out.partition(b"\r\n\r\n")
    assert head.startswith(b"HTTP/1.1 413")
    assert json.loads(body)["code"] == "too_large"


def _early_close_server(path, response):
    """Reads only the request head, optionally replies, then closes without reading the body."""
    sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    sock.bind(path)
    sock.listen(1)

    def serve():
        conn, _ = sock.accept()
        buf = b""
        while b"\r\n\r\n" not in buf:
            chunk = conn.recv(4096)
            if not chunk:
                break
            buf += chunk
        if response:
            conn.sendall(response)
        conn.close()
        sock.close()

    t = threading.Thread(target=serve, daemon=True)
    t.start()
    return t


def test_client_surfaces_413_when_server_closes_before_body(tmp_path):
    path = str(tmp_path / "s")
    body = json.dumps({"code": "too_large", "message": "body too large"}).encode()
    resp = (
        b"HTTP/1.1 413 Payload Too Large\r\nContent-Type: application/json\r\n"
        b"Connection: close\r\nContent-Length: " + str(len(body)).encode() + b"\r\n\r\n" + body
    )
    t = _early_close_server(path, resp)
    c = racer.Client(socket_path=path, timeout=5)
    with pytest.raises(racer.PrefetchError) as exc:
        c._json_call("PUT", "/v1/prefetch/x", "Content-Type: application/json\r\n", b"x" * (16 << 20))
    assert exc.value.status == 413
    assert exc.value.code == "too_large"
    t.join(5)


def test_oversized_put_send_error_without_response(tmp_path):
    path = str(tmp_path / "s")
    t = _early_close_server(path, b"")
    c = racer.Client(socket_path=path, timeout=5)
    with pytest.raises((BrokenPipeError, ConnectionResetError)):
        c._json_call("PUT", "/v1/prefetch/x", "Content-Type: application/json\r\n", b"x" * (16 << 20))
    t.join(5)
