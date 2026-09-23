# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

"""Explicit real-kernel smoke: python3 transport_smoke.py BIN_DIR EXT4_SCRATCH."""
import os
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import time

binary_dir, scratch = map(Path, sys.argv[1:])
cpus = sorted(os.sched_getaffinity(0))
if len(cpus) < 2:
    raise RuntimeError("two allowed CPUs required")


def port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def command(name, cpu, mode, address, *extra):
    return ["taskset", "-c", str(cpu), str(binary_dir / name), mode,
            "--listen" if mode == "server" else "--connect", address,
            "--connections-per-worker", "2", "--request-timeout", "2", *extra]


for name, body in [("metadata-bench", None), ("tcp-page-bench", "buffer"),
                   ("tcp-page-bench", "file")]:
    address = f"127.0.0.1:{port()}"
    extra = ["--body", body] if body else []
    server_extra = extra + (["--slab-dir", str(scratch)] if body == "file" else [])
    with tempfile.TemporaryFile(dir=scratch, mode="w+") as log:
        server = subprocess.Popen(command(name, cpus[0], "server", address, *server_extra),
                                  stdout=log, stderr=log)
        try:
            deadline = time.monotonic() + 10
            while True:
                log.seek(0)
                text = log.read()
                if "READY" in text:
                    break
                if server.poll() is not None or time.monotonic() >= deadline:
                    raise RuntimeError(f"server startup failed: {text}")
                time.sleep(0.05)
            for trial in range(2):
                client = subprocess.run(command(name, cpus[1], "client", address, *extra,
                                                "--warmup", "1", "--duration", "1"),
                                        capture_output=True, text=True, timeout=15)
                print(client.stdout, client.stderr, flush=True)
                assert client.returncode == 0 and "errors=0" in client.stdout
            if name == "metadata-bench":
                interrupted = subprocess.Popen(
                    command(name, cpus[1], "client", address,
                            "--warmup", "1", "--duration", "10"),
                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
                try:
                    # Disconnect an established workload, not just a refused connect.
                    time.sleep(2)
                    assert interrupted.poll() is None
                    server.terminate()
                    out, err = interrupted.communicate(timeout=10)
                    assert interrupted.returncode != 0 and "RESULT" not in out, (out, err)
                finally:
                    if interrupted.poll() is None:
                        interrupted.kill()
                        interrupted.wait(timeout=5)
        finally:
            server.terminate()
            server.wait(timeout=10)
            log.seek(0)
            print(log.read(), flush=True)
        assert server.returncode == 0

# Failure must exit nonzero, promptly, without a successful RESULT line.
address = f"127.0.0.1:{port()}"
failed = subprocess.run(command("metadata-bench", cpus[0], "client", address,
                                "--warmup", "1", "--duration", "1"),
                        capture_output=True, text=True, timeout=10)
assert failed.returncode != 0 and "RESULT" not in failed.stdout
with socket.socket() as silent:
    silent.bind(("127.0.0.1", 0))
    silent.listen(8)
    address = f"127.0.0.1:{silent.getsockname()[1]}"
    failed = subprocess.run(command("metadata-bench", cpus[0], "client", address,
                                    "--warmup", "1", "--duration", "1"),
                            capture_output=True, text=True, timeout=10)
    assert failed.returncode != 0 and "RESULT" not in failed.stdout
print("PASS metadata, buffered/file TCP, sequential clients, disconnect and timeout")
