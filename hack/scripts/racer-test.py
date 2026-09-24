#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Bounded Racer suites. Build manifests are explicit; failures never retry away."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import shlex
import re
import signal
import subprocess
import sys
import time

ROOT = Path(__file__).resolve().parents[2]


def terminate(child, result):
    for sig, grace in ((signal.SIGTERM, 10), (signal.SIGKILL, 2)):
        try:
            os.killpg(child.pid, sig)
        except ProcessLookupError:
            pass
        try:
            child.wait(timeout=grace)
        except subprocess.TimeoutExpired:
            if sig == signal.SIGKILL:
                result['unreaped_pid'] = child.pid
                print(f'PID {child.pid} remains unreaped after KILL; inspect host kernel wait state',
                      file=sys.stderr, flush=True)
        # Always send KILL to descendants even if the parent exited on TERM.


def run(name, command, seconds, log_dir, env=None, cwd=None, require_one_test=False):
    log_dir.mkdir(parents=True, exist_ok=True)
    # Unique logs preserve repeated diagnostic runs rather than hiding a failure.
    stem = log_dir / f"{name}-{time.time_ns()}"
    started = time.monotonic()
    result = dict(name=name, command=command, timeout_seconds=seconds,
                  log=str(stem.with_suffix('.log')), timed_out=False,
                  cwd=str(Path(cwd or '.').resolve()))
    print(f"[{name}] deadline={seconds}s log={result['log']}\n  {shlex.join(command)}", flush=True)
    with open(result['log'], 'w') as log:
        try:
            child = subprocess.Popen(command, stdout=log, stderr=subprocess.STDOUT,
                                     start_new_session=True, env=env, cwd=cwd)
            try:
                code = child.wait(timeout=seconds)
            except subprocess.TimeoutExpired:
                result['timed_out'] = True
                # Keep the complete test output, including the last active test,
                # and process/thread wait sites before terminating the group.
                with open(stem.with_suffix('.processes.log'), 'w') as diagnostic:
                    try:
                        # subprocess.run(timeout=...) itself performs an unbounded
                        # reap after KILL. Use the same bounded policy for ps.
                        probe = subprocess.Popen(['ps', '-eLo', 'pid,ppid,pgid,tid,stat,wchan:32,args'],
                                                 stdout=diagnostic, stderr=subprocess.STDOUT,
                                                 start_new_session=True)
                        try:
                            probe.wait(timeout=3)
                        except subprocess.TimeoutExpired:
                            diagnostic_result = {}
                            terminate(probe, diagnostic_result)
                            result['diagnostic_cleanup'] = diagnostic_result
                    except OSError as error:
                        diagnostic.write(f'{error}\n')
                terminate(child, result)
                code = 124
        except OSError as error:
            log.write(f"{error}\n")
            code = 127
    if code == 0 and require_one_test and not re.search(
            r'test result: ok\. 1 passed; 0 failed; 0 ignored;',
            Path(result['log']).read_text()):
        result['validation_error'] = 'no evidence of exactly one executed test'
        print(f"[{name}] {result['validation_error']}", file=sys.stderr)
        code = 1
    result.update(exit_code=code, elapsed_seconds=round(time.monotonic() - started, 3))
    try:
        stem.with_suffix('.json').write_text(json.dumps(result, indent=2) + '\n')
    except OSError as error:
        # Disk exhaustion must not replace the test's exit status with a Python
        # traceback. stderr is captured by the parent/CI even if the disk is full.
        print(f'[{name}] cannot write result JSON: {error}; '
              f'original_result={json.dumps(result)}', file=sys.stderr, flush=True)
        # Losing evidence is still a harness failure if the command passed.
        code = code or 1
    print(f"[{name}] {'TIMEOUT' if result['timed_out'] else 'PASS' if code == 0 else 'FAIL'} "
          f"exit={code} elapsed={result['elapsed_seconds']}s", flush=True)
    if code:
        # Logs remain complete on disk; bound terminal output on large failures.
        with open(result['log'], 'rb') as log:
            log.seek(max(0, log.seek(0, 2) - 8192))
            print(log.read().decode(errors='replace'), flush=True)
    return code, Path(result['log'])


def cargo_command(crate):
    prefix = 'RACER_CONTROLPLANE' if crate == 'controlplane' else 'RACER'
    directory = Path(os.environ.get(f'{prefix}_CARGO_TARGET_DIR', f'cmd/racer-{crate}/target')).resolve()
    return shlex.split(os.environ.get('CARGO', 'cargo')) + [
        'test', '--manifest-path', f'cmd/racer-{crate}/Cargo.toml',
        '--target-dir', str(directory), '--locked']


