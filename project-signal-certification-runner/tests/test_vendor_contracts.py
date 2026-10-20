"""CPU-only vendor-contract and retained certification-output fixtures.

NVBandwidth: NVIDIA/nvbandwidth@82fc4e8c6afa0babb8687793678f615b3b8d793e,
testcases.cpp:1042-1084 and json_output.cpp:74-95.
NCCL: NVIDIA/nccl@72d2432094d6ae36abd6e511c3a16a2d052dbf94,
src/init.cc:360-365,1467-1470; src/transport/{p2p.cc:384-387,net.cc:217-225}.
Legacy channel format: NVIDIA/nccl@d97a32fac84f69cbd022ffdc57d5687c760742c7.
Retained outputs: Project Signal certification rows 07, 08, 11, and 12.
"""

import ast
import gzip
import hashlib
import json
import math
import pathlib
import re
import types
import unittest


SCRIPTS = pathlib.Path(__file__).resolve().parents[2] / "project-signal-certification/scripts"
FIXTURES = pathlib.Path(__file__).resolve().parent / "fixtures/certification-remediation"


def fixture_bytes(name):
    payload = (FIXTURES / name).read_bytes()
    return gzip.decompress(payload) if name.endswith(".gz") else payload


class Rejected(Exception):
    pass


def reject(reason):
    raise Rejected(reason)


def functions(script, names, **values):
    text = (SCRIPTS / script).read_text()
    payload = re.search(r"<<'PY'\n(.*)\nPY(?:\n|$)", text, re.DOTALL)
    if payload is None:
        raise AssertionError("Embedded Python source boundary missing")
    tree = ast.parse(payload[1])
    selected = []
    for node in tree.body:
        name = node.name if isinstance(node, ast.FunctionDef) else None
        if isinstance(node, ast.Assign) and len(node.targets) == 1 and isinstance(node.targets[0], ast.Name):
            name = node.targets[0].id
        if name in names:
            selected.append(node)
    if len(selected) != len(names):
        raise AssertionError("Requested actual parser definitions are missing")
    namespace = {"re": re, "math": math, "json": json, "inconclusive": reject, **values}
    exec(compile(ast.Module(body=selected, type_ignores=[]), script, "exec"), namespace)
    return namespace


def worker_source(script):
    text = (SCRIPTS / script).read_text()
    payload = re.search(r"<<'PY'[^\n]*\n(.*)\nPY(?:\n|$)", text, re.DOTALL)
    if payload is None:
        raise AssertionError("Embedded Python source boundary missing")
    for node in ast.parse(payload[1]).body:
        if (isinstance(node, ast.Assign) and len(node.targets) == 1
                and isinstance(node.targets[0], ast.Name) and node.targets[0].id == "WORKER"):
            return ast.literal_eval(node.value)
    raise AssertionError("Embedded worker source missing")


def worker_functions(script, names, **values):
    tree = ast.parse(worker_source(script))
    selected = []
    for node in tree.body:
        name = node.name if isinstance(node, (ast.FunctionDef, ast.ClassDef)) else None
        if isinstance(node, ast.Assign) and len(node.targets) == 1 and isinstance(node.targets[0], ast.Name):
            name = node.targets[0].id
        if name in names:
            selected.append(node)
    if len(selected) != len(names):
        raise AssertionError("Requested actual worker definitions are missing")
    namespace = {**values}
    exec(compile(ast.Module(body=selected, type_ignores=[]), script + ":WORKER", "exec"), namespace)
    return namespace


def channel_messages(*, modern, backend=False, version=None):
    selected = [{"index": 3, "packed_bus": 0x170000}, {"index": 7, "packed_bus": 0x6C000}]
    version = version or ("2.27.3" if modern else "2.18.1")
    messages = ["NCCL version " + version, "1 coll channels", "Using network Socket"]
    for rank, gpu in enumerate(selected):
        nvml = f" nvmlDev {gpu['index']}" if modern else ""
        messages.append(
            f"comm 0x{rank + 1:x} rank {rank} nranks 2 cudaDev {rank}{nvml} "
            f"busId {gpu['packed_bus']:x} - Init COMPLETE"
        )
    if backend:
        messages.append("NET/Socket : Using [0]bond0:192.0.2.1<0>")
    for source, destination in ((0, 1), (1, 0)):
        first = str(selected[source]["index"]) if modern else f"{selected[source]['packed_bus']:x}"
        second = str(selected[destination]["index"]) if modern else f"{selected[destination]['packed_bus']:x}"
        prefix = f"Channel 00 : {source}[{first}] -> {destination}[{second}]"
        if backend:
            messages.extend(prefix + f" [{direction}] via NET/Socket/0" for direction in ("send", "receive"))
        else:
            messages.append(prefix + " via P2P/CUMEM")
    return messages, selected


