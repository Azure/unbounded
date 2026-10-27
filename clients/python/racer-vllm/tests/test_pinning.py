# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Version pinning and the per-load summary."""

import json
import logging
import types

import pytest
from fakeserver import Obj
from test_loader import _Param, install_fake_vllm, setup_model

import racer
from racer_vllm import loader, model
from racer_vllm.model import ModelChangedError
from racer_vllm.weights import LoadStats, MemoryBackend, iter_weights

ALL = {"w1": b"\1" * 300, "w2": b"\2" * 50, "w3": b"\3" * 900, "w4": b"\0" * 8}


@pytest.fixture
def uri():
    return model.parse_uri("racer://cache1/models/org/llama")


def replace(server, uri, name, etag='"v2"'):
    """Swap in a new version of an object, as the origin would."""
    old = server.objects[uri.request(name).hex]
    server.objects[uri.request(name).hex] = Obj(old.data, etag)


def test_fetch_records_etags(server, uri, tmp_path):
    setup_model(server, uri)
    replace(server, uri, "b.safetensors", '"vb"')
    c = racer.Client(socket_path=server.path)
    model.fetch(uri, str(tmp_path / "m"), client=c)
    doc = json.loads((tmp_path / "m" / model.MARKER).read_text())
    assert doc["uri"] == str(uri)
    assert doc["etags"]["config.json"] == '"v1"'
    assert doc["etags"][model.INDEX] == '"v1"'
    assert doc["etags"]["a.safetensors"] == '"v1"'
    assert doc["etags"]["b.safetensors"] == '"vb"'
    assert model.resolve_pins(str(tmp_path / "m"), uri) == doc["etags"]
    # Pins belong to the fetched URI only.
    assert model.resolve_pins(str(tmp_path / "m"), model.parse_uri("racer://x/y/z")) == {}
    assert model.resolve_pins(str(tmp_path / "nowhere"), uri) == {}


def test_fetch_single_file_records_etag(server, uri, tmp_path):
    setup_model(server, uri, index=False)
    model.fetch(uri, str(tmp_path / "m"), client=racer.Client(socket_path=server.path))
    doc = json.loads((tmp_path / "m" / model.MARKER).read_text())
    assert doc["etags"][model.SINGLE] == '"v1"'


def test_fetch_without_weights_still_writes_marker(server, uri, tmp_path):
    server.objects[uri.request("config.json").hex] = Obj(b"{}")
    model.fetch(uri, str(tmp_path / "m"), client=racer.Client(socket_path=server.path))
    doc = json.loads((tmp_path / "m" / model.MARKER).read_text())
    assert doc["etags"] == {"config.json": '"v1"'}


def test_bad_pins_rejected(uri, tmp_path):
    (tmp_path / model.MARKER).write_text(json.dumps({"uri": str(uri), "etags": {"a": 1}}))
    with pytest.raises(ValueError, match="bad etags"):
        model.resolve_pins(str(tmp_path), uri)


def test_read_pinned(server, uri):
    setup_model(server, uri)
    c = racer.Client(socket_path=server.path)
    assert model.read_pinned(c, uri, "config.json") == (b'{"model_type":"llama"}', '"v1"')
    assert model.read_pinned(c, uri, "config.json", '"v1"')[1] == '"v1"'
    assert model.read_pinned(c, uri, "missing.json") is None
    with pytest.raises(ModelChangedError, match="config.json"):
        model.read_pinned(c, uri, "config.json", '"v0"')
    # A pinned file that was deleted also counts as a change.
    with pytest.raises(ModelChangedError):
        model.read_pinned(c, uri, "missing.json", '"v1"')


def test_all_files_stat_before_data_reads(server, uri):
    setup_model(server, uri)
    c = racer.Client(socket_path=server.path, staging_bytes=4096)
    assert dict(iter_weights(c, uri, MemoryBackend(), staging_bytes=400, depth=1)) == ALL
    a = uri.request("a.safetensors").hex
    b = uri.request("b.safetensors").hex
    heads = [i for i, s in enumerate(server.seen) if s.method == "HEAD" and s.target.endswith((a, b))]
    posts = [i for i, s in enumerate(server.seen) if s.method == "POST" and (a in s.target or b in s.target)]
    assert len(heads) == 2 and max(heads) < min(posts)
    # Every data read carries If-Match.
    for i in posts:
        assert server.seen[i].headers.get("if-match") == '"v1"'


