#!/usr/bin/env python3
"""Current-source preflight kernel checks; no image build, kind, or slab allocation.

python3 e2e/racer/scripts/preflight.py bin/racer-preflight tmp
Uses the existing ubuntu:noble image (override RACER_PREFLIGHT_IMAGE).
The normal case uses seccomp=unconfined, matching the operator deployment.
This is not a full deployment/hardware certification.
"""
import argparse
import os
from pathlib import Path
import subprocess
import tempfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("binary", type=Path)
    parser.add_argument("parent", type=Path, help="existing workspace-local ext4 scratch directory")
    args = parser.parse_args()
    binary = args.binary.resolve(strict=True)
    parent = args.parent.resolve(strict=True)
    image = os.environ.get("RACER_PREFLIGHT_IMAGE", "ubuntu:noble")

    with tempfile.TemporaryDirectory(prefix="racer-preflight-", dir=parent) as directory:
        # Container root drops DAC_OVERRIDE, so allow its unlinked storage probe.
        Path(directory).chmod(0o777)

        def check(name, *, unconfined=True, resource=True, cpu="3", memory="2g", lock="268435456",
                  tmpfs=False, cpuset=None, expected=None, shell=False):
            command = ["docker", "run", "--rm", "--pull=never", "--network=none",
                       "--user", "0:0", "--cap-drop=ALL", "--read-only",
                       "--security-opt=no-new-privileges",
                       "--cpus", cpu, "--memory", memory, "--memory-swap", memory,
                       "--ulimit", f"memlock={lock}:{lock}", "--mount",
                       f"type=bind,src={binary},dst=/probe,readonly"]
            if resource:
                command += ["--cap-add=SYS_RESOURCE"]
            if unconfined:
                command += ["--security-opt=seccomp=unconfined"]
            if cpuset:
                command += ["--cpuset-cpus", cpuset]
            command += (["--tmpfs", "/cache"] if tmpfs else
                        ["--mount", f"type=bind,src={directory},dst=/cache"])
            for name_, value in dict(RACER_SHARDS=1, RACER_IO_WORKERS=1,
                                    RACER_COMPUTE_WORKERS=1, RACER_BUFFERS_PER_NODE=8,
                                    RACER_SLAB_SIZE=67108864,
                                    RACER_SLAB_PATH="/cache/cache.slab").items():
                command += ["-e", f"{name_}={value}"]
            command += [image]
            command += (["/bin/sh", "-ec", "ulimit -l 262144; exec /probe"]
                        if shell else ["/probe"])
            result = subprocess.run(command, capture_output=True, text=True, timeout=20)
            output = result.stdout + result.stderr
            if expected is None:
                assert result.returncode == 0 and "preflight passed" in output, (name, output)
            else:
                assert result.returncode != 0 and expected in output, (name, output)
            print(f"PASS {name}: {output.strip()}", flush=True)
            assert not list(Path(directory).iterdir()), "preflight left a slab/scratch file"

        check("production pool/ring startup")
        check("shell raises low hard memlock and exec inherits it", lock="65536", shell=True)
        check("no capabilities needed with sufficient inherited memlock", resource=False)
        check("raising hard memlock requires SYS_RESOURCE", resource=False, lock="65536",
              shell=True, expected="Operation not permitted")
        check("quota smaller than affinity", cpu="1", expected="CPU quota below 3")
        check("undersized memory", memory="1g", expected="2GiB container memory.max")
        check("low inherited memlock", lock="65536", expected="inherited soft memlock")
        check("hostPath on wrong filesystem", tmpfs=True, expected="cache mount must be ext4")
        check("only one physical core", cpuset=str(min(os.sched_getaffinity(0))),
              expected="needs disjoint positive I/O and compute counts")
        check("default container seccomp denies required syscall", unconfined=False,
              expected="Operation not permitted")


if __name__ == "__main__":
    main()
