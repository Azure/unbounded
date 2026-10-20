import contextlib
import grp
import hashlib
import importlib.util
import io
import json
import pathlib
import pwd
import shutil
import subprocess
import tarfile
import tempfile
import unittest
from unittest import mock


ROOT = pathlib.Path(__file__).resolve().parents[1]


def load(name, path):
    specification = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(specification)
    specification.loader.exec_module(module)
    return module


builder = load("certification_builder", ROOT / "build.py")
fetcher = load("certification_fetcher", ROOT / "fetch-sources.py")
validator = load("certification_validator", ROOT / "validate-image.py")


class InputTests(unittest.TestCase):
    def test_committed_fifteen_script_contract(self):
        _, report = builder.check_inputs()
        self.assertEqual(report["script_commit"], "483c4704dc4bc265267a3b0037b2df3f833964e0")
        self.assertEqual(len(report["context_sha256"]), 64)

    def test_mutations_fail_closed(self):
        changes = (
            ("Containerfile", lambda text: text.replace("@sha256:", "@sha256:0", 1)),
            ("requirements.lock", lambda text: text.replace("--hash=sha256:", "--hash=sha512:", 1)),
            ("requirements.lock", lambda text: text.replace("https://", "http://", 1)),
            ("ubuntu.sources", lambda text: text.replace("20261015T000000Z", "20261016T000000Z")),
        )
        for filename, change in changes:
            with self.subTest(filename=filename), tempfile.TemporaryDirectory() as directory:
                root = pathlib.Path(directory) / ROOT.name
                shutil.copytree(ROOT, root)
                scripts = root.parent / "project-signal-certification/scripts"
                shutil.copytree(ROOT.parent / "project-signal-certification/scripts", scripts)
                path = root / filename
                path.write_text(change(path.read_text()))
                with self.assertRaises(ValueError):
                    builder.check_inputs(root)

    def test_frozen_script_mutation_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / ROOT.name
            shutil.copytree(ROOT, root)
            scripts = root.parent / "project-signal-certification/scripts"
            shutil.copytree(ROOT.parent / "project-signal-certification/scripts", scripts)
            with (scripts / "gpu-health-monitor.sh").open("ab") as output:
                output.write(b"\n")
            with self.assertRaisesRegex(ValueError, "Frozen script changed"):
                builder.check_inputs(root)

    def test_fixed_identity_does_not_change_default_user_or_mpi_shell(self):
        recipe = (ROOT / "Containerfile").read_text()
        self.assertIn("groupadd --gid 65532 certification", recipe)
        self.assertIn("useradd --uid 65532 --gid 65532 --no-log-init --no-create-home", recipe)
        self.assertIn("--home-dir /home/certification --shell /bin/bash certification", recipe)
        self.assertIn("install -d -o 65532 -g 65532 -m 0755 /home/certification", recipe)
        self.assertNotRegex(recipe, r"(?m)^USER ")
        self.assertIn("WORKDIR /opt/gpu-burn\n", recipe)
        self.assertIn("ENTRYPOINT []\n", recipe)

    def test_apt_retries_only_the_locked_snapshot(self):
        recipe = (ROOT / "Containerfile").read_text()
        self.assertEqual(recipe.count("Acquire::Retries=10"), 2)
        self.assertIn("Acquire::https::Timeout=60", recipe)
        self.assertNotIn("--fix-missing", recipe)

    def test_build_checks_real_nonroot_ssh_without_adding_privileges(self):
        recipe = (ROOT / "Containerfile").read_text()
        self.assertIn("setpriv --reuid=65532 --regid=65532 --clear-groups", recipe)
        self.assertIn("--bounding-set=-all --no-new-privs", recipe)
        self.assertIn('["runtime_identity_check"]()', recipe)

    def test_probe_only_dependencies_are_not_required(self):
        contract = json.loads((ROOT / "script-contract.json").read_text())
        lock = json.loads((ROOT / "dependencies.lock.json").read_text())
        self.assertTrue({"gcc", "cc", "nvcc"} <= set(contract["path_tools"]))
        self.assertTrue({"clang", "file"}.isdisjoint(contract["path_tools"]))
        self.assertNotIn("numpy", lock["python_distributions"])


