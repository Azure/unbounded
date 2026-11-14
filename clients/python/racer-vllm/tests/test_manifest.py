# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Manifests, URIs, settings and packaging metadata."""

import copy
import json
import os
import struct
import sys

import pytest
from conftest import make_safetensors
from modelfix import checkpoint, manifest_for

import racer_vllm
from racer_vllm import manifest as mf
from racer_vllm import settings

HERE = os.path.dirname(__file__)


# URIs


def test_parse_uri():
    u = racer_vllm.parse_uri("racer://c/b/a/b/c/")
    assert (u.cache, u.bucket, u.prefix) == ("c", "b", "a/b/c")
    assert u.key("x.json") == "a/b/c/x.json"
    assert str(u) == "racer://c/b/a/b/c"
    assert u.child("/sharded/fp/").prefix == "a/b/c/sharded/fp"
    bare = racer_vllm.parse_uri("racer://c/b")
    assert bare.prefix == "" and bare.key("x") == "x" and str(bare) == "racer://c/b"
    assert u.request("x").hex == racer_vllm.parse_uri("racer://other/b/a/b/c").request("x").hex


@pytest.mark.parametrize("bad", ["s3://c/b/p", "racer://c", "racer:///b/p", "racer://c//p", "racer://c/b/../x", None])
def test_parse_uri_rejects(bad):
    with pytest.raises(ValueError):
        racer_vllm.parse_uri(bad)


# Model manifests


def test_manifest_round_trip_and_digest():
    m = manifest_for(checkpoint())
    text = mf.dumps(m)
    assert mf.loads(text) == m
    assert mf.loads(text.encode()) == m
    shuffled = json.loads(text)
    shuffled["files"][0] = dict(reversed(list(shuffled["files"][0].items())))
    assert mf.digest(shuffled) == mf.digest(m)
    changed = copy.deepcopy(m)
    changed["source"]["revision"] = "r2"
    assert mf.digest(changed) != mf.digest(m)
    names = [f.name for f in mf.safetensors_files(m)]
    assert names == ["a.safetensors", "b.safetensors"]
    a = mf.safetensors_files(m)[0]
    assert [t.name for t in a.tensors] == ["w1", "w2"]
    assert a.tensors[0].start == 8 + a.header_bytes and a.tensors[0].nbytes == 300
    assert mf.file_entry(m, "config.json")["kind"] == "aux"
    with pytest.raises(mf.ManifestError):
        mf.file_entry(m, "nope")


def _mutate(path, value):
    def f(m):
        target = m
        for k in path[:-1]:
            target = target[k]
        if value is KeyError:
            del target[path[-1]]
        else:
            target[path[-1]] = value

    return f


def _aux(m):
    return next(i for i, f in enumerate(m["files"]) if f["kind"] == "aux")


def _weights(m):
    return next(i for i, f in enumerate(m["files"]) if f["kind"] == "safetensors")


@pytest.mark.parametrize(
    "mutate, match",
    [
        (_mutate(["schema"], "v0"), "schema"),
        (_mutate(["extra"], 1), "unknown manifest fields"),
        (_mutate(["source"], "hf"), "source"),
        (_mutate(["files"], {}), "files must be a list"),
        (_mutate(["index"], "missing.json"), "index"),
        (lambda m: m["files"].__setitem__(0, dict(m["files"][0], name="../x")), "unsafe"),
        (lambda m: m["files"].__setitem__(0, dict(m["files"][0], name="/abs")), "unsafe"),
        (lambda m: m["files"].append(dict(m["files"][0])), "listed twice"),
        (lambda m: m["files"][0].__setitem__("size", -1), "size"),
        (lambda m: m["files"][0].__setitem__("size", True), "size"),
        (lambda m: m["files"][0].__setitem__("sha256", "ABC"), "sha256"),
        (lambda m: m["files"][0].__setitem__("kind", "bin"), "kind"),
        (lambda m: m["files"][_aux(m)].__setitem__("tensors", {}), "must not carry tensors"),
        (lambda m: m["files"][_weights(m)].__setitem__("header_bytes", 1), "header_bytes"),
        (lambda m: m["files"][_weights(m)].__setitem__("size", 10), "exceeds the file size"),
        (lambda m: m["files"][_weights(m)]["tensors"]["w1"].__setitem__("shape", [299]), "size mismatch"),
        (lambda m: m["files"][_weights(m)]["tensors"]["w1"].__setitem__("dtype", "Q4"), "dtype"),
        (lambda m: m["files"][_weights(m)]["tensors"]["w2"].__setitem__("data_offsets", [0, 50]), "overlapping"),
        (lambda m: m["files"][_weights(m)]["tensors"]["w2"].__setitem__("data_offsets", [400, 450]), "past the end"),
    ],
)
def test_manifest_rejects(mutate, match):
    m = copy.deepcopy(manifest_for(checkpoint()))
    mutate(m)
    with pytest.raises(mf.ManifestError, match=match):
        mf.validate(m)


