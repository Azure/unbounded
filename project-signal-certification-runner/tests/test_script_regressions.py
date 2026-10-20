import errno
import json
import os
import pathlib
import stat
import subprocess
import tempfile
import textwrap
import unittest


SCRIPTS = pathlib.Path(__file__).resolve().parents[2] / "project-signal-certification/scripts"


def executable(path, body):
    path.write_text(textwrap.dedent(body).lstrip())
    path.chmod(path.stat().st_mode | stat.S_IXUSR)


class ScriptRegressionTests(unittest.TestCase):
    def run_script(self, name, environment):
        return subprocess.run(
            ["/bin/bash", str(SCRIPTS / name)],
            env={**os.environ, **environment},
            text=True,
            capture_output=True,
            timeout=30,
        )

    def test_hardware_preflight_ignores_sysfs_metadata_files(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            commands = root / "bin"
            commands.mkdir()
            executable(commands / "nvidia-smi", """\
                #!/bin/sh
                printf '0, Fixture GPU, 1.0, Enabled, 1024\n'
            """)
            executable(commands / "lscpu", """\
                #!/bin/sh
                printf '%s\n' '{"lscpu":[{"field":"On-line CPU(s) list:","data":"0"},{"field":"NUMA node(s):","data":"1"},{"field":"Model name:","data":"Fixture CPU"}]}'
            """)
            executable(commands / "ip", """\
                #!/bin/sh
                printf '%s\n' '[{"ifname":"lo","operstate":"UNKNOWN","mtu":65536}]'
            """)
            executable(commands / "lsblk", """\
                #!/bin/sh
                printf '%s\n' '{"blockdevices":[]}'
            """)
            proc = root / "proc"
            sysfs = root / "sys"
            results = root / "results"
            scratch = root / "scratch"
            proc.mkdir()
            (sysfs / "class/net").mkdir(parents=True)
            (sysfs / "class/infiniband").mkdir()
            scratch.mkdir()
            (proc / "meminfo").write_text("MemTotal:       2048 kB\n")
            (sysfs / "class/net/bonding_masters").write_text("")
            expected = {
                "gpu": {
                    "count": 1,
                    "product_regex": "^Fixture GPU$",
                    "vbios_regex": "^1[.]0$",
                    "ecc": "Enabled",
                    "memory_min_mib": 1024,
                },
                "cpu": {"online_count": 1, "numa_nodes": 1, "model_regex": "^Fixture CPU$"},
                "memory_min_mib": 1,
                "nics": [],
                "fabric": [],
                "nvme": [],
            }
            run = self.run_script("hardware-preflight.sh", {
                "PATH": str(commands) + os.pathsep + os.environ["PATH"],
                "PROJECT_SIGNAL_RESULTS_DIR": str(results),
                "SCRATCH_ROOT": str(scratch),
                "DURATION_SECONDS": "0",
                "COMMAND_TIMEOUT_SECONDS": "5",
                "HOST_PROC_ROOT": str(proc),
                "HOST_SYSFS_ROOT": str(sysfs),
                "EXPECTED_HARDWARE_JSON": json.dumps(expected, separators=(",", ":")),
            })
            self.assertEqual(run.returncode, 0, run.stderr)
            self.assertEqual((results / "verdict").read_text(), "Pass")
            self.assertEqual((results / "reason").read_text(), "DeclaredInventoryMatched")

    def fio_fixture(self, path):
        executable(path, r"""
            #!/usr/bin/env python3
            import errno
            import json
            import os
            import sys

            arguments = sys.argv[1:]
            with open(os.environ["FIO_CALLS"], "a") as output:
                output.write(json.dumps(arguments) + "\n")
            if arguments == ["--version"]:
                print("fio-3.36")
                raise SystemExit
            values = dict(argument[2:].split("=", 1) for argument in arguments if argument.startswith("--") and "=" in argument)
            name = values["name"]
            mode = values["rw"]
            verify_only = "--verify_only=1" in arguments
            if "--time_based=1" in arguments and mode.endswith("write") and "--do_verify=1" in arguments:
                print("fio: verification read phase will never start because write phase uses all of runtime")
            error = int(os.environ.get("FAKE_FIO_VERIFY_ERRNO", "0")) if verify_only else 0
            size = int(values["size"])
            direction = "read" if mode.endswith("read") else "write"
            empty = {
                "io_bytes": 0, "total_ios": 0, "runtime": 0, "bw_bytes": 0, "iops": 0,
                "short_ios": 0, "clat_ns": {"max": 0},
            }
            active = {
                "io_bytes": size, "total_ios": 1, "runtime": 1, "bw_bytes": size,
                "iops": 1, "short_ios": 0, "clat_ns": {"max": 1},
            }
            row = {
                "jobname": name,
                "error": error,
                "read": active if direction == "read" or name == "initialize" else empty,
                "write": active if direction == "write" else empty,
            }
            print(json.dumps({"jobs": [row]}))
        """)

    def fio_environment(self, root, *, verify_errno=None):
        commands = root / "bin"
        commands.mkdir()
        executable(commands / "findmnt", """\
            #!/usr/bin/env python3
            import json
            import os
            print(json.dumps({"filesystems": [{
                "source": "/dev/fixture",
                "fstype": "ext4",
                "target": os.environ["SCRATCH_ROOT"],
            }]}))
        """)
        fio = commands / "fio"
        self.fio_fixture(fio)
        scratch = root / "scratch"
        results = root / "results"
        scratch.mkdir()
        environment = {
            "PATH": str(commands) + os.pathsep + os.environ["PATH"],
            "PROJECT_SIGNAL_RESULTS_DIR": str(results),
            "SCRATCH_ROOT": str(scratch),
            "EXPECTED_MOUNT_SOURCE": "/dev/fixture",
            "EXPECTED_FILESYSTEM_TYPE": "ext4",
            "FIO_BIN": str(fio),
            "DURATION_SECONDS": "1",
            "COMMAND_TIMEOUT_SECONDS": "5",
            "FIO_BYTES": "4096",
            "FIO_BLOCK_BYTES": "4096",
            "FIO_JOBS": "1",
            "FIO_IODEPTH": "1",
            "FIO_IOENGINE": "sync",
            "FIO_DIRECT": "0",
            "FIO_RANDOM_SEED": "1",
            "FIO_LIMITS_JSON": "{}",
            "FIO_CALLS": str(root / "fio-calls.jsonl"),
        }
        if verify_errno is not None:
            environment["FAKE_FIO_VERIFY_ERRNO"] = str(verify_errno)
        return environment, results

    def test_fio_time_based_writes_receive_full_crc_verification(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            environment, results = self.fio_environment(root)
            run = self.run_script("fio-storage.sh", environment)
            self.assertEqual(run.returncode, 0, run.stderr)
            self.assertEqual((results / "verdict").read_text(), "Pass")
            self.assertEqual((results / "reason").read_text(), "StorageCasesChecked")
            calls = [json.loads(line) for line in pathlib.Path(environment["FIO_CALLS"]).read_text().splitlines()]
            write_cases = [call for call in calls if "--time_based=1" in call and any(
                argument in ("--rw=write", "--rw=randwrite") for argument in call
            )]
            self.assertEqual(len(write_cases), 2)
            self.assertTrue(all("--do_verify=0" in call for call in write_cases))
            verify_cases = [call for call in calls if "--verify_only=1" in call]
            self.assertEqual(len(verify_cases), 2)
            self.assertTrue(all("--rw=read" in call for call in verify_cases))

    def test_fio_post_write_integrity_failure_remains_typed_fail(self):
        with tempfile.TemporaryDirectory() as directory:
            environment, results = self.fio_environment(
                pathlib.Path(directory), verify_errno=errno.EILSEQ,
            )
            run = self.run_script("fio-storage.sh", environment)
            self.assertEqual(run.returncode, 0, run.stderr)
            self.assertEqual((results / "verdict").read_text(), "Fail")
            self.assertEqual((results / "reason").read_text(), "StorageIOOrIntegrityFailure")


if __name__ == "__main__":
    unittest.main()