class NativeHostTests(unittest.TestCase):
    def test_native_docker_host(self):
        response = subprocess.CompletedProcess([], 0, '{"OSType":"linux","Architecture":"x86_64"}')
        with mock.patch.object(builder.subprocess, "run", return_value=response):
            self.assertEqual(builder.native_host("docker")["architecture"], "x86_64")

    def test_native_podman_host(self):
        response = subprocess.CompletedProcess([], 0, '{"host":{"os":"linux","arch":"amd64"}}')
        with mock.patch.object(builder.subprocess, "run", return_value=response):
            self.assertEqual(builder.native_host("podman")["architecture"], "amd64")

    def test_arm_and_non_linux_are_rejected(self):
        for operating_system, architecture in (("linux", "aarch64"), ("linux", "arm64"), ("darwin", "amd64")):
            response = subprocess.CompletedProcess(
                [], 0, json.dumps({"OSType": operating_system, "Architecture": architecture}),
            )
            with self.subTest(architecture=architecture), mock.patch.object(
                builder.subprocess, "run", return_value=response,
            ), self.assertRaisesRegex(ValueError, "Native linux/amd64 builder required"):
                builder.native_host("docker")

    def test_arm_preflight_never_builds_and_reports_failure(self):
        response = subprocess.CompletedProcess([], 0, '{"OSType":"linux","Architecture":"arm64"}')
        with tempfile.TemporaryDirectory() as directory, mock.patch.object(
            builder.subprocess, "run", return_value=response,
        ) as run, contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            evidence = pathlib.Path(directory) / "evidence"
            self.assertEqual(builder.build("docker", evidence), 2)
            report = json.loads((evidence / "result.json").read_text())
            self.assertEqual(report["stage"], "native-host")
            self.assertEqual(report["status"], "blocked")
            self.assertIsNone(report["image_id"])
            self.assertFalse(report["registry_publication"])
            self.assertEqual(run.call_count, 1)
            self.assertEqual(run.call_args.args[0][1], "info")

    def test_unreachable_engine_is_not_a_success(self):
        failure = subprocess.CalledProcessError(1, ["docker", "info"], stderr="Connection refused")
        with tempfile.TemporaryDirectory() as directory, mock.patch.object(
            builder.subprocess, "run", side_effect=failure,
        ), contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            evidence = pathlib.Path(directory) / "evidence"
            self.assertEqual(builder.build("docker", evidence), 2)
            report = json.loads((evidence / "result.json").read_text())
            self.assertEqual(report["error"], "Connection refused")
            self.assertIsNone(report["registry_digest"])

    def test_identity_failure_cannot_pass_after_software_validation(self):
        def run(arguments, **_):
            if arguments[1] == "info":
                return subprocess.CompletedProcess([], 0, '{"OSType":"linux","Architecture":"amd64"}')
            if arguments[1] == "build":
                pathlib.Path(arguments[arguments.index("--iidfile") + 1]).write_text("sha256:" + "a" * 64)
            if "--user=65532:65532" in arguments:
                raise subprocess.CalledProcessError(
                    255, arguments, stderr="No user exists for uid 65532",
                )
            return subprocess.CompletedProcess(arguments, 0)
        with tempfile.TemporaryDirectory() as directory, mock.patch.object(
            builder.subprocess, "run", side_effect=run,
        ), contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            evidence = pathlib.Path(directory) / "evidence"
            self.assertEqual(builder.build("docker", evidence), 2)
            report = json.loads((evidence / "result.json").read_text())
            self.assertEqual(report["stage"], "offline-identity-check")
            self.assertEqual(report["status"], "blocked")
            self.assertEqual(report["error"], "No user exists for uid 65532")

    def test_preflight_timeout_retains_bounded_failure(self):
        failure = subprocess.TimeoutExpired(["docker", "info"], 30, stderr=b"connection timeout")
        with tempfile.TemporaryDirectory() as directory, mock.patch.object(
            builder.subprocess, "run", side_effect=failure,
        ), contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            evidence = pathlib.Path(directory) / "evidence"
            self.assertEqual(builder.build("docker", evidence), 2)
            report = json.loads((evidence / "result.json").read_text())
            self.assertEqual(report["error"], "connection timeout")

    def test_build_success_requires_isolated_image_validation(self):
        def run(arguments, **_):
            if arguments[1] == "info":
                return subprocess.CompletedProcess([], 0, '{"OSType":"linux","Architecture":"amd64"}')
            if arguments[1] == "build":
                image_id = pathlib.Path(arguments[arguments.index("--iidfile") + 1])
                image_id.write_text("sha256:" + "a" * 64 + "\n")
            return subprocess.CompletedProcess(arguments, 0)
        with tempfile.TemporaryDirectory() as directory, mock.patch.object(
            builder.subprocess, "run", side_effect=run,
        ) as executed, contextlib.redirect_stdout(io.StringIO()):
            evidence = pathlib.Path(directory) / "evidence"
            self.assertEqual(builder.build("docker", evidence), 0)
            report = json.loads((evidence / "result.json").read_text())
            self.assertEqual(report["status"], "built-offline-validated")
            validation = report["validation_argv"]
            self.assertIn("--network=none", validation)
            self.assertIn("--pull=never", validation)
            self.assertIn("--cap-drop=ALL", validation)
            self.assertIn("--read-only", validation)
            self.assertNotIn("--gpus", validation)
            identity = report["identity_validation_argv"]
            for flag in ("--user=65532:65532", "--network=none", "--pull=never",
                         "--cap-drop=ALL", "--security-opt=no-new-privileges", "--read-only"):
                self.assertIn(flag, identity)
            self.assertIn('["runtime_identity_check"]()', identity[-1])
            self.assertFalse(any("push" in call.args[0] for call in executed.call_args_list))
            self.assertIsNone(report["registry_digest"])


