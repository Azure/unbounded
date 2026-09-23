"""Test-only adapter from Racer's backend contract to an emulated S3 API.

Moto stores a deterministic safetensors object. Every object HEAD and GET
request makes an actual S3 HeadObject or GetObject request, recorded in /hits.
"""

import array
import hashlib
import json
import os
import socketserver
import struct
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import boto3
from botocore.exceptions import ClientError
from moto.server import ThreadedMotoServer


def weights():
    tensors = [
        ("weight", [1024, 1024], ((i % 97) / 16 for i in range(1024 * 1024))),
        ("bias", [1024], (i / 16 for i in range(1024))),
    ]
    header, body = {}, bytearray()
    for name, shape, values in tensors:
        data = array.array("f", values)
        if sys.byteorder != "little":
            data.byteswap()
        start = len(body)
        body.extend(data.tobytes())
        header[name] = {"dtype": "F32", "shape": shape, "data_offsets": [start, len(body)]}
    encoded = json.dumps(header).encode()
    encoded += b" " * (-len(encoded) % 8)
    return struct.pack("<Q", len(encoded)) + encoded + body


class Adapter(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def address_string(self):
        return os.environ.get("NODE_NAME", "local")

    def do_HEAD(self):
        self.serve()

    def do_GET(self):
        self.serve()

    def serve(self):
        if self.path in ("/healthz", "/hits"):
            with lock:
                body = json.dumps(hits if self.path == "/hits" else {"ready": True}).encode()
            self.send_response(200)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            if self.command != "HEAD":
                self.wfile.write(body)
            return
        target = self.requestline.split()[1]
        if target != "/models/model.safetensors":
            self.send_error(404)
            return
        args = {"Bucket": "models", "Key": "model.safetensors"}
        if self.command == "GET":
            if self.headers.get("Range"):
                args["Range"] = self.headers["Range"]
        if self.headers.get("If-Match"):
            if self.headers["If-Match"] != checksum_etag:
                self.send_error(412)
                return
            # The immutable seeded object has both a Racer checksum and an S3
            # validator. Pin the same S3 version; never forward a SHA-256 as MD5.
            args["IfMatch"] = s3_etag
        try:
            result = s3.head_object(**args) if self.command == "HEAD" else s3.get_object(**args)
        except ClientError as error:
            self.send_error(error.response["ResponseMetadata"]["HTTPStatusCode"])
            return
        with lock:
            hits.append({"method": self.command, "target": target,
                         "source": os.environ["NODE_NAME"], "range": self.headers.get("Range", "")})
        self.send_response(result["ResponseMetadata"]["HTTPStatusCode"])
        self.send_header("ETag", checksum_etag)
        for key in ("ContentLength", "CacheControl", "ContentRange"):
            if key in result:
                name = {"ContentLength": "Content-Length", "CacheControl": "Cache-Control",
                        "ContentRange": "Content-Range"}.get(key, key)
                self.send_header(name, str(result[key]))
        self.send_header("Accept-Ranges", "bytes")
        self.end_headers()
        if self.command == "GET":
            with result["Body"] as body:
                self.wfile.write(body.read())


if __name__ == "__main__":
    server = ThreadedMotoServer(ip_address="127.0.0.1", port=5000)
    server.start()
    s3 = boto3.client("s3", endpoint_url="http://127.0.0.1:5000", region_name="us-east-1",
                      aws_access_key_id="e2e", aws_secret_access_key="e2e")
    s3.create_bucket(Bucket="models")
    data = weights()
    checksum_etag = '"' + hashlib.sha256(data).hexdigest() + '"'
    result = s3.put_object(Bucket="models", Key="model.safetensors", Body=data,
                           CacheControl="public, max-age=3600")
    s3_etag = result["ETag"]
    hits, lock = [], threading.Lock()
    class UnixHTTPServer(socketserver.ThreadingUnixStreamServer):
        daemon_threads = True

    path = "/dev/racer/s3-cache/origin"
    unix = UnixHTTPServer(path, Adapter)
    os.chmod(path, 0o660)
    threading.Thread(target=unix.serve_forever, daemon=True).start()
    ThreadingHTTPServer(("0.0.0.0", 8080), Adapter).serve_forever()
