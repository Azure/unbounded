# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""The loader, its filters, fingerprints and sharded artifacts, on a fake vLLM."""

import logging
import types

import pytest
from modelfix import (
    DefaultModelLoader,
    DefaultModelLoaderV030,
    URI,
    W,
    Model,
    Source,
    client,
    install_fake_vllm,
    make_loader,
    model_config,
    model_state,
    publish,
    put,
    serve_dir,
    set_parallel,
)

import racer_vllm
from racer_vllm import cli, filters, loader, reader, sharded
from racer_vllm import manifest as mf
from racer_vllm.store import LocalStore


def key_of(seen):
    """The object key of a request the fake server saw, or ""."""
    prefix = "/v1/objects/"
    return seen.target[len(prefix) :].partition("/")[0] if seen.target.startswith(prefix) else ""


@pytest.fixture(autouse=True)
def fresh_modes():
    reader.forget()
    yield
    reader.forget()


def materialized(server, tmp_path, **kw):
    u, files, m = publish(server, **kw)
    d = str(tmp_path / "model")
    with client(server) as c:
        cli.materialize(c, u, d, out=lambda *_: None)
    return u, files, m, d


def test_register_is_idempotent_and_builds_a_subclass(monkeypatch, server):
    ld = make_loader(monkeypatch, server)
    assert isinstance(ld, DefaultModelLoader)
    assert type(ld).__name__ == "RacerModelLoader"
    assert racer_vllm.RacerModelLoader is type(ld)
    assert racer_vllm.LOAD_FORMAT == "racer"


def test_loader_reads_a_materialized_model(monkeypatch, server, torch, tmp_path, caplog):
    u, files, m, d = materialized(server, tmp_path)
    ld = make_loader(monkeypatch, server)
    model = Model()
    start = len(server.seen)
    with caplog.at_level(logging.INFO, logger="vllm.racer_vllm.loader"):
        ld.load_weights(model, model_config(d))
    assert model.got == W
    # _prepare_weights answers vLLM's call with the manifest's weight files.
    assert ld.prepared == [(d, ["a.safetensors", "b.safetensors"], True, "model.safetensors.index.json")]
    line = next(r.getMessage() for r in caplog.records if r.getMessage().startswith("racer: loaded"))
    assert "files=2" in line and "host=2" in line and "throughput=" in line
    # Every weight file was stat'ed at its pinned ETag before any data read.
    weights = {u.request(n).hex: n for n in ("a.safetensors", "b.safetensors")}
    load = [s for s in server.seen[start:] if key_of(s) in weights]
    heads = [s for s in load if s.method == "HEAD"]
    assert len(heads) == 2 and all(s.headers.get("if-match") == '"v1"' for s in heads)
    assert load.index(heads[-1]) < min(i for i, s in enumerate(load) if s.method == "POST")
    assert ld._racer_clients == {} and ld._racer_readers == {}


def test_loader_follows_secondary_weights_and_prefixes(monkeypatch, server, torch, tmp_path):
    u, files, m, d = materialized(server, tmp_path)
    draft = racer_vllm.parse_uri("racer://cache1/models/org/draft")
    publish(server, draft)
    ld = make_loader(monkeypatch, server)
    model = Model(secondary=[Source(str(draft), prefix="draft.")])
    ld.load_weights(model, model_config(d))
    assert set(model.got) == set(W) | {"draft." + n for n in W}


def test_loader_reads_a_racer_uri_directly(monkeypatch, server, torch):
    publish(server)
    ld = make_loader(monkeypatch, server, verify="none")
    model = Model()
    ld.load_weights(model, model_config(URI))
    assert model.got == W


def test_loader_uses_the_uri_setting(monkeypatch, server, torch, tmp_path):
    u, files, m, d = materialized(server, tmp_path)
    (tmp_path / "model" / mf.SOURCE_NAME).unlink()
    ld = make_loader(monkeypatch, server, uri=URI, verify="none")
    model = Model()
    ld.load_weights(model, model_config(d))
    assert model.got == W


def test_loader_without_a_uri_fails(monkeypatch, server, torch, tmp_path):
    u, files, m, d = materialized(server, tmp_path)
    (tmp_path / "model" / mf.SOURCE_NAME).unlink()
    ld = make_loader(monkeypatch, server)
    with pytest.raises(ValueError, match="racer-vllm materialize"):
        ld.load_weights(Model(), model_config(d))


