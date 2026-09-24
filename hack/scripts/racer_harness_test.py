#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Small harness regressions; no Cargo, Docker daemon, or hardware required."""

import argparse
import errno
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock


def load(name):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(name + '.py'))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


harness = load('racer-test')
context = load('racer-source-context')


class HarnessTests(unittest.TestCase):
    def setUp(self):
        # Keep all test artifacts inside the workspace, even without TMPDIR.
        scratch = Path(__file__).resolve().parents[2] / 'tmp'
        scratch.mkdir(exist_ok=True)
        self.temporary = tempfile.TemporaryDirectory(prefix='racer-harness-', dir=scratch)
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.args = argparse.Namespace(artifacts=self.root, logs=self.root / 'logs',
                                      test_timeout=10, build_timeout=10, release=False)

    def test_exit_and_timeout_records(self):
        for name, program, deadline, expected in [
            ('success', 'print("evidence")', 5, 0),
            ('failure', 'raise SystemExit(7)', 5, 7),
            ('hang', 'import time; time.sleep(30)', 0.1, 124),
        ]:
            with self.subTest(name=name):
                code, log = harness.run(name, [sys.executable, '-c', program], deadline, self.args.logs)
                self.assertEqual(code, expected)
                record = json.loads(log.with_suffix('.json').read_text())
                self.assertEqual(record['exit_code'], expected)
                self.assertEqual(record['timed_out'], expected == 124)
                self.assertGreater(record['elapsed_seconds'], 0)
                if expected == 124:
                    self.assertTrue(log.with_suffix('.processes.log').exists())

    def test_result_json_disk_full_preserves_original_outcome(self):
        for original, expected in [(7, 7), (0, 1)]:
            with self.subTest(original=original):
                stderr = io.StringIO()
                with mock.patch.object(Path, 'write_text', side_effect=OSError(errno.ENOSPC, 'disk full')), mock.patch.object(sys, 'stderr', stderr):
                    code, _ = harness.run('json-disk-full', [sys.executable, '-c', f'raise SystemExit({original})'], 5, self.args.logs)
                self.assertEqual(code, expected)
                self.assertIn('cannot write result JSON', stderr.getvalue())
                record = json.loads(stderr.getvalue().split('original_result=', 1)[1])
                self.assertEqual(record['exit_code'], original)

    def test_library_failure_still_executes_binary_and_doctests(self):
        artifacts = [
            dict(name='library', kind=['lib'], executable='/fake/lib'),
            dict(name='daemon', kind=['bin'], executable='/fake/bin'),
        ]
        calls = []

        def run(name, command, *args, **kwargs):
            calls.append(command)
            return (1 if command[0] == '/fake/lib' else 0), self.root / 'unused'

        with mock.patch.object(harness, 'run', side_effect=run), mock.patch.object(harness, 'load_artifacts', return_value=artifacts), mock.patch.object(harness, 'validate_executable'):
            self.assertEqual(harness.execute_crate('controlplane', self.args), 1)
        self.assertEqual(calls[0][0], '/fake/lib')
        self.assertEqual(calls[1][0], '/fake/bin')
        self.assertIn('--doc', calls[2])

    def test_timeout_kills_descendant_after_parent_exits(self):
        marker = self.root / 'escaped-child'
        gate = self.root / 'supervisor-returned'
        descendant = (f'import time\nfrom pathlib import Path\n'
                      f'while not Path({str(gate)!r}).exists(): time.sleep(0.01)\n'
                      f'Path({str(marker)!r}).touch()\n')
        program = ('import subprocess,sys,time; '
                   f'subprocess.Popen([sys.executable,"-c",{descendant!r}]); '
                   'time.sleep(30)')
        code, _ = harness.run('process-group', [sys.executable, '-c', program], 0.2, self.args.logs)
        self.assertEqual(code, 124)
        gate.touch()
        time.sleep(0.2)
        self.assertFalse(marker.exists(), 'timeout leaked a descendant')

    def test_failed_compile_invalidates_old_artifacts(self):
        manifest = self.root / 'controlplane.json'
        manifest.write_text('[]')
        with mock.patch.object(harness, 'run', return_value=(1, self.root / 'unused')):
            self.assertEqual(harness.compile_crate('controlplane', self.args), 1)
        self.assertFalse(manifest.exists())

    def test_export_receipt_only_after_success(self):
        artifacts = dict(controlplane=[
            dict(name='placement_export', executable='/fake/export'),
        ], dataplane=[
            dict(name='library', kind=['lib'], executable='/fake/dataplane'),
        ])
        for export_code in (0, 1):
            with self.subTest(export_code=export_code):
                def run(name, command, seconds, logs, env, **kwargs):
                    if name == 'compiler-export':
                        return export_code, self.root / 'unused'
                    receipt = Path(env['RACER_PLACEMENT_EXPORT']) / 'export-receipt.json'
                    self.assertEqual(receipt.exists(), export_code == 0)
                    if receipt.exists():
                        self.assertEqual(json.loads(receipt.read_text()), dict(
                            producer='racer-controlplane/placement_export::export_dataplane_placement',
                            run_id=env['RACER_PLACEMENT_EXPORT_RUN_ID']))
                    return 0, self.root / 'unused'

                with mock.patch.object(harness, 'run', side_effect=run), mock.patch.object(harness, 'load_artifacts', side_effect=lambda crate, args, **kwargs: artifacts[crate]), mock.patch.object(harness, 'validate_executable'):
                    self.assertEqual(harness.execute_crate('dataplane', self.args), export_code)

    def test_export_validates_only_exporter_but_never_accepts_replacement(self):
        library, exporter = self.root / 'library', self.root / 'exporter'
        library.write_text('library build 1')
        exporter.write_text('exporter build 1')
        artifacts = [dict(name=name, kind=['test'], executable=str(path),
                          identity=harness.executable_identity(path))
                     for name, path in [('library', library), ('placement_export', exporter)]]
        (self.root / 'controlplane.json').write_text(json.dumps(dict(
            root=str(harness.ROOT), source_digest='source-v1', artifacts=artifacts)))
        library.write_text('a concurrent build replaced the library')
        with mock.patch.object(harness, 'source_digest', return_value='source-v1'), mock.patch.object(harness, 'run', return_value=(0, self.root / 'unused')) as run:
            failed, env = harness.execution_env('dataplane', self.args)
            self.assertFalse(failed)
            self.assertEqual(run.call_args.args[1][0], str(exporter))
            self.assertTrue((Path(env['RACER_PLACEMENT_EXPORT']) / 'export-receipt.json').exists())
            with self.assertRaisesRegex(ValueError, 'executable changed.*library'):
                harness.load_artifacts('controlplane', self.args)
            run.reset_mock()
            exporter.write_text('a different worktree replaced the exporter')
            failed, env = harness.execution_env('dataplane', self.args)
            self.assertTrue(failed)
            run.assert_not_called()
            self.assertFalse((Path(env['RACER_PLACEMENT_EXPORT']) / 'export-receipt.json').exists())
        evidence = [json.loads(path.read_text()) for path in self.args.logs.glob('artifact-changed-*.json')]
        changed_exporter = next(item for item in evidence if item['name'] == 'placement_export')
        self.assertEqual(changed_exporter['expected']['size'], len('exporter build 1'))
        self.assertEqual(changed_exporter['actual']['size'], exporter.stat().st_size)

    def test_execution_rechecks_later_artifact_and_continues_to_doctests(self):
        first, second = self.root / 'first', self.root / 'second'
        first.write_text('first build')
        second.write_text('second build')
        artifacts = [dict(name=path.name, kind=['test'], executable=str(path),
                          identity=harness.executable_identity(path)) for path in (first, second)]
        calls = []

        def run(name, command, *args, **kwargs):
            calls.append(command)
            if command[0] == str(first):
                second.write_text('concurrent replacement while first test ran')
            return 0, self.root / 'unused'

        with mock.patch.object(harness, 'load_artifacts', return_value=artifacts), mock.patch.object(harness, 'run', side_effect=run):
            self.assertEqual(harness.execute_crate('controlplane', self.args), 1)
        self.assertEqual(len(calls), 2)
        self.assertEqual(calls[0][0], str(first))
        self.assertIn('--doc', calls[1])

    def test_post_kill_reaping_is_bounded_and_reported(self):
        child = mock.Mock(pid=12345)
        child.wait.side_effect = subprocess.TimeoutExpired('stuck kernel child', 1)
        probe = mock.Mock()
        probe.wait.return_value = 0
        with mock.patch.object(harness.subprocess, 'Popen', side_effect=[child, probe]), mock.patch.object(harness.os, 'killpg') as kill:
            code, log = harness.run('unreapable', ['fake'], 0.01, self.args.logs)
        self.assertEqual(code, 124)
        self.assertEqual(child.wait.call_args_list, [mock.call(timeout=0.01), mock.call(timeout=10), mock.call(timeout=2)])
        self.assertEqual(kill.call_count, 2)
        self.assertEqual(json.loads(log.with_suffix('.json').read_text())['unreaped_pid'], 12345)

    def write_manifest(self, crate, executable, digest='source-v1'):
        (self.root / f'{crate}.json').write_text(json.dumps(dict(
            root=str(harness.ROOT), source_digest=digest,
            artifacts=[dict(name='fixture', kind=['lib'], executable=str(executable),
                            identity=harness.executable_identity(executable))])))

    def test_manifest_rejects_source_worktree_and_binary_drift(self):
        binary = self.root / 'executable'
        binary.write_text('original')
        self.write_manifest('controlplane', binary)
        with mock.patch.object(harness, 'source_digest', return_value='source-v1'):
            self.assertEqual(harness.load_artifacts('controlplane', self.args)[0]['executable'], str(binary))
            binary.write_text('changed build artifact')
            with self.assertRaisesRegex(ValueError, 'executable changed'):
                harness.load_artifacts('controlplane', self.args)
        self.write_manifest('controlplane', binary)
        with mock.patch.object(harness, 'source_digest', return_value='source-v2'):
            with self.assertRaisesRegex(ValueError, 'stale or foreign'):
                harness.load_artifacts('controlplane', self.args)
        with mock.patch.object(harness, 'ROOT', self.root):
            with self.assertRaisesRegex(ValueError, 'stale or foreign'):
                harness.load_artifacts('controlplane', self.args)

    def test_compile_normalizes_relative_executable_before_changing_cwd(self):
        binary = self.root / 'test-executable'
        binary.write_text('fake binary')
        relative = os.path.relpath(binary)
        log = self.root / 'cargo.json'
        log.write_text(json.dumps(dict(reason='compiler-artifact', executable=relative,
                                      profile=dict(test=True), target=dict(name='fixture', kind=['lib']))) + '\n')
        with mock.patch.object(harness, 'source_digest', return_value='source-v1'), mock.patch.object(harness, 'run', return_value=(0, log)):
            self.assertEqual(harness.compile_crate('controlplane', self.args), 0)
            artifact = harness.load_artifacts('controlplane', self.args)[0]
        self.assertEqual(artifact['executable'], str(binary))

    def test_selected_requires_execution_and_prepares_compiler_export(self):
        executable = self.root / 'libtest'
        # This fake libtest exercises real process invocation, argument selection,
        # crate cwd, and artifact environment without compiling Rust.
        executable.write_text(f'''#!{sys.executable}
import os,sys
from pathlib import Path
assert Path.cwd() == Path({str(harness.ROOT / 'cmd/racer-dataplane')!r})
test = sys.argv[sys.argv.index('--exact') + 1]
if '--list' in sys.argv:
    if '--ignored' not in sys.argv or test == 'ignored_case':
        print(test + ': test')
elif test == 'empty_case':
    print('test result: ok. 0 passed; 0 failed; 1 ignored;')
else:
    assert Path(os.environ['RACER_PLACEMENT_EXPORT']).is_dir()
    assert os.environ['RACER_PLACEMENT_EXPORT_RUN_ID'] == 'test-run'
    print('test result: ok. 1 passed; 0 failed; 0 ignored;')
''')
        executable.chmod(0o755)
        self.write_manifest('dataplane', executable)
        self.args.crate = 'dataplane'
        prepared = os.environ | dict(RACER_PLACEMENT_EXPORT=str(self.root), RACER_PLACEMENT_EXPORT_RUN_ID='test-run')
        with mock.patch.object(harness, 'source_digest', return_value='source-v1'), mock.patch.object(harness, 'execution_env', return_value=(False, prepared)) as export:
            for name, ignored, error in [('ignored_case', False, '--ignored'), ('ordinary_case', True, '--ignored')]:
                self.args.test, self.args.ignored = name, ignored
                with self.subTest(name=name), self.assertRaisesRegex(ValueError, error):
                    harness.selected(self.args)
            self.args.test, self.args.ignored = 'empty_case', False
            self.assertEqual(harness.selected(self.args), 1)
            record = json.loads(next(self.args.logs.glob('empty_case-*.json')).read_text())
            self.assertEqual(record['exit_code'], 1)
            self.assertIn('no evidence', record['validation_error'])
            for name, ignored in [('ignored_case', True), ('compiler_consumer', False)]:
                self.args.test, self.args.ignored = name, ignored
                self.assertEqual(harness.selected(self.args), 0)
            self.assertEqual(export.call_count, 3)

    def test_release_build_manifest_drives_selected_without_release_flag(self):
        executable = self.root / 'release-test'
        executable.write_text('release executable')
        cargo_log = self.root / 'cargo.json'
        cargo_log.write_text(json.dumps(dict(
            reason='compiler-artifact', executable=str(executable),
            profile=dict(test=True), target=dict(name='scale', kind=['lib']))) + '\n')
        self.args.release = True
        with mock.patch.object(harness, 'source_digest', return_value='source-v1'):
            with mock.patch.object(harness, 'run', return_value=(0, cargo_log)) as build:
                self.assertEqual(harness.compile_crate('controlplane', self.args), 0)
                self.assertIn('--release', build.call_args.args[1])
            manifest = json.loads((self.root / 'controlplane.json').read_text())
            self.assertIn('--release', manifest['build_command'])
            self.args.release = False
            self.args.crate, self.args.test, self.args.ignored = 'controlplane', 'scale_test', True
            commands = []

            def run(name, command, *args, **kwargs):
                commands.append(command)
                log = self.root / f'{len(commands)}.log'
                log.write_text('scale_test: test\n' if '--list' in command else
                               'test result: ok. 1 passed; 0 failed; 0 ignored;\n')
                return 0, log

            with mock.patch.object(harness, 'run', side_effect=run):
                self.assertEqual(harness.selected(self.args), 0)
            self.assertEqual(len(commands), 3)
            self.assertTrue(all(command[0] == str(executable) for command in commands))
            self.assertTrue(all('--release' not in command for command in commands))

    def test_source_context_does_not_walk_excluded_directories(self):
        source = self.root / 'repo'
        source.mkdir()
        subprocess.run(['git', 'init', '-q', str(source)], check=True)
        (source / 'source.txt').write_text('old')
        (source / '.dockerignore').write_text('.worktrees/\ntmp/\n!**/.env.example\n')
        subprocess.run(['git', '-C', str(source), 'add', 'source.txt', '.dockerignore'], check=True)
        (source / 'source.txt').write_text('uncommitted edit')
        blocked = source / '.worktrees' / 'other' / 'tmp'
        blocked.mkdir(parents=True)
        blocked.chmod(0)
        self.addCleanup(blocked.chmod, 0o700)
        output = self.root / 'context'
        context.snapshot(source, output)
        self.assertEqual((output / 'source.txt').read_text(), 'uncommitted edit')
        self.assertFalse((output / '.worktrees').exists())
        self.assertFalse((output / '.git').exists())
        with self.assertRaises(FileExistsError):
            context.snapshot(source, output)

    def test_live_script_selects_reduced_and_full_campaign_once(self):
        go = self.root / 'go'
        calls = self.root / 'go-calls.json'
        go.write_text(f'#!{sys.executable}\nimport json,sys\nfrom pathlib import Path\n'
                      f'Path({str(calls)!r}).write_text(json.dumps(sys.argv[1:]))\n')
        go.chmod(0o755)
        env = os.environ | dict(PATH=str(self.root) + os.pathsep + os.environ['PATH'],
                                TMPDIR=str(self.root), RACER_CONTROLPLANE_BINARY='/fake/cp',
                                RACER_DATAPLANE_BINARY='/fake/dp', KUBEBUILDER_ASSETS='/fake/assets',
                                RACER_LIVE_SOCKET_ROOT=str(self.root))
        subprocess.run(['bash', str(harness.ROOT / 'hack/scripts/racer-controlplane-live.sh')],
                       env=env, check=True, timeout=5)
        args = json.loads(calls.read_text())
        self.assertEqual(args[args.index('-run') + 1], '^(TestColdObjectMultiPeer|TestProductionBinaryCampaign)$')
        self.assertEqual(args.count('./e2e/racer-controlplane'), 1)
        self.assertIn('-count=1', args)

    def test_source_context_rejects_symlink_parent_escape(self):
        source = self.root / 'source'
        source.mkdir()
        outside = self.root / 'outside'
        outside.mkdir()
        (outside / 'secret').write_text('must not copy')
        (source / 'directory').symlink_to(outside, target_is_directory=True)
        with mock.patch.object(context.subprocess, 'check_output', return_value=b'directory/secret\0'):
            with self.assertRaisesRegex(ValueError, 'symlink parent'):
                context.snapshot(source, self.root / 'destination')


if __name__ == '__main__':
    unittest.main()
