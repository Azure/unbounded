#!/usr/bin/env python3
"""Fetch only checksum-verified, commit-pinned public build sources."""

import hashlib
import json
import os
import pathlib
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
import urllib.parse
import urllib.request


LICENSES = pathlib.Path("/usr/share/licenses/project-signal-certification-runner")


def git_metadata(target, component):
    repository = "https://github.com/" + "/".join(
        urllib.parse.urlsplit(component["url"]).path.strip("/").split("/")[:2]
    ) + ".git"
    if component["git_metadata"] != repository or (target / ".git").exists():
        raise ValueError(f"Unexpected Git metadata source: {component['name']}")
    environment = {"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "HOME": str(target),
                   "LC_ALL": "C", "GIT_TERMINAL_PROMPT": "0",
                   "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_SYSTEM": "/dev/null"}
    for name in ("GIT_EXEC_PATH", "HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy"):
        if name in os.environ:
            environment[name] = os.environ[name]
    prefix = ["git", "-C", str(target)]
    for arguments in (
        ["init", "--quiet", "--template="],
        ["fetch", "--depth=1", "--no-tags", repository, component["commit"]],
        ["reset", "--mixed", component["commit"]],
        ["diff", "--quiet", "--exit-code", "HEAD", "--"],
    ):
        subprocess.run(prefix + arguments, check=True, env=environment, timeout=120)
    actual = subprocess.run(
        prefix + ["rev-parse", "HEAD"], check=True, capture_output=True,
        text=True, env=environment, timeout=30,
    ).stdout.strip()
    if actual != component["commit"]:
        raise ValueError(f"Fetched Git metadata does not match the locked commit: {component['name']}")


def fetch(lock_path, destination):
    lock = json.loads(lock_path.read_text())
    destination.mkdir(mode=0o700, parents=True, exist_ok=False)
    for component in lock["sources"]:
        name = component["name"]
        if not re.fullmatch(r"[a-z][a-z0-9-]+", name):
            raise ValueError("Invalid source component name")
        url = urllib.parse.urlsplit(component["url"])
        if url.scheme != "https" or url.netloc != "codeload.github.com":
            raise ValueError(f"Unsupported source host: {name}")
        if not re.fullmatch(r"[0-9a-f]{40}", component["commit"]):
            raise ValueError(f"Source commit is not immutable: {name}")
        if not url.path.endswith("/" + component["commit"]):
            raise ValueError(f"Source URL/commit mismatch: {name}")
        if not re.fullmatch(r"[0-9a-f]{64}", component["sha256"]):
            raise ValueError(f"Missing source archive checksum: {name}")
        with tempfile.TemporaryDirectory(prefix=name + "-", dir=destination) as temporary:
            temporary = pathlib.Path(temporary)
            archive = temporary / "source.tar.gz"
            digest, size = hashlib.sha256(), 0
            with urllib.request.urlopen(component["url"], timeout=90) as response, archive.open("xb") as output:
                while chunk := response.read(1024 * 1024):
                    size += len(chunk)
                    if size > 64 * 1024 * 1024:
                        raise ValueError(f"Source archive exceeds 64MiB: {name}")
                    digest.update(chunk)
                    output.write(chunk)
            if digest.hexdigest() != component["sha256"]:
                raise ValueError(f"Source checksum mismatch: {name}")
            unpack = temporary / "unpack"
            unpack.mkdir(mode=0o700)
            with tarfile.open(archive, "r:gz") as source:
                source.extractall(unpack, filter="data")
            roots = list(unpack.iterdir())
            if len(roots) != 1 or not roots[0].is_dir() or roots[0].is_symlink():
                raise ValueError(f"Unexpected archive root: {name}")
            target = destination / name
            roots[0].rename(target)
            if component.get("git_metadata"):
                git_metadata(target, component)
            licenses = LICENSES / name
            licenses.mkdir(parents=True, exist_ok=False)
            if not component["licenses"]:
                raise ValueError(f"Missing upstream license list: {name}")
            for filename in component["licenses"]:
                relative_license = pathlib.PurePosixPath(filename)
                if relative_license.is_absolute() or ".." in relative_license.parts:
                    raise ValueError(f"Invalid upstream license path: {name}")
                license_path = target / relative_license
                if (not license_path.resolve().is_relative_to(target.resolve())
                        or not license_path.is_file() or license_path.is_symlink()):
                    raise ValueError(f"Missing upstream license: {name}")
                shutil.copyfile(license_path, licenses / relative_license.name)
            print(json.dumps({"component": name, "commit": component["commit"],
                              "archive_sha256": digest.hexdigest(), "archive_bytes": size}))


if __name__ == "__main__":
    if len(sys.argv) != 3:
        raise SystemExit("Usage: fetch-sources.py LOCK_JSON NEW_DESTINATION")
    fetch(pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2]))