class SourceFetchTests(unittest.TestCase):
    def archive(self, name="source/LICENSE"):
        output = io.BytesIO()
        with tarfile.open(fileobj=output, mode="w:gz") as archive:
            data = b"controlled source fixture\n"
            header = tarfile.TarInfo(name)
            header.size = len(data)
            archive.addfile(header, io.BytesIO(data))
        return output.getvalue()

    def run_fetch(self, directory, archive, *, checksum=None, license_name="LICENSE"):
        lock = {"sources": [{
            "name": "controlled-source", "commit": "a" * 40,
            "url": "https://codeload.github.com/example/source/tar.gz/" + "a" * 40,
            "sha256": checksum or hashlib.sha256(archive).hexdigest(), "licenses": [license_name],
        }]}
        lock_path = directory / "lock.json"
        lock_path.write_text(json.dumps(lock))
        with mock.patch.object(fetcher.urllib.request, "urlopen", return_value=io.BytesIO(archive)), mock.patch.object(
            fetcher, "LICENSES", directory / "licenses",
        ), contextlib.redirect_stdout(io.StringIO()):
            fetcher.fetch(lock_path, directory / "sources")

    def test_verified_source_and_license_are_retained(self):
        with tempfile.TemporaryDirectory() as directory:
            directory = pathlib.Path(directory)
            self.run_fetch(directory, self.archive())
            self.assertTrue((directory / "sources/controlled-source/LICENSE").is_file())
            self.assertEqual((directory / "licenses/controlled-source/LICENSE").read_bytes(),
                             b"controlled source fixture\n")

    def test_bad_checksum_precedes_extraction(self):
        with tempfile.TemporaryDirectory() as directory, self.assertRaisesRegex(ValueError, "checksum mismatch"):
            self.run_fetch(pathlib.Path(directory), self.archive(), checksum="0" * 64)

    def test_archive_path_escape_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory, self.assertRaises(tarfile.FilterError):
            self.run_fetch(pathlib.Path(directory), self.archive("../escaped"))

    def test_license_path_escape_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory, self.assertRaisesRegex(ValueError, "license path"):
            self.run_fetch(pathlib.Path(directory), self.archive(), license_name="../outside")

    def test_real_git_objects_are_joined_without_creating_a_commit(self):
        component = {
            "name": "nvbandwidth", "commit": "a" * 40,
            "url": "https://codeload.github.com/NVIDIA/nvbandwidth/tar.gz/" + "a" * 40,
            "git_metadata": "https://github.com/NVIDIA/nvbandwidth.git",
        }
        response = subprocess.CompletedProcess([], 0, "a" * 40 + "\n")
        with tempfile.TemporaryDirectory() as directory, mock.patch.object(
            fetcher.subprocess, "run", return_value=response,
        ) as run, mock.patch.dict(fetcher.os.environ, {"GIT_ASKPASS": "/untrusted/helper"}):
            fetcher.git_metadata(pathlib.Path(directory), component)
            arguments = [call.args[0][3] for call in run.call_args_list]
            self.assertEqual(arguments, ["init", "fetch", "reset", "diff", "rev-parse"])
            self.assertNotIn("GIT_ASKPASS", run.call_args.kwargs["env"])
            self.assertEqual(run.call_args.kwargs["env"]["GIT_CONFIG_GLOBAL"], "/dev/null")

    def test_unrelated_git_metadata_source_is_rejected(self):
        component = {
            "name": "nvbandwidth", "commit": "a" * 40,
            "url": "https://codeload.github.com/NVIDIA/nvbandwidth/tar.gz/" + "a" * 40,
            "git_metadata": "https://github.com/unrelated/source.git",
        }
        with tempfile.TemporaryDirectory() as directory, self.assertRaisesRegex(ValueError, "Git metadata source"):
            fetcher.git_metadata(pathlib.Path(directory), component)