def test_manifest_rejects_bad_json_and_missing_file(tmp_path):
    with pytest.raises(mf.ManifestError, match="not valid JSON"):
        mf.loads(b"{")
    with pytest.raises(mf.ManifestError, match="racer-vllm materialize"):
        mf.load(str(tmp_path / mf.MANIFEST_NAME))
    (tmp_path / mf.MANIFEST_NAME).write_text('{"schema": "x"}')
    with pytest.raises(mf.ManifestError, match=str(tmp_path)):
        mf.load(str(tmp_path / mf.MANIFEST_NAME))


def test_parse_header_skips_metadata_and_sorts():
    data = make_safetensors([("b", "F16", (2, 2), b"\0" * 8), ("a", "U8", (3,), b"abc")], metadata={"format": "pt"})
    n = mf.header_length(data[:8])
    tensors = mf.parse_header(data[8 : 8 + n], len(data))
    assert [(t.name, t.dtype, t.shape) for t in tensors] == [("b", "F16", (2, 2)), ("a", "U8", (3,))]
    assert tensors[1].start == 8 + n + 8 and tensors[1].end == len(data)


@pytest.mark.parametrize(
    "header",
    [b"[]", b"{bad", b'{"t": {"dtype": "U8", "shape": [2], "data_offsets": [0, 3]}}',
     b'{"t": {"dtype": "U8", "shape": [-1], "data_offsets": [0, 0]}}',
     b'{"t": {"dtype": "U8", "shape": [true], "data_offsets": [0, 1]}}'],
)
def test_parse_header_rejects(header):
    with pytest.raises(mf.ManifestError):
        mf.parse_header(header)


def test_header_length_bounds():
    assert mf.header_length(struct.pack("<Q", 2)) == 2
    for bad in (struct.pack("<Q", 1), struct.pack("<Q", mf.MAX_HEADER_BYTES + 1), b"short"):
        with pytest.raises(mf.ManifestError):
            mf.header_length(bad)


# Sharded manifests, index and source


def sharded_doc(**kw):
    doc = {
        "schema": mf.SHARDED_SCHEMA, "fingerprint": "0" * 32, "rank": 0, "world_size": 2, "role": "target",
        "source_manifest_sha256": "1" * 64,
        "files": [{"name": "model.safetensors", "tensors": {
            "w": {"dtype": "BF16", "shape": [2], "offset": 16, "size": 4, "sha256": "2" * 64}}}],
    }
    doc.update(kw)
    return doc


def test_sharded_manifest_round_trip():
    doc = sharded_doc()
    assert mf.loads_sharded(mf.dumps_sharded(doc)) == doc


@pytest.mark.parametrize(
    "kw, match",
    [
        ({"rank": 2}, "rank"),
        ({"world_size": 0}, "world_size"),
        ({"role": "both"}, "role"),
        ({"fingerprint": "xyz"}, "fingerprint"),
        ({"other": 1}, "unknown"),
        ({"files": [{"name": "m", "tensors": {"w": {"dtype": "BF16", "shape": [2], "offset": 0, "size": 3,
                                                    "sha256": "2" * 64}}}]}, "size"),
    ],
)
def test_sharded_manifest_rejects(kw, match):
    with pytest.raises(mf.ManifestError, match=match):
        mf.validate_sharded(sharded_doc(**kw))


