#!/opt/venv/bin/python3
"""No-GPU software contract check, never a certification verdict."""

import decimal
import grp
import hashlib
import importlib.metadata
import inspect
import json
import os
import pathlib
import pwd
import re
import shutil
import signal
import subprocess
import sys
import tempfile


ROOT = pathlib.Path("/opt/certification-image")


def command(arguments, *, environment=None):
    return subprocess.run(
        arguments, check=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
        text=True, env=environment, timeout=60,
    ).stdout.strip()


def runtime_identity_check():
    uid, gid = os.getuid(), os.getgid()
    if (uid, os.geteuid(), gid, os.getegid()) != (65532, 65532, 65532, 65532):
        raise RuntimeError("The non-root identity probe requires real/effective UID/GID 65532")
    try:
        account = pwd.getpwuid(uid)
    except KeyError as error:
        raise RuntimeError(f"No passwd identity exists for uid {uid}") from error
    try:
        group = grp.getgrgid(gid)
    except KeyError as error:
        raise RuntimeError(f"No group identity exists for gid {gid}") from error
    if (not account.pw_name or not group.gr_name or account.pw_uid != uid
            or account.pw_gid != gid or group.gr_gid != gid or account.pw_shell != "/bin/bash"):
        raise RuntimeError("The fixed identity must have its primary group and a Bash shell")
    home = pathlib.Path(account.pw_dir)
    if not home.is_absolute() or not home.is_dir() or not os.access(home, os.R_OK | os.X_OK):
        raise RuntimeError(f"The fixed identity needs an accessible absolute home directory: {home}")
    try:
        version = command(["ssh", "-V"])
    except (subprocess.CalledProcessError, subprocess.TimeoutExpired) as error:
        raise RuntimeError(
            f"Non-root ssh -V failed: {error}; output={error.output!r}, stderr={error.stderr!r}"
        ) from error
    if not version.startswith("OpenSSH_"):
        raise RuntimeError(f"Non-root ssh -V did not report OpenSSH: {version!r}")
    evidence = {
        "uid": uid, "gid": gid, "user": account.pw_name, "group": group.gr_name,
        "home": str(home), "shell": account.pw_shell, "ssh_version": version,
    }
    print(json.dumps({"nonroot_identity": evidence}, sort_keys=True), flush=True)
    return evidence


def mpi_check(executable):
    try:
        result = subprocess.run(
            [executable, "--version"], check=False, capture_output=True, text=True, timeout=60,
        )
    except subprocess.TimeoutExpired as error:
        raise RuntimeError(
            f"MPI launcher version timed out: stdout={error.stdout!r}, stderr={error.stderr!r}"
        ) from error
    evidence = {
        "executable": executable, "returncode": result.returncode,
        "stdout": result.stdout, "stderr": result.stderr,
    }
    print(json.dumps({"mpi_launcher_version": evidence}, sort_keys=True), flush=True)
    lines = result.stdout.splitlines()
    header = rf"{re.escape(pathlib.Path(executable).name)} \((?:Open MPI|OpenRTE)\) [0-9]+\.[0-9]+\.[0-9]+"
    if result.returncode != 0 or not lines or re.fullmatch(header, lines[0]) is None:
        raise RuntimeError(
            "The MPI launcher did not report a valid OpenMPI/OpenRTE version; "
            "see mpi_launcher_version exit code, stdout and stderr"
        )
    return evidence


def loader_check(executable):
    environment = {"PATH": "/usr/sbin:/usr/bin:/sbin:/bin", "HOME": "/tmp", "LC_ALL": "C"}
    dynamic = command(["/usr/bin/readelf", "--dynamic", executable], environment=environment)
    if re.search(r"(?m)\((?:RPATH|RUNPATH)\).*?/stubs(?:/|[:\]])", dynamic):
        raise RuntimeError(f"CUDA driver stubs must not appear in runtime paths: {executable}")
    text = command(["/usr/bin/ldd", executable], environment=environment)
    missing = re.findall(r"(?m)^\s*(\S+)\s+=>\s+not found\s*$", text)
    if "/stubs/" in text:
        raise RuntimeError(f"CUDA driver stubs must not resolve at runtime: {executable}")
    unexpected = sorted(set(missing) - {"libcuda.so.1", "libnvidia-ml.so.1"})
    if unexpected:
        raise RuntimeError(f"Sanitized loader cannot resolve {executable}: {unexpected}")
    if pathlib.Path(executable).name == "all_reduce_perf":
        nccl = re.search(r"(?m)^\s*libnccl\.so\.2\s+=>\s+(\S+)", text)
        expected = pathlib.Path("/opt/venv/lib/python3.12/site-packages/nvidia/nccl/lib/libnccl.so.2")
        if nccl is None or pathlib.Path(nccl[1]).resolve() != expected.resolve():
            raise RuntimeError("all_reduce_perf must resolve the exact wheel-provided NCCL runtime")
    return {"executable": executable, "driver_injected_libraries_absent": sorted(set(missing))}