def test_download_model_needs_a_manifest(monkeypatch, server, tmp_path):
    ld = make_loader(monkeypatch, server)
    with pytest.raises(FileNotFoundError, match="racer-vllm materialize"):
        ld.download_model(model_config(str(tmp_path)))
    assert ld.download_model(model_config(URI)) == URI


def test_loader_fails_when_the_model_changed_since_materialize(monkeypatch, server, torch, tmp_path):
    u, files, m, d = materialized(server, tmp_path)
    put(server, u, "a.safetensors", files["a.safetensors"], etag='"v2"')
    ld = make_loader(monkeypatch, server)
    with pytest.raises(mf.ManifestError, match="changed since materialize"):
        ld.load_weights(Model(), model_config(d))


def test_verify_size_rejects_a_wrong_length(monkeypatch, server, torch):
    u, files, m = publish(server)
    put(server, u, "b.safetensors", files["b.safetensors"] + b"\0")
    ld = make_loader(monkeypatch, server)
    with pytest.raises(mf.ManifestError, match="bytes in Racer"):
        ld.load_weights(Model(), model_config(URI))


def test_verify_full_checks_sha256(monkeypatch, server, torch, tmp_path):
    u, files, m, d = materialized(server, tmp_path)
    ld = make_loader(monkeypatch, server, verify="full", staging_bytes=1 << 16)
    model = Model()
    ld.load_weights(model, model_config(d))
    assert model.got == W
    bad = bytearray(files["b.safetensors"])
    bad[-1] ^= 0xFF
    put(server, u, "b.safetensors", bytes(bad))
    reader.forget()
    ld = make_loader(monkeypatch, server, verify="full", staging_bytes=1 << 16, uri=URI)
    (tmp_path / "model" / mf.SOURCE_NAME).unlink()
    with pytest.raises(mf.ManifestError, match="sha256 mismatch"):
        ld.load_weights(Model(), model_config(d))


def test_unused_weights_are_not_read(monkeypatch, server, torch, tmp_path):
    u, files, m, d = materialized(server, tmp_path)
    ld = make_loader(monkeypatch, server)
    model = Model()
    model.is_unused_checkpoint_weight = lambda name: name == "w3"
    ld.load_weights(model, model_config(d))
    assert set(model.got) == {"w1", "w2", "w4"}


# Filters


class DeepSeekMTP:
    def __init__(self, layers):
        self.config = types.SimpleNamespace(num_hidden_layers=layers)


def test_draft_rule_keeps_only_mtp_layers():
    keep = filters.keep_predicate(DeepSeekMTP(2), "draft")
    assert keep("model.layers.2.mlp.weight")
    assert not keep("model.layers.1.mlp.weight")
    assert keep("lm_head.weight") and keep("model.embed_tokens.weight")
    assert filters.keep_predicate(DeepSeekMTP(2), "target")("model.layers.0.x")
    assert filters.draft_rule(object()) is None
    no_cfg = DeepSeekMTP(None)
    assert filters.draft_rule(no_cfg) is None


def test_source_and_expert_filters():
    keep = filters.keep_predicate(object(), "target", source_is_unused=lambda n: n == "x")
    assert not keep("x") and keep("y")
    from racer_vllm import compat

    src = types.SimpleNamespace(is_unused_weight=["a"])
    assert compat.source_is_unused(src)("a") and not compat.source_is_unused(src)("b")
    assert compat.source_is_unused(types.SimpleNamespace()) is None
    keep = filters.keep_predicate(object(), "target", local_expert_ids=[0, 2])
    assert keep("m.experts.0.w") and keep("m.experts.2.w") and not keep("m.experts.1.w") and keep("m.attn.w")


# Fingerprints


