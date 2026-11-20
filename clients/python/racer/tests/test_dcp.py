# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""DCP storage plug-in tests (designs/racer-writes.md, WP10). Skipped without torch."""

import subprocess
import sys
import warnings

import pytest

import racer


def test_racer_does_not_import_torch():
    code = "import sys, racer; assert 'torch' not in sys.modules and 'racer.dcp' not in sys.modules"
    subprocess.run([sys.executable, "-c", code], check=True, timeout=60)


torch = pytest.importorskip("torch")
dcp = pytest.importorskip("torch.distributed.checkpoint")

from torch.distributed.checkpoint.api import CheckpointException  # noqa: E402

import racer.dcp as rdcp  # noqa: E402


def rank0_failure(call):
    """Run a DCP call that must fail and return rank 0's exception."""
    with pytest.raises(CheckpointException) as e:
        call()
    return e.value.failures[0][0]

BUCKET = "ckpt"
PREFIX = "runs/a"


@pytest.fixture(autouse=True)
def uploads(server):
    # Data objects upload in the background while the writer polls them.
    server.advance_uploads = True


@pytest.fixture
def client(server):
    return racer.Client(socket_path=server.path, timeout=10)


def state():
    base = torch.arange(64, dtype=torch.float32).reshape(8, 8)
    return {
        "w": torch.randn(16, 4),
        "b": torch.arange(10, dtype=torch.int64),
        "h": torch.ones(3, dtype=torch.bfloat16),
        "view": base[2:4],  # a view of a larger storage
        "t": base.t(),  # non-contiguous
        "step": 7,
        "name": "model",
    }


def blank(s):
    return {k: torch.zeros_like(v) if isinstance(v, torch.Tensor) else None for k, v in s.items()}


def save(client, sd, checkpoint_id, **kw):
    kw.setdefault("step", 1)
    # Bound the upload wait: the writer's defaults poll each second for up to
    # an hour, so a fake that never commits would stall the suite.
    kw.setdefault("upload_poll_interval", 0.01)
    kw.setdefault("upload_timeout", 5)
    writer = rdcp.RacerStorageWriter(client, BUCKET, PREFIX, **kw)
    with warnings.catch_warnings():
        warnings.simplefilter("ignore")
        dcp.save(sd, checkpoint_id=checkpoint_id, storage_writer=writer)
    return writer


def load(client, sd, checkpoint_id):
    reader = rdcp.RacerStorageReader(client, BUCKET, PREFIX)
    with warnings.catch_warnings():
        warnings.simplefilter("ignore")
        dcp.load(sd, checkpoint_id=checkpoint_id, storage_reader=reader)
    return reader


def key(name, checkpoint_id="c1"):
    return rdcp.checkpoint_request(BUCKET, PREFIX, checkpoint_id, name).hex


def test_save_load_round_trip(server, client):
    sd = state()
    writer = save(client, sd, "c1")
    assert writer.written == ["__0_0.distcp", ".metadata"]
    out = blank(sd)
    load(client, out, "c1")
    for k, v in sd.items():
        if isinstance(v, torch.Tensor):
            assert torch.equal(out[k], v), k
        else:
            assert out[k] == v, k


def test_metadata_is_written_last_with_replicas_2(server, client):
    save(client, state(), "c1")
    data = server.written[key("__0_0.distcp")]
    meta = server.written[key(".metadata")]
    assert meta.seq > data.seq
    assert max(w.seq for w in server.written.values()) == meta.seq
    assert data.replicas == meta.replicas == 2
    assert data.kind == "host-write" and meta.kind == "put"
    metadata = server.written[key(".metadata")].metadata
    assert metadata == racer.canonical_json(BUCKET, f"{PREFIX}/c1/.metadata")


@pytest.mark.parametrize(
    "step,every,mode",
    [(0, 0, "never"), (3, 0, "never"), (3, 3, "auto"), (4, 3, "never"), (6, 3, "auto"), (5, 1, "auto")],
)
def test_upload_mode_by_step(server, client, step, every, mode):
    assert rdcp.upload_mode(step, every) == mode
    cid = f"s{step}-{every}"
    save(client, state(), cid, step=step, persist_every=every, hold_seconds=600)
    data = server.written[key("__0_0.distcp", cid)]
    meta = server.written[key(".metadata", cid)]
    assert data.hold_seconds == meta.hold_seconds == 600
    assert data.upload == mode
    if mode == "auto":
        # Data uploads; .metadata is manual and persisted after the data commits.
        assert data.state == "committed"
        assert meta.upload == "manual" and meta.state == "pending"
        assert server.persisted == [(key(".metadata", cid), [key("__0_0.distcp", cid)])]
    else:
        assert data.state == meta.state == meta.upload == "never"
        assert server.persisted == []