class RuntimeIdentityTests(unittest.TestCase):
    @contextlib.contextmanager
    def identity(self, *, account=None, group=None):
        account = account or pwd.struct_passwd((
            "certification", "x", 65532, 65532, "", "/home/certification", "/bin/bash",
        ))
        group = group or grp.struct_group(("certification", "x", 65532, []))
        with contextlib.ExitStack() as stack:
            for function in ("getuid", "geteuid", "getgid", "getegid"):
                stack.enter_context(mock.patch.object(validator.os, function, return_value=65532))
            stack.enter_context(mock.patch.object(validator.pwd, "getpwuid", return_value=account))
            stack.enter_context(mock.patch.object(validator.grp, "getgrgid", return_value=group))
            stack.enter_context(mock.patch.object(validator.pathlib.Path, "is_dir", return_value=True))
            stack.enter_context(mock.patch.object(validator.os, "access", return_value=True))
            yield

    def test_named_nonroot_identity_and_ssh_version_are_reported(self):
        with self.identity(), mock.patch.object(
            validator, "command", return_value="OpenSSH_9.6p1 Ubuntu-3ubuntu13.13",
        ) as run, contextlib.redirect_stdout(io.StringIO()) as output:
            report = validator.runtime_identity_check()
        self.assertEqual(report["uid"], 65532)
        self.assertEqual(report["gid"], 65532)
        self.assertEqual(report["user"], "certification")
        self.assertEqual(report["group"], "certification")
        self.assertEqual(report["home"], "/home/certification")
        self.assertEqual(report["shell"], "/bin/bash")
        self.assertEqual(json.loads(output.getvalue())["nonroot_identity"], report)
        run.assert_called_once_with(["ssh", "-V"])

    def test_missing_passwd_identity_reproduces_dapple_failure(self):
        with self.identity(), mock.patch.object(
            validator.pwd, "getpwuid", side_effect=KeyError(65532),
        ), mock.patch.object(validator, "command") as run:
            with self.assertRaisesRegex(RuntimeError, "passwd identity.*65532"):
                validator.runtime_identity_check()
            run.assert_not_called()

    def test_missing_group_identity_is_rejected(self):
        with self.identity(), mock.patch.object(
            validator.grp, "getgrgid", side_effect=KeyError(65532),
        ), self.assertRaisesRegex(RuntimeError, "group identity.*65532"):
            validator.runtime_identity_check()

    def test_root_execution_cannot_substitute_for_nonroot_validation(self):
        with self.identity(), mock.patch.object(
            validator.os, "geteuid", return_value=0,
        ), mock.patch.object(validator, "command") as run:
            with self.assertRaisesRegex(RuntimeError, "UID/GID 65532"):
                validator.runtime_identity_check()
            run.assert_not_called()

    def test_mismatched_primary_group_or_nologin_shell_is_rejected(self):
        for gid, shell in ((0, "/bin/bash"), (65532, "/usr/sbin/nologin")):
            account = pwd.struct_passwd((
                "certification", "x", 65532, gid, "", "/home/certification", shell,
            ))
            with self.subTest(gid=gid, shell=shell), self.identity(
                account=account,
            ), self.assertRaisesRegex(RuntimeError, "primary group.*Bash shell"):
                validator.runtime_identity_check()

    def test_home_must_be_an_accessible_absolute_directory(self):
        with self.identity(), mock.patch.object(
            validator.os, "access", return_value=False,
        ), self.assertRaisesRegex(RuntimeError, "home directory"):
            validator.runtime_identity_check()

    def test_original_ssh_exit_255_retains_uid_error(self):
        error = subprocess.CalledProcessError(255, ["ssh", "-V"], output="No user exists for uid 65532\n")
        with self.identity(), mock.patch.object(
            validator, "command", side_effect=error,
        ), self.assertRaisesRegex(RuntimeError, "No user exists for uid 65532"):
            validator.runtime_identity_check()

    def test_unexpected_ssh_banner_is_rejected(self):
        with self.identity(), mock.patch.object(
            validator, "command", return_value="unexpected version",
        ), self.assertRaisesRegex(RuntimeError, "OpenSSH"):
            validator.runtime_identity_check()

    def test_ssh_timeout_retains_partial_output(self):
        error = subprocess.TimeoutExpired(["ssh", "-V"], 60, output=b"partial SSH output")
        with self.identity(), mock.patch.object(
            validator, "command", side_effect=error,
        ), self.assertRaisesRegex(RuntimeError, "partial SSH output"):
            validator.runtime_identity_check()