class RetainedCertificationTests(unittest.TestCase):
    HASHES = {
        "row07-gpu-gemm-bf16.log": "4f4170ee5203717bb703beacf717bf64b669f30ca329ef8aa20069045138dde3",
        "row08-gpu-gemm-fp8.log": "ee4fd2a004beef95a4e3ad63795efe718372c60c3b487b3533d5fcf843b1e18c",
        "row11-topology.stdout.gz": "bfa458f3d00960890f901f175c12dddb60c65969c2afe20943d8598ef77c56fe",
        "row12-all-reduce.stdout.gz": "d932324caa612b506972bf6dec885735d14f58ea7e3200c23332973790d8629b",
    }

    def test_retained_fixture_hashes(self):
        for name, expected in self.HASHES.items():
            with self.subTest(name=name):
                self.assertEqual(hashlib.sha256(fixture_bytes(name)).hexdigest(), expected)

    def test_retained_gpu_plans_include_sixteen_mib_workspace(self):
        expected_plans = {
            "row07-gpu-gemm-bf16.log": 16778752,
            "row08-gpu-gemm-fp8.log": 16778768,
        }
        for name, expected_plan in expected_plans.items():
            with self.subTest(name=name):
                records = [json.loads(line) for line in (FIXTURES / name).read_text().splitlines()
                           if line.startswith("{")]
                configuration = next(record for record in records if record.get("event") == "configuration")
                outcome = next(record for record in records if record.get("event") == "outcome")
                self.assertEqual(configuration["workspace_bytes"], 16 * 1024 * 1024)
                self.assertEqual(configuration["gpu_plan_bytes"], expected_plan)
                self.assertEqual(outcome["reason"], "ObservedCUDAAllocationExceedsPlan")


class GPUAllocationGuardTests(unittest.TestCase):
    def guard(self, script, *, allocated_peak, allocated_baseline, reserved_peak, reserved_baseline, plan):
        events = []
        cuda = types.SimpleNamespace(
            max_memory_allocated=lambda _device: allocated_peak,
            max_memory_reserved=lambda _device: reserved_peak,
        )
        namespace = worker_functions(
            script, {"Inconclusive", "Failure", "peak_check"},
            torch=types.SimpleNamespace(cuda=cuda),
            cfg={"gpu_plan_bytes": plan},
            allocated_baselines={0: allocated_baseline},
            reserved_baselines={0: reserved_baseline},
            event=lambda kind, **values: events.append({"event": kind, **values}),
        )
        return namespace, events, lambda: namespace["peak_check"](0, "test")

    def test_reserved_segment_slack_is_evidence_not_workload_usage(self):
        for script in ("gpu-gemm-bf16.sh", "gpu-gemm-fp8.sh"):
            with self.subTest(script=script):
                namespace, events, check = self.guard(
                    script,
                    allocated_peak=2 * 1024 * 1024,
                    allocated_baseline=0,
                    reserved_peak=20 * 1024 * 1024,
                    reserved_baseline=0,
                    plan=2 * 1024 * 1024,
                )
                accounting = check()
                self.assertEqual(accounting["cuda_incremental_peak_allocated_bytes"], 2 * 1024 * 1024)
                self.assertEqual(accounting["cuda_incremental_peak_reserved_bytes"], 20 * 1024 * 1024)
                self.assertEqual(events, [{"event": "cuda_memory_accounting", "device": 0, "phase": "test",
                                           "plan_bytes": 2 * 1024 * 1024, **accounting}])
                self.assertIn("Failure", namespace)

    def test_incremental_active_allocation_at_plan_boundary_passes(self):
        for script in ("gpu-gemm-bf16.sh", "gpu-gemm-fp8.sh"):
            with self.subTest(script=script):
                _, _, check = self.guard(
                    script,
                    allocated_peak=22 * 1024 * 1024,
                    allocated_baseline=20 * 1024 * 1024,
                    reserved_peak=64 * 1024 * 1024,
                    reserved_baseline=0,
                    plan=2 * 1024 * 1024,
                )
                self.assertEqual(check()["cuda_incremental_peak_allocated_bytes"], 2 * 1024 * 1024)

    def test_incremental_active_allocation_over_plan_is_failure(self):
        for script in ("gpu-gemm-bf16.sh", "gpu-gemm-fp8.sh"):
            with self.subTest(script=script):
                namespace, events, check = self.guard(
                    script,
                    allocated_peak=22 * 1024 * 1024 + 1,
                    allocated_baseline=20 * 1024 * 1024,
                    reserved_peak=64 * 1024 * 1024,
                    reserved_baseline=0,
                    plan=2 * 1024 * 1024,
                )
                with self.assertRaisesRegex(namespace["Failure"], "ObservedCUDAAllocationExceedsPlan"):
                    check()
                self.assertEqual(events[0]["cuda_incremental_peak_allocated_bytes"], 2 * 1024 * 1024 + 1)

    def test_allocator_peak_below_baseline_fails_closed(self):
        for script in ("gpu-gemm-bf16.sh", "gpu-gemm-fp8.sh"):
            with self.subTest(script=script):
                namespace, events, check = self.guard(
                    script,
                    allocated_peak=20 * 1024 * 1024 - 1,
                    allocated_baseline=20 * 1024 * 1024,
                    reserved_peak=32 * 1024 * 1024,
                    reserved_baseline=16 * 1024 * 1024,
                    plan=2 * 1024 * 1024,
                )
                with self.assertRaisesRegex(namespace["Inconclusive"], "CUDAAllocatorPeakMalformed"):
                    check()
                self.assertEqual(events, [])

    def test_real_bf16_and_fp8_operations_are_preserved(self):
        self.assertIn("torch.mm(a, b, out=output)", worker_source("gpu-gemm-bf16.sh"))
        fp8 = worker_source("gpu-gemm-fp8.sh")
        self.assertIn("torch._scaled_mm(a, b, scale_a=scale_a, scale_b=scale_b,", fp8)
        self.assertIn("out_dtype=torch.bfloat16, use_fast_accum=False", fp8)


