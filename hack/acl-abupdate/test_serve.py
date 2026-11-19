# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

"""Unit tests for serve.py, which run without a VM; the TLS round trip needs the openssl CLI and
only uses loopback:

    python3 -m unittest discover -s hack/acl-abupdate -p 'test_serve.py'
"""

from __future__ import annotations

import http.client
import json
import os
import shutil
import socket
import ssl
import stat
import subprocess
import sys
import tempfile
import threading
import time
import unittest
import unittest.mock
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import serve  # noqa: E402
from errors import StepError  # noqa: E402


def run(cmd: list[str], *, input: str | None = None, timeout: float | None = None,
        stream: bool = False) -> subprocess.CompletedProcess[str]:
    return subprocess.run(cmd, input=input, capture_output=True, text=True, timeout=timeout, check=False)


class RangeTests(unittest.TestCase):
    def test_ranges(self) -> None:
        cases = {
            None: None,
            "bytes=0-99": (0, 99),
            "bytes=100-": (100, 999),
            "bytes=-50": (950, 999),
            "bytes=-5000": (0, 999),
            "bytes=990-2000": (990, 999),
            "bytes=999-999": (999, 999),
            "Bytes= 0-0": (0, 0),
        }
        for header, expected in cases.items():
            with self.subTest(header=header):
                self.assertEqual(serve.parse_range(header, 1000), expected)

    def test_invalid_ranges(self) -> None:
        for header in ("bytes=1000-", "bytes=1000-1001", "bytes=50-10", "bytes=-0", "bytes=-", "bytes=0-1,5-6",
                       "items=0-1", "bytes=a-b", "", "bytes=0x10-"):
            with self.subTest(header=header), self.assertRaises(serve.RangeError):
                serve.parse_range(header, 1000)
        with self.assertRaises(serve.RangeError):
            serve.parse_range("bytes=-1", 0)


class SanTests(unittest.TestCase):
    def test_normalize(self) -> None:
        self.assertEqual(serve.normalize_sans(["IP:10.0.2.2", "ip:192.168.114.1", "DNS:LocalHost", "127.0.0.1", "::1",
                                               "example.test", "IP:10.0.2.2"]),
                         ["IP:10.0.2.2", "IP:192.168.114.1", "DNS:localhost", "IP:127.0.0.1", "IP:::1", "DNS:example.test"])
        for sans in ([], ["IP:not-an-ip"], ["DNS:bad_name"], ["URI:https://x"]):
            with self.subTest(sans=sans), self.assertRaises(StepError):
                serve.normalize_sans(sans)

    def test_parse_openssl_output(self) -> None:
        text = "X509v3 Subject Alternative Name: \n    IP Address:10.0.2.2, IP Address:0:0:0:0:0:0:0:1, DNS:localhost\n"
        self.assertEqual(serve.parse_openssl_sans(text), {"IP:10.0.2.2", "IP:::1", "DNS:localhost"})


class ThrottleTests(unittest.TestCase):
    def test_pace_spreads_chunks_over_time_across_connections(self) -> None:
        front = serve.Front(host="127.0.0.1", port=0, tls=serve.TLSFiles.at(Path("/nonexistent")),
                            upstream="http://127.0.0.1:1", public_host="127.0.0.1", log=lambda _: None)
        clock = [100.0]
        sleeps: list[float] = []

        def sleep(seconds: float) -> None:
            sleeps.append(round(seconds, 6))
            clock[0] += seconds

        with unittest.mock.patch.object(serve.time, "monotonic", lambda: clock[0]), \
                unittest.mock.patch.object(serve.time, "sleep", sleep):
            front._pace(1000)
            self.assertEqual(sleeps, [], "without a throttle nothing waits")
            front.throttle(1000)
            for _ in range(3):
                front._pace(500)
            self.assertEqual(sleeps, [0.5, 0.5, 0.5])
            # A second connection's chunk queues behind the first's, so the total rate holds.
            sleeps.clear()
            front._pace(1000)
            self.assertEqual(sleeps, [1.0])
            front.throttle(0)
            sleeps.clear()
            front._pace(10**9)
            self.assertEqual(sleeps, [])
        with self.assertRaises(StepError):
            front.throttle(-1)

    def test_upstream_must_be_http(self) -> None:
        with self.assertRaises(StepError):
            serve.Front(host="127.0.0.1", port=0, tls=serve.TLSFiles.at(Path("/nonexistent")),
                        upstream="https://127.0.0.1:1", public_host="127.0.0.1", log=lambda _: None)


