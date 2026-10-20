#!/usr/bin/env python3
"""Check locked inputs or build on an explicitly selected native amd64 engine."""

import argparse
import hashlib
import json
import pathlib
import re
import stat
import subprocess
import sys
import urllib.parse


ROOT = pathlib.Path(__file__).resolve().parent
INPUTS = (
    ".dockerignore",
    "Containerfile",
    "dependencies.lock.json",
    "fetch-sources.py",
    "requirements.lock",
    "script-contract.json",
    "ubuntu-InRelease.sha256",
    "ubuntu.sources",
    "validate-image.py",
)


def check_inputs(root=ROOT):
    hashes = {}
    for name in INPUTS:
        path = root / name
        if not path.is_file() or path.is_symlink():
            raise ValueError(f"Missing or nonregular build input: {name}")
        hashes[name] = hashlib.sha256(path.read_bytes()).hexdigest()
    ignored = (root / ".dockerignore").read_text().splitlines()
    if ignored != ["**", *("!" + name for name in INPUTS)]:
        raise ValueError("The build context must allow only the locked image inputs")
    lock = json.loads((root / "dependencies.lock.json").read_text())
    if lock["platform"] != "linux/amd64":
        raise ValueError("Only the confirmed linux/amd64 target is supported")
    if not re.fullmatch(r"nvidia/cuda:[^@\s]+@sha256:[0-9a-f]{64}", lock["base"]):
        raise ValueError("The official CUDA base must include a full digest")
    if f"FROM {lock['base']}\n" not in (root / "Containerfile").read_text():
        raise ValueError("Containerfile and locked CUDA base disagree")
    if not re.fullmatch(r"[0-9]{8}T[0-9]{6}Z", lock["ubuntu_snapshot"]):
        raise ValueError("Ubuntu snapshot timestamp is not immutable")
    snapshot = f"https://snapshot.ubuntu.com/ubuntu/{lock['ubuntu_snapshot']}/"
    if f"URIs: {snapshot}\n" not in (root / "ubuntu.sources").read_text():
        raise ValueError("Ubuntu source and snapshot lock disagree")
    for component in lock["sources"]:
        url = urllib.parse.urlsplit(component["url"])
        if url.scheme != "https" or url.netloc != "codeload.github.com":
            raise ValueError(f"Unsupported upstream source URL: {component['name']}")
        if not re.fullmatch(r"[0-9a-f]{40}", component["commit"]):
            raise ValueError(f"Unpinned upstream commit: {component['name']}")
        if not url.path.endswith("/" + component["commit"]):
            raise ValueError(f"Archive URL and commit disagree: {component['name']}")
        if not re.fullmatch(r"[0-9a-f]{64}", component["sha256"]):
            raise ValueError(f"Unpinned source archive: {component['name']}")
    requirements = (root / "requirements.lock").read_text().splitlines()
    if not requirements:
        raise ValueError("Python wheel lock is empty")
    distributions = {}
    def normalize(name):
        return re.sub(r"[-_.]+", "-", name).lower()
    for line in requirements:
        match = re.fullmatch(r"([A-Za-z0-9_.-]+) @ (\S+) --hash=sha256:([0-9a-f]{64})", line)
        if not match:
            raise ValueError("Every Python dependency needs one exact wheel URL and SHA-256")
        name = normalize(match[1])
        url = urllib.parse.urlsplit(match[2])
        if url.scheme != "https" or url.netloc not in (
            "download.pytorch.org", "download-r2.pytorch.org", "files.pythonhosted.org",
        ):
            raise ValueError("Python wheels must use their official distribution host")
        if not url.path.endswith(".whl") or url.query or url.fragment or url.username:
            raise ValueError("Unsupported Python wheel URL")
        wheel = urllib.parse.unquote(pathlib.PurePosixPath(url.path).name)[:-4].split("-")
        if len(wheel) != 5 or normalize(wheel[0]) != name or name in distributions:
            raise ValueError("Python wheel identity is missing, duplicate, or inconsistent")
        if wheel[2] not in ("py3", "cp312") or wheel[3] not in ("none", "cp312"):
            raise ValueError("Python wheel must support the locked CPython3.12 ABI")
        if wheel[4] != "any" and not all(
            re.fullmatch(r"manylinux(?:[0-9]+|_[0-9]+_[0-9]+)_x86_64", tag)
            for tag in wheel[4].split(".")
        ):
            raise ValueError("Python wheel must support the locked linux/amd64 platform")
        distributions[name] = wheel[1]
    expected = {normalize(name): version for name, version in lock["python_distributions"].items()}
    if distributions != expected:
        raise ValueError("Python wheel versions and installed-distribution expectations disagree")
    contract = json.loads((root / "script-contract.json").read_text())
    scripts = root.parent / "project-signal-certification/scripts"
    actual = {path.name: path for path in scripts.glob("*.sh")}
    if len(actual) != 15 or set(actual) != set(contract["scripts"]):
        raise ValueError("The exact fifteen-script source contract is required")
    for name, expected in contract["scripts"].items():
        path = actual[name]
        info = path.lstat()
        if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) != 0o755:
            raise ValueError(f"Script must be a regular 0755 file: {name}")
        if hashlib.sha256(path.read_bytes()).hexdigest() != expected:
            raise ValueError(f"Frozen script changed: {name}")
    payload = json.dumps(hashes, sort_keys=True, separators=(",", ":")).encode()
    return lock, {"platform": lock["platform"], "inputs": hashes,
                  "context_sha256": hashlib.sha256(payload).hexdigest(),
                  "script_commit": contract["commit"]}


