# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

from pathlib import Path
import subprocess
import tempfile
import tomllib
import unittest


ROOT = Path(__file__).resolve().parents[2]
CHECK = ROOT / "images/racer-dataplane/check-debug-info.sh"


def run(*command):
    return subprocess.run(
        ["timeout", "--signal=TERM", "--kill-after=10s", "30s", *map(str, command)],
        cwd=ROOT, capture_output=True, text=True, timeout=45,
    )


class RacerReleaseSymbolsTest(unittest.TestCase):
    def test_release_debug_is_independent_of_features(self):
        manifest = tomllib.loads((ROOT / "cmd/racer-dataplane/Cargo.toml").read_text())
        self.assertEqual(manifest["features"]["default"], [])
        self.assertEqual(manifest["profile"]["release"]["debug"], 1)
        container = (ROOT / "images/racer-dataplane/Containerfile").read_text()
        self.assertNotIn("CARGO_PROFILE_RELEASE_DEBUG", container)
        self.assertIn("ARG RACER_HEAP_PROFILING=false", container)

    def test_both_build_paths_check_before_install(self):
        for path, check in [
            ("Makefile", 'sh images/racer-dataplane/check-debug-info.sh "$(RACER_CARGO_TARGET_DIR)/release/racer-dataplane"'),
            ("images/racer-dataplane/Containerfile", "sh ./check-debug-info.sh target/release/racer-dataplane"),
        ]:
            with self.subTest(path=path):
                text = (ROOT / path).read_text()
                self.assertIn(check, text)
                # Both commands are in the build recipe, immediately before install.
                self.assertTrue(text.split(check, 1)[1].splitlines()[1].lstrip().startswith("install "))

    def test_real_elf_accepts_debug_and_rejects_missing_or_stripped_debug(self):
        with tempfile.TemporaryDirectory(dir=ROOT / "tmp") as directory:
            directory = Path(directory)
            source = directory / "fixture.c"
            source.write_text("int main(void) { return 0; }\n")
            for debug in (True, False):
                with self.subTest(debug=debug):
                    binary = directory / "fixture"
                    result = run("cc", "-g" if debug else "-g0", source, "-o", binary)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    result = run("sh", CHECK, binary)
                    self.assertEqual(result.returncode, 0 if debug else 1, result.stderr)
                    if debug:
                        # Keep .debug_info but remove the line table to check both requirements.
                        result = run("objcopy", "--remove-section=.debug_line", binary)
                        self.assertEqual(result.returncode, 0, result.stderr)
                        result = run("sh", CHECK, binary)
                        self.assertEqual(result.returncode, 1)
                        self.assertIn("missing .debug_line;", result.stderr)
                    result = run("strip", "--strip-debug", binary)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    result = run("sh", CHECK, binary)
                    self.assertEqual(result.returncode, 1)
                    self.assertIn("missing .debug_info;", result.stderr)

    def test_invalid_or_missing_elf_fails(self):
        with tempfile.TemporaryDirectory(dir=ROOT / "tmp") as directory:
            invalid = Path(directory) / "not-elf"
            invalid.write_text("not an ELF\n")
            for path in (invalid, Path(directory) / "missing"):
                with self.subTest(path=path):
                    self.assertNotEqual(run("sh", CHECK, path).returncode, 0)

    def test_usage(self):
        for args in ((), ("one", "two")):
            with self.subTest(args=args):
                result = run("sh", CHECK, *args)
                self.assertEqual(result.returncode, 2)
                self.assertIn("usage:", result.stderr)