class MPILauncherTests(unittest.TestCase):
    """Source-derived Ubuntu 4.1.6-7ubuntu2 fixtures, not captured native output.

    openmpi_4.1.6.orig.tar.xz, orte/orted/orted_submit.c:275-295 and VERSION:62-64.
    Ubuntu's active patches leave that version-printing branch unchanged.
    """

    def test_pinned_openmpi_and_resolved_openrte_banners(self):
        for name, vendor in (("mpirun", "Open MPI"), ("orterun", "OpenRTE"),
                             ("mpirun.openmpi", "OpenRTE")):
            executable = "/usr/bin/" + name
            stdout = f"{name} ({vendor}) 4.1.6\n\nReport bugs to http://www.open-mpi.org/community/help/\n"
            stderr = "controlled stderr diagnostic\n"
            response = subprocess.CompletedProcess([], 0, stdout, stderr)
            output = io.StringIO()
            with self.subTest(name=name), mock.patch.object(
                validator.subprocess, "run", return_value=response,
            ) as run, contextlib.redirect_stdout(output):
                evidence = validator.mpi_check(executable)
                self.assertEqual(evidence, {
                    "executable": executable, "returncode": 0, "stdout": stdout, "stderr": stderr,
                })
                self.assertEqual(json.loads(output.getvalue())["mpi_launcher_version"], evidence)
                run.assert_called_once_with(
                    [executable, "--version"], check=False, capture_output=True, text=True, timeout=60,
                )

    def test_non_openmpi_and_malformed_headers_fail_with_output(self):
        for stdout in (
            "", "\n", "HYDRA build details:\nVersion: 4.1.6\n",
            "mpirun (MPICH) 4.1.6\n", "Intel(R) MPI Library 2021.16\n",
            "Open MPI\n", "unsupported launcher mentioning Open MPI\n",
            "mpirun (Open MPI) unknown\n", "mpirun (OpenRTE) 4.1\n",
            "other (Open MPI) 4.1.6\n", "mpirun (Open MPI) 4.1.6 unexpected\n",
            "not OpenMPI\nmpirun (Open MPI) 4.1.6\n",
        ):
            stderr = "controlled failure detail\n"
            response = subprocess.CompletedProcess([], 0, stdout, stderr)
            output = io.StringIO()
            with self.subTest(stdout=stdout), mock.patch.object(
                validator.subprocess, "run", return_value=response,
            ), contextlib.redirect_stdout(output), self.assertRaisesRegex(RuntimeError, "valid OpenMPI/OpenRTE"):
                validator.mpi_check("/usr/bin/mpirun")
            evidence = json.loads(output.getvalue())["mpi_launcher_version"]
            self.assertEqual(evidence["stdout"], stdout)
            self.assertEqual(evidence["stderr"], stderr)

    def test_nonzero_exit_cannot_pass_with_a_valid_banner(self):
        response = subprocess.CompletedProcess(
            [], 1, "orterun (OpenRTE) 4.1.6\n", "launcher failure\n",
        )
        output = io.StringIO()
        with mock.patch.object(
            validator.subprocess, "run", return_value=response,
        ), contextlib.redirect_stdout(output), self.assertRaisesRegex(RuntimeError, "valid OpenMPI/OpenRTE"):
            validator.mpi_check("/usr/bin/orterun")
        evidence = json.loads(output.getvalue())["mpi_launcher_version"]
        self.assertEqual(evidence["returncode"], 1)
        self.assertEqual(evidence["stdout"], response.stdout)
        self.assertEqual(evidence["stderr"], response.stderr)

    def test_stderr_banner_does_not_replace_empty_stdout(self):
        response = subprocess.CompletedProcess([], 0, "", "mpirun (Open MPI) 4.1.6\n")
        with mock.patch.object(
            validator.subprocess, "run", return_value=response,
        ), contextlib.redirect_stdout(io.StringIO()), self.assertRaisesRegex(RuntimeError, "valid OpenMPI/OpenRTE"):
            validator.mpi_check("/usr/bin/mpirun")

    def test_timeout_retains_partial_stdout_and_stderr(self):
        error = subprocess.TimeoutExpired(
            ["/usr/bin/orterun", "--version"], 60, output=b"partial banner", stderr=b"timeout detail",
        )
        with mock.patch.object(validator.subprocess, "run", side_effect=error):
            with self.assertRaisesRegex(RuntimeError, "timed out") as failure:
                validator.mpi_check("/usr/bin/orterun")
        self.assertIn("stdout=b'partial banner'", str(failure.exception))
        self.assertIn("stderr=b'timeout detail'", str(failure.exception))


