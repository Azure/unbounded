"""Verify canonical SHA-256 validators are translated to the seeded S3 ETag."""

import hashlib
import http.client
import threading
import unittest
from http.server import ThreadingHTTPServer

import boto3
from moto.server import ThreadedMotoServer

import origin


class OriginContract(unittest.TestCase):
    def test_checksum_translation(self):
        moto = ThreadedMotoServer(ip_address="127.0.0.1", port=0)
        moto.start()
        self.addCleanup(moto.stop)
        host, port = moto.get_host_and_port()
        origin.s3 = boto3.client(
            "s3", endpoint_url=f"http://{host}:{port}", region_name="us-east-1",
            aws_access_key_id="e2e", aws_secret_access_key="e2e",
        )
        origin.s3.create_bucket(Bucket="models")
        data = origin.weights()
        self.assertEqual(len(data), 4198568)
        origin.checksum_etag = '"' + hashlib.sha256(data).hexdigest() + '"'
        result = origin.s3.put_object(
            Bucket="models", Key="model.safetensors", Body=data,
            CacheControl="public, max-age=3600",
        )
        origin.s3_etag = result["ETag"]
        self.assertNotEqual(origin.checksum_etag, origin.s3_etag)
        origin.hits, origin.lock = [], threading.Lock()
        server = ThreadingHTTPServer(("127.0.0.1", 0), origin.Adapter)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        connection = http.client.HTTPConnection(*server.server_address, timeout=10)
        self.addCleanup(connection.close)

        connection.request("HEAD", "/models/model.safetensors")
        response = connection.getresponse()
        self.assertEqual(response.status, 200)
        self.assertEqual(response.getheader("ETag"), origin.checksum_etag)
        self.assertEqual(int(response.getheader("Content-Length")), len(data))
        self.assertEqual(response.read(), b"")

        for start, end in ((0, 4194303), (4194304, len(data) - 1)):
            connection.request("GET", "/models/model.safetensors", headers={
                "If-Match": origin.checksum_etag, "Range": f"bytes={start}-{end}",
            })
            response = connection.getresponse()
            self.assertEqual(response.status, 206)
            self.assertEqual(response.getheader("ETag"), origin.checksum_etag)
            self.assertEqual(response.getheader("Content-Range"),
                             f"bytes {start}-{end}/{len(data)}")
            self.assertEqual(response.read(), data[start:end + 1])
        self.assertEqual([hit["method"] for hit in origin.hits], ["HEAD", "GET", "GET"])

        connection.request("GET", "/models/model.safetensors", headers={
            "If-Match": origin.s3_etag, "Range": "bytes=0-7",
        })
        response = connection.getresponse()
        self.assertEqual(response.status, 412)
        response.read()
        self.assertEqual(len(origin.hits), 3)


if __name__ == "__main__":
    unittest.main()
