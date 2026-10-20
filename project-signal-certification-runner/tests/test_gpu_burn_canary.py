import os
import pathlib
import shutil
import subprocess
import tempfile
import threading
import time
import unittest


SCRIPT = (
    pathlib.Path(__file__).resolve().parents[2]
    / "project-signal-certification/scripts/gpu-burn-canary.sh"
)


class GPUBurnCanaryTests(unittest.TestCase):
    def setUp(self):
        self.temporary_directory = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.temporary_directory.name)
        self.work = self.root / "work"
        self.results = self.root / "results"
        self.bin = self.root / "bin"
        self.trace = self.root / "trace"
        for path in (self.work, self.results, self.bin, self.trace):
            path.mkdir()

        source = SCRIPT.read_text()
        localized = (
            source.replace("/work", str(self.work))
            .replace("/results", str(self.results))
            .replace("/dev/termination-log", str(self.root / "termination.log"))
        )
        self.script = self.root / SCRIPT.name
        self.script.write_text(localized)
        self.script.chmod(0o755)

        self.write_tool(
            "nvidia-smi",
            r"""#!/usr/bin/env bash
if [[ "$*" == *"index,name,temperature.gpu"* ]]; then
  for ((index = 0; index < EXPECTED_GPU_COUNT; index++)); do
    printf '%s, NVIDIA B200, 30, 50, 100, 1000, 0, 0, 0x0\n' "${index}"
  done
else
  for ((index = 0; index < EXPECTED_GPU_COUNT; index++)); do
    printf '%s, GPU-%s, 0000:%02x:00.0, NVIDIA B200\n' "${index}" "${index}" "$((index + 1))"
  done
fi
""",
        )
        self.write_tool("dmesg", "#!/usr/bin/env bash\nexit 0\n")
        self.write_tool(
            "gpu_burn",
            r"""#!/usr/bin/env bash
printf '%b' "${GPU_BURN_FIXTURE}"
""",
        )
        self.write_tool(
            "dd",
            r"""#!/usr/bin/env bash
if mkdir "${TRACE_DIR}/dd-${PPID}" 2>/dev/null; then
  printf '%s\n' "${PPID}" >> "${TRACE_DIR}/worker-pids"
fi
output=
for argument in "$@"; do
  [[ "${argument}" == of=* ]] && output="${argument#of=}"
done
if [[ -n "${output}" ]]; then
  printf fixture > "${output}"
else
  printf fixture
fi
""",
        )

    def tearDown(self):
        self.temporary_directory.cleanup()

    def write_tool(self, name, body):
        path = self.bin / name
        path.write_text(body)
        path.chmod(0o755)

    def environment(self, fixture, gpu_count):
        return os.environ | {
            "PATH": str(self.bin) + os.pathsep + os.environ["PATH"],
            "TRACE_DIR": str(self.trace),
            "GPU_BURN_FIXTURE": fixture,
            "EXPECTED_GPU_COUNT": str(gpu_count),
            "OBSERVED_GPU_CAPACITY": str(gpu_count),
            "OBSERVED_GPU_ALLOCATABLE": str(gpu_count),
            "EXPECTED_GPU_PRODUCT_REGEX": "NVIDIA B200",
            "BURN_DURATION_SECONDS": "1",
            "SAMPLE_INTERVAL_SECONDS": "1",
            "POWER_LIMIT_TOLERANCE_PERCENT": "0",
            "POWER_LIMIT_CONSECUTIVE_SAMPLES": "1",
            "CPU_WORKERS": "1",
            "MEMORY_MIB": "1",
            "STORAGE_MIB": "1",
        }

    def component_results_after_stop(self):
        while not (self.work / "stop").exists():
            time.sleep(0.01)
        for name in ("cpu-stress", "memory-integrity", "storage-integrity"):
            (self.results / f"{name}-reason").write_text("ComponentCompleted")
            (self.results / f"{name}-verdict").write_text("Pass")

    def run_canary(self, fixture, gpu_count=2, component="standalone"):
        helper = None
        if component == "gpu-burn":
            for name in ("cpu-stress", "memory-integrity", "storage-integrity"):
                (self.work / f"{name}.started").touch()
            helper = threading.Thread(target=self.component_results_after_stop)
            helper.start()

        completed = subprocess.run(
            ["/bin/bash", str(self.script), component],
            env=self.environment(fixture, gpu_count),
            text=True,
            capture_output=True,
            timeout=15,
        )
        if helper is not None:
            helper.join(timeout=5)
            self.assertFalse(helper.is_alive())
        return completed

    def assert_result(self, verdict, reason):
        self.assertEqual((self.results / "verdict").read_text(), verdict)
        self.assertEqual((self.results / "reason").read_text(), reason)
        self.assertEqual(
            (self.root / "termination.log").read_text(),
            f"verdict={verdict} reason={reason}\n",
        )
        self.assertFalse((self.results / "verdict.tmp").exists())

        wrapper = self.root / "wrapper"
        wrapper.mkdir()
        shutil.copy2(self.results / "verdict", wrapper / "verdict")
        shutil.copy2(self.results / "reason", wrapper / "reason")
        self.assertEqual((wrapper / "verdict").read_text(), verdict)
        self.assertEqual((wrapper / "reason").read_text(), reason)

    def assert_workers_stopped(self):
        self.assertTrue((self.work / "stop").exists())
        path = self.trace / "worker-pids"
        if not path.exists():
            return
        for value in set(path.read_text().splitlines()):
            with self.subTest(pid=value):
                with self.assertRaises(ProcessLookupError):
                    os.kill(int(value), 0)

    def test_standalone_rejects_partial_gpu_computation_despite_ok_footer(self):
        completed = self.run_canary(
            "100.0%  proc'd: 12 (200 Gflop/s) - 0 (0 Gflop/s)"
            "   errors: 0 - 0\nTested 2 GPUs:\n"
            "        GPU 0: OK\n        GPU 1: OK\n"
        )
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assert_result("Fail", "GPUBurnMeasurementsMissing")
        self.assert_workers_stopped()

    def test_standalone_rejects_zero_gpu_computation_despite_ok_footer(self):
        completed = self.run_canary(
            "100.0%  proc'd: 0 (0 Gflop/s) - 0 (0 Gflop/s)"
            "   errors: 0 - 0\nTested 2 GPUs:\n"
            "        GPU 0: OK\n        GPU 1: OK\n"
        )
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assert_result("Fail", "GPUBurnMeasurementsMissing")
        self.assert_workers_stopped()

    def test_standalone_rejects_warning_output(self):
        completed = self.run_canary(
            "100.0%  proc'd: 12 (200 Gflop/s) - 13 (210 Gflop/s)"
            "   errors: 0 - 0\nWARNING! compare mismatch\n"
        )
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assert_result("Fail", "GPUBurnComputationFailed")
        self.assert_workers_stopped()

    def test_standalone_rejects_dead_worker_output(self):
        completed = self.run_canary(
            "100.0%  proc'd: 12 (200 Gflop/s) - 13 (210 Gflop/s)"
            "   errors: 0 - 0\nDIED! worker 1\n"
        )
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assert_result("Fail", "GPUBurnComputationFailed")
        self.assert_workers_stopped()

    def test_standalone_passes_and_publishes_complete_all_gpu_result(self):
        completed = self.run_canary(
            "100.0%  proc'd: 12 (200 Gflop/s) - 13 (210 Gflop/s)"
            "   errors: 0 - 0\nTested 2 GPUs:\n"
            "        GPU 0: OK\n        GPU 1: OK\n"
        )
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assert_result("Pass", "BurnCanaryPassed")
        self.assert_workers_stopped()
        self.assertIn("12 (200 Gflop/s)", (self.results / "measurements.txt").read_text())

    def test_component_mode_preserves_quantitative_and_group_results(self):
        completed = self.run_canary(
            "100.0%  proc'd: 12 (200 Gflop/s) - 13 (210 Gflop/s)"
            "   errors: 0 - 0\n",
            component="gpu-burn",
        )
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assert_result("Pass", "BurnCanaryPassed")
        self.assertEqual((self.results / "gpu-burn-verdict").read_text(), "Pass")
        self.assertEqual((self.results / "gpu-burn-reason").read_text(), "BurnCanaryPassed")
        self.assertTrue((self.work / "burn-complete").exists())
        self.assert_workers_stopped()


if __name__ == "__main__":
    unittest.main()
