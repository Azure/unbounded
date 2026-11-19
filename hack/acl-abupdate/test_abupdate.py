# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

"""Unit tests for the harness's parsing and bookkeeping, which run without a VM:

    python3 -m unittest discover -s hack/acl-abupdate -p 'test_*.py'
"""

from __future__ import annotations

import contextlib
import io
import json
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest
import unittest.mock
import urllib.parse
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import abupdate  # noqa: E402
import scenarios  # noqa: E402

# Shaped like `trident get status`: serde_yaml's output, with sequences at their key's indent.
HOST_STATUS = """\
spec:
  image:
    url: file:///var/tmp/acl-abupdate/0123456789abcdef.cosi
    sha384: "aa11"
  storage:
    abUpdate:
      volumePairs:
      - id: root
        volumeAId: root-a
        volumeBId: root-b
    disks: []
  scripts:
    postConfigure:
    - name: rw-overlay
      content: |
        mkdir -p /var/lib/trident-overlay
      runOn:
      - all
specOld: {}
servicingState: provisioned
lastError: null
partitionPaths:
  esp: /dev/vda1
abActiveVolume: volume-b
installIndex: 0
"""

FAILED_STATUS = """\
servicingState: ab-update-health-check-failed
lastError:
  category: servicing
  error: health-checks-failed
  message: "Health check 'acl-abupdate-forced-failure' failed: exit 1"
abActiveVolume: volume-a
"""

VERITY = """\
/dev/mapper/usr is active and is in use.
  type:        VERITY
  status:      verified
  hash type:   1
  data block:  4096
  data device: /dev/vda4
  size:        2097152 sectors
  mode:        readonly
  hash device: /dev/vda5
  root hash:   0f1e2d3c4b5a69788796a5b4c3d2e1f0
"""


def efi_hex(payload: bytes) -> str:
    """A variable as the probe reads it from efivarfs: 4 bytes of attributes, then its data."""
    return (b"\x07\x00\x00\x00" + payload).hex()


def load_option(description: str) -> bytes:
    return b"\x01\x00\x00\x00" + b"\x10\x00" + description.encode("utf-16-le") + b"\0\0" + b"\x7f\xff\x04\x00"


def make_cosi(path: Path, metadata: dict, *, marker: bool = True) -> bytes:
    raw = json.dumps(metadata).encode()
    with tarfile.open(path, "w", format=tarfile.USTAR_FORMAT) as tar:
        entries = [("cosi-marker", b"")] if marker else []
        entries += [("metadata.json", raw), ("images/usr.rawzst", b"\0" * 1024)]
        for name, content in entries:
            info = tarfile.TarInfo(name)
            info.size = len(content)
            tar.addfile(info, io.BytesIO(content))
    return raw


COSI_METADATA = {
    "version": "1.2",
    "osArch": "x86_64",
    "osRelease": 'NAME="Azure Container Linux"\nVERSION="3.0.20261106"\nVERSION_ID="3.0"\n',
    "images": [
        {"image": {"path": "images/esp.rawzst"}, "mountPoint": "/boot/efi", "verity": None},
        {"image": {"path": "images/usr.rawzst"}, "mountPoint": "/usr",
         "verity": {"image": {"path": "images/usr-hash.rawzst"}, "roothash": "feedface"}},
    ],
}