class NCCLRetainedOutputTests(unittest.TestCase):
    SELECTED_BACKEND_GPUS = [{"index": 0, "packed_bus": 0x13000}, {"index": 1, "packed_bus": 0x63000}]

    def parse_backend(self, stdout):
        namespace = functions(
            "nccl-backend.sh",
            {"NUMBER", "HEADER", "INFO", "INLINE_INFO", "CHANNEL", "INIT",
             "integer", "metric", "transport", "parse_collective"},
            PROFILE="backend",
        )
        return namespace["parse_collective"](
            stdout, "", 4, self.SELECTED_BACKEND_GPUS, 1, 0, "Socket", "bond0", [])

    def test_ansi_formatted_retained_topology_is_verified(self):
        topology = fixture_bytes("row11-topology.stdout.gz").decode()
        summary = {}
        namespace = functions(
            "nccl-nvlink.sh", {"topology"}, summary=summary,
            run=lambda _argv, _env: (0, topology, ""),
        )
        namespace["topology"]("nvidia-smi", {}, [{"index": 0}, {"index": 1}])
        self.assertEqual(summary["topology"], {"selected_directed_links": 2, "link_types": ["NV18"]})

    def test_unsupported_topology_control_sequence_fails_closed(self):
        topology = fixture_bytes("row11-topology.stdout.gz").decode().replace("\x1b[4m", "\x1b[2K", 1)
        namespace = functions(
            "nccl-nvlink.sh", {"topology"}, summary={},
            run=lambda _argv, _env: (0, topology, ""),
        )
        with self.assertRaisesRegex(Rejected, "TopologyControlSequenceUnsupported"):
            namespace["topology"]("nvidia-smi", {}, [{"index": 0}, {"index": 1}])

    def test_duplicate_topology_header_fails_closed(self):
        topology = fixture_bytes("row11-topology.stdout.gz").decode().replace("GPU0\tGPU1", "GPU0\tGPU0", 1)
        namespace = functions(
            "nccl-nvlink.sh", {"topology"}, summary={},
            run=lambda _argv, _env: (0, topology, ""),
        )
        with self.assertRaisesRegex(Rejected, "TopologyHeaderMalformed"):
            namespace["topology"]("nvidia-smi", {}, [{"index": 0}, {"index": 1}])

    def test_interleaved_retained_collective_row_is_reconstructed(self):
        result = self.parse_backend(fixture_bytes("row12-all-reduce.stdout.gz").decode())
        self.assertEqual(result["out_of_place_time_us"], 80.85)
        self.assertEqual(result["in_place_time_us"], 82.01)
        self.assertEqual(result["wrong_count"], 0)
        self.assertEqual(result["out_of_bounds_count"], 0)
        self.assertEqual(result["evidence"]["transport"], "NET/Socket")
        self.assertEqual(result["evidence"]["observed_network_devices"], ["bond0"])
        self.assertEqual(result["evidence"]["channel_device_identifier"], "nvml-index")

    def test_incomplete_interleaved_collective_row_fails_closed(self):
        stdout = fixture_bytes("row12-all-reduce.stdout.gz").decode().replace(
            "    82.01    0.00    0.00       0\n", "", 1)
        with self.assertRaisesRegex(Rejected, "RequestedCollectiveRowOrSummaryMissing"):
            self.parse_backend(stdout)

    def test_ambiguous_extra_collective_fragment_fails_closed(self):
        stdout = fixture_bytes("row12-all-reduce.stdout.gz").decode().replace(
            "    82.01    0.00    0.00       0\n",
            "    82.01    0.00    0.00       0\n    1\n", 1)
        with self.assertRaisesRegex(Rejected, "RequestedCollectiveRowOrSummaryMissing"):
            self.parse_backend(stdout)

    def test_wrong_collective_root_fails_closed(self):
        stdout = fixture_bytes("row12-all-reduce.stdout.gz").decode().replace(
            "float     sum      -1gpu-07-03", "float     sum       0gpu-07-03", 1)
        with self.assertRaisesRegex(Rejected, "RequestedCollectiveRowMismatch"):
            self.parse_backend(stdout)