def source_digest():
    # Include uncommitted and new source inputs, not target/scratch directories.
    paths = subprocess.check_output(['git', 'ls-files', '--cached', '--others',
                                    '--exclude-standard', '-z', '--',
                                    'cmd/racer-controlplane', 'cmd/racer-dataplane',
                                    'api/racer', '.cargo', 'rust-toolchain*'], cwd=ROOT)
    digest = hashlib.sha256()
    for raw in sorted(set(paths.split(b'\0')) - {b''}):
        digest.update(raw + b'\0')
        path = ROOT / os.fsdecode(raw)
        if not path.exists():
            digest.update(b'deleted\0')
            continue
        with path.open('rb') as source:
            while block := source.read(1024 * 1024):
                digest.update(block)
    return digest.hexdigest()


def executable_identity(path):
    stat = Path(path).stat()
    return [stat.st_dev, stat.st_ino, stat.st_size, stat.st_mtime_ns]


def validate_executable(artifact, args):
    executable = Path(artifact['executable'])
    expected = artifact['identity']
    try:
        actual = executable_identity(executable)
    except OSError as error:
        actual = str(error)
    if not executable.is_absolute() or actual != expected:
        fields = ('device', 'inode', 'size', 'mtime_ns')
        diagnostic = dict(executable=str(executable), name=artifact['name'],
                          expected=dict(zip(fields, expected)),
                          actual=dict(zip(fields, actual)) if isinstance(actual, list) else actual)
        args.logs.mkdir(parents=True, exist_ok=True)
        evidence = args.logs / f'artifact-changed-{time.time_ns()}.json'
        evidence.write_text(json.dumps(diagnostic, indent=2) + '\n')
        raise ValueError(f'executable changed: {executable}; expected={diagnostic["expected"]}; '
                         f'actual={diagnostic["actual"]}; evidence={evidence}; '
                         'recompile tests after other users of this Cargo target directory finish, '
                         'or use an isolated target directory')


def load_artifacts(crate, args, *, name=None):
    manifest = args.artifacts / f'{crate}.json'
    data = json.loads(manifest.read_text())
    if (not isinstance(data, dict) or data.get('root') != str(ROOT)
            or data.get('source_digest') != source_digest()):
        raise ValueError(f'{manifest}: stale or foreign source manifest; recompile tests')
    # An exporter depends on its own executable, not on unrelated library/bin
    # test executables in the same Cargo manifest. Never refresh identities here:
    # a replaced exporter may have been compiled from another worktree.
    artifacts = [a for a in data['artifacts'] if name is None or a['name'] == name]
    for artifact in artifacts:
        validate_executable(artifact, args)
    return artifacts


def compile_crate(crate, args):
    manifest = args.artifacts / f'{crate}.json'
    manifest.unlink(missing_ok=True)  # Never execute stale binaries after a failed build.
    digest = source_digest()
    command = cargo_command(crate) + (['--release'] if args.release else []) + [
        '--all-targets', '--no-run', '--message-format=json']
    code, log = run(f'{crate}-build', command, args.build_timeout, args.logs)
    if code:
        return code
    artifacts = []
    for line in log.read_text().splitlines():
        try:
            message = json.loads(line)
        except ValueError:
            continue
        if message.get('reason') == 'compiler-artifact' and message.get('executable') and message['profile']['test']:
            executable = str(Path(message['executable']).resolve())
            artifacts.append(dict(name=message['target']['name'],
                                  kind=message['target']['kind'], executable=executable,
                                  identity=executable_identity(executable)))
    if not artifacts:
        raise RuntimeError(f'{crate}: Cargo emitted no test executables; see {log}')
    if source_digest() != digest:
        raise ValueError('source changed during compilation; recompile tests')
    manifest.write_text(json.dumps(dict(root=str(ROOT), source_digest=digest,
                                        build_command=command, artifacts=artifacts), indent=2) + '\n')
    return 0


def execution_env(crate, args):
    failed = False
    env = os.environ.copy()
    env.setdefault('RUST_TEST_THREADS', '2')
    if crate == 'dataplane':
        # Fresh export each invocation, from the actual compiler test executable.
        # The consumer validates the artifact; no nested Cargo during execution.
        try:
            exporters = load_artifacts('controlplane', args, name='placement_export')
        except (OSError, ValueError) as error:
            print(error, file=sys.stderr)
            exporters = []
        run_id = f'{os.getpid()}-{time.time_ns()}'
        export = args.artifacts / f'placement-{run_id}'
        export.mkdir()
        env['RACER_PLACEMENT_EXPORT'] = str(export.resolve())
        env['RACER_PLACEMENT_EXPORT_RUN_ID'] = run_id
        if len(exporters) != 1:
            print('Missing compiler exporter; run make racer-rust-test-compile', file=sys.stderr)
            failed = True
        else:
            code, _ = run('compiler-export', [exporters[0]['executable'], '--exact',
                          'export_dataplane_placement', '--nocapture'], 90, args.logs, env,
                          cwd=ROOT / 'cmd/racer-controlplane', require_one_test=True)
            failed |= code != 0
            if code == 0:
                (export / 'export-receipt.json').write_text(json.dumps({
                    'producer': 'racer-controlplane/placement_export::export_dataplane_placement',
                    'run_id': run_id,
                }) + '\n')
    return failed, env