class LoaderTests(unittest.TestCase):
    def test_only_driver_libraries_may_be_absent(self):
        output = "libcuda.so.1 => not found\nlibcudart.so.12 => /opt/cuda/libcudart.so.12 (0x123)"
        with mock.patch.object(validator, "command", return_value=output) as run:
            report = validator.loader_check("/opt/bin/nvbandwidth")
            self.assertEqual(report["driver_injected_libraries_absent"], ["libcuda.so.1"])
            self.assertNotIn("LD_LIBRARY_PATH", run.call_args.kwargs["environment"])
            self.assertNotIn("TOOL_LIBRARY_PATH", run.call_args.kwargs["environment"])

    def test_missing_cuda_runtime_is_not_accepted(self):
        with mock.patch.object(validator, "command", return_value="libcudart.so.12 => not found"):
            with self.assertRaisesRegex(RuntimeError, "cannot resolve"):
                validator.loader_check("/opt/bin/nvbandwidth")

    def test_driver_stub_is_not_a_runtime(self):
        with mock.patch.object(validator, "command", return_value="libcuda.so.1 => /usr/local/cuda/lib64/stubs/libcuda.so.1"):
            with self.assertRaisesRegex(RuntimeError, "stubs"):
                validator.loader_check("/opt/bin/nvbandwidth")

    def test_unresolved_driver_stub_rpath_is_also_rejected(self):
        output = "0x1 (RUNPATH) Library runpath: [/usr/local/cuda/lib64/stubs:/other/lib]"
        with mock.patch.object(validator, "command", return_value=output) as run:
            with self.assertRaisesRegex(RuntimeError, "runtime paths"):
                validator.loader_check("/opt/bin/nvbandwidth")
            self.assertEqual(run.call_count, 1)

    def test_base_image_nccl_cannot_shadow_wheel_runtime(self):
        with mock.patch.object(validator, "command", return_value="libnccl.so.2 => /usr/lib/libnccl.so.2 (0x123)"):
            with self.assertRaisesRegex(RuntimeError, "exact wheel-provided"):
                validator.loader_check("/opt/bin/all_reduce_perf")

    def test_exact_nccl_path_is_accepted(self):
        output = "libnccl.so.2 => /opt/venv/lib/python3.12/site-packages/nvidia/nccl/lib/libnccl.so.2 (0x123)"
        with mock.patch.object(validator, "command", return_value=output):
            self.assertEqual(validator.loader_check("/opt/bin/all_reduce_perf")["driver_injected_libraries_absent"], [])


if __name__ == "__main__":
    unittest.main()
