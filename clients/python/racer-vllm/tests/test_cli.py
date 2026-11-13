# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""The racer-vllm command."""

import hashlib
import json
import os

import pytest
from fakeprefetch import Outcome
from modelfix import URI, W, Model, checkpoint, client, install_fake_vllm, model_state, publish, put

import racer_vllm
from racer_vllm import cli, importer, sharded, store
from racer_vllm import manifest as mf


def run(capsys, *argv):
    rc = racer_vllm.main(list(argv))
    out, err = capsys.readouterr()
    return rc, out, err


# Argument handling


@pytest.mark.parametrize(
    "argv, match",
    [
        ([], "required: COMMAND"),
        (["bogus"], "invalid choice"),
        (["materialize", "racer://c/b/p"], "required: DIR"),
        (["materialize", "s3://c/b/p", "dir"], "not a racer:// URI"),
        (["import", "--source", "x"], "required: --dest"),
        (["prepare", "racer://c/b/p"], "required: --dest"),
        (["inspect", "racer://c/b/p", "--", "x"], "only prepare takes arguments"),
        (["warm", "racer://c/b/p", "--timeout", "0"], "must be positive"),
        (["warm", "racer://c/b/p", "--priority", "urgent"], "invalid choice"),
        (["import", "--source", "x", "--dest", "file:///x", "--max-concurrency", "0"], "at least 1"),
    ],
)
def test_usage_errors_exit_2(capsys, argv, match):
    rc, out, err = run(capsys, *argv)
    assert rc == 2 and match in err


@pytest.mark.parametrize("cmd", [[], ["materialize"], ["import"], ["prepare"], ["warm"], ["inspect"]])
def test_help_everywhere(capsys, cmd):
    rc, out, err = run(capsys, *cmd, "--help")
    assert rc == 0 and "usage: racer-vllm" in out


def test_fetch_alias(capsys, server, tmp_path):
    u, files, m = publish(server)
    rc = racer_vllm.fetch_main([URI, str(tmp_path / "d"), "--socket", server.path])
    out, err = capsys.readouterr()
    assert rc == 0 and "deprecated" in err
    assert (tmp_path / "d" / mf.SOURCE_NAME).is_file()
    assert racer_vllm.fetch_main(["--help"]) == 0


def test_prepare_rejects_loader_flags(capsys):
    rc, out, err = run(capsys, "prepare", URI, "--dest", "file:///x", "--", "--load-format=auto")
    assert rc == 1 and "--load-format is set by racer-vllm prepare" in err


# materialize


def test_materialize_copies_aux_files_and_pins(capsys, server, tmp_path):
    u, files, m = publish(server)
    d = tmp_path / "model"
    rc, out, err = run(capsys, "materialize", URI, str(d), "--socket", server.path)
    assert rc == 0, err
    got = sorted(p.name for p in d.iterdir())
    aux = sorted(n for n in files if not n.endswith(".safetensors"))
    assert got == sorted(aux + [mf.MANIFEST_NAME, mf.SOURCE_NAME])
    for n in aux:
        assert (d / n).read_bytes() == files[n]
    assert mf.load(str(d / mf.MANIFEST_NAME)) == m
    src = mf.load_source(str(d))
    assert src["uri"] == URI and set(src["etags"]) == set(files)
    assert "2 weight files pinned" in out


def test_materialize_rejects_a_corrupt_file(capsys, server, tmp_path):
    u, files, m = publish(server)
    put(server, u, "config.json", b'{"model_type":"llamb"}')
    d = tmp_path / "model"
    rc, out, err = run(capsys, "materialize", URI, str(d), "--socket", server.path)
    assert rc == 1 and "config.json: sha256 mismatch" in err
    assert not (d / mf.SOURCE_NAME).exists()


def test_materialize_needs_a_manifest(capsys, server, tmp_path):
    rc, out, err = run(capsys, "materialize", URI, str(tmp_path / "d"), "--socket", server.path)
    assert rc == 1 and "racer-vllm import" in err


