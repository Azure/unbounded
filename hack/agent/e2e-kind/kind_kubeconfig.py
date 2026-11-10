# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

"""Pin the e2e harnesses to the Kind cluster's own kubeconfig.

The harnesses create secrets, Machines and controllers. Run against the user's
default kubeconfig, whatever its current-context names at that moment receives
them: another shell switching the context mid-run sends bootstrap tokens to a
real cluster. pin() points KUBECONFIG, for this process and every child it
starts (kubectl, kubectl-unbounded, helm, go test), at a file that holds only
the Kind cluster's context, and refuses to go on when it does not.
"""

from __future__ import annotations

import contextlib
import os
import subprocess
import tempfile
from collections.abc import Callable, MutableMapping
from pathlib import Path

# ENV_OVERRIDE names a kubeconfig to use as it is instead of the one written
# from `kind get kubeconfig`. It is still checked.
ENV_OVERRIDE = "E2E_KUBECONFIG"

# A runner takes an argv and returns (exit code, stdout, stderr).
Runner = Callable[[list[str]], tuple[int, str, str]]


class KubeconfigError(Exception):
    """The kubeconfig cannot be pinned to the Kind cluster."""


def kind_context(cluster: str) -> str:
    """Return the context name kind gives *cluster*."""
    return f"kind-{cluster}"


def default_path(repo_root: Path, cluster: str) -> Path:
    """Return the dedicated kubeconfig of *cluster* under the repo's tmp/."""
    return repo_root / "tmp" / "e2e-kind" / f"{cluster}.kubeconfig"


def resolve_path(repo_root: Path, cluster: str, environ: MutableMapping[str, str],
                 path: Path | None = None) -> tuple[Path, bool]:
    """Return the kubeconfig path and whether it is written from kind.

    ENV_OVERRIDE wins and is used as it is; otherwise *path*, or the default
    under the repo's tmp/, is written from kind.
    """
    override = environ.get(ENV_OVERRIDE, "")
    if override:
        return Path(override), False
    return (path or default_path(repo_root, cluster)), True


def _run(argv: list[str]) -> tuple[int, str, str]:
    try:
        done = subprocess.run(argv, capture_output=True, text=True, check=False, timeout=60)
    except (OSError, subprocess.TimeoutExpired) as exc:
        return 127, "", str(exc)
    return done.returncode, done.stdout, done.stderr


def user_default_kubeconfig(environ: MutableMapping[str, str]) -> Path:
    """Return kubectl's default kubeconfig, which the harness must never use."""
    home = environ.get("HOME") or str(Path.home())
    return Path(home) / ".kube" / "config"


def write_from_kind(cluster: str, path: Path, runner: Runner = _run) -> None:
    """Write `kind get kubeconfig` of *cluster* to *path*, owner-only.

    kind reads the cluster from the container runtime, not from any kubeconfig,
    so this never consults the user's.
    """
    rc, out, err = runner(["kind", "get", "kubeconfig", "--name", cluster])
    if rc != 0 or not out.strip():
        raise KubeconfigError(f"kind get kubeconfig --name {cluster} failed: {(err or out).strip() or f'exit {rc}'}")

    path.parent.mkdir(parents=True, exist_ok=True)
    # Several harness processes pin at once (the agent config e2e runs them in
    # parallel), so each writes its own temporary file. A shared name lets one
    # process rename the file away before another renames it.
    fd, tmp = tempfile.mkstemp(dir=path.parent, prefix=path.name + ".", suffix=".tmp")
    try:
        with os.fdopen(fd, "w") as handle:
            handle.write(out)
        os.replace(tmp, path)
    except BaseException:
        with contextlib.suppress(FileNotFoundError):
            os.unlink(tmp)
        raise


def check(cluster: str, path: Path, runner: Runner = _run, environ: MutableMapping[str, str] | None = None) -> None:
    """Fail unless *path* holds exactly the Kind context, and it is current."""
    environ = os.environ if environ is None else environ
    want = kind_context(cluster)

    if path.expanduser().resolve() == user_default_kubeconfig(environ).resolve():
        raise KubeconfigError(f"refusing to use the default kubeconfig {path}; the e2e harness needs its own file")

    if not path.is_file():
        raise KubeconfigError(f"kubeconfig {path} does not exist")

    base = ["kubectl", "--kubeconfig", str(path), "config"]
    rc, out, err = runner([*base, "get-contexts", "-o", "name"])
    if rc != 0:
        raise KubeconfigError(f"cannot list the contexts of {path}: {err.strip() or f'exit {rc}'}")
    contexts = out.split()
    if contexts != [want]:
        raise KubeconfigError(f"kubeconfig {path} must hold only context {want!r}, it holds {contexts}")

    rc, out, err = runner([*base, "current-context"])
    current = out.strip()
    if rc != 0 or current != want:
        raise KubeconfigError(
            f"kubeconfig {path} current-context is {current or '(unset)'!r}, not the Kind cluster's {want!r}")


def pin(repo_root: Path, cluster: str, *, required: bool = True, path: Path | None = None,
        runner: Runner = _run, environ: MutableMapping[str, str] | None = None) -> Path | None:
    """Point KUBECONFIG at the Kind cluster's own kubeconfig and check it.

    KUBECONFIG is set before anything else, so even when the file cannot be
    written no child falls back to the user's default kubeconfig. With
    *required* False a cluster that does not exist yet is not an error (the
    command does not need one), and None is returned; a file that exists but
    names another cluster always is.
    """
    environ = os.environ if environ is None else environ
    path, from_kind = resolve_path(repo_root, cluster, environ, path)
    environ["KUBECONFIG"] = str(path)

    if from_kind:
        try:
            write_from_kind(cluster, path, runner)
        except KubeconfigError:
            if required:
                raise
            # A stale file of an earlier cluster must not stand in for it.
            path.unlink(missing_ok=True)
            return None
    elif not required and not path.is_file():
        return None

    check(cluster, path, runner, environ)
    return path