def test_fingerprint_is_stable_and_sensitive():
    cfg = model_config("x")
    par = {"tp": 8, "pp": 1, "ep": 8, "enable_expert_parallel": True}
    base = sharded.fingerprint(Model(), "target", "a" * 64, "0.33.0", model_config=cfg, parallel=par)
    assert len(base) == 32
    assert base == sharded.fingerprint(Model(), "target", "a" * 64, "0.33.0", model_config=cfg, parallel=dict(par))
    variants = [
        sharded.fingerprint(Model(), "draft", "a" * 64, "0.33.0", model_config=cfg, parallel=par),
        sharded.fingerprint(Model(), "target", "b" * 64, "0.33.0", model_config=cfg, parallel=par),
        sharded.fingerprint(Model(), "target", "a" * 64, "0.30.0", model_config=cfg, parallel=par),
        sharded.fingerprint(Model(), "target", "a" * 64, "0.33.0", model_config=cfg, parallel=dict(par, tp=4)),
        sharded.fingerprint(Model(), "target", "a" * 64, "0.33.0", model_config=cfg, parallel=dict(par, ep=1)),
        sharded.fingerprint(Model(), "target", "a" * 64, "0.33.0", model_config=model_config("x", dtype="float16"),
                            parallel=par),
        sharded.fingerprint(DeepSeekMTP(1), "target", "a" * 64, "0.33.0", model_config=cfg, parallel=par),
    ]
    q = model_config("x")
    q.quantization = "fp8"
    variants.append(sharded.fingerprint(Model(), "target", "a" * 64, "0.33.0", model_config=q, parallel=par))
    assert len({base, *variants}) == len(variants) + 1


# Sharded artifacts


STATE = {"layer.a": b"\x11" * 64, "layer.b": b"\x22" * 32}


def capture_to(tmp_path, state, fp="f" * 32, ranks=(0,), world_size=1):
    out = str(tmp_path / "capture")
    for rank in ranks:
        sharded.capture(Model(model_state(state)), "target", out, fingerprint=fp, rank=rank,
                        world_size=world_size, source_manifest_sha256="0" * 64)
    return out


def test_capture_publish_and_load_sharded(monkeypatch, server, torch, tmp_path, caplog):
    u, files, m, d = materialized(server, tmp_path)
    ld = make_loader(monkeypatch, server, sharded="require")
    fp, _, _ = ld._fingerprint(Model(), model_config(d))
    root = tmp_path / "origin"
    cli.publish_captured(LocalStore(str(root)), capture_to(tmp_path, STATE, fp), "0.33.0", out=lambda *_: None)
    serve_dir(server, u, str(root))
    target = Model(model_state({n: b"\0" * len(v) for n, v in STATE.items()}))
    ld.load_weights(target, model_config(d))
    assert {n: t.bytes() for n, t in target.state.items()} == STATE
    assert target.got == {}  # the checkpoint was not read


def test_sharded_auto_falls_back_and_require_fails(monkeypatch, server, torch, tmp_path, caplog):
    u, files, m, d = materialized(server, tmp_path)
    ld = make_loader(monkeypatch, server, sharded="auto")
    model = Model(model_state(STATE))
    with caplog.at_level(logging.INFO, logger="vllm.racer_vllm.sharded"):
        ld.load_weights(model, model_config(d))
    assert model.got == W
    assert any("not using sharded artifact" in r.getMessage() for r in caplog.records)
    ld = make_loader(monkeypatch, server, sharded="require")
    with pytest.raises(sharded.ShardedMismatch, match="not found"):
        ld.load_weights(Model(model_state(STATE)), model_config(d))


def test_sharded_mismatch_is_detected(monkeypatch, server, torch, tmp_path):
    u, files, m, d = materialized(server, tmp_path)
    ld = make_loader(monkeypatch, server, sharded="require")
    fp, _, _ = ld._fingerprint(Model(), model_config(d))
    root = tmp_path / "origin"
    cli.publish_captured(LocalStore(str(root)), capture_to(tmp_path, STATE, fp), out=lambda *_: None)
    serve_dir(server, u, str(root))
    other = Model(model_state({"layer.a": b"\0" * 64, "layer.c": b"\0" * 4}))
    with pytest.raises(sharded.ShardedMismatch, match="layer"):
        ld.load_weights(other, model_config(d))
    ld = make_loader(monkeypatch, server, sharded="auto")
    other = Model(model_state({"layer.a": b"\0" * 64, "layer.c": b"\0" * 4}))
    ld.load_weights(other, model_config(d))
    assert other.got == W


