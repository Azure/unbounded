# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

"""The harness's one HTTPS port, which the VMs and the cluster's pods reach on the bridge: the COSIs
the harness serves, at /images/NAME, and everything else passed on to Nebraska, which speaks plain
HTTP on the host's loopback. One port, one certificate, and one CA, which the VMs and centerpivot's
Site trust.

TLS stops at 1.2, as Trident's storm proxies do: an OpenSSL client build trident-acl-agent and
tridentd link mishandles TLS 1.3's post-handshake session tickets. Image ranges are served exactly:
tridentd reads Content-Length bytes from any 2xx answer as if they were the range it asked for
(io_utils/http/subfile.rs), so anything else would corrupt the image.
"""

from __future__ import annotations

import contextlib
import dataclasses
import datetime
import http.client
import ipaddress
import os
import re
import secrets
import shutil
import socket
import socketserver
import ssl
import subprocess
import sys
import threading
import time
import urllib.parse
from collections.abc import Callable
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any

from errors import StepError

Run = Callable[..., subprocess.CompletedProcess[str]]
Log = Callable[[str], None]

IMAGE_PATH = "/images/"
UPDATE_PATH = "/v1/update"
SERVER_NAME = "acl-abupdate"

CA_DAYS = 3650
SERVER_DAYS = 365
CA_RENEW_SECONDS = 30 * 86400
SERVER_RENEW_SECONDS = 7 * 86400
OPENSSL_TIMEOUT = 60

CHUNK_SIZE = 1 << 20
MAX_BODY = 4 << 20
HANDSHAKE_TIMEOUT = 30
# A keep-alive connection idle this long is closed; tridentd opens a new one.
IDLE_TIMEOUT = 120
UPSTREAM_TIMEOUT = 60
PROGRESS_LOG_SECONDS = 30
# How much of each Omaha request and answer the log keeps.
OMAHA_LOG_BYTES = 4096

NAME_PATTERN = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{0,127}")
HOSTNAME_PATTERN = re.compile(
    r"(?=.{1,253}$)[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*",
    re.ASCII)
RANGE_PATTERN = re.compile(r"bytes=([0-9]*)-([0-9]*)", re.ASCII | re.IGNORECASE)
# Not passed on, in either direction: they describe one hop.
HOP_HEADERS = frozenset({"connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailers",
                         "transfer-encoding", "upgrade", "host", "content-length"})


def now_iso() -> str:
    return datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="seconds")


def human_size(size: int) -> str:
    value = float(size)
    for unit in ("B", "KiB", "MiB", "GiB"):
        if value < 1024 or unit == "GiB":
            return f"{value:.0f} {unit}" if unit == "B" else f"{value:.1f} {unit}"
        value /= 1024
    return f"{size} B"


def url_host(host: str) -> str:
    """A host as it goes in a URL: an IPv6 address in brackets."""
    with contextlib.suppress(ValueError):
        if ipaddress.ip_address(host).version == 6:
            return f"[{host}]"
    return host


# ---------------------------------------------------------------------------------------------
# TLS material

CA_CONFIG = """\
[req]
prompt = no
distinguished_name = dn
x509_extensions = v3_ca

[dn]
CN = {common_name}
O = acl-abupdate harness

[v3_ca]
basicConstraints = critical, CA:TRUE, pathlen:0
keyUsage = critical, keyCertSign, cRLSign
subjectKeyIdentifier = hash
"""

SERVER_CONFIG = """\
[req]
prompt = no
distinguished_name = dn

[dn]
CN = acl-abupdate front
O = acl-abupdate harness

[v3_server]
basicConstraints = critical, CA:FALSE
keyUsage = critical, digitalSignature
extendedKeyUsage = serverAuth
subjectAltName = {sans}
subjectKeyIdentifier = hash
authorityKeyIdentifier = keyid:always
"""


@dataclasses.dataclass(frozen=True)
class TLSFiles:
    """The CA the VMs and the cluster trust, and the server certificate and key the CA signed."""

    directory: Path
    ca_cert: Path
    server_cert: Path
    server_key: Path

    @property
    def ca_key(self) -> Path:
        return self.directory / "ca.key"

    @classmethod
    def at(cls, directory: Path) -> TLSFiles:
        return cls(directory, directory / "ca.pem", directory / "server.pem", directory / "server.key")