class ParseYamlTest(unittest.TestCase):
    def test_host_status(self) -> None:
        flat = abupdate.parse_yaml(HOST_STATUS)
        cases = {
            "spec.image.url": "file:///var/tmp/acl-abupdate/0123456789abcdef.cosi",
            "spec.image.sha384": "aa11",
            "servicingState": "provisioned",
            "abActiveVolume": "volume-b",
            "lastError": None,
            "partitionPaths.esp": "/dev/vda1",
            "installIndex": "0",
            "spec.storage.abUpdate.volumePairs": abupdate.BLOCK,
            "spec.storage.disks": "[]",
            "specOld": "{}",
        }
        for key, want in cases.items():
            with self.subTest(key=key):
                self.assertEqual(flat.get(key), want)
        # Keys inside sequence items aren't flattened, so they can't shadow the mapping's.
        self.assertNotIn("spec.storage.abUpdate.volumePairs.id", flat)
        self.assertNotIn("spec.scripts.postConfigure.content", flat)

    def test_nested_last_error(self) -> None:
        flat = abupdate.parse_yaml(FAILED_STATUS)
        self.assertEqual(flat["lastError"], abupdate.BLOCK)
        self.assertEqual(flat["lastError.message"], "Health check 'acl-abupdate-forced-failure' failed: exit 1")
        self.assertEqual(flat["abActiveVolume"], "volume-a")
        self.assertIn("health-checks-failed", abupdate.yaml_block(FAILED_STATUS, "lastError"))

    def test_block_scalar_and_empty_mapping(self) -> None:
        flat = abupdate.parse_yaml("a:\n  text: |\n    one\n    two\n  after: x\nempty:\nlast: 'it''s'\n")
        self.assertEqual(flat["a.text"], "one\ntwo")
        self.assertEqual(flat["a.after"], "x")
        self.assertIsNone(flat["empty"])
        self.assertEqual(flat["last"], "it's")

    def test_scalars(self) -> None:
        cases = {"null": None, "~": None, '"quoted: yes"': "quoted: yes", "plain # comment": "plain",
                 "'#not a comment'": "#not a comment"}
        for raw, want in cases.items():
            with self.subTest(raw=raw):
                self.assertEqual(abupdate.yaml_scalar(raw), want)