def test_capture_reads_only_its_own_rank(monkeypatch, server, torch, tmp_path):
    u, files, m, d = materialized(server, tmp_path)
    ld = make_loader(monkeypatch, server, sharded="require")
    fp, _, _ = ld._fingerprint(Model(), model_config(d))
    root = tmp_path / "origin"
    out = capture_to(tmp_path, STATE, fp, ranks=(0, 1), world_size=2)
    cli.publish_captured(LocalStore(str(root)), out, out=lambda *_: None)
    serve_dir(server, u, str(root))
    # The fingerprint stands for the parallel layout; this loader is rank 0.
    target = Model(model_state({n: b"\0" * len(v) for n, v in STATE.items()}))
    ld.load_weights(target, model_config(d))
    assert {n: t.bytes() for n, t in target.state.items()} == STATE
    rank1 = sharded.artifact_prefix(fp, "target", 1)
    keys = {u.child(rank1).request(n).hex for n in (mf.SHARDED_NAME, sharded.DATA_NAME)}
    assert not any(key_of(s) in keys for s in server.seen)


def test_captured_rejects_incomplete_ranks(monkeypatch, tmp_path, torch):
    install_fake_vllm(monkeypatch)
    out = capture_to(tmp_path, STATE, ranks=(0,), world_size=2)
    with pytest.raises(mf.ManifestError, match="do not cover"):
        sharded.captured(out)


def test_capture_runs_between_load_weights_and_processing(monkeypatch, server, torch, tmp_path):
    u, files, m, d = materialized(server, tmp_path)
    cap = str(tmp_path / "cap")
    ld = make_loader(monkeypatch, server, capture_dir=cap, sharded="auto")
    model = Model(model_state(STATE))
    calls = []
    real_capture = sharded.capture
    monkeypatch.setattr(sharded, "capture", lambda *a, **k: calls.append("capture") or real_capture(*a, **k))
    vllm_config = types.SimpleNamespace(make_model=lambda: model)
    assert ld.load_model(vllm_config, model_config(d)) is model
    assert model.got == W  # capture_dir reads the checkpoint, never an artifact
    assert calls == ["capture"] and ld.events == ["process_weights_after_loading"]
    (key, ranks), = sharded.captured(cap).items()
    assert key[1] == "target" and list(ranks) == [0]
    monkeypatch.setenv(loader.CAPTURE_EXIT_ENV, "1")
    reader.forget()
    ld = make_loader(monkeypatch, server, capture_dir=str(tmp_path / "cap2"))
    vllm_config = types.SimpleNamespace(make_model=lambda: Model(model_state(STATE)))
    with pytest.raises(loader.CaptureComplete):
        ld.load_model(vllm_config, model_config(d))
    assert ld.events == []


@pytest.mark.parametrize("base, arity", [(DefaultModelLoader, 4), (DefaultModelLoaderV030, 3)])
def test_prepare_weights_matches_the_vllm_release(monkeypatch, server, tmp_path, base, arity):
    u, files, m, d = materialized(server, tmp_path)
    ld = make_loader(monkeypatch, server, base=base)
    got = ld._prepare_weights(d, None, None, True, None)
    assert len(got) == arity
    assert got[:3] == (d, ["a.safetensors", "b.safetensors"], True)
    if arity == 4:
        assert got[3] == "model.safetensors.index.json"


def test_fake_vllm_registry_is_isolated(monkeypatch):
    reg = install_fake_vllm(monkeypatch)
    racer_vllm.register()
    assert reg["calls"] == ["racer"]


def test_mm_encoder_only_filter_uses_vllms_function(monkeypatch, server, torch, tmp_path):
    import sys

    u, files, m, d = materialized(server, tmp_path)
    ld = make_loader(monkeypatch, server)
    calls = []

    def filt(names, folder, index, prefixes, *, weights_mapper=None):
        calls.append((list(names), folder, index, prefixes))
        return [n for n in names if n != "b.safetensors"]

    wu = types.ModuleType("vllm.model_executor.model_loader.weight_utils")
    wu.filter_mm_encoder_only_safetensors_files = filt
    monkeypatch.setitem(sys.modules, wu.__name__, wu)
    ld._encoder_only_lm_prefixes = ("language_model.",)
    model = Model()
    ld.load_weights(model, model_config(d))
    assert set(model.got) == {"w1", "w2"}
    assert calls == [(["a.safetensors", "b.safetensors"], d, "model.safetensors.index.json", ("language_model.",))]


