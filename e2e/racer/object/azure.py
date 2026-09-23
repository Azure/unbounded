# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Small anonymous Azure Blob protocol fixture with an observable request ledger."""

import array
import hashlib
import json
import re
import struct
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import unquote, urlsplit


def weights():
    header, body = {}, bytearray()
    for name, shape, values in [
        ("weight", [1024, 1024], ((i % 97) / 16 for i in range(1024 * 1024))),
        ("bias", [1024], (i / 16 for i in range(1024))),
    ]:
        data = array.array("f", values)
        if sys.byteorder != "little":
            data.byteswap()
        start = len(body)
        body.extend(data.tobytes())
        header[name] = {"dtype": "F32", "shape": shape, "data_offsets": [start, len(body)]}
    encoded = json.dumps(header).encode()
    encoded += b" " * (-len(encoded) % 8)
    return struct.pack("<Q", len(encoded)) + encoded + body


class Azure(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def reply(self, status, body=b"", headers=None):
        self.send_response(status)
        self.send_header("Content-Length", str(len(body)))
        for key, value in (headers or {}).items():
            self.send_header(key, value)
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(body)

    def do_POST(self):
        if self.path != "/offline":
            self.reply(404)
            return
        with self.server.lock:
            self.server.offline = True
        self.reply(200)

    def do_HEAD(self):
        self.do_GET()

    def do_GET(self):
        path = unquote(urlsplit(self.path).path)
        with self.server.lock:
            if path in ("/healthz", "/hits"):
                self.reply(200, json.dumps(self.server.hits).encode())
                return
            span = self.headers.get("x-ms-range", self.headers.get("Range", ""))
            self.server.hits.append({"method": self.command, "target": path,
                                     "range": span, "if_match": self.headers.get("If-Match", "")})
            if self.server.offline:
                self.reply(503)
                return
        if path != "/weights/immutable/model.safetensors":
            self.reply(404)
            return
        data, etag = self.server.data, self.server.etag
        headers = {"ETag": etag, "x-ms-blob-type": "BlockBlob",
                   "Last-Modified": "Wed, 23 Sep 2026 00:00:00 GMT"}
        if self.command == "HEAD":
            self.reply(200, data, headers)
            return
        if self.headers.get("If-Match") != etag:
            self.reply(412)
            return
        match = re.fullmatch(r"bytes=(\d+)-(\d+)", span)
        if not match:
            self.reply(400)
            return
        start, end = map(int, match.groups())
        if not 0 <= start <= end < len(data):
            self.reply(416)
            return
        headers["Content-Range"] = f"bytes {start}-{end}/{len(data)}"
        self.reply(206, data[start:end + 1], headers)


if __name__ == "__main__":
    server = ThreadingHTTPServer(("0.0.0.0", 8080), Azure)
    server.data = weights()
    server.etag = '"' + hashlib.sha256(server.data).hexdigest() + '"'
    server.hits, server.lock, server.offline = [], threading.Lock(), False
    server.serve_forever()