class ProbeTest(unittest.TestCase):
    def probe(self, **sections: str) -> abupdate.Probe:
        base = {"boot_id": "b1", "verity": VERITY, "status": HOST_STATUS, "usr_writable": "no"}
        return abupdate.Probe({**base, **sections})

    def test_slot_and_status(self) -> None:
        probe = self.probe()
        self.assertEqual(probe.pair, ("/dev/vda4", "/dev/vda5"))
        self.assertEqual(probe.roothash, "0f1e2d3c4b5a69788796a5b4c3d2e1f0")
        self.assertEqual(probe.state, "provisioned")
        self.assertEqual(probe.active_volume, "volume-b")
        self.assertIsNone(probe.last_error)
        self.assertEqual(probe.image_sha384, "aa11")
        self.assertFalse(probe.usr_writable)

    def test_last_error_text(self) -> None:
        probe = self.probe(status=FAILED_STATUS)
        self.assertIn("acl-abupdate-forced-failure", probe.last_error or "")

    def test_status_unavailable(self) -> None:
        probe = self.probe(status="!error\nError: datastore not found")
        self.assertIsNone(probe.status)
        self.assertIsNone(probe.state)
        self.assertIn("datastore", probe.status_error)

    def test_efi_and_markers(self) -> None:
        efi = "\n".join([
            f"BootCurrent={efi_hex(bytes([3, 0]))}",
            f"BootOrder={efi_hex(bytes([3, 0, 1, 0, 0x0a, 0]))}",
            f"Boot0003={efi_hex(load_option('ACL B'))}",
        ])
        probe = self.probe(efi=efi, markers="/etc/m\t1-aa 2-bb \n/opt/m\t", boots="3")
        self.assertEqual(probe.boot_current, "0003")
        self.assertEqual(probe.boot_order, ["0003", "0001", "000A"])
        self.assertEqual(probe.boot_next, "")
        self.assertEqual(probe.boot_label("0003"), "0003 (ACL B)")
        self.assertEqual(probe.boot_label("0001"), "0001")
        self.assertEqual(probe.markers, {"/etc/m": ["1-aa", "2-bb"], "/opt/m": []})
        self.assertEqual(probe.boot_count, 3)

    def test_sections(self) -> None:
        sections = abupdate.parse_sections("noise\n### a\none\n\n### b\ntwo\nthree\n")
        self.assertEqual(sections, {"a": "one", "b": "two\nthree"})

    def test_probe_script_parses(self) -> None:
        if not shutil.which("bash"):
            self.skipTest("bash isn't installed")
        script = abupdate.probe_script()
        result = subprocess.run(["bash", "-n"], input=script, text=True, capture_output=True, check=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("@", script.replace("$@", ""), "a placeholder wasn't replaced")


class FilesTest(unittest.TestCase):
    def setUp(self) -> None:
        self.directory = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.directory)

    def test_read_cosi(self) -> None:
        path = self.directory / "update.cosi"
        raw = make_cosi(path, COSI_METADATA)
        info = abupdate.read_cosi(path)
        self.assertEqual(info["metadataSha384"], abupdate.hashlib.sha384(raw).hexdigest())
        self.assertEqual(info["roothash"], "feedface")
        self.assertEqual(info["osRelease"]["VERSION"], "3.0.20261106")
        image = abupdate.Image("20261106.1", path, "cosi", abupdate.describe_file(path, "cosi"))
        self.assertEqual(image.version, "3.0.20261106")

    def test_read_cosi_needs_the_marker(self) -> None:
        path = self.directory / "plain.cosi"
        make_cosi(path, COSI_METADATA, marker=False)
        with self.assertRaises(ValueError):
            abupdate.read_cosi(path)

    def test_other_architecture(self) -> None:
        path = self.directory / "arm.cosi"
        make_cosi(path, {**COSI_METADATA, "osArch": "arm64"})
        with self.assertRaises(abupdate.StepError):
            abupdate.describe_file(path, "cosi")

    def test_iso_volume_id(self) -> None:
        path = self.directory / "seed.iso"
        descriptor = bytearray(2048)
        descriptor[0:6] = b"\x01CD001"
        descriptor[40:72] = b"ACLSEED".ljust(32)
        path.write_bytes(bytes(0x8000) + bytes(descriptor))
        self.assertEqual(abupdate.iso_volume_id(path), "ACLSEED")
        (self.directory / "not.iso").write_bytes(bytes(0x9000))
        self.assertIsNone(abupdate.iso_volume_id(self.directory / "not.iso"))

    def test_seed(self) -> None:
        if not any(shutil.which(tool) for tool in ("xorriso", "genisoimage", "mkisofs", "bsdtar")):
            self.skipTest("no ISO tool is installed")
        iso = abupdate.build_seed(self.directory, "ssh-ed25519 AAAA test")
        self.assertEqual(abupdate.iso_volume_id(iso), "ACLSEED")
        config = json.loads((self.directory / "seed" / "ignition" / "config.ign").read_text())
        self.assertEqual(config["passwd"]["users"][0]["sshAuthorizedKeys"], ["ssh-ed25519 AAAA test"])

    def test_ignition_sudoers(self) -> None:
        config = abupdate.ignition_config("key")
        sudoers = config["storage"]["files"][0]
        self.assertEqual(json.loads(json.dumps(sudoers))["mode"], 288)
        content = urllib.parse.unquote(sudoers["contents"]["source"].removeprefix("data:,"))
        self.assertEqual(content, "core ALL=(ALL) NOPASSWD: ALL\n")


class SeedTest(unittest.TestCase):
    def test_seed_leaves_trident_acl_agent_alone(self) -> None:
        # Unbounded's host agent configures trident-acl-agent on a node centerpivot updates.
        self.assertNotIn("systemd", abupdate.ignition_config("key"))

    def test_seed_digest(self) -> None:
        digest = abupdate.seed_digest()
        self.assertEqual(abupdate.seed_digest(), digest)
        with unittest.mock.patch.object(abupdate, "DEFAULT_HOSTNAME", "changed"):
            self.assertNotEqual(abupdate.seed_digest(), digest)