class StrictDefaultModelLoader(DefaultModelLoader):
    """Like vLLM's DefaultModelLoader: rejects extra config keys outside its allowlist."""

    ALLOWED = {"enable_multithread_load", "num_threads", "enable_weights_track"}

    def __init__(self, load_config):
        extra = set(load_config.model_loader_extra_config or {}) - self.ALLOWED
        if extra:
            raise ValueError(f"Unexpected extra config keys for load format racer: {extra}")
        self.seen_extra = dict(load_config.model_loader_extra_config or {})
        super().__init__(load_config)


def test_settings_pass_the_base_allowlist(monkeypatch, server):
    ld = make_loader(monkeypatch, server, base=StrictDefaultModelLoader, uri=URI, verify="none", role="draft")
    assert ld.seen_extra == {}
    assert ld.racer.uri == URI and ld.racer.verify == "none" and ld.racer.role == "draft"
    # The caller's config is restored, so vLLM and the loader both see the real settings.
    assert ld.load_config.model_loader_extra_config["uri"] == URI


def published_rank0(monkeypatch, server, tmp_path, mode):
    """A rank 0 artifact for a tp=2 layout, and a loader in mode. Returns (loader, model dir)."""
    import sys

    u, files, m, d = materialized(server, tmp_path)
    ld = make_loader(monkeypatch, server, sharded=mode)
    set_parallel(sys.modules["vllm.distributed"], tp=2)
    fp, _, _ = ld._fingerprint(Model(), model_config(d))
    root = tmp_path / "origin"
    out = capture_to(tmp_path, STATE, fp, ranks=(0, 1), world_size=2)
    cli.publish_captured(LocalStore(str(root)), out, out=lambda *_: None)
    serve_dir(server, u, str(root))
    return ld, d


def broken_world_group():
    raise RuntimeError("world group is not initialized")


@pytest.mark.parametrize("layout", ["unknown", "out_of_range"])
def test_sharded_never_guesses_the_rank(monkeypatch, server, torch, tmp_path, caplog, layout):
    import sys

    ld, d = published_rank0(monkeypatch, server, tmp_path, "auto")
    dist = sys.modules["vllm.distributed"]
    if layout == "unknown":
        dist.get_world_group = broken_world_group  # tp reads fine, the rank does not
    else:
        set_parallel(dist, tp=2, rank=5)
    model = Model(model_state({n: b"\0" * len(v) for n, v in STATE.items()}))
    with caplog.at_level(logging.WARNING, logger="vllm.racer_vllm.loader"):
        ld.load_weights(model, model_config(d))
    assert model.got == W  # the checkpoint, not rank 0's artifact
    assert all(t.bytes() == b"\0" * len(STATE[n]) for n, t in model.state.items())
    assert any("not using sharded artifacts" in r.getMessage() for r in caplog.records)
    reader.forget()
    ld = make_loader(monkeypatch, server, sharded="require")
    dist = sys.modules["vllm.distributed"]
    if layout == "unknown":
        set_parallel(dist, tp=2)
        dist.get_world_group = broken_world_group
    else:
        set_parallel(dist, tp=2, rank=5)
    with pytest.raises(sharded.ShardedMismatch, match="unknown|outside"):
        ld.load_weights(Model(model_state(STATE)), model_config(d))


def test_parallel_info_reports_an_unknown_rank(monkeypatch):
    import sys

    install_fake_vllm(monkeypatch)
    dist = sys.modules["vllm.distributed"]
    set_parallel(dist, tp=4)
    assert sharded.parallel_info()["rank"] == 0
    dist.get_world_group = broken_world_group
    par = sharded.parallel_info()
    assert par["rank"] is None and par["world_size"] is None
    assert sharded.rank_problem(par) and sharded.rank_problem({"rank": 2, "world_size": 2})


