# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""gpu-read against a live dataplane on a real GPU.

Runs only when RACER_GPU_E2E_SOCKET names a running dataplane's client
socket, RACER_GPU_E2E_KEY is the 64-hex-digit key of an object it can serve
(RACER_GPU_E2E_METADATA and RACER_GPU_E2E_AUTHORIZATION are passed through),
and libcuda can allocate GPU memory. `make racer-gpu-test` runs this. Set RACER_GPU_E2E=require to fail instead of
skipping.
"""
import os

import pytest

from racer import Client, Request, cuda

SOCKET = os.environ.get("RACER_GPU_E2E_SOCKET")
KEY = os.environ.get("RACER_GPU_E2E_KEY")
REQUIRED = os.environ.get("RACER_GPU_E2E") == "require"


def _missing() -> str | None:
    if not SOCKET or not KEY:
        return "set RACER_GPU_E2E_SOCKET and RACER_GPU_E2E_KEY to run"
    if not cuda.available():
        return "libcuda is unavailable"
    return None


@pytest.fixture
def live():
    why = _missing()
    if why:
        if REQUIRED:
            pytest.fail(f"RACER_GPU_E2E=require but {why}")
        pytest.skip(why)
    request = Request.from_hex(
        KEY,
        os.environ.get("RACER_GPU_E2E_METADATA"),
        os.environ.get("RACER_GPU_E2E_AUTHORIZATION"),
    )
    return Client(socket_path=SOCKET), request


@pytest.mark.gpu
def test_gpu_read_matches_stream_read(live):
    client, request = live
    expected = client.read(request)
    assert expected, "the object is empty"
    offset = 4096
    with cuda.GpuAllocation(offset + len(expected)) as g:
        fd = g.export_fd()
        try:
            result = client.gpu_read(request, fd, g.size, offset, len(expected))
        finally:
            os.close(fd)
        assert result.length == len(expected)
        out = bytearray(len(expected))
        g.copy_to_host(offset, out)
        assert out == expected, "GPU bytes differ from the stream read"


@pytest.mark.gpu
def test_read_into_gpu_partial_range(live):
    client, request = live
    expected = client.read(request)
    start = len(expected) // 3
    length = max(1, len(expected) // 2)
    with cuda.GpuAllocation(length) as g:
        client.read_into_gpu(request, g, 0, offset=start, length=length)
        out = bytearray(length)
        g.copy_to_host(0, out)
        assert out == expected[start : start + length]