class TargetTest(unittest.TestCase):
    BASE = dict(code=Path("/c.fd"), disk=Path("/d.qcow2"), variables=Path("/v.qcow2"), memory=6144, cpus=2,
                ssh_port=2222)

    def test_qemu_command(self) -> None:
        plain = abupdate.qemu_command(**self.BASE)
        self.assertNotIn("-netdev", plain)
        self.assertNotIn("-S", plain)
        with_nic = abupdate.qemu_command(**self.BASE, nic_tap="tap-acl1", nic_mac="52:54:00:ab:71:0a", loadvm="base")
        user_nic = with_nic.index("-nic")
        tap = with_nic.index("-netdev")
        # The user NIC stays first, so it's eth0 and keeps SSH, the default route, and DNS.
        self.assertLess(user_nic, tap)
        self.assertEqual(with_nic[tap + 1], "tap,id=nic1,ifname=tap-acl1,script=no,downscript=no")
        self.assertEqual(with_nic[tap + 3], "virtio-net-pci,netdev=nic1,mac=52:54:00:ab:71:0a,addr=0x10")
        self.assertEqual(with_nic[-2:], ["-loadvm", "base"])
        self.assertEqual(abupdate.qemu_command(**self.BASE, loadvm="base", paused=True)[-1], "-S")

    def test_qemu_command_smbios_uuid(self) -> None:
        self.assertNotIn("-uuid", abupdate.qemu_command(**self.BASE))
        command = abupdate.qemu_command(**self.BASE, smbios_uuid="0b6a5c1e-1111-2222-3333-444455556666")
        self.assertEqual(command[command.index("-uuid") + 1], "0b6a5c1e-1111-2222-3333-444455556666")

    def test_seed_names_the_host_and_claims_the_second_nic(self) -> None:
        files = {f["path"]: f for f in abupdate.ignition_config("key")["storage"]["files"]}
        hostname = files["/etc/hostname"]
        self.assertTrue(hostname["overwrite"])
        self.assertEqual(urllib.parse.unquote(hostname["contents"]["source"].removeprefix("data:,")), "acl-abupdate\n")
        network = urllib.parse.unquote(files[abupdate.NIC_NETWORK]["contents"]["source"].removeprefix("data:,"))
        # By PCI address, not MAC, so VMs from one installed base each have their own MAC.
        self.assertIn("Path=pci-0000:00:10.0", network)
        self.assertIn("RequiredForOnline=no", network)
        self.assertIn("DHCP=no", network)
        self.assertFalse(any(line.startswith("Address=") for line in network.splitlines()), network)
        # It sorts before the image's catch-all zz-default.network, so it claims the NIC first.
        self.assertLess(Path(abupdate.NIC_NETWORK).name, "zz-default.network")

    def test_nic_mac(self) -> None:
        self.assertEqual(abupdate.nic_mac("192.168.113.10"), "52:54:00:ab:71:0a")
        self.assertEqual(abupdate.nic_mac("192.168.113.11"), "52:54:00:ab:71:0b")

    def test_state_keeps_the_nic(self) -> None:
        state = abupdate.State(nic={"tap": "tap-acl1", "address": "192.168.113.10", "mac": "m"}, hostname="acl-1")
        restored = abupdate.State.from_dict(abupdate.dataclasses.asdict(state))
        self.assertEqual((restored.nic["tap"], restored.hostname), ("tap-acl1", "acl-1"))

    def test_pick_other(self) -> None:
        base = abupdate.Image("base", Path("/b"), "cosi", {"roothash": "aa"})
        update = abupdate.Image("update", Path("/u"), "cosi", {"roothash": "bb"})
        self.assertIs(abupdate.pick_other("aa", base, update), update)
        self.assertIs(abupdate.pick_other("bb", base, update), base)
        with self.assertRaises(abupdate.StepError):
            abupdate.pick_other("cc", base, update)

    def test_efi_order_ignores_a_trailing_byte(self) -> None:
        self.assertEqual(abupdate.efi_boot_order(efi_hex(bytes([1, 0, 2]))), ["0001"])
        self.assertEqual(abupdate.efi_boot_number("zz"), "")

    def test_kernel_slot(self) -> None:
        self.assertEqual(abupdate.kernel_slot("mount.usr=/dev/mapper/usr acl.slot=b ro"), "b")
        self.assertEqual(abupdate.kernel_slot("root=LABEL=ROOT"), "")

    def test_booted_at(self) -> None:
        probe = abupdate.Probe({"btime": "1794517000"})
        self.assertEqual(probe.booted_at, abupdate.datetime.datetime.fromtimestamp(1794517000, abupdate.datetime.timezone.utc))
        self.assertIsNone(abupdate.Probe({}).booted_at)