@pytest.mark.parametrize("source", ["group", "config"])
def test_capture_refuses_data_parallelism(monkeypatch, server, torch, tmp_path, source):
    import sys

    u, files, m, d = materialized(server, tmp_path)
    ld = make_loader(monkeypatch, server, capture_dir=str(tmp_path / "cap"))
    cfg = model_config(d)
    if source == "group":
        sys.modules["vllm.distributed"].get_dp_group = lambda: types.SimpleNamespace(world_size=2)
    else:
        cfg.parallel_config = types.SimpleNamespace(data_parallel_size=2)
    with pytest.raises(RuntimeError, match="data parallel size is 2"):
        ld.load_weights(Model(model_state(STATE)), cfg)
    assert not (tmp_path / "cap" / "sharded").exists()


def test_verify_full_hashes_filtered_files(monkeypatch, server, torch, caplog):
    import functools

    # Small pages, so skipping w3 leaves a gap the reads do not cover.
    monkeypatch.setattr(loader, "plan", functools.partial(loader.plan, page_size=8))
    u, files, m = publish(server)
    bad = bytearray(files["b.safetensors"])
    off = bytes(bad).index(W["w3"])
    bad[off + 10] ^= 0xFF  # corrupt w3, which the model does not load
    put(server, u, "b.safetensors", bytes(bad))
    ld = make_loader(monkeypatch, server, verify="full")
    model = Model()
    model.is_unused_checkpoint_weight = lambda name: name == "w3"
    with caplog.at_level(logging.WARNING, logger="vllm.racer_vllm.loader"):
        with pytest.raises(mf.ManifestError, match="b.safetensors: sha256 mismatch"):
            ld.load_weights(model, model_config(URI))
    warned = [r.getMessage() for r in caplog.records if "verify=full" in r.getMessage()]
    assert len(warned) == 1 and "b.safetensors" in warned[0]
    # An intact file with a filtered tensor verifies and loads.
    put(server, u, "b.safetensors", files["b.safetensors"])
    reader.forget()
    ld = make_loader(monkeypatch, server, verify="full")
    model = Model()
    model.is_unused_checkpoint_weight = lambda name: name == "w3"
    ld.load_weights(model, model_config(URI))
    assert set(model.got) == {"w1", "w2", "w4"}


def test_verify_full_checks_the_size_of_an_unpinned_uri(monkeypatch, server, torch):
    u, files, m = publish(server)
    put(server, u, "b.safetensors", files["b.safetensors"] + b"\0")
    ld = make_loader(monkeypatch, server, verify="full")
    with pytest.raises(mf.ManifestError, match="bytes in Racer"):
        ld.load_weights(Model(), model_config(URI))


def test_verify_full_never_uses_gpu_delivery(monkeypatch, server, torch):
    with pytest.raises(ValueError, match="verify=full"):
        make_loader(monkeypatch, server, verify="full", delivery="gpu")
    publish(server)
    asked = []
    real = reader.probe
    monkeypatch.setattr(reader, "probe", lambda c, delivery, make: asked.append(delivery) or real(c, "host", make))
    ld = make_loader(monkeypatch, server, verify="full", delivery="auto")
    model = Model()
    ld.load_weights(model, model_config(URI))
    assert model.got == W and asked == ["host"]


def _tamper(doc, data, how):
    f = doc["files"][0]
    a, b = f["tensors"]["layer.a"], f["tensors"]["layer.b"]
    if how == "truncated":  # an old artifact without a recorded size, and a short data file
        del f["size"]
        data = data[:-8]
    elif how == "overlap":
        b["offset"] = a["offset"] + 8
    elif how == "past_end":
        del f["size"]
        b["offset"] = len(data)
    return doc, data


@pytest.mark.parametrize("how", ["truncated", "overlap", "past_end"])
def test_corrupt_sharded_artifact_falls_back(monkeypatch, server, torch, tmp_path, how):
    import json

    u, files, m, d = materialized(server, tmp_path)
    ld = make_loader(monkeypatch, server, sharded="auto")
    fp, _, _ = ld._fingerprint(Model(), model_config(d))
    root = tmp_path / "origin"
    cli.publish_captured(LocalStore(str(root)), capture_to(tmp_path, STATE, fp), out=lambda *_: None)
    serve_dir(server, u, str(root))
    art = u.child(sharded.artifact_prefix(fp, "target", 0))
    doc = json.loads(server.objects[art.request(mf.SHARDED_NAME).hex].data)
    data = server.objects[art.request(sharded.DATA_NAME).hex].data
    doc, data = _tamper(doc, data, how)
    put(server, art, mf.SHARDED_NAME, mf.canonical(doc))
    put(server, art, sharded.DATA_NAME, data)
    model = Model(model_state({n: b"\0" * len(v) for n, v in STATE.items()}))
    ld.load_weights(model, model_config(d))
    assert model.got == W
    reader.forget()
    ld = make_loader(monkeypatch, server, sharded="require")
    with pytest.raises(sharded.ShardedMismatch):
        ld.load_weights(Model(model_state(STATE)), model_config(d))


