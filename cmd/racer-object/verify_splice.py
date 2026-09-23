#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Trace the production frontend alone, and reject userspace payload I/O."""

import argparse
import hashlib
import http.client
import json
import os
from pathlib import Path
import re
import signal
import socket
import subprocess
import tempfile
import threading
import time


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True)
    parser.add_argument("--scratch", required=True)
    args = parser.parse_args()
    # A separate Unix fixture emits coalesced headers/body. Only the production
    # frontend is traced, so its body cannot be confused with fixture/client I/O.
    payload = b"RACER_BODY_MUST_STAY_IN_KERNEL_" * 32768
    with tempfile.TemporaryDirectory(prefix="splice-", dir=args.scratch) as directory:
        root = Path(directory)
        config = {"azure_endpoint": "https://example.blob.core.windows.net", "objects": [
            {"bucket": "models", "key": "weights", "container": "models", "blob": "weights"}]}
        identity = json.dumps(["racer-object/azure/v1", config["azure_endpoint"], "models", "weights"], separators=(",", ":"))
        etag = '"' + hashlib.sha256(identity.encode()).hexdigest() + '"'
        (root / "objects.json").write_text(json.dumps(config))
        origin = socket.socket(socket.AF_UNIX)
        origin.bind(str(root / "cache"))
        origin.listen()
        failures = []

        def serve():
            try:
                conn, _ = origin.accept()
                with conn:
                    for _ in range(2):
                        request = b""
                        while not request.endswith(b"\r\n\r\n"):
                            part = conn.recv(1)
                            if not part:
                                raise RuntimeError("unexpected frontend disconnect")
                            request += part
                        header = f"HTTP/1.1 200 OK\r\nContent-Length: {len(payload)}\r\nETag: {etag}\r\n\r\n".encode()
                        conn.sendall(header + payload)
            except Exception as error:
                failures.append(error)

        worker = threading.Thread(target=serve, daemon=True)
        worker.start()
        with socket.socket() as reservation:
            reservation.bind(("127.0.0.1", 0))
            port = reservation.getsockname()[1]
        trace = root / "trace"
        with (root / "frontend.log").open("w+") as log:
            process = subprocess.Popen([
                "strace", "-f", "-yy", "-s", "256", "-o", str(trace),
                "-e", "trace=read,readv,recvfrom,recvmsg,write,writev,sendto,sendmsg,splice",
                os.path.abspath(args.binary), "frontend", "--config", str(root / "objects.json"),
                "--socket", str(root / "cache"), "--listen", f"127.0.0.1:{port}"],
                stdout=log, stderr=log, start_new_session=True)
            try:
                deadline = time.monotonic() + 10
                while True:
                    try:
                        probe = socket.create_connection(("127.0.0.1", port), timeout=0.1)
                        probe.close()
                        break
                    except OSError:
                        if process.poll() is not None or time.monotonic() > deadline:
                            raise RuntimeError("frontend failed to start")
                        time.sleep(0.05)
                client = http.client.HTTPConnection("127.0.0.1", port, timeout=10)
                for _ in range(2):
                    client.request("GET", "/models/weights")
                    response = client.getresponse()
                    assert response.status == 200
                    assert response.read() == payload
                client.close()
            finally:
                os.killpg(process.pid, signal.SIGTERM)
                process.wait(timeout=10)
                origin.close()
                worker.join(timeout=10)
                if worker.is_alive() or failures:
                    raise RuntimeError(f"fixture failed: {failures}")
        content = trace.read_text()
        assert "RACER_BODY" not in content, "payload reached frontend userspace"
        # strace can split calls across threads into unfinished/resumed lines.
        transferred = sum(int(n) for n in re.findall(r"(?:splice\([^\n]*|<\.\.\. splice resumed>[^\n]*)\s= (\d+)\n", content))
        assert transferred == 4 * len(payload), (transferred, 4 * len(payload))
        assert re.search(r"read\([^\n]*UNIX", content), "missing Unix header reads"
        print(f"PASS: {2 * len(payload)} body bytes, two persistent requests, "
              f"{transferred} successful splice bytes across both pipe legs; no userspace body I/O")


if __name__ == "__main__":
    main()