def normalize_sans(sans: list[str] | tuple[str, ...]) -> list[str]:
    """Subject alternative names in the form OpenSSL's config takes, IP:... or DNS:..., in order
    and without duplicates. A bare address or name is taken as one."""
    normalized: list[str] = []
    for raw in sans:
        text = raw.strip()
        kind, separator, value = text.partition(":")
        kind = kind.upper()
        if not separator or kind not in ("IP", "DNS"):
            kind, value = "", text
        entry = san_entry(kind, value, raw)
        if entry not in normalized:
            normalized.append(entry)
    if not normalized:
        raise StepError("the server certificate needs at least one subject alternative name")
    return normalized


def san_entry(kind: str, value: str, raw: str) -> str:
    if kind in ("", "IP"):
        try:
            return f"IP:{ipaddress.ip_address(value)}"
        except ValueError:
            if kind == "IP":
                raise StepError(f"SAN {raw!r} isn't an IP address") from None
    if HOSTNAME_PATTERN.fullmatch(value):
        return f"DNS:{value.lower()}"
    raise StepError(f"SAN {raw!r} isn't an IP address or a DNS name")


def parse_openssl_sans(text: str) -> set[str]:
    """The SANs in `openssl x509 -noout -ext subjectAltName` output, normalized."""
    found = set()
    for token in re.split(r"[,\n]", text):
        token = token.strip()
        if token.startswith("IP Address:"):
            with contextlib.suppress(ValueError):
                found.add(f"IP:{ipaddress.ip_address(token.removeprefix('IP Address:').strip())}")
        elif token.startswith("DNS:"):
            found.add(f"DNS:{token.removeprefix('DNS:').strip().lower()}")
    return found


def make_tls(directory: Path, sans: list[str] | tuple[str, ...], *, run: Run, log: Log | None = None) -> TLSFiles:
    """A CA and a server certificate for the given SANs, under a directory only the user can read.

    It's idempotent: what's there is kept while it's valid, so the VMs' trust survives harness
    runs. A server certificate that's expiring, doesn't verify, or names other SANs is replaced,
    signed by the same CA, so the VMs needn't trust anything new. Only a missing, expiring, or
    broken CA is replaced, and then the VMs have to trust the new one.
    """
    if shutil.which("openssl") is None:
        raise StepError("openssl isn't installed; the harness needs it to make its server's certificate")
    wanted = normalize_sans(sans)
    directory = directory.expanduser()
    try:
        directory.mkdir(parents=True, exist_ok=True)
        os.chmod(directory, 0o700)
    except OSError as error:
        raise StepError(f"can't make {directory}: {error}") from error
    files = TLSFiles.at(directory)
    renewed_ca = False
    if not ca_valid(files, run):
        make_ca(files, run)
        renewed_ca = True
        if log:
            log(f"made a CA for the harness's server, {files.ca_cert}; the VMs have to trust it")
    if renewed_ca or not server_valid(files, wanted, run):
        make_server_cert(files, wanted, run)
        if log:
            log(f"made the harness's server certificate for {', '.join(wanted)}")
    try:
        for key in (files.ca_key, files.server_key):
            os.chmod(key, 0o600)
        for cert in (files.ca_cert, files.server_cert):
            os.chmod(cert, 0o644)
    except OSError as error:
        raise StepError(f"can't set the permissions of the TLS files in {directory}: {error}") from error
    if not server_valid(files, wanted, run):
        raise StepError(f"the certificate made in {directory} doesn't verify; is openssl 1.1.1 or later installed?")
    return files


def openssl(run: Run, args: list[str], what: str) -> subprocess.CompletedProcess[str]:
    result = run(["openssl", *args], timeout=OPENSSL_TIMEOUT)
    if result.returncode != 0:
        detail = "\n".join((result.stderr or result.stdout or "").strip().splitlines()[-6:])
        raise StepError(f"can't {what}: openssl {args[0]} exited {result.returncode}: {detail}")
    return result


def openssl_output(run: Run, args: list[str]) -> str | None:
    result = run(["openssl", *args], timeout=OPENSSL_TIMEOUT)
    return (result.stdout or "") if result.returncode == 0 else None


def key_matches(cert: Path, key: Path) -> bool:
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    try:
        context.load_cert_chain(cert, key)
    except (ssl.SSLError, OSError, ValueError):
        return False
    return True


