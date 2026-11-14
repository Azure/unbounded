# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
import socket
import threading

import pytest

import racer


def client(server, **kw):
    return racer.Client(socket_path=server.path, **kw)


def test_capabilities_returns_the_json(server):
    server.capabilities = {"schema": 1, "delivery": ["host", "stream"], "gpu_uuids": [], "future": {"x": 1}}
    got = client(server).capabilities()
    assert got == server.capabilities
    s = server.seen[-1]
    assert (s.method, s.target, s.headers["host"]) == ("GET", "/v1/capabilities", "racer")


def test_capabilities_missing_endpoint_is_not_found(server):
    server.capabilities = None
    with pytest.raises(racer.NotFound) as e:
        client(server).capabilities()
    assert e.value.status == 404


@pytest.mark.parametrize("body", [[1, 2], {"delivery": "gpu"}])
def test_capabilities_rejects_bad_shapes(server, body):
    server.capabilities = body
    with pytest.raises(racer.ProtocolError):
        client(server).capabilities()


def test_capabilities_rejects_bad_json(tmp_path):
    path = str(tmp_path / "sock")
    srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    srv.bind(path)
    srv.listen(1)

    def serve():
        conn, _ = srv.accept()
        conn.recv(4096)
        conn.sendall(b"HTTP/1.1 200 OK\r\nContent-Length: 3\r\nConnection: close\r\n\r\nnot")
        conn.close()

    t = threading.Thread(target=serve, daemon=True)
    t.start()
    with pytest.raises(racer.ProtocolError):
        racer.Client(socket_path=path).capabilities()
    t.join(5)
    srv.close()


def test_capabilities_times_out(tmp_path):
    path = str(tmp_path / "sock")
    srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    srv.bind(path)
    srv.listen(1)
    try:
        with pytest.raises(OSError):  # socket.timeout is TimeoutError, an OSError
            racer.Client(socket_path=path, timeout=0.2).capabilities()
    finally:
        srv.close()