def native_host(engine):
    result = subprocess.run(
        [engine, "info", "--format", "{{json .}}"], check=True,
        capture_output=True, text=True, timeout=30,
    )
    info = json.loads(result.stdout)
    if engine == "docker":
        operating_system, architecture = info["OSType"], info["Architecture"]
    else:
        operating_system, architecture = info["host"]["os"], info["host"]["arch"]
    if operating_system != "linux" or architecture not in ("amd64", "x86_64"):
        raise ValueError(
            f"Native linux/amd64 builder required; found {operating_system}/{architecture}. "
            "ARM builds and emulation are not authorized."
        )
    return {"os": operating_system, "architecture": architecture}


def build(engine, directory, root=ROOT):
    directory = directory.resolve()
    if directory == root or root in directory.parents:
        raise ValueError("Build evidence must be outside the image source directory")
    directory.mkdir(parents=True, exist_ok=False)
    report = {"status": "blocked", "stage": "inputs", "registry_publication": False,
              "registry_digest": None, "image_id": None}
    try:
        lock, checked = check_inputs(root)
        report.update(checked)
        report["stage"] = "native-host"
        report["host"] = native_host(engine)
        report["stage"] = "build"
        tag = "localhost/project-signal-certification-runner:" + checked["context_sha256"][:16]
        command = [
            engine, "build", "--platform", "linux/amd64",
            "--file", str(root / "Containerfile"), "--tag", tag,
            "--build-arg", f"SOURCE_DATE_EPOCH={lock['source_date_epoch']}",
            "--iidfile", str(directory / "image-id"), str(root),
        ]
        report["build_argv"] = command
        with (directory / "build.log").open("x") as log:
            subprocess.run(command, check=True, stdout=log, stderr=subprocess.STDOUT)
        image = (directory / "image-id").read_text().strip()
        if not re.fullmatch(r"sha256:[0-9a-f]{64}", image):
            raise ValueError("Engine did not produce a full local image ID")
        report["image_id"], report["local_tag"] = image, tag
        report["stage"] = "offline-image-check"
        isolation = [
            engine, "run", "--rm", "--pull=never", "--network=none",
            "--cap-drop=ALL", "--security-opt=no-new-privileges", "--read-only",
            "--tmpfs", "/tmp:rw,nosuid,nodev,size=64m",
            "--env", "NVIDIA_VISIBLE_DEVICES=void", "--env", "CUDA_VISIBLE_DEVICES=",
        ]
        command = [
            *isolation,
            "--entrypoint", "/opt/venv/bin/python3", image,
            "/opt/certification-image/validate-image.py",
        ]
        report["validation_argv"] = command
        with (directory / "offline-image-check.log").open("x") as log:
            subprocess.run(command, check=True, stdout=log, stderr=subprocess.STDOUT)
        report["stage"] = "offline-identity-check"
        command = [
            *isolation, "--user=65532:65532",
            "--entrypoint", "/opt/venv/bin/python3", image, "-c",
            'import runpy; runpy.run_path("/opt/certification-image/validate-image.py")["runtime_identity_check"]()',
        ]
        report["identity_validation_argv"] = command
        with (directory / "offline-identity-check.log").open("x") as log:
            subprocess.run(command, check=True, stdout=log, stderr=subprocess.STDOUT)
        report["status"], report["stage"] = "built-offline-validated", "complete"
    except (FileNotFoundError, subprocess.SubprocessError, ValueError, KeyError) as error:
        detail = getattr(error, "stderr", None) or str(error)
        if isinstance(detail, bytes):
            detail = detail.decode(errors="replace")
        report["error"] = detail[:4096]
        print(f"Runner build blocked at {report['stage']}: {detail}", file=sys.stderr)
    (directory / "result.json").write_text(json.dumps(report, indent=2, sort_keys=True) + "\n")
    print(json.dumps(report, indent=2, sort_keys=True))
    return 0 if report["status"] == "built-offline-validated" else 2


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("check", "build"))
    parser.add_argument("--engine", choices=("docker", "podman"), default="docker")
    parser.add_argument("--output-directory", type=pathlib.Path)
    args = parser.parse_args()
    if args.action == "build":
        if args.output_directory is None:
            parser.error("build requires a new --output-directory outside the image source")
        return build(args.engine, args.output_directory)
    _, report = check_inputs()
    print(json.dumps({"status": "locked-inputs-verified", **report}, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