def ca_valid(files: TLSFiles, run: Run) -> bool:
    if not (files.ca_cert.is_file() and files.ca_key.is_file()):
        return False
    if openssl_output(run, ["x509", "-in", str(files.ca_cert), "-noout", "-checkend", str(CA_RENEW_SECONDS)]) is None:
        return False
    extensions = openssl_output(run, ["x509", "-in", str(files.ca_cert), "-noout", "-ext", "basicConstraints,keyUsage"])
    if extensions is None or "CA:TRUE" not in extensions or "Certificate Sign" not in extensions:
        return False
    return key_matches(files.ca_cert, files.ca_key)


def server_valid(files: TLSFiles, wanted: list[str], run: Run) -> bool:
    if not (files.server_cert.is_file() and files.server_key.is_file() and files.ca_cert.is_file()):
        return False
    cert = str(files.server_cert)
    if openssl_output(run, ["x509", "-in", cert, "-noout", "-checkend", str(SERVER_RENEW_SECONDS)]) is None:
        return False
    if openssl_output(run, ["verify", "-CAfile", str(files.ca_cert), "-purpose", "sslserver", cert]) is None:
        return False
    sans = openssl_output(run, ["x509", "-in", cert, "-noout", "-ext", "subjectAltName"])
    if sans is None or parse_openssl_sans(sans) != set(wanted):
        return False
    return key_matches(files.server_cert, files.server_key)


def make_ca(files: TLSFiles, run: Run) -> None:
    # A unique subject: a VM that still trusts an earlier CA mustn't find two with one hash.
    config = files.directory / "ca.cnf"
    config.write_text(CA_CONFIG.format(common_name=f"acl-abupdate test CA {secrets.token_hex(4)}"))
    for stale in (files.server_cert, files.server_key):
        stale.unlink(missing_ok=True)
    openssl(run, ["genpkey", "-algorithm", "EC", "-pkeyopt", "ec_paramgen_curve:P-256", "-out", str(files.ca_key)],
            "make the CA's key")
    os.chmod(files.ca_key, 0o600)
    openssl(run, ["req", "-new", "-x509", "-config", str(config), "-extensions", "v3_ca", "-key", str(files.ca_key),
                  "-sha256", "-days", str(CA_DAYS), "-out", str(files.ca_cert)], "make the CA's certificate")


def make_server_cert(files: TLSFiles, sans: list[str], run: Run) -> None:
    config = files.directory / "server.cnf"
    config.write_text(SERVER_CONFIG.format(sans=", ".join(sans)))
    request = files.directory / "server.csr"
    openssl(run, ["genpkey", "-algorithm", "EC", "-pkeyopt", "ec_paramgen_curve:P-256", "-out", str(files.server_key)],
            "make the server's key")
    os.chmod(files.server_key, 0o600)
    try:
        openssl(run, ["req", "-new", "-config", str(config), "-key", str(files.server_key), "-out", str(request)],
                "make the server's certificate request")
        openssl(run, ["x509", "-req", "-in", str(request), "-CA", str(files.ca_cert), "-CAkey", str(files.ca_key),
                      "-set_serial", f"0x{secrets.randbits(127):x}", "-sha256", "-days", str(SERVER_DAYS),
                      "-extfile", str(config), "-extensions", "v3_server", "-out", str(files.server_cert)],
                "sign the server's certificate")
    finally:
        request.unlink(missing_ok=True)



# ---------------------------------------------------------------------------------------------
# Byte ranges


class RangeError(ValueError):
    """A Range header the server won't serve exactly, which gets 416."""


def parse_range(header: str | None, size: int) -> tuple[int, int] | None:
    """The inclusive byte span a Range header asks for, or None for the whole file.

    Stricter than RFC 9110, which lets a server ignore a Range it doesn't like and send the whole
    file: tridentd reads Content-Length bytes from any 2xx answer as if they were the range it
    asked for (io_utils/http/subfile.rs), so anything but the exact range would corrupt the image.
    Multiple ranges and malformed ones are refused instead.
    """
    if header is None:
        return None
    match = RANGE_PATTERN.fullmatch(header.replace(" ", "").replace("\t", ""))
    if match is None:
        raise RangeError(f"Range {header!r} isn't a single byte range")
    first, last = match.groups()
    if not first:
        if not last or int(last) == 0 or size == 0:
            raise RangeError(f"Range {header!r} asks for nothing of {size} bytes")
        return max(0, size - int(last)), size - 1
    start = int(first)
    if start >= size:
        raise RangeError(f"Range {header!r} starts past the end of {size} bytes")
    if not last:
        return start, size - 1
    end = int(last)
    if end < start:
        raise RangeError(f"Range {header!r} ends before it starts")
    return start, min(end, size - 1)