def test_uploaded_metadata_waits_for_every_data_object(server, client):
    server.upload_steps[key("__0_0.distcp")] = 3
    writer = save(client, state(), "c1", step=2, persist_every=2, upload_poll_interval=0.01)
    assert writer.written == ["__0_0.distcp", ".metadata"]
    data = key("__0_0.distcp")
    assert server.heads.count(data) == 4
    assert server.persisted == [(key(".metadata"), [data])]
    assert server.written[key(".metadata")].state == "pending"


def test_uploads_finished_before_polling_count_as_committed(server, client):
    # Every data upload committed before the first poll and the server no
    # longer tracks the sessions: HEAD answers from the origin with a Blob
    # ETag and no upload state.
    server.advance_uploads = False
    server.forget_uploads = True
    writer = save(client, state(), "c1", step=1, persist_every=1, upload_poll_interval=0.01, upload_timeout=5)
    data = key("__0_0.distcp")
    assert writer.written == ["__0_0.distcp", ".metadata"]
    assert server.objects[data].etag.startswith('"blob-')
    assert server.heads.count(data) == 1
    assert server.persisted == [(key(".metadata"), [data])]


def test_untracked_racer_etag_is_not_committed(server, client):
    # A Racer version ETag with no upload state is not known to be at the
    # origin, so the writer keeps waiting and times out.
    writer = rdcp.RacerStorageWriter(
        client, BUCKET, PREFIX, step=1, persist_every=1, upload_poll_interval=0.01, upload_timeout=0.05
    )
    write_data = writer._write_data

    def untracked(plan, planner):
        results = write_data(plan, planner)
        for name, w in server.written.items():
            if name != key(".metadata"):
                w.forgotten = True
        return results

    writer._write_data = untracked
    with warnings.catch_warnings():
        warnings.simplefilter("ignore")
        failure = rank0_failure(lambda: dcp.save(state(), checkpoint_id="c1", storage_writer=writer))
    assert isinstance(failure, rdcp.CheckpointUploadError)
    assert server.persisted == []


def test_failed_data_upload_does_not_persist_metadata(server, client):
    server.upload_fail.add(key("__0_0.distcp"))
    failure = rank0_failure(lambda: save(client, state(), "c1", step=1, persist_every=1, upload_poll_interval=0.01))
    assert isinstance(failure, rdcp.CheckpointUploadError)
    assert server.written[key(".metadata")].state == "manual"
    assert server.persisted == []


def test_upload_wait_times_out(server, client):
    server.upload_steps[key("__0_0.distcp")] = 10**6
    failure = rank0_failure(
        lambda: save(client, state(), "c1", step=1, persist_every=1, upload_poll_interval=0.01, upload_timeout=0.05)
    )
    assert isinstance(failure, rdcp.CheckpointUploadError)
    assert server.persisted == []


def test_upload_wait_rejects_bad_settings(client):
    with pytest.raises(racer.RacerError):
        rdcp.RacerStorageWriter(client, BUCKET, PREFIX, step=1, upload_poll_interval=0)
    with pytest.raises(racer.RacerError):
        rdcp.RacerStorageWriter(client, BUCKET, PREFIX, step=1, upload_timeout=-1)


def test_upload_mode_rejects_negative():
    with pytest.raises(racer.RacerError):
        rdcp.upload_mode(1, -1)


def test_incomplete_checkpoint(server, client):
    save(client, state(), "c1")
    reader = rdcp.RacerStorageReader(client, BUCKET, PREFIX, "c1")
    assert reader.is_complete()
    client.delete(rdcp.checkpoint_request(BUCKET, PREFIX, "c1", ".metadata"))
    assert not reader.is_complete()
    err = rank0_failure(lambda: load(client, blank(state()), "c1"))
    assert "has no .metadata" in str(err)
    with pytest.raises(rdcp.IncompleteCheckpoint):
        reader.read_metadata()
    with pytest.raises(FileNotFoundError):
        reader.read_metadata()