def main():
    lock = json.loads((ROOT / "dependencies.lock.json").read_text())
    contract = json.loads((ROOT / "script-contract.json").read_text())
    if sys.version_info[:2] != (3, 12) or os.uname().machine != "x86_64":
        raise RuntimeError("The locked Python3.12/linux-amd64 ABI is required")
    resolved = {}
    for name in contract["path_tools"]:
        path = shutil.which(name)
        if path is None:
            raise RuntimeError(f"Required image tool missing: {name}")
        resolved[name] = str(pathlib.Path(path).resolve())
    for literal in ("/bin/bash", "/bin/true", "/usr/sbin/sshd"):
        if not pathlib.Path(literal).is_file() or not os.access(literal, os.X_OK):
            raise RuntimeError(f"Required literal executable missing: {literal}")
    for private_key in pathlib.Path("/etc/ssh").glob("ssh_host_*_key"):
        raise RuntimeError(f"Image contains an SSH host private key: {private_key}")
    if pathlib.Path("/root/.ssh/authorized_keys").exists():
        raise RuntimeError("The image must not contain authorized SSH keys")
    if decimal.Decimal("15e-255") <= 0:
        raise RuntimeError("Exact decimal support is unavailable")
    signal.signal(signal.SIGCHLD, signal.SIG_DFL)
    child = os.fork()
    if child == 0:
        os._exit(0)
    try:
        first = os.waitid(os.P_PID, child, os.WEXITED | os.WNOWAIT)
        again = os.waitid(os.P_PID, child, os.WEXITED | os.WNOWAIT)
        if first.si_pid != child or again.si_pid != child:
            raise RuntimeError("waitid/WNOWAIT does not retain the owned child")
    finally:
        os.waitpid(child, 0)
    installed = {}
    for name, expected in lock["python_distributions"].items():
        actual = importlib.metadata.version(name)
        if actual != expected:
            raise RuntimeError(f"Python distribution mismatch: {name}={actual}, expected {expected}")
        installed[name] = actual
    os.environ["OMP_NUM_THREADS"] = "1"
    import torch
    if torch.cuda.is_initialized():
        raise RuntimeError("Offline validation must not initialize CUDA")
    if torch.version.cuda != lock["cuda_release"]:
        raise RuntimeError(f"PyTorch CUDA release mismatch: {torch.version.cuda}")
    for name in ("mm", "softmax", "_scaled_mm", "float8_e4m3fn", "float64", "bfloat16"):
        if not hasattr(torch, name):
            raise RuntimeError(f"Required PyTorch API missing: {name}")
    if "including_emulation" not in inspect.signature(torch.cuda.is_bf16_supported).parameters:
        raise RuntimeError("Native-only BF16 capability API missing")
    flags = torch._C._cuda_getArchFlags()
    if not {"sm_100", "sm_100a"}.intersection(flags.split()):
        raise RuntimeError(f"PyTorch does not contain the required B200 SM100 target: {flags}")
    if torch.cuda.is_initialized():
        raise RuntimeError("Offline API inspection initialized CUDA")
    if not re.fullmatch(r"fio-3\.[0-9]+(?:\.[0-9]+)?", command([resolved["fio"], "--version"])):
        raise RuntimeError("PATH fio is not the required fio3.x executable")
    if not re.search(r"\bcrc32c\b", command([resolved["fio"], "--cmdhelp=verify"])):
        raise RuntimeError("fio CRC32C verification is unavailable")
    mpi_version = mpi_check(resolved["mpirun"])
    command([resolved["ssh"], "-V"])
    command(["/usr/sbin/sshd", "-V"])
    command([resolved["journalctl"], "--version"])
    if f"release {lock['cuda_release']}," not in command([resolved["nvcc"], "--version"]):
        raise RuntimeError("CUDA compiler release disagrees with the lock")
    with tempfile.TemporaryDirectory(prefix="openmp-image-check-") as directory:
        source, output = pathlib.Path(directory) / "check.c", pathlib.Path(directory) / "check"
        source.write_text("#include <omp.h>\nint main(void) { return omp_get_max_threads() < 1; }\n")
        command([resolved["cc"], "-O3", "-std=c11", "-fopenmp", str(source), "-o", str(output), "-lm"])
        loader_check(str(output))
    loaders = [loader_check(resolved[name]) for name in ("nvbandwidth", "all_reduce_perf", "gpu_burn")]
    kernel = pathlib.Path("/opt/gpu-burn/compare.fatbin")
    if pathlib.Path.cwd() != kernel.parent or not kernel.is_file() or kernel.stat().st_size == 0:
        raise RuntimeError("Frozen gpu_burn argv requires its real compare.fatbin in the image working directory")
    command([sys.executable, "-m", "pip", "check"])
    print(json.dumps({
        "status": "software-prerequisites-only",
        "script_commit": contract["commit"], "platform": lock["platform"],
        "python": sys.version.split()[0], "python_distributions": installed,
        "torch_cuda_release": torch.version.cuda, "torch_compiled_architectures": flags,
        "mpi_launcher_version": mpi_version,
        "resolved_tools": resolved, "sanitized_loader": loaders,
        "compare_fatbin_sha256": hashlib.sha256(kernel.read_bytes()).hexdigest(),
        "dpkg_packages": [
            line[3:] for line in command([
                "dpkg-query", "-W", "-f=${db:Status-Abbrev}${binary:Package}=${Version}\n",
            ]).splitlines() if line.startswith("ii ")
        ],
        "gpu_workloads_executed": False, "hardware_qualification": False,
        "remaining_runtime_gates": contract["runtime_gates"],
    }, indent=2, sort_keys=True))


if __name__ == "__main__":
    if len(sys.argv) != 1:
        raise SystemExit("validate-image.py accepts no script arguments or runtime policies")
    main()