# ---------------------------------------------------------------------------------------------
# The server


class Front:
    """The harness's HTTPS port, served from daemon threads:

        with Front(tls=tls, upstream="http://127.0.0.1:8000", public_host="192.168.114.1", log=log) as front:
            front.serve_image("e2e.cosi", path)
            ...a Nebraska package's URL is front.image_url("e2e.cosi"), and its server front.update_url

    It records what it passes on to Nebraska's Omaha endpoint, for the run's artifacts.
    """

    def __init__(self, *, host: str, port: int, tls: TLSFiles, upstream: str, public_host: str, log: Log) -> None:
        parts = urllib.parse.urlsplit(upstream)
        if parts.scheme != "http" or not parts.hostname:
            raise StepError(f"the upstream {upstream!r} isn't an http URL")
        self.host = host
        self.public_host = public_host
        self.tls = tls
        self.log = log
        self._upstream = (parts.hostname, parts.port or 80)
        self._port = port
        self._lock = threading.Lock()
        self._images: dict[str, Path] = {}
        # Bytes per second the images are served at, across all connections, or 0 for as fast as
        # they go; and when the next chunk may be sent.
        self._rate = 0
        self._next_send = 0.0
        self._served: dict[str, list[float]] = {}
        self._omaha: list[dict[str, Any]] = []
        self._connections: set[ssl.SSLSocket] = set()
        self._server: _HTTPServer | None = None
        self._thread: threading.Thread | None = None
        self._stopping = False

    def __enter__(self) -> Front:
        self.start()
        return self

    def __exit__(self, *exc_info: object) -> None:
        self.stop()

    def start(self) -> None:
        if self._server is not None:
            return
        context = server_context(self.tls)
        try:
            server = _HTTPServer((self.host, self._port), context, self)
        except OSError as error:
            raise StepError(f"the harness's server can't listen on {url_host(self.host)}:{self._port}: {error}") from error
        self._port = server.server_address[1]
        self._server = server
        self._stopping = False
        self._thread = threading.Thread(target=server.serve_forever, name=f"front-{self._port}", daemon=True)
        self._thread.start()
        self.log(f"serving {self.base_url}: images, and Nebraska at {self.update_url}")

    def stop(self) -> None:
        server, thread = self._server, self._thread
        if server is None:
            return
        self._stopping = True
        server.shutdown()
        server.server_close()
        with self._lock:
            connections = list(self._connections)
        # Wakes handler threads blocked on keep-alive connections, which serve_forever's shutdown
        # doesn't reach. The base class's shutdown, because SSLSocket's drops its SSL object.
        for connection in connections:
            with contextlib.suppress(OSError):
                socket.socket.shutdown(connection, socket.SHUT_RDWR)
        if thread is not None:
            thread.join(timeout=10)
        self._server = None
        self._thread = None

    @property
    def port(self) -> int:
        return self._port

    @property
    def base_url(self) -> str:
        if self._port == 0:
            raise StepError("the harness's server's port isn't known until it starts")
        return f"https://{url_host(self.public_host)}:{self._port}"

    @property
    def update_url(self) -> str:
        """Nebraska's Omaha endpoint, as the VMs and the cluster reach it."""
        return self.base_url + UPDATE_PATH

    def image_url(self, name: str) -> str:
        return self.base_url + IMAGE_PATH + name

    def serve_image(self, name: str, path: Path) -> str:
        """Serve the file at /images/NAME from now on, and return its URL."""
        if not NAME_PATTERN.fullmatch(name):
            raise StepError(f"{name!r} can't name an image")
        if not path.is_file():
            raise StepError(f"there's no image at {path}")
        with self._lock:
            self._images[name] = path
        return self.image_url(name)

    def throttle(self, bytes_per_second: int) -> None:
        """Serve images at most this fast, across all connections, or as fast as they go for 0. A
        slow download keeps a stage running long enough to interrupt it."""
        if bytes_per_second < 0:
            raise StepError("a throttle can't be negative")
        with self._lock:
            self._rate = bytes_per_second
            self._next_send = 0.0
        self.log(f"serving images at {human_size(bytes_per_second)}/s" if bytes_per_second
                 else "serving images as fast as they go")

    def served(self, name: str) -> int:
        """How many bytes of the image have been sent."""
        with self._lock:
            return int(self._served.get(name, [0.0])[0])

    @property
    def omaha(self) -> list[dict[str, Any]]:
        """Each Omaha request passed on to Nebraska, in order: time, client, status, and the start
        of the request and the answer."""
        with self._lock:
            return [dict(entry) for entry in self._omaha]

    # From connection threads

    def _image(self, name: str) -> Path | None:
        with self._lock:
            return self._images.get(name)

    def _pace(self, size: int) -> None:
        """Wait until size more bytes may be sent, at the throttle's rate."""
        with self._lock:
            rate = self._rate
            if not rate:
                return
            now = time.monotonic()
            start = max(now, self._next_send)
            self._next_send = start + size / rate
            wait = self._next_send - now
        if wait > 0:
            time.sleep(wait)

    def _served_progress(self, name: str, sent: int) -> None:
        """Log a download's progress at most every PROGRESS_LOG_SECONDS: tridentd reads a COSI in
        many range requests, too many to log one by one."""
        now = time.monotonic()
        with self._lock:
            progress = self._served.setdefault(name, [0.0, 0.0, now])
            progress[0] += sent
            progress[1] += 1
            due = now - progress[2] >= PROGRESS_LOG_SECONDS
            if due:
                progress[2] = now
            total, count = int(progress[0]), int(progress[1])
        if due:
            self.log(f"served {human_size(total)} of {name} in {count} requests so far")

    def _record_omaha(self, entry: dict[str, Any]) -> None:
        with self._lock:
            self._omaha.append(entry)

    def _opened(self, connection: ssl.SSLSocket) -> None:
        with self._lock:
            self._connections.add(connection)

    def _closed(self, connection: ssl.SSLSocket) -> None:
        with self._lock:
            self._connections.discard(connection)

    def _tls_failed(self, client: tuple[Any, ...], error: Exception) -> None:
        if self._stopping:
            return
        hint = "; the client doesn't trust the harness's CA" if "unknown ca" in str(error).lower() else ""
        self.log(f"TLS handshake with {client[0]} failed: {error}{hint}")

    def _connection_failed(self, client: tuple[Any, ...], error: BaseException) -> None:
        if self._stopping or isinstance(error, (ConnectionError, TimeoutError, ssl.SSLError)):
            return
        self.log(f"the connection from {client[0]} failed: {type(error).__name__}: {error}")