class NCCLChannelTests(unittest.TestCase):
    def check(self, script, *, modern, backend=False, mutate=None, version=None, mutate_selected=None):
        namespace = functions(script, {"CHANNEL", "INIT", "transport"}, PROFILE="backend" if backend else "nvlink")
        messages, selected = channel_messages(modern=modern, backend=backend, version=version)
        if mutate:
            mutate(messages)
        if mutate_selected:
            mutate_selected(selected)
        return namespace["transport"](messages, selected, "Socket", "bond0", [])

    def test_legacy_pci_channel_identity_is_preserved(self):
        for script in ("nccl-nvlink.sh", "nccl-backend.sh"):
            with self.subTest(script=script):
                result = self.check(script, modern=False, backend=script == "nccl-backend.sh")
                self.assertEqual(result["channels"], 1)
                self.assertEqual(result["channel_device_identifier"], "packed-pci")

    def test_pinned_nccl_nvml_channel_identity(self):
        for script in ("nccl-nvlink.sh", "nccl-backend.sh"):
            with self.subTest(script=script):
                result = self.check(script, modern=True, backend=script == "nccl-backend.sh")
                self.assertEqual(result["nccl_version"], "2.27.3")
                self.assertEqual(result["channels"], 1)
                self.assertEqual(result["channel_device_identifier"], "nvml-index")

    def test_nvml_mapping_is_derived_from_verified_initialization_not_rank(self):
        for script in ("nccl-nvlink.sh", "nccl-backend.sh"):
            with self.subTest(script=script):
                result = self.check(script, modern=True, backend=script == "nccl-backend.sh",
                                    mutate_selected=lambda selected: selected[0].update(index=0))
                self.assertEqual(result["channel_device_identifier"], "nvml-index")

    def test_wrong_cuda_ordinal_still_fails(self):
        for script in ("nccl-nvlink.sh", "nccl-backend.sh"):
            with self.subTest(script=script), self.assertRaises(Rejected):
                self.check(script, modern=True, backend=script == "nccl-backend.sh",
                           mutate=lambda messages: messages.__setitem__(
                               3, messages[3].replace("cudaDev 0", "cudaDev 1")))

    def test_wrong_physical_bus_still_fails(self):
        for script in ("nccl-nvlink.sh", "nccl-backend.sh"):
            with self.subTest(script=script), self.assertRaises(Rejected):
                self.check(script, modern=True, backend=script == "nccl-backend.sh",
                           mutate=lambda messages: messages.__setitem__(
                               3, messages[3].replace("busId 170000", "busId 180000")))

    def test_wrong_nvml_device_still_fails(self):
        for script in ("nccl-nvlink.sh", "nccl-backend.sh"):
            with self.subTest(script=script), self.assertRaises(Rejected):
                self.check(script, modern=True, backend=script == "nccl-backend.sh",
                           mutate=lambda messages: messages.__setitem__(
                               3, messages[3].replace("nvmlDev 3", "nvmlDev 4")))

    def test_duplicate_nvml_mapping_still_fails(self):
        for script in ("nccl-nvlink.sh", "nccl-backend.sh"):
            with self.subTest(script=script), self.assertRaises(Rejected):
                self.check(script, modern=True, backend=script == "nccl-backend.sh",
                           mutate=lambda messages: messages.__setitem__(
                               4, messages[4].replace("nvmlDev 7", "nvmlDev 3")))

    def test_wrong_channel_device_still_fails(self):
        for script in ("nccl-nvlink.sh", "nccl-backend.sh"):
            with self.subTest(script=script), self.assertRaises(Rejected):
                self.check(script, modern=True, backend=script == "nccl-backend.sh",
                           mutate=lambda messages: messages.__setitem__(
                               -1, messages[-1].replace("0[3]", "0[4]")))

    def test_packed_pci_is_not_a_modern_nvml_tag(self):
        for script in ("nccl-nvlink.sh", "nccl-backend.sh"):
            with self.subTest(script=script), self.assertRaises(Rejected):
                self.check(script, modern=False, backend=script == "nccl-backend.sh", version="2.27.3")

    def test_disconnected_or_incomplete_ring_still_fails(self):
        for script in ("nccl-nvlink.sh", "nccl-backend.sh"):
            with self.subTest(script=script), self.assertRaises(Rejected):
                self.check(script, modern=True, backend=script == "nccl-backend.sh",
                           mutate=lambda messages: messages.pop())

    def test_unknown_version_does_not_guess_nvml_semantics(self):
        for script in ("nccl-nvlink.sh", "nccl-backend.sh"):
            with self.subTest(script=script), self.assertRaises(Rejected):
                self.check(script, modern=True, backend=script == "nccl-backend.sh", version="2.99.0")