def test_materialize_reports_a_size_mismatch(capsys, server, tmp_path):
    u, files, m = publish(server)
    put(server, u, "a.safetensors", files["a.safetensors"][:-1])
    rc, out, err = run(capsys, "materialize", URI, str(tmp_path / "d"), "--socket", server.path)
    assert rc == 1 and "a.safetensors" in err and "import the model again" in err


# import


def hf_snapshot(root, files):
    """A Hugging Face cache layout: snapshot entries link into ../../blobs."""
    model = root / "models--org--llama"
    snap = model / "snapshots" / "rev1"
    (model / "blobs").mkdir(parents=True)
    for name, data in files.items():
        blob = model / "blobs" / hashlib.sha256(data).hexdigest()
        blob.write_bytes(data)
        link = snap / name
        link.parent.mkdir(parents=True, exist_ok=True)
        os.symlink(os.path.relpath(blob, link.parent), link)
    return snap


class Recording(store.LocalStore):
    def __init__(self, root):
        super().__init__(root)
        self.ops = []

    def upload_file(self, name, path, sha):
        self.ops.append(("upload", name))
        super().upload_file(name, path, sha)

    def put(self, name, data):
        self.ops.append(("put", name))
        super().put(name, data)

    def delete(self, name):
        self.ops.append(("delete", name))
        super().delete(name)


def test_import_uploads_blobs_then_the_manifest(capsys, tmp_path):
    files = checkpoint()
    files["sub/extra.py"] = b"print(1)\n"
    snap = hf_snapshot(tmp_path, files)
    dest = tmp_path / "dest"
    rc, out, err = run(capsys, "import", "--source", str(snap), "--dest", f"file://{dest}")
    assert rc == 0, err
    m = mf.load(str(dest / mf.MANIFEST_NAME))
    assert m["source"] == {"hub": "huggingface", "repo": "org/llama", "revision": "rev1"}
    assert m["index"] == importer.INDEX_NAME
    assert {f["name"] for f in m["files"]} == set(files)
    kinds = {f["name"]: f["kind"] for f in m["files"]}
    assert kinds["a.safetensors"] == "safetensors" and kinds["sub/extra.py"] == "aux"
    for name, data in files.items():
        assert (dest / name).read_bytes() == data
    assert "7 uploaded" in out

    rec = Recording(str(dest))
    listed = importer.scan(str(snap))
    done = importer.upload(rec, listed, importer.build_manifest(listed, m["source"]), log=lambda *_: None)
    assert done["uploaded"] == 0 and done["skipped"] == len(files)
    assert rec.ops == [("put", mf.MANIFEST_NAME)]


def test_import_hides_the_old_manifest_before_changing_blobs(tmp_path):
    files = checkpoint()
    snap = hf_snapshot(tmp_path, files)
    dest = Recording(str(tmp_path / "dest"))
    listed = importer.scan(str(snap))
    importer.upload(dest, listed, importer.build_manifest(listed, importer.guess_source(str(snap))), log=lambda *_: None)
    os.remove(snap / "config.json")
    (snap / "config.json").write_bytes(b'{"model_type":"other"}')
    dest.ops.clear()
    listed = importer.scan(str(snap))
    importer.upload(dest, listed, importer.build_manifest(listed, importer.guess_source(str(snap))), log=lambda *_: None)
    assert dest.ops == [("delete", mf.MANIFEST_NAME), ("upload", "config.json"), ("put", mf.MANIFEST_NAME)]


def test_import_rejects_links_out_of_the_cache(capsys, tmp_path):
    snap = hf_snapshot(tmp_path, {"config.json": b"{}"})
    secret = tmp_path / "secret"
    secret.write_text("x")
    os.symlink(secret, snap / "leak.json")
    rc, out, err = run(capsys, "import", "--source", str(snap), "--dest", f"file://{tmp_path}/d")
    assert rc == 1 and "links outside the model cache directory" in err