def test_failed_data_write_leaves_no_metadata(server, client):
    def reject(kind, k, headers):
        return (503, "dirty-cap", 1) if kind == "host-write" else None

    server.write_reject = reject
    assert isinstance(rank0_failure(lambda: save(client, state(), "c1")), racer.Busy)
    assert key(".metadata") not in server.written
    assert not rdcp.RacerStorageReader(client, BUCKET, PREFIX, "c1").is_complete()


def test_checkpoints_are_create_once(server, client):
    save(client, state(), "c1")
    assert isinstance(rank0_failure(lambda: save(client, state(), "c1")), racer.Exists)


def test_separate_checkpoints_do_not_collide(server, client):
    a, b = state(), state()
    save(client, a, "c1")
    save(client, b, "c2", step=2)
    out = blank(b)
    load(client, out, "c2")
    assert torch.equal(out["w"], b["w"]) and not torch.equal(out["w"], a["w"])


def test_writer_needs_checkpoint_id(client):
    writer = rdcp.RacerStorageWriter(client, BUCKET, PREFIX, step=0)
    with pytest.raises(racer.RacerError):
        writer.request(".metadata")
    assert not rdcp.RacerStorageWriter.validate_checkpoint_id("c1")
    assert not rdcp.RacerStorageReader.validate_checkpoint_id("c1")


def test_checkpoint_request_paths():
    assert rdcp.checkpoint_request("b", "/p/", "c", "x").metadata == racer.canonical_json("b", "p/c/x")
    assert rdcp.checkpoint_request("b", "", "c", "x").metadata == racer.canonical_json("b", "c/x")


def test_multi_rank_save_load(server, tmp_path, small_pages):
    # torch.distributed object collectives need numpy.
    pytest.importorskip("numpy")
    import torch.multiprocessing as mp

    import dcp_worker

    mp.spawn(
        dcp_worker.save_and_load,
        args=(2, str(tmp_path / "store"), server.path, small_pages),
        nprocs=2,
        join=True,
    )
    names = {rdcp.checkpoint_request(BUCKET, "runs/m", "multi", n).hex: n for n in ("__0_0.distcp", "__1_0.distcp", ".metadata")}
    seqs = {names[k]: w.seq for k, w in server.written.items() if k in names}
    assert set(seqs) == set(names.values())
    assert seqs[".metadata"] > max(seqs["__0_0.distcp"], seqs["__1_0.distcp"])
    assert all(server.written[k].upload == "auto" for k, n in names.items() if n != ".metadata")
    meta = rdcp.checkpoint_request(BUCKET, "runs/m", "multi", ".metadata").hex
    assert server.written[meta].upload == "manual"
    # .metadata is persisted only after both ranks' data objects committed.
    assert len(server.persisted) == 1
    persisted, committed = server.persisted[0]
    assert persisted == meta
    assert set(committed) == {k for k, n in names.items() if n != ".metadata"}


def test_uploaded_step_needs_the_origin_container_as_bucket(server, client):
    # Like the dataplane: an uploading write must name the origin's container.
    def reject(kind, k, headers):
        upload = headers.get("racer-upload", server.default_upload)
        if upload != "never" and f'"bucket":"{BUCKET}"' not in headers.get("racer-metadata", ""):
            return (400, "bucket", None)
        return None

    server.write_reject = reject
    def foreign():
        writer = rdcp.RacerStorageWriter(client, "other", PREFIX, step=3, persist_every=3)
        with warnings.catch_warnings():
            warnings.simplefilter("ignore")
            dcp.save(state(), checkpoint_id="c1", storage_writer=writer)

    failure = rank0_failure(foreign)
    assert isinstance(failure, racer.errors.WriteInvalid) and failure.code == "bucket"
    # Cache-only steps never reach the origin, so any bucket works.
    writer = rdcp.RacerStorageWriter(client, "other", PREFIX, step=4, persist_every=3)
    with warnings.catch_warnings():
        warnings.simplefilter("ignore")
        dcp.save(state(), checkpoint_id="c2", storage_writer=writer)
    assert writer.written == ["__0_0.distcp", ".metadata"]
    # With the container as bucket, the uploaded step saves.
    save(client, state(), "c3", step=3, persist_every=3)