class NVBandwidthLatencyTests(unittest.TestCase):
    DESCRIPTIONS = {
        "host_device_latency_sm": "memory latency SM CPU(row) <-> GPU(column) (ns)",
        "device_to_device_latency_sm": "Device to Device Latency SM GPU(row) <-> GPU(column) (ns)",
    }

    def fixture(self, case):
        return {"nvbandwidth": {"git_version": "controlled-fixture", "testcases": [{
            "name": case, "status": "Passed", "bandwidth_description": self.DESCRIPTIONS[case],
            "bandwidth_matrix": [["1", "2"]] if case == "host_device_latency_sm" else [["N/A", "1"], ["2", "N/A"]],
        }]}}

    def parse(self, case, fixture):
        namespace = functions("nvbandwidth.sh", {"CASES", "parse_case", "json_object", "json_load"})
        return namespace["parse_case"](json.dumps(fixture), case, 2)

    def test_exact_pinned_vendor_latency_descriptions(self):
        for case in self.DESCRIPTIONS:
            with self.subTest(case=case):
                values, errors, revision = self.parse(case, self.fixture(case))
                self.assertEqual(values, [1.0, 2.0])
                self.assertEqual(errors, 0)
                self.assertEqual(revision, "controlled-fixture")

    def test_legacy_latency_unit_description_remains_supported(self):
        for case in self.DESCRIPTIONS:
            with self.subTest(case=case):
                fixture = self.fixture(case)
                fixture["nvbandwidth"]["testcases"][0]["bandwidth_description"] = "latency (ns)"
                self.assertEqual(self.parse(case, fixture)[0], [1.0, 2.0])

    def test_wrong_units_unknown_descriptions_and_swapped_cases_fail(self):
        for case, description in self.DESCRIPTIONS.items():
            for changed in (description.replace("(ns)", "(us)"), description + " unverified",
                            next(value for name, value in self.DESCRIPTIONS.items() if name != case)):
                with self.subTest(case=case, changed=changed), self.assertRaises(Rejected):
                    fixture = self.fixture(case)
                    fixture["nvbandwidth"]["testcases"][0]["bandwidth_description"] = changed
                    self.parse(case, fixture)

    def test_missing_peer_cell_does_not_pass(self):
        fixture = self.fixture("device_to_device_latency_sm")
        fixture["nvbandwidth"]["testcases"][0]["bandwidth_matrix"][0][1] = "N/A"
        with self.assertRaises(Rejected):
            self.parse("device_to_device_latency_sm", fixture)

    def test_ambiguous_metric_fields_do_not_pass(self):
        fixture = self.fixture("host_device_latency_sm")
        fixture["nvbandwidth"]["testcases"][0]["latency_description"] = "latency (ns)"
        with self.assertRaises(Rejected):
            self.parse("host_device_latency_sm", fixture)

    def test_empty_revision_still_fails(self):
        fixture = self.fixture("host_device_latency_sm")
        fixture["nvbandwidth"]["git_version"] = ""
        with self.assertRaises(Rejected):
            self.parse("host_device_latency_sm", fixture)


if __name__ == "__main__":
    unittest.main()