def test_import_rejects_a_bad_safetensors_file(capsys, tmp_path):
    snap = hf_snapshot(tmp_path, {"model.safetensors": b"\xff" * 16})
    rc, out, err = run(capsys, "import", "--source", str(snap), "--dest", f"file://{tmp_path}/d")
    assert rc == 1 and "model.safetensors" in err


@pytest.mark.parametrize(
    "dest, match",
    [
        ("s3://bucket/x", "destination must be"),
        ("https://acct.blob.core.windows.net", "names no container"),
        ("https://acct.blob.core.windows.net/c/p?sig=x", "query"),
        ("https://acct.blob.core.windows.net/c/../x", "'..'"),
        ("file://host/x", "file:///absolute/path"),
    ],
)
def test_bad_destinations(dest, match):
    with pytest.raises(store.StoreError, match=match):
        store.open_store(dest)


def test_azure_destination_without_the_extra(monkeypatch):
    import sys

    for mod in ("azure", "azure.identity", "azure.storage", "azure.storage.blob", "azure.core",
                "azure.core.exceptions"):
        monkeypatch.setitem(sys.modules, mod, None)
    with pytest.raises(store.StoreError, match=r"racer-vllm\[azure\]"):
        store.open_store("https://acct.blob.core.windows.net/models/org/llama")


# warm


def test_warm_prefetches_every_weight_file(capsys, server):
    u, files, m = publish(server)
    rc, out, err = run(capsys, "warm", URI, "--socket", server.path, "--poll", "0.01")
    assert rc == 0, err
    put_bodies = [json.loads(s.body) for s in server.seen if s.method == "PUT"]
    keys = {r["key"] for b in put_bodies for r in b["refs"]}
    assert keys == {u.request(n).hex for n in ("a.safetensors", "b.safetensors")}
    assert all("retain" not in json.dumps(b) for b in put_bodies)
    assert "refs 2 succeeded, 0 failed of 2" in out


def test_warm_fails_when_a_ref_fails(capsys, server):
    publish(server)
    server.prefetches.outcome = lambda tid, ref: Outcome(error="origin_error")
    rc, out, err = run(capsys, "warm", URI, "--socket", server.path, "--poll", "0.01")
    assert rc == 1 and "ended FAILED" in out and "prefetch did not complete" in err


def test_warm_times_out(capsys, server):
    publish(server)
    server.prefetches.outcome = lambda tid, ref: Outcome(pending=True)
    rc, out, err = run(capsys, "warm", URI, "--socket", server.path, "--poll", "0.01", "--timeout", "0.05")
    assert rc == 1 and "timed out" in out


# inspect


def test_inspect(capsys, server):
    u, files, m = publish(server)
    rc, out, err = run(capsys, "inspect", URI, "--socket", server.path)
    assert rc == 0, err
    assert "org/llama @ r1" in out and "sharded   none" in out
    assert f"status    {len(files)} ok, 0 missing, 0 size mismatch" in out
    del server.objects[u.request("b.safetensors").hex]
    rc, out, err = run(capsys, "inspect", URI, "--socket", server.path)
    assert rc == 1 and "missing   b.safetensors" in out


# prepare