class BookkeepingTest(unittest.TestCase):
    def test_check_labels(self) -> None:
        checks = abupdate.Checks()
        with checks.scope("pingpong"), checks.scope("P2"):
            checks.expect(True, "A1", "staged")
            checks.expect(False, "A4", "committed", "it isn't")
        checks.skip("H1", "health", "unsupported")
        self.assertEqual([r["id"] for r in checks.results], ["pingpong/P2/A1", "pingpong/P2/A4", "H1"])
        self.assertEqual([r["result"] for r in checks.results], ["pass", "fail", "skip"])
        self.assertEqual(len(checks.failed()), 1)
        self.assertEqual(checks.failed(2), [])

    def test_installed_base_per_seed(self) -> None:
        # Another harness's seed, sharing the cache, gets its own install rather than replacing this one.
        here = abupdate.install_dir("20261105.1")
        self.assertTrue(here.name.startswith("20261105.1-"))
        with unittest.mock.patch.object(abupdate, "DEFAULT_HOSTNAME", "changed"):
            self.assertNotEqual(abupdate.install_dir("20261105.1"), here)
        with tempfile.TemporaryDirectory() as directory:
            installed = Path(directory)
            self.assertIn("there's no installed base", abupdate.install_needed(installed, False))
            (installed / "info.json").write_text("{}")
            self.assertEqual(abupdate.install_needed(installed, False), "")
            self.assertIn("--fresh-install", abupdate.install_needed(installed, True))

    def test_state_round_trip(self) -> None:
        state = abupdate.State(base_tag="1", update_tag="2", markers=["1-aa"])
        data = {**abupdate.dataclasses.asdict(state), "unknown": True}
        self.assertEqual(abupdate.State.from_dict(data), state)

    def test_scenarios(self) -> None:
        self.assertEqual(list(scenarios.REGISTRY)[0], "forward")
        for name, scenario in scenarios.REGISTRY.items():
            with self.subTest(name=name):
                self.assertEqual(scenario.name, name)
                self.assertTrue(scenario.description)
                self.assertTrue(scenario.checks)
                self.assertTrue(callable(scenario.run))
        with self.assertRaises(ValueError):
            scenarios.scenario("all", "reserved")(lambda ctx: None)
        with self.assertRaises(ValueError):
            scenarios.scenario("forward", "taken")(lambda ctx: None)

    def test_parser(self) -> None:
        parser = abupdate.build_parser()
        args = parser.parse_args(["run", "pingpong", "manual-rollback", "--rounds", "3", "--from", "base"])
        self.assertEqual((args.scenarios, args.rounds, args.from_snapshot), (["pingpong", "manual-rollback"], 3, "base"))
        args = parser.parse_args(["--json", "update", "--to", "base", "--no-commit"])
        self.assertTrue(args.json and args.no_commit)
        self.assertEqual(args.to, "base")
        with self.assertRaises(SystemExit), contextlib.redirect_stderr(io.StringIO()):
            parser.parse_args(["run", "nonesuch"])

    def test_several_vm_parser(self) -> None:
        parser = abupdate.build_parser()
        args = parser.parse_args(["up", "--nic-tap", "tap-acl1", "--nic-address", "192.168.113.10", "--hostname", "acl-1"])
        self.assertEqual((args.nic_tap, args.nic_address, args.hostname), ("tap-acl1", "192.168.113.10", "acl-1"))
        self.assertEqual(parser.parse_args(["up"]).nic_tap, "")
        self.assertTrue(parser.parse_args(["snapshot", "restore", "joined", "--hold"]).hold)
        self.assertFalse(parser.parse_args(["snapshot", "restore", "joined"]).hold)
        self.assertEqual(parser.parse_args(["resume", "--restart", "a.service", "--restart", "b.service"]).restart,
                         ["a.service", "b.service"])
        self.assertEqual(parser.parse_args(["fail-next-boot", "--timeout", "60"]).timeout, 60)