def test_sharded_index_merge():
    a = {"fingerprint": "a" * 32, "role": "target", "world_size": 8, "vllm": "0.30.0"}
    b = {"fingerprint": "b" * 32, "role": "target", "world_size": 8}
    idx = mf.merge_sharded_index(None, [b, a])
    assert [x["fingerprint"][0] for x in idx["artifacts"]] == ["a", "b"]
    a2 = dict(a, vllm="0.33.0")
    idx2 = mf.merge_sharded_index(mf.loads_sharded_index(mf.canonical(idx)), [a2])
    assert idx2["artifacts"][0]["vllm"] == "0.33.0" and len(idx2["artifacts"]) == 2
    with pytest.raises(mf.ManifestError):
        mf.validate_sharded_index({"schema": mf.SHARDED_INDEX_SCHEMA, "artifacts": [a, a]})


def test_source_file(tmp_path):
    assert mf.load_source(str(tmp_path)) is None
    (tmp_path / mf.SOURCE_NAME).write_text(mf.dumps_source("racer://c/b/p", {"a.safetensors": '"v1"'}))
    assert mf.load_source(str(tmp_path))["etags"] == {"a.safetensors": '"v1"'}
    (tmp_path / mf.SOURCE_NAME).write_text('{"schema": "x"}')
    with pytest.raises(mf.ManifestError, match="schema"):
        mf.load_source(str(tmp_path))
    with pytest.raises(mf.ManifestError):
        mf.dumps_source("s3://x", {})


# Settings


def test_settings_defaults_and_values():
    s = settings.parse(None)
    assert (s.sharded, s.delivery, s.clone, s.verify, s.role) == ("auto", "auto", "auto", "size", "target")
    s = settings.parse({"staging_bytes": "1024", "max_staging_bytes": 4096, "delivery": "gpu", "uri": "racer://c/b"})
    assert (s.staging_bytes, s.max_staging_bytes, s.delivery) == (1024, 4096, "gpu")


@pytest.mark.parametrize(
    "extra, match",
    [
        ({"use_vmm": False}, "was removed"),
        ({"sharded_state": True}, "was removed"),
        ({"pattern": "x"}, "was removed"),
        ({"mode_retry_seconds": 1}, "was removed"),
        ({"retain": True}, "unknown option"),
        ({"delivery": "vmm"}, "delivery must be one of"),
        ({"staging_bytes": 0}, "positive"),
        ({"concurrency": True}, "integer"),
        ({"uri": 3}, "string"),
        ({"staging_bytes": 4096, "max_staging_bytes": 1024}, "at least staging_bytes"),
    ],
)
def test_settings_reject(extra, match):
    with pytest.raises(ValueError, match=match):
        settings.parse(extra)


# Packaging


def test_pyproject():
    try:
        import tomllib
    except ImportError:  # Python 3.10
        pytest.skip("tomllib needs Python 3.11")
    with open(os.path.join(HERE, "..", "pyproject.toml"), "rb") as f:
        proj = tomllib.load(f)
    with open(os.path.join(HERE, "..", "..", "racer", "pyproject.toml"), "rb") as f:
        client_proj = tomllib.load(f)
    p = proj["project"]
    assert p["scripts"] == {"racer-vllm": "racer_vllm:main", "racer-vllm-fetch": "racer_vllm:fetch_main"}
    assert p["entry-points"]["vllm.general_plugins"] == {"racer": "racer_vllm:register"}
    assert proj["tool"]["setuptools"]["packages"]["find"] == {"where": ["src"]}
    assert set(p["optional-dependencies"]["azure"]) >= {"azure-storage-blob>=12.19", "azure-identity>=1.15"}
    major, minor, _ = client_proj["project"]["version"].split(".")
    assert p["dependencies"] == [f"racer-client>={major}.{minor}.0,<{major}.{int(minor) + 1}"]
    markers = proj["tool"]["pytest"]["ini_options"]["markers"]
    assert {m.split(":")[0] for m in markers} >= {"vllm", "gpu"}


def test_import_is_light():
    import subprocess

    code = (
        "import sys, racer_vllm, racer_vllm.cli, racer_vllm.loader, racer_vllm.store, racer_vllm.importer\n"
        "assert racer_vllm.main(['--help']) == 0\n"
        "heavy = [m for m in ('vllm', 'torch', 'azure') if m in sys.modules]\n"
        "assert not heavy, heavy\n"
    )
    env = dict(os.environ, PYTHONPATH=os.pathsep.join(p for p in sys.path if p))
    r = subprocess.run([sys.executable, "-c", code], env=env, capture_output=True, text=True, timeout=60)
    assert r.returncode == 0, r.stderr