class Nebraska(BaseHTTPRequestHandler):
    """Nebraska's stand-in: it answers every request with what it got."""

    protocol_version = "HTTP/1.1"

    def log_message(self, format: str, *args: object) -> None:
        pass

    def answer(self) -> None:
        body = self.rfile.read(int(self.headers.get("Content-Length") or 0))
        reply = json.dumps({"method": self.command, "path": self.path, "body": body.decode(),
                            "forwardedFor": self.headers.get("X-Forwarded-For"),
                            "contentType": self.headers.get("Content-Type")}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Set-Cookie", "ASLBSA=kept")
        self.send_header("Content-Length", str(len(reply)))
        self.end_headers()
        self.wfile.write(reply)

    do_GET = do_POST = answer


@unittest.skipIf(shutil.which("openssl") is None, "the openssl CLI isn't installed")
class LoopbackTests(unittest.TestCase):
    """make_tls with the real openssl CLI, and a round trip with the server on an ephemeral
    loopback port."""

    SANS = ["IP:10.0.2.2", "IP:192.168.114.1", "IP:127.0.0.1", "DNS:localhost"]

    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.logs: list[str] = []
        self.tls = serve.make_tls(self.root / "tls", self.SANS, run=run, log=self.logs.append)

    def client_context(self) -> ssl.SSLContext:
        return ssl.create_default_context(cafile=str(self.tls.ca_cert))

    def test_tls_files(self) -> None:
        self.assertEqual(stat.S_IMODE(self.tls.directory.stat().st_mode), 0o700)
        for key in (self.tls.server_key, self.tls.ca_key):
            self.assertEqual(stat.S_IMODE(key.stat().st_mode), 0o600)
        extensions = run(["openssl", "x509", "-in", str(self.tls.ca_cert), "-noout", "-ext", "basicConstraints,keyUsage"])
        self.assertIn("CA:TRUE", extensions.stdout)
        server = run(["openssl", "x509", "-in", str(self.tls.server_cert), "-noout", "-ext",
                      "subjectAltName,extendedKeyUsage"])
        self.assertIn("TLS Web Server Authentication", server.stdout)
        self.assertEqual(serve.parse_openssl_sans(server.stdout), set(self.SANS))

    def test_make_tls_is_idempotent(self) -> None:
        before = {path: path.read_bytes() for path in (self.tls.ca_cert, self.tls.server_cert, self.tls.server_key)}
        self.logs.clear()
        again = serve.make_tls(self.root / "tls", list(reversed(self.SANS)), run=run, log=self.logs.append)
        self.assertEqual(again, self.tls)
        self.assertEqual({path: path.read_bytes() for path in before}, before)
        self.assertEqual(self.logs, [])

    def test_new_sans_keep_the_ca(self) -> None:
        ca = self.tls.ca_cert.read_bytes()
        server = self.tls.server_cert.read_bytes()
        serve.make_tls(self.root / "tls", ["IP:10.0.2.2"], run=run)
        self.assertEqual(self.tls.ca_cert.read_bytes(), ca)
        self.assertNotEqual(self.tls.server_cert.read_bytes(), server)

    def test_round_trip(self) -> None:
        upstream = ThreadingHTTPServer(("127.0.0.1", 0), Nebraska)
        threading.Thread(target=upstream.serve_forever, daemon=True).start()
        self.addCleanup(upstream.server_close)
        self.addCleanup(upstream.shutdown)
        data = os.urandom(1000)
        image = self.root / "e2e.cosi"
        image.write_bytes(data)
        front = serve.Front(host="127.0.0.1", port=0, tls=self.tls, upstream=f"http://127.0.0.1:{upstream.server_port}",
                            public_host="127.0.0.1", log=self.logs.append)
        with front:
            url = front.serve_image("e2e.cosi", image)
            self.assertEqual(url, f"https://127.0.0.1:{front.port}/images/e2e.cosi")
            self.assertEqual(front.update_url, f"https://127.0.0.1:{front.port}/v1/update")
            connection = http.client.HTTPSConnection("127.0.0.1", front.port, context=self.client_context(), timeout=10)

            # Omaha goes to Nebraska, and is logged; its cookie comes back.
            connection.request("POST", "/v1/update/", body=b"<request/>", headers={"Content-Type": "application/xml"})
            response = connection.getresponse()
            answer = json.loads(response.read())
            self.assertEqual(response.status, 200)
            self.assertEqual(connection.sock.version(), "TLSv1.2")
            self.assertEqual((answer["method"], answer["path"], answer["body"], answer["contentType"]),
                             ("POST", "/v1/update/", "<request/>", "application/xml"))
            self.assertEqual(answer["forwardedFor"], "127.0.0.1")
            self.assertEqual(response.getheader("Set-Cookie"), "ASLBSA=kept")
            self.assertEqual([entry["request"] for entry in front.omaha], ["<request/>"])

            # So does the admin API, which isn't logged.
            connection.request("GET", "/api/apps")
            response = connection.getresponse()
            self.assertEqual(json.loads(response.read())["path"], "/api/apps")
            self.assertEqual(len(front.omaha), 1)

            # tridentd's HEAD, then its ranges.
            connection.request("HEAD", "/images/e2e.cosi")
            response = connection.getresponse()
            response.read()
            self.assertEqual((response.status, response.getheader("Content-Length"), response.getheader("Accept-Ranges")),
                             (200, "1000", "bytes"))
            for header, status, expected, content_range in (
                    ("bytes=0-99", 206, data[:100], "bytes 0-99/1000"),
                    ("bytes=900-", 206, data[900:], "bytes 900-999/1000"),
                    (None, 200, data, None),
                    ("bytes=2000-", 416, None, "bytes */1000")):
                with self.subTest(range=header):
                    connection.request("GET", "/images/e2e.cosi", headers={"Range": header} if header else {})
                    response = connection.getresponse()
                    body = response.read()
                    if expected is not None:
                        self.assertEqual(body, expected)
                    self.assertEqual(response.status, status)
                    self.assertEqual(response.getheader("Content-Range"), content_range)
            self.assertEqual(front.served("e2e.cosi"), 1200)

            connection.request("GET", "/images/other.cosi")
            response = connection.getresponse()
            response.read()
            self.assertEqual(response.status, 404)
            connection.close()

            # TLS 1.3 isn't offered.
            strict = self.client_context()
            strict.minimum_version = ssl.TLSVersion.TLSv1_3
            with self.assertRaises(ssl.SSLError), socket.create_connection(("127.0.0.1", front.port), timeout=5) as raw:
                strict.wrap_socket(raw, server_hostname="127.0.0.1")
            port = front.port
        with self.assertRaises(OSError), socket.create_connection(("127.0.0.1", port), timeout=2):
            pass

    def test_nebraska_down(self) -> None:
        with socket.socket() as unused:
            unused.bind(("127.0.0.1", 0))
            port = unused.getsockname()[1]
        with serve.Front(host="127.0.0.1", port=0, tls=self.tls, upstream=f"http://127.0.0.1:{port}",
                         public_host="127.0.0.1", log=self.logs.append) as front:
            connection = http.client.HTTPSConnection("127.0.0.1", front.port, context=self.client_context(), timeout=10)
            connection.request("POST", "/v1/update/", body=b"<request/>")
            response = connection.getresponse()
            self.assertEqual(response.status, 502)
            self.assertIn(b"Nebraska didn't answer", response.read())
            connection.close()
        time.sleep(0)


if __name__ == "__main__":
    unittest.main()