class ImageSettingsTest(unittest.TestCase):
    def setUp(self) -> None:
        names = ("REGISTRY_PATH", "ISO_REPO", "COSI_REPO", "BASE_ISO", "BASE_COSI", "UPDATE_COSI")
        saved = {name: getattr(abupdate, name) for name in names}
        self.addCleanup(lambda: [setattr(abupdate, name, value) for name, value in saved.items()])
        for name in names:
            setattr(abupdate, name, "")

    def test_no_defaults(self) -> None:
        self.assertEqual(abupdate.up_settings_missing("", ""),
                         ["ACL_AB_BASE_TAG", "ACL_AB_UPDATE_TAG", "ACL_AB_REGISTRY", "ACL_AB_ISO_REPO", "ACL_AB_COSI_REPO"])
        with self.assertRaises(abupdate.StepError) as raised:
            abupdate.fetch_artifact("", "1.0", "cosi")
        self.assertIn("ACL_AB_REGISTRY, ACL_AB_COSI_REPO", str(raised.exception))
        self.assertIsNone(abupdate.cosi_if_present("1.0"))

    def test_local_files_need_no_registry(self) -> None:
        abupdate.BASE_ISO, abupdate.BASE_COSI, abupdate.UPDATE_COSI = "/a.iso", "/a.cosi", "/b.cosi"
        self.assertEqual(abupdate.up_settings_missing("base", "update"), [])
        abupdate.UPDATE_COSI = ""
        self.assertEqual(abupdate.up_settings_missing("base", "update"), ["ACL_AB_REGISTRY", "ACL_AB_COSI_REPO"])

    def test_the_vm_keeps_its_settings(self) -> None:
        state = abupdate.State(registry="example.azurecr.io/acl", iso_repo="iso", cosi_repo="cosi")
        abupdate.use_images(state)
        self.assertEqual((abupdate.REGISTRY_PATH, abupdate.ISO_REPO, abupdate.COSI_REPO),
                         ("example.azurecr.io/acl", "iso", "cosi"))
        self.assertEqual(abupdate.registry_host(), "example.azurecr.io")
        # A VM from before up recorded them keeps what the environment gives.
        abupdate.use_images(abupdate.State())
        self.assertEqual(abupdate.REGISTRY_PATH, "example.azurecr.io/acl")