def execute_crate(crate, args):
    try:
        artifacts = load_artifacts(crate, args)
        invalid_manifest = False
    except (OSError, ValueError) as error:
        print(f'{error}; run make racer-rust-test-compile', file=sys.stderr)
        invalid_manifest, artifacts = True, []
    failed, env = execution_env(crate, args)
    failed |= invalid_manifest
    for artifact in artifacts:
        # Earlier suites can take minutes. Recheck at the point of use rather
        # than trusting a validation performed before the entire crate ran.
        try:
            validate_executable(artifact, args)
        except ValueError as error:
            print(error, file=sys.stderr)
            failed = True
            continue
        code, _ = run(f"{crate}-{artifact['name']}-{'-'.join(artifact['kind'])}",
                      [artifact['executable'], '--nocapture'], args.test_timeout, args.logs, env,
                      cwd=ROOT / f'cmd/racer-{crate}')
        failed |= code != 0
    # Stable Cargo has no doctest --no-run. Keep its compile/run budget separate.
    code, _ = run(f'{crate}-doctests', cargo_command(crate) + ['--doc'],
                  args.test_timeout, args.logs, env)
    return int(failed or code != 0)


def selected(args):
    artifacts = load_artifacts(args.crate, args)
    for artifact in artifacts:
        command = [artifact['executable'], '--exact', args.test]
        for ignored_only in (False, True):
            code, log = run('test-discovery', command + ['--list'] +
                            (['--ignored'] if ignored_only else []), 10, args.logs,
                            cwd=ROOT / f'cmd/racer-{args.crate}')
            if code:
                return code
            found = f'{args.test}: test' in log.read_text().splitlines()
            if not ignored_only and not found:
                break
            if ignored_only:
                if found != args.ignored:
                    raise ValueError(f'{args.test}: --ignored must match the test ignore status')
                failed, env = execution_env(args.crate, args)
                if failed:
                    return 1
                validate_executable(artifact, args)
                code, log = run(args.test.replace('::', '-'), command + ['--nocapture'] +
                                (['--ignored'] if args.ignored else []), args.test_timeout,
                                args.logs, env, cwd=ROOT / f'cmd/racer-{args.crate}',
                                require_one_test=True)
                return code
    raise ValueError(f'test not found: {args.test}')


def main():
    os.chdir(ROOT)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--logs', type=Path, default=Path(os.environ.get('RACER_TEST_LOG_DIR', 'tmp/racer-test-logs')))
    parser.add_argument('--artifacts', type=Path, default=Path(os.environ.get('RACER_TEST_ARTIFACT_DIR', 'tmp/racer-test-artifacts')))
    parser.add_argument('--build-timeout', type=int, default=int(os.environ.get('RACER_BUILD_TIMEOUT_SECONDS', '1200')))
    parser.add_argument('--test-timeout', type=int, default=int(os.environ.get('RACER_TEST_TIMEOUT_SECONDS', '600')))
    parser.add_argument('--release', action='store_true', help='compile release tests (use a separate artifact directory)')
    sub = parser.add_subparsers(dest='action', required=True)
    for action in ('compile', 'execute', 'test'):
        child = sub.add_parser(action)
        child.add_argument('crates', nargs='+', choices=['controlplane', 'dataplane'])
    child = sub.add_parser('selected')
    child.add_argument('crate', choices=['controlplane', 'dataplane'])
    child.add_argument('test')
    child.add_argument('--ignored', action='store_true')
    child = sub.add_parser('run')
    child.add_argument('name')
    child.add_argument('seconds', type=int)
    child.add_argument('command', nargs=argparse.REMAINDER)
    args = parser.parse_args()
    if args.action == 'run':
        return run(args.name, args.command, args.seconds, args.logs)[0]
    args.artifacts.mkdir(parents=True, exist_ok=True)
    if args.action == 'selected':
        return selected(args)
    failed = False
    if args.action in ('compile', 'test'):
        crates = list(dict.fromkeys((['controlplane'] if 'dataplane' in args.crates else []) + args.crates))
        for crate in crates:
            try:
                failed |= compile_crate(crate, args) != 0
            except (OSError, ValueError, RuntimeError) as error:
                print(error, file=sys.stderr)
                failed = True
    if args.action in ('execute', 'test'):
        for crate in args.crates:
            failed |= execute_crate(crate, args) != 0
    return int(failed)


if __name__ == '__main__':
    sys.exit(main())