def test_prepare_captures_and_publishes(capsys, monkeypatch, server, torch, tmp_path):
    install_fake_vllm(monkeypatch)
    u, files, m = publish(server)
    dest = tmp_path / "origin"
    state = {"layer.a": b"\1" * 16}
    seen = {}

    def runner(model_dir, extra, args):
        seen.update(extra=extra, args=args, model_dir=model_dir)
        assert mf.load_source(model_dir)["uri"] == URI
        for rank in range(2):
            sharded.capture(Model(model_state(state)), "target", extra["capture_dir"], fingerprint="c" * 32,
                            rank=rank, world_size=2, source_manifest_sha256=mf.digest(m))

    entries = cli.prepare(u, f"file://{dest}", str(tmp_path / "work"), ["--tensor-parallel-size", "2"],
                          socket_path=server.path, runner=runner)
    assert seen["args"] == ["--tensor-parallel-size", "2"]
    assert seen["extra"]["sharded"] == "off" and seen["extra"]["uri"] == URI
    assert entries == [{"fingerprint": "c" * 32, "role": "target", "world_size": 2, "vllm": "0.33.0"}]
    for rank in range(2):
        p = dest / sharded.artifact_prefix("c" * 32, "target", rank)
        assert (p / mf.SHARDED_NAME).is_file() and (p / sharded.DATA_NAME).is_file()
    index = mf.loads_sharded_index((dest / mf.SHARDED_INDEX_NAME).read_bytes())
    assert index["artifacts"] == entries
    # A second run refuses to mix with the earlier capture.
    with pytest.raises(cli.CommandError, match="earlier capture"):
        cli.prepare(u, f"file://{dest}", str(tmp_path / "work"), [], socket_path=server.path, runner=runner)


def test_prepare_does_not_publish_an_incomplete_capture(monkeypatch, server, torch, tmp_path):
    install_fake_vllm(monkeypatch)
    u, files, m = publish(server)
    dest = tmp_path / "origin"

    def runner(model_dir, extra, args):
        sharded.capture(Model(model_state({"x": b"\0" * 4})), "target", extra["capture_dir"], fingerprint="d" * 32,
                        rank=1, world_size=2, source_manifest_sha256=mf.digest(m))

    with pytest.raises(mf.ManifestError, match="do not cover"):
        cli.prepare(u, f"file://{dest}", str(tmp_path / "work"), [], socket_path=server.path, runner=runner)
    assert not (dest / mf.SHARDED_INDEX_NAME).exists()


def test_prepare_without_vllm(capsys, monkeypatch, server, tmp_path):
    import sys

    publish(server)
    for mod in ("vllm", "vllm.engine", "vllm.engine.arg_utils"):
        monkeypatch.setitem(sys.modules, mod, None)
    monkeypatch.setattr(racer_vllm, "_registered", True)
    rc, out, err = run(capsys, "prepare", URI, "--dest", f"file://{tmp_path}/o", "--workdir", str(tmp_path / "w"),
                       "--socket", server.path)
    assert rc == 1 and "needs vLLM" in err


def test_inspect_lists_sharded_artifacts(capsys, monkeypatch, server, torch, tmp_path):
    install_fake_vllm(monkeypatch)
    u, files, m = publish(server)
    out_dir = str(tmp_path / "cap")
    sharded.capture(Model(model_state({"x": b"\0" * 4})), "target", out_dir, fingerprint="e" * 32, rank=0,
                    world_size=1, source_manifest_sha256=mf.digest(m))
    root = tmp_path / "origin"
    cli.publish_captured(store.LocalStore(str(root)), out_dir, "0.33.0", out=lambda *_: None)
    from modelfix import serve_dir

    serve_dir(server, u, str(root))
    rc, out, err = run(capsys, "inspect", URI, "--socket", server.path)
    assert rc == 0 and f"sharded   {'e' * 32} role=target ranks 1/1 vllm=0.33.0" in out
    assert W  # the checkpoint fixtures stay importable


@pytest.mark.parametrize("args", [["--data-parallel-size", "2"], ["--data-parallel-size=4"], ["-dp", "2"]])
def test_prepare_refuses_data_parallelism(monkeypatch, server, tmp_path, args):
    install_fake_vllm(monkeypatch)
    publish(server)
    dest = tmp_path / "origin"
    calls = []
    with pytest.raises(cli.CommandError, match="data-parallel-size"):
        cli.prepare(racer_vllm.parse_uri(URI), f"file://{dest}", str(tmp_path / "work"), args,
                    socket_path=server.path, runner=lambda *a: calls.append(a))
    assert calls == [] and not dest.exists() and not (tmp_path / "work").exists()