def server_context(tls: TLSFiles) -> ssl.SSLContext:
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.minimum_version = ssl.TLSVersion.TLSv1_2
    context.maximum_version = ssl.TLSVersion.TLSv1_2
    # TLS 1.2 sends its ticket inside the handshake, which is fine; going without makes the
    # server's behavior one thing less to wonder about when a client misbehaves.
    context.options |= ssl.OP_NO_TICKET
    try:
        context.load_cert_chain(tls.server_cert, tls.server_key)
    except (ssl.SSLError, OSError) as error:
        raise StepError(f"can't load the harness's server certificate {tls.server_cert}: {error}") from error
    return context


class _HTTPServer(ThreadingHTTPServer):
    """One thread per connection, each doing its own TLS handshake: done in accept(), a client
    that stalls mid-handshake would stop the server accepting anyone else."""

    daemon_threads = True

    def __init__(self, address: tuple[str, int], context: ssl.SSLContext, owner: Front):
        self.address_family = socket.AF_INET6 if ":" in address[0] else socket.AF_INET
        self.tls_context = context
        self.owner = owner
        super().__init__(address, _Handler)

    def server_bind(self) -> None:
        # HTTPServer's own looks the host's FQDN up, which can stall on a host without DNS.
        socketserver.TCPServer.server_bind(self)
        self.server_name, self.server_port = str(self.server_address[0]), int(self.server_address[1])

    def finish_request(self, request: Any, client_address: Any) -> None:
        request.settimeout(HANDSHAKE_TIMEOUT)
        try:
            connection = self.tls_context.wrap_socket(request, server_side=True)
        except (ssl.SSLError, OSError) as error:
            self.owner._tls_failed(client_address, error)
            return
        self.owner._opened(connection)
        try:
            self.RequestHandlerClass(connection, client_address, self)
        finally:
            self.owner._closed(connection)
            with contextlib.suppress(OSError):
                connection.close()

    def handle_error(self, request: Any, client_address: Any) -> None:
        # socketserver prints a traceback to stderr; a client hanging up isn't worth one.
        error = sys.exc_info()[1]
        if error is not None:
            self.owner._connection_failed(client_address, error)