def test_sharded_manifest_checks_offsets():
    good = {"schema": mf.SHARDED_SCHEMA, "fingerprint": "f" * 32, "rank": 0, "world_size": 1, "role": "target",
            "source_manifest_sha256": "0" * 64,
            "files": [{"name": "m", "size": 24, "tensors": {
                "a": {"dtype": "U8", "shape": [8], "offset": 8, "size": 8, "sha256": "0" * 64},
                "b": {"dtype": "U8", "shape": [8], "offset": 16, "size": 8, "sha256": "0" * 64}}}]}
    mf.validate_sharded(good)
    import copy

    for path, value, match in [(("size",), 23, "past the end"), (("tensors", "b", "offset"), 12, "overlaps"),
                               (("tensors", "a", "offset"), 0, "length prefix")]:
        doc = copy.deepcopy(good)
        node = doc["files"][0]
        for k in path[:-1]:
            node = node[k]
        node[path[-1]] = value
        with pytest.raises(mf.ManifestError, match=match):
            mf.validate_sharded(doc)


def test_mm_encoder_only_filter_reads_the_index_of_a_uri(monkeypatch, server, torch):
    import json
    import os
    import sys

    publish(server)
    ld = make_loader(monkeypatch, server, verify="none")
    calls = []

    def filt(names, folder, index, prefixes, *, weights_mapper=None):
        # Like vLLM: keep files holding a tensor outside the language model.
        with open(os.path.join(folder, index)) as f:
            wm = json.load(f)["weight_map"]
        calls.append(sorted(wm))
        return [n for n in names if any(f == n and not t.startswith(prefixes) for t, f in wm.items())]

    wu = types.ModuleType("vllm.model_executor.model_loader.weight_utils")
    wu.filter_mm_encoder_only_safetensors_files = filt
    monkeypatch.setitem(sys.modules, wu.__name__, wu)
    ld._encoder_only_lm_prefixes = ("w3", "w4")
    model = Model()
    ld.load_weights(model, model_config(URI))
    assert set(model.got) == {"w1", "w2"}
    assert calls == [["w1", "w2", "w3", "w4"]]


def test_parallel_info_reads_enable_expert_parallel(monkeypatch):
    import sys

    install_fake_vllm(monkeypatch)
    dist = sys.modules["vllm.distributed"]
    # No EP group (older vLLM): the parallel config decides.
    on = types.SimpleNamespace(enable_expert_parallel=True)
    assert sharded.parallel_info(on)["enable_expert_parallel"] is True
    assert sharded.parallel_info(types.SimpleNamespace(enable_expert_parallel=False))["enable_expert_parallel"] is False
    # vLLM's current config is used when none is passed.
    cfgmod = types.ModuleType("vllm.config")
    cfgmod.get_current_vllm_config = lambda: types.SimpleNamespace(parallel_config=on)
    monkeypatch.setitem(sys.modules, "vllm.config", cfgmod)
    assert sharded.parallel_info()["enable_expert_parallel"] is True
    monkeypatch.delitem(sys.modules, "vllm.config")
    # Without a config, an EP group of more than one rank means EP is on.
    dist.get_ep_group = lambda: types.SimpleNamespace(world_size=4)
    par = sharded.parallel_info()
    assert par["ep"] == 4 and par["enable_expert_parallel"] is True
    dist.get_ep_group = lambda: types.SimpleNamespace(world_size=1)
    assert sharded.parallel_info()["enable_expert_parallel"] is False
    # The flag reaches the fingerprint.
    cfg = model_config("x")
    fps = {sharded.fingerprint(Model(), "target", "a" * 64, "0.33.0", model_config=cfg,
                               parallel=sharded.parallel_info(types.SimpleNamespace(enable_expert_parallel=v)))
           for v in (True, False)}
    assert len(fps) == 2