def test_pinned_version_mismatch_fails_before_reading(server, uri):
    setup_model(server, uri)
    replace(server, uri, "b.safetensors")
    c = racer.Client(socket_path=server.path, staging_bytes=4096)
    pins = {"a.safetensors": '"v1"', "b.safetensors": '"v1"'}
    with pytest.raises(ModelChangedError, match="b.safetensors"):
        list(iter_weights(c, uri, MemoryBackend(), staging_bytes=400, depth=1, pins=pins))
    # Nothing past the up-front stat was read.
    assert not [s for s in server.seen if s.method == "POST" and "range" in s.headers]


def test_pinned_index_mismatch(server, uri):
    setup_model(server, uri)
    replace(server, uri, model.INDEX)
    c = racer.Client(socket_path=server.path)
    with pytest.raises(ModelChangedError, match="index"):
        model.weight_files(c, uri, {model.INDEX: '"v1"'})


@pytest.mark.parametrize("stop", ["w1", "w3"])
def test_model_changes_mid_load(server, uri, stop):
    # depth=1 reads one chunk ahead of the consumer. After w1, b.safetensors
    # has not been opened, so its header read fails; after w3 (b's dedicated
    # chunk), the header was read at v1 and the w4 data read fails.
    setup_model(server, uri)
    c = racer.Client(socket_path=server.path, staging_bytes=4096)
    gen = iter_weights(c, uri, MemoryBackend(), staging_bytes=400, depth=1)
    for name, _ in gen:
        if name == stop:
            break
    replace(server, uri, "b.safetensors")
    with pytest.raises(ModelChangedError, match="b.safetensors"):
        for _ in gen:
            pass


def test_load_stats_summary():
    now = [10.0]
    s = LoadStats(clock=lambda: now[0])
    s.files = 2
    s.add_read(racer.GPU, 1 << 30)
    s.add_read(racer.STREAM, 1 << 29)
    now[0] = 12.0
    s.finish()
    now[0] = 100.0
    assert s.seconds == 2.0
    text = s.summary("racer://c/b/p")
    assert "loaded 2 files, 1.50 GiB in 2.00s (0.75 GiB/s) from racer://c/b/p" in text
    assert "gpu=1 (1.00 GiB) host=0 (0.00 GiB) stream=1 (0.50 GiB)" in text


def test_load_stats_zero_time():
    s = LoadStats(clock=lambda: 5.0)
    s.finish()
    assert "(0.00 GiB/s)" in s.summary("x")


def test_iter_weights_counts_reads(server, uri):
    setup_model(server, uri)
    c = racer.Client(socket_path=server.path, staging_bytes=4096)
    stats = LoadStats()
    assert dict(iter_weights(c, uri, MemoryBackend(), staging_bytes=400, depth=2, stats=stats)) == ALL
    assert stats.files == 2
    assert stats.bytes == sum(len(v) for v in ALL.values())
    # MemoryBackend targets cannot be exported, so gpu-read is skipped and
    # host-read into staging is used.
    assert stats.reads[racer.HOST] >= 2 and stats.reads[racer.GPU] == 0


def test_loader_logs_summary_and_uses_pins(server, uri, monkeypatch, tmp_path, caplog):
    registry = install_fake_vllm(monkeypatch)
    loader.register()
    setup_model(server, uri)
    model.fetch(uri, str(tmp_path / "m"), client=racer.Client(socket_path=server.path))
    monkeypatch.setenv("RACER_SOCKET", server.path)
    monkeypatch.setattr(loader, "TorchBackend", lambda **kw: MemoryBackend())
    inst = registry["racer"](types.SimpleNamespace(model_loader_extra_config={"staging_bytes": 400}))
    mc = types.SimpleNamespace(model=str(tmp_path / "m"), quantization=None)

    class Model:
        def load_weights(self, weights):
            return {n for n, _ in weights}

        def named_parameters(self):
            return [(n, _Param()) for n in ALL]

    with caplog.at_level(logging.INFO, logger="vllm.racer_vllm.loader"):
        inst.load_weights(Model(), mc)
    msgs = [r.getMessage() for r in caplog.records if r.name == "vllm.racer_vllm.loader"]
    assert len(msgs) == 1 and "loaded 2 files" in msgs[0] and "host=" in msgs[0]

    # The model was replaced after racer-vllm-fetch: the load fails clearly.
    replace(server, uri, "a.safetensors")
    with pytest.raises(ModelChangedError, match="racer-vllm-fetch"):
        inst.load_weights(Model(), mc)


def test_mode_retry_setting(monkeypatch):
    assert loader._settings({})["mode_retry_seconds"] == racer.DEFAULT_MODE_RETRY_SECONDS
    assert loader._settings({"mode_retry_seconds": None})["mode_retry_seconds"] is None
    assert loader._settings({"mode_retry_seconds": "30"})["mode_retry_seconds"] == 30.0