class _Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = SERVER_NAME
    sys_version = ""
    timeout = IDLE_TIMEOUT
    server: _HTTPServer

    @property
    def owner(self) -> Front:
        return self.server.owner

    def log_message(self, format: str, *args: Any) -> None:
        pass

    def _send(self, status: int, body: bytes = b"", *, content_type: str = "text/plain; charset=utf-8",
              headers: list[tuple[str, str]] | None = None) -> None:
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        for name, value in headers or []:
            self.send_header(name, value)
        self.end_headers()
        if body and self.command != "HEAD":
            self.wfile.write(body)

    def _image_name(self) -> str | None:
        path = urllib.parse.urlsplit(self.path).path
        return path[len(IMAGE_PATH):] if path.startswith(IMAGE_PATH) else None

    def do_GET(self) -> None:
        name = self._image_name()
        if name is None:
            self._proxy()
        else:
            self._serve_image(name)

    def do_HEAD(self) -> None:
        name = self._image_name()
        if name is None:
            self._proxy()
        else:
            self._serve_image(name)

    def do_POST(self) -> None:
        self._proxy()

    def do_PUT(self) -> None:
        self._proxy()

    def do_PATCH(self) -> None:
        self._proxy()

    def do_DELETE(self) -> None:
        self._proxy()

    def _serve_image(self, name: str) -> None:
        path = self.owner._image(name)
        if path is None:
            self._send(404, f"no image {name}\n".encode())
            return
        size = path.stat().st_size
        try:
            span = parse_range(self.headers.get("Range"), size)
        except RangeError as error:
            self._send(416, f"{error}\n".encode(), headers=[("Content-Range", f"bytes */{size}")])
            return
        start, end = span if span else (0, size - 1)
        length = end - start + 1 if size else 0
        self.send_response(206 if span else 200)
        self.send_header("Content-Type", "application/octet-stream")
        self.send_header("Content-Length", str(length))
        self.send_header("Accept-Ranges", "bytes")
        if span:
            self.send_header("Content-Range", f"bytes {start}-{end}/{size}")
        self.end_headers()
        if self.command == "HEAD" or not length:
            return
        sent = 0
        try:
            with path.open("rb") as image:
                image.seek(start)
                while sent < length:
                    chunk = image.read(min(CHUNK_SIZE, length - sent))
                    if not chunk:
                        break
                    self.owner._pace(len(chunk))
                    self.wfile.write(chunk)
                    sent += len(chunk)
        finally:
            self.owner._served_progress(name, sent)

    def _proxy(self) -> None:
        """Pass the request on to Nebraska, and its answer back."""
        try:
            length = int(self.headers.get("Content-Length") or 0)
        except ValueError:
            self._send(400, b"bad Content-Length\n")
            return
        if length > MAX_BODY:
            self._send(413, b"too large\n")
            return
        body = self.rfile.read(length) if length else b""
        headers = {name: value for name, value in self.headers.items() if name.lower() not in HOP_HEADERS}
        headers["X-Forwarded-For"] = self.client_address[0]
        upstream = http.client.HTTPConnection(*self.owner._upstream, timeout=UPSTREAM_TIMEOUT)
        try:
            upstream.request(self.command, self.path, body=body or None, headers=headers)
            response = upstream.getresponse()
            answer = response.read()
            status, reply_headers = response.status, response.getheaders()
        except OSError as error:
            self._send(502, f"Nebraska didn't answer: {error}\n".encode())
            return
        finally:
            upstream.close()
        if urllib.parse.urlsplit(self.path).path.rstrip("/") == UPDATE_PATH.rstrip("/"):
            self.owner._record_omaha({
                "time": now_iso(), "client": self.client_address[0], "status": status,
                "request": body[:OMAHA_LOG_BYTES].decode(errors="replace"),
                "response": answer[:OMAHA_LOG_BYTES].decode(errors="replace"),
            })
        self.send_response(status)
        for name, value in reply_headers:
            if name.lower() not in HOP_HEADERS:
                self.send_header(name, value)
        self.send_header("Content-Length", str(len(answer)))
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(answer)