class BuildTest(unittest.TestCase):
    def setUp(self) -> None:
        self.directory = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.directory)
        self.addCleanup(setattr, abupdate, "BUILDS", abupdate.BUILDS)
        self.addCleanup(setattr, abupdate, "BASE_TAG", abupdate.BASE_TAG)
        abupdate.BUILDS = self.directory

    def add_build(self, name: str, roothash: str, version: str, count: int = 1) -> abupdate.Image:
        directory = self.directory / name
        directory.mkdir()
        metadata = json.loads(json.dumps(COSI_METADATA))
        metadata["images"][1]["verity"]["roothash"] = roothash
        make_cosi(directory / f"{name}.cosi", metadata)
        info = abupdate.describe_file(directory / f"{name}.cosi", "cosi")
        info["built"] = {"from": {"name": "20261105.1"}, "version": version, "count": count}
        (directory / "info.json").write_text(json.dumps(info))
        image = abupdate.build_image(name)
        assert image is not None
        return image

    def test_names(self) -> None:
        for name in ("hello", "my-build2", "a" * 40):
            with self.subTest(name=name):
                self.assertEqual(abupdate.build_name_problem(name), "")
        for name in ("Hello", "1abc", "a_b", "a.b", "a" * 41, "base", "update", "other", ""):
            with self.subTest(name=name):
                self.assertIn("can't name a build", abupdate.build_name_problem(name))

    def test_versions(self) -> None:
        tagged = abupdate.Image("20261105.1", Path("/a.cosi"), "cosi", {"osRelease": {"VERSION_ID": "3.0.20261104"}})
        self.assertEqual(abupdate.build_version("hello", tagged, None), ("3.0.20261104-hello.1", 1))
        previous = self.add_build("hello", "aa" * 32, "3.0.20261104-hello.2", count=2)
        self.assertEqual(abupdate.build_version("hello", tagged, previous), ("3.0.20261104-hello.3", 3))
        # A build on a build keeps the first's core version.
        built = abupdate.Image("hello", Path("/b.cosi"), "cosi", {"osRelease": {"VERSION_ID": "3.0.20261104-hello.2"}})
        self.assertEqual(abupdate.build_version("world", built, None), ("3.0.20261104-world.1", 1))
        # One that sorts above the source, as Nebraska needs to offer it.
        self.assertEqual(abupdate.build_version("e2e", tagged, None, "3.0.20261105-e2e.1"), ("3.0.20261105-e2e.1", 1))
        for release in ({}, {"VERSION_ID": "3.0"}):
            unversioned = abupdate.Image("custom.cosi", Path("/c.cosi"), "cosi", {"osRelease": release})
            self.assertEqual(abupdate.build_version("x", unversioned, None), ("", 1), release)

    def test_semver(self) -> None:
        for version in ("3.0.20261104", "3.0.20261105-e2e.1", "1.2.3+build.5"):
            self.assertTrue(abupdate.is_semver(version), version)
        for version in ("3.0", "20261105.1", "v1.2.3", "01.2.3"):
            self.assertFalse(abupdate.is_semver(version), version)

    def test_a_build_is_a_target(self) -> None:
        roothash = "cd" * 32
        image = self.add_build("hello", roothash, "20261105.1.0-hello.1")
        for leftover in ("hello.partial", "hello.work", "Upper"):
            (self.directory / leftover).mkdir()
        self.assertEqual([build.name for build in abupdate.known_builds()], ["hello"])
        resolved = abupdate.resolve_target("hello", abupdate.State(), lambda: "")
        self.assertEqual((resolved.name, resolved.roothash), ("hello", roothash))
        self.assertEqual(abupdate.image_name(roothash), "hello")
        self.assertIn("built from 20261105.1, version 20261105.1.0-hello.1", image.describe())
        self.assertIsNone(abupdate.build_image("missing"))

    def test_build_source(self) -> None:
        abupdate.BASE_TAG = ""
        with self.assertRaisesRegex(abupdate.StepError, "ACL_AB_BASE_TAG"):
            abupdate.build_source("base", None)
        with self.assertRaisesRegex(abupdate.StepError, "--from takes"):
            abupdate.build_source("other", abupdate.State())
        self.add_build("hello", "ef" * 32, "20261105.1.0-hello.1")
        self.assertEqual(abupdate.build_source("hello", None).name, "hello")

    def test_delete(self) -> None:
        builds = self.directory / "builds"
        builds.mkdir()
        abupdate.BUILDS = builds
        (builds / "hello").mkdir()
        (builds / "hello.work").mkdir()
        (self.directory / "victim.work").mkdir()
        parser = abupdate.build_parser()
        with unittest.mock.patch.object(abupdate.cosibuild, "reclaim"), \
                unittest.mock.patch.object(abupdate, "STATE_FILE", self.directory / "no-state.json"):
            abupdate.image_delete(parser.parse_args(["image", "delete", "hello"]))
            self.assertEqual(list(builds.iterdir()), [])
            with self.assertRaises(SystemExit), contextlib.redirect_stderr(io.StringIO()):
                abupdate.image_delete(parser.parse_args(["image", "delete", "../victim"]))
        self.assertTrue((self.directory / "victim.work").is_dir())

    def test_parser(self) -> None:
        parser = abupdate.build_parser()
        args = parser.parse_args(["image", "build", "hello", "--files", "d", "--config", "c.yaml", "--from", "update"])
        self.assertEqual((args.action, args.name, args.files, args.config, args.source),
                         ("build", "hello", "d", "c.yaml", "update"))
        self.assertFalse(args.keep_work)
        self.assertEqual(args.os_version, "")
        self.assertEqual(parser.parse_args(["image", "build", "e2e", "--files", "d", "--os-version", "3.0.1-e2e.1"]).os_version,
                         "3.0.1-e2e.1")
        self.assertEqual(parser.parse_args(["image", "build", "hello", "--files", "d"]).source, "base")
        self.assertEqual(parser.parse_args(["image", "fetch", "1", "2", "--iso"]).tags, ["1", "2"])
        self.assertEqual(parser.parse_args(["image", "delete", "hello"]).name, "hello")
        self.assertEqual(parser.parse_args(["image", "list"]).action, "list")
        for retired in (["fetch"], ["images"]):
            with self.subTest(command=retired), self.assertRaises(SystemExit), contextlib.redirect_stderr(io.StringIO()):
                parser.parse_args(retired)


if __name__ == "__main__":
    unittest.main()
